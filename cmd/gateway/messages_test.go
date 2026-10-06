package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// postMessages 构造一条带鉴权的 /v1/messages 请求。
func postMessages(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", testAccessKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// 非流式：Anthropic 入口 → OpenAI 上游 → Anthropic 形状响应。
// 这是「Claude Code 指向网关、上游是 OpenAI 系」的最常见链路。
func TestMessages_NonStream(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, anthropicMessagesCodec{})

	rec := postMessages(h, `{"model":"flash","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}

	var out struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Type != "message" || out.Role != "assistant" || out.Model != "flash" {
		t.Fatalf("envelope: %+v", out)
	}
	if out.StopReason != "end_turn" || len(out.Content) != 1 || out.Content[0].Text != "pong" {
		t.Fatalf("content/stop_reason: %+v", out)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", out.Usage)
	}
}

// 流式：上游 OpenAI SSE → 网关转成 Anthropic 事件序列。
func TestMessages_Stream(t *testing.T) {
	up := fakeStreamGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, anthropicMessagesCodec{})

	rec := postMessages(h, `{"model":"flash","max_tokens":1024,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	raw := rec.Body.String()

	names := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v\nraw:\n%s", names, want, raw)
	}
	if !strings.Contains(raw, `"type":"text_delta"`) || !strings.Contains(raw, `"text":"pong"`) {
		t.Fatalf("text delta missing:\n%s", raw)
	}
}

// 鉴权失败按 Anthropic 错误形状返回（401 authentication_error）。
func TestMessages_AuthErrorShape(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, anthropicMessagesCodec{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Type != "error" || out.Error.Type != "authentication_error" {
		t.Fatalf("error shape: %+v", out)
	}
}

// 未知模型 → Anthropic 形状的 404 not_found_error。
func TestMessages_ModelNotFoundShape(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	h, _, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, anthropicMessagesCodec{})

	rec := postMessages(h, `{"model":"nope","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error.Type != "not_found_error" {
		t.Fatalf("error type = %q, want not_found_error", out.Error.Type)
	}
}

// D9：/v1/models 按认证头分流；别名路径强制形状；include=upstream 展开轨道二。
func TestListModels_ShapeSplit(t *testing.T) {
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "prov", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&routing.UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "gpt-x", Enabled: true})
	ri.AddRoute(&routing.Route{ID: "r1", PublicName: "flash", ProviderID: "p1", UpstreamModelID: "m1", Enabled: true})
	snapshot.Init(&snapshot.Snapshot{
		Routes:     ri,
		Providers:  map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: snapshotKeysForTest(),
		UsersByID:  snapshotUsersForTest(),
	})
	defer snapshot.Init(&snapshot.Snapshot{Routes: routing.NewRouteIndex(), Providers: map[string]*snapshot.ProviderSnapshot{}, KeysByHash: map[string]*snapshot.KeySnapshot{}, UsersByID: map[string]*snapshot.UserSnapshot{}})

	h := handleListModels("")

	authed := func(path string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		return req
	}

	// Bearer + /v1/models → OpenAI 形状。
	rec := httptest.NewRecorder()
	h(rec, authed("/v1/models"))
	if !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Fatalf("openai shape expected: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"flash"`) {
		t.Fatalf("virtual name missing: %s", rec.Body.String())
	}

	// x-api-key + /v1/models → Anthropic 形状。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("x-api-key", testAccessKey)
	h(rec2, req2)
	if !strings.Contains(rec2.Body.String(), `"has_more":false`) || strings.Contains(rec2.Body.String(), `"object":"list"`) {
		t.Fatalf("anthropic shape expected: %s", rec2.Body.String())
	}

	// ?include=upstream 展开 slug/model。
	rec3 := httptest.NewRecorder()
	h(rec3, authed("/v1/models?include=upstream"))
	if !strings.Contains(rec3.Body.String(), `"id":"prov/gpt-x"`) {
		t.Fatalf("upstream include missing: %s", rec3.Body.String())
	}

	// 别名路径强制形状：/anthropic/v1/models 即使带 Bearer 也返回 Anthropic 形状。
	anth := handleListModels("anthropic")
	rec4 := httptest.NewRecorder()
	anth(rec4, authed("/anthropic/v1/models"))
	if !strings.Contains(rec4.Body.String(), `"has_more":false`) {
		t.Fatalf("forced anthropic shape expected: %s", rec4.Body.String())
	}

	// 未鉴权 → 401，形状按分流结果。
	rec5 := httptest.NewRecorder()
	anth(rec5, httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil))
	if rec5.Code != http.StatusUnauthorized || !strings.Contains(rec5.Body.String(), "authentication_error") {
		t.Fatalf("unauth shape: code=%d body=%s", rec5.Code, rec5.Body.String())
	}
}

// testUserID 是测试用访问密钥的归属用户。
//
// 为什么必须有它：多用户改造 P0 之后，鉴权要求 key 有**存在且启用**的归属
// 用户 —— 查不到即 ErrKeyUnowned（401）。只往快照里塞 KeysByHash 而不塞
// 对应的 UsersByID，所有数据面测试都会 401，且错误体是
// "authentication failed"（writeAuthError 的 default 分支），
// 看不出真正原因是「缺用户」。
const testUserID = "u-test"

// snapshotUsersForTest 构造与测试 key 配套的用户快照。
//
// AllowedModels 显式写 AllowAll()：ModelAllow 的零值是「拒绝全部」
// （见其类型注释的刻意设计），漏填会让全部模型不可见。测试要表达的是
// 「不受限」这个明确意图，所以不能靠零值。
func snapshotUsersForTest() map[string]*snapshot.UserSnapshot {
	return map[string]*snapshot.UserSnapshot{
		testUserID: {
			ID: testUserID, Name: "tester", Role: "user", Status: "active",
			AuthVersion: 1, AllowedModels: snapshot.AllowAll(),
		},
	}
}

// snapshotKeysForTest 构造一把测试用访问密钥快照（含归属用户与不限模型）。
func snapshotKeysForTest() map[string]*snapshot.KeySnapshot {
	sum := sha256.Sum256([]byte(testAccessKey))
	sumHex := hex.EncodeToString(sum[:])
	// GroupModelAllow 也要显式给 AllowAll()：鉴权读的是 key 上**已折算**的
	// 那一份（P2 的 key 级组覆盖在快照重建时就并入了），不是用户的组。
	// 漏掉它会命中 ModelAllow 刻意的零值 =「拒绝全部」，
	// 症状是全部数据面用例变成 403 model_not_allowed。
	return map[string]*snapshot.KeySnapshot{sumHex: {
		ID: "k1", KeyHash: sumHex, Name: "t", Enabled: true,
		UserID: testUserID, AllowedModels: snapshot.AllowAll(),
		GroupModelAllow: snapshot.AllowAll(),
	}}
}
