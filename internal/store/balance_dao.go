package store

// 用户余额：读余额、预检、扣费。
//
// # 三条不可动摇的口径
//
//  1. **金额是整数分**（人民币，100 分 = 1 元），见 User.BalanceCents。
//     绝不用浮点存余额。
//  2. **「不限额」= 列值 NULL**，不是 0。这与 quota_tokens 的「0 = 不限」
//     刻意相反，因为 0 在余额语义下是「真没钱」，两者含义对立。
//     见 User.BalanceCents 里的完整对比。
//  3. **扣费金额由调用方给出，且必须等于 cost_total 换算的分**。本包
//     **不重算费用**：调用方拿的是 CreateUsageRecordWithCost 返回的那个
//     cost 值（元），用 YuanToCents 换算成分。公式只有 priceUsage 一份，
//     所以「报表显示花了 X、余额扣了 Y」在结构上不可能发生。
//
// # 策略：预检拒绝，不做预占/退款
//
// 与 key_dao.ReserveQuota 的「终身累计额度预占」不同，余额**不做预占**。
// 理由：token 是整数且可预估，扣到 0 是个确定事件；金额取决于请求结束才知道
// 的真实输出长度，请求前的估算与真实值必然不等。一旦预占就必须做「按差值
// 校正」，而那正是 2026-10-07 的 P0-2 —— 一个请求被记两次。所以口径是：
// 请求前用估算值**预检**（BalanceOf + 调用方自己比），不够就拒绝；
// 请求成功后按**真实费用**扣一次，不预占、不退款、不找零。
//
// 并发下的轻微超扣是可接受的：预检是「查了再放」，两个并发请求可能同时
// 看到同一份余额并都通过。**但扣费那一层不允许超扣** —— ChargeBalance 的
// UPDATE 自带余额守卫，把越界扣减挡在门外（见其注释）。

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// ErrInsufficientBalance 表示扣费时余额不足。
//
// 与「幂等重复扣费」严格区分：后者**不是错误**（见 ChargeBalance 的注释），
// 前者才是 —— 前者是「用户真的没钱了」，需要被日志与 402 响应捕获。
var ErrInsufficientBalance = errors.New("insufficient balance")

// YuanToCents 把费用（元，float64）换算成整数分，四舍五入。
//
// **这是「元 → 分」的唯一实现**，扣费方必须用它，保证与 cost_total 的
// 换算方式一致。math.Round 对正数即 floor(x+0.5)，即最普通的四舍五入。
//
// 为什么四舍五入而不是截断（int 强转）：截断会**系统性地向下少收** ——
// 每次都丢掉不足半分，量大时累积成可观的漏收（每 100 万次 0.4 元级别的
// 误差起步），且方向单一、无法自愈。四舍五入的误差零均值，每次最多半分，
// 永远小于最小计价粒度。
//
// 负数与 0 一律返回 0：费用没有负的概念（负 token 已在 CreateUsageRecord
// 入口归一为 0），让负费用流进扣费路径只会造出「扣费即充值」的反直觉行为。
func YuanToCents(yuan float64) int64 {
	if yuan <= 0 {
		return 0
	}
	return int64(math.Round(yuan * 100))
}

// BalanceOf 读某用户的余额（单位：分）。
//
// 返回的 unlimited=false 表示**不限额**（列值 NULL）：此时 cents 无意义
// （恒为 0），调用方应直接放行、**不要比较数值** —— 把 0 当成「余额为零」
// 会让所有不限额用户被判成欠费。这是本字段最容易被踩的语义，见
// User.BalanceCents 里「为什么 0 不能表示不限」的对比。
//
// 用户不存在时返回 (0, true, ErrNotFound)：**刻意不是** (0, false, nil) ——
// 「用户不存在」在热路径上意味着鉴权出了 bug，绝不能被误读成「不限额放行」。
// 这里的取舍是「保守 + 报错」：cents=0、unlimited=true 意味着若调用方
// 忽略 err 直接用返回值，会判定成「余额 0、不够」而拒绝（fail-closed），
// 这正是我们希望的方向。
//
// BalanceOf 读某用户的余额（单位：分）。
//
// # 返回的 limited 是「有余额上限」，**不是**「不限额」
//
// 这个名字是本函数最容易被误读的一处，故显式声明：`limited == true` 表示
// **这一行受余额约束**（balance_cents 非 NULL），`limited == false` 表示
// **不限额**（列值为 NULL），此时 cents 无意义（恒为 0），调用方应直接放行、
// **不要比较数值** —— 把 0 当成「余额为零」会让所有不限额用户被判成欠费。
//
// 为什么用「有上限」这个正向说法，而不是 `unlimited`：与 User.Unlimited
// 恰好相反的两个布尔量并排出现在同一个调用点（`u.Unlimited` 与这里的
// `limited`）时，**反着的名字比正着的名字危险得多** —— `if !limited` 读成
// 「不限额」几乎不需要思考，而 `if !u.Unlimited` 读错则会静默放行欠费用户。
// 这里让两者都取正向语义：User.Unlimited=true 是「不限额」，本函数
// limited=true 是「有上限」。判定一律看布尔，**不要看数值**。
//
// # 用户不存在 / 读失败
//
// 两者都返回 (0, true, err)：**刻意不是** (0, false, nil) ——
// 「用户不存在」在热路径上意味着鉴权出了 bug，绝不能被误读成「不限额放行」；
// 读池抖动同理。这里的取舍是「保守 + 报错」：limited=true、cents=0 意味着
// 即使调用方忽略 err 直接用返回值，也会判定成「余额 0、不够」而拒绝
// （fail-closed），这正是我们希望的方向。
//
// 为什么不复用 GetUser：热路径只需要余额一个字段，而 GetUser 会把 15 列
// 全部扫一遍（含 JSON 解析与 IP 白名单解析）。这条是每请求都走的判定。
func (s *Store) BalanceOf(ctx context.Context, userID string) (cents int64, limited bool, err error) {
	var raw sql.NullInt64
	err = s.read.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, ErrNotFound
	}
	if err != nil {
		// 读失败同样返回「有上限 + 0」：读池抖动绝不能被当成「不限额放行」。
		return 0, true, err
	}
	if !raw.Valid {
		return 0, false, nil // NULL = 不限额
	}
	return raw.Int64, true, nil
}

// HasSufficientBalance 预检：余额是否够 amountCents。
//
// # ok 与 err 必须分开处理
//
//	ok=true   放行（不限额，或余额 >= amountCents）
//	ok=false  余额真不够 —— 调用方应回 402 之类的「欠费」响应
//	err!=nil  读失败 —— 余额未知。放行等于漏扣（钱），拒绝等于误伤（可用性）。
//	          这个判断属于调用方，网关的既定选择是 fail-closed。
//
// 热路径请改用 BalanceOf 自己比：那样能把「读失败」与「余额不够」两种
// 完全不同的响应分开（本方法把两者都压成 false + err，够用但不如分开精确）。
// 本方法保留给不需要区分错误来源的调用方（管理面、报表、测试）。
func (s *Store) HasSufficientBalance(ctx context.Context, userID string, amountCents int64) (bool, error) {
	cents, limited, err := s.BalanceOf(ctx, userID)
	if err != nil {
		return false, err
	}
	if !limited {
		return true, nil // 不限额，永远够
	}
	return cents >= amountCents, nil
}

// ChargeBalance 扣一次费。amountCents **必须**等于该次调用的 cost_total
// 换算成的分（用 YuanToCents 换算），否则余额与报表对不上。
//
// # 幂等：同一 requestID 只扣一次
//
// 网络重试、流式中断重连、上游重试后的重放，都会让**同一次调用**的收尾
// 走两遍。扣费是「减余额」这种不可逆操作，重复执行等于重复扣钱。
//
// 做法是 balance_charges 表以 request_id 为主键（见 store.go 的建表），
// 在**同一事务内**先 INSERT OR IGNORE 占位、再执行扣费：
//   - 占位命中（影响 0 行）说明这个 requestID 已扣过 → 直接返回 nil
//     （幂等命中**不是错误**：重试方要的正是「别再扣一次」，回错误只会
//     让调用方把它当成扣费失败而重试更多次），不碰余额；
//   - 占位成功 → 执行 UPDATE 扣费 → 提交。
//
// 「先占位再扣、且在同一事务」是不可颠倒的：反过来（先扣后占位）时两个
// 并发重试会各自扣一次、再各占一次位（第二次占位被主键拒绝，但**余额已经
// 扣过了**）—— 主键只挡住了流水重复，没挡住钱重复。
//
// requestID 为空时不做幂等（每次调用都扣）。真实请求都有 request_id
// （网关从请求头取），空串只在手工/测试路径出现。
//
// # 绝不扣成负数
//
// 扣费 SQL 自带余额守卫（`balance_cents >= ?`）与下界夹住（`MAX(0, …)`）。
// 这是**最后一道闸**：上游逻辑是「预检拒绝」，正常路径上余额一定够；但
// 万一热路径漏了预检、或预检与扣费之间被并发透支，这里保证余额停在**原值**
// 而不是变负 —— 负余额语义未定义，且会让后续所有「余额不足」判定静默失效
// （`cents >= amount` 恒为假，用户被永久锁死）。
//
// # 余额不足时的行为：不扣 + 报 ErrInsufficientBalance
//
// 刻意**不是**「尽力扣到 0」。理由：预检在前，正常路径根本走不到这里；
// 走到的都是边角情形（漏预检、并发透支）。此时「不扣 + 明确报欠费」比
// 「扣掉一部分 + 报欠费」更好：
//   - 余额是整数分，「扣掉一部分」意味着**扣的金额与 cost_total 不等**，
//     报表说花了 3.00 元、余额只少了 0.80 元 —— 正是这个模块要杜绝的漂移；
//   - 余额守在原处，两种做法下都停在同一个可观察状态（余额为 0 或接近 0），
//     区别只在流水金额是否与报表一致。
//
// # amountCents == 0 时完全 no-op
//
// 未配价模型、usage missing（上游没报 token 数）的记录，cost_total 恒 0。
// 此时**不占位、不 UPDATE**：反复往 balance_charges 塞 0 行只会让表无意义
// 地增长，而余额本来就该纹丝不动。完全 no-op 也让「报表 0 元」与「没扣钱」
// 在账目上天然对齐（都是 0）。
//
// # 不限额用户不扣
//
// balance_cents 为 NULL（不限额）的用户，扣费直接返回 nil 且不动余额。
// 余额无限、减一个数没有意义；更不能把它 COALESCE 成 0 去「扣 0 元」——
// 那会凭空造出一条流水，让对账凭空多出一笔「消费 0 元」。
func (s *Store) ChargeBalance(ctx context.Context, userID string, amountCents int64, requestID string) error {
	if userID == "" {
		return ErrNotFound
	}
	// 0 元 = 完全 no-op：见上方「amountCents == 0 时完全 no-op」。
	if amountCents <= 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	if requestID != "" {
		// 先占位：主键冲突 = 已扣过，幂等返回。
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO balance_charges (request_id, user_id, amount_cents, ts)
			 VALUES (?, ?, ?, ?)`,
			requestID, userID, amountCents, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
			return tx.Commit()
		}
	}

	// 扣费：**一条 UPDATE 原子完成「判余额 + 扣减」**。
	//
	// 为什么必须一条语句（而不是先 SELECT 余额、再 UPDATE 扣）：分两步
	// 就是 check-then-act —— 两个并发请求都读到同一份余额、都判定「够」，
	// 然后各自扣一次，余额被扣成负数。这与 ReserveQuota 把「检查」与
	// 「占用」合并进同一条语句是同一个理由（见 key_dao.go 的注释），差别
	// 只在本项目的策略是预检拒绝、不预占（见文件头）。
	//
	// 三个子句各有职责：
	//   - `balance_cents IS NOT NULL` → **不限额用户不扣**。余额无限、减一个数
	//     没有意义（见文件末的说明）。
	//   - `balance_cents >= ?`       → 余额守卫。够才扣，不够则命中 0 行。
	//     这是并发下不超扣的关键：两个并发请求都在**执行这条语句的那一刻**
	//     被 SQLite 写锁串行化，第二个进来时看到的是第一个扣完的余额。
	//   - `MAX(0, balance_cents - ?)` → 下界夹住（防御纵深）。守卫已保证不会
	//     为负，这行防的是「日后有人放宽守卫」时余额不会静默变负。
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx,
		`UPDATE users
		    SET balance_cents = MAX(0, balance_cents - ?), updated_at = ?
		  WHERE id = ? AND balance_cents IS NOT NULL AND balance_cents >= ?`,
		amountCents, now, userID, amountCents)
	if err != nil {
		return err
	}
	charged, err := res.RowsAffected()
	if err != nil {
		// 无法确认影响行数时不谎报成功：宁可让调用方把这次扣费当失败。
		return err
	}

	// UPDATE 命中 0 行有三种原因，语义完全相反，必须区分后再报错。
	// 区分办法是读回该行的余额（同一事务内，读到的一定是本事务的状态）：
	//   - 行不存在         → ErrNoRows。不是欠费，也不该报错让调用方
	//     重试（重试同样打不中任何行）。按 no-op 收尾。
	//   - balance 为 NULL  → 不限额。正常返回 nil。
	//   - balance < amount  → **余额真的不够**（守卫拦下的那一种）。
	var balance sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&balance)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrNotFound
	case err != nil:
		return err
	case balance.Valid && balance.Int64 < amountCents && charged == 0:
		// 余额确实不够。结算已经发生（上游已被调用、费用已固化进
		// cost_total），钱收不回 —— 报欠费比悄悄放过更诚实。
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrInsufficientBalance
	}
	return tx.Commit()
}

// SetBalance 管理员设置某用户的余额（绝对值）。
//
// 用于管理面「充值/改为某值」。若要「相对增减」用 AdjustBalance。
// unlimited=true 时落 NULL（不限额）。负数一律夹到 0：余额没有负的语义。
// 用户不存在返回 ErrNotFound。
func (s *Store) SetBalance(ctx context.Context, userID string, cents int64, unlimited bool) error {
	if cents < 0 {
		cents = 0
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET balance_cents = ?, updated_at = ? WHERE id = ?`,
		balanceValue(cents, unlimited), time.Now().UnixMilli(), userID)
	return checkAffected(res, err)
}

// AdjustBalance 管理员对余额做**相对增减**（充值用这个更安全）。
//
// 用 `balance_cents + ?` 在 SQL 里原子自增，天然免疫「读-改-写」的
// 丢失更新：两个管理员并发充值都生效，不会后写者覆盖先写者。
//
// 减到负数会被夹在 0（MAX(0, …)）：管理员误操作不应把余额搞成负数
// （负余额会让「余额不足」判定恒真，用户被永久锁死）。需要扣成负
// （如记账欠费）时用 SetBalance 显式设置。
//
// 不限额（NULL）用户做相对增减：COALESCE 把它当 0 起算，于是「充值
// +100 元」会**把不限额切成 100 元**。这是刻意的 —— 管理员给一个不限额
// 用户充值，意图通常正是「给他设个额度」，而静默无操作会让人以为充值
// 失败。若只想充值而保持不限额，请不要调用本方法（余额无限，无可充）。
func (s *Store) AdjustBalance(ctx context.Context, userID string, deltaCents int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents, 0) + ?), updated_at = ?
		  WHERE id = ?`,
		deltaCents, time.Now().UnixMilli(), userID)
	return checkAffected(res, err)
}
