package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// reinitAllowance 在当前快照的基础上改写两个维度的模型白名单。
//
// 按快照的不可变契约复制一份再整体发布（与 ratelimit_test 改写 keys 同一手法）：
// 已发布的快照被并发请求无锁读取，原地改字段是数据竞争。
//
// groupAllow 传 nil = 不改组维度。
//
// **两个维度都要写**：鉴权读的是 KeySnapshot.GroupModelAllow —— 那份是
// rebuild 折算好的最终结果（P2 的 key 级组覆盖在重建时就并入了），
// 用户的 UserSnapshot.AllowedModels 只在重建阶段参与计算。
// 只改用户那一处会得到「组限制没生效」的假失败。
func reinitAllowance(t *testing.T, keyAllow snapshot.ModelAllow, groupAllow *snapshot.ModelAllow) {
	t.Helper()
	old := snapshot.Get()

	keys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for h, ks := range old.KeysByHash {
		cp := *ks
		cp.AllowedModels = keyAllow
		if groupAllow != nil {
			cp.GroupModelAllow = *groupAllow
		}
		keys[h] = &cp
	}
	users := make(map[string]*snapshot.UserSnapshot, len(old.UsersByID))
	for id, us := range old.UsersByID {
		cp := *us
		if groupAllow != nil {
			cp.AllowedModels = *groupAllow
		}
		users[id] = &cp
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes:     old.Routes,
		Providers:  old.Providers,
		KeysByHash: keys,
		UsersByID:  users,
		Runtime:    old.Runtime,
	})
}

// 白名单之外的模型必须被拒，且**不触碰上游**。
//
// harness 的上游是 fakeGood（恒 200），所以拿到 403 就证明请求在网关内
// 就被挡住了，而不是上游返回的。
func TestModelWhitelist_RejectsUnlistedModel(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"some-other-model"}), nil)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("body should carry the model_not_allowed code: %s", rec.Body.String())
	}
}

// 白名单内的模型照常放行。
func TestModelWhitelist_AllowsListedModel(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), nil)

	rec := postChat(h, "flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// 两个维度求交：key 允许但组不允许 → 拒绝。
func TestModelWhitelist_GroupRestrictionWins(t *testing.T) {
	h, _ := goodHarness(t)
	groupAllow := snapshot.AllowOnly([]string{"nope"})
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), &groupAllow)

	if rec := postChat(h, "flash"); rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (key 允许但组不允许)", rec.Code)
	}
}

// 两个维度都允许 → 放行。确认判定不是「任一受限即拒绝」。
func TestModelWhitelist_BothDimensionsAllow(t *testing.T) {
	h, _ := goodHarness(t)
	groupAllow := snapshot.AllowOnly([]string{"flash"})
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), &groupAllow)

	if rec := postChat(h, "flash"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (两个维度都允许)", rec.Code)
	}
}

// 没配白名单 → 全部可用。这是 P1 的向后兼容保证：升级后不配分组，
// 行为与改造前完全一致。
func TestModelWhitelist_UnrestrictedStillAllowsEverything(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowAll(), nil)

	if rec := postChat(h, "flash"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

// `provider/model` 直连形式必须**同样**受白名单约束，否则白名单形同虚设。
//
// # 这条为什么最关键
//
// 直连形式绕过具名路由，直接按「provider slug + 上游 model_id」打到上游。
// 如果白名单只对具名路由生效，受限用户只要把请求改成 `good/good-model`
// 就能拿到白名单之外的上游模型 —— 一次改写就绕过整条权限链。
//
// 实现上这是自然成立的：routing.Resolve 对直连形式会合成一条 route，
// 其 PublicName 恰为请求原文，所以按 PublicName 查白名单 = 按客户端
// 实际写的标识符查。前提是**不要**在别处放宽成「具名或直连任一通过」。
func TestModelWhitelist_SlugFormIsAlsoGated(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), nil)

	rec := postChat(h, "good/good-model")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("直连形式绕过了白名单：code = %d body = %s", rec.Code, rec.Body.String())
	}
}

// 反过来：管理员把直连标识符本身写进白名单时，该形式必须可用
// （它是命名空间里的一个普通字符串，不是特殊豁免）。
func TestModelWhitelist_SlugFormAllowedWhenListed(t *testing.T) {
	h, _ := goodHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"good/good-model"}), nil)

	rec := postChat(h, "good/good-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("显式列出的直连标识符应可用：code = %d body = %s", rec.Code, rec.Body.String())
	}
	// 具名路由此时应当**不可**用 —— 白名单是精确集合，不是前缀。
	if rec2 := postChat(h, "flash"); rec2.Code != http.StatusForbidden {
		t.Errorf("flash 未在白名单中，应被拒；code = %d", rec2.Code)
	}
}

// /v1/models 必须与转发路径用同一套判定：列表里只出现白名单内的名字。
func TestListModels_FiltersByWhitelist(t *testing.T) {
	initHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), nil)

	list := fetchModelIDs(t, "/v1/models")
	if len(list) != 1 || list[0] != "flash" {
		t.Fatalf("ids = %v, want [flash]", list)
	}
}

// 白名单里没有一个能匹配 → 空列表，而不是退回「列出全部」。
func TestListModels_EmptyWhenNothingAllowed(t *testing.T) {
	initHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"not-a-route"}), nil)

	if ids := fetchModelIDs(t, "/v1/models"); len(ids) != 0 {
		t.Fatalf("ids = %v, want empty (受限时不能退回全量)", ids)
	}
}

// include=upstream 的直连轨道同样要收窄：不在白名单里的标识符不得出现。
//
// 与转发路径同源 —— 列表说「你不能用」，转发就必须拒；反过来若列表不显示
// 却能调，那只是提示不准，不构成越权。这里的断言锁的是前者（列表不泄露
// 白名单之外的名字），那是真正有安全意义的方向。
func TestListModels_UpstreamTrackRespectsWhitelist(t *testing.T) {
	initHarness(t)
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash"}), nil)

	list := fetchModelIDs(t, "/v1/models?include=upstream")
	if len(list) != 1 || list[0] != "flash" {
		t.Fatalf("ids = %v, want [flash] — 直连标识符不在白名单里就不该出现", list)
	}

	// 显式把直连标识符写进白名单 → 两条轨道都出现。
	reinitAllowance(t, snapshot.AllowOnly([]string{"flash", "good/good-model"}), nil)
	both := fetchModelIDs(t, "/v1/models?include=upstream")
	if len(both) != 2 {
		t.Fatalf("ids = %v, want both (flash + good/good-model)", both)
	}
}

// 未配置白名单时，include=upstream 仍要列出 slug（回归：别把整条轨道砍掉）。
func TestListModels_UpstreamTrackIntactWithoutWhitelist(t *testing.T) {
	initHarness(t)
	reinitAllowance(t, snapshot.AllowAll(), nil)

	list := fetchModelIDs(t, "/v1/models?include=upstream")
	if len(list) != 2 {
		t.Fatalf("ids = %v, want both tracks (flash + good/good-model)", list)
	}
}

// initHarness 只为副作用装好「库 + 快照 + 测试 key」。
//
// 模型列表端点（handleListModels）是独立 handler，不需要 harness 返回的
// 转发 handler，所以这里把返回值丢掉，而不是留一个「声明未使用」的变量。
func initHarness(t *testing.T) {
	t.Helper()
	goodHarness(t)
}

// fetchModelIDs 请求模型列表并取出 data[].id。
func fetchModelIDs(t *testing.T, path string) []string {
	t.Helper()
	h := handleListModels("")
	rec := httptest.NewRecorder()
	h(rec, authedGet(path))
	if rec.Code != http.StatusOK {
		t.Fatalf("list models: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v body=%s", err, rec.Body.String())
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids
}
