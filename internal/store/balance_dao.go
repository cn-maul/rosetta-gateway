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
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// MicrosPerCent 是「一分」对应的微元数（1 元 = 1e6 微元，1 分 = 1e4 微元）。
//
// 换算：1 元 / 100 分 = 1e6 / 1e2 = 1e4。写成 100_000 是错的（那是 0.1 元）。
const MicrosPerCent = 10_000

// YuanToMicros 把「元」换算成「微元」（1e-6 元）的整数。
//
// 存在意义见 balance_remainder 列的注释：单次调用可能只有几毫元
// （0.004 元），按分取整直接变 0；而余额扣减仍以**分**为单位，
// 所以中间需要一个比「分」细、比浮点稳的粒度来攒。
//
// 用 int64 而非 REAL：浮点累加会留长尾误差（与 balance_cents 用 INTEGER
// 是同一个理由，见 store.go 的建表注释）。
//
// 负数与 0 返回 0，与 YuanToCents 同口径：费用没有负的概念。
func YuanToMicros(yuan float64) int64 {
	if yuan <= 0 {
		return 0
	}
	// 先四舍五入到微元，再转 int64。直接 int64(yuan*1e6) 是**截断**：
	// 一次 0.0000019 元的调用会变 0，而它确实是发生的费用。
	micros := math.Round(yuan * 1e6)
	if micros < 1 {
		// 极端小的正费用（低于 0.5 微元）也至少记 1 微元 ——
		// 记 0 等于把这次消费丢掉，而余数机制的意义正是「不丢」。
		//
		// # 这是一处**故意的单边偏差**，要知道它的方向（2026-10-10 实测）
		//
		// 与 YuanToCents 的四舍五入不同：后者的误差零均值（有时多、有时少，
		// 大数下互相抵消），而这里**永远向上取整**，所以偏差是单向的、
		// 随调用次数**线性累积**，不会自愈。
		//
		// 实测：10000 次 × 0.0000001 元（每次真实费用 0.1 微元），
		// 真实合计 0.1 分，实扣 1 分 —— 放大约 10 倍。
		//
		// 为什么仍然选向上取整：两种偏差里，这一种更小也更可控。
		//   - 向上取整：每次最多多收 1 微元（1e-6 元），要累积到 1 分钱
		//     需要一万次「费用低于半微元」的调用 —— 而这种量级的单价
		//     （< 0.5 元/百万 token）在本项目的定价里不现实。
		//   - 记 0：那笔消费**永久消失**，且与「未配价模型记 0」在账上
		//     完全同形，事后无法区分「本来免费」与「漏收」。
		//
		// 若将来出现真正低于 1 微元/次的定价，正确做法是**下沉记账单位**
		// （如微元的千分之一），而不是在这里继续放大 —— 那会把线性偏差
		// 变成不可接受的数量级。
		return 1
	}
	return int64(micros)
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
// BalanceRemainderOf 读某用户当前攒下的不足一分余数（微元）。
//
// 单独一个方法而不是并进 BalanceOf：热路径（余额预检）只需要余额本身，
// 多扫一列是白付的成本；而展示层（总览、/me）要的恰恰是这个值。
//
// 与 BalanceOf 同一口径：用户不存在返回 ErrNotFound，读失败原样上抛。
// **不降级成 0** —— 余数显示成 0 等于告诉用户「钱没被扣」，而真实情况
// 可能是「查不出来」，那正是需要有人看一眼的情况。
func (s *Store) BalanceRemainderOf(ctx context.Context, userID string) (int64, error) {
	var v int64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(balance_remainder, 0) FROM users WHERE id = ?`, userID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

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

// ChargeBalance 扣一次费。amountYuan **必须**等于该次调用的 cost_total
// （元）。扣减余额的单位仍是**整数分**，但入参改成元 —— 因为单次费用可能
// 远小于一分（见下方「余数」一节），用整数分当入参会在函数入口就把
// 这笔费用抹成 0。
//
// # 余数：不足一分的费用怎么收（2026-10-10 修复）
//
// 实测一个 price_input=3.0 元/百万 tokens 的模型，一次 8000 token 的调用
// 只有 0.004 元。旧实现按 `YuanToCents(cost)` = math.Round(0.4) = **0**
// 入参，而 0 分直接 no-op —— 于是**一次都扣不到钱**，无论余额多少。
// 报表却在正常累计费用：实测 6 次调用真实消费 3.33 分、实扣 2 分。
// 这不是「偶尔漏一次」，而是低价模型下**不存在能收上钱的单次调用**，
// 计费对该部署形态整体失效。
//
// 做法：函数内部把费用累进 users.balance_remainder（单位：微元，1e-6 元），
// 每满 MicrosPerCent 个微元（= 1 分，见该常量的定义）才真正扣减余额，
// 并把零头留在余数里继续攒。
// 于是「每次不足一分」变成「延迟到凑够一分时一次性收」：
//   - 账目仍然对齐：余额减少的总量 == 报表费用总量（余数留在余数列里，
//     它同样是可对账的）；
//   - 幂等语义不变：占位行仍然只写一次，重试不会重复累加。
//
// # 幂等：同一 requestID 只扣一次
//
// 网络重试、流式中断重连、上游重试后的重放，都会让**同一次调用**的收尾
// 走两遍。扣费是「减余额」这种不可逆操作，重复执行等于重复扣钱。
//
// 做法是 balance_charges 表以 (request_id, user_id) 为主键（见 store.go 的建表），
// 在**同一事务内**先 INSERT OR IGNORE 占位、再累加余数与扣费：
//   - 占位命中（影响 0 行）说明这个 requestID 对这个用户已扣过 → 直接返回 nil
//     （幂等命中**不是错误**：重试方要的正是「别再扣一次」，回错误只会
//     让调用方把它当成扣费失败而重试更多次），不碰余额；
//   - 占位成功 → 累加余数 → 够一分则扣余额 → 提交。
//
// 主键里带上 user_id 不是冗余：只按 request_id 去重时，**两个不同用户**撞上
// 同一个 request_id，第二个人会被静默跳过 —— 一次真正的扣费凭空消失且无任何
// 报错（2026-10-10 修复的 P0；复合主键由 canonical schema 保证）。
//
// 「先占位再累加、且在同一事务」是不可颠倒的：反过来（先扣后占位）时两个
// 并发重试会各自扣一次、再各占一次位（第二次占位被主键拒绝，但**余额已经
// 扣过了**）—— 主键只挡住了流水重复，没挡住钱重复。
//
// 扣费失败时占位行必须随事务一起回滚：它证明的是「已扣过」，而失败的
// 那次并没有扣。把它提交下去会让此后**每一次**重试都走幂等快速路径、
// 一分钱都不扣（见下方 ErrInsufficientBalance 分支的注释）。
//
// requestID 为空时仍会生成一个单次调用的随机幂等键（*X17）。
// 这不会把两次独立调用误去重，但能保证余额扣减与流水使用同一个键；
// 真实请求恒非空，由 server.Middleware 生成。
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
// # 余额不足时余数会**被回滚丢弃**（2026-10-10 实测确认的已知缺口）
//
// 此处原先写着「余数照留，不因扣费失败而丢弃」—— 那与实现**矛盾**：
// 失败分支走 tx.Rollback()，同一事务内先累加的 balance_remainder
// 会被一并撤销。下面那条分支的注释记了实测结论（调用方拿到
// ErrInsufficientBalance 后只记日志、**不重试**，所以「靠重试重新累加」
// 的说法不成立）。
//
// 实际后果：已欠费用户每次不足一分的零头静默漏收（不扣款、无流水、
// 对账查不出）。严重度有限 —— ≥1 分的部分本就按设计不扣（欠费不硬扣），
// 丢的只是零头。
//
// 正确的修法是让**调用方在欠费时也保留零头**（例如记进一条独立的待结算
// 流水），那是口径变更，未做。要改这里之前先读下面那条分支的完整说明。
//
// # amountYuan <= 0 时完全 no-op
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
// 余数同理不清：不限额用户不需要「攒够再扣」。
func (s *Store) ChargeBalance(ctx context.Context, userID string, amountYuan float64, requestID string) error {
	if userID == "" {
		return ErrNotFound
	}
	// 0 元 = 完全 no-op：见上方「amountYuan <= 0 时完全 no-op」。
	if amountYuan <= 0 {
		return nil
	}

	chargeKey := requestID
	if chargeKey == "" {
		chargeKey = generateChargeKey()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	// 审计 *X17：欠费需要在同一事务里撤销「扣费尝试」并写欠款。
	// savepoint 正好给出这个局部回滚边界：占位、余数累加和余额更新都属于
	// 尝试的一部分；欠款写入属于失败后的终态，不能被它一起撤掉。
	if _, err := tx.ExecContext(ctx, `SAVEPOINT charge_attempt`); err != nil {
		return err
	}
	_, err = s.chargeBalanceTx(ctx, tx, userID, amountYuan, chargeKey)
	if errors.Is(err, ErrInsufficientBalance) {
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO charge_attempt`); rerr != nil {
			return rerr
		}
		if _, rerr := tx.ExecContext(ctx, `RELEASE charge_attempt`); rerr != nil {
			return rerr
		}
		// 占位行与余数累加已被局部回滚，充值后同 request_id 仍能重试。
		// 欠款以同一个幂等键写入；重试成功时再删掉它，避免充值结转后
		// 又被同一次扣费收第二遍。
		if perr := s.recordPendingRemainderTx(ctx, tx, chargeKey, userID,
			YuanToMicros(amountYuan)); perr != nil {
			return perr
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrInsufficientBalance
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE charge_attempt`); err != nil {
		return err
	}

	// 成功扣费（或幂等命中）后，清掉同一调用留下的欠款。正常情况下这里
	// 没有行；它覆盖的是「欠费提交 → 充值/改余额 → 同 request_id 重试成功」
	// 这条恢复路径。若不清理，充值结转和本次补扣会把同一笔钱收两遍。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM balance_pending WHERE request_id = ? AND user_id = ?`,
		chargeKey, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// BalanceCharge is one idempotent charge request in a batch.
type BalanceCharge struct {
	UserID     string
	AmountYuan float64
	RequestID  string
}

// ChargeBalances applies a batch of idempotent balance charges in one write
// transaction. Per-record insufficient-balance outcomes are returned in the
// result slice; infrastructure errors abort the whole transaction and return
// an error so the caller can fall back to the per-record path.
func (s *Store) ChargeBalances(ctx context.Context, charges []BalanceCharge) ([]error, error) {
	errs := make([]error, len(charges))
	if len(charges) == 0 {
		return errs, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // already committed is a no-op

	for i, charge := range charges {
		if charge.UserID == "" {
			errs[i] = ErrNotFound
			continue
		}
		if charge.AmountYuan <= 0 {
			continue
		}
		chargeKey := charge.RequestID
		if chargeKey == "" {
			chargeKey = generateChargeKey()
		}

		sp := fmt.Sprintf("charge_batch_%d", i)
		if _, err := tx.ExecContext(ctx, `SAVEPOINT `+sp); err != nil {
			return nil, err
		}
		_, err := s.chargeBalanceTx(ctx, tx, charge.UserID, charge.AmountYuan, chargeKey)
		if errors.Is(err, ErrInsufficientBalance) {
			if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO `+sp); rerr != nil {
				return nil, rerr
			}
			if _, rerr := tx.ExecContext(ctx, `RELEASE `+sp); rerr != nil {
				return nil, rerr
			}
			if perr := s.recordPendingRemainderTx(ctx, tx, chargeKey, charge.UserID,
				YuanToMicros(charge.AmountYuan)); perr != nil {
				return nil, perr
			}
			errs[i] = ErrInsufficientBalance
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `RELEASE `+sp); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM balance_pending WHERE request_id = ? AND user_id = ?`,
			chargeKey, charge.UserID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return errs, nil
}

// chargeBalanceTx 是 ChargeBalance 的事务内实现。
//
// # 为什么要把它从 ChargeBalance 里拆出来（2026-10-21 修 P0-5）
//
// RecomputeCost 需要在**同一事务**里做三件事：改明细的 cost_total、
// 补记累计表（表 B）、补扣余额。把扣费逻辑留在 ChargeBalance 里就做不到
// —— 那是「开一笔自己的事务」，于是「明细改了、余额没扣」或反之都可能发生。
//
// # chargeKey 是**幂等键**，不是业务上的 request_id（2026-10-21 修 P0-5 的第二层成因）
//
// 落库时的正常扣费用 request_id 当幂等键（同一 request_id 只扣一次）。
// 但「重算补扣」必须用**另一个**键 —— 否则补扣会撞上落库时那次扣费的占位行，
// 命中「已扣过」而直接返回，**一分钱都补不上**。
//
// 实测（2026-10-21 修复前的探针）：落库时按 cost=0.0006 扣过一次（占位行已写），
// 重算把费用改成 0.1004，差额 0.0998 元本该补扣，
// 但因为幂等键相同，chargeBalanceTx 直接返回「已扣过」——
// 余额分文未动，而明细与报表都说这笔该收 0.1004 元。
// 这正是 P0-5「重算只修报表不修余额」的**第二层**成因：既便补扣逻辑
// 写对了，幂等键选错也让它变成空操作。
//
// 补扣的键由 recomputeChargeKey 生成（见那里的说明），它编码了
// 「目标费用」—— 于是：
//   - 同一笔重算重复执行 → 同键 → 幂等命中 → 不重复扣；
//   - 费用再次变化（管理员改价）→ 不同的目标费用 → 不同的键 → 补扣新的差额。
//
// 欠费时本函数**不**回滚、**不**记账，只返回 ErrInsufficientBalance：
// 回滚与 balance_pending 记账都由持有事务的调用方在连接释放后决定
// （理由见函数体内「为什么本函数不自己回滚」）。
// 注意：幂等命中时**不报错、不改余额**，但占位行本身已经写进事务 ——
// 调用方若在同事务里做了别的事，这些事要一并提交还是回滚由调用方决定。
func (s *Store) chargeBalanceTx(ctx context.Context, tx *sql.Tx, userID string, amountYuan float64, chargeKey string) (charged bool, err error) {

	if chargeKey != "" {
		// 先占位：主键冲突 = 已扣过，幂等返回。
		//
		// amount_cents 的初值写死 0，且**它就是最终值**（除非本次费用够一分，
		// 那时下面的分支会把它 UPDATE 成实扣分数）。这不是随手写的默认值：
		// 「不足一分」那条分支依赖它保持 0，所以**不要再补一条把它写成 0 的
		// UPDATE** —— 那是纯冗余的写（2026-10-10 已删除过一次，见下方注释）。
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO balance_charges (request_id, user_id, amount_cents, ts)
			 VALUES (?, ?, 0, ?)`,
			chargeKey, userID, time.Now().UnixMilli())
		if err != nil {
			return false, err
		}
		if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
			// 幂等命中：已经扣过，本次不重复扣。
			return false, nil
		}
	}

	// 累加余数（微元），并**读回**累加后的新值。
	//
	// 必须用一条 UPDATE 的 RETURNING 读法而不是「SELECT 再 UPDATE」：
	// 后者是 check-then-act，两个并发请求都读到同一份余数、各自认为
	// 「还没凑够一分」，于是这一分永远收不上（或者反过来重复收）。
	// balance_remainder 的累加与读回放在同一条语句里，SQLite 写锁保证串行。
	//
	// `balance_cents IS NOT NULL` 把不限额用户挡在外面：余额无限，
	// 不需要「攒够再扣」，余数对他也没有意义（见文件末的说明）。
	var remainder int64
	err = tx.QueryRowContext(ctx,
		`UPDATE users SET balance_remainder = balance_remainder + ?
		  WHERE id = ? AND balance_cents IS NOT NULL
		  RETURNING balance_remainder`,
		YuanToMicros(amountYuan), userID).Scan(&remainder)
	if errors.Is(err, sql.ErrNoRows) {
		// UPDATE 没命中：要么用户不存在，要么是不限额用户。两者语义相反，
		// 必须区分 —— 把「不限额」报成 ErrNotFound 会让上层以为账号有问题。
		var probe sql.NullInt64
		if perr := tx.QueryRowContext(ctx,
			`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&probe); perr != nil {
			if errors.Is(perr, sql.ErrNoRows) {
				return false, ErrNotFound
			}
			return false, perr
		}
		// 行存在但 balance_cents 为 NULL = 不限额：正常 no-op。
		// 余额无限、这笔钱不用扣 —— 但**占位行已经写了**，它表达的是
		// 「这次已经处理过」，对账需要它。
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// 未够一分：余数已攒下，余额不动，流水行的 amount_cents **保持占位时的 0**。
	// 这是正常状态而不是「没扣到钱」——下次凑够时一起收。
	//
	// 2026-10-10（性能）：此处原有一条
	//	UPDATE balance_charges SET amount_cents = 0 WHERE request_id=? AND user_id=?
	// 已删除。它是**纯冗余的写** —— 占位 INSERT 时 amount_cents 写死的就是 0
	// （见上面的 INSERT，第三个参数是字面量 0），这条 UPDATE 只是把已经是 0
	// 的列再写成 0。
	//
	// 为什么值得删：低价模型下单次费用普遍不足一分，**这条语句几乎每笔都
	// 执行**，是纯写放大。实测（Windows，本机）单笔 ChargeBalance 约 171µs，
	// 其中单条 UPDATE...RETURNING 约 50µs —— 删掉一条语句是能测出来的收益。
	//
	// 为什么安全：一笔扣费只走「不足一分」或「够扣的钱」**其中一个**分支，
	// 而唯一会写非 0 的地方是后者（下面的 UPDATE）。两条路径互斥，所以
	// 不足一分时该列恒为占位时的 0。行为完全不变，只是少一次写。
	if remainder < MicrosPerCent {
		// 余数已攒下，余额不动。占位行已写（本次已处理），返回 charged=true
		// 表示「这一笔已入账（金额 0 分，进了余数）」。
		return true, nil
	}

	// 够扣的钱：整分部分从余额扣掉，零头留在余数里继续攒。
	//
	// 取**整除**而不是四舍五入：余数已经保证累积总额是准的，这里若再
	// round 一次就等于把余数的精度优势抵消掉。零头留在余数列，下一轮
	// 继续攒 —— 这正是「不丢一分钱」的关键。
	cents := remainder / MicrosPerCent
	newRemainder := remainder % MicrosPerCent
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance_remainder = ?, updated_at = ? WHERE id = ?`,
		newRemainder, time.Now().UnixMilli(), userID); err != nil {
		return false, err
	}
	// 流水行记**本次实际从余额扣掉的分数**，让对账有据可依。
	if _, err := tx.ExecContext(ctx,
		`UPDATE balance_charges SET amount_cents = ? WHERE request_id = ? AND user_id = ?`,
		cents, chargeKey, userID); err != nil {
		return false, err
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
		cents, now, userID, cents)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		// 无法确认影响行数时不谎报成功：宁可让调用方把这次扣费当失败。
		return false, err
	}

	// UPDATE 命中 0 行有三种原因，语义完全相反，必须区分后再报错。
	// 区分办法是读回该行的余额（同一事务内，读到的一定是本事务的状态）：
	//   - 行不存在         → ErrNoRows。不是欠费，也不该报错让调用方
	//     重试（重试同样打不中任何行）。按 no-op 收尾。
	//   - balance 为 NULL  → 不限额。正常返回 nil。
	//   - balance < cents  → **余额真的不够**（守卫拦下的那一种）。
	// 失败路径必须**回滚**，不能提交（2026-10-10 修复的 P0）。
	//
	// 占位行是「这次已经扣过了」的证据，与扣费结果必须同生共死。
	// 这两条分支一旦提交，就把「其实没扣钱」的占位永久写进了
	// balance_charges：此后**每一次**重试都命中上面的 RowsAffected()==0
	// 快速路径（见 ChargeBalance 开头的幂等说明），直接 return，什么都不扣。
	// 现象是静默的：请求正常返回、报表照常出账，余额却一直不动 ——
	// 而这恰恰是重试路径存在的全部意义被悄悄废掉。
	//
	// 可达性不是理论问题：ErrInsufficientBalance 在余额刚好被并发透支时
	// 就会走到（balance_dao_test.go 的 TestChargeBalance_* 系列也直接覆盖），
	// 随后用户充值重试 —— 钱没扣。
	//
	// 交给 defer 的 Rollback 即可：已提交的分支在 Rollback 里是 no-op。
	//
	// # 回滚会撤销余数累加 —— 那笔零头不能就这么丢了（2026-10-12 修 P1-3）
	//
	// 这个失败分支里，同一事务内先累加的 balance_remainder 会随回滚一起被
	// 撤销。原注释写着「余数照留，由调用方重试时重新累加实现」—— 但实测读了
	// 真实调用方（cmd/gateway/main.go 的 usageRecorder.charge）：它拿到
	// ErrInsufficientBalance 只记一条 ERROR 日志就返回，**没有任何重试**。
	// 那条「调用方会重试」的路径并不存在。
	//
	// 后果：已欠费用户每次不足一分的零头静默漏收（不扣款、无流水、对账查不出）。
	// 严重度有限 —— ≥1 分的部分本就按设计不扣（欠费不硬扣是既定口径），
	// 真正丢的只是零头；但零头随调用次数线性累积，不能当它不存在。
	//
	// # 修法：把零头记进独立账本 balance_pending，并**单独提交**
	//
	// 为什么不在这里直接提交 balance_remainder：那要么让「本次费用」在调用方
	// 重试时被累加两次（若真有重试），要么需要把余数累加挪到事务外 —— 后者
	// 会让并发累加失去原子性（余数用 UPDATE...RETURNING 保证不丢，见上文）。
	//
	// 独立账本同时解决幂等：主键是 (request_id, user_id)，同一个 request_id
	// 重复写入会被 INSERT OR IGNORE 挡掉。所以「调用方将来真的加重试」也不会
	// 重复累积 —— 这正是在欠费这条不稳定路径上必须有的性质。
	//
	// 结转在 AdjustBalanceWithLedger（充值）里做：欠的钱充值后自然该收掉。
	var balance sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&balance)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case balance.Valid && balance.Int64 < cents && rows == 0:
		// 余额确实不够。结算已经发生（上游已被调用、费用已固化进
		// cost_total），钱收不回 —— 报欠费比悄悄放过更诚实。
		// 同样回滚占位：这次没扣成，充值后的重试必须还能扣。
		//
		// 欠费：把事实报回去，**回滚与记账由调用方决定**。
		//
		// ★ 为什么本函数不自己回滚（2026-10-21）
		//
		// 1. 它可能不是事务的**所有者**：chargeBalanceTx 也被
		//    RecomputeCost 调用，而那条路径的事务是外层建的、还要用它写
		//    明细与累计表 —— 在这里回滚会把**外层**的工作一起撤掉。
		// 2. **回滚后不能在本函数里记账**：记账要走 s.db 取连接，而写池是
		//    SetMaxOpenConns(1)、本事务正**独占**那唯一一条连接。回滚之后
		//    连接已释放、才能安全记账（2026-10-12 实测踩到的死锁：
		//    ChargeBalance 挂死、堆栈停在 database/sql.(*DB).conn 的 select）。
		//    所以记账必须由**持有事务的那个调用方**在回滚之后做。
		//
		// 3. 若本函数在事务仍打开时记账，就是自己等自己 —— 死锁。
		//
		// 于是分工是：本函数只报「欠费」；调用方回滚后自行决定要不要
		// 记 balance_pending（见 ChargeBalance 与 RecomputeCost）。
		return false, ErrInsufficientBalance
	}
	return true, nil
}

// recordPendingRemainder 把一笔欠费金额记进 balance_pending 待结算账本。
//
// 独立于 ChargeBalance 的主事务：主事务在这条路径上必然回滚，所以这里
// 用自己的短事务提交，写的行不受那次回滚影响。
//
// 幂等：主键 (request_id, user_id) + INSERT OR IGNORE，重复调用安全。
// 这不是锦上添花 —— 欠费是「余额守卫拦下」这条不稳定路径，未来若给调用方
// 加重试，没有幂等就会把零头累积两次（凭空多收用户钱）。
//
// recordID 为空时不做：没有幂等键就无法保证不重复，只能放弃这一笔
// （与 ChargeBalance 里「空 request_id 不扣费」的口径一致）。
func (s *Store) recordPendingRemainderTx(ctx context.Context, tx *sql.Tx, requestID, userID string, micros int64) error {
	if micros <= 0 {
		return nil
	}
	key := requestID
	if key == "" {
		// 审计 *X17：空 request_id 不能变成「不入账」的理由。生成一个
		// 单次事务内的随机键，既保留唯一约束，也不会把两次独立调用误去重。
		key = "internal-pending-" + generateChargeKey()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO balance_pending (request_id, user_id, amount_micros, ts)
		 VALUES (?, ?, ?, ?)`,
		key, userID, micros, time.Now().UnixMilli())
	return err
}

// generateChargeKey 为没有 request_id 的调用生成单次幂等键。
// 8 字节与 server 的 request_id 同量级；这里只要求同一事务内不撞键，
// 不需要跨进程可追踪。
func generateChargeKey() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败时不能返回空串（会退化成 SQL NULL 一样的行为）。
		// 时间戳至少能保证当前进程内单次调用得到一个非空、近乎唯一的键。
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// pendingRow 是 balance_pending 的一行：某次欠费留下的待结算金额。
//
// requestID 是幂等键（与 balance_charges 同源）—— 精确删除靠它。
type pendingRow struct {
	requestID string
	micros    int64
	userID    string
}

// settlePendingForUser 在**充值**时把该用户的待结算零头并入余额。
//
// 零头本来就是他欠的钱，充值后收掉天经地义，不需要额外的对账流程。
//
// # 必须单事务 + 精确删行（2026-10-21 修 P0-1/2/3/4）
//
// 旧实现是三条**自动提交**语句（SUM → UPDATE → DELETE），中间没有任何互斥：
//
//	SELECT COALESCE(SUM(amount_micros),0) FROM balance_pending WHERE user_id=?
//	UPDATE users SET balance_cents = MAX(0, COALESCE(balance_cents,0) - ?) ...
//	DELETE FROM balance_pending WHERE user_id = ?          ← 整表按 user_id 删
//
// 四个缺陷，全在这三条语句的组合里：
//
//   - **P0-1 重复结转（多收用户的钱）**：两个管理员并发给同一用户充值，
//     各自 SUM 到同一笔欠款、各自扣一次 —— 同一笔欠款被扣两次。
//     实测复现率 59/60（98%）。
//   - **P0-2 崩溃窗口（同一笔欠款被扣两次）**：UPDATE 与 DELETE 之间进程
//     被杀 → 余额已扣、账本还在 → 下次充值再扣一次。这是**确定性**的。
//   - **P0-3 无限额被切成有限额**：`COALESCE(balance_cents,0)` 把 NULL
//     （= 不限额）当成 0，于是结转把「无限额」写回一个非 NULL 整数。
//     实测：不限额用户充值后变成 9700 分的有限额用户。
//   - **P0-4 并发新欠款被无辜抹掉（漏收）**：DELETE 删的是「该用户全部行」，
//     不是「我 SUM 到的那些行」—— 结算期间并发插入的新欠款被一并删掉，
//     而它**从未被扣**。实测复现率 60/60（100%）。
//
// # 现在的形态
//
//  1. **一个事务**包住「读账本 → 扣余额 → 删账本」。SQLite 的写锁把并发
//     充值串行化：第二个事务进来时读到的账本已经是第一个事务清过的。
//     崩溃窗口随之消失（要么全成、要么全不成）。
//  2. **精确删行**：只删本次 SUM 覆盖到的 request_id。并发插入的新欠款
//     自然不被触及（P0-4）。
//  3. **NULL 语义正确**：balance_cents IS NULL（不限额）时**不写这一列**
//     （P0-3）。无限额用户不存在「余额不足」，欠款无从谈起 ——
//     不限额用户本来也进不了欠费分支（ChargeBalance 的守卫排除它），
//     但 SetBalance 可以在欠费**之后**把用户切成不限额，于是 pending 行
//     就成了「给一个无限额用户记账的债务」：那种行应当被**核销**而不是
//     扣款。
//
// # 为什么仍然允许「不足一分的部分直接核销」
//
// 充值是结清时刻，与 balance_remainder 的「攒够才扣」不同 —— 留着它永远
// 不足一分、永远扣不出去，那是纯粹的泄漏。
func (s *Store) settlePendingForUser(ctx context.Context, tx *sql.Tx, userID string) (int64, error) {
	// 读账本：既要总额，也要**具体哪些行**（用于精确删）。
	//
	// 不用 SUM 而用逐行读：精确删行需要行标识，而 SUM 拿不到。
	// pending 的行数是「该用户欠费次数」，量级很小（欠费用户才非零），
	// 全量读入内存没有压力。
	rows, err := tx.QueryContext(ctx,
		`SELECT request_id, amount_micros, user_id FROM balance_pending WHERE user_id = ?
		 ORDER BY ts, request_id`,
		userID)
	if err != nil {
		return 0, fmt.Errorf("read pending: %w", err)
	}
	var items []pendingRow
	var totalMicros int64
	for rows.Next() {
		var it pendingRow
		if err := rows.Scan(&it.requestID, &it.micros, &it.userID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending: %w", err)
		}
		items = append(items, it)
		totalMicros += it.micros
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate pending: %w", err)
	}
	if len(items) == 0 || totalMicros <= 0 {
		return 0, nil
	}

	// 扣款前的余额。
	//
	// NULL = 不限额。理论上不该走到这里（SetBalance 会把用户设成不限额
	// 的同时核销欠款，充值路径也会先把不限额转成有限额），但**防御**一下：
	// 万一余额在本次事务内被改成 NULL，绝不能用 COALESCE 把它当 0 扣成
	// 有限额（P0-3）。核销掉这批行、不扣款。
	var balance sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 用户不存在：账本里的行是孤儿（用户被删了）。FK 是
			// ON DELETE CASCADE，理论上不会留孤儿；但老数据可能没有。
			// 核销掉，不报错 —— 否则一笔历史欠款能永久挡住充值。
			s.logger.Warn("settle pending: user not found, dropping orphan pending rows",
				"user_id", userID, "rows", len(items))
			return 0, s.deletePendingRows(ctx, tx, items)
		}
		return 0, fmt.Errorf("read balance for settle: %w", err)
	}

	if !balance.Valid {
		// 不限额 → 核销（P0-3：绝不把 NULL 写成有限额）。
		if err := s.deletePendingRows(ctx, tx, items); err != nil {
			return 0, err
		}
		s.logger.Info("settle pending: unlimited user, pending rows dropped",
			"user_id", userID, "rows", len(items), "micros", totalMicros)
		return 0, nil
	}

	cents := totalMicros / MicrosPerCent
	if cents > 0 {
		// 守卫：余额不够时不能把余额扣成负数。
		//
		// MAX(0, ...) 是下界夹住（防御纵深），但真正该做的是「只扣得起的
		// 部分」：欠款超过余额时，扣光余额并把剩下的欠款**留在账本里** ——
		// 那是用户还欠的钱，不该被核销掉（旧实现会把超出部分一并删掉，
		// 那正是「用户欠了 10 元、余额 3 元、充值后账本清零」的漏收）。
		deduct := cents
		if deduct > balance.Int64 {
			deduct = balance.Int64
		}
		if deduct > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET balance_cents = MAX(0, balance_cents - ?), updated_at = ?
				  WHERE id = ? AND balance_cents IS NOT NULL`,
				deduct, time.Now().UnixMilli(), userID); err != nil {
				return 0, fmt.Errorf("settle pending: %w", err)
			}
		}
		// 扣光的部分核销，**扣不动的那部分留在账本里**。
		// 逐行删：本次结转能覆盖的行才删，其余留给下次充值。
		//
		// 覆盖规则：从最早的一行开始扣（按 ts 排序 —— 欠费是有先后的，
		// 先欠的先结），扣到余额用完为止。
		var settled []pendingRow
		budget := deduct * MicrosPerCent
		for _, it := range items {
			if budget <= 0 {
				break
			}
			if it.micros <= budget {
				budget -= it.micros
				settled = append(settled, it)
				continue
			}
			// 这一行只能扣一部分：把已扣的部分扣掉，剩下的留账本。
			if budget > 0 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE balance_pending SET amount_micros = amount_micros - ?
					  WHERE request_id = ? AND user_id = ?`,
					budget, it.requestID, userID); err != nil {
					return 0, fmt.Errorf("settle pending partial: %w", err)
				}
				budget = 0
			}
		}
		if len(settled) > 0 {
			if err := s.deletePendingRows(ctx, tx, settled); err != nil {
				return 0, err
			}
		}
		return deduct, nil
	}

	// 总额不足一分：直接核销（充值是结清时刻，见函数头说明）。
	return 0, s.deletePendingRows(ctx, tx, items)
}

// deletePendingRows 精确删除给定的那几行 pending。
//
// **绝不**用 `DELETE ... WHERE user_id = ?`：那会连并发写入的新欠款一起删掉
// （P0-4）。逐个 request_id 删，作用集合与「本次 SUM 覆盖到的行」严格一致。
func (s *Store) deletePendingRows(ctx context.Context, tx *sql.Tx, items []pendingRow) error {
	for _, it := range items {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM balance_pending WHERE request_id = ? AND user_id = ?`,
			it.requestID, it.userID); err != nil {
			return fmt.Errorf("clear pending row: %w", err)
		}
	}
	return nil
}

// dbtx 是 *sql.DB 与 *sql.Tx 的公共最小面（两者都有这三个方法）。
type dbtx interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
	// 设为「不限额」= 免掉该用户的欠费（P0-3 的正解在这里）。
	//
	// 「不限额」的语义是「没有余额约束」—— 既然余额无限，「欠费」就无从
	// 谈起，那笔待结算的欠款是历史遗留（用户当时余额不足）。
	//
	// 为什么必须在这里清、而不是在结转时清：结转只在**充值**时发生，而
	// 「设为不限额」是一条独立的管理写。把「免债」绑在充值上，就变成
	// 「管理员免了债，但必须先充一次钱才生效」—— 而充值本身会把不限额
	// 转成有限额（那是 was_unlimited 字段刻意记录的语义），于是顺序上
	// 互相打架。
	//
	// 旧实现不在这里清，而是让结转去处理，于是结转用
	// COALESCE(balance_cents,0) 把 NULL 当 0 —— 把「无限额」写回一个
	// 非 NULL 整数（用户从无限额度变成有限额 9700 分，方向还可能正好
	// 是 0 → 立刻被余额预检拦成 402）。这是 P0-3 的实际危害。
	if unlimited {
		return s.setBalanceUnlimited(ctx, userID)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET balance_cents = ?, updated_at = ? WHERE id = ?`,
		balanceValue(cents, unlimited), time.Now().UnixMilli(), userID)
	return checkAffected(res, err)
}

// setBalanceUnlimited 把用户设为不限额，并核销其待结算欠款。
//
// 两步在**同一个事务**里：设成不限额与免掉欠款必须同时成立 ——
// 只设不限额而留着欠款，那笔账会在下次充值时被收回来（等于「免掉的债
// 又活了」）；只免欠款而没设不限额，则用户仍然被余额约束。
func (s *Store) setBalanceUnlimited(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET balance_cents = NULL, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), userID)
	if err != nil {
		return err
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		// 用户不存在：与 SetBalance 的既有口径一致（checkAffected 会报
		// ErrNotFound）。这里直接走同一条路径。
		return checkAffected(res, nil)
	}

	// 核销待结算欠款。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM balance_pending WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clear pending for unlimited: %w", err)
	}
	return tx.Commit()
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
