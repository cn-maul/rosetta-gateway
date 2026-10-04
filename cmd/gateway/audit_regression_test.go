package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 上游「先返 200 并发出一个事件，然后直接断流」—— 最常见的一类真实故障。
// 网关已经写出字节、无法回退换目标，但它必须被记成失败，
// 否则目标永不熔断、故障永不转移。
func fakeStreamCutAfterFirstEvent() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		// 立刻返回且不写终止序列 → 网关侧表现为断流（truncated）。
	}))
}

func TestFailover_CutStreamCountsAsFailureAndTripsCircuit(t *testing.T) {
	cut := fakeStreamCutAfterFirstEvent()
	defer cut.Close()

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", cut.URL, false}}, false, openaiChatCodec{})

	targetID := "p1#t0"
	for i := 0; i < 3; i++ {
		rec := postChatStream(h, "flash")
		if rec.Code != http.StatusOK {
			t.Fatalf("断流也应已提交 200，实际=%d", rec.Code)
		}
	}

	// 阈值 3：连续 3 次断流后该目标必须被熔断（不再可用）。
	// 修复前 attemptStream 恒返回 committed:true 且外层无条件记成功，
	// 这里会一直是 available。
	if pool.TargetAvailable(targetID) {
		t.Errorf("连续 3 次「先200 再断流」后目标仍可用，熔断从未触发")
	}
}

// 非流式成功绝不能被记成目标失败。
//
// 修复断流熔断时给 attemptOutcome 加了 success 字段，流式路径补了
// `success: status == "ok"`，但非流式路径漏了 —— 而 success 的零值是 false，
// 于是每次非流式成功都走RecordTargetFailure。后果：健康的链首目标在
// threshold 个**成功**请求后被误熔断，RecordCredentialSuccess 永不生效。
// 这类「新增布尔字段漏填零值反义」的 bug 只有断言能挡住。
func TestCircuitBreaker_NonStreamSuccessIsNotCountedAsFailure(t *testing.T) {
	good := fakeGood()
	defer good.Close()

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", good.URL, false}}, false, openaiChatCodec{})

	const targetID = "p1#t0"
	// 阈值默认 3。连续 7 次非流式成功：若成功被误记为失败，第3 次就熔断。
	for i := 0; i < 7; i++ {
		if rec := postChat(h, "flash"); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if !pool.TargetAvailable(targetID) {
		t.Errorf("7 次非流式全成功后目标仍不可用：成功被误记为失败，熔断语义反了")
	}
}

// 非流式失败必须被记为失败（与上一条成对，防止「修成功时把失败也一起放过」）。
func TestCircuitBreaker_NonStreamFailureIsCounted(t *testing.T) {
	bad := fakeBad()
	defer bad.Close()

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", bad.URL, true}}, false, openaiChatCodec{})

	const targetID = "p1#t0"
	for i := 0; i < 3; i++ {
		postChat(h, "flash")
	}
	if pool.TargetAvailable(targetID) {
		t.Errorf("连续 3 次非流式失败后目标仍可用，熔断未触发")
	}
}

// Install 每次重建都把 targets 清空，而任何 admin 写操作都会触发重建
// —— 运维改一个模型名，正在熔断中的坏上游立刻复活被打满。
// 熔断状态必须跨重建保留。
func TestCircuitBreaker_SurvivesPoolReinstall(t *testing.T) {
	bad := fakeBad()
	defer bad.Close()

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", bad.URL, true}}, false, openaiChatCodec{})

	const targetID = "p1#t0"
	for i := 0; i < 3; i++ {
		postChat(h, "flash")
	}
	if pool.TargetAvailable(targetID) {
		t.Fatalf("前置条件不成立：目标未熔断")
	}

	// 模拟一次 admin 写操作后的重建。liveTargetIDs 仍含该 target。
	pool.Install(pool.ProvidersSnapshot(), map[string]bool{targetID: true})

	if pool.TargetAvailable(targetID) {
		t.Errorf("重建后仍在熔断期内的目标被恢复可用：熔断状态没有跨 Install 保留")
	}
}

// 已从配置中删除的 target，其熔断条目必须被丢弃 —— targetID 每次保存链都
// 重新生成，保留会让 targets map 单调堆积。
func TestCircuitBreaker_InstallDropsDeletedTargets(t *testing.T) {
	bad := fakeBad()
	defer bad.Close()

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"p1", bad.URL, true}}, false, openaiChatCodec{})

	for i := 0; i < 3; i++ {
		postChat(h, "flash")
	}

	// 重建且目标已从库中消失。
	pool.Install(pool.ProvidersSnapshot(), map[string]bool{})

	// 目标不在 live 集合里 → 条目被丢弃 → 等价于「全新目标」，默认可用。
	if !pool.TargetAvailable("p1#t0") {
		t.Errorf("已删除的 target仍留有熔断状态")
	}
}

func TestOrgBucket_TimestampsAreUnixSeconds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ts := time.Now().UnixMilli()
	sum := sha256.Sum256([]byte(testAccessKey))
	if err := db.CreateAccessKey(context.Background(), &store.AccessKey{
		ID: "k1", KeyHash: hex.EncodeToString(sum[:]), KeyPrefix: "sk-gw-test",
		Name: "t", Enabled: true, QuotaTokens: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUsageRecord(context.Background(), &store.UsageRecord{
		ID: "u1", Ts: ts, AccessKeyID: "k1", PublicModel: "flash", ProviderID: "p1",
		UpstreamModel: "p1-model", IngressProtocol: "openai-chat",
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes:     routing.NewRouteIndex(),
		Providers:  map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: map[string]*snapshot.KeySnapshot{hex.EncodeToString(sum[:]): {ID: "k1", Enabled: true}},
	})

	for name, h := range map[string]http.HandlerFunc{
		"usage": orgUsageCompletions(db),
		"costs": orgCosts(db),
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/organization/"+name+"?bucket_width=1d", nil)
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var page struct {
			Data []struct {
				StartTime int64 `json:"start_time"`
				EndTime   int64 `json:"end_time"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("%s decode: %v body=%s", name, err, rec.Body.String())
		}
		if len(page.Data) == 0 {
			t.Fatalf("%s 返回空页：%s", name, rec.Body.String())
		}
		b := page.Data[0]
		// 桶起点落在当前这24h 内（毫秒值会是 1000 倍）。
		if b.StartTime > 100_000_000_000 {
			t.Errorf("%s start_time=%d 是毫秒，OpenAI 契约要求 Unix 秒", name, b.StartTime)
		}
		if d := b.EndTime - b.StartTime; d != 86400 {
			t.Errorf("%s 桶宽=%d秒，期望 86400（桶起点与 end_time 单位不一致）", name, d)
		}
	}
}
