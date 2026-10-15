package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

// ErrBootstrapTokenMismatch 表示引导令牌比对不通过（给错了或没给）。
//
// 为什么用哨兵错误而不是一个额外的 bool 返回值：设密本身已经有一个
// 「没写成」的布尔结果（窗口已关），令牌不匹配塞进同一个 bool 之后，
// 调用方分不清「你输错令牌、窗口还开着、拿正确令牌还能继续」与
// 「窗口已经关了、你怎么重试都没用」—— 这两种情况的处置完全相反。
var ErrBootstrapTokenMismatch = errors.New("bootstrap token mismatch")

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
	return s.setInitialAdminPassword(ctx, id, passwordHash, false, "")
}

// setInitialAdminPassword 是上面那个方法的真实实现，多带一个引导令牌闸门。
//
// # 为什么需要令牌（2026-10-14 上线前审计加固）
//
// 引导窗口是**免鉴权**的，且容器默认监听 0.0.0.0 —— 于是「谁先连到端口，
// 谁就是管理员」。SameOrigin 只能挡浏览器发起的跨站请求，挡不住任何能
// 直接访问该端口的人：curl 没有 Origin，服务端无从分辨是运维还是攻击者。
// 部署到公网/共享网络时这是一条真实的抢注路径。
//
// 令牌由启动流程生成并打印到日志（见 cmd/gateway 的引导令牌生成段），
// 只有部署者拿得到。检查与写入在**同一事务**里：比对通过才写密码，
// 比对不过则 0 行受影响、窗口照旧开着，运维拿正确令牌重试即可。
//
// 必须同事务的理由与「置 bootstrap_completed 标记」完全一致：分成两步，
// 就会出现「密码设上了但令牌没清」的中间态 —— 那一态下攻击者仍持有效令牌，
// 而管理员以为窗口已关。
func (s *Store) setInitialAdminPassword(ctx context.Context, id, passwordHash string,
	requireToken bool, token string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 先读一次窗口状态，再比令牌。
	//
	// # 为什么必须这个顺序
	//
	// 顺序反过来（先比令牌）会稳定地给出**错误诊断**：窗口已关这一态在
	// 库里的表现恰好是「令牌已被消费清空」，于是它落到
	// ErrBootstrapTokenMismatch 上，HTTP 层回 403「令牌不正确」。运维看到
	// 的是「我令牌明明是对的，怎么一直 403」—— 而真实原因是这扇门已经
	// **永久**关上了、怎么重试都没用。两种诊断指向完全相反的处置：
	// 一个让你去核对令牌，一个让你去确认部署状态。
	//
	// 那为什么不干脆把条件 UPDATE 提到最前？因为那等于每发一个**错令牌**
	// 请求就发起一次真实写入（哪怕随后回滚），凭一个免鉴权端点就能反复
	// 触发写事务、放大写锁竞争。只读 SELECT 先判一次，写只留在成功路径。
	//
	// # 它不是守卫，只是诊断
	//
	// 读与写是两个时刻，其间窗口可能被正常流程关掉。**权威判定始终是下面
	// 那条带 AND password_hash = '' 的条件 UPDATE** —— 并发下的「只设一次」
	// 由数据库原子保证。这一点不能因为多了一次读就放松。
	var open int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE id = ? AND password_hash = '' AND role = 'admin'`,
		id).Scan(&open)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 窗口没开（已设过 / 非 admin / 不存在）：不写标记，交由调用方回 409。
		return false, nil
	case err != nil:
		return false, err
	}

	// 令牌闸门：只在「本安装配置了令牌」时生效（老部署与本地直跑没有，
	// 保持升级后行为不变）。比对用恒定时间函数，避免按前缀猜口令。
	if requireToken {
		var want string
		tokErr := tx.QueryRowContext(ctx,
			`SELECT value FROM app_settings WHERE key = ?`, settingBootstrapTokenKey).Scan(&want)
		switch {
		case tokErr != nil && !errors.Is(tokErr, sql.ErrNoRows):
			return false, tokErr
		case tokErr == nil && want == "":
			// 记录存在但为空 = 令牌已被消费（设密成功的那个事务会把它清空）。
			// 走到这里说明引导并未完成，窗口却已经没有令牌了 —— 只可能是
			// 进程在「写密码」与「置 completed 标记」之间被打断，事务已回滚。
			// 绝不能在这里放行：那一态下任何人都能无凭据完成引导。
			return false, ErrBootstrapTokenMismatch
		case tokErr == nil:
			// 恒定时间比较，避免按前缀逐位试探。
			if subtle.ConstantTimeCompare([]byte(want), []byte(token)) != 1 {
				return false, ErrBootstrapTokenMismatch
			}
		}
		// tokErr == ErrNoRows = 库里没有令牌记录 → 不要求令牌（老库兼容路径）。
	}
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
		// 上面那次读通过了、这里却 0 行：两次读之间窗口被关掉了
		// （并发请求先落地）。仍然按 409 走，且不产生任何写入。
		return false, nil
	}
	if err := setSettingExec(ctx, tx, settingBootstrapCompletedKey, "1"); err != nil {
		// 整个事务回滚，密码也没设上 —— 调用方按失败重试，窗口保持原状。
		return false, err
	}
	// 令牌与密码、标记三者同进同退：设密成功即作废令牌，避免它长期留在
	// app_settings 里成为一块过期但看起来很权威的凭据。
	if err := setSettingExec(ctx, tx, settingBootstrapTokenKey, ""); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// SetInitialAdminPasswordWithToken 是带引导令牌闸门的设密入口，供 HTTP 层
// 调用（admin.UserHandler.BootstrapSetup）。
//
// 三种结局必须能被调用方区分开，因为它们对应三种不同的 HTTP 响应：
//   - (true, nil)              写入成功；
//   - (false, ErrBootstrapTokenMismatch) 令牌不对 → 403；
//   - (false, nil)             窗口已关（已设过密码 / 非 admin / 不存在）→ 409。
//
// 为什么不把令牌不匹配也塞进 false：那会让「你输错令牌」与「窗口已经关了」
// 返回同一句话。前者运维需要知道要去日志里核对令牌、窗口其实还开着；
// 后者再怎么重试都没用。同一个 bool 表达两种语义，调用方只能靠猜。
func (s *Store) SetInitialAdminPasswordWithToken(ctx context.Context, id, passwordHash,
	token string) (bool, error) {
	return s.setInitialAdminPassword(ctx, id, passwordHash, true, token)
}

// BootstrapTokenRequired 报告本安装是否配置了引导令牌。
// 为 false 时（老库、本地直跑）BootstrapSetup 放行不带令牌的请求。
//
// 用途只有一个：告诉前端「这个安装的引导窗口要凭令牌才能完成」。
// **不要**用它来做放行/拒绝的判定 —— 那是事务内那次比对的事，
// 拿这里的结果去判会开一个 TOCTOU 窗口（预判通过之后令牌才被消费掉）。
func (s *Store) BootstrapTokenRequired(ctx context.Context) (bool, error) {
	tok, err := s.BootstrapToken(ctx)
	if err != nil {
		return false, err
	}
	return tok != "", nil
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
	err := s.read.QueryRowContext(ctx,
		`SELECT total_tokens FROM user_usage_totals WHERE user_id = ?`, userID).Scan(&used)
	if err == nil {
		return used, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// users 里没有这一行 —— 回退到实时 SUM，保持与旧实现逐字一致的语义。
	//
	// 什么时候会走到：usage_records 对 users **没有外键**（孤儿 key 的历史行
	// 必须能存在，见 store.go 里 usage_records.user_id 的迁移注释），所以
	// user_id 可以指向一个已被删除的用户。此时累加列无从累加，实时 SUM
	// 反而是唯一正确的数据源。
	//
	// 数据面不会走到这里：调用方（main.go 的用户级配额检查）先查快照确认
	// 用户存在才调用本函数。这条分支是为**直接调用方**与测试保底的 ——
	// 代价是一次 O(明细行) 查询，发生在「用户已不存在」这种非热路径上。
	var fallback int64
	src, args := UsageSource(UsageFilter{UserID: userID})
	if qerr := s.read.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&fallback); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return 0, nil // 真的没有任何用量，与旧实现一致
		}
		return 0, qerr
	}
	return fallback, nil
}

// ReserveUserQuota 为一次请求**原子地**预占 est 个 token 的用户级配额。
//
// # 为什么需要它（与 key 级 ReserveQuota 同构）
//
// 修复前的用户级预检是裸读比较：SumUserUsedTokens → used >= quota 判定 → 放行。
// used 只含**已落库**的真实用量，不含尚未终结的在途请求，而这条读-判-放行
// 三步之间没有任何互斥：N 个并发请求会读到同一个 used 并**全部通过**。
// 实测配额 1000、50 并发各估 200 token 时 50/50 全部放行（占用口径 10000），
// 超发 10 倍 —— 用户的总额度上限形同虚设。
//
// 本函数把「检查」与「占用」合并进同一条写事务，并发请求被 SQLite 写锁
// 串行化：第二个进来时读到的 used+reserved 已含第一个的预占。语义与
// key 级 ReserveQuota 逐字一致，两级配额自此不再一个原子一个不原子。
//
// 列分离的必要性同 access_keys.reserved_tokens：used 来自累加列
// user_usage_totals（由 usage_records 触发器维护），预占若混写进去就是双倍记账。
func (s *Store) ReserveUserQuota(ctx context.Context, userID string, est int64) (reserved int64, ok bool, err error) {
	if est <= 0 {
		// 预估为 0（未知长度）时不能直接放行 —— 那等于没有预检；也不能因为
		//「要花 0」就拒绝。折中：按 1 个 token 预占，至少能把并发请求数本身
		// 变成约束。与 ReserveQuota 同处理。
		est = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	// 累加列可能缺行（孤儿 user_id：用户已删但用量行仍在，见 SumUserUsedTokens
	// 的回退分支）。缺行按 used=0 处理，不因此让整次预检失败。
	var quota int64
	err = tx.QueryRowContext(ctx,
		`SELECT quota_tokens FROM users WHERE id = ?`, userID).Scan(&quota)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// quota<=0 = 不限额，与 key 级判定一致；此时不预占也不受限。
	if quota <= 0 {
		return 0, true, tx.Commit()
	}

	var used int64
	// 用**写池的 tx**而非 s.read：读池在 WAL 下可能读到本事务之前的状态，
	// 与 RecomputeUsedTokens 的同一理由 —— 跨事务读会让判定在并发下不可复现。
	// 缺行回退到 0（该用户还没有任何用量）。
	uerr := tx.QueryRowContext(ctx,
		`SELECT total_tokens FROM user_usage_totals WHERE user_id = ?`, userID).Scan(&used)
	if errors.Is(uerr, sql.ErrNoRows) {
		used = 0
	} else if uerr != nil {
		return 0, false, uerr
	}

	var reservedNow int64
	if rerr := tx.QueryRowContext(ctx,
		`SELECT reserved_tokens FROM users WHERE id = ?`, userID).Scan(&reservedNow); rerr != nil {
		return 0, false, rerr
	}

	// 判定必须计入**在途预占**：漏掉 reservedNow，并发的第二个请求会在第一个
	// 收尾前看到假性余量 —— 正是预占要堵住的 check-then-act 窗口。
	if used+reservedNow+est > quota {
		return 0, false, nil
	}
	// 同一事务内把预占推上去。刻意**不写** user_usage_totals：那列是触发器
	// 维护的派生值，这里再写就是双倍记账。
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET reserved_tokens = reserved_tokens + ? WHERE id = ?`, est, userID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return est, true, nil
}

// ReleaseUserQuota 原额退掉一次请求的用户级配额预占（按 reserved 全额退，不补差）。
//
// 真实用量**不在这里记账**：usage_records 落库时触发器已把 total_tokens 累进
// user_usage_totals，这里再按 actual-est 补差就是双倍扣费。与 ReleaseQuota
// 同语义。
//
// MAX(0, …) 夹住下界：重复释放或与其它路径的释放竞争时可能退过头，
// reserved_tokens 变负等于凭空多出额度。
func (s *Store) ReleaseUserQuota(ctx context.Context, userID string, reserved int64) error {
	if reserved <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET reserved_tokens = MAX(0, reserved_tokens - ?) WHERE id = ?`, reserved, userID)
	return err
}

// UserQuotaAndUsed 一次取回「配额 + 已用」，供 /stats 顺带下发。
//
// 2026-10-11：前端的钱包页原先只能从 /me 拿这两个字段，而 /me 是**登录快照**，
// 于是「已用量」在页面上长期停更（前端审计 H1）。
// 把它们并入 /stats 后，界面每次刷新都能拿到当前值。
//
// 为什么单开这个方法而不是让调用方自己查两次：
//   - quota 来自 users 表、used 来自累加列 user_usage_totals，**两张表**；
//   - 已用量刻意复用 SumUserUsedTokens 的口径（那是配额**判定的**数据源），
//     保证「界面显示的已用」与「后端判超额的已用」是同一个数字。
//     若图省事改读 users.used_tokens（展示列），两者会分叉，用户会看到
//     「明明还有额度却被拒」。
//
// 用户不存在时返回 (0, 0, nil)：quota=0 在前端恰好是「不限」的既有语义，
// 与 meResponse 对未知用户的处理同口径。
func (s *Store) UserQuotaAndUsed(ctx context.Context, userID string) (quota, used int64, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT COALESCE(quota_tokens, 0) FROM users WHERE id = ?`, userID).Scan(&quota)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	used, err = s.SumUserUsedTokens(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	return quota, used, nil
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
