package store

// 余额充值流水（balance_topups）。
//
// # 为什么需要这张表
//
// 充值此前**完全没有留痕**。AdjustBalance 只做一句
// `UPDATE users SET balance_cents = …` 就返回，于是「谁给谁充了多少钱」
// 这件事在原理上无法回答：
//
//   - balance_charges 记的是**扣费**（方向相反的减法），不是充值；
//   - audit_log 刻意**只存字段名不存值**（audit_dao.go 的注释说明了理由：
//     body 里可能有 api_key 与密码明文），所以它能证明「有人调过充值接口」，
//     却说不出「充了多少、给了谁」。
//
// 这在钱上是个不可接受的缺口：余额可以凭空增加（管理员手误、误操作、
// 或将来任何一条写错 user_id 的代码），而事后没有任何东西能与之对账。
// 本表把「余额增加」这一侧补齐，让用户的钱有完整的进出两条流水。
//
// # 与 balance_charges 的分工
//
//	balance_charges  扣费（系统收钱）  按 (request_id, user_id) 幂等
//	balance_topups   充值（管理员给钱）无幂等需求，每次调用就是一次操作
//
// 两者方向相反、触发方不同、频率差几个数量级，合并成一张表只会让「这个数
// 是加的还是减的」变成一个需要看调用方才能回答的字段。
//
// # 金额一律整数分
//
// 与 users.balance_cents 同口径。充值流水是**对账依据**，一旦掺进浮点，
// 「流水加起来等于余额变化」这条验收就永远无法成立（0.1+0.2!=0.3）。

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"math/rand"
	"time"
)

// Topup 是**一次**余额充值/扣减操作的事后记录。
//
// 注意方向：delta 为正=充值（钱进来），为负=扣减（钱出去）。
// 这与 balance_charges 的 amount_cents 语义相反 —— 后者恒为非负，
// 因为那里记录的是「扣了多少」而不是「余额变化了多少」。
type Topup struct {
	// ID 是 32 位十六进制主键，与网关各处 ID 同构（见 generateTargetID）。
	ID string `json:"id"`
	// UserID 是**被调整余额**的用户，不是操作者。操作者在 OperatorID。
	//
	// 两者必须分列：把操作者写进 user_id 会让「这个人的充值记录」查询
	// 答非所问（那是「谁充过钱」，不是「这个人被充过多少钱」），
	// 而钱包页要的显然是后者。
	UserID string `json:"user_id"`
	// DeltaCents 是本次相对调整量（分）。正=充值，负=扣减。
	//
	// 刻意存**相对量**而不是「调整前/调整后两个绝对值」：余额是相对变动，
	// 流水要回答的问题是「发生了什么变化」，而不是「当时有多少钱」。
	// 只存 balance_after 的话，链式相减才能还原每一笔 —— 中间任何一笔
	// 丢失都会让整条链错位。
	DeltaCents int64 `json:"delta_cents"`
	// BalanceAfter 是**本次调整之后**的余额（分），由 UPDATE ... RETURNING
	// 直接读回，不是「旧值 + delta」算出来的。
	//
	// 为什么必须回读：并发充值时「读旧值 → 加 delta」算出来的数会与真实
	// 余额不符（另一个管理员可能刚好在中间改过）。流水里的 balance_after
	// 是对账时的锚点，算错了整条链就废了。
	//
	// 局限（刻意接受）：并发下它只保证**本次自己的结果**是权威值，不保证
	// 与相邻那条流水首尾相接 —— 两次并发充值的落库顺序由 SQLite 写锁决定，
	// 与 ts 的毫秒精度无关。逐笔核对应以 delta_cents 求和，而非逐行比
	// balance_after。
	BalanceAfter int64 `json:"balance_after"`
	// WasUnlimited 记录「本次是否把一个不限额用户切成了有限额」。
	//
	// **必须有这一列**：AdjustBalance 对 NULL 余额用
	// `COALESCE(balance_cents, 0) + ?` 起算，于是「给不限额用户充值 100 元」
	// 会把「无限」变成「100 元」—— 这是**语义突变**，不是普通的加钱。
	// 不记这一列，事后对账只看到「+100.00 元」，无法区分
	// 「给一个本来有额度的人充值」与「把一个无限额度的人降级成了有限额度」。
	// 后者对用户是实打实的限制收紧，性质与前者完全不同。
	WasUnlimited bool `json:"was_unlimited"`
	// OperatorID / OperatorUsername 记录**谁操作的**。
	//
	// 余额是钱，给钱是管理行为，「谁给的」必须可追溯到具体账号 ——
	// 只记时间与金额的话，出了事只能知道「有人动过」，不知道「是谁动的」。
	// 用户名冗余存一份：账号可能被改名或删除，流水不该因为主体消失就失去
	// 署名（与 usage_records 冗余固化 user_id 同一个理由）。
	OperatorID       string `json:"operator_id"`
	OperatorUsername string `json:"operator_username"`
	// Ts 是毫秒时间戳，与全仓其它时间字段同口径。
	Ts int64 `json:"ts"`
	// Remark 是管理员可选的备注（充值原因、工单号…）。可空。
	Remark string `json:"remark"`
}

// topupColumns 是 balance_topups 的统一读列清单。
//
// 抽成常量的理由与 user_dao.userColumns / key_dao.keyColumns 完全一致：
// 手工维护多份 SELECT 必然漂移，而漂移的表现不是报错而是**静默赋错值**
// （少一列时 database/sql 把后面几列扫进前面的字段，编译与测试全绿，
// 账目却在暗处对不上）。
const topupColumns = `id, user_id, delta_cents, balance_after, was_unlimited, ` +
	`operator_id, COALESCE(operator_username,''), ts, COALESCE(remark,'')`

// generateTopupID 生成 32 位十六进制主键，与 generateTargetID 同构。
func generateTopupID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AdjustBalanceWithLedger 调整余额并**记一条充值流水**。
//
// # 为什么是两条语句而不是一个事务（这是本函数最需要说清的一处取舍）
//
// 一眼看着，把「改余额」与「记流水」放进同一个 BeginTx 显然更整齐：要么都成、
// 要么都不成，不会出现「钱加了但没流水」。**但本函数刻意不做**，理由有三条，
// 按重要性排：
//
//  1. **不能改变 AdjustBalance 的并发语义。** AdjustBalance 用
//     `balance_cents + ?` 做原子自增，天然免疫丢失更新，
//     balance_dao_test.go 的 TestAdjustBalance_ConcurrentTopUpsDoNotLoseUpdates
//     钉住了这一点。包进事务本身不改变这条语句的原子性（仍是单条 UPDATE），
//     但会让**整个调用**持有写事务直到 Commit —— 而写池是单连接
//     （SetMaxOpenConns(1)），持锁期间所有用量落库、扣费、管理写全部排队。
//     充值虽然是低频操作，但把它做成一个可能长时间持锁的事务没有收益。
//
//  2. **流水写失败不该让充值看起来失败。** 余额已经改了 —— 那是既成事实，
//     管理员看到「充值失败」却发现余额其实变了，比「充值成功但流水缺一笔」
//     更糟：前者会诱导他**再充一次**，那就是真的多充了钱。
//     宁可漏记（可事后审计发现），不可让操作者对已生效的改动产生误解。
//
//  3. **流水是事后账，不是准入闸门。** 它不参与任何判定
//     （余额预检读 users.balance_cents，不看本表），因此它的短暂缺失
//     不会造成错误放行或错误拒绝 —— 与 quota/balance 这类**判定依据**
//     的原子性要求不在一个量级。
//
// 代价是明确的：**余额改了而流水没写**是一个可达状态。所以流水写失败必须
// 记 ERROR 日志（含 user_id / delta / operator），让运维能发现并手工补记。
// 若将来要求「绝不允许缺流水」，正确做法是加一个「待写流水」的对账任务，
// 而不是把低频管理写绑进长事务。
//
// # 返回值语义（重要）
//
// **余额已改但流水写失败时，本函数返回 (Topup, nil) —— 即调用方看到成功。**
// 这是刻意的：见上面第 2 点。此时错误已经以 ERROR 记进日志，调用方
// 无法也无需据其改变响应（余额确实变了，报错反而误导）。
// 返回的 Topup 携带已生成的流水字段（含 ID），供将来接对账任务复用。
//
// # balance_after 为什么用 RETURNING 而不是读两次
//
// 「先 SELECT 旧值 → UPDATE → SELECT 新值」在并发下会读到他人的中间态：
// 另一个管理员的充值可能正好插在两次读之间，于是回读到的新值不是本次的结果。
// `UPDATE ... RETURNING` 把「改」与「读回改后值」压进**同一条语句**，
// 由 SQLite 写锁保证两者之间没有别的写入 —— 这与 balance_dao 里
// balance_remainder 的累加读回是同一个理由。
func (s *Store) AdjustBalanceWithLedger(ctx context.Context, userID string, deltaCents int64,
	operatorID, operatorUsername, remark string) (Topup, error) {

	// 0 是合法的「调平」，但作为一次操作毫无意义，且更可能是调用方把空输入
	// 当成了 0。与 AdjustBalance 的语义保持一致：这里拦下并报调用方 bug。
	// （管理面 handler 另有更具体的 400 文案，这里是包内的最后一道闸。）
	if deltaCents == 0 {
		return Topup{}, errors.New("delta_cents 不能为 0")
	}
	if userID == "" {
		return Topup{}, ErrNotFound
	}

	now := time.Now().UnixMilli()

	// 调整**之前**读一次旧值。
	//
	// 为什么必须前置读、而不是从 RETURNING 的新值反推：RETURNING 只能给出
	// 新值，而 was_unlimited 判定的是**旧值**为 NULL 这件事。事后反推
	// （「新值 == delta 就是突变」）是错的 —— 一个本来就 0 分的用户被充值
	// 到恰好等于 delta 的值时同样满足该等式，却并不是语义突变。这种
	// 「碰巧相等」无法与真突变区分开。
	//
	// 这条前置读与 UPDATE 之间可能有并发（另一个管理员同时在动这个用户），
	// 那种情况下 was_unlimited 可能记错方向。但它记错的**后果**被严格限制：
	// 这一列只用于事后审计的分类说明，不参与任何判定、不影响余额、
	// 不影响预检。宁可分类标签偶尔不准，也不为此把低频管理写绑进长事务
	// （理由见函数注释）。
	//
	// 读不到行 = 用户不存在：直接返回 ErrNotFound，**不写流水**。
	// 流水是为一次**已发生**的余额变化留的痕，没发生的事不留痕。
	var oldBalance sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&oldBalance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Topup{}, ErrNotFound
		}
		return Topup{}, err
	}

	// 改余额 + 读回改后值，压进**同一条语句**：写锁保证两者之间没有别的写入，
	// 因此读到的就是「本次自己的结果」，不会是他人的中间态。
	var after int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE users
		    SET balance_cents = MAX(0, COALESCE(balance_cents, 0) + ?), updated_at = ?
		  WHERE id = ?
		 RETURNING balance_cents`,
		deltaCents, now, userID).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return Topup{}, ErrNotFound
	}
	if err != nil {
		return Topup{}, err
	}

	t := Topup{
		ID:           generateTopupID(),
		UserID:       userID,
		DeltaCents:   deltaCents,
		BalanceAfter: after,
		// 旧值是 NULL = 这次把「无限额度」变成了有限额。COALESCE 起算后新值
		// 恒非 NULL，所以「有没有发生语义突变」完全由旧值决定。
		WasUnlimited:     !oldBalance.Valid,
		OperatorID:       operatorID,
		OperatorUsername: operatorUsername,
		Ts:               now,
		Remark:           remark,
	}

	if err := s.insertTopup(ctx, t); err != nil {
		// 余额已经改了。**绝不**把错误往上抛成「充值失败」—— 见函数注释
		// 第 2 点：那会诱导管理员重复充值。记 ERROR 让运维能发现并补记。
		s.logger.Error("balance adjusted but topup ledger write failed (MUST reconcile manually)",
			"error", err, "user_id", userID, "delta_cents", deltaCents,
			"balance_after", after, "operator_id", operatorID)
		return t, nil
	}
	return t, nil
}

// insertTopup 写一条流水。单独拆出是为了让「写流水失败」能被调用方
// 明确识别为**可恢复**的失败（余额已生效），而不是与余额调整的错误混为一谈。
func (s *Store) insertTopup(ctx context.Context, t Topup) error {
	wasUnlimited := 0
	if t.WasUnlimited {
		wasUnlimited = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO balance_topups (id, user_id, delta_cents, balance_after, was_unlimited,
		                             operator_id, operator_username, ts, remark)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.DeltaCents, t.BalanceAfter, wasUnlimited,
		t.OperatorID, nullIfEmpty(t.OperatorUsername), t.Ts, nullIfEmpty(t.Remark))
	return err
}

// ListTopupsByUser 分页列出某用户的充值流水，按时间**倒序**（新→旧）。
//
// userID 为空时返回**空切片**而非全部：与 ListAccessKeysByUser 同一个理由 ——
// 「无归属」不该被当成「查所有」，否则一个漏传 userID 的调用会把所有人的
// 充值记录（金额 + 操作者署名）一并暴露出去。
//
// 倒序：钱包页展示的是「最近发生过什么」，新记录在最上面。
//
// 次序键用 `ts DESC, rowid DESC` 而不是 `ts DESC, id DESC`：ts 是毫秒
// 精度，而**同一毫秒内的多笔充值完全可能发生**（批量充值、或测试里连续
// 调用）。用随机十六进制 id 做次序键，同一毫秒内的顺序就是随机的 ——
// 翻页时同一条记录可能在第 1 页和第 2 页各出现一次，还可能漏掉某条。
// rowid 是 SQLite 的隐式自增行号（本表是 TEXT 主键、非 WITHOUT ROWID，
// 所以 rowid 存在），它随插入单调递增，正是分页需要的稳定次序键。
//
// 走读池：纯查询，且可能与写池上的充值写入并发（WAL 下互不阻塞）。
func (s *Store) ListTopupsByUser(ctx context.Context, userID string, limit, offset int64) ([]Topup, error) {
	if userID == "" {
		return []Topup{}, nil
	}
	// 上限钳制：与 usage_handler.clampLimit 同口径。SQLite 的 `LIMIT -1`
	// 语义是「不限制」，一个漏钳的上游参数就是一次全表物化。
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+topupColumns+`
		   FROM balance_topups
		  WHERE user_id = ?
		  ORDER BY ts DESC, rowid DESC
		  LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Topup, 0, 16)
	for rows.Next() {
		var t Topup
		var wasUnlimited int
		if err := rows.Scan(&t.ID, &t.UserID, &t.DeltaCents, &t.BalanceAfter, &wasUnlimited,
			&t.OperatorID, &t.OperatorUsername, &t.Ts, &t.Remark); err != nil {
			return nil, err
		}
		t.WasUnlimited = wasUnlimited == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAllTopups 返回**全站**充值流水，按时间倒序，仅管理员使用。
//
// # 为什么需要它（2026-10-10）
//
// 管理员的钱包页要展示的是「我给所有用户充过多少钱」的台账，而
// ListTopupsByUser 只能取 me.ID 名下那一份 —— 管理员不能被充值
// （自充值被后端拒绝），于是那一份**恒为空**，需求落不了地。
//
// 所以这里补一条全站查询，与上面按用户查询并存而不是替换它：
// 普通用户那条路径（ListTopupsByUser）依然按会话身份收窄，
// 全站视图只有 handler 层确认过 IsAdmin 之后才会走到这里。
//
// 次序键与上限钳制与 ListTopupsByUser 逐字一致（ts DESC, rowid DESC），
// 否则两个端点对同一条记录的排序会不一致，翻页会出现漏/重。
func (s *Store) ListAllTopups(ctx context.Context, limit, offset int64) ([]Topup, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+topupColumns+`
		   FROM balance_topups
		  ORDER BY ts DESC, rowid DESC
		  LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Topup, 0, 16)
	for rows.Next() {
		var t Topup
		var wasUnlimited int
		if err := rows.Scan(&t.ID, &t.UserID, &t.DeltaCents, &t.BalanceAfter, &wasUnlimited,
			&t.OperatorID, &t.OperatorUsername, &t.Ts, &t.Remark); err != nil {
			return nil, err
		}
		t.WasUnlimited = wasUnlimited == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountAllTopups 返回全站流水总条数，供管理员钱包页算分页页数。
func (s *Store) CountAllTopups(ctx context.Context) (int64, error) {
	var n int64
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM balance_topups`).Scan(&n)
	return n, err
}

// CountTopupsByUser 返回某用户的流水总条数，供前端算分页页数。
//
// 空 userID 返回 0，与 ListTopupsByUser 的空切片约定保持一致。
func (s *Store) CountTopupsByUser(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	var n int64
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM balance_topups WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}
