package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

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
		// 0700 而不是 0755（2026-10-21 修 A10-P1-3）。
		//
		// 库里有 password_hash、access_keys.key_hash、
		// provider_credentials.api_key_enc
		// 与完整的 audit_log。目录是 0755 时，同机的其他本地账号可以
		// 进去把库整个拷走离线爆破。
		//
		// 为什么**不**在启动时把既有目录收紧：改权限是**破坏性**操作 ——
		// 运维可能有意让另一个账号（备份服务、监控 agent）读这个目录，
		// 默默 chmod 0700 会让那些服务在半夜开始失败，且症状与本缺陷
		// 无关（表现为「备份任务不写了」），极难归因。
		//
		// 所以这里的取舍是：**新建的目录**用 0700（收紧只影响我们自己
		// 刚建的目录，不动任何既有部署），**既有的宽权限目录**只告警，
		// 把决定权留给运维。库文件本身见 hardenDBPermissions。
		if err := os.MkdirAll(dir, 0700); err != nil {
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
	if err := s.ensureSchema(context.Background()); err != nil {
		db.Close()
		read.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}

	// 收紧库文件的权限（2026-10-21 修 A10-P1-3）。见 hardenDBPermissions。
	hardenDBPermissions(dbPath, dir, logger)

	return s, nil
}

// hardenDBPermissions 把数据库文件（含 WAL / SHM 副产物）的权限收紧到
// 仅属主可读写，并**告警**目录权限过宽。
//
// # 为什么只对库文件动手、不动目录
//
// 见 Open 里 0700 那段注释：改既有目录的权限可能打断运维有意安排的
// 同机读取方（备份服务等），而症状与本缺陷无关、极难归因。
// 库文件是**我们自己创建的产物**，收紧它不会影响任何外部读者 ——
// 唯一会受影响的是「同机其他账号读库」这件事，而那正是要防的。
//
// # WAL / SHM 为什么也要管
//
// SQLite 的 WAL 模式下，库文件旁边会出现 `gateway.db-wal` 与
// `gateway.db-shm` 两个文件。WAL 里同样装着**已提交但尚未 checkpoint**
// 的行 —— 也就是刚写入的用量与凭据。只收紧主库文件而放过这两个，
// 等于给同机其他账号留了一条同样好用的旁路。
//
// # 权限检查的语义
//
// os.Chmod 在 Windows 上只影响只读位（NTFS 的 ACL 才是权限模型），
// 因此在 Windows 上这一步基本是 no-op —— 那是**平台事实**，不是缺陷：
// 本项目面向的部署环境是 Linux 容器与 Linux 裸机。函数在 Windows 上
// 静默返回，不报错、不告警，避免给开发机带来噪音。
func hardenDBPermissions(dbPath, dir string, logger *slog.Logger) {
	if runtime.GOOS == "windows" {
		// Windows 的权限模型是 NTFS ACL，Chmod 只能改只读位。
		// 强改只读位反而会让 WAL 模式下的正常写入失败。
		return
	}
	// 库文件本身：创建时 SQLite 用的是 umask 派生的权限（常见 0644）。
	// 收紧到 0600。
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil {
			// 文件不存在是常态（WAL 文件在第一次写之前不创建）。
			if !os.IsNotExist(err) {
				logger.Warn("cannot tighten database file permissions",
					"path", p, "error", err)
			}
			continue
		}
	}
	// 目录：只告警，不动手（理由见函数注释）。
	if dir == "" {
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		logger.Warn("database directory is readable by other users on this host; "+
			"consider 'chmod 0700' on it (the database file itself is already 0600)",
			"dir", dir, "mode", fmt.Sprintf("%04o", perm))
	}
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

// Ping 对写池与读池各做一次最轻量的查询，确认 SQLite 连接真的可用。
//
// 供 /healthz 用。此前容器的健康检查只打静态页面，而那条路径只读 embed FS、
// 完全不碰数据库 —— 于是写池被 WAL 或磁盘故障卡住、用量写入持续失败时，
// 容器仍然报 healthy，编排系统既不重启也不告警。
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("write pool: %w", err)
	}
	if err := s.read.PingContext(ctx); err != nil {
		return fmt.Errorf("read pool: %w", err)
	}
	return nil
}
