package outwire

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// WriteErrorReporter 报告编码过程中**第一次**向下游写入失败的原因。
//
// # 为什么需要它（2026-10-21 修 *X12 / 汇总 §3.10）
//
// 三个 sink 的所有 SSE 写点此前都丢弃 fmt.Fprintf 的返回值
// （openai_chat.go 的 WriteEvent/WriteComment、anthropic.go 的
// writeRaw/Keepalive、openai_responses.go 的 writeEvent/Keepalive）。
// 下游断开或挂死之后写入必然失败，但状态机照常推进：
//
//   - 残缺的流被当成**成功**收尾（status=ok），于是「先 200 再断流」
//     这类故障既不进熔断计数，也不在用量里留痕；
//   - handler 与上游流继续跑到底（上游还在按 token 计费），
//     只为了把字节写进一个已经死掉的连接。
//
// 修复不是「写失败就 panic」，而是**把失败变成可观测的终态**：
// 主循环在流结束后查一次 FirstWriteError，有值就把这一条标成下游写失败
// （见 cmd/gateway 的 attemptStream），于是它不再被记成成功。
//
// # 为什么用能力探测而不是把方法加进 StreamSink
//
// 与 BindWriteDeadline / BindDowngradedTools 同一理由：接口已有三个实现
// 加测试替身，加方法会强制它们一起改，而「写失败上报」只在真实
// ResponseWriter 路径上有意义（测试替身用 bytes.Buffer，永不失败）。
type WriteErrorReporter interface {
	// WriteError 返回第一次写失败的原因；从未失败时为 nil。
	// 实现必须可并发调用（心跳 goroutine 与主循环都会写）。
	WriteError() error
}

// FirstWriteError 做能力探测并返回 sink 的第一次写失败。
//
// 不支持上报的 sink（测试替身、未来新增但未实现的实现）返回 nil ——
// 也就是退回「不看写错误」的旧行为，绝不因为探测失败而改变既有语义。
func FirstWriteError(s StreamSink) error {
	if r, ok := s.(WriteErrorReporter); ok {
		return r.WriteError()
	}
	return nil
}

// IsDownstreamWriteError 判断一个写错误是否是「下游连接已经不可用」。
//
// 归因很重要：下游的问题**不该**计入上游熔断（否则客户端一断网就把
// 健康上游熔断）。判据只认明确表示「连接不可用」的错误族，认不出来的
// 一律按「网关自身错误」处理 —— 宁可把自家 bug 当自家 bug，
// 也不要悄悄算到客户端头上。
func IsDownstreamWriteError(err error) bool {
	if err == nil {
		return false
	}
	// 对端重置 / 管道破裂：任何平台的写入端都可能看到这两类。
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// 连接已被关闭（连接复用或 handler 提前返回后的写入）。
	if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	// net.Error 覆盖超时与底层连接错误（*net.OpError 实现它）。
	// 放在最后：它的覆盖面最广，前面几条是更精确的判据。
	var netErr net.Error
	return errors.As(err, &netErr)
}
