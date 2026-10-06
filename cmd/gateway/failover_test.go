package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

const testAccessKey = "sk-gw-test-key"

func fakeBad() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
}

func fakeGood() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","created":0,
			"model":"good-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
}

// fakeNotFound 模拟「这个提供商没有这个模型」—— 上游退役模型、或链上模型名配错
// 时最常见的那一种失败。
func fakeNotFound() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request_error"}}`))
	}))
}

// fakeStreamGood 返回一条正常 SSE 流（role → content → finish_reason → [DONE]）。
func fakeStreamGood() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"good-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// fakeStreamHang 建立 200 + SSE 头后一个事件都不发（模拟「慢到不出首字」的上游）。
// 只在上游请求 ctx 被取消时返回 —— 也就是网关的 TTFT 看门狗掐流时。
func fakeStreamHang() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
}

// buildHarness 组一套「pool + 快照 + 临时库」，链顺序由 providers 入参决定（第一个=链首）。
func buildHarness(t *testing.T, chain []struct {
	slug, url string
	fail      bool
}, failover bool, ttftMs ...int) (http.HandlerFunc, *store.Store) {
	t.Helper()
	h, db, _ := buildHarnessFull(t, chain, failover, openaiChatCodec{}, ttftMs...)
	return h, db
}

// buildHarnessFull 同 buildHarness，但额外把 pool 交出来，供需要断言健康态
// （凭据冷却 / 目标熔断）的用例使用。可选 ttftMs 覆盖 route 的流式首字超时。
func buildHarnessFull(t *testing.T, chain []struct {
	slug, url string
	fail      bool
}, failover bool, codec ingressCodec, ttftMs ...int) (http.HandlerFunc, *store.Store, *upstream.Pool) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Default()
	cfg.Defaults.MaxRetries = 0 // 关掉 SDK 内部重试，把「转移」这件事单独交给网关验证
	bps := make([]config.BootstrapProvider, 0, len(chain))
	for _, c := range chain {
		bps = append(bps, config.BootstrapProvider{
			Slug:        c.slug,
			Name:        c.slug,
			Protocol:    "openai-chat",
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

	// 配额预检读的是库里的 access_keys 行（id 与快照 KeySnapshot.ID 一致 = "k1"）。
	// 默认 quota 0 = 不限，故 failover 用例不受配额影响；配额用例再自行改额度/灌用量。
	// 归属用户必须真实存在：用量查询按 user_id 收窄（外键也要求它先在）。
	// 之前 DB 侧无归属、快照侧却是 testUserID，两边不一致 ——
	// 「org-wide 端点按调用者收窄」后这些用例就查不到数据了。
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
		ri.AddProvider(&routing.ProviderRef{ID: c.slug, Slug: c.slug, Endpoint: c.url, Protocol: "openai-chat", Enabled: true})
		ri.AddUpstreamModel(&routing.UpstreamModel{ID: c.slug + "/m", ProviderID: c.slug, ModelID: c.slug + "-model", Enabled: true})
		targets = append(targets, &routing.Target{
			ID: c.slug + "#t" + string(rune('0'+i)), RouteID: "r1",
			ProviderID: c.slug, UpstreamModelID: c.slug + "/m", Position: i, Enabled: true,
		})
	}
	ttft := 0
	if len(ttftMs) > 0 {
		ttft = ttftMs[0]
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
		// 故障转移策略是全局的（设置页写入），测试里用快照的 Runtime 默认驱动。
		Runtime: snapshot.RuntimeDefaults{
			FailoverMaxTargets:        len(chain),
			StreamFirstTokenTimeoutMs: ttft,
		},
	}
	snapshot.Init(snap)

	rl := ratelimit.New()
	return handleIngress(pool, cfg, newUsageRecorder(db, logger), rl, codec), db, pool
}

func postChat(h http.HandlerFunc, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func postChatStream(h http.HandlerFunc, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 链首 404（该上游没有这个模型）、次目标正常：应当转移。
//
// 2026-09-24 之前 404 不在可转移集合里，这条链会硬失败 —— 而「上游退役了链首
// 用的那个模型」正是链最需要起作用的场景。判定理由见 outwire.FailoverEligible。
func TestFailover_NonStreamSwitchesOnUpstream404(t *testing.T) {
	missing := fakeNotFound()
	good := fakeGood()
	defer missing.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"missing", missing.URL, true},
		{"good", good.URL, false},
	}, true)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after failover on 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("expected served-by-good content, body=%s", rec.Body.String())
	}
}

// 单目标链 + 上游 404：对外应是 404 model_not_found，
// 既不是 502 upstream_error，也不能把上游那句裸 message 原样透给客户端。
func TestFailover_SingleTarget404MapsToModelNotFound(t *testing.T) {
	missing := fakeNotFound()
	defer missing.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"missing", missing.URL, true},
	}, false)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("want code=model_not_found, body=%s", rec.Body.String())
	}
}

// 链首 500、次目标正常：非流式应在写出任何字节前转移到好上游，返回 pong。
func TestFailover_NonStreamSwitchesOnUpstream5xx(t *testing.T) {
	bad := fakeBad()
	good := fakeGood()
	defer bad.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"bad", bad.URL, true},
		{"good", good.URL, false},
	}, true)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after failover, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("expected served-by-good content, body=%s", rec.Body.String())
	}
}

// 关闭故障转移：只打链首，500 原样透出，绝不碰后面的好上游。
func TestFailover_DisabledKeepsSingleTarget(t *testing.T) {
	bad := fakeBad()
	good := fakeGood()
	defer bad.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"bad", bad.URL, true},
		{"good", good.URL, false},
	}, false)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failover disabled: want 502 from bad, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("should NOT have reached good upstream, body=%s", rec.Body.String())
	}
}

// 整条链都坏：耗尽后透出上游错误（502），不 panic、不 200。
func TestFailover_AllTargetsFail(t *testing.T) {
	bad := fakeBad()
	defer bad.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"bad", bad.URL, true},
		{"bad2", bad.URL, true},
	}, true)

	rec := postChat(h, "flash")
	if rec.Code == http.StatusOK {
		t.Fatalf("all-bad chain must not return 200, body=%s", rec.Body.String())
	}
	if rec.Code < 500 {
		t.Fatalf("want 5xx from exhausted chain, got %d", rec.Code)
	}
}

// 客户端在请求进入时已断开：网关不该往链上任何目标打一次（不烧下游配额、
// 也不把一次在途取消误记成目标失败）。见 handleChatCompletions 循环顶部的取消检查。
func TestFailover_ClientGoneAbortsBeforeAnyUpstream(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"bad", srv.URL, true},
		{"bad2", srv.URL, true},
	}, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	h(httptest.NewRecorder(), req)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("client gone: expected 0 upstream calls, got %d", got)
	}
}

// 客户端在「上游调用进行中」断开（首字节前）：属于客户端行为，不是目标故障。
// 网关不该因此把一把健康凭据打进冷却、也不该累计目标熔断 —— 否则单 key provider
// 会被几次用户中断搞成 60s 不可用（凭据仅冷却，冷却期内不再被选中，
// 没有任何请求能成功以触发复苏）。
func TestFailover_ClientDisconnectMidFlightDoesNotPoisonCredential(t *testing.T) {
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		// 挂住直到客户端断开（网关取消上游请求 → 本 handler 的 ctx 被取消）。
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	defer close(release)

	h, _, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{
		{"slow", srv.URL, false},
	}, true, openaiChatCodec{})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAccessKey)

	done := make(chan struct{})
	go func() {
		h(httptest.NewRecorder(), req)
		close(done)
	}()

	// 等上游真的被打了，再模拟客户端断开。
	<-hit
	cancel()
	<-done

	// 断开是客户端行为：该 provider 的凭据必须仍是健康可选的。
	if _, _, err := pool.GetAnyClient("slow"); err != nil {
		t.Fatalf("client disconnect poisoned the credential pool: %v", err)
	}
}

// 流式 + 链首 5xx：SSE 头推迟到拿到首个上游事件之后才写，故此处尚未提交，
// 应在写出任何字节前转移到次目标，客户端最终拿到好上游的完整流。
func TestFailover_StreamSwitchesOnUpstream5xx(t *testing.T) {
	bad := fakeBad()
	good := fakeStreamGood()
	defer bad.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"bad", bad.URL, true},
		{"good", good.URL, false},
	}, true)

	rec := postChatStream(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after stream failover, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("want SSE content-type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("expected served-by-good stream content, body=%s", rec.Body.String())
	}
}

// 流式首字看门狗：链首建立连接后一个事件都不发，TTFT 超时应掐流并转移到次目标。
// 这是「慢上游」这一最需要故障转移的场景，且必须发生在写 SSE 头之前（可回退）。
func TestFailover_StreamSwitchesOnFirstTokenTimeout(t *testing.T) {
	hang := fakeStreamHang()
	good := fakeStreamGood()
	defer hang.Close()
	defer good.Close()

	h, _ := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{
		{"hang", hang.URL, true},
		{"good", good.URL, false},
	}, true, 150) // route 级 TTFT 覆盖 = 150ms

	rec := postChatStream(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after TTFT failover, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("expected served-by-good stream content, body=%s", rec.Body.String())
	}
}
