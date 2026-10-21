package store

import (
	"context"
	"testing"
	"time"
)

// 审计 *X4：剪枝后近期表现必须从归档表继续可读，而不是归零。
// 这条用例把明细全部推到保留窗口之外，然后比较剪枝前后的样本数与数值。
func TestGetRecentPerformance_SurvivesPrune(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now()
	freezeNow(t, base)

	local := base.In(time.Local)
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -DefaultRetentionDays)
	ts := cutoff.AddDate(0, 0, -2).Add(9 * time.Hour).UnixMilli()

	for i := 0; i < 3; i++ {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID:           seqName(100 + i),
			Ts:           ts + int64(i),
			UserID:       "u1",
			AccessKeyID:  "k1",
			PublicModel:  "m",
			ProviderID:   "p1",
			Status:       "ok",
			OutputTokens: 100,
			LatencyMs:    1000,
			TTFBMs:       200,
			TotalTokens:  100,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	before, err := st.GetRecentPerformance(ctx, 5, "u1")
	if err != nil {
		t.Fatalf("before prune: %v", err)
	}
	if before.ThroughputSamples != 3 || before.TtfbSamples != 3 {
		t.Fatalf("before prune samples = %+v, want 3/3", before)
	}

	if _, err := st.PruneOldUsage(ctx, DefaultRetentionDays); err != nil {
		t.Fatalf("prune: %v", err)
	}
	after, err := st.GetRecentPerformance(ctx, 5, "u1")
	if err != nil {
		t.Fatalf("after prune: %v", err)
	}
	if after.ThroughputSamples != before.ThroughputSamples || after.TtfbSamples != before.TtfbSamples {
		t.Fatalf("samples changed across prune: before=%+v after=%+v", before, after)
	}
	if after.TokensPerSec != before.TokensPerSec || after.TtfbMs != before.TtfbMs {
		t.Fatalf("metrics changed across prune: before=%+v after=%+v", before, after)
	}
}
