package inwire

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 超限 body 必须以 *http.MaxBytesError 报出（调用方据此映射 413），而不是被
// LimitReader 静默截断后以「unexpected end of JSON input」报 400 —— 那会让
// 客户端分不清「body 太大该减内容」还是「body 格式错该改请求」。
func TestDecodeOpenAIChatRequest_BodyTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 128)+`"}]}`))

	_, err := DecodeOpenAIChatRequest(req, 16)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected *http.MaxBytesError, got %v", err)
	}
	if tooLarge.Limit != 16 {
		t.Fatalf("limit = %d, want 16", tooLarge.Limit)
	}
}

// 恰好等于上限的 body 必须正常解码：「多读一个字节再判长度」不能误伤满额请求。
func TestDecodeOpenAIChatRequest_ExactlyAtLimit(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	got, err := DecodeOpenAIChatRequest(req, int64(len(body)))
	if err != nil {
		t.Fatalf("decode at limit: %v", err)
	}
	if got.Model != "m" || len(got.Messages) != 1 {
		t.Fatalf("unexpected decode result: %+v", got)
	}
}
