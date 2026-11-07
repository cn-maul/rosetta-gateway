package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta"
)

// TestBuildClient_RejectsReservedExtraKeys 锁住「网关不装 Extra 覆盖开关」这条
// 约定：任何保留键出现在 Extra 里都必须被 SDK 当场拒成 ErrInvalidRequest。
//
// 网关自 2026-11-07 起不往 Extra 写任何东西 —— 曾经唯一需要覆盖的 `messages`
// （Responses→chat 的 turn 合并）已经改在入站把一轮合成一条 Message。
//
// 这条用例的价值全在**反向**：如果哪天有人图省事加了
// `WithExtraOverrides(true)` 或 `WithExtraOverrideKeys(...)` 去绕某个报错，
// 越界写入就会变成静默覆盖 SDK 编码结果（编译期与常规测试都抓不住），
// 这里会当场红。
func TestBuildClient_RejectsReservedExtraKeys(t *testing.T) {
	var payloads []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		payloads = append(payloads, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,
			"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	prov := &ProviderEntry{Slug: "pv", Endpoint: srv.URL, Protocol: "chat", Enabled: true}
	client, err := buildClient(context.Background(), prov, "k", nil)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}

	for _, key := range []string{"messages", "stream", "tool_choice", "temperature"} {
		t.Run(key, func(t *testing.T) {
			req := &rosetta.ChatRequest{
				Model:    "m",
				Messages: []rosetta.Message{rosetta.User("hi")},
				Extra:    map[string]any{key: true},
			}
			before := len(payloads)
			_, err := client.Chat(context.Background(), req)
			if !errors.Is(err, rosetta.ErrInvalidRequest) {
				t.Fatalf("Extra[%q] 应被拒成 ErrInvalidRequest（网关不该开 Extra 覆盖），实际 %v", key, err)
			}
			if len(payloads) != before {
				t.Fatalf("被拒的请求不该到达上游")
			}
		})
	}
}
