package probe

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestClassify 是健康判定的边界表。这是特性③的核心逻辑，每个分支都要钉住。
//
// 判定口径（见 Classify 注释）：
//   - 窗口无记录 → unknown
//   - 连续失败 ≥ downThreshold → down
//   - 最近成功且窗口成功率 ≥ healthyRate → healthy
//   - 其余 → degraded
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		h        store.ProbeHealth
		down     int
		rate     float64
		expected Health
	}{
		{"无记录", store.ProbeHealth{WindowCount: 0}, 3, 0.8, HealthUnknown},
		{"连续失败达阈值", store.ProbeHealth{WindowCount: 3, WindowOK: 0, ConsecutiveFail: 3, LastOK: false}, 3, 0.8, HealthDown},
		{"连续失败超阈值", store.ProbeHealth{WindowCount: 5, WindowOK: 1, ConsecutiveFail: 4, LastOK: false}, 3, 0.8, HealthDown},
		{"单次失败未达阈值", store.ProbeHealth{WindowCount: 1, WindowOK: 0, ConsecutiveFail: 1, LastOK: false}, 3, 0.8, HealthDegraded},
		{"最近成功且成功率达标", store.ProbeHealth{WindowCount: 10, WindowOK: 9, ConsecutiveFail: 0, LastOK: true}, 3, 0.8, HealthHealthy},
		{"最近成功但成功率偏低", store.ProbeHealth{WindowCount: 10, WindowOK: 5, ConsecutiveFail: 0, LastOK: true}, 3, 0.8, HealthDegraded},
		{"最近失败但窗口曾成功", store.ProbeHealth{WindowCount: 10, WindowOK: 8, ConsecutiveFail: 2, LastOK: false}, 3, 0.8, HealthDegraded},
		{"成功率恰好在阈值", store.ProbeHealth{WindowCount: 10, WindowOK: 8, ConsecutiveFail: 0, LastOK: true}, 3, 0.8, HealthHealthy},
		{"成功率略低于阈值", store.ProbeHealth{WindowCount: 10, WindowOK: 7, ConsecutiveFail: 0, LastOK: true}, 3, 0.8, HealthDegraded},
		{"down阈值=1 单次即判死", store.ProbeHealth{WindowCount: 1, WindowOK: 0, ConsecutiveFail: 1, LastOK: false}, 1, 0.8, HealthDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.h, c.down, c.rate)
			if got != c.expected {
				t.Fatalf("Classify(%+v, %d, %v) = %q, want %q", c.h, c.down, c.rate, got, c.expected)
			}
		})
	}
}

// TestClassify_HealthyRateBoundary 验证「最近成功」与「成功率」是 AND 关系：
// 最近失败时即使窗口成功率很高，也不能判 healthy（那会掩盖「现在挂了」）。
func TestClassify_HealthyRateBoundary(t *testing.T) {
	// 窗口 10 次全成功，但最近一次失败 → 不能 healthy
	h := store.ProbeHealth{WindowCount: 10, WindowOK: 9, ConsecutiveFail: 1, LastOK: false}
	if got := Classify(h, 3, 0.8); got != HealthDegraded {
		t.Fatalf("最近失败不应判 healthy，got %q", got)
	}
}
