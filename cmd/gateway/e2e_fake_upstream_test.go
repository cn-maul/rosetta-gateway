package main

// 动态端到端测试用的假上游。
//
// # 为什么单独一个文件
//
// failover_test.go 里现有的 fake* 都是「给定响应、不记请求」。本轮的场景需要
// **证据**：断言「上游一次都没被碰」「调用的顺序是 A→B」「断开时上游观察到
// ctx 取消」这些都不能只看状态码 —— 一个 403 完全可能在打完上游之后才写出，
// 状态码断言照样绿，而那次上游调用已经真的花掉了额度。
//
// 所以这里的每个 fake 都带**计数器 / 请求日志 / 可观测的断开信号**，
// 让「没被碰」和「碰了几次、按什么顺序」变成可断言的事实。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/server"
)

// upstreamCall 是一次到达假上游的请求快照。
//
// 记下 body 与 path 才能断言「转发过去的模型名是上游模型的 model_id，而不是
// 客户端写的公开名」—— 这个映射是路由层最容易出错、也最难从响应反推的地方。
type upstreamCall struct {
	Method string
	Path   string
	Body   string
	Auth   string
}

// fakeRecorder 是一个会记账的假上游。
//
// 并发安全：服务端 handler 在 httptest 自己的 goroutine 里跑，而断言在测试
// goroutine 里跑，两者并发访问计数与日志，必须加锁。生产代码里
// `go test -race` 会抓这个，但普通 `go test` 不会 —— 所以这里主动做好。
type fakeRecorder struct {
	srv *httptest.Server

	mu    sync.Mutex
	calls []upstreamCall
}

func (f *fakeRecorder) URL() string { return f.srv.URL }

// Count 返回到达本上游的请求数。
func (f *fakeRecorder) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Calls 返回到目前为止的请求日志副本。
func (f *fakeRecorder) Calls() []upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstreamCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// LastBody 返回最近一次请求体；没有请求时返回空串。
func (f *fakeRecorder) LastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1].Body
}

// record 记一次请求并把 body 读完（读完才能让连接复用，且 body 要落进日志）。
func (f *fakeRecorder) record(r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, upstreamCall{
		Method: r.Method, Path: r.URL.Path, Body: string(b),
		Auth: r.Header.Get("Authorization"),
	})
}

// newFakeUpstream 造一个记账假上游。handler 收到的每个请求都会被记下来，
// 之后交给 reply 决定回什么。
func newFakeUpstream(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *fakeRecorder {
	t.Helper()
	f := &fakeRecorder{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		reply(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ---- 各类响应的构造函数 ----

// okJSON 是一个合法的 chat.completion 响应体（含 usage）。
// usage 的 total 刻意设成 8（prompt 5 + completion 3），与既有 fakeGood 一致，
// 便于和已有用例对照。
func okJSON(model, content string) string {
	return fmt.Sprintf(`{"id":"cmpl-1","object":"chat.completion","created":0,
		"model":%q,
		"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, model, content)
}

// replyOK 恒回 200 + 正常 completion。
func replyOK(content string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okJSON("upstream-model", content))
	}
}

// replyStatus 恒回某个状态码 + 一个 OpenAI 形状的错误体。
func replyStatus(code int, msg string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error"},
		})
		_, _ = w.Write(body)
	}
}

// replySSE 回一条完整 SSE 流，并在末尾带 usage 块与 [DONE]。
//
// 带 usage 是必须的：不带的话扣费走 usage missing 分支恒为 0，
// 「流式扣费」用例会变成假绿（测到的是"流式不扣费"）。
func replySSE(content string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content),
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// replySSEThenFail 先写一段**真实内容**，再让连接以异常方式断掉。
//
// 专用于验证「流已写出字节之后失败，绝不能重试」这条边界：
// 用 httptest.Server 的 CloseClientConnections 或 panic 都不够可控，
// 这里用 hijack 拿到原始连接后直接 Close —— 客户端读到的是
// io.ErrUnexpectedEOF（真实截断），而不是干净的 EOF。
func replySSEThenFail(firstChunk string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", firstChunk)
		if fl != nil {
			fl.Flush()
		}
		// 拿到原始连接直接关掉：模拟「上游进程被杀 / 中间代理掐断」。
		// 不走 w 的 flush 路径，避免 net/http 补一个合法的 chunked 结束标记
		// （那样下游看到的是干净 EOF，而不是截断）。
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// disconnectingUpstream 是一个「能观察到客户端断开」的假上游。
//
// # 为什么要专门造它
//
// 「客户端断开后上游连接是否释放」这条**不能**靠看网关侧的状态码或有没报错
// 来推断 —— 网关什么都不写、什么都不报也可能是对的（canceled 就是要静默）。
// 唯一可信的证据是**上游自己观察到 ctx 被取消**：那意味着网关真的把
// cancel 传播到了在途的上游请求。TCP 连接被归还连接池却仍在跑，
// 从网关侧完全看不出来。
type disconnectingUpstream struct {
	srv *httptest.Server

	// started 在收到请求并开始吐首片后关闭：测试据此知道「上游已在途」。
	started chan struct{}
	// released 在上游的 r.Context() 被取消（或超时兜底）时关闭。
	released chan struct{}
	// startOnce / releaseOnce 各自保护**一个** channel。
	//
	// ⚠️ 这两个 Once 必须分开。用同一个 sync.Once 去关两个不同的 channel 时，
	// 第二次 Do 是 no-op —— released 永远不会闭合，测试会误报
	// 「断开没传播到上游」。这个坑本轮实测踩过：诊断显示上游其实在 842µs 内
	// 就观察到了 ctx 取消，而等待 released 的断言恒超时。
	startOnce   sync.Once
	releaseOnce sync.Once

	// disconnectObserved 记录「断开是网关取消上游 ctx 造成的」这个事实。
	// 它与 released 的区别：released 也可能由测试兜底超时触发，
	// 所以断言必须看这个布尔量，而不是只看 released 闭合了。
	disconnectObserved atomic.Bool
}

// newDisconnectingUpstream 造一个「吐一片就挂住、等 ctx 取消」的上游。
//
// 它先发一片内容（这样网关已提交 SSE 头），然后阻塞等待 r.Context().Done()。
// 网关把客户端断开传播过来时，这个 ctx 会被取消 —— 那就是我们要的证据。
func newDisconnectingUpstream(t *testing.T) *disconnectingUpstream {
	t.Helper()
	d := &disconnectingUpstream{
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// 首片：让网关越过「首字」门槛并提交 SSE 头。之后网关进入事件循环，
		// 客户端断开时 r.Context() 才会被取消。
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		d.startOnce.Do(func() { close(d.started) })

		select {
		case <-r.Context().Done():
			// 唯一有价值的信号：网关把客户端的断开传播成了上游 ctx 取消。
			d.disconnectObserved.Store(true)
		case <-time.After(5 * time.Second):
			// 兜底：测试结束也别把连接永久挂住，否则 httptest.Server.Close
			// 会等到超时（表现为整个测试包卡住 5s+）。
		}
		d.releaseOnce.Do(func() { close(d.released) })
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// bodyHasModel 检查请求体里的 "model" 字段值，用于断言上游收到的模型名。
func bodyHasModel(body, want string) bool {
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return false
	}
	return payload.Model == want
}

// sseDataLines 抽出 SSE 响应体里所有 `data:` 行的载荷（含 [DONE]）。
// 用于断言「只看到一次成功响应」「[DONE] 恰好出现一次」这类事实。
func sseDataLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, after)
		}
	}
	return out
}

// middlewareForTest 把 handler 包一层生产同款的 server.Middleware。
//
// # 为什么每个 E2E 请求都必须走它
//
// 生产里 request_id 由 Middleware 生成，而**扣费拿它当幂等键**
// （usageRecorder.charge 对空 request_id 记 ERROR 并跳过扣费）。
// 直接调 handleIngress 会绕过中间件，于是 context 里没有 request_id，
// 扣费的断言会「因为没扣」而假绿 —— 一个不报错的假绿，比失败更危险。
//
// 走真 Middleware 而不是手工往 context 塞 key：requestIDKey 是 server 包的
// 私有类型，测试无法构造，复制字面量等于在测试里重述实现（server 一改
// 这个测试就会莫名其妙地红）。与 balance_test.go 的 postChatWithID 同手法。
func middlewareForTest(h http.HandlerFunc) http.Handler {
	return server.Middleware(h, discardLogger())
}
