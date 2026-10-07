package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// harnessChain 与 failover_test.go 里 buildHarness 的匿名链结构对应，
// 但每个 provider 自带 protocol —— 硬约束过滤的用例必须让链上混有
// openai-chat 与 anthropic 两种协议，统一 openai-chat 的旧 harness 不够用。
type harnessChain struct {
	slug     string
	url      string
	protocol string
}

// buildHarnessProtocols 组一套「pool + 快照 + 临时库」，链顺序由 providers
// 入参决定（第一个=链首），protocol 逐 provider 生效。结构复制自
// buildHarnessFull（它的匿名入参结构被十几个既有用例内联声明，不便改签名）。
func buildHarnessProtocols(t *testing.T, chain []harnessChain, failover bool, codec ingressCodec) http.HandlerFunc {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Default()
	cfg.Defaults.MaxRetries = 0 // 关掉 SDK 内部重试，故障转移行为归网关自己
	bps := make([]config.BootstrapProvider, 0, len(chain))
	for _, c := range chain {
		bps = append(bps, config.BootstrapProvider{
			Slug:        c.slug,
			Name:        c.slug,
			Protocol:    c.protocol,
			Endpoint:    c.url,
			Credentials: []config.BootstrapCredential{{Label: "k", APIKey: "sk-" + c.slug}},
		})
	}
	cfg.Bootstrap.Providers = bps

	pool := upstream.NewPool(logger)
	if err := pool.BuildFromConfig(cfg); err != nil {
		t.Fatalf("build pool: %v", err)
	}

	db, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.CreateUser(context.Background(), &store.User{
		ID: testUserID, Username: "tester", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	sum0 := sha256.Sum256([]byte(testAccessKey))
	if err := db.CreateAccessKey(context.Background(), &store.AccessKey{
		ID: "k1", KeyHash: hex.EncodeToString(sum0[:]), KeyPrefix: "sk-gw-test",
		Name: "t", Enabled: true, QuotaTokens: 0, UserID: testUserID,
	}); err != nil {
		t.Fatalf("seed access key: %v", err)
	}

	ri := routing.NewRouteIndex()
	var targets []*routing.Target
	for i, c := range chain {
		ri.AddProvider(&routing.ProviderRef{ID: c.slug, Slug: c.slug, Endpoint: c.url, Protocol: c.protocol, Enabled: true})
		ri.AddUpstreamModel(&routing.UpstreamModel{ID: c.slug + "/m", ProviderID: c.slug, ModelID: c.slug + "-model", Enabled: true})
		targets = append(targets, &routing.Target{
			ID: c.slug + "#t" + string(rune('0'+i)), RouteID: "r1",
			ProviderID: c.slug, UpstreamModelID: c.slug + "/m", Position: i, Enabled: true,
		})
	}
	ri.AddRoute(&routing.Route{
		ID: "r1", PublicName: "flash",
		ProviderID: chain[0].slug, UpstreamModelID: chain[0].slug + "/m",
		Enabled: true, FailoverEnabled: failover,
	})
	for _, tg := range targets {
		ri.AddRouteTarget(tg)
	}

	sum := sha256.Sum256([]byte(testAccessKey))
	sumHex := hex.EncodeToString(sum[:])
	snap := &snapshot.Snapshot{
		Routes:     ri,
		Providers:  map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: map[string]*snapshot.KeySnapshot{sumHex: {ID: "k1", KeyHash: sumHex, Name: "t", Enabled: true, UserID: testUserID, AllowedModels: snapshot.AllowAll(), GroupModelAllow: snapshot.AllowAll()}},
		UsersByID:  snapshotUsersForTest(),
		Runtime: snapshot.RuntimeDefaults{
			FailoverMaxTargets: len(chain),
		},
	}
	snapshot.Init(snap)

	rl := ratelimit.New()
	return handleIngress(pool, cfg, newUsageRecorder(db, logger), rl, codec)
}

// fakeAnthropic 起一个 Anthropic Messages 形状的假上游（SDK 会 POST
// <endpoint>/v1/messages），命中次数记入计数器供断言「未被命中」。
func fakeAnthropic(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude",
			"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
}

// (a) 链上只有 anthropic 上游 + 带 json_schema 的 Chat 请求 → 400，
// 且文案点明结构化输出 —— 而不是 200 但约束静默丢失（P1-8）。
func TestStructuredOutput_AnthropicOnlyChainRejected(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, openaiChatCodec{})

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{"type":"object"}}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "structured output") {
		t.Fatalf("错误文案必须点明结构化输出：%s", rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
}

// (b) 链 [openai-chat(可转移失败), anthropic] + json_schema → 最终错误，
// 而不是「转移到 anthropic 后 200 但约束丢失」。anthropic 假上游必须零命中。
func TestStructuredOutput_FailoverSkipsAnthropicCandidate(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()
	bad := fakeBad()
	defer bad.Close()

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "openai", url: bad.URL, protocol: "openai-chat"},
		{slug: "claude", url: anth.URL, protocol: "anthropic"},
	}, true, openaiChatCodec{})

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"}}}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("结构化输出请求不得在 anthropic 上游拿到 200（约束会静默丢失）：%s", rec.Body.String())
	}
	if rec.Code < 400 {
		t.Fatalf("want 4xx/5xx, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
	// openai-chat 目标确实被打过（失败是真实上游错误，不是过滤造成的空转）。
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("应透出上游错误而非其它：%s", rec.Body.String())
	}
}

// 正向对照：openai 系候选不受过滤影响，json_schema 照常转发（约束仍生效）。
// 防止过滤器把「能服务的请求」也拦掉。
func TestStructuredOutput_OpenAIChatTargetStillServes(t *testing.T) {
	ch := make(chan map[string]any, 4)
	up := recordingUpstream(t, ch, false)
	defer up.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "openai", url: up.URL, protocol: "openai-chat"}}, false, openaiChatCodec{})

	rec := postRaw(h, `{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"}}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	m := <-ch
	rf, ok := m["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format 未透传给 openai-chat 上游：%v", m["response_format"])
	}
}

// Responses 入口同样受硬约束约束：text.format 为 json_schema 时，
// anthropic-only 链 → 400。
func TestStructuredOutput_ResponsesIngressAnthropicChainRejected(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, openaiResponsesCodec{})

	body := `{"model":"flash","input":"hi","text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "structured output") {
		t.Fatalf("错误文案必须点明结构化输出：%s", rec.Body.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("anthropic 上游不应被命中，实际 %d 次", got)
	}
}

// Anthropic 入站协议没有结构化输出概念，永不触发过滤：
// anthropic 入口 → anthropic 上游照常 200。
func TestStructuredOutput_AnthropicIngressNotFiltered(t *testing.T) {
	var hits atomic.Int64
	anth := fakeAnthropic(t, &hits)
	defer anth.Close()

	h := buildHarnessProtocols(t, []harnessChain{{slug: "claude", url: anth.URL, protocol: "anthropic"}}, false, anthropicMessagesCodec{})

	rec := postMessages(h, `{"model":"flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("anthropic 入口不受结构化输出过滤影响，want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("anthropic 上游应被命中 1 次，实际 %d", got)
	}
	var out struct {
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Usage.Input != 5 || out.Usage.Output != 3 {
		t.Fatalf("usage = %+v, want 5/3", out.Usage)
	}
}
