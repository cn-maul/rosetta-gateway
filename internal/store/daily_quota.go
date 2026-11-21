package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const dailyQuotaReservationsDDL = `CREATE TABLE IF NOT EXISTS daily_quota_reservations (
	request_id    TEXT PRIMARY KEY,
	access_key_id TEXT NOT NULL,
	day           TEXT NOT NULL,
	tokens        INTEGER NOT NULL,
	track_tokens  INTEGER NOT NULL,
	started       INTEGER NOT NULL DEFAULT 0,
	actual_tokens INTEGER,
	lease_until   INTEGER NOT NULL
)`

const dailyQuotaLeaseIndexDDL = `CREATE INDEX IF NOT EXISTS idx_daily_quota_lease_until
	ON daily_quota_reservations(lease_until)`

const dailyQuotaUsageTriggerDDL = `CREATE TRIGGER IF NOT EXISTS trg_daily_quota_usage
	AFTER INSERT ON usage_records
	WHEN NEW.request_id IS NOT NULL AND NEW.usage_state NOT IN ('none', 'missing')
	BEGIN
		UPDATE daily_quota_reservations SET actual_tokens = MAX(0, NEW.total_tokens)
		WHERE request_id = NEW.request_id AND access_key_id = NEW.access_key_id AND started = 1;
	END`

// dailyUsageDay 返回本地日界下的当天日期键（'YYYY-MM-DD'）。
//
// 与 usage_daily_rollups.day / GroupByDay 用同一套本地日界：运营看到的
// 「今天用了多少」与报表里的「今天」必须是同一天，否则跨零点前后的数字
// 会对不上，而且没有任何报错。
func dailyUsageDay(now time.Time) string {
	return now.In(time.Local).Format("2006-01-02")
}

// DailyQuotaLimit 是一把密钥在某一天的两档上限（0 = 不限）。
type DailyQuotaLimit struct {
	Requests int64
	Tokens   int64
}

// Unlimited 报告两档都不设限，调用方可以整段跳过。
func (l DailyQuotaLimit) Unlimited() bool {
	return l.Requests <= 0 && l.Tokens <= 0
}

// DailyQuotaReservation 是一次请求在当天计数上占下的份额。
//
// 它不直接对应「已用量」：请求终结时用 SettleDailyQuota 把预占的估算值
// 校正成真实用量。Tokens == 0 表示这次请求没有占用 token 份额（未配
// token 限额），但请求数那一次仍然记了。
type DailyQuotaReservation struct {
	RequestID string
	Day       string
	Tokens    int64
}

// ReserveDailyQuota 为一次请求原子地占用当天的请求数并预占 tokens。
//
// # 为什么每日额度必须落库，而 rpm/tpm 可以留在内存
//
// rpm/tpm 是速率：窗口一分钟一翻，重启最多丢失一个窗口的计数，运营上
// 无害。每日额度是配额：一天一个窗口，重启一次就多出一整天的额度，
// 等于把限制变成「重启即可重置」—— 那和没有限制是一样的。
//
// # 为什么请求数在准入时就记
//
// 「每天最多 N 次请求」必须在放行前判定，否则第 N+1 个请求已经打到上游
// 才发现超限，钱已经花了。所以请求数在这里直接 +1，一次请求只加一次。
//
// token 数则不同：请求前只能估算，真实用量要等上游回报。这里的 tokens
// 是预占值，终结时由 SettleDailyQuota 校正。
//
// # 并发
//
// 判定与占用是同一条 UPDATE（SQLite 单写者），第二个并发请求进来时读到的
// 已是第一个的占用值，不会出现「都读到同一个已用量然后全部通过」。
func (s *Store) ReserveDailyQuota(ctx context.Context, requestID, keyID string, limit DailyQuotaLimit, estTokens int64) (DailyQuotaReservation, bool, error) {
	return s.reserveDailyQuota(ctx, requestID, keyID, limit, estTokens, time.Now())
}

func (s *Store) reserveDailyQuota(ctx context.Context, requestID, keyID string, limit DailyQuotaLimit, estTokens int64, now time.Time) (DailyQuotaReservation, bool, error) {
	day := dailyUsageDay(now)
	res := DailyQuotaReservation{RequestID: requestID, Day: day}
	if limit.Unlimited() {
		return DailyQuotaReservation{}, true, nil
	}
	if requestID == "" {
		return res, false, errors.New("daily quota requires a request ID")
	}
	if estTokens < 0 {
		estTokens = 0
	}
	// 只有配了 token 上限时才预占 token：没配 token 限额的部署不该为它
	// 付一次计数更新，也不该在库里留下一个没人看的数字。
	reservedTokens := int64(0)
	if limit.Tokens > 0 {
		reservedTokens = estTokens
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	// 先确保当天有行，再做条件更新。
	//
	// 顺序不能反：条件 UPDATE 命中 0 行时无法区分「今天还没有行」与
	// 「额度已满」，而这两者的处理完全不同（前者要先插入，后者必须拒绝）。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO access_key_daily_usage (access_key_id, day) VALUES (?, ?)
		 ON CONFLICT(access_key_id, day) DO NOTHING`, keyID, day); err != nil {
		return res, false, fmt.Errorf("ensure daily usage row: %w", err)
	}

	// 判定与占用合并成一条 UPDATE：请求数上限、token 预占上限都在 WHERE 里。
	// 0 = 该档不限，用 `? <= 0` 短路，避免把「不限」写成「必须为 0」。
	upd, err := tx.ExecContext(ctx,
		`UPDATE access_key_daily_usage
		    SET requests = requests + 1,
		        tokens   = tokens + ?
		  WHERE access_key_id = ? AND day = ?
		    AND (? <= 0 OR requests + 1 <= ?)
		    AND (? <= 0 OR tokens + ? <= ?)`,
		reservedTokens, keyID, day,
		limit.Requests, limit.Requests,
		limit.Tokens, reservedTokens, limit.Tokens)
	if err != nil {
		return res, false, fmt.Errorf("reserve daily quota: %w", err)
	}
	n, err := upd.RowsAffected()
	if err != nil {
		return res, false, err
	}
	if n == 0 {
		return res, false, tx.Commit()
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = now.Add(quotaReservationLeaseTTL)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO daily_quota_reservations
		(request_id, access_key_id, day, tokens, track_tokens, lease_until)
		VALUES (?, ?, ?, ?, ?, ?)`, requestID, keyID, day, reservedTokens, limit.Tokens > 0, deadline.Add(5*time.Minute).UnixMilli()); err != nil {
		return res, false, fmt.Errorf("record daily quota reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, false, err
	}
	res.Tokens = reservedTokens
	return res, true, nil
}

// SettleDailyQuota 把本次请求预占的 token 校正为真实用量。
//
// actual < reserved 时退回差额，actual > reserved 时补记差额。请求数**不**
// 调整：一次调用就是一次，无论成败 —— 这与「每天最多 N 次调用」的直觉
// 一致（失败后重试也是一次调用）。
func (s *Store) SettleDailyQuota(ctx context.Context, keyID string, res DailyQuotaReservation, actualTokens int64) error {
	return s.finishDailyQuota(ctx, keyID, res.RequestID, max(0, actualTokens))
}

// ReleaseDailyQuota 退回一次请求占下的全部份额（请求数与 token 预占）。
//
// 只有「已准入但根本没打到上游就失败」的路径才该用它：那种请求不构成
// 一次真实调用，却已经吃掉了当天的一次请求额度。一旦上游已经收到请求
// （哪怕返回错误），就该保留这次计数 —— 上游的额度或费用已经花了。
func (s *Store) ReleaseDailyQuota(ctx context.Context, keyID string, res DailyQuotaReservation) error {
	return s.finishDailyQuota(ctx, keyID, res.RequestID, -1)
}

func (s *Store) StartDailyQuota(ctx context.Context, keyID string, res DailyQuotaReservation) error {
	result, err := s.db.ExecContext(ctx, `UPDATE daily_quota_reservations SET started = 1
		WHERE request_id = ? AND access_key_id = ?`, res.RequestID, keyID)
	if err != nil {
		return err
	}
	return checkAffected(result, nil)
}

func (s *Store) finishDailyQuota(ctx context.Context, keyID, requestID string, actualTokens int64) error {
	if requestID == "" {
		return nil
	}
	return s.RunTx(ctx, func(ctx context.Context, tx execer) error {
		_, err := tx.ExecContext(ctx, `UPDATE access_key_daily_usage AS usage
			SET requests = MAX(0, requests - CASE WHEN lease.started = 0 AND ? < 0 THEN 1 ELSE 0 END),
			    tokens = MAX(0, usage.tokens + CASE
			        WHEN ? >= 0 AND lease.track_tokens = 1 THEN ? - lease.tokens
			        WHEN lease.started = 0 AND ? < 0 THEN -lease.tokens ELSE 0 END)
			FROM daily_quota_reservations AS lease
			WHERE usage.access_key_id = lease.access_key_id AND usage.day = lease.day
			  AND lease.request_id = ? AND lease.access_key_id = ?`,
			actualTokens, actualTokens, actualTokens, actualTokens, requestID, keyID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM daily_quota_reservations WHERE request_id = ? AND access_key_id = ?`, requestID, keyID)
		return err
	})
}

// GetDailyUsage 读一把密钥当天的已用计数（请求数、token）。
// 没有当天记录时返回 0,0。
func (s *Store) GetDailyUsage(ctx context.Context, keyID string) (requests, tokens int64, err error) {
	day := dailyUsageDay(time.Now())
	err = s.read.QueryRowContext(ctx,
		`SELECT requests, tokens FROM access_key_daily_usage
		  WHERE access_key_id = ? AND day = ?`, keyID, day).Scan(&requests, &tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return requests, tokens, err
}

func (s *Store) SweepExpiredDailyQuota(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	err := s.RunTx(ctx, func(ctx context.Context, tx execer) error {
		var err error
		count, err = recoverDailyQuota(ctx, tx, now.UnixMilli())
		return err
	})
	return count, err
}

func recoverDailyQuota(ctx context.Context, tx execer, cutoff int64) (int64, error) {
	_, err := tx.ExecContext(ctx, `UPDATE access_key_daily_usage AS usage
		SET requests = MAX(0, requests - expired.unstarted),
		    tokens = MAX(0, usage.tokens + expired.token_delta)
		FROM (
			SELECT access_key_id, day, SUM(1 - started) AS unstarted,
			       SUM(CASE WHEN started = 0 THEN -tokens
			                WHEN track_tokens = 1 AND actual_tokens IS NOT NULL THEN actual_tokens - tokens
			                ELSE 0 END) AS token_delta
			FROM daily_quota_reservations WHERE lease_until <= ? GROUP BY access_key_id, day
		) AS expired
		WHERE usage.access_key_id = expired.access_key_id AND usage.day = expired.day`, cutoff)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM daily_quota_reservations WHERE lease_until <= ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
