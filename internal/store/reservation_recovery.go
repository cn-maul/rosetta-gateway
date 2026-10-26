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
	"errors"
	"fmt"
	"time"
)

// ErrNoOwnedQuotaReservations means the request had a quota reservation in
// memory but no durable lease row was written. RateCommit uses this signal to
// perform a checked fallback release instead of clearing its counters as if
// the database had been updated.
var ErrNoOwnedQuotaReservations = errors.New("no owned quota reservations")

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
	if _, err := tx.ExecContext(ctx,
		`UPDATE quota_reservation_leases SET released = 1, lease_until = 0
		  WHERE released = 0`); err != nil {
		return 0, fmt.Errorf("close prior-run reservation leases: %w", err)
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

// ReleaseOwnedQuotaReservations releases every outstanding reservation owned
// by requestID in one transaction. A transport or store error leaves the lease
// rows intact so a later retry (or the startup/periodic sweep) can finish the
// complete set. Successful rows are marked released, not immediately deleted,
// so callers can observe exactly-once release evidence.
func (s *Store) ReleaseOwnedQuotaReservations(ctx context.Context, requestID string) error {
	if requestID == "" {
		return ErrNoOwnedQuotaReservations
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit

	type lease struct {
		scope, target string
		reserved      int64
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT scope, target_id, reserved
		   FROM quota_reservation_leases
		  WHERE request_id = ? AND released = 0
		  ORDER BY scope, target_id`, requestID)
	if err != nil {
		return err
	}
	var leases []lease
	for rows.Next() {
		var l lease
		if err := rows.Scan(&l.scope, &l.target, &l.reserved); err != nil {
			rows.Close()
			return err
		}
		leases = append(leases, l)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(leases) == 0 {
		return ErrNoOwnedQuotaReservations
	}
	for _, l := range leases {
		var sqlText string
		switch l.scope {
		case "key":
			sqlText = `UPDATE access_keys
			              SET reserved_tokens = MAX(0, reserved_tokens - ?)
			            WHERE id = ?`
		case "user":
			sqlText = `UPDATE users
			              SET reserved_tokens = MAX(0, reserved_tokens - ?)
			            WHERE id = ?`
		default:
			return fmt.Errorf("unknown quota reservation scope %q", l.scope)
		}
		if _, err := tx.ExecContext(ctx, sqlText, l.reserved, l.target); err != nil {
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE quota_reservation_leases SET last_error = ? WHERE request_id = ? AND scope = ? AND target_id = ?`,
				err.Error(), requestID, l.scope, l.target); uerr != nil {
				s.logger.Warn("record quota release failure also failed",
					"request_id", requestID, "scope", l.scope, "target_id", l.target, "error", uerr)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE quota_reservation_leases
			    SET released = 1, lease_until = 0, last_error = NULL
			  WHERE request_id = ? AND scope = ? AND target_id = ?`,
			requestID, l.scope, l.target); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SweepExpiredQuotaReservations releases only leases whose owning process had
// time to finish. It runs at startup and periodically, so a transient release
// failure does not strand quota until restart.
func (s *Store) SweepExpiredQuotaReservations(ctx context.Context, now time.Time) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM quota_reservation_leases WHERE released = 1 AND lease_until = 0`); err != nil {
		return 0, err
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT request_id
		   FROM quota_reservation_leases
		  WHERE released = 0 AND lease_until < ?`, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	released := 0
	for _, id := range ids {
		if err := s.ReleaseOwnedQuotaReservations(ctx, id); err != nil {
			s.logger.Warn("expired quota reservation sweep failed",
				"request_id", id, "error", err)
			continue
		}
		released++
	}
	return released, nil
}

const (
	quotaReservationLeaseTTL = 30 * time.Minute
)

// RecordQuotaReservationLease persists ownership for a reservation made by the
// current request. Once both key and user reservations have been recorded, the
// finalizer can release the complete set in one transaction and a maintenance
// sweep can later recover only leases whose owner is provably gone.
func (s *Store) RecordQuotaReservationLease(ctx context.Context, requestID, keyID, userID string,
	keyReserved, userReserved int64) error {
	if requestID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit
	now := time.Now().UnixMilli()
	until := time.Now().Add(quotaReservationLeaseTTL).UnixMilli()
	if keyReserved > 0 && keyID != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_reservation_leases
			   (request_id, scope, target_id, reserved, released, lease_until, created_at)
			 VALUES (?, 'key', ?, ?, 0, ?, ?)
			 ON CONFLICT(request_id, scope, target_id) DO UPDATE SET
			   reserved = excluded.reserved,
			   released = 0,
			   lease_until = excluded.lease_until,
			   last_error = NULL`,
			requestID, keyID, keyReserved, until, now); err != nil {
			return err
		}
	}
	if userReserved > 0 && userID != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_reservation_leases
			   (request_id, scope, target_id, reserved, released, lease_until, created_at)
			 VALUES (?, 'user', ?, ?, 0, ?, ?)
			 ON CONFLICT(request_id, scope, target_id) DO UPDATE SET
			   reserved = excluded.reserved,
			   released = 0,
			   lease_until = excluded.lease_until,
			   last_error = NULL`,
			requestID, userID, userReserved, until, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
