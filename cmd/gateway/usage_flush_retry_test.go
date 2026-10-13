package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 批量落库失败必须**不丢记录**（2026-10-12 修 P1-1）。
//
// 原实现的 flush 遇错只打一条 ERROR 就 return —— 记录已从队列出队，
// 不重试、不落补偿队列，整批最多 50 条用量与扣费静默丢失。后果：
// 这些请求既不入报表、不计配额、也不扣余额，**用户实际消耗了上游 token
// 却完全免单**，且配额被永久放宽。触发只需一次写池故障（SQLITE_BUSY /
// 磁盘抖动），**不需要攻击者**。
//
// 本文件用 batchInsert 注入点构造这个场景：真实 SQLite 无法稳定制造
// SQLITE_BUSY（依赖真实锁竞争时序），所以那条分支在没注入点时永远测不到 ——
// 而它恰恰是整个修复的核心。

// flakyInsert 让前 failTimes 次批量写入失败，之后走真实落库。
//
// 「之后走真实」很重要：flush 的成功路径必须**真的写进库**，否则
// 「重试救回来了」只是把计数骗过去，库里其实什么都没有。
// 注意它不能在失败时也调真实写入 —— 那会让「批量失败」的场景里
// 数据已经落库，降级/回灌的行为完全观察不到。
func flakyInsert(db *store.Store, failTimes int32, calls *atomic.Int32) func(context.Context, []*store.UsageRecord) ([]float64, error) {
	return func(ctx context.Context, recs []*store.UsageRecord) ([]float64, error) {
		n := calls.Add(1)
		if n <= failTimes {
			return nil, errors.New("injected SQLITE_BUSY")
		}
		return db.CreateUsageRecordsBatched(ctx, recs)
	}
}

// countUsageRecords 读库里 usage_records 的行数。
//
// 借 GetUsageStats 的 TotalRequests —— 它走 UsageSource（明细 + 归档），
// 但本测试只往明细里写、不跑剪枝，所以两者等价。
func countUsageRecords(t *testing.T, db *store.Store) int {
	t.Helper()
	stats, err := db.GetUsageStats(context.Background(), 0, 0, "")
	if err != nil {
		t.Fatalf("count usage: %v", err)
	}
	return int(stats.TotalRequests)
}

func newFlakyRecorder(t *testing.T, failTimes int32) (*usageRecorder, *store.Store, *atomic.Int32) {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/gw.db", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// 素材要先落进去一部分，才能验「至少写了 N 条」；这里反过来：
	// 我们要验的是「批量失败那批最终仍落库」，所以库里初始应为空。
	var calls atomic.Int32
	rec := newUsageRecorder(db, discardLogger())
	rec.batchInsert = flakyInsert(db, failTimes, &calls)
	return rec, db, &calls
}

// 瞬时故障必须被重试救回来 —— 记录一条都不能少。
func TestUsageFlush_瞬时失败重试后不丢记录(t *testing.T) {
	// 前 1 次批量写失败（消耗 1 次重试），第 2 次成功。
	rec, db, calls := newFlakyRecorder(t, 1)

	const n = 5
	for i := 0; i < n; i++ {
		rec.record(shutdownTestRec(uniqueRecID("retry", i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !rec.wait(ctx) {
		t.Error("wait 报未排空，但故障是瞬时的，应被重试救回")
	}

	if got := countUsageRecords(t, db); got != n {
		t.Errorf("落库 %d 条，期望 %d —— 瞬时失败不应丢记录", got, n)
	}
	if c := calls.Load(); c < 2 {
		t.Errorf("批量写入只被调用 %d 次：重试没生效", c)
	}
}

// 降级逐条：整批一直失败时，**每条独立事务**仍应各自落库。
//
// 这是第二级保险 —— 把爆炸半径从「整批 50 条」缩到「单条」。
// 失败次数给足（超过 usageFlushMaxAttempts），确保走完重试才降级。
func TestUsageFlush_整批持久失败降级为逐条(t *testing.T) {
	// 批量插入**永远**失败（远超重试上限），单条路径仍正常。
	rec, db, calls := newFlakyRecorder(t, 1<<30)

	const n = 4
	for i := 0; i < n; i++ {
		rec.record(shutdownTestRec(uniqueRecID("degrade", i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 降级成功 → 全部落库 → wait 应报干净。
	rec.wait(ctx)

	if got := countUsageRecords(t, db); got != n {
		t.Errorf("降级逐条后落库 %d 条，期望 %d —— 整批失败不该连坐", got, n)
	}
	_ = calls
}

// 关停时仍有记录没落干净，wait 必须**报出来**，而不是静默成功。
//
// 这是本次修复的最后一环：原来 wait 永远返回「排空了」，
// 日志上一片祥和、实际最多 50 条计费记录凭空消失。
func TestUsageFlush_关停未落干净必须报告(t *testing.T) {
	// 批量永久失败，且让**单条也失败** —— 构造真正无法落库的场景。
	// 做法：关掉 store，让所有写入都报错。
	rec, _, _ := newFlakyRecorder(t, 1<<30)
	dbClosed := rec.db
	if err := dbClosed.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	for i := 0; i < 3; i++ {
		rec.record(shutdownTestRec(uniqueRecID("lost", i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	clean := rec.wait(ctx)
	if clean {
		t.Error("★ wait 报「已排空」，但记录一条都没落库 —— 这正是原来的静默丢失\n" +
			"  关停流程据此会以为一切正常，日志上不会有任何痕迹")
	}
	if !rec.HasPendingUsage() {
		t.Error("HasPendingUsage 应为 true：还有未落库的记录")
	}
}

// 连续多批失败时，失败批不能被后续新记录挤掉 ——
// worker 必须优先重试仍Pending 的那批。
//
// 这条守的是「不丢」的最后一环：若实现改成失败就丢弃、或让新记录插到
// 前面，长期故障下失败批会被逐步挤出直到消失。
func TestUsageFlush_连续失败不丢记录(t *testing.T) {
	// 批量永久失败，单条正常 → 每批都会走降级逐条，记录仍在库里。
	rec, db, _ := newFlakyRecorder(t, 1<<30)

	const batches = 3
	const perBatch = 3
	for b := 0; b < batches; b++ {
		for i := 0; i < perBatch; i++ {
			rec.record(shutdownTestRec(uniqueRecID("multi", b*perBatch+i)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec.wait(ctx)

	want := batches * perBatch
	if got := countUsageRecords(t, db); got != want {
		t.Errorf("多批连续失败后落库 %d 条，期望 %d", got, want)
	}
}

// uniqueRecID 生成全局唯一的记录 ID（usage_records.id 是主键）。
func uniqueRecID(prefix string, i int) string {
	return prefix + "-" + time.Now().Format("150405.000000000") + "-" + itoaTest(i)
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
