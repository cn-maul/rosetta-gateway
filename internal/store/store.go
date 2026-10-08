package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB // 写池（单连接）
	read   *sql.DB // 读池（WAL 并发读者）
	logger *slog.Logger
}

func Open(dbPath string, logger *slog.Logger) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	// _busy_timeout：写锁被占用时等最多 5 秒再放弃，而不是立刻抛 SQLITE_BUSY。
	// 单进程内 SetMaxOpenConns(1) 已经避免了自争抢，但备份脚本、CLI 工具、
	// 误起的第二个实例都会以独立连接打开同一个文件 —— 那种情况下没有它，
	// 一次写撞锁就直接失败，而配额预检是 fail-open 的，会静默放行超额请求。
	// _synchronous=NORMAL 是 WAL 模式的官方推荐搭配：WAL 下 FULL 只多保护
	// 「掉电丢最近几个已提交事务」这一种情形（不损坏），代价是每次 commit 都
	// fsync —— 本表的用量 INSERT 每请求一条，NORMAL 省掉这笔开销且无完整性风险。
	dsn := dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// 写池：单连接。SQLite 同一时刻只有一个写者，多连接写只会互相撞锁；
	// 用量落库（异步 worker）+ 管理写 + 审计写都在这里排队。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// 读池：WAL 允许任意多读者与单写者并行。用量看板的重查询（全区间聚合、
	// 窗口函数）走读池，不再阻塞写池上的配额预检与用量 INSERT —— 这是读写
	// 分池的全部意义。读多写少的局域网网关给 4 条已绰绰有余。
	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open db (read pool): %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)
	read.SetConnMaxLifetime(0)

	s := &Store{db: db, read: read, logger: logger}
	if err := s.migrate(); err != nil {
		db.Close()
		read.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	errDb := s.db.Close()
	errRead := s.read.Close()
	if errDb != nil {
		return errDb
	}
	return errRead
}

// DB 返回写池（管理面个别查询路径在用）。新代码请用 Reader。
func (s *Store) DB() *sql.DB {
	return s.db
}

// Reader 返回读池：一切只读查询都应走这里，与写池上的写入互不阻塞。
func (s *Store) Reader() *sql.DB {
	return s.read
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS providers (
			id            TEXT PRIMARY KEY,
			slug          TEXT NOT NULL UNIQUE,
			name          TEXT NOT NULL,
			protocol      TEXT NOT NULL DEFAULT 'auto',
			endpoint      TEXT NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			timeout_ms    INTEGER NOT NULL DEFAULT 0,
			max_retries   INTEGER NOT NULL DEFAULT 2,
			quirks_json   TEXT,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS provider_credentials (
			id            TEXT PRIMARY KEY,
			provider_id   TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			label         TEXT,
			api_key_enc   BLOB NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			weight        INTEGER NOT NULL DEFAULT 1,
			status        TEXT NOT NULL DEFAULT 'healthy',
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			last_error    TEXT,
			created_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS upstream_models (
			id                TEXT PRIMARY KEY,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model_id          TEXT NOT NULL,
			display_name      TEXT,
			enabled           INTEGER NOT NULL DEFAULT 1,
			context_window    INTEGER,
			max_output_tokens INTEGER,
			supports_thinking INTEGER,
			price_input       REAL,
			price_cache_hit   REAL,
			price_output      REAL,
			UNIQUE (provider_id, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS routes (
			id                TEXT PRIMARY KEY,
			public_name       TEXT NOT NULL UNIQUE,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
			upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
			enabled           INTEGER NOT NULL DEFAULT 1,
			failover_enabled  INTEGER NOT NULL DEFAULT 0,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS access_keys (
			id              TEXT PRIMARY KEY,
			key_hash        TEXT NOT NULL UNIQUE,
			key_prefix      TEXT NOT NULL,
			name            TEXT NOT NULL,
			enabled         INTEGER NOT NULL DEFAULT 1,
			quota_tokens    INTEGER NOT NULL DEFAULT 0,
			used_tokens     INTEGER NOT NULL DEFAULT 0,
			-- reserved_tokens 是**在途预占**（2026-10-07 计费修复 P0-2）。
			-- 必须与 used_tokens 分列：used_tokens 由 usage_records 的 AFTER INSERT
			-- 触发器累加（真实用量），预占若混写进去，收尾按差值校正时一次请求的
			-- 净记账是 2×actual（每个请求双倍扣费）。判定用 used+reserved+est，
			-- 释放只减 reserved_tokens，两列各自只有一个写入者，口径不再纠缠。
			reserved_tokens INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL
		)`,
		// groups：模型可见性的分组（多用户改造 P1）。
		//
		// 刻意**只有名字**，没有 quota/rpm/tpm 列 —— 那些能力现在没有执行点，
		// 建了列就是「schema 在说谎」（见 dropDeadColumns 的教训）：
		// 运维照着「组级限额」去配，配完发现毫无效果。真要加组级额度时，
		// 与「列 + 管理写入口 + 热路径执行点」三件套一起加。
		`CREATE TABLE IF NOT EXISTS groups (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL COLLATE NOCASE UNIQUE,
			description TEXT,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		// user_group_models：组 → 公开模型名白名单。
		//
		// 为什么单独一张表而不是抄 new-api 的 Ability：Ability 把「可见性」与
		// 「路由」绑在一起，而我们的 routes 是管理员显式配的资源 ——
		// 绑上去会让「改一个组的模型权限」意外改动路由拓扑。
		`CREATE TABLE IF NOT EXISTS user_group_models (
			group_id     TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			public_model TEXT NOT NULL,
			PRIMARY KEY (group_id, public_model)
		)`,
		// users：访问密钥的归属主体（多用户改造 P0）。
		//
		// 管理员账号也在这里（统一认证后 users 表是唯一身份来源，
		// 没有旁路凭据文件）——代价是「删库 = 失明」：库删了，
		// 重启后重新走首次设置密码流程（/admin/api/bootstrap）。
		// 用户表与配额、路由、key 归属强相关，删库时这些数据
		// 本来也要一起消失，账号随库走是自洽的。
		`CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			-- COLLATE NOCASE 让唯一约束大小写不敏感：否则 "admin" 与 "Admin"
			-- 能建成两个账号，而 GetUserByUsername 查时用 NOCASE 又只会命中
			-- 其中一个 ——登录时报「密码错误」，实际是账号名撞车。
			username      TEXT NOT NULL COLLATE NOCASE UNIQUE,
			display_name  TEXT,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			status        TEXT NOT NULL DEFAULT 'active',
			quota_tokens  INTEGER NOT NULL DEFAULT 0,
			used_tokens   INTEGER NOT NULL DEFAULT 0,
			-- balance_cents 是**可空**的账户余额（单位：分，即人民币 0.01 元）。
			--
			-- 为什么是 INTEGER 而不是 REAL：余额是**反复累加**的账目，浮点的
			-- 二进制表示无法精确表达十进制小数，每次加减都留下长尾误差，
			-- 几十上百次扣费后「余额还剩多少」的判断就不可靠了 ——
			-- 尤其「扣到 0.0000001 元」这种阈值判断，浮点下要么恒不成立
			-- （永远扣不下去），要么因误差误判。整数分是十进制的精确表示，
			-- 累加无误差，判零/判负都是整数比较。
			--
			-- 为什么**可空**（NULL = 不限额）而 quota_tokens 用 0 = 不限：
			-- 两者语义相反。quota_tokens 是「额度上限」，0 可以自然地表示
			-- 「不设上限」；而 balance_cents 的 0 是一个**有意义的实数状态** ——
			-- 「账户里确实没钱」。若用 0 = 不限，就无法区分「不限额」与
			-- 「一分钱都没有」，而这两种状态在业务上截然不同（前者放行、
			-- 后者拒绝），必须用**不同的值**表示。SQLite 的 NULL 是唯一
			-- 天然的「此处无值」载体，且 COALESCE 后可安全落进 int64 扫描，
			-- 不会触发 "converting NULL to string/int is unsupported"。
			balance_cents INTEGER,
			-- balance_remainder 是**不足一分**的累计余数（2026-10-10 新增）。
			--
			-- 为什么需要它：单价可能远低于「一分」。实测一个 price_input=3.0
			-- 元/百万 tokens 的模型，一次 8000 token 的调用只有 0.004 元 ——
			-- 而扣费单位是分，math.Round(0.4) = 0，于是 ChargeBalance 对
			-- 0 分直接 no-op。结果是**一次都扣不到钱**，无论余额多少，
			-- 报表却在正常累计费用（实测 6 次调用消费 3.33 分、实扣 2 分）。
			-- 这不是「漏了一次」，是低价模型下**根本不存在能收上钱的单次调用**。
			--
			-- 单位选 **1e-6 元（微元）**而不是「分的小数」：浮点累加会留下
			-- 长尾误差（与 balance_cents 用 INTEGER 而非 REAL 的理由相同），
			-- 而 int64 的微元可以精确表示到百万分之一元，远细于一分。
			-- 满 MicrosPerCent 个微元（= 1 分，见 balance_dao 的常量定义）
			-- 时才折算成 1 分真正扣减余额，零头继续留在本列攒着。
			--
			-- 语义与 balance_cents **刻意不同**：余数对不限额用户无意义
			-- （余额无限，不需要「攒够再扣」），故用 NOT NULL DEFAULT 0
			-- 而不是 NULL —— NULL 在这一列没有第三种语义可表达。
			balance_remainder INTEGER NOT NULL DEFAULT 0,
			auth_version  INTEGER NOT NULL DEFAULT 1,
			remark        TEXT,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			last_login_at INTEGER NOT NULL DEFAULT 0
		)`,
		// rpm_limit / tpm_limit：Key 维度的每分钟限速（DESIGN §11.4），
		// 0 = 不限。曾作为占位列在 2026-09-24 摘除（当时无执行点），
		// 2026-10-04 随限速功能三件套（列 + 管理写入口 + 热路径执行点）加回。
		// expires_at / last_used_at 仍无读写路径，继续由 dropDeadColumns 摘除。
		`CREATE TABLE IF NOT EXISTS usage_records (
			id                TEXT PRIMARY KEY,
			ts                INTEGER NOT NULL,
			access_key_id     TEXT NOT NULL,
			public_model      TEXT NOT NULL,
			provider_id       TEXT NOT NULL,
			upstream_model    TEXT NOT NULL,
			ingress_protocol  TEXT NOT NULL,
			stream            INTEGER NOT NULL,
			input_tokens      INTEGER NOT NULL DEFAULT 0,
			output_tokens     INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
			cached_tokens     INTEGER NOT NULL DEFAULT 0,
			usage_state       TEXT NOT NULL,
			status            TEXT NOT NULL,
			http_status       INTEGER NOT NULL,
			error_code        TEXT,
			latency_ms        INTEGER NOT NULL,
			ttfb_ms           INTEGER NOT NULL DEFAULT 0,
			request_id        TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_records(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_key_ts ON usage_records(access_key_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage_records(public_model, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_prov_ts ON usage_records(provider_id, ts)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key         TEXT PRIMARY KEY,
			value       TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		// route_targets：一条 route 后面的有序上游链。
		// routes.provider_id / upstream_model_id 是「主目标」（position 0）的向后兼容列，
		// 真正的故障转移按本表 position 升序逐个尝试（见 DESIGN §10 的 P2 语义）。
		`CREATE TABLE IF NOT EXISTS route_targets (
			id                TEXT PRIMARY KEY,
			route_id          TEXT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
			provider_id       TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
			upstream_model_id TEXT NOT NULL REFERENCES upstream_models(id) ON DELETE RESTRICT,
			position          INTEGER NOT NULL DEFAULT 0,
			enabled           INTEGER NOT NULL DEFAULT 1,
			created_at        INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_route_targets_route ON route_targets(route_id, position)`,
		// 触发器**先 DROP 再 CREATE**，不用 CREATE ... IF NOT EXISTS。
		//
		// 后者在触发器已存在时**完全跳过**，于是一旦改了触发器体（口径调整、
		// 补字段），老库会永远保留旧体、新库用新体 —— 两边静默分叉，
		// 且不报任何错：totals 悄悄漂移，只有对账时才可能发现。
		// DROP + CREATE 是幂等的（每次都得到代码里这份定义），代价只是
		// 启动时重建一次触发器，可忽略。
		//
		// used_tokens 是这个触发器的专属输出：真实用量落一条记录涨一次。
		// 配额**预占**绝不允许写进 used_tokens —— 那会让一次请求被记两次
		// （预占一次、触发器一次），2026-10-07 的 P0-2 缺陷正是这个成因。
		// 预占走独立的 reserved_tokens 列（ReserveQuota / ReleaseQuota 维护，
		// 见 key_dao.go），本触发器不需要、也不得感知它。
		`DROP TRIGGER IF EXISTS trg_update_used_tokens`,
		`CREATE TRIGGER trg_update_used_tokens
		 AFTER INSERT ON usage_records
		 BEGIN
		   UPDATE access_keys SET used_tokens = used_tokens + NEW.total_tokens WHERE id = NEW.access_key_id;
		 END`,
		// audit_log：管理后台写操作审计（DESIGN §13.3）。只记「谁/何时/动了哪类
		// 资源/动了哪些字段名」，不记请求体值 —— body 里可能有 api_key、密码明文。
		`CREATE TABLE IF NOT EXISTS audit_log (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts      INTEGER NOT NULL,
			actor   TEXT NOT NULL,
			remote  TEXT NOT NULL,
			method  TEXT NOT NULL,
			path    TEXT NOT NULL,
			status  INTEGER NOT NULL,
			fields  TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts)`,
		// ---- 用量归档（P1.5）----
		//
		// 两层结构：表 A 按维度按天归档（支撑趋势与分项统计），
		// 表 B 恒定单行记终身累计（支撑总览「全部」档的大数字）。
		//
		// **表 B 才是「剪枝后累计不变」的保证**：它由触发器在明细落库时
		// 实时累加，与剪枝完全解耦 —— 剪枝只动表 A 与明细表。
		// 表 A 是按维度分组的，「历史总 token」要全表聚合，rollup 行数会随
		// 「天数 × 用户 × key × 模型」增长，所以另设一张 O(1) 读的恒定单行表。
		//
		// day 列统一用**本地时区**（strftime 'localtime'），与前端 GroupByDay
		// 的口径一致 —— 那是用户在界面上看到的那一套。切分点也用本地午夜。
		// 注意 `usage_dao.SumTokensByDayForKey` 用的是 UTC（那是
		// /v1/organization/* 的数据源，与界面无关），两套并存是**已知且刻意**的。
		usageDailyRollupsDDL,
		`CREATE INDEX IF NOT EXISTS idx_rollup_day ON usage_daily_rollups(day)`,
		// ⚠️ 表 B 恒定单行，**必须先插入 (id=1) 占位**。
		// 否则触发器里的 `UPDATE ... WHERE id = 1` 命中 0 行 → 空操作 →
		// 累计值静默丢失，而且没有任何报错（这正是设计文档里记的三个陷阱之一）。
		`CREATE TABLE IF NOT EXISTS usage_totals (
			id               INTEGER PRIMARY KEY CHECK (id = 1),
			request_count    INTEGER NOT NULL DEFAULT 0,
			success_count    INTEGER NOT NULL DEFAULT 0,
			error_count      INTEGER NOT NULL DEFAULT 0,
			input_tokens     INTEGER NOT NULL DEFAULT 0,
			output_tokens    INTEGER NOT NULL DEFAULT 0,
			cached_tokens    INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens     INTEGER NOT NULL DEFAULT 0,
			cost_total       REAL NOT NULL DEFAULT 0,
			latency_sum_ms   INTEGER NOT NULL DEFAULT 0,
			-- 终身最早一条记录的时间。明细剪掉之后，总览「全部」档的起点靠它，
			-- 否则趋势图会凭空丢掉 30 天以前的历史。
			first_record_at  INTEGER NOT NULL DEFAULT 0,
			-- 归档水位：已剪枝到哪一天。0 = 从未剪过。
			pruned_through_day TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO usage_totals (id) VALUES (1)`,
		// balance_charges：扣费流水，按 (request_id, user_id) 幂等去重。
		//
		// 为什么需要它：网络重试与流式中断重连会让**同一次调用**的收尾
		// 走两遍（客户端重试、上游重试后的重放）。若直接 `UPDATE ... SET
		// balance = balance - ?` 幂等性就无从谈起 —— 每次调用都扣一遍，
		// 余额被重复扣减且没有任何提示。
		//
		// 主键是 **(request_id, user_id) 复合键**，不是单独的 request_id
		// （2026-10-10 修复的 P0）。只按 request_id 去重时，两个不同用户
		// 碰巧撞上同一个 request_id，第二个人会被**静默跳过** —— 一次
		// 真正的扣费就这样凭空消失，且没有任何报错。request_id 来源于
		// 客户端可控的请求头，碰撞并不遥远；更要紧的是这条不变量本身就是
		// 错的，「去重」的定义必须是「同一次调用」，而调用属于某个用户。
		// 见 balance_dao.ChargeBalance 的说明与 fixBalanceChargesPK。
		//
		// 在**事务内先占位再扣费**，保证「占位成功但扣费失败」不会留下
		// 重复扣费的窗口（见 balance_dao.ChargeBalance）。
		//
		// amount_cents 记录实际扣掉的金额（含被夹到 0 的情况），便于
		// 事后排查「报表显示花了 X、余额少了 Y」这类漂移。ts 是毫秒时间戳。
		`CREATE TABLE IF NOT EXISTS balance_charges (
			request_id   TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			amount_cents INTEGER NOT NULL,
			ts           INTEGER NOT NULL,
			PRIMARY KEY (request_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_balance_charges_user ON balance_charges(user_id, ts)`,
		// balance_topups：充值流水（管理员给钱），balance_charges 的**反向**一侧。
		//
		// 为什么必须有它：充值此前完全没有留痕 —— AdjustBalance 只做一句
		// UPDATE users SET balance_cents = … 就返回，而 balance_charges 记的
		// 是方向相反的扣费，audit_log 刻意只存字段名不存值（「充了多少」根本
		// 没有被记下来）。于是余额可以凭空增加，而事后没有任何东西能与它
		// 对账。余额是钱，这条流水是「钱进来」这一侧的对账依据。
		//
		// **刻意不写数据迁移**：存量充值已经丢失，补记等于凭空造账 ——
		// 那比「查不到历史充值」糟得多（查不到是已知缺口，造账是假数据）。
		// 本表从启用之日起才可信。
		//
		// 金额一律整数分（与 users.balance_cents 同口径）：对账要求
		// 「流水求和 == 余额变化」严格成立，掺浮点就永远不成立。
		`CREATE TABLE IF NOT EXISTS balance_topups (
			id                TEXT PRIMARY KEY,
			user_id           TEXT NOT NULL,
			delta_cents       INTEGER NOT NULL,
			balance_after     INTEGER NOT NULL,
			-- was_unlimited：本次是否把「不限额」（balance_cents IS NULL）
			-- 切成了有限额。AdjustBalance 对 NULL 用 COALESCE 起算，于是
			-- 「给不限额用户充值 100 元」会把无限变成 100 元 —— 那是**语义
			-- 突变**（对用户是实打实的收紧），不是普通加钱。不记这一列，
			-- 对账时无法区分「充值」与「把无限额度降级成有限额度」。
			was_unlimited      INTEGER NOT NULL DEFAULT 0,
			-- 操作者：余额是钱，「谁给的」必须可追溯到具体账号。用户名冗余
			-- 存一份 —— 账号可能被改名或删除，流水不该因主体消失而失去署名
			-- （与 usage_records 冗余固化 user_id 同一理由）。
			--
			-- operator_id NOT NULL：**没有操作者就��是一次无法追责的改动**，
			-- 不该被允许落库。operator_username 可空（引导态合成管理员没有
			-- 真实用户名，见 user_admin_handler.requireAdmin），读回时 COALESCE
			-- 成空串。
			operator_id        TEXT NOT NULL,
			operator_username  TEXT,
			ts                 INTEGER NOT NULL,
			remark             TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_balance_topups_user ON balance_topups(user_id, ts)`,
	}

	for i, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}

	if err := s.ensureColumns(); err != nil {
		return err
	}
	// 扣费幂等表的主键修复（2026-10-10 P0）：单列 request_id 主键会让两个用户
	// 撞同一个 request_id 时第二个人的扣费被静默跳过 —— 一次真正的扣费凭空消失。
	// 必须排在任何扣费写入之前，且要逐行搬运老数据（那里面是已发生的账目）。
	if err := s.fixBalanceChargesPK(); err != nil {
		return err
	}
	// P1.5 归档流水线。顺序是**刻意的**，别重排：
	//
	//  1. ensureRollupDimensions —— 表 A 的主键可能是旧的 7 列（缺
	//     upstream_model）。SQLite 不能 ALTER 主键，只能整张重建，必须排在
	//     任何「按 8 列做 ON CONFLICT」的写入之前。
	//  2. backfillUsageCost —— 累计表要读 cost_total，存量行必须先补上固化
	//     费用，否则「全部」档的费用永久少算。
	//  3. ensureUsageTotalsTrigger —— 触发器体引用 NEW.cost_total，必须排在
	//     ensureColumns 之后（老库那一列是它补的）。
	//  4. reconcileUsageTotals —— 一次性把「触发器存在之前就已落库」的明细灌进
	//     累计表。排在最后：它读的是补好 cost_total 的明细。
	if err := s.ensureRollupDimensions(); err != nil {
		return err
	}
	if err := s.backfillUsageCost(); err != nil {
		return err
	}
	if err := s.ensureUsageTotalsTrigger(); err != nil {
		return err
	}
	if err := s.reconcileUsageTotals(); err != nil {
		return err
	}
	if err := s.dropDeadColumns(); err != nil {
		return err
	}
	if err := s.backfillRouteTargets(); err != nil {
		return err
	}
	if err := s.retireOrphanKeys(); err != nil {
		return err
	}

	s.logger.Info("database migrations completed")
	return nil
}

// dropDeadColumns 删除「全仓从来没有读写路径」的列。
//   - access_keys 的 expires_at / rpm_limit / tpm_limit / last_used_at 是建表时就
//     写进去的占位能力：没有任何一处读或写它们。
//   - routes.priority 更糟 —— 它在界面上可编辑、文案写着「按 priority 升序择优」，
//     但 runtime 完全不读它，且 public_name 带 UNIQUE 约束使「同名路由择优」
//     在数据层就不可能存在（同名根本插不进去）。真正需要「一个公开模型名后面
//     挂多个上游」的场景由 route_targets 链承担，那是另一套（有序、可故障转移）机制。
//   - routes.extra_json / upstream_models.default_extra_json 存了但没有任何地方拿它
//     构造请求（`ApplyProtocolPrivateExtra` 只管客户端带的协议私有字段）。要接线得先定清
//     语义：链上各目标的协议可能不同，「保留键」校验没法在写入时一次做完；还要定
//     路由级与模型级同时存在时谁覆盖谁。那是功能设计，不是修 bug —— 没有设计就接线，
//     只会把「静默不生效」换成「静默生效但语义不明」。
//   - routes.fallback_route_id 是**已被 route_targets 有序链取代**的单跳兜底
//     （DESIGN §10 明确写了取代关系）。留着它会与链形成两套并行的兜底机制，
//     且界面上没有入口（API 却能写），是典型的「隐形配置」。
//   - routes.max_targets / failure_threshold / stream_first_token_timeout_ms /
//     nonstream_timeout_ms 是原先的**按路由**故障转移策略覆盖。策略参数已统一收进
//     「设置」页（app_settings 的 runtime_defaults），不再按 route 存 ——
//     否则会出现「全局设置了、某条路由却被旧覆盖值悄悄盖住」的排查地狱。
//
// 留着它们的代价不是磁盘，而是 **schema 与界面在说谎**：运维照着「0 = 不过期」
// 「按优先级择优」去配，配完发现毫无效果，只能翻源码才发现没接线。
// 真要实现时用 ensureColumns 加回来即可（ADD COLUMN 比 DROP 简单得多）。
//
// DROP COLUMN 不幂等（列不存在会报错），所以先查 table_info。
func (s *Store) dropDeadColumns() error {
	for _, d := range []struct{ table, column string }{
		// expires_at 曾于 2026-09-24 作为占位列被摘除（当时只有列、没有执行点）。
		// 2026-10-05 随 P2「密钥有效期」加回，列 + 管理写入口 + 鉴权执行点
		// 三件套齐备，因此**移出摘除名单** —— 再删回去就等于让刚做好的功能
		// 「界面能配但不生效」，正是本函数存在的目的所反对的那类状态。
		{"access_keys", "last_used_at"},
		// rpm_limit / tpm_limit 已随限速功能加回（见 ensureColumns），不再摘除。
		{"routes", "priority"},
		{"routes", "extra_json"},
		{"routes", "fallback_route_id"},
		{"routes", "max_targets"},
		{"routes", "failure_threshold"},
		{"routes", "stream_first_token_timeout_ms"},
		{"routes", "nonstream_timeout_ms"},
		{"upstream_models", "default_extra_json"},
	} {
		has, err := s.columnExists(d.table, d.column)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", d.table, d.column, err)
		}
		if !has {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, d.table, d.column)); err != nil {
			// 删列失败不能挡住启动：这些列已经没有任何读写路径，留着只是冗余；
			// 而启动失败会让整个网关不可用（代价完全不成比例）。记 WARN 继续。
			s.logger.Warn("failed to drop obsolete column (ignored)",
				"table", d.table, "column", d.column, "error", err)
		}
	}
	return nil
}

// ensureColumns 为已存在的表补齐新增列。
//
// 为什么单独走一遍而不是塞进 migrations 列表：SQLite 的 ALTER TABLE ADD COLUMN
// 不幂等（列已存在会报错），而本函数在每次 Open 时都会执行。先查 table_info，
// 缺哪列补哪列，老库升级与新库首建都覆盖到。
func (s *Store) ensureColumns() error {
	type col struct {
		table string
		name  string
		def   string // ADD COLUMN 后面的类型与默认值片段
	}
	additions := []col{
		{"routes", "failover_enabled", "INTEGER NOT NULL DEFAULT 0"},
		// Key 维度每分钟限速（DESIGN §11.4，2026-10-04 落地）：0 = 不限。
		{"access_keys", "rpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"access_keys", "tpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		// 配额在途预占列（2026-10-07 计费修复 P0-2）。为什么在 CREATE TABLE 里
		// 写了还要在这里再写一遍：`CREATE TABLE IF NOT EXISTS` 对**已存在**的表
		// 是空操作，老库升级必须靠这里拿到该列 —— 缺了它 ReserveQuota 的
		// SELECT/UPDATE 直接报「无此列」，配额预检 fail-open，终身配额整体失效。
		// NOT NULL DEFAULT 0：ALTER 加列时存量行自动填 0（无在途预占），语义正确。
		{"access_keys", "reserved_tokens", "INTEGER NOT NULL DEFAULT 0"},
		// 模型单价（元 / 百万 tokens），可空：NULL = 未配置价格，统计费用按 0 计。
		// price_input 是「缓存未命中输入」单价；命中的输入另按 price_cache_hit 计
		// （为 0 时回退到 price_input，见 usage_dao 的费用口径）。
		{"upstream_models", "price_input", "REAL"},
		{"upstream_models", "price_cache_hit", "REAL"},
		{"upstream_models", "price_output", "REAL"},
		// 多用户改造 P0：key 归属。
		//
		// 刻意**可空**：存量 key 在迁移后 user_id 为 NULL，表示「无归属」。
		// 硬把存量 key 塞给某个 admin 等于在数据层撒谎 —— 那把 key 的实际
		// 使用者并不是那个账号。
		//
		// 列仍然可空（而不是 NOT NULL）有两个理由：一是 ALTER TABLE 加
		// 非空列必须带默认值，写死一个「无归属」的哨兵值会让语义更糊；
		// 二是「无归属」这个状态在迁移窗口内真实存在。
		//
		// 但**运行期不再接受它**：迁移 retireOrphanKeys 会把无归属 key
		// 置为 enabled=0，鉴权查不到归属用户即返回 ErrKeyUnowned（401）。
		// 早期设计里的「无归属 → 跳过用户级配额、照常可用」已被
		// 2026-10-05 的决策推翻（MULTIUSER.md §5.1 修订），别按旧注释写代码。
		{"access_keys", "user_id", "TEXT REFERENCES users(id) ON DELETE CASCADE"},
		// 分用户配额预检与用量查询走 SUM(... WHERE user_id=?)，缺索引会全表扫。
		// 该列是**冗余固化**的（不靠 JOIN access_keys 回溯）：归属是历史事实，
		// key 被删后用量记录仍在，JOIN 就不成立了。
		{"usage_records", "user_id", "TEXT"},
		// 多用户改造 P1：模型可见性。
		//
		// users.group_id 可空 = 不属于任何组。**空组语义是「不限制」**，
		// 不是「什么都看不到」—— 这是 P1 的向后兼容保证：groups 表为空
		// （未启用分组）时，全部 key 依旧可见全部模型，行为与改造前一致。
		{"users", "group_id", "TEXT REFERENCES groups(id) ON DELETE SET NULL"},
		// key 级模型白名单，存 JSON 数组（如 ["gpt-4o","claude-sonnet"]）。
		// 用 JSON 而不是关联表：key 白名单的量级是「个位数」，读路径是
		// 「随快照整体加载后内存判定」，没有任何按模型反查 key 的需求，
		// 单独建表只多一次 JOIN 和一次级联清理。
		//
		// NULL/空数组 = 不限制。与组白名单求**交**，且 key 级只能更紧。
		{"access_keys", "allowed_models_json", "TEXT"},
		// P2：密钥有效期（毫秒时间戳，0 = 永不过期）。
		// 用 0 而不是 NULL 表示「无限期」，是为了让热路径只做一次数值比较，
		// 不必处理可空列（NULL 扫进 int64 会直接报错）。
		{"access_keys", "expires_at", "INTEGER NOT NULL DEFAULT 0"},
		// P2：来源 IP 白名单（CIDR 或单 IP，逗号分隔；空 = 不限制）。
		//
		// 刻意存**逗号分隔的原文**而不是 JSON：这个字段天然要被人手工编辑
		// （从防火墙规则里抄一段），CSV 风格的可读性比 JSON 重要；
		// 解析在读取时做一次（见 key_dao 的 parseAllowedNets），热路径只遍历
		// 已解析好的 netip.Prefix。
		{"access_keys", "allowed_ips", "TEXT"},
		// P2：key 级分组覆盖。留空 = 沿用归属用户的分组。
		// 只允许管理员设置（见 admin/key_handler），否则用户可以把自己的 key
		// 指向一个更宽松的组来绕过自己组的限制。
		{"access_keys", "group_id", "TEXT REFERENCES groups(id) ON DELETE SET NULL"},
		// P1.5：固化费用。落库时在 Go 侧按**当时**单价算好写进这一列，
		// 之后所有费用查询一律 SUM(cost_total)，不再 JOIN upstream_models
		// 按当前单价重算 —— 那会让「管理员改个价，昨天报表跟着变」。
		//
		// 用 REAL：单价是「元 / 百万 tokens」，本身就是小数。
		{"usage_records", "cost_total", "REAL NOT NULL DEFAULT 0"},
		// P1.5 归档层的补列。为什么在 CREATE TABLE 里写了还要在这里再写一遍：
		// `CREATE TABLE IF NOT EXISTS` 对**已存在**的表是空操作，而 P1.5 的
		// schema 已经在前一轮落过盘（表存在、列不齐）。少了这几条，那批库上的
		// 归档会静默少算 total_tokens / error_count，剪枝则直接报「无此列」。
		{"usage_daily_rollups", "error_count", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_daily_rollups", "total_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_totals", "total_tokens", "INTEGER NOT NULL DEFAULT 0"},
		// 归档水位（已剪到哪一天）。剪枝流程靠它做幂等边界。
		{"usage_totals", "pruned_through_day", "TEXT NOT NULL DEFAULT ''"},
		// 账户余额（单位：分）。为什么在 CREATE TABLE 里写了还要在这里再写一遍：
		// `CREATE TABLE IF NOT EXISTS` 对**已存在**的表是空操作，老库升级必须
		// 靠这里拿到该列 —— 缺了它所有余额 DAO 直接报「无此列」，余额功能
		// 整体不可用。
		//
		// **可空且刻意不带 DEFAULT**：SQLite 的 ALTER TABLE ADD COLUMN 对带
		// DEFAULT 的列会把存量行填成那个默认值，而「余额默认 0」对老用户
		// 是错的 —— 他们升级后应当是「不限额」（NULL），而不是「一分钱没有」，
		// 后者会立刻把所有存量用户挡在门外。老库升级的语义必须是：
		// 「以前没有余额概念，现在也没有 ⇒ 不限额」。因此这里刻意**不带
		// DEFAULT**，存量行迁移后为 NULL。
		{"users", "balance_cents", "INTEGER"},
		// 不足一分的余数（2026-10-10）。DEFAULT 0：存量用户的余数从 0 起算，
		// 这是唯一正确的初值 —— 他们历史上那些「不足一分」的调用已经
		// 永久丢失了，无法也不该凭空补记（那等于凭空多扣用户的钱）。
		{"users", "balance_remainder", "INTEGER NOT NULL DEFAULT 0"},
	}

	for _, a := range additions {
		has, err := s.columnExists(a.table, a.name)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", a.table, a.name, err)
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, a.table, a.name, a.def)); err != nil {
			return fmt.Errorf("add column %s.%s: %w", a.table, a.name, err)
		}
	}

	// 依赖新列的索引必须放在 ensureColumns 之后建。
	// CREATE TABLE IF NOT EXISTS users 里带 FKREFERENCES users(id)，
	// 对**老库**执行 ALTER TABLE ADD COLUMN ... REFERENCES 是允许的
	//（SQLite 允许加带 FK 的列，但要求外键目标存在，所以 users 表的迁移顺序在前）。
	return s.ensureUserIndexes()
}

// ensureUserIndexes 建依赖新列的索引。
//
// 单独拆出来是因为它们必须在 ensureColumns 之后：migrations 列表里的
// CREATE INDEX 跑在 ADD COLUMN 之前，而老库的 usage_records 尚无 user_id 列，
// 建索引会直接报错 —— 而 migrate() 失败意味着**整个网关起不来**。
// 迁移里凡是「依赖新列」的对象都要这样处理。
func (s *Store) ensureUserIndexes() error {
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_usage_user_ts ON usage_records(user_id, ts)`,
		// 按用户列 key：管理面「这个人的所有 key」列表走这条索引。
		`CREATE INDEX IF NOT EXISTS idx_access_keys_user ON access_keys(user_id)`,
		// P1：按组列人（「这个组里有哪些账号」）与 ON DELETE SET NULL 的级联。
		// 注意 group → models 的关联表不需要额外索引，主键 (group_id, public_model)
		// 的最左前缀已经覆盖「取某组的全部白名单」这一唯一查法。
		`CREATE INDEX IF NOT EXISTS idx_users_group ON users(group_id)`,
		// P1.5 归档表（表 A）按用户/密钥维度的聚合。
		//
		// 表 A 的主键是 (day, user_id, access_key_id, …)，所以**按天**的查询
		// 走主键就够（idx_rollup_day 其实是主键前缀的重复，但按天的范围扫
		// 用它更省）。而「这个用户 / 这把 key 的终身用量」——热路径的用户配额
		// 预检正是这个形状——落在主键的**中间列**上，SQLite 无法只用主键前缀
		// 定位，那就会全表扫归档表。表 A 是明细剪掉后的全部历史，扫它和当初
		// 扫明细一样贵，所以这两条索引不是优化而是必需。
		`CREATE INDEX IF NOT EXISTS idx_rollup_user ON usage_daily_rollups(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_rollup_key ON usage_daily_rollups(access_key_id)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("create user index: %w", err)
		}
	}
	return nil
}

// columnExists 用 PRAGMA table_info 判断列是否存在。
// 表名/列名来自上面的常量白名单，不做参数化也不接外部输入，无注入面。
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid   int
			name  string
			ctype string
			nn    int
			dflt  sql.NullString
			pk    int
		)
		if err := rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// backfillRouteTargets 为「还没有任何目标行」的老 route 补一条 position 0 的目标，
// 取该 route 自身的 (provider_id, upstream_model_id)。NOT EXISTS 守卫使其幂等：
// 已经通过后台建过目标链的 route 不会被重复插入。
func (s *Store) backfillRouteTargets() error {
	_, err := s.db.Exec(
		`INSERT INTO route_targets (id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
		 SELECT r.id || '#t0', r.id, r.provider_id, r.upstream_model_id, 0, r.enabled, r.created_at
		 FROM routes r
		 WHERE NOT EXISTS (SELECT 1 FROM route_targets t WHERE t.route_id = r.id)`)
	if err != nil {
		return fmt.Errorf("backfill route_targets: %w", err)
	}
	return nil
}

// retireOrphanKeys 禁用所有「无归属」的存量 key。
//
// # 为什么是禁用而不是删除
//
// 2026-10-05 定：多用户改造后**不发新 key 就不给用** —— 无归属 key 一律失效。
// 但实现上选 `enabled=0` 而非物理删除，理由有三条：
//
//  1. 可逆。删了就是真没了，误操作（迁移逻辑写错、误在生产库上跑）无法挽回。
//     禁用只是把开关拨到 0，管理员确认后可随时打开。
//  2. 用量历史不断。usage_records 里的历史记录按 key_id 归集，
//     key 被物理删除后那些记录就成了悬空的数字，对账时无法解释「这把 key
//     上个月用掉的两百万 token 是谁花的」。
//  3. 责任可追溯。谁在什么时候用过这把 key，依然查得到。
//
// # 为什么不能顺手把它们归给 bootstrap admin
//
// 那等于在数据层说谎：使用这把 key 的人并不是那个引导账号。
// 归属是事实，不能为了让数据「看起来干净」而伪造。
//
// # 幂等
//
// 条件是 `user_id IS NULL AND enabled=1`，每次启动都跑但只影响仍处于
// 「启用且无归属」的行。管理员后来给它补了 user_id 并启用，就不再被碰。
func (s *Store) retireOrphanKeys() error {
	res, err := s.db.Exec(
		`UPDATE access_keys SET enabled = 0 WHERE user_id IS NULL AND enabled = 1`)
	if err != nil {
		return fmt.Errorf("retire orphan keys: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		s.logger.Info("retired unowned access keys; users must be issued new keys",
			"count", n,
			"reason", "多用户改造后无归属密钥不再可用（见 MULTIUSER.md §5.1）",
			"next_step", "为每个使用者建users 账号并重新发 key；旧 key 的用量历史仍可在统计页查看")
	}
	return nil
}
