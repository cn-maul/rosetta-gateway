package outwire

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/cn-maul/rosetta"
)

// errWriter 是一个从第 N 次写开始失败的 writer（0 = 每次都失败）。
//
// 用它而不是 http.ResponseWriter 的测试替身：我们要验证的是「字节真的
// 写不出去」时 sink 与调用方的行为，而 ResponseRecorder 永不失败，
// 于是这条路径在旧测试体系里根本没有被触发的可能。
type errWriter struct {
	remaining int
	err       error
	written   int
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	w.remaining--
	w.written += len(p)
	return len(p), nil
}

func (w *errWriter) Flush() {}

// errWriter 同时实现 http.ResponseWriter：sink 的构造签名收的是它
// （写截止时间的能力探测只需要这个类型），Header 与 WriteHeader 用不到
// 但必须满足接口。
func (w *errWriter) Header() http.Header        { return http.Header{} }
func (w *errWriter) WriteHeader(statusCode int) {}

var (
	_ http.ResponseWriter = (*errWriter)(nil)
	_ http.Flusher        = (*errWriter)(nil)
)

// TestWriteErrorReporter_AllSinksCaptureFirstFailure 钉住 *X12 / A11-T3：
// 三个 sink 的每一个 SSE 写点都必须把第一次写失败记录下来。
//
// 旧实现全部丢弃 fmt.Fprintf 的返回值，于是下游断开后状态机照常推进，
// 残缺的流被当成 status=ok 收尾 —— 既不进熔断计数，用量里也无痕。
// 这条测试是「写失败会被看见」的最低门禁：任何一处写点丢掉错误，
// 对应协议的断言就会失败。
func TestWriteErrorReporter_AllSinksCaptureFirstFailure(t *testing.T) {
	usage := rosetta.Usage{InputTokens: 10, OutputTokens: 5}
	ev := &rosetta.Event{Type: rosetta.EventTextDelta, Text: "hello"}

	for _, tc := range []struct {
		name string
		sink func(w http.ResponseWriter) StreamSink
	}{
		{"openai_chat", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewOpenAISink(NewSSEWriter(w, fl, "chatcmpl-x", "m", 0))
		}},
		{"anthropic", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewAnthropicSSE(w, fl, "msg_x", "m")
		}},
		{"openai_responses", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewResponsesSSE(w, fl, "resp_x", "m", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boom := errors.New("connection reset by peer")
			w := &errWriter{remaining: 0, err: boom}
			s := tc.sink(w)

			// 事件写 + 收尾写都要触发：两类写点各自都可能失败。
			s.Event(ev)
			s.Finish("ok", rosetta.StopEnd, usage, true)

			got := FirstWriteError(s)
			if got == nil {
				t.Fatalf("%s: 写失败没有被记录下来 —— 下游断开会被当成成功收尾", tc.name)
			}
			if !errors.Is(got, boom) {
				t.Fatalf("%s: WriteError() = %v，期望底层错误 %v", tc.name, got, boom)
			}
		})
	}
}

// TestWriteErrorReporter_KeepaliveFailureCaptured 覆盖心跳写点。
//
// 心跳与主事件循环并发写同一个 writer，是最先撞上「下游已经不读」的路径
// （下游挂死时主循环可能还堵在等上游事件上）。漏掉它，看门狗与心跳就都
// 看不见下游已死，handler 会继续把上游的 token 烧完。
func TestWriteErrorReporter_KeepaliveFailureCaptured(t *testing.T) {
	for _, tc := range []struct {
		name string
		sink func(w http.ResponseWriter) StreamSink
	}{
		{"openai_chat", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewOpenAISink(NewSSEWriter(w, fl, "chatcmpl-x", "m", 0))
		}},
		{"anthropic", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewAnthropicSSE(w, fl, "msg_x", "m")
		}},
		{"openai_responses", func(w http.ResponseWriter) StreamSink {
			fl, _ := w.(http.Flusher)
			return NewResponsesSSE(w, fl, "resp_x", "m", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &errWriter{remaining: 0, err: errors.New("broken pipe")}
			s := tc.sink(w)
			s.Keepalive()
			if err := FirstWriteError(s); err == nil {
				t.Fatalf("%s: 心跳写失败没有被记录", tc.name)
			}
		})
	}
}

// TestWriteErrorReporter_NoFailureStaysNil 确认成功路径不回假错误：
// 能力探测的默认语义是「没有失败」，绝不能因为实现了接口就报错。
func TestWriteErrorReporter_NoFailureStaysNil(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewOpenAISink(NewSSEWriter(rec, nopFlusher{}, "chatcmpl-x", "m", 0))
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "hi"})
	s.Finish("ok", rosetta.StopEnd, rosetta.Usage{InputTokens: 1, OutputTokens: 1}, true)
	if err := FirstWriteError(s); err != nil {
		t.Fatalf("成功路径 WriteError() = %v，期望 nil", err)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("成功路径没有写出内容: %q", rec.Body.String())
	}
}

// TestWriteErrorReporter_UnsupportedSinkReturnsNil 钉住能力探测的兜底：
// 测试替身与未来未实现的实现必须得到 nil（退回旧行为），而不是 panic
// 或凭空一个错误。
func TestWriteErrorReporter_UnsupportedSinkReturnsNil(t *testing.T) {
	if err := FirstWriteError(plainSink{}); err != nil {
		t.Fatalf("不支持上报的 sink 应得 nil，得到 %v", err)
	}
}

// plainSink 是刻意不实现 WriteErrorReporter 的最小 StreamSink。
type plainSink struct{}

func (plainSink) Event(*rosetta.Event)                                   {}
func (plainSink) Finish(string, rosetta.StopReason, rosetta.Usage, bool) {}
func (plainSink) Keepalive()                                             {}
func (plainSink) WroteContent() bool                                     { return false }
func (plainSink) SawTerminal() bool                                      { return false }

// TestIsDownstreamWriteError_ClassifiesConnectionFailures 钉住归因边界。
//
// 这一条比看起来重要：写失败若被误判成上游故障，客户端一断网就会把健康上游
// 熔断（错误率虚高 → 熔断器开 → 整条链被跳过）。判据只认「连接不可用」族，
// 认不出来的一律按网关自身错误处理。
func TestIsDownstreamWriteError_ClassifiesConnectionFailures(t *testing.T) {
	// 用**真实**错误形态而不是错误字符串：生产里写失败是
	// net.OpError -> os.SyscallError -> syscall.Errno 三层包装。
	// 字符串匹配会给出与线上不同的结论 —— 这也是 IsDownstreamWriteError
	// 用 errors.Is / errors.As 而不是 strings.Contains 的原因。
	brokenPipe := &net.OpError{Op: "write", Net: "tcp",
		Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}
	reset := &net.OpError{Op: "write", Net: "tcp",
		Err: &os.SyscallError{Syscall: "write", Err: syscall.ECONNRESET}}
	for name, err := range map[string]error{
		"broken pipe":        brokenPipe,
		"connection reset":   reset,
		"net err closed":     netErrClosed{},
		"wrapped connection": errors.Join(errors.New("outer"), netErrClosed{}),
	} {
		if !IsDownstreamWriteError(err) {
			t.Errorf("%s 应判为下游写失败", name)
		}
	}
	// 单独再钉 EPIPE：它是「客户端提前关连接」最常见的形态，
	// 漏掉它等于把每一次正常断连都记成上游故障并进熔断计数。
	if !IsDownstreamWriteError(brokenPipe) {
		t.Error("EPIPE 未被识别 —— 下游断连会被误判成上游故障并进熔断计数")
	}
	if IsDownstreamWriteError(nil) {
		t.Error("nil 不应判为写失败")
	}
	if IsDownstreamWriteError(errors.New("some gateway bug")) {
		t.Error("普通错误不应判为下游写失败 —— 那会把自家 bug 记到客户端头上")
	}
}

// netErrClosed 实现 net.Error，覆盖「超时/连接类」这一族。
type netErrClosed struct{}

func (netErrClosed) Error() string   { return "use of closed network connection" }
func (netErrClosed) Timeout() bool   { return false }
func (netErrClosed) Temporary() bool { return false }
