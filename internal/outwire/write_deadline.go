package outwire

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// writePerFrameTimeout 是单次写向客户端的预算（2026-10-17）。
//
// # 为什么需要它
//
// 已有的 idle 看门狗管的是「上游多久没给数据」。它看不见另一类卡死：上游正常
// 吐字，但下游客户端挂死不读（休眠、断网、被防火墙黑洞）。此时内核发送缓冲
// 填满后 Write 会一直阻塞，handler 与它占用的上游流一起泄漏 —— 上游还在按
// token 计费，网关却连看门狗都跑不到（主循环卡在写里）。
//
// # 为什么用 ResponseController 而不是包一层 writer
//
// 包装 writer 的方案要么每次写起 goroutine（流式下开销大、超时后写协程仍卡在
// 内核缓冲里），要么在超时后丢弃分片（会破坏 SSE 握手序列：role 分片丢了但
// 状态已置位，后续分片全部错位）。正确做法是给底层 net.Conn 设写截止时间：
// 由内核让 Write 自己返回超时错误，不改写任何分片语义。
//
// statusResponseWriter 已实现 Unwrap，所以 ResponseController 能穿透中间件
// 拿到底层 ResponseWriter；若拿不到 SetWriteDeadline 能力（例如测试里的
// httptest.ResponseRecorder），本次设置静默跳过，行为与改造前一致。
const writePerFrameTimeout = 30 * time.Second

// setWriteDeadline 为本次写设置截止时间。
//
// 每次写前都设一个新的 now+timeout，写完不必清除：下一次写会重新覆盖。
// handler 返回时连接关闭，残留的截止时间随之失效。
func setWriteDeadline(w http.ResponseWriter) {
	if w == nil {
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(writePerFrameTimeout)); err != nil {
		// ErrNotSupported 是常态（记录器、部分包装），不值得每条请求打日志。
		if !errors.Is(err, http.ErrNotSupported) {
			slog.Debug("stream write deadline not supported", "error", err)
		}
	}
}

// clearWriteDeadline 在流结束时清掉截止时间，避免连接被复用时带着过期的
// 写截止时间进入下一次请求（net/http 通常会在下一次写前重置，但这里不赌）。
func clearWriteDeadline(w http.ResponseWriter) {
	if w == nil {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
