package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
)

// 停止序列必须真的到达 wire。
//
// 这条路径在 rosetta v1.3.0 里静默失效了整整一个 minor 版本：适配器把
// stop_sequences 当成「2026-10-21 已移除」丢弃，不发、不报错、只写一行
// debug 日志。上游于是按默认行为生成到 max_tokens，stop_reason 从
// stop_sequence 变成 end_turn —— 客户端拿不到任何异常信号。当时没有任何
// 测试盯着「字段是否到达上游」，所以直到审计才被发现。
//
// v1.3.2 恢复渲染该字段。下面两条用例把「入口建模 → 统一请求 → 协议适配
// → 出站 JSON」整条链路锁住：一条同协议，一条跨协议。
func TestWire_AnthropicStopSequencesReachUpstream(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],`+
			`"stop_sequences":["\n\nHuman:","END"]}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.Anthropic, "claude-sonnet-5-5"))

	seqs, ok := got["stop_sequences"].([]any)
	if !ok {
		t.Fatalf("anthropic 上游必须收到 stop_sequences，实际 payload=%v", got)
	}
	if len(seqs) != 2 || seqs[0] != "\n\nHuman:" || seqs[1] != "END" {
		t.Fatalf("stop_sequences 内容被改写：%v", seqs)
	}
}

// OpenAI 入口的 stop 是同一语义的另一个字段名（且允许裸字符串），
// 打到 Anthropic 上游时必须落成 stop_sequences。跨协议翻译错了同样是静默失效。
func TestWire_ChatStopReachesAnthropicUpstream(t *testing.T) {
	got := probeWireModel(t, NewOpenAIChat(),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.Anthropic, "claude-sonnet-5-5"))

	seqs, ok := got["stop_sequences"].([]any)
	if !ok || len(seqs) != 1 || seqs[0] != "END" {
		t.Fatalf("chat 入口的 stop 应翻成 stop_sequences，实际 payload=%v", got)
	}
	// 双重来源会互相覆盖：SDK 渲染 + Extra 里再塞一份，谁赢取决于写入顺序。
	// 断言数组长度就是在防这个 —— 网关侧不许往 Extra 塞 stop_sequences。
	if len(seqs) != 1 {
		t.Fatalf("stop_sequences 出现了重复来源：%v", seqs)
	}
}

// service_tier 目前故意不建模，这条用例是护栏不是功能测试。
//
// SDK 1.3.2 给 Anthropic 侧的 service_tier 加了值域白名单
// （auto / standard_only），而统一字段的词汇是 OpenAI 那一套
// （default / flex / priority）。所以「顺手透传」会把 priority 变成
// 一次本地 ErrInvalidRequest —— 一个必然的 400，且发生在网关进程内。
//
// 谁要放开透传，这条会先红，逼他先把词表映射写清楚。
func TestWire_AnthropicServiceTierNotPassedThrough(t *testing.T) {
	got := probeWireModel(t, NewAnthropicMessages(),
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],`+
			`"service_tier":"priority"}`,
		rosetta.ProtoAnthropic, "/anthropic", modelWith(dialect.Anthropic, "claude-sonnet-5-5"))

	if v, ok := got["service_tier"]; ok {
		t.Fatalf("service_tier 仍应不建模（priority 会触发本地 400），实际收到 %v", v)
	}
}
