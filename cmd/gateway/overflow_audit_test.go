package main

import (
	"testing"
	"time"
)

// TestSafeMillis_ClampsBeforeOverflow 坐实一个能把全站转发打挂的缺陷。
//
// time.Duration 是 int64 **纳秒**（上限 ≈ 9.223e18）。任何
// `time.Duration(ms) * time.Millisecond` 在 ms > 9.223e12 时整数回绕成负数：
//
//	9223372036855 ms（≈292 年）→ -9223372036854551616 ns
//
// 负 Duration 的后果不是「超时太长」，而是：
//   - context.WithTimeout(ctx, 负值) → deadline 立即过期 → 全部非流式请求秒挂；
//   - time.AfterFunc(负值, ...)     → 立即开火 → 流式响应刚发出头就被自己关掉。
//
// 而 UI 上显示的是一个「巨大但合法」的超时值，没有任何异常信号。
//
// 校验层（config.validate / settings_handler）只能挡住**新写入**的脏值，
// 挡不住数据库里已有的、或经其它部署路径写进去的。转换点钳制是最后一道。
func TestSafeMillis_ClampsBeforeOverflow(t *testing.T) {
	cases := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"0 保持 0（调用方另行回落）", 0, 0},
		{"负数保持 0", -1, 0},
		{"常规值原样通过", 30_000, 30 * time.Second},
		{"恰好在 24h 上界", 86_400_000, 24 * time.Hour},
		// 关键用例：这些值在旧实现下全部回绕成负数。
		{"9.223e12 毫秒（溢出临界）", 9_223_372_036_855, 24 * time.Hour},
		{"292 年", 9_223_372_036_854_775, 24 * time.Hour},
		{"math.MaxInt64", 1<<63 - 1, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeMillis(tc.ms)
			if got != tc.want {
				t.Fatalf("safeMillis(%d) = %v，期望 %v", tc.ms, got, tc.want)
			}
			// 无论输入多大，输出必须始终为正 —— 这是不变式。
			if tc.ms > 0 && got <= 0 {
				t.Fatalf("safeMillis(%d) 返回了非正 Duration %v，会让超时立即触发", tc.ms, got)
			}
		})
	}
}

// TestSafeMillis_NeverExceedsMaxInt64 直接对不变式做暴力验证：
// 在整个 int32 正数区间内扫描，输出不得为负、不得溢出。
func TestSafeMillis_NeverExceedsInt32Range(t *testing.T) {
	for ms := 1; ms <= 1<<31-1; ms += 4093 { // 质数步长，覆盖整个区间
		got := safeMillis(ms)
		if got < 0 {
			t.Fatalf("safeMillis(%d) = %v：负 Duration 会让 context deadline 立即过期", ms, got)
		}
		if got > 24*time.Hour {
			t.Fatalf("safeMillis(%d) = %v：未被钳制到 24h 上界", ms, got)
		}
	}
}
