package outwire

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta"
)

// Responses 的 usage 明细字段（cached_tokens / reasoning_tokens）必须
// **流式与非流式两侧都在**。
//
// # 为什么值得单测（2026-10-21 修审计汇总 O6 的遗留）
//
// Codex / Agent 类客户端默认走流式，读的是 response.completed 里的 usage。
// 而流式侧此前一条都没有报 input_tokens_details.cached_tokens ——
// 非流式侧反而有。于是同一次调用，走 Responses 流式的客户端看到
// 「缓存命中 0」，走 Chat 的客户端看到真实值。
//
// 这不是「上游没报」：CachedInputTokens 早就被 SDK 解析并写进了
// usage_records.cached_tokens，只是出站编码时在流式这一路被丢了。
// 线上表现就是用户看到的现象 —— 接入应用后缓存命中率始终为 0，
// 换成 chat 接口就正常。
//
// 两侧一起测而不是只测流式：这个字段当初就是「只在一边补了」，
// 只测一边等于允许它在另一边再丢一次。

type usageEnvelope struct {
	Usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		TotalTokens        int64 `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func TestResponsesNonStream_ReportsCachedAndReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteResponsesResponse(rec, &rosetta.ChatResponse{
		ID: "up-1", Model: "x",
		Content: []rosetta.Block{{Type: rosetta.BlockText, Text: "答"}},
		Usage: rosetta.Usage{
			InputTokens: 100, CachedInputTokens: 72, ReasoningTokens: 30,
			OutputTokens: 20, TotalTokens: 120,
		},
	}, "gw-model")

	var out usageEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Usage.InputTokensDetails.CachedTokens != 72 {
		t.Errorf("非流式 input_tokens_details.cached_tokens = %d, want 72", out.Usage.InputTokensDetails.CachedTokens)
	}
	if out.Usage.OutputTokensDetails.ReasoningTokens != 30 {
		t.Errorf("非流式 output_tokens_details.reasoning_tokens = %d, want 30", out.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

func TestResponsesStream_ReportsCachedAndReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewResponsesSSE(rec, nil, "resp-1", "gw-model", nil)
	s.Event(&rosetta.Event{Type: rosetta.EventTextDelta, Text: "答"})
	s.Finish("ok", "", rosetta.Usage{
		InputTokens: 100, CachedInputTokens: 72, ReasoningTokens: 30,
		OutputTokens: 20, TotalTokens: 120,
	}, false)

	usage := lastCompletedUsage(t, s)
	if usage == nil {
		t.Fatal("没捕获到 response.completed 事件")
	}
	if usage.Usage.InputTokensDetails.CachedTokens != 72 {
		t.Errorf("流式 input_tokens_details.cached_tokens = %d, want 72 —— "+
			"Codex / Agent 客户端读的就是这个事件，缺了它命中率一律显示 0",
			usage.Usage.InputTokensDetails.CachedTokens)
	}
	if usage.Usage.OutputTokensDetails.ReasoningTokens != 30 {
		t.Errorf("流式 output_tokens_details.reasoning_tokens = %d, want 30",
			usage.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

// lastCompletedUsage 从 sink 已写出的事件里取最后一个 response.completed 的 usage。
func lastCompletedUsage(t *testing.T, s *ResponsesSSE) *usageEnvelope {
	t.Helper()
	rec, ok := s.w.(*httptest.ResponseRecorder)
	if !ok {
		t.Fatalf("sink 的 writer 不是 ResponseRecorder，无法回读已写出的事件")
	}
	var last *usageEnvelope
	for _, line := range splitLines(rec.Body.String()) {
		const prefix = "data: "
		if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
			continue
		}
		var envelope struct {
			Type     string        `json:"type"`
			Response usageEnvelope `json:"response"`
		}
		if err := json.Unmarshal([]byte(line[len(prefix):]), &envelope); err != nil {
			continue
		}
		if envelope.Type != "response.completed" {
			continue
		}
		u := envelope.Response
		last = &u
	}
	return last
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
