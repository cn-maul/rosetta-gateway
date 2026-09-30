package routing

import "testing"

// newFixture 造一条含两个 provider、各一个上游模型的 route。
func newFixture() *RouteIndex {
	ri := NewRouteIndex()
	ri.AddProvider(&ProviderRef{ID: "pA", Slug: "alpha", Protocol: "openai-chat", Enabled: true})
	ri.AddProvider(&ProviderRef{ID: "pB", Slug: "beta", Protocol: "openai-chat", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "mA", ProviderID: "pA", ModelID: "alpha-chat", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "mB", ProviderID: "pB", ModelID: "beta-chat", Enabled: true})
	ri.AddRoute(&Route{ID: "r1", PublicName: "flash", ProviderID: "pA", UpstreamModelID: "mA", Enabled: true, FailoverEnabled: true})
	return ri
}

// 从未配置 route_targets 的老 route：应回落为「主目标」单元素链（零回归）。
func TestResolve_FallbackToPrimaryTarget(t *testing.T) {
	ri := newFixture()
	res, err := ri.Resolve("flash")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(res.Candidates))
	}
	c := res.Candidates[0]
	if c.Provider.ID != "pA" || c.UpstreamModel.ID != "mA" {
		t.Fatalf("primary target mismatch: %+v", c)
	}
	if c.TargetID != "r1#synth" {
		t.Fatalf("synth target id = %q, want r1#synth", c.TargetID)
	}
}

// 配了有序目标链：按 position 升序返回。
func TestResolve_OrderedChain(t *testing.T) {
	ri := newFixture()
	ri.AddRouteTarget(&Target{ID: "t0", RouteID: "r1", ProviderID: "pA", UpstreamModelID: "mA", Position: 0, Enabled: true})
	ri.AddRouteTarget(&Target{ID: "t1", RouteID: "r1", ProviderID: "pB", UpstreamModelID: "mB", Position: 1, Enabled: true})

	res, err := ri.Resolve("flash")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(res.Candidates))
	}
	if res.Candidates[0].Provider.ID != "pA" || res.Candidates[1].Provider.ID != "pB" {
		t.Fatalf("wrong order: %s then %s", res.Candidates[0].Provider.ID, res.Candidates[1].Provider.ID)
	}
}

// 链上 disabled 的 target、以及指向被禁 provider 的 target 都要被过滤掉。
func TestResolve_FiltersDisabled(t *testing.T) {
	ri := newFixture()
	ri.AddRouteTarget(&Target{ID: "t0", RouteID: "r1", ProviderID: "pA", UpstreamModelID: "mA", Position: 0, Enabled: false})
	ri.AddRouteTarget(&Target{ID: "t1", RouteID: "r1", ProviderID: "pB", UpstreamModelID: "mB", Position: 1, Enabled: true})

	res, _ := ri.Resolve("flash")
	if len(res.Candidates) != 1 || res.Candidates[0].Provider.ID != "pB" {
		t.Fatalf("expected only enabled beta target, got %+v", res.Candidates)
	}

	// 禁用整个 provider 后，引用它的目标也该被剔除。
	ri2 := newFixture()
	ri2.AddRouteTarget(&Target{ID: "t0", RouteID: "r1", ProviderID: "pA", UpstreamModelID: "mA", Position: 0, Enabled: true})
	ri2.AddRouteTarget(&Target{ID: "t1", RouteID: "r1", ProviderID: "pB", UpstreamModelID: "mB", Position: 1, Enabled: true})
	ri2.AddProvider(&ProviderRef{ID: "pA", Slug: "alpha", Protocol: "openai-chat", Enabled: false})
	res2, _ := ri2.Resolve("flash")
	if len(res2.Candidates) != 1 || res2.Candidates[0].Provider.ID != "pB" {
		t.Fatalf("disabled provider target not filtered: %+v", res2.Candidates)
	}
}

// 全链皆不可用（provider 都禁用）→ 模型不存在。
func TestResolve_NoAvailableTarget(t *testing.T) {
	ri := newFixture()
	ri.AddRouteTarget(&Target{ID: "t0", RouteID: "r1", ProviderID: "pA", UpstreamModelID: "mA", Position: 0, Enabled: false})
	if _, err := ri.Resolve("flash"); err != ErrModelNotFound {
		t.Fatalf("want ErrModelNotFound, got %v", err)
	}
}

// provider/model 直连形式仍工作，返回合成单目标。
func TestResolve_ProviderSlashModel(t *testing.T) {
	ri := newFixture()
	res, err := ri.Resolve("beta/beta-chat")
	if err != nil {
		t.Fatalf("resolve direct: %v", err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].UpstreamModel.ModelID != "beta-chat" {
		t.Fatalf("unexpected: %+v", res.Candidates)
	}
}
