package api

import (
	"fmt"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/outwire"
)

// v1NotFound 是数据面兜底：未匹配的 /v1 路径也必须是 JSON。
//
// 不注册兜底时，未匹配的请求落到 net/http 的默认 404 —— 响应体是一行
// **text/plain**（"404 page not found"）。而 /v1 下所有正常错误都是 JSON：
// OpenAI SDK 的 res.json() 解析失败后抛 SyntaxError，客户端日志里看着像
// 网关崩了，实际只是路径拼错，或者模型 id 里的 "/" 没被正确寻址（见
// catalog.Get 里 {model...} 的说明）。
//
// 形状必须跟着别名走：/anthropic/v1/* 的调用方只认 Anthropic 的
// {"type":"error"} 结构，回一个 OpenAI 信封等于没回。
func v1NotFound(anthropic bool) http.HandlerFunc {
	writeErr := outwire.WriteOpenAIError
	if anthropic {
		writeErr = outwire.WriteAnthropicError
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// 回显方法+路径是刻意的：这类 404 的成因几乎全是客户端拼错地址，
		// 把「它到底请求了什么」原样回给对端，比让人去翻代理日志快得多。
		// 路径来自请求本身，不含任何服务端信息。
		writeErr(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("unknown endpoint %s %s", r.Method, r.URL.Path))
	}
}
