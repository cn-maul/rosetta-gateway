package routing

import (
	"testing"
)

// TestResolve_DisabledRouteIsNotBypassedByProviderModelFallback 坐实一条
// 配置与实际行为不一致的缺陷。
//
// Resolve 的第一个分支是 `if r, ok := ri.byPublicName[model]; ok && r.Enabled`：
// 命中一条**被禁用**的具名路由时，条件因 !r.Enabled 短路，控制流直接落到
// 第二个分支（provider/model 直连兜底）—— 而那一支只看 p.Enabled 和
// m.Enabled，**完全不再看「这个 model 字符串是否命中了一条被显式禁用的路由」**。
//
// 触发路径完全现实：route_handler 对 public_name 只校验非空、不限制是否含
// "/"，所以运维完全可以建一条 PublicName 为 "acme/gpt-4" 的路由（这在
// 「按客户分模型名」的部署里很常见），随后把它禁掉。此时客户端请求
// model:"acme/gpt-4"：
//
//	byPublicName["acme/gpt-4"] 命中但 Enabled=false → 跳过
//	strings.Index("acme/gpt-4", "/") = 4 > 0       → 进入直连分支
//	slug="acme" 命中某个启用 provider，其下有 model_id="gpt-4" 的启用模型
//	→ 返回合成 Resolution，请求成功打到上游
//
// 结果：运维的显式禁用**没有生效**，流量照旧通过。这与 buildCandidates 里
// 「绝不复活死目标」的设计原则直接冲突 —— 一个被禁用的模型名仍然能被打到。
func TestResolve_DisabledRouteIsNotBypassedByProviderModelFallback(t *testing.T) {
	ri := NewRouteIndex()
	ri.AddProvider(&ProviderRef{ID: "p-acme", Slug: "acme", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "m-gpt4", ProviderID: "p-acme", ModelID: "gpt-4", Enabled: true})

	// 运维建了一条公开名含 "/" 的路由，然后禁用了它。
	ri.AddRoute(&Route{
		ID:              "r1",
		PublicName:      "acme/gpt-4",
		ProviderID:      "p-acme",
		UpstreamModelID: "m-gpt4",
		Enabled:         false,
	})

	if _, err := ri.Resolve("acme/gpt-4"); err == nil {
		t.Fatal("被显式禁用的路由仍能通过 provider/model 兜底命中 —— " +
			"运维的禁用没有生效，流量照旧打到上游")
	}
}

// TestResolve_NamesWithoutSlashStillUseDirectConnect 是对照组：确认修复没有
// 打断 provider/model 直连这个正常能力（名字里不含 / 的普通禁用路由，
// 本来就走不到直连分支）。
func TestResolve_NamesWithoutSlashStillUseDirectConnect(t *testing.T) {
	ri := NewRouteIndex()
	ri.AddProvider(&ProviderRef{ID: "p-acme", Slug: "acme", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "m-gpt4", ProviderID: "p-acme", ModelID: "gpt-4", Enabled: true})

	res, err := ri.Resolve("acme/gpt-4")
	if err != nil {
		t.Fatalf("未配置同名路由时，provider/model 直连必须可用: %v", err)
	}
	if res.Route.UpstreamModelID != "m-gpt4" {
		t.Fatalf("直连解析到了错误的上游模型 %q", res.Route.UpstreamModelID)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].UpstreamModel.ModelID != "gpt-4" {
		t.Fatalf("候选链不正确: %+v", res.Candidates)
	}
}

// TestResolve_EnabledRouteWithSlashWinsOverDirectConnect 确认优先级方向没搞反：
// 启用的具名路由必须优先于直连兜底。
func TestResolve_EnabledRouteWithSlashWinsOverDirectConnect(t *testing.T) {
	ri := NewRouteIndex()
	ri.AddProvider(&ProviderRef{ID: "p-acme", Slug: "acme", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "m-gpt4", ProviderID: "p-acme", ModelID: "gpt-4", Enabled: true})
	// 另造一个「直连兜底」会命中的模型，验证具名路由的优先。
	ri.AddUpstreamModel(&UpstreamModel{ID: "m-direct", ProviderID: "p-acme", ModelID: "gpt-4", Enabled: true})

	ri.AddRoute(&Route{
		ID:              "r1",
		PublicName:      "acme/gpt-4",
		ProviderID:      "p-acme",
		UpstreamModelID: "m-gpt4",
		Enabled:         true,
	})

	res, err := ri.Resolve("acme/gpt-4")
	if err != nil {
		t.Fatalf("启用的具名路由必须可解析: %v", err)
	}
	if res.Route.ID != "r1" {
		t.Fatalf("启用的具名路由应优先，实际用的是 route=%q（疑似走了直连兜底）", res.Route.ID)
	}
}
