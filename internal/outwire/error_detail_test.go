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
	if got != "rate limit exceeded, retry in 60s" {
		t.Fatalf("got %q", got)
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
	long := strings.Repeat("余额不足请充值", 500) // 每个汉字 3 字节，远超 1KB
	apiErr := &rosetta.APIError{StatusCode: 402, Message: long}
	got := UpstreamErrorDetail(apiErr)
	if len(got) > 1024 {
		t.Fatalf("未截断: %d 字节", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断结果不是合法 UTF-8（劈开了汉字）")
	}
	// 截掉的字节数必须 <4（一个汉字最多 3 字节 + 边界修正），
	// 否则就是「少截了整整好几个字」—— 截断点远离上界同样是 bug。
	if 1024-len(got) >= 4 {
		t.Fatalf("截断点离上界太远（%d 字节），rune 回退逻辑可能有误", 1024-len(got))
	}
}

// TestUpstreamErrorDetail_NoBodyKinds 无正文的三类失败必须返回空串。
//
// TransportError（连接层失败，压根没收到响应体）、ErrUpstreamMalformed
// （SDK 刻意不附 body）与 ErrStreamTruncated 都没有可截留的上游原文。
// 这不是缺陷，是边界 —— 调用历史对这类失败显示「无更多详情」。
func TestUpstreamErrorDetail_NoBodyKinds(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"TransportError", &rosetta.TransportError{Err: errors.New("dial tcp: connection refused")}},
		{"ErrUpstreamMalformed", fmt.Errorf("wrap: %w", rosetta.ErrUpstreamMalformed)},
		{"ErrStreamTruncated", fmt.Errorf("wrap: %w", rosetta.ErrStreamTruncated)},
		{"普通错误", errors.New("something local")},
	}
	for _, c := range cases {
		if got := UpstreamErrorDetail(c.err); got != "" {
			t.Fatalf("%s: 期望空串，got %q", c.name, got)
		}
	}
}

// TestUpstreamErrorDetail_InBandError 上游 200 + 错误体（中转站常见）也要能截留。
func TestUpstreamErrorDetail_InBandError(t *testing.T) {
	apiErr := &rosetta.APIError{StatusCode: 200, InBand: true, Message: "your workspace has exhausted its quota"}
	got := UpstreamErrorDetail(apiErr)
	if got != "your workspace has exhausted its quota" {
		t.Fatalf("in-band 错误应同样截留，got %q", got)
	}
}
