package quota

import (
	"context"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type QuotaManager struct {
	store *store.Store
}

func NewQuotaManager(st *store.Store) *QuotaManager {
	return &QuotaManager{store: st}
}

func (qm *QuotaManager) Check(keyID string) (ok bool, reason string) {
	snap := snapshot.Get()
	ks, exists := snap.Keys[keyID]
	if !exists {
		return true, ""
	}

	if !ks.Enabled {
		return false, "key_disabled"
	}

	if ks.QuotaTokens > 0 && ks.UsedTokens >= ks.QuotaTokens {
		return false, "quota_exceeded"
	}

	return true, ""
}

func (qm *QuotaManager) CheckAndDeny(keyID string) (blocked bool, statusCode int) {
	ok, reason := qm.Check(keyID)
	if ok {
		return false, 0
	}

	switch reason {
	case "key_disabled":
		return true, 403
	case "quota_exceeded":
		return true, 429
	default:
		return true, 429
	}
}

func (qm *QuotaManager) RecordUsage(ctx context.Context, keyID string, tokens int64) error {
	if qm.store == nil {
		return nil
	}

	return qm.store.AddUsageTokens(ctx, keyID, tokens)
}

func (qm *QuotaManager) RecordUsageRecord(ctx context.Context, rec *store.UsageRecord) error {
	if qm.store == nil {
		return nil
	}

	if err := qm.store.CreateUsageRecord(ctx, rec); err != nil {
		return err
	}

	return qm.store.AddUsageTokens(ctx, rec.AccessKeyID, rec.TotalTokens)
}

type RateLimiter struct {
	mu       sync.Mutex
	rpm      map[string]*windowCounter
	tpm      map[string]*windowCounter
	rpmLimit int
	tpmLimit int
}

type windowCounter struct {
	count     int64
	windowStart time.Time
}

func NewRateLimiter(rpmLimit, tpmLimit int) *RateLimiter {
	rl := &RateLimiter{
		rpm:      make(map[string]*windowCounter),
		tpm:      make(map[string]*windowCounter),
		rpmLimit: rpmLimit,
		tpmLimit: tpmLimit,
	}

	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) Allow(keyID string, estimatedTokens int64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
 minuteStart := now.Truncate(time.Minute)

	if rl.rpmLimit > 0 {
		wc, exists := rl.rpm[keyID]
		if !exists || !wc.windowStart.Equal(minuteStart) {
			rl.rpm[keyID] = &windowCounter{count: 1, windowStart: minuteStart}
		} else {
			if wc.count >= int64(rl.rpmLimit) {
				return false
			}
			wc.count++
		}
	}

	if rl.tpmLimit > 0 {
		wc, exists := rl.tpm[keyID]
		if !exists || !wc.windowStart.Equal(minuteStart) {
			rl.tpm[keyID] = &windowCounter{count: estimatedTokens, windowStart: minuteStart}
		} else {
			if wc.count+estimatedTokens > int64(rl.tpmLimit) {
				return false
			}
			wc.count += estimatedTokens
		}
	}

	return true
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		minuteStart := now.Truncate(time.Minute)

		for k, wc := range rl.rpm {
			if !wc.windowStart.Equal(minuteStart) {
				delete(rl.rpm, k)
			}
		}
		for k, wc := range rl.tpm {
			if !wc.windowStart.Equal(minuteStart) {
				delete(rl.tpm, k)
			}
		}
		rl.mu.Unlock()
	}
}
