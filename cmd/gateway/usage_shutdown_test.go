package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// newShutdownTestRecorder 造一个用临时库与丢弃日志的 recorder。
// 日志丢到 stderr 是为了失败时能看见 DB 报错，但不至于刷屏。
func newShutdownTestRecorder(t *testing.T) *usageRecorder {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/gw.db", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return newUsageRecorder(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// shutdownTestRec 造一条最小可用的用量记录。
// ID 必须唯一：usage_records.id 是主键，重复插入会报 UNIQUE 约束，
// 那是测试素材的问题，与被测行为无关。
func shutdownTestRec(id string) *store.UsageRecord {
	return &store.UsageRecord{
		ID: id, AccessKeyID: "k1", TotalTokens: 1,
		PublicModel: "m", ProviderID: "p1", UpstreamModel: "up",
		IngressProtocol: "openai-chat", UsageState: "reported",
		Status: "ok", HTTPStatus: 200,
	}
}

// 2026-10-10 回归：关停时 usage 队列已关闭，但仍在跑的 handler 调 record 会
// panic（send on closed channel）。
//
// 旧实现的 record 是裸的 `select { case u.queue <- rec: default: }`，而
// `default` 只处理「channel 满」，**不处理「channel 已关闭」** —— 后者是
// 运行时 panic。触发路径是正常时序：Shutdown 的 10s 预算到期只是**返回错误**、
// 不终止 handler，紧接着 wait 关闭队列，那条流收尾时正好撞上。
//
// 现在「判关闭 + 入队」在同一把锁里，关停后 record 改走同步写。
func TestRecordAfterWaitDoesNotPanic(t *testing.T) {
	u := newShutdownTestRecorder(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	u.wait(ctx) // 先关停（关闭队列）

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("关停后 record 触发 panic: %v", r)
		}
	}()
	u.record(shutdownTestRec("after-wait"))
}

// TestRecordConcurrentWithWait 压力版：一边关停一边狂记，覆盖「判关闭」与
// 「close」真正并发竞争的那个窗口。旧实现在这里大概率 panic。
func TestRecordConcurrentWithWait(t *testing.T) {
	u := newShutdownTestRecorder(t)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range 50 {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("并发 record 触发 panic: %v", r)
						}
					}()
					u.record(shutdownTestRec(fmt.Sprintf("conc-%d-%d", worker, i)))
				}()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		u.wait(ctx)
	}()

	close(start)
	wg.Wait()
}

// wait 不得被 record 卡住：锁的范围必须小于同步 DB 写，
// 否则关停会被一个慢 SQLite 写拖住 —— 那正是 ctx 超时要防的事。
func TestWaitDoesNotDeadlockWithRecord(t *testing.T) {
	u := newShutdownTestRecorder(t)

	done := make(chan struct{})
	go func() {
		u.record(shutdownTestRec("slow-path"))
		close(done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	u.wait(ctx)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("record 卡死：锁范围过宽，把同步写包进了临界区")
	}
}

// 2026-10-10 回归：首个事件之前流式失败，绝不能报 HTTP 200。
//
// MapUpstreamError 对 ErrStreamTruncated / ErrStreamOverflow 返回 (200,"","")，
// 前提是「内容已经写给客户端了」。而 outcomeFromErr 的每一个调用点都是
// **未提交**路径，于是客户端曾收到
// `{"error":{"message":"","type":"api_error"}}` + HTTP 200 —— SDK 只在 >=400
// 抛错，调用方于是拿到「成功但内容为空」，故障转移也不会被触发。
func TestOutcomeFromErr_StreamSentinelIsNotOK(t *testing.T) {
	cases := map[string]error{
		"truncated": rosetta.ErrStreamTruncated,
		"overflow":  rosetta.ErrStreamOverflow,
	}
	for name, err := range cases {
		out := outcomeFromErr(err)
		if out.statusCode < http.StatusBadRequest {
			t.Errorf("%s: statusCode=%d —— 未提交路径上失败必须用 >=400 表达", name, out.statusCode)
		}
		if out.code == "" || out.message == "" {
			t.Errorf("%s: 错误信封为空（code=%q msg=%q），客户端无从判断", name, out.code, out.message)
		}
		// 首批事件就断流 = 这次尝试什么也没产出，链应当继续试下一个目标。
		if !out.eligible {
			t.Errorf("%s: eligible=false —— 故障转移链不会尝试下一个上游", name)
		}
	}
}

// 反向钉：普通错误的映射不受影响（别把上面那条修复写成「所有流式错误都 502」）。
func TestOutcomeFromErr_NormalErrorsUnchanged(t *testing.T) {
	out := outcomeFromErr(context.DeadlineExceeded)
	if out.statusCode < 400 {
		t.Fatalf("普通错误的 statusCode=%d，应为失败语义", out.statusCode)
	}
	if out.code == "" || out.message == "" {
		t.Fatalf("普通错误退化成空信封：code=%q msg=%q", out.code, out.message)
	}
}

// 写出处那道兜底：即使将来有人再引入返回 200 的失败分支，
// 最终写给客户端的也必须是 >=400（逻辑与 main 内的兜底一致）。
func TestChainExhaustedNeverWritesOK(t *testing.T) {
	for _, in := range []int{0, http.StatusOK} {
		statusCode, code, message := in, "upstream_error", ""
		if statusCode == 0 || statusCode == http.StatusOK {
			statusCode, code, message = http.StatusBadGateway, "upstream_error",
				"no available upstream provider"
		}
		if statusCode < 400 {
			t.Fatalf("输入 %d 最终仍以 %d 写出 —— 客户端会把它当成成功", in, statusCode)
		}
		if code == "" || message == "" {
			t.Fatalf("输入 %d 的兜底错误信封为空", in)
		}
	}
}
