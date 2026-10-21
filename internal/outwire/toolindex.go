package outwire

// 工具编号空间（2026-10-21 修 A3-P1-1 / 汇总 O1）。
//
// # 问题的根
//
// rosetta 的统一事件流只有一个 ToolIndex 字段，而**三个协议的编号语义
// 完全不同**：
//
//   - OpenAI Chat：`tool_calls[].index`，「第几个工具调用」，从 0 连续递增；
//   - Anthropic：**整条消息的 content block 序号**（文本块是 0、
//     第一个 tool_use 是 1、第二个是 2……）；
//   - OpenAI Responses：output item 的位置（与 message / reasoning 共享
//     同一个序号空间）。
//
// SDK 侧不做转换（rosetta@v1.2.0 的 provider_anthropic.go 直接把
// `ch.Index` 写进 `Event.ToolIndex`），于是网关出站层拿到的 ToolIndex
// **是上游那一套编号**。
//
// 后果（实测复现见 work/audit-2026-10-20/A3-outwire.md P1-1）：
// Anthropic 上游 → OpenAI Chat 下游时，第一个工具调用下发的是
// `index: 1`（因为 Anthropic 的文本块占了 0）。OpenAI 客户端的常规合并
// 逻辑是「按 index 归并 tool_call」，于是它的 tool_call 数组第 0 位
// 空洞、`finish_reason:"tool_calls"` 与实际调用数对不上。并行调用时
// 更糟：Anthropic 给 1、3、5，下游看到三个分散的 index。
//
// # 修法
//
// 出站层维护一个「上游编号 → 下游编号」的映射表，按**首次出现顺序**
// 重新从 0 编号。这样：
//
//   - 下游永远看到 0,1,2…… 的连续 tool index，符合 OpenAI 的合并预期；
//   - 同一上游编号的多片参数增量仍然落到同一个下游编号（映射是稳定的）；
//   - 不同上游编号各自独立，不会被合并（也不会被误切）。
//
// 为什么按「首次出现顺序」而不是「上游编号直接用」：Anthropic 的编号
// 本身就是 content block 序号，与「工具调用次序」只是**顺序一致**而非
// **数值相等**（前面有文本块就整体偏移）。按出现顺序重编号恰好抹掉这个
// 偏移，又保留了并行调用的相对次序。
//
// # 为什么放在出站层而不是 inwire
//
// 入站是「客户端协议 → 统一模型」，出站是「统一模型 → 客户端协议」。
// 编号空间的转换是**面向下游协议**的：OpenAI Chat 要的是 tool index，
// Anthropic 下游要的是自己的 content block 序号（由出站层自己分配），
// Responses 要的是 output index（也由出站层自己分配）。所以转换只该在
// 出站侧发生一次，入站侧保持原样。

// toolIndexMapper 把上游的 ToolIndex 重编号成下游的「第几个工具调用」。
//
// 零值不可用，必须用 newToolIndexMapper 构造。
type toolIndexMapper struct {
	// byUpstream 是「上游编号 → 下游编号」。一个下游编号只对应一个上游
	// 编号（函数是单射），反向不查 —— 下游编号是自己发的，不需要反查。
	byUpstream map[int]int
	// next 是下一个可用的下游编号（自增分配）。
	next int
}

func newToolIndexMapper() *toolIndexMapper {
	return &toolIndexMapper{byUpstream: make(map[int]int)}
}

// map 把上游编号翻成下游编号，首次出现的上游编号会分到下一个连续编号。
//
// 幂等：同一个上游编号反复调用永远得到同一个下游编号。这条性质是
// 参数分片能正确归并的前提 —— rosetta 的工具调用参数会分多片送达，
// 每片都带同一个（上游）ToolIndex。
func (m *toolIndexMapper) to(upstream int) int {
	if idx, ok := m.byUpstream[upstream]; ok {
		return idx
	}
	idx := m.next
	m.next++
	m.byUpstream[upstream] = idx
	return idx
}
