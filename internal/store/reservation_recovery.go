package store

// 预占崩溃恢复（2026-10-21 修 P0-9）。
//
// # 问题
//
// `access_keys.reserved_tokens` 与 `users.reserved_tokens` 是**终身**预占：
// 它们没有「窗口结束」这回事，只有在请求正常收尾时由
// ReleaseQuota / ReleaseUserQuota 原额退回。
//
// 进程崩溃或被强杀（SIGKILL、OOM、容器被杀）时，收尾代码不会执行 ——
// 那些预占就**永久留在库里**。而终身配额的判定是
// `used + reserved + est <= quota`，于是这份残留直接吃掉该 key / 该用户
// 的终身额度。实测：一次崩溃 + 一个 est=2000 的预占就能让一把
// quota=10000 的 key 少掉 20% 的额度；反复崩溃则额度完全冻结。
//
// 恢复只能靠人工改库 —— 这类「需要 DBA 介入」的故障本不该存在。
//
// # 为什么启动时清零是安全的
//
// 预占的语义是「**本进程**正在处理的请求占掉的额度」。进程重启后，
// 上一进程的所有请求都不复存在：
//   - 成功完成的请求：真实用量已由 usage 落库触发器累加进
//     used_tokens，不需要预占来记账；
//   - 未完成的请求：上游可能已经计费了，但这条用量本来就进不了账
//     （usage 记录由请求收尾时落库，进程已死）—— 保留它的预占等于
//     为一笔**永远不会入账**的消耗额度。
//
// 所以「启动时把所有 reserved_tokens 清零」与「预占的真实语义」完全
// 一致：没有进程在途，就没有预占。
//
// # 边界：多实例部署
//
// 本项目是**单进程**网关（DESIGN §2：单写连接、单进程内存态熔断）。
// 多实例共享同一个 SQLite 文件本身就不被支持（写锁串行化会让吞吐塌到
// 单写者的水平，且熔断状态在各实例间不一致）。因此「启动即清零」不会
// 误伤别的实例的在途预占。
//
// 若将来真要支持多实例，必须把这一条换成**带租约的预占**
// （reserve 记 expires_at，回收只清过期行）—— 那是另一个设计，不是本函数。

import (
	"context"
	"fmt"
)

// recoverStaleReservations 把所有 key / 用户的在途预占清零。
//
// 调用点在 ensureSchema 成功之后、网关开始接流量之前 —— 这保证「清零」与
// 「开始记账」之间没有并发写入。
//
// 返回被清掉的预占总额，仅用于日志。
func (s *Store) recoverStaleReservations(ctx context.Context) (int64, error) {
	// 先读总额再清，是为了让日志能说明「这次救回了多少额度」。
	// 读与写之间没有并发（此刻还没接流量），所以这个数不会失真到需要
	// 精确 —— 它只是给人看的。
	var keys, users int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(reserved_tokens), 0) FROM access_keys`).Scan(&keys); err != nil {
		return 0, fmt.Errorf("read stale key reservations: %w", err)
	}
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(reserved_tokens), 0) FROM users`).Scan(&users); err != nil {
		return 0, fmt.Errorf("read stale user reservations: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin reservation recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	if _, err := tx.ExecContext(ctx,
		`UPDATE access_keys SET reserved_tokens = 0 WHERE reserved_tokens > 0`); err != nil {
		return 0, fmt.Errorf("clear key reservations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET reserved_tokens = 0 WHERE reserved_tokens > 0`); err != nil {
		return 0, fmt.Errorf("clear user reservations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reservation recovery: %w", err)
	}

	if keys > 0 || users > 0 {
		s.logger.Warn("recovered stale quota reservations from a previous run",
			"key_tokens", keys, "user_tokens", users,
			"reason", "进程重启后没有在途请求，预占必须清零否则终身额度被永久冻结")
	}
	return keys + users, nil
}
