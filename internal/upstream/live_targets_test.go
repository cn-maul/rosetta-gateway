package upstream

// A5-P1-2（2026-10-21 修）：读库失败时**不能**把熔断状态清空。
//
// 旧接口只传一个 map[string]bool，Install 以「集合外一律丢弃」为语义，
// 于是「读失败」与「目标确实没了」被压成同一件事：一次短暂的读池抖动
// （SQLITE_BUSY / 磁盘瞬时故障）就把**全站**熔断状态清空，正在熔断的
// 坏上游立刻复活 —— 而这一轮重复打满的坏上游，恰好是故障期内的真实压力。
//
// 现在 LiveTargets 带 Known 标志，三态分明：
//   - Known=true  且集合非空 → 按集合过滤（正常路径）；
//   - Known=true  且集合为空 → 目标确实没了，丢弃旧状态正确；
//   - Known=false             → 集合未知，**保留全部**既有熔断状态。

import "testing"

// 读失败（Known=false）必须保留既有熔断状态。
func TestInstall_读失败时保留熔断状态(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})

	// 前置条件：目标已熔断。
	p.RecordTargetFailure(tid, 0, 1)
	if p.TargetAvailable(tid) {
		t.Fatalf("前置条件不成立：目标未熔断")
	}

	// 模拟一次「读库失败」的重建。
	p.Install(map[string]*ProviderEntry{}, LiveTargets{Known: false})

	if p.TargetAvailable(tid) {
		t.Errorf("读失败时重建清空了熔断状态：正在熔断的坏上游立刻复活")
	}
}

// 对照组：读到了、集合确实为空 → 旧状态**应当**被丢弃。
//
// 没有这条，上面那条的修复可能退化成「Install 什么都不做」——
// 那会让已删除的目标的熔断条目在内存里单调堆积。
func TestInstall_目标确实消失时丢弃熔断状态(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})
	p.RecordTargetFailure(tid, 0, 1)

	// 读到了，且集合为空 = 目标确实从库里消失了。
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{}, Known: true})

	if !p.TargetAvailable(tid) {
		t.Errorf("已删除的目标仍留有熔断状态（内存条目单调堆积）")
	}
}

// 读失败后，**仍在库里的**目标也不该丢掉熔断状态；
// 而 Known=false 时集合内容本身无意义（不能按它过滤）。
func TestInstall_读失败时不做集合过滤(t *testing.T) {
	p := newTestPool()
	const tid = "t#0"
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{tid: true}, Known: true})
	p.RecordTargetFailure(tid, 0, 1)

	// 即使调用方顺手带了一个集合，Known=false 也不能按它过滤。
	p.Install(map[string]*ProviderEntry{}, LiveTargets{IDs: map[string]bool{}, Known: false})

	if p.TargetAvailable(tid) {
		t.Errorf("Known=false 仍按集合过滤了：熔断状态被清空")
	}
}
