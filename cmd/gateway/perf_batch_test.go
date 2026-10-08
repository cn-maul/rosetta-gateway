package main

// perf_batch_test.go —— 量化「批量落库」的**真实收益上界**。
//
// # 为什么必须先量这个，而不是直接实现批量
//
// perf_split_test.go 已经把成本拆开了（conc=200）：
//
//	INSERT ≈ 244µs/请求   扣费 ≈ 338µs/请求
//
// 看上去「批量 INSERT 能省掉 244µs 里的大部分」。但这个推断有一个前提
// 没有验证：**那 244µs 里到底有多少是「一次 commit 的固定开销」，
// 有多少是「每行 INSERT 的语句开销」**。
//
// 两者决定批量能省多少：
//   - 若是**每行开销**主导 → 批量把 N 行并进 1 次 commit 能省接近 (1-1/N)；
//   - 若是**每次 commit 的固定开销**主导 → 批量同样能省，因为 commit 次数
//     从 N 降到 1；
//   - 但如果两者都很小、瓶颈其实在**写锁串行排队**（而 commit 只是排队
//     的一部分），那批量的收益会远小于预期 —— 因为单写连接下，
//     批量并不减少「必须串行执行的总工作量」。
//
// 也就是说：单看「每事务 244µs」无法推出批量后的每请求成本。
// **必须直接量一次批量事务的真实吞吐**，再和 N 次独立事务对比。
//
// # 量什么
//
//	(a) N 次独立事务（现状）
//	(b) 1 个事务内 N 行 INSERT（候选方案）
//
// 两者的记录内容必须**完全相同**（同样的列、同样的行数），
// 否则比的是「少插了几列」而不是「少提交了几次」。
//
// 另外单独量：
//	(c) 1 个事务内 1 行（= 批大小为 1 的退化情形）—— 用来看固定开销占比
//
//	go test ./cmd/gateway/ -run TestBatchUpside -v -count=1 -timeout 20m

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// batchProbeDB 开一个带价格模型的库，供两种落库路径共用。
func batchProbeDB(t *testing.T) (*store.Store, func(*store.UsageRecord)) {
	t.Helper()
	db, err := store.Open(t.TempDir()+"/batch.db", discardLoggerForSplit())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	if err := db.CreateUser(ctx, &store.User{
		ID: "u1", Username: "u1", Role: store.RoleUser, Status: store.UserStatusActive,
		BalanceCents: 100_000_000,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "p1", Slug: "p1", Name: "p1", Protocol: "openai-chat", Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "m", Enabled: true,
		PriceInput: 30, PriceOutput: 30,
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return db, func(r *store.UsageRecord) {
		r.UserID = "u1"
		r.AccessKeyID = "k1"
		r.PublicModel = "m"
		r.ProviderID = "p1"
		r.UpstreamModel = "m"
		r.IngressProtocol = "openai-chat"
		r.Status = "ok"
		r.InputTokens, r.OutputTokens, r.TotalTokens = 5, 3, 8
	}
}

// TestBatchUpside 直接量「N 次独立事务」vs「1 个事务 N 行」的吞吐差。
func TestBatchUpside(t *testing.T) {
	if testing.Short() {
		t.Skip("批量收益测量是重负载的，-short 下跳过")
	}
	db, decorate := batchProbeDB(t)
	ctx := context.Background()

	// 批大小与总量：批大小覆盖 1/10/50/200，固定总行数以便直接比吞吐。
	const totalRows = 2000
	batchSizes := []int{1, 10, 50, 200}

	// —— (a) 现状：N 次独立事务 ——
	var seq int
	decorateRec := func() *store.UsageRecord {
		seq++
		r := &store.UsageRecord{ID: fmt.Sprintf("a-%d", seq), RequestID: fmt.Sprintf("aq-%d", seq)}
		decorate(r)
		return r
	}
	start := time.Now()
	for i := 0; i < totalRows; i++ {
		if _, err := db.CreateUsageRecordWithCost(ctx, decorateRec()); err != nil {
			t.Fatalf("独立事务插入 %d: %v", i, err)
		}
	}
	elA := time.Since(start)
	perRow := elA / time.Duration(totalRows)
	t.Logf("batch (a) N 次独立事务: %d 行 %v → %.0f 行/s  每行 %v",
		totalRows, elA.Round(time.Millisecond),
		float64(totalRows)/elA.Seconds(), perRow.Round(time.Microsecond))

	// —— (b) 单事务多行 INSERT ——
	//
	// 这里**直接用一条多值 INSERT**（ VALUES (…),(…),… ）而不是循环 Exec：
	// 循环 Exec 在同一事务里仍是 N 次语句执行，量到的是「省了 commit、
	// 没省语句」的收益，正是我们想区分的那两者。
	for _, bs := range batchSizes {
		if bs > totalRows {
			continue
		}
		start := time.Now()
		for done := 0; done < totalRows; done += bs {
			n := bs
			if done+n > totalRows {
				n = totalRows - done
			}
			// 每个批次**现造**记录：复用同一批会导致 usage_records.id 主键
			// 冲突（第一版就踩了这个：UNIQUE constraint failed: usage_records.id）。
			// 记录必须逐批不同，否则量到的是「插入失败」而不是批量吞吐。
			recs := make([]*store.UsageRecord, n)
			for i := range recs {
				recs[i] = decorateRec()
			}
			if err := insertManyInOneTx(ctx, db, recs); err != nil {
				t.Fatalf("批量插入(bs=%d): %v", bs, err)
			}
		}
		el := time.Since(start)
		t.Logf("batch (b) 单事务多值INSERT bs=%-4d: %d 行 %v → %.0f 行/s  每行 %v",
			bs, totalRows, el.Round(time.Millisecond),
			float64(totalRows)/el.Seconds(), (el / time.Duration(totalRows)).Round(time.Microsecond))
	}

	// —— (c) 批大小 1 时「单事务 1 行」—— 与 (a) 的差别**只有 commit 次数**
	//     之外的一切（语句完全相同）。若 (c) 明显快于 (a)，
	//     说明「每次独立事务」的**事务管理开销**本身就有可观成本；
	//     若基本相同，说明成本主要在语句执行本身。
	var seq1 int
	decorate1 := func() *store.UsageRecord {
		seq1++
		r := &store.UsageRecord{ID: fmt.Sprintf("c-%d", seq1), RequestID: fmt.Sprintf("cq-%d", seq1)}
		decorate(r)
		return r
	}
	start = time.Now()
	for i := 0; i < totalRows; i++ {
		if err := insertManyInOneTx(ctx, db, []*store.UsageRecord{decorate1()}); err != nil {
			t.Fatalf("单行事务 %d: %v", i, err)
		}
	}
	elC := time.Since(start)
	t.Logf("batch (c) 每次开事务但只插 1 行: %d 行 %v → %.0f 行/s  每行 %v",
		totalRows, elC.Round(time.Millisecond),
		float64(totalRows)/elC.Seconds(), (elC / totalRows).Round(time.Microsecond))

	// —— 汇总：批量能省多少 ——
	// (a) 与 (c) 都是「一事务一行」，差别只在 Begin/Commit 的封装方式；
	// 真正的收益上界是 (a) vs「最大批大小」。
	t.Logf("")
	t.Logf("=== 批量收益汇总 ===")
	t.Logf("batch 现状(每行一事务)   每行 %v", perRow.Round(time.Microsecond))
	t.Logf("batch 退化为每行一事务   每行 %v  ← 事务封装本身的开销占比见下", (elC / totalRows).Round(time.Microsecond))
}

// insertManyInOneTx 在**一个事务**里用一条多值 INSERT 写入多条记录。
//
// 刻意不调用 store 的公开方法：这里要的是「最理想形态」的吞吐参考，
// 用来给批量方案定收益上界，而不是复现任何具体实现。
func insertManyInOneTx(ctx context.Context, db *store.Store, recs []*store.UsageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`INSERT INTO usage_records
		(id, ts, access_key_id, user_id, public_model, provider_id, upstream_model,
		 ingress_protocol, stream, input_tokens, output_tokens, total_tokens,
		 reasoning_tokens, cached_tokens, usage_state, status, http_status,
		 error_code, latency_ms, ttfb_ms, request_id, cost_total)
		VALUES `)
	for i, r := range recs {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,0,?,?,?,0,0,?,?,200,NULL,0,0,?,0)")
		ts := r.Ts
		if ts == 0 {
			ts = time.Now().UnixMilli()
		}
		args = append(args, r.ID, ts, r.AccessKeyID, r.UserID, r.PublicModel,
			r.ProviderID, r.UpstreamModel, r.IngressProtocol,
			r.InputTokens, r.OutputTokens, r.TotalTokens,
			r.UsageState, r.Status, r.RequestID)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return err
	}
	return tx.Commit()
}
