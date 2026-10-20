package outwire

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// deadlineBinder 必须与 BindWriteDeadline 里的匿名接口逐字一致：
// interface{ WithResponseWriter(http.ResponseWriter) } —— 无返回值。
// 带返回值的那版 WithResponseWriter(rw) *SSEWriter **不满足**它：
// Go 的方法集按签名精确匹配，不看返回值是否被调用方丢弃。
type deadlineBinder interface{ WithResponseWriter(http.ResponseWriter) }

var (
	_ deadlineBinder = (*SSEWriter)(nil)
	_ deadlineBinder = (*AnthropicSSE)(nil)
	_ deadlineBinder = (*ResponsesSSE)(nil)
	_ deadlineBinder = (*openaiSink)(nil)
)

// TestBindWriteDeadline_AllSinksImplementBinder 是「单次写截止时间」这条
// 30 秒防挂死保护的回归护栏。
//
// 这个能力此前整条失效：三个编码器的 WithResponseWriter 都带返回值，
// 能力探测全部失败，rw 恒为 nil，于是 BindWriteDeadline 对每种协议都是
// no-op。下游挂死不读时 Write 阻塞在 send buffer，handler 与上游流一起泄漏，
// 而空闲看门狗根本跑不到（主循环卡在写里）。
//
// 失效形式是静默的：没有编译错误、没有日志，只有线上偶发的连接泄漏。
func TestBindWriteDeadline_AllSinksImplementBinder(t *testing.T) {
	rec := httptest.NewRecorder()
	sinks := map[string]StreamSink{
		"openai_chat":      NewOpenAISink(NewSSEWriter(rec, nopFlusher{}, "chatcmpl-x", "m", 0)),
		"anthropic":        NewAnthropicSSE(rec, nopFlusher{}, "msg_x", "m"),
		"openai_responses": NewResponsesSSE(rec, nopFlusher{}, "resp_x", "m", nil),
	}
	for name, s := range sinks {
		if _, ok := s.(deadlineBinder); !ok {
			t.Errorf("%s sink 不满足 deadlineBinder —— 写截止时间对该协议静默失效", name)
		}
	}
}

// TestBindWriteDeadline_BindsNonNil 确认绑定后内部 rw 确实非 nil。
// openaiSink 是唯一带转发层的实现（sse 在它里面），值得单独钉一下。
func TestBindWriteDeadline_BindsNonNil(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := NewOpenAISink(NewSSEWriter(rec, nopFlusher{}, "chatcmpl-x", "m", 0))
	BindWriteDeadline(sink, rec)

	os, ok := sink.(*openaiSink)
	if !ok {
		t.Fatalf("unexpected sink type %T", sink)
	}
	if os.sse.rw == nil {
		t.Fatal("BindWriteDeadline 后 sse.rw 仍为 nil —— 30s 写截止时间未生效")
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}
