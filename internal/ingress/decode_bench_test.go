package ingress

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// chatBody 是一份接近真实 agent 请求的 body：系统提示 + 多轮 + 工具定义，
// 避免用几十字节的玩具请求测出一个看不出差别的数字。
const chatBody = `{
  "model": "%s",
  "messages": [
    {"role":"system","content":"You are a coding agent. Be precise. Use tools when needed."},
    {"role":"user","content":"Refactor the request decoder so vendored reasoning fields survive, and keep the tests green."},
    {"role":"assistant","content":"I will inspect the decoder first."},
    {"role":"user","content":"Also check whether the streaming path writes a keep-alive every 15 seconds."}
  ],
  "tools":[{"type":"function","function":{"name":"read_file","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
  "temperature":0.2,
  "stream":false,
  "reasoning_effort":"high"
}`

func benchRequest(model string) *http.Request {
	body := strings.Replace(chatBody, "%s", model, 1)
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	return req
}

func BenchmarkDecodeChatPlain(b *testing.B) {
	codec := NewOpenAIChat()
	b.ReportAllocs()
	for b.Loop() {
		req := benchRequest("deepseek-chat")
		if _, err := codec.Decode(req, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// 带后缀的路径与不带后缀走同一套解析；把两者放在一起，
// 可以直接看出后缀扫描本身的成本。
func BenchmarkDecodeChatWithSuffix(b *testing.B) {
	codec := NewOpenAIChat()
	b.ReportAllocs()
	for b.Loop() {
		req := benchRequest("deepseek-chat@thinking:off")
		if _, err := codec.Decode(req, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// 数据面每个 attempt 都会做的事：解码 → 建 SDK 请求 → 夹取档位 → 写方言。
func BenchmarkAttemptPrep(b *testing.B) {
	codec := NewOpenAIChat()
	model := &routing.UpstreamModel{ModelID: "deepseek-chat", ThinkingDialect: dialect.DeepSeek}
	b.ReportAllocs()
	for b.Loop() {
		req := benchRequest("deepseek-chat")
		ing, err := codec.Decode(req, 0)
		if err != nil {
			b.Fatal(err)
		}
		up := ing.BuildRosetta()
		up.Model = model.ModelID
		ing.ApplyThinkingCapability(up, model, "chat")
		ing.ApplyProtocolThinking(up, model, "chat")
	}
}

func BenchmarkAttemptPrepWithSuffix(b *testing.B) {
	codec := NewOpenAIChat()
	model := &routing.UpstreamModel{ModelID: "deepseek-chat", ThinkingDialect: dialect.DeepSeek}
	b.ReportAllocs()
	for b.Loop() {
		req := benchRequest("deepseek-chat@thinking:off")
		ing, err := codec.Decode(req, 0)
		if err != nil {
			b.Fatal(err)
		}
		up := ing.BuildRosetta()
		up.Model = model.ModelID
		ing.ApplyThinkingCapability(up, model, "chat")
		ing.ApplyProtocolThinking(up, model, "chat")
	}
}
