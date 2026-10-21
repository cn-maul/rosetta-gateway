package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// trickleUpstream 是一个"持续吐分片"的假上游。
//
// # 为什么写失败测试需要它
//
// 这条路径的触发条件是"**失败之后**还有一次写"。只吐一个分片就挂住的
// 假上游给不了这个机会：网关写完首片后主循环阻塞在 stream.Next()，
// 此时测试再怎么 failNow() 也不会有第二次写，writeErr 永远是 nil。
// 要测的必须是"上游还在产、下游已经死了"那才是汇总 *X12 描述的泄漏
// 场景：上游还在按 token 计费，而我们还在为死连接付钱。
type trickleUpstream struct {
	srv *httptest.Server
}

func newTrickleUpstream(t *testing.T, chunks int, gap time.Duration) *trickleUpstream {
	t.Helper()
	tr := &trickleUpstream{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			chunk := fmt.Sprintf(
				`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"content":"chunk-%d"},"finish_reason":null}]}`, i)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
		// 分片吐完仍不收尾：要么网关发现下游写失败后主动关流（ctx 取消），要么等
		// 测试清理。不提前发 [DONE]，否则流会正常结束、测不到写失败。
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}
