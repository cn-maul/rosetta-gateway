package outwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cn-maul/rosetta"
)

// TestUpstreamErrorDetail_Message 基础路径：取 Message（上游那句人话）。
func TestUpstreamErrorDetail_Message(t *testing.T) {
	apiErr := &rosetta.APIError{StatusCode: 429, Message: "rate limit exceeded, retry in 60s"}
	got := UpstreamErrorDetail(apiErr)
	if !strings.Contains(got, "message: rate limit exceeded, retry in 60s") {
		t.Fatalf("got %q", got)
	}
}

// Message 与 Raw 同时存在时必须都保留：结构化 400 的关键信息常在
// code / param / request_id 里，不能因为有一句 message 就丢掉正文。
func TestUpstreamErrorDetail_MessageAndRaw(t *testing.T) {
	apiErr := &rosetta.APIError{
		StatusCode: 400,
		Message:    "tool call ids mismatch",
		Raw:        json.RawMessage(`{"error":{"type":"invalid_request_error","code":"tool_call_mismatch","param":"messages"}}`),
	}
	got := UpstreamErrorDetail(apiErr)
	for _, want := range []string{"message: tool call ids mismatch", "raw:", "tool_call_mismatch", `"param":"messages"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail 缺少 %q: %q", want, got)
		}
	}
}

// TestUpstreamErrorDetail_FallsBackToRaw Message 为空时退回原始响应体。
//
// 部分上游（尤其中转站）错误体是 {"detail": "..."} 这类非标形状，
// SDK 解析不出 Message 时 Raw 里仍然有完整正文 —— 那是运维唯一能看到的。
func TestUpstreamErrorDetail_FallsBackToRaw(t *testing.T) {
	raw := json.RawMessage(`{"detail":"insufficient balance for this billing account"}`)
	apiErr := &rosetta.APIError{StatusCode: 402, Raw: raw}
	got := UpstreamErrorDetail(apiErr)
	if !strings.Contains(got, "insufficient balance") {
		t.Fatalf("detail 应含 Raw 正文，got %q", got)
	}
}

// TestUpstreamErrorDetail_MasksSecrets 落库文本绝不能含明文凭据。
//
// 这条测试是整个特性的安全底线：上游会把调用方的 key 回显在错误 message 里
// （「invalid api key: sk-abc...」是真实存在的上游行为）。SDK 只在 Error()
// 拼串时掩码，Message 字段是原文 —— 我们截留的正是这个字段，必须自己掩。
func TestUpstreamErrorDetail_MasksSecrets(t *testing.T) {
	key := "sk-AbCdEf1234567890XyZ"
	cases := []struct {
		name string
		err  error
	}{
		{"Message 含 key", &rosetta.APIError{StatusCode: 401, Message: "invalid api key: " + key}},
		{"Raw 含 key", &rosetta.APIError{StatusCode: 401, Raw: json.RawMessage(`{"error":"unauthorized: ` + key + `"}`)}},
		{"JWT 形状", &rosetta.APIError{StatusCode: 401, Message: "bad token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N-xg"}},
		{"AWS key 形状", &rosetta.APIError{StatusCode: 401, Message: "aws AKIAIOSFODNN7EXAMPLE rejected"}},
	}
	for _, c := range cases {
		got := UpstreamErrorDetail(c.err)
		if strings.Contains(got, key) {
			t.Fatalf("%s: 明文 key 泄漏进落库文本: %q", c.name, got)
		}
		// JWT / AWS 形状同样不能原样穿透
		if strings.Contains(got, "eyJhbGciOiJSUzI1NiJ9") || strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("%s: 凭据形状未掩码: %q", c.name, got)
		}
		if !strings.Contains(got, "***") {
			t.Fatalf("%s: 应看到掩码标记，got %q", c.name, got)
		}
	}
}

// TestUpstreamErrorDetail_TruncatesAtRuneBoundary 超长截断且不劈开汉字。
//
// 上游中文报错（DeepSeek / 月之暗面常见）若按字节截，最后一个字会剩
// 残字节，JSON 编码后变 replacement char —— 界面显示乱码尾巴。
func TestUpstreamErrorDetail_TruncatesAtRuneBoundary(t *testing.T) {
	const maxDetailBytes = 8 << 10
	long := strings.Repeat("余额不足请充值", maxDetailBytes/3) // 每字 3 字节，明显超上限
	apiErr := &rosetta.APIError{StatusCode: 402, Message: long}
	got := UpstreamErrorDetail(apiErr)
	if len(got) > maxDetailBytes {
		t.Fatalf("未截断: %d 字节", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断结果不是合法 UTF-8（劈开了汉字）")
	}
	// 截掉的字节数必须 <4（一个汉字最多 3 字节 + 边界修正），
	// 否则就是「少截了整整好几个字」—— 截断点远离上界同样是 bug。
	if maxDetailBytes-len(got) >= 4 {
		t.Fatalf("截断点离上界太远（%d 字节），rune 回退逻辑可能有误", maxDetailBytes-len(got))
	}
}

// TestUpstreamErrorDetail_NoResponseStillExplainsFailure 没有响应体不等于
// 没有可排障原因：连接阶段的 DNS/TCP/TLS/proxy 错误，以及流截断/解码失败，
// 都必须把底层原因留给调用历史。只有 nil 与无关的本地普通错误返回空串。
func TestUpstreamErrorDetail_NoResponseStillExplainsFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"TransportError", &rosetta.TransportError{Err: errors.New("dial tcp: connection refused")}, "connection refused"},
		{"ErrUpstreamMalformed", fmt.Errorf("wrap: %w", rosetta.ErrUpstreamMalformed), "could not be decoded"},
		{"ErrStreamTruncated", fmt.Errorf("wrap: %w", rosetta.ErrStreamTruncated), "truncated"},
		{"普通错误", errors.New("something local"), ""},
	}
	for _, c := range cases {
		got := UpstreamErrorDetail(c.err)
		if c.want == "" && got != "" {
			t.Fatalf("%s: 期望空串，got %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Fatalf("%s: 详情缺少 %q，got %q", c.name, c.want, got)
		}
	}
}

func TestUpstreamErrorDetail_TransportMasksEndpointAndSecrets(t *testing.T) {
	key := "sk-AbCdEf1234567890XyZ"
	err := &rosetta.TransportError{Err: errors.New("proxyconnect tcp: dial tcp 127.0.0.1:7897: connect: connection refused; key=" + key)}
	got := UpstreamErrorDetail(err)
	if strings.Contains(got, key) {
		t.Fatalf("transport 详情泄漏 key: %q", got)
	}
	if !strings.Contains(got, "proxyconnect") || !strings.Contains(got, "connection refused") {
		t.Fatalf("transport 详情缺少根因: %q", got)
	}
}

// TestUpstreamErrorDetail_InBandError 上游 200 + 错误体（中转站常见）也要能截留。
func TestUpstreamErrorDetail_InBandError(t *testing.T) {
	apiErr := &rosetta.APIError{StatusCode: 200, InBand: true, Message: "your workspace has exhausted its quota"}
	got := UpstreamErrorDetail(apiErr)
	if !strings.Contains(got, "message: your workspace has exhausted its quota") {
		t.Fatalf("in-band 错误应同样截留，got %q", got)
	}
}
