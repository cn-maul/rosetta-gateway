package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/admin"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func authedGet(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	return req
}

// /v1/models 带容量元数据；/v1/models/{model} 单模型详情。
func TestModels_Metadata(t *testing.T) {
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "prov", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&routing.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "gpt-x", Enabled: true,
		ContextWindow: 128000, MaxOutputTokens: 16384,
	})
	ri.AddRoute(&routing.Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	snapshot.Init(&snapshot.Snapshot{
		Routes:     ri,
		Providers:  map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: snapshotKeysForTest(),
		Runtime:    snapshot.RuntimeDefaults{DefaultContextWindow: 8192, DefaultMaxOutputTokens: 4096},
	})
	defer snapshot.Init(&snapshot.Snapshot{Routes: routing.NewRouteIndex(), Providers: map[string]*snapshot.ProviderSnapshot{}, KeysByHash: map[string]*snapshot.KeySnapshot{}})

	h := handleListModels("")
	rec := httptest.NewRecorder()
	h(rec, authedGet("/v1/models"))
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			MaxOutputToks int64  `json:"max_output_tokens"`
			MaxInputToks  int64  `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v body=%s", err, rec.Body.String())
	}
	if len(list.Data) != 1 || list.Data[0].ContextLength != 128000 ||
		list.Data[0].MaxOutputToks != 16384 || list.Data[0].MaxInputToks != 128000 {
		t.Fatalf("list metadata: %+v", list.Data)
	}

	// PathValue 依赖 ServeMux 的路由上下文，必须经 mux 转发。
	detailMux := http.NewServeMux()
	detailMux.HandleFunc("GET /v1/models/{model}", handleGetModel(""))
	rec2 := httptest.NewRecorder()
	detailMux.ServeHTTP(rec2, authedGet("/v1/models/flash"))
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"context_length":128000`) {
		t.Fatalf("detail: code=%d body=%s", rec2.Code, rec2.Body.String())
	}

	// 无容量且无默认 → 不编造字段。
	ri2 := routing.NewRouteIndex()
	ri2.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "prov", Protocol: "openai-chat", Enabled: true})
	ri2.AddUpstreamModel(&routing.UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "gpt-x", Enabled: true})
	ri2.AddRoute(&routing.Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	snapshot.Init(&snapshot.Snapshot{
		Routes: ri2, Providers: map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: snapshotKeysForTest(),
	})
	rec3 := httptest.NewRecorder()
	detailMux.ServeHTTP(rec3, authedGet("/v1/models/flash"))
	if strings.Contains(rec3.Body.String(), "context_length") {
		t.Fatalf("must not fabricate capacity: %s", rec3.Body.String())
	}

	// 未知模型 404。
	rec4 := httptest.NewRecorder()
	detailMux.ServeHTTP(rec4, authedGet("/v1/models/nope"))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("unknown model: code=%d", rec4.Code)
	}
}

// 额度查询三件套：subscription（PTM 等价映射）、usage（美分）、organization/costs（cny）。
func TestBillingEndpoints(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})
	ctx := context.Background()

	// key 配额 200 万 token，已用 50 万（插一条 usage 记录驱动触发器）。
	k, _ := db.GetAccessKey(ctx, "k1")
	k.QuotaTokens = 2_000_000
	if err := db.UpdateAccessKey(ctx, "k1", k); err != nil {
		t.Fatalf("update key: %v", err)
	}
	if err := db.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "u1", AccessKeyID: "k1", PublicModel: "flash", ProviderID: "good",
		UpstreamModel: "good-model", IngressProtocol: "openai-chat",
		InputTokens: 500_000, OutputTokens: 0, TotalTokens: 500_000,
		UsageState: "reported", Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	rec := httptest.NewRecorder()
	billingSubscription(db)(rec, authedGet("/dashboard/billing/subscription"))
	var sub struct {
		Object       string  `json:"object"`
		HardLimitUSD float64 `json:"hard_limit_usd"`
		TotalUsed    float64 `json:"total_used"`
		TotalAvail   float64 `json:"total_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatalf("subscription: %v body=%s", err, rec.Body.String())
	}
	if sub.Object != "billing_subscription" || sub.HardLimitUSD != 2.0 || sub.TotalUsed != 0.5 || sub.TotalAvail != 1.5 {
		t.Fatalf("subscription mapping: %+v", sub)
	}

	rec2 := httptest.NewRecorder()
	billingUsage(db)(rec2, authedGet("/v1/dashboard/billing/usage"))
	var usage struct {
		Object     string  `json:"object"`
		TotalUsage float64 `json:"total_usage"`
		TotalToks  int64   `json:"total_tokens"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Object != "list" || usage.TotalUsage != 50 || usage.TotalToks != 500_000 {
		t.Fatalf("usage mapping: %+v", usage)
	}

	// organization/costs：给模型配 2 元/百万输出价，100 万输出 = 2 元。
	// harness 的 provider 只存在于快照里，DB 需要实体行满足外键。
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "good", Slug: "good", Name: "good", Protocol: "openai-chat",
		Endpoint: up.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.UpsertUpstreamModel(ctx, &store.UpstreamModel{
		ID: "pm1", ProviderID: "good", ModelID: "good-model", Enabled: true, PriceOutput: 2,
	}); err != nil {
		t.Fatalf("seed price: %v", err)
	}
	if err := db.CreateUsageRecord(ctx, &store.UsageRecord{
		ID: "u2", AccessKeyID: "k1", PublicModel: "flash", ProviderID: "good",
		UpstreamModel: "good-model", IngressProtocol: "openai-chat",
		OutputTokens: 1_000_000, TotalTokens: 1_000_000,
		UsageState: "reported", Status: "ok", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("seed usage 2: %v", err)
	}
	rec3 := httptest.NewRecorder()
	orgCosts(db)(rec3, authedGet("/v1/organization/costs?bucket_width=1d"))
	var costs struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string `json:"object"`
			StartTime int64  `json:"start_time"`
			Results   []struct {
				Object   string `json:"object"`
				LineItem string `json:"line_item"`
				Amount   struct {
					Value    float64 `json:"value"`
					Currency string  `json:"currency"`
				} `json:"amount"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &costs); err != nil {
		t.Fatalf("costs: %v body=%s", err, rec3.Body.String())
	}
	if costs.Object != "page" || len(costs.Data) == 0 || costs.Data[0].Object != "bucket" {
		t.Fatalf("costs envelope: %+v", costs)
	}
	r0 := costs.Data[0].Results[0]
	if r0.Object != "organization.costs.result" || r0.LineItem != "model:good-model" ||
		r0.Amount.Currency != "cny" || r0.Amount.Value != 2 {
		t.Fatalf("cost result: %+v", r0)
	}
}

// History 过滤（status）与 CSV 导出共用同一套过滤。
func TestUsageHistoryFilterAndCSV(t *testing.T) {
	db, err := store.Open(t.TempDir()+"/h.db", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for i, st := range []string{"ok", "error", "ok"} {
		if err := db.CreateUsageRecord(context.Background(), &store.UsageRecord{
			ID: "r-" + strconv.Itoa(i), AccessKeyID: "k1",
			PublicModel: "flash", UpstreamModel: "m", IngressProtocol: "openai-chat",
			TotalTokens: 10, UsageState: "reported", Status: st, HTTPStatus: 200,
			Ts: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	h := admin.NewUsageHandler(db).History
	rec := httptest.NewRecorder()
	h(rec, authedGet("/admin/api/usage/history?status=error"))
	var page struct {
		Records []struct {
			Status string `json:"status"`
		} `json:"records"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("history: %v body=%s", err, rec.Body.String())
	}
	if page.Total != 1 || len(page.Records) != 1 || page.Records[0].Status != "error" {
		t.Fatalf("status filter: %+v", page)
	}

	csvH := admin.NewUsageHandler(db).ExportCSV
	rec2 := httptest.NewRecorder()
	csvH(rec2, authedGet("/admin/api/usage/history.csv"))
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("csv content type: %s", ct)
	}
	lines := strings.Split(strings.TrimSpace(rec2.Body.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ts,public_model") {
		t.Fatalf("csv rows: %v", lines)
	}
}

// /v1/responses 冒烟：OpenAI 上游 → Responses 形状非流式响应。
func TestResponsesIngress_NonStream(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiResponsesCodec{})

	body := `{"model":"flash","input":"hi","max_output_tokens":256}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Object != "response" || out.Status != "completed" ||
		len(out.Output) != 1 || out.Output[0].Type != "message" || out.Output[0].Content[0].Text != "pong" ||
		out.Usage.TotalTokens != 8 {
		t.Fatalf("response: %+v", out)
	}
}

// /v1/responses 流式冒烟：事件序列符合 Responses 规范骨架。
func TestResponsesIngress_Stream(t *testing.T) {
	up := fakeStreamGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiResponsesCodec{})

	body := `{"model":"flash","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	var names []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v\nraw:\n%s", names, rec.Body.String())
	}
}
