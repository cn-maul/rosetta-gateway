package dataplane

import (
	"context"
	"testing"
	"time"
)

// newAdmissionHandler 造一个只用于测并发名额的 handler。
//
// 不打上游：名额在**解码之后、触碰上游之前**就被申请，而准入本身
// 是个独立方法，不必经过整条数据面链路。
func newAdmissionHandler(t *testing.T, limit int, timeout time.Duration) *Handler {
	t.Helper()
	h := &Handler{
		streams:          make(chan struct{}, limit),
		admissionTimeout: timeout,
	}
	return h
}

// TestAcquireStreamSlot_TimesOutWhenFull 钉住「过载时快速失败」的
// 背压性质（2026-10-22 修 §7.1-3）。
//
// 旧实现是**纯阻塞**地等名额：过载时请求堆在网关里，每个都占着
// goroutine、连接与一份已预占的配额，而客户端只看到「越来越慢」，
// 没有任何可重试的信号。
func TestAcquireStreamSlot_TimesOutWhenFull(t *testing.T) {
	h := newAdmissionHandler(t, 1, 50*time.Millisecond)

	// 占满唯一的名额。
	h.streams <- struct{}{}
	defer func() { <-h.streams }()

	start := time.Now()
	ok := h.acquireStreamSlot(context.Background())
	elapsed := time.Since(start)

	if ok {
		t.Fatal("名额已满时不该拿到名额")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("等待超过 2 秒才返回（实际 %v）—— 背压必须是有界的", elapsed)
	}
}

// TestAcquireStreamSlot_SucceedsWhenSlotAvailable 确认有空闲名额时正常放行 ——
// 上一条不能靠「一律失败」来通过。
func TestAcquireStreamSlot_SucceedsWhenSlotAvailable(t *testing.T) {
	h := newAdmissionHandler(t, 1, time.Second)

	if !h.acquireStreamSlot(context.Background()) {
		t.Fatal("有空闲名额时不该失败")
	}
	// 名额被占用后立刻释放。
	<-h.streams
}

// TestAcquireStreamSlot_ReturnsOnContextCancel 确认客户端断开时立刻放行，
// 不占用过载等待的额度。
func TestAcquireStreamSlot_ReturnsOnContextCancel(t *testing.T) {
	h := newAdmissionHandler(t, 1, 10*time.Second)
	h.streams <- struct{}{}
	defer func() { <-h.streams }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if h.acquireStreamSlot(ctx) {
		t.Fatal("ctx 已取消时不该拿到名额")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ctx 取消后仍等了 %v —— 必须立刻返回", elapsed)
	}
}

// TestAcquireStreamSlot_ConcurrentSlotsAreExclusive 钉住名额的互斥性：
// N 个名额最多放行 N 个并发流，第 N+1 个必须被拒。
func TestAcquireStreamSlot_ConcurrentSlotsAreExclusive(t *testing.T) {
	const limit = 3
	h := newAdmissionHandler(t, limit, 50*time.Millisecond)

	for i := 0; i < limit; i++ {
		if !h.acquireStreamSlot(context.Background()) {
			t.Fatalf("第 %d 个名额不该失败", i+1)
		}
	}
	if h.acquireStreamSlot(context.Background()) {
		t.Fatal("名额用尽后不该再拿到 —— 并发上限失效")
	}
}
