package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 上游在 responses 流的终止事件里给出自相矛盾的 sealed 载荷时，网关必须
// 如实记一条失败，但**不能**因此把整家上游熔断。
//
// # 现场（2026-11-11 tiantoken）
//
// 同一个 reasoning item 被描述两次：output_item.done 带一份 encrypted_content，
// 终止事件 response.completed 的 output 数组里带另一份。SDK 判 errSealConflict
// 并包成 ErrUpstreamMalformed（provider_openai_responses.go 的 mapItemErr）。
// 这是上游翻译层的缺陷，但修复前它按普通已提交失败记账：默认阈值 3 一到，
// 整家上游被熔断，之后所有请求变成「502 · upstream_error」（且那时候 502
// 连 error_detail 都没有），看起来像网关自己坏了。
//
// 所以这里断言两件事必须同时成立：错误要留痕（错误码/详情可查），
// 目标要还活着（第四次请求仍然打到上游，而不是被熔断跳过）。
func TestE2E_Responses_SealConflictIsRecordedButDoesNotCircuitBreak(t *testing.T) {
	const (
		sealedDone  = "gAAAAA-sealed-from-done"
		sealedFinal = "gAAAAA-sealed-from-completed"
	)

	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		events := []string{
			`{"type":"response.created","response":{"id":"resp_up_s","object":"response","created_at":1,"model":"relay-model","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_conflict"}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_conflict","output_index":0,"summary_index":0,"delta":"先想一下"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_conflict","summary":[{"type":"summary_text","text":"先想一下"}],"encrypted_content":"` + sealedDone + `"}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_s","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","item_id":"msg_s","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_s","output_index":1,"content_index":0,"delta":"答案是 42"}`,
			`{"type":"response.content_part.done","item_id":"msg_s","output_index":1,"content_index":0,"part":{"type":"output_text","text":"答案是 42","annotations":[]}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_s","status":"completed","role":"assistant","content":[{"type":"output_text","text":"答案是 42"}]}}`,
			// 关键：终止事件里同一个 reasoning item 换了一份密文。
			`{"type":"response.completed","response":{"id":"resp_up_s","object":"response","created_at":1,"model":"relay-model","status":"completed","output":[` +
				`{"type":"reasoning","id":"rs_conflict","summary":[{"type":"summary_text","text":"先想一下"}],"encrypted_content":"` + sealedFinal + `"}` +
				`],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer up.Close()

	h, db, pool := buildHarnessProtocolsFull(t, []harnessChain{
		{slug: "good", url: up.URL, protocol: "response"},
	}, false, ingress.NewResponses())

	body := `{
		"model":"flash","stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"1+1=?"}]}]
	}`

	// 默认熔断阈值是 3，所以这里正好打三次：修复前第三次就会熔断目标。
	const attempts = 3
	for i := 0; i < attempts; i++ {
		rec := e2ePost(h, "/v1/responses", testAccessKey, body)
		// 已提交：内容与思考都已经交付，所以仍是 200（没有可写的错误体）。
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求期望 200（流已提交），实际 %d，body=%s", i+1, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "答案是 42") {
			t.Fatalf("第 %d 次请求没拿到正文，说明不是「已提交后失败」这条路径：%s", i+1, rec.Body.String())
		}
	}

	// 落库：必须留痕，且错误码要能把这个缺陷与普通 upstream_error 分开。
	rows := e2eWaitUsageCount(t, db, "k1", attempts)
	for i, r := range rows {
		if r.Status != store.UsageStatusError {
			t.Errorf("第 %d 条落库 status = %q，期望 error", i+1, r.Status)
		}
		if r.ErrorCode != "stream_malformed" {
			t.Errorf("第 %d 条落库 error_code = %q，期望 stream_malformed", i+1, r.ErrorCode)
		}
		if !strings.Contains(r.ErrorDetail, "conflicting sealed payloads") {
			t.Errorf("第 %d 条 error_detail 未保留上游的原始判定，排障时无从下手: %q", i+1, r.ErrorDetail)
		}
	}

	// 核心断言：三次失败之后目标必须仍然可用。
	if !pool.TargetAvailable("good#t0") {
		t.Fatal("stream_malformed 不该计入目标熔断：三次请求就把整家上游打成 502，比缺陷本身危害更大")
	}
	before := hits.Load()
	rec := e2ePost(h, "/v1/responses", testAccessKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("第四次请求应照常打到上游，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != before+1 {
		t.Fatalf("第四次请求没有打到上游（命中 %d → %d），目标已被熔断跳过", before, got)
	}
}
