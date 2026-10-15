package main

// n > 1 的端到端回归（2026-10-14，交接文档 §7.2 第 1 条）。
//
// # 为什么还要一层 e2e
//
// internal/inwire 的测试只证明了「decode 会返回 error」。真正要交付的
// 承诺是「**客户端收到一个 400，且上游一次都没被打到**」，而那取决于
// handleIngress 拿到 decode error 之后怎么映射。
//
// 「上游没被打到」单独值得断言一次：静默降级的旧行为里，请求是**完整**
// 转发到上游的、还花了钱。若修复只做了「回 400 但仍然转发」，或者
// 反过来「不转发但回了 200」，单元测试层都看不出来。

import (
	"net/http"
	"strings"
	"testing"
)

// TestE2E_Adv10_MultipleChoicesRejected 是 n>1 的入站拒绝。
//
// 三个断言缺一不可：
//
//   - 400（不是 200 也不是 5xx）：语义是「客户端的请求本身不被支持」，
//     客户端应当改代码，而不是重试。
//   - 错误体里指明 n：调用方要能据此改代码。
//   - 上游零调用：被拒的请求不该消耗上游额度。
//
// 对照组同样重要 —— n=1 必须照常 200 且真的打到上游。只测「拒绝」的
// 套件看不见「拒绝范围写宽了、正常请求也被拒」这种回归。
func TestE2E_Adv10_MultipleChoicesRejected(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db := buildHarness(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL()}}, false)
	_ = db

	// n=3 → 400，且上游零调用。
	rec := e2ePost(h, "/v1/chat/completions", testAccessKey,
		`{"model":"flash","messages":[{"role":"user","content":"hi"}],"n":3}`)
	t.Logf("Adv10 n=3: status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("n=3 期望 400，实际 %d —— 静默降级会让客户端以为自己拿到了 3 个候选", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "n=") {
		t.Errorf("错误体没有指明是 n 的问题，客户端无从改代码：%s", rec.Body.String())
	}
	if up.Count() != 0 {
		t.Errorf("被拒的请求打了上游 %d 次 —— 不该为一个不支持的请求消耗上游额度", up.Count())
	}

	// 对照：n=1 必须照常通过，且真的打到上游。
	rec = e2ePost(h, "/v1/chat/completions", testAccessKey,
		`{"model":"flash","messages":[{"role":"user","content":"hi"}],"n":1}`)
	t.Logf("Adv10 n=1: status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	if rec.Code != http.StatusOK {
		t.Errorf("n=1 期望 200，实际 %d —— 拒绝范围写宽了（把 OpenAI 默认值也拒了）", rec.Code)
	}
	if up.Count() != 1 {
		t.Errorf("n=1 的上游调用次数 = %d，期望 1", up.Count())
	}
}
