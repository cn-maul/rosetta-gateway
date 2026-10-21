package main

import (
	"encoding/json"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProtocolMatrix_AnyIngressToAnyUpstream 验证用户问的那件事：
// **任意入站协议 + 任意上游协议**的组合都能跑通，且出站形状由入站协议决定
// —— 上游是 chat 还是 responses 不影响客户端看到的格式。
//
// 这是整个网关的核心价值主张：「/v1/responses 进、openai-chat 上游」这种
// 组合必须可用，否则 Codex CLI 之类只说 responses 的客户端就没法接
// DeepSeek / 大部分国产 chat 上游。三条入站路径共享一个 handleIngress，
// 协议差异收在 ingress.Codec 里，转换本体由 rosetta SDK 承担。
//
// 假上游按**自己的**协议回一个最小成功响应，测试只断言两件事：
//  1. 网关返回 200（转换没炸）；
//  2. 出站形状符合入站协议（chat→choices[0].message.content、
//     responses→output[] 里 output_text、anthropic→content[].text）。
func TestProtocolMatrix_AnyIngressToAnyUpstream(t *testing.T) {
	upstreams := []struct {
		protocol string
		handler  http.HandlerFunc
	}{
		{"openai-chat", fakeUpstreamChat},
		{"openai-responses", fakeUpstreamResponses},
		{"anthropic", fakeUpstreamAnthropic},
	}
	ingresses := []struct {
		name    string
		path    string
		body    string
		codec   ingress.Codec
		extract func(t *testing.T, body []byte) string
	}{
		{
			name:  "chat",
			path:  "/v1/chat/completions",
			body:  `{"model":"flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			codec: ingress.NewOpenAIChat(),
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatalf("chat 响应非法: %v body=%s", err, body)
				}
				if len(v.Choices) == 0 {
					t.Fatalf("chat 响应没有 choices: %s", body)
				}
				return v.Choices[0].Message.Content
			},
		},
		{
			name:  "responses",
			path:  "/v1/responses",
			body:  `{"model":"flash","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			codec: ingress.NewResponses(),
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Output []struct {
						Type    string `json:"type"`
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatalf("responses 响应非法: %v body=%s", err, body)
				}
				for _, o := range v.Output {
					for _, c := range o.Content {
						if c.Type == "output_text" && c.Text != "" {
							return c.Text
						}
					}
				}
				t.Fatalf("responses 响应里没有 output_text: %s", body)
				return ""
			},
		},
		{
			name:  "anthropic",
			path:  "/v1/messages",
			body:  `{"model":"flash","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			codec: ingress.NewAnthropicMessages(),
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatalf("anthropic 响应非法: %v body=%s", err, body)
				}
				for _, c := range v.Content {
					if c.Type == "text" && c.Text != "" {
						return c.Text
					}
				}
				t.Fatalf("anthropic 响应里没有 text block: %s", body)
				return ""
			},
		},
	}

	for _, up := range upstreams {
		srv := httptest.NewServer(up.handler)
		defer srv.Close()
		for _, in := range ingresses {
			t.Run(up.protocol+"<-"+in.name, func(t *testing.T) {
				h := buildHarnessProtocols(t, []harnessChain{
					{slug: "up", url: srv.URL, protocol: up.protocol},
				}, false, in.codec)

				rec := e2ePost(h, in.path, testAccessKey, in.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
				}
				got := in.extract(t, rec.Body.Bytes())
				if !strings.Contains(got, "HELLO") {
					t.Fatalf("正文应包含上游产出，实际 %q，body=%s", got, rec.Body.String())
				}
			})
		}
	}
}

// 三个假上游：各自按自己协议的最小成功形状回包，正文统一带 HELLO 便于断言。

func fakeUpstreamChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"up-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"HELLO from chat"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
}

func fakeUpstreamResponses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"up-model","status":"completed",
		"output":[{"type":"message","id":"msg_up","status":"completed","role":"assistant",
			"content":[{"type":"output_text","text":"HELLO from responses"}]}],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
}

func fakeUpstreamAnthropic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"up-model",
		"content":[{"type":"text","text":"HELLO from anthropic"}],"stop_reason":"end_turn","stop_sequence":null,
		"usage":{"input_tokens":1,"output_tokens":2}}`))
}

// TestProtocolMatrix_Streaming 覆盖**流式**的 3×3 矩阵。
//
// 流式与非流式是两条不同的编码路径（StreamSink vs WriteNonStream），
// 非流式通不代表流式通 —— 而流式恰恰是「用户主动停止」、断流、看门狗
// 这些真实场景的形态，也是 Codex CLI / Claude Code 的默认用法。
// 所以矩阵必须各跑一遍。
//
// 断言只取「最终拿到的正文里有没有 HELLO」，不逐一核对事件名
// （事件名与顺序是 outwire 单测的职责）。
func TestProtocolMatrix_Streaming(t *testing.T) {
	upstreams := []struct {
		protocol string
		handler  http.HandlerFunc
	}{
		{"openai-chat", fakeStreamChat},
		{"openai-responses", fakeStreamResponses},
		{"anthropic", fakeStreamAnthropic},
	}
	ingresses := []struct {
		name  string
		path  string
		body  string
		codec ingress.Codec
	}{
		{"chat", "/v1/chat/completions",
			`{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`,
			ingress.NewOpenAIChat()},
		{"responses", "/v1/responses",
			`{"model":"flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			ingress.NewResponses()},
		{"anthropic", "/v1/messages",
			`{"model":"flash","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			ingress.NewAnthropicMessages()},
	}

	for _, up := range upstreams {
		srv := httptest.NewServer(up.handler)
		defer srv.Close()
		for _, in := range ingresses {
			t.Run(up.protocol+"<-"+in.name, func(t *testing.T) {
				h := buildHarnessProtocols(t, []harnessChain{
					{slug: "up", url: srv.URL, protocol: up.protocol},
				}, false, in.codec)

				rec := e2ePost(h, in.path, testAccessKey, in.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
				}
				body := rec.Body.String()
				if !strings.Contains(body, "HELLO") {
					t.Fatalf("流式响应里没有上游产出，body=%s", body)
				}
			})
		}
	}
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func fakeStreamChat(w http.ResponseWriter, r *http.Request) {
	sseHeaders(w)
	f := w.(http.Flusher)
	chunks := []string{
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","created":1,"model":"up-model","choices":[{"index":0,"delta":{"role":"assistant","content":"HELLO"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","created":1,"model":"up-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","created":1,"model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	for _, c := range chunks {
		_, _ = w.Write([]byte("data: " + c + "\n\n"))
		f.Flush()
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	f.Flush()
}

func fakeStreamResponses(w http.ResponseWriter, r *http.Request) {
	sseHeaders(w)
	f := w.(http.Flusher)
	events := []string{
		`{"type":"response.created","response":{"id":"resp_up","object":"response","status":"in_progress","model":"up-model","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_up","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_up","delta":"HELLO"}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_up","delta":" world"}`,
		`{"type":"response.output_text.done","output_index":0,"item_id":"msg_up","text":"HELLO world"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_up","status":"completed","role":"assistant","content":[{"type":"output_text","text":"HELLO world"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_up","object":"response","status":"completed","model":"up-model","output":[{"type":"message","id":"msg_up","status":"completed","role":"assistant","content":[{"type":"output_text","text":"HELLO world"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	}
	for _, e := range events {
		_, _ = w.Write([]byte("data: " + e + "\n\n"))
		f.Flush()
	}
}

func fakeStreamAnthropic(w http.ResponseWriter, r *http.Request) {
	sseHeaders(w)
	f := w.(http.Flusher)
	write := func(event, data string) {
		_, _ = w.Write([]byte("event: " + event + "\ndata: " + data + "\n\n"))
		f.Flush()
	}
	write("message_start", `{"type":"message_start","message":{"id":"msg_up","type":"message","role":"assistant","model":"up-model","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
	write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"HELLO"}}`)
	write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`)
	write("content_block_stop", `{"type":"content_block_stop","index":0}`)
	write("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
	write("message_stop", `{"type":"message_stop"}`)
}
