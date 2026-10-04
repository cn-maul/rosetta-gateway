package outwire

import (
	"github.com/cn-maul/rosetta"
)

// StreamSink 把 rosetta 的统一事件流编码成某个下游协议的 SSE 线格式。
//
// 事件的消费方（cmd/gateway 的 attemptStream）只负责与协议无关的部分：
// 看门狗、心跳、断流状态归类、用量落库；「统一事件 → 协议分片」的翻译
// 全部收在各 sink 实现里。新增下游协议（如 Anthropic Messages）时实现本
// 接口即可，不再复制一份流式循环。
//
// 并发契约：心跳 goroutine 会与主事件循环并发调用 Keepalive / Event，
// 实现必须自行串行化写底层 writer（OpenAI sink 依赖 SSEWriter 内置的锁）。
type StreamSink interface {
	// Event 消费一个上游事件并写出对应协议分片；协议的开场分片
	// （OpenAI 的 role chunk / Anthropic 的 message_start）由实现自行补齐。
	Event(ev *rosetta.Event)
	// Finish 在流终态时收尾。status 为 "ok" 时发协议的终止序列
	// （finish_reason 分片 + [DONE]，或 message_delta + message_stop）；
	// truncated/overflow/error 按各协议断流语义收尾（DESIGN §8.2，
	// OpenAI 不发 [DONE]、Anthropic 发 error 事件）；canceled 不写任何东西
	// —— 客户端已经走了。
	Finish(status string, stop rosetta.StopReason, usage rosetta.Usage, sendUsage bool)
	// Keepalive 写一条空闲心跳。用 SSE 注释实现：对所有标准客户端与
	// 中间代理都是无害字节。
	Keepalive()
	// WroteContent 报告是否写过任何内容增量（文本/思考/工具调用）。
	// 供「上游给了终止事件却零内容」的 WARN 留痕用（DESIGN §8.2）。
	WroteContent() bool
	// SawTerminal 报告是否收到过 EventMessageEnd。供空闲看门狗开火后的
	// 终态判定（idleTimedOut && !sawTerminal → truncated）。
	SawTerminal() bool
}

// openaiSink 把统一事件流编码为 OpenAI chat.completion.chunk 序列。
// 编码本体在 SSEWriter，这里只做事件分发与状态留痕。
type openaiSink struct {
	sse          *SSEWriter
	wroteContent bool
	sawTerminal  bool
}

// NewOpenAISink 用现有的 SSEWriter 组装 OpenAI 下游 sink。
func NewOpenAISink(sse *SSEWriter) StreamSink {
	return &openaiSink{sse: sse}
}

func (s *openaiSink) Event(ev *rosetta.Event) {
	switch ev.Type {
	case rosetta.EventMessageStart:
		s.sse.SetResponseID(ev.ID)
	case rosetta.EventTextDelta:
		if ev.Text != "" {
			s.wroteContent = true
		}
		s.sse.WriteTextDelta(ev.Text)
	case rosetta.EventThinkingDelta:
		// 思考增量必须透传：只吐 reasoning_content 的流若被丢掉，下游会收到
		// 一条「零内容 + finish_reason:stop + [DONE]」的假正常流。空 Text 是
		// Anthropic thinking signature 载体，OpenAI 下游无对应字段，跳过不算丢内容。
		if ev.Text != "" {
			s.wroteContent = true
			s.sse.WriteThinkingDelta(ev.Text)
		}
	case rosetta.EventToolCall:
		s.wroteContent = true
		s.sse.WriteToolCallDelta(ev.ToolIndex, ev.ToolID, ev.ToolName, ev.ArgumentsDelta)
	case rosetta.EventMessageEnd:
		s.sawTerminal = true
	}
}

func (s *openaiSink) Finish(status string, stop rosetta.StopReason, usage rosetta.Usage, sendUsage bool) {
	if status == "ok" {
		s.sse.WriteFinish(OpenAIFinishReason(stop))
	}
	if sendUsage && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
		s.sse.WriteUsage(usage)
	}
	if status == "ok" {
		s.sse.WriteDone()
	}
	// 非 ok（truncated/overflow/canceled/error）：不发 [DONE]，客户端据此
	// 判定流异常 —— 见 DESIGN §8.2，行为与 SSEWriter 单用时代完全一致。
}

func (s *openaiSink) Keepalive() { s.sse.WriteComment("keepalive") }
func (s *openaiSink) WroteContent() bool {
	return s.wroteContent
}
func (s *openaiSink) SawTerminal() bool {
	return s.sawTerminal
}
