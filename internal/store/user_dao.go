package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// User 是访问密钥的归属主体（多用户改造 P0）。
//
// 为什么需要它：改造前 access_keys 是一组「事实上的全局资源」——
// 谁拿到 key 谁就能用，没有「这是谁的」这层概念。多用户后必须回答
// 「这把 key 属于谁、这个人的额度还剩多少」。
//
// 密码字段只存 PBKDF2 派生的结果，**明文不落库不进内存缓存**，
// 参数与格式见 userauth 包（整套口令体系只有那一处定义）。
type User struct {
	ID          string
	Username    string
	DisplayName string
	// PasswordHash 是 PBKDF2-HMAC-SHA256 的自描述编码串，
	// 格式见 userauth.EncodeHash。空串表示「账号已建出但还没设密码」：
	// 登录必然失败，只能走 /admin/api/bootstrap 首次设置（仅对 admin 意义）。
	PasswordHash string
	// Role 决定管理面能碰什么：admin 管全部，user 只能碰自己的资源。
	Role string
	// Status: active / disabled。禁用后其名下全部 key 立即失效（经快照生效）。
	Status string
	// GroupID 是所属分组（多用户改造 P1）。空串 = 不属于任何组。
	//
	// **空组 = 不限制模型可见性**，不是「什么都看不到」：groups 表为空
	// （未启用分组功能）时全部 key 依旧可见全部模型，与改造前一致。
	// allowed_models_json 非空则再与 key 级白名单求交（只能更紧）。
	GroupID string
	// QuotaTokens 是用户级总额度（token 计量，input+output）。
	// 0 = 不限。与 access_keys.quota_tokens 同口径。
	QuotaTokens int64
	// BalanceCents 是账户余额，单位**分**（人民币，100 分 = 1 元）。
	//
	// # 为什么是整数分而不是浮点元
	//
	// 余额是一个**反复累加**的账目，而浮点的二进制表示无法精确表达十进制
	// 小数：0.1 + 0.2 != 0.3，每加减一次都留一点长尾。几十上百次扣费之后，
	// 尾数累积到「还剩多少钱」的判断开始不可靠 —— 尤其「扣到 0.0000001 元
	// 就停手」这种阈值判断，在浮点下要么因误差永远差一点点而扣不下去，
	// 要么因误差把余额扣成负数。整数分是十进制的**精确**表示，累加无误差，
	// 判零/判负都是整数比较。币种是人民币，与 usage_records.cost_total
	// 同一口径（元），两者相除即得分。
	//
	// # Unlimited=true = 不限额（**不是**「余额为零」）
	//
	// unlimited 的表示是**列值为 NULL**，与 QuotaTokens 的「0 = 不限」刻意
	// 相反。原因：0 在余额语义下是一个**有意义的实数状态** ——「账户里确实
	// 一分钱都没有」，它必须能被表达、且必须触发拒绝。若沿用 quota_tokens
	// 的「0 = 不限」，就无法区分「不限额」与「已欠费」，而这两种状态在业务上
	// 截然不同（前者放行、后者 402），必须用不同的值表示。
	//
	// 这条对比是本字段最容易被后来者踩的地方：看到 0 就当成「不限」会得到
	// 一个**永远能用的欠费账户**。判定一律走 Unlimited 标志，不要看数值。
	BalanceCents int64
	Unlimited    bool
	// BalanceRemainder 是**不足一分**的累计余数（微元，1e-6 元）。
	//
	// 为什么用户对象需要带它：管理面必须把「已扣」与「待结算」都显示出来。
	// 余额只按分扣减，而单价可能远低于一分（实测 3 元/百万 token 时，一次
	// 一万 token 的调用只有 5 厘），于是相当长一段时间里余额纹丝不动 ——
	// 用户会以为没扣钱。只有把余数一起显示，「钱去哪了」才是自洽的。
	//
	// 单位微元、不是分的小数：与 balance_cents 用 INTEGER 而非 REAL 同一
	// 理由（浮点累加留长尾误差），见 store.go 的建表注释。
	BalanceRemainder int64
	// UsedTokens 不由本包维护 —— 见 §4.3 决策：用户级已用量
	// 走实时 SUM 查询，不用触发器（触发器无法感知 key 被删除，
	// 会永久留下偏高的计数）。此字段仅供管理面展示导入用。
	UsedTokens int64
	// AuthVersion 是会话失效栅栏：改密码/禁用/改角色时自增，
	// JWT 里带的 av 与快照里的当前值不一致即 401。
	AuthVersion int64
	Remark      string
	CreatedAt   int64
	UpdatedAt   int64
	LastLoginAt int64
}

// UserRole 取值。
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// UserStatus 取值。
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// IsAdmin 报告该用户是否具备管理面权限。
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// IsActive 报告该用户是否处于可用状态。
func (u *User) IsActive() bool { return u != nil && u.Status == UserStatusActive }

// userColumns 是 users 表的读列清单（含可空列的 COALESCE 兜底）。
//
// 抽成常量是因为原先 ListUsers / GetUser / GetUserByUsername 各写一份，
// 加一列要改三处 —— 漏一处的表现是「某一处 SELECT 少一列，scanUser 报
// 列数不匹配」，而报错点与漏改点不在一处。三处现在共用本常量。
//
// group_id 必须 COALESCE：老库升级后该列对存量行是真 NULL，
// 扫进 string 会报 "converting NULL to string is unsupported"。
//
// ⚠️ **本常量与 scanUser 必须逐列同步**，新增列时两处一起改。手写列清单
// 必然漂移，而漂移的典型后果不是报错而是**静默赋错值**：少一列时
// database/sql 会把后面几列扫进前面的字段（把 balance_cents 扫进
// used_tokens），编译与测试全绿，账目却在暗处对不上。key_dao.go 的 keyColumns
// 注释记的就是同一个教训。
//
// balance_cents 用**布尔表达式** `IS NULL` 直接产出「不限额」标志，而不是
// COALESCE(..,0) 再补一列：前者少一个扫描目标，也免得「NULL 与 0 谁在前面」
// 这种顺序错位（正是上面警告的那类静默错误）。
const userColumns = `id, username, COALESCE(display_name,''), COALESCE(password_hash,''),
		        role, status, COALESCE(group_id,''), quota_tokens, used_tokens, auth_version,
		        COALESCE(remark,''), created_at, updated_at, last_login_at,
		        COALESCE(balance_cents, 0) AS balance_cents,
		        (balance_cents IS NULL) AS balance_unlimited,
		        COALESCE(balance_remainder, 0) AS balance_remainder`

// ListUsers 返回全部用户，按创建时间排序。
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]User, 0, 8)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// GetUser 按 id 取用户；不存在时返回 (nil, nil)。
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// GetUserByUsername 按用户名取用户，供登录使用。
// 用户名即登录标识，故此处大小写不敏感（避免 Admin / admin 两个账号）。
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ? COLLATE NOCASE`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// CountUsers 返回用户总数，供 bootstrap 判定「库是否为空」。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// FindUninitializedAdmin 找出「已建出但还没设过密码」的管理员。
//
// 首次登录引导靠它判定「现在该不该让用户设密码」。条件必须同时包含
// role='admin' **且** password_hash 为空 —— 只看 password_hash 为空会
// 命中任何「管理员为某个人建的空密码账号」，而那些账号的密码由管理员
// 掌握，让本人自助设密等于绕过管理员。
//
// 不存在时返回 (nil, nil) —— 引导已完成是正常状态，不是错误。
func (s *Store) FindUninitializedAdmin(ctx context.Context) (*User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users
	  WHERE role = ? AND password_hash = ''
	  ORDER BY created_at ASC LIMIT 1`, RoleAdmin)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// CreateUser 插入一个用户。用户名冲突由唯一约束报出，
// 调用方应先用 IsUniqueViolation(err) 区分「重名」与「其他故障」。
//
// balance_cents **随创建写入**：新建账号时管理员通常要同时给一个初始余额。
// Unlimited=true 落 NULL（不限额），否则落整数分。与迁移路径的编码一致
// （见 store.ensureColumns 里对 balance_cents 可空性的说明）。
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	now := time.Now().UnixMilli()
	if u.CreatedAt == 0 {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if u.Role == "" {
		u.Role = RoleUser
	}
	if u.Status == "" {
		u.Status = UserStatusActive
	}
	if u.AuthVersion == 0 {
		u.AuthVersion = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, display_name, password_hash, role, status,
		                    group_id, quota_tokens, used_tokens, auth_version, remark,
		                    created_at, updated_at, last_login_at, balance_cents, balance_remainder)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.Status,
		nullIfEmpty(u.GroupID), u.QuotaTokens, u.UsedTokens, u.AuthVersion, u.Remark,
		u.CreatedAt, u.UpdatedAt, u.LastLoginAt, balanceValue(u.BalanceCents, u.Unlimited),
		// 余数建号时恒为 0（新建账号不可能有历史消费）。显式写出而不是让它
		// 走 DEFAULT，是为了让「这一列存在且为 0」与「忘了这一列」在代码里
		// 区分得开 —— 后者会静默丢钱。
		u.BalanceRemainder)
	if err == nil {
		// 回写归一化后的值：落库用的是局部 now，结构体可能仍是零值，
		// 会让创建响应里的 created_at 是 0（与随后 GET 到的同一条不一致）。
		u.UpdatedAt = now
	}
	return err
}

// balanceValue 把 (cents, unlimited) 编码成可直接绑定的列值。
//
// Unlimited → nil（SQL NULL = 不限额），否则落整数分。集中在一处是为了
// 让「NULL 才是不限额」这条约定只有一份实现（CreateUser / SetBalance /
// 迁移的编码必须一致，分散写必然漂移）。见 User.BalanceCents 的注释。
func balanceValue(cents int64, unlimited bool) any {
	if unlimited {
		return nil
	}
	return cents
}

// UpdateUser 更新可管理字段。
//
// 刻意不写 used_tokens —— 它是派生值，改它等于让请求体随意篡改用量计数。
// 修正用法的入口是 RecomputeUserUsage。
//
// 同样刻意**不写 auth_version**：会话失效栅栏只能前进，不能被资料更新碰。
// 本方法处理的调用方（PATCH handler）拿到的是**读库时的旧快照**，若把快照里的
// auth_version 原样写回，读库与落库之间发生的改密（SetUserPassword 已把版本
// 原子递增）会被覆盖回旧值 —— 改密前被窃取的旧 JWT 重新通过校验，等于吊销被
// 静默撤销。需要让既有会话失效的操作（禁用/改角色）走 BumpAuthVersion：
// 它在 SQL 里做相对递增，不依赖任何快照值。
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	u.UpdatedAt = time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET display_name = ?, role = ?, status = ?, group_id = ?,
		                  quota_tokens = ?, remark = ?, updated_at = ?
		 WHERE id = ?`,
		u.DisplayName, u.Role, u.Status, nullIfEmpty(u.GroupID), u.QuotaTokens,
		u.Remark, u.UpdatedAt, u.ID)
	return checkAffected(res, err)
}

// SetUserPassword 单独更新密码哈希，隐式递增 auth_version。
//
// 递增放在 SQL 里而不是让调用方先读后写：改密码与「让旧会话失效」
// 必须是同一个原子动作，分两步会出现「密码改了但旧 JWT 还能用」的窗口。
func (s *Store) SetUserPassword(ctx context.Context, id, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, auth_version = auth_version + 1, updated_at = ?
		 WHERE id = ?`,
		passwordHash, time.Now().UnixMilli(), id)
	return checkAffected(res, err)
}

// SetInitialAdminPassword 为**尚未设过密码的管理员**设置初始密码，返回是否写入。
// false = 目标已有密码、不是管理员或不存在，一个字都没动。
//
// 条件守卫（AND password_hash = ” AND role = 'admin'）是 bootstrap 防线的一部分，
// 不是优化：首次设密的「查库判定 → 写库」若分两步且写入不带条件，两个并发请求
// 都能通过判定、后写者覆盖先写者 —— 「设过即 409」在并发下就成了「最后一个说了算」，
// 而且静默无痕。把判定收敛进 SQL 后，唯一性由数据库原子保证；role 条件让
// 管理员建的空密码**普通用户**账号不可能经这条路径被初始化（那是 FindUninitializedAdmin
// 之外的第二道闸）。
//
// # 事务边界：设密与关闭引导窗口必须原子
//
// 条件 UPDATE 命中（RowsAffected==1）的**同一个事务**里还会写入
// bootstrap_completed 标记（app_settings 表，见 settings_dao.go）：窗口的关闭
// 与密码的写入要么同时生效、要么都不生效。分两步写的话，「设密成功但置标失败」
// 会留下一个**免鉴权窗口仍开着**的库 —— 而密码已经是攻击者（或任何抢先者）
// 设的那把；反过来「置标成功但设密失败」会把真正要引导的人锁在门外。
// 只有真正设上密码（UPDATE 命中）才置标，重复初始化（0 行）不产生任何写入。
func (s *Store) SetInitialAdminPassword(ctx context.Context, id, passwordHash string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, auth_version = auth_version + 1, updated_at = ?
		 WHERE id = ? AND password_hash = '' AND role = 'admin'`,
		passwordHash, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// 窗口没开（已设过 / 非 admin / 不存在）：不写标记，交由调用方回 409。
		return false, nil
	}
	if err := setSettingExec(ctx, tx, settingBootstrapCompletedKey, "1"); err != nil {
		// 整个事务回滚，密码也没设上 —— 调用方按失败重试，窗口保持原状。
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// BumpAuthVersion 递增 auth_version，用于「禁用账号」「改角色」这类
// 必须让既有会话失效、但不动密码的操作。
//
// 它是这类操作的**唯一**失效入口：UpdateUser 从不写 auth_version（见其注释），
// 所以角色/状态变更后必须显式调用本方法；递增在 SQL 里相对进行，
// 不依赖调用方读到的快照值，天然免疫「读旧值写旧值」的竞态。
func (s *Store) BumpAuthVersion(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET auth_version = auth_version + 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return checkAffected(res, err)
}

// TouchUserLogin 记录最后登录时间。失败只该记日志，不该让登录失败 ——
// 这个字段纯粹是给管理员看的辅助信息。
func (s *Store) TouchUserLogin(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// DeleteUser 删除用户。名下access_keys 因外键 ON DELETE CASCADE 一并删除。
//
// 已产生的 usage_records 保留（其 access_key_id 不带外键约束）——
// 用量是历史事实，key 删了也不该抹掉对账依据。
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "users", id)
}

// SumUserUsedTokens 返回该用户名下所有 key 的已用 token 总和。
//
// 为什么不存一个计数器：见 §4.3。触发器按 access_key_id 累加，
// key 被删除时无法回退那份计数——用户会永久撞在 quota 上限上，
// 而管理界面里没有任何修正入口（这正是 key_dao.RecomputeUsedTokens
// 想解决的问题，不该复制到用户层）。
//
// 数据源是 UsageSource（明细 ∪ 日归档）。**这不是优化，是配额正确性**：
// 本函数在热路径上做配额预检（main.go 用户级 429），若只查明细，
// 30 天后每次剪枝都会让这个 SUM 变小，用户配额会**自动清零**并重新获得
// 无限额度 —— 剪枝越勤，配额拦得越松。索引 idx_usage_user_ts 让明细那一支
// 走覆盖扫描；归档那一支按 user_id 列（表 A 主键第二列）。
func (s *Store) SumUserUsedTokens(ctx context.Context, userID string) (int64, error) {
	var used int64
	src, args := UsageSource(UsageFilter{UserID: userID})
	err := s.read.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&used)
	return used, err
}

// RecomputeUserUsage 重算某用户的已用量并回写。
//
// 与 key_dao.RecomputeUsedTokens 同构：SUM 口径天然不会漂移，
// 所以这个入口主要用于「让管理员看到与预检一致的数字」，
// 以及将来若引入分用户累计表时的一次性回填。
//
// 数据源必须是 UsageSource（明细 ∪ 日归档），与 SumUserUsedTokens 同口径：
// 少算归档部分就等于**把这个入口变成「把用户已用量改小」** ——
// 一次误用就永久放宽了该用户的配额，而界面上看不出任何异常。
func (s *Store) RecomputeUserUsage(ctx context.Context, userID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var used int64
	src, args := UsageSource(UsageFilter{UserID: userID})
	if err := tx.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&used); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET used_tokens = ? WHERE id = ?`, used, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		s.logger.Debug("recompute user usage: RowsAffected unavailable", "error", err)
	} else if n == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return used, nil
}

// scanUser 用 COALESCE 承接可空列后落进结构体。
//
// 直接把可空列扫进 string/int 会在列为 NULL 时报
// "converting NULL to string is unsupported"（见 model_dao 的同款注释），
// 故所有可空列在 SELECT 里已用 COALESCE 兜成零值。
//
// 扫描目标必须与 userColumns **逐列对应**（末尾两列是余额，见 User.BalanceCents
// 的注释）：数目或顺序对不上时，database/sql 不会报错而是**把后面几列扫进
// 前面几个字段** —— 编译通过、测试全绿，账目却在暗处对不上。limited 扫成
// int 而非 bool 是为了与 enabled 的既有写法一致（COALESCE 后的 NULL 兜底）。
func scanUser(sc scanner) (*User, error) {
	var u User
	var unlimited int
	if err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.Role, &u.Status, &u.GroupID, &u.QuotaTokens, &u.UsedTokens, &u.AuthVersion,
		&u.Remark, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
		&u.BalanceCents, &unlimited, &u.BalanceRemainder); err != nil {
		return nil, err
	}
	u.Unlimited = unlimited == 1
	return &u, nil
}
