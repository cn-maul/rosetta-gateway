package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// *X0：drain 超时后必须能停下仍在重试的 worker，并把未落库的 request_id
// 留在日志里。
//
// 原实现 wait 超时只是返回，worker 还在 time.Sleep 里无限重试；main 随后
// 返回、db.Close() 与它并发，日志刷满 "database is closed"，而进程一退
// 手上那批记录就连一条可对账的痕迹都没有。
func TestUsageShutdown_AbortStopsRetryLoopAndLogsPending_X0(t *testing.T) {
	db, err := store.Open(t.TempDir()+"/gw.db", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var logs bytes.Buffer
	rec := newUsageRecorder(db, slog.New(slog.NewTextHandler(&logs, nil)))
	// 批量写永久失败；随后把库关掉，让降级逐条也失败 —— 记录只能留在
	// worker 的 stillPending 里，复现 drain 超时的现场。
	rec.batchInsert = func(context.Context, []*store.UsageRecord) ([]float64, error) {
		return nil, errors.New("injected persistent write failure")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	usage := shutdownTestRec(uniqueRecID("abort-x0", 0))
	usage.RequestID = "req-X0-abandoned"
	rec.record(usage)

	// 等 worker 真正进入「失败待重试」状态，否则 wait 的返回值取决于时序。
	deadline := time.Now().Add(5 * time.Second)
	for !rec.HasPendingUsage() {
		if time.Now().After(deadline) {
			t.Fatal("worker 未在 5s 内进入待重试状态，测试前提不成立")
		}
		time.Sleep(10 * time.Millisecond)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if rec.wait(expired) {
		t.Fatal("drain 已超时且记录未落库，wait 不应报告干净")
	}

	rec.abort()

	done := make(chan struct{})
	go func() {
		rec.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abort 后 worker 仍未退出：关停会被无限重试拖住")
	}

	out := logs.String()
	if !strings.Contains(out, "usage recorder aborted with unpersisted records") {
		t.Errorf("缺少放弃 drain 的汇总日志，实际日志：\n%s", out)
	}
	if !strings.Contains(out, usage.RequestID) {
		t.Errorf("汇总日志未包含未落库记录的 request_id %q，实际日志：\n%s", usage.RequestID, out)
	}
}
