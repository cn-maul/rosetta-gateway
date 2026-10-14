package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// initProbeScopedSnapshot 装一份「每个公开名各有自己上游模型行」的快照。
//
// 为什么不能用 initSnapshotWithUsers：那份 fixtures 让所有公开名共用**同一个**
// 上游模型行（good/m），于是无论白名单收窄成什么样，「组外模型」的 id 都
// 与「组内模型」的 id 相同 —— 收窄断言会永远通过，等于没测。这里刻意让
// 每个公开名指向不同的行（up/<name>），组外 id 才是真的够不着。
func initProbeScopedSnapshot(t *testing.T, users map[string]*snapshot.UserSnapshot) {
	t.Helper()
	ri := routing.NewRouteIndex()
	ri.AddProvider(&routing.ProviderRef{ID: "p1", Slug: "good", Protocol: "openai-chat", Enabled: true})
	for _, n := range []string{"flash", "pro", "opus"} {
		ri.AddUpstreamModel(&routing.UpstreamModel{
			ID: "up/" + n, ProviderID: "p1", ModelID: n, Enabled: true,
		})
		ri.AddRoute(&routing.Route{
			ID: "r-" + n, PublicName: n,
			ProviderID: "p1", UpstreamModelID: "up/" + n, Enabled: true,
		})
	}
	if users == nil {
		users = map[string]*snapshot.UserSnapshot{}
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes: ri, Providers: map[string]*snapshot.ProviderSnapshot{},
		KeysByHash: map[string]*snapshot.KeySnapshot{},
		UsersByID:  users,
	})
	t.Cleanup(func() { snapshot.Init(&snapshot.Snapshot{}) })
}

// seedProbe 给模型落一条探测记录（成功或失败）。
//
// 必须**同时**把模型行插进 upstream_models：ProbeHealthSummary 是
// LEFT JOIN 到该表的，只写 model_probes 的话摘要查出来是空 —— 那不是
// handler 收窄的问题，是 fixtures 缺行。两者都写，才能测到「收窄本身」。
func seedProbe(t *testing.T, st *store.Store, modelID string, ok bool) {
	t.Helper()
	ctx := context.Background()
	// provider 行也必须先在：upstream_models.provider_id 是外键，
	// 只插模型会撞 FOREIGN KEY constraint failed。幂等 —— 一个测试里
	// 往往要给多个模型写记录。
	if existing, err := st.GetProvider(ctx, "p1"); err == nil && existing == nil {
		if err := st.CreateProvider(ctx, &store.Provider{
			ID: "p1", Slug: "good", Name: "good", Protocol: "openai-chat",
			Endpoint: "https://example.invalid/v1", Enabled: true,
		}); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	// 只在缺行时插入：多个测试会给同一模型写多条探测记录。
	if existing, err := st.GetUpstreamModel(ctx, modelID); err == nil && existing != nil {
		// 已有行，跳过插入。
	} else {
		err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
			ID: modelID, ProviderID: "p1", ModelID: modelID, Enabled: true,
		})
		if err != nil {
			t.Fatalf("seed upstream model %s: %v", modelID, err)
		}
	}
	err := st.RecordProbe(ctx, store.ProbeRecord{
		ModelID: modelID, ProviderID: "p1", Ts: time.Now().UnixMilli(),
		OK: ok, InBand: ok, LatencyMs: 12,
	})
	if err != nil {
		t.Fatalf("seed probe %s: %v", modelID, err)
	}
}

func fetchProbeHealths(t *testing.T, h *ProbeHandler, r *http.Request) []modelHealthResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Models(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe models: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out []modelHealthResponse
	decodeBody(t, rec, &out)
	return out
}

func fetchTimeline(t *testing.T, h *ProbeHandler, r *http.Request) (int, []store.ProbeTimelineBucket) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Timeline(rec, r)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var out []store.ProbeTimelineBucket
	decodeBody(t, rec, &out)
	return rec.Code, out
}

// probeUser 在受限组里，只够 flash + pro。
func probeUser(id string) *snapshot.UserSnapshot {
	return snapUser(id, "g1", snapshot.AllowOnly([]string{"flash", "pro"}))
}

// 普通用户只该看到组内模型的探测摘要。
//
// 关键：响应里**绝不能**出现 up/opus 的 id。它不在 model-names 里，
// 而 model-names 普通用户可读 —— 所以「opus 这个上游模型存在且探测失败」
// 本身就是一条不该外泄的信息。
func TestProbeModels_ScopedToGroupAllowlist(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, map[string]*snapshot.UserSnapshot{"u1": probeUser("u1")})
	seedProbe(t, st, "up/flash", true)
	seedProbe(t, st, "up/pro", false)
	seedProbe(t, st, "up/opus", false)
	h := NewProbeHandler(st)

	got := fetchProbeHealths(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/probes/models", nil), "u1"))
	seen := map[string]bool{}
	for _, g := range got {
		seen[g.ModelID] = true
		if g.ModelID == "up/opus" {
			t.Fatalf("收窄用户拿到了组外模型的探测数据（model_id=%s）", g.ModelID)
		}
	}
	if !seen["up/flash"] || !seen["up/pro"] {
		t.Errorf("组内模型缺失：%v", seen)
	}
}

// 管理员不受收窄 —— 否则管理员看不到已经停用/已归其它组的模型故障。
//
// 期望 3 条：三个模型行都插进库（即使只有一个有探测记录）。摘要查询
// LEFT JOIN upstream_models，未探测的模型**也**会列出来（health=unknown）——
// 总览页与卡片都要能显示「还没探测过」而不是让它凭空消失。若将来这条
// 收窄被误改成「只列有探测记录的」，这里会掉到 1。
func TestProbeModels_AdminSeesEverything(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, nil)
	seedProbe(t, st, "up/flash", true)
	seedProbe(t, st, "up/pro", true)
	seedProbe(t, st, "up/opus", false)
	h := NewProbeHandler(st)

	got := fetchProbeHealths(t, h, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/probes/models", nil)))
	if len(got) != 3 {
		t.Errorf("admin got %d models, want 3（未被收窄）", len(got))
	}
}

// 白名单与路由无交集 → 必须返回**空集**，不能退回全集。
//
// 这是最容易写反的一处：返回全集在权限上更糟（把权限收紧呈现成放开），
// 而且没有任何界面提示能让人发现。
func TestProbeModels_EmptyIntersectionReturnsEmptyNotAll(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, map[string]*snapshot.UserSnapshot{
		"u3": snapUser("u3", "g1", snapshot.AllowOnly([]string{"nonexistent"})),
	})
	seedProbe(t, st, "up/flash", true)
	h := NewProbeHandler(st)

	got := fetchProbeHealths(t, h, asUser(httptest.NewRequest(http.MethodGet, "/admin/api/probes/models", nil), "u3"))
	if len(got) != 0 {
		t.Errorf("交集为空却返回了 %d 条（must be 0，不能退回全集）：%+v", len(got), got)
	}
}

// 未登录一律 401，且**不**当成 admin 放行全集。
func TestProbeModels_RequiresLogin(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, nil)
	seedProbe(t, st, "up/flash", true)
	h := NewProbeHandler(st)

	rec := httptest.NewRecorder()
	h.Models(rec, httptest.NewRequest(http.MethodGet, "/admin/api/probes/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无身份请求 code = %d, want 401（fail-closed：空身份不得当 admin）", rec.Code)
	}
}

// 时间轴是摘要的旁路，必须**同样**收窄 —— 否则普通用户可以对任意
// upstream_model_id 查曲线，用「有没有桶」来枚举组外模型的存在与健康。
func TestProbeTimeline_ScopedAndDeniedIDsAreNotSilentlyDropped(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, map[string]*snapshot.UserSnapshot{"u1": probeUser("u1")})
	seedProbe(t, st, "up/flash", true)
	seedProbe(t, st, "up/opus", false)
	h := NewProbeHandler(st)

	// 全部越权 → 403（说清是权限问题，而不是回一个空数组让人以为「没数据」）。
	code, _ := fetchTimeline(t, h, asUser(
		httptest.NewRequest(http.MethodGet, "/admin/api/probes/timeline?model_id=up%2Fopus", nil), "u1"))
	if code != http.StatusForbidden {
		t.Errorf("全越权请求 code = %d, want 403", code)
	}

	// 混着合法 id → 剔除越权的那条，合法的那条照常返回。
	// 整条拒绝会让一整批卡片都查不出来，被误读成「这些模型都挂了」。
	code, buckets := fetchTimeline(t, h, asUser(
		httptest.NewRequest(http.MethodGet, "/admin/api/probes/timeline?model_id=up%2Fflash%2Cup%2Fopus", nil), "u1"))
	if code != http.StatusOK {
		t.Fatalf("混合请求 code = %d, want 200（应剔除越权项而非整体拒绝）", code)
	}
	if len(buckets) == 0 {
		t.Fatal("合法模型的桶被一并丢掉了")
	}
	for _, b := range buckets {
		if b.ModelID != "up/flash" {
			t.Errorf("时间轴泄露了越权模型的桶：%s", b.ModelID)
		}
	}

	// 一个都没传 → 400（参数问题），与 403（权限问题）必须可区分。
	code, _ = fetchTimeline(t, h, asUser(
		httptest.NewRequest(http.MethodGet, "/admin/api/probes/timeline", nil), "u1"))
	if code != http.StatusBadRequest {
		t.Errorf("缺 model_id code = %d, want 400", code)
	}
}

// 管理员的时间轴不被收窄。
func TestProbeTimeline_AdminUnrestricted(t *testing.T) {
	st := newScopeStore(t)
	initProbeScopedSnapshot(t, nil)
	seedProbe(t, st, "up/opus", false)
	h := NewProbeHandler(st)

	code, buckets := fetchTimeline(t, h, asAdmin(
		httptest.NewRequest(http.MethodGet, "/admin/api/probes/timeline?model_id=up%2Fopus", nil)))
	if code != http.StatusOK {
		t.Fatalf("admin code = %d, want 200", code)
	}
	if len(buckets) == 0 {
		t.Error("管理员应能查到 opus 的桶")
	}
}
