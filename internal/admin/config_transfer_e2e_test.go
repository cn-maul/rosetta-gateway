package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// e2eSource 建一套有代表性的源配置：不同协议、不同启用状态、
// 带定价与上下文设置、含凭据。
func e2eSource(t *testing.T) (*ConfigTransferHandler, *store.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/src.db", logger)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := NewConfigTransferHandler(st, []byte("0123456789abcdef0123456789abcdef"), nil)

	ctx := context.Background()
	seed := []struct {
		p store.Provider
		m []store.UpstreamModel
	}{
		{
			p: store.Provider{ID: "p1", Slug: "alpha", Name: "Alpha 云", Protocol: "openai-chat",
				Endpoint: "https://alpha.example.com/v1", Enabled: true, TimeoutMs: 90000, MaxRetries: 3},
			m: []store.UpstreamModel{
				{ID: "m1", ProviderID: "p1", ModelID: "alpha-1", DisplayName: "Alpha 一号",
					Enabled: true, ContextWindow: 131072, MaxOutputTokens: 32768, PriceInput: 2, PriceOutput: 8},
				{ID: "m2", ProviderID: "p1", ModelID: "alpha-2", Enabled: false, ContextWindow: 65536},
			},
		},
		{
			p: store.Provider{ID: "p2", Slug: "beta", Name: "Beta", Protocol: "anthropic",
				Endpoint: "https://beta.example.com", Enabled: true},
			m: []store.UpstreamModel{
				{ID: "m3", ProviderID: "p2", ModelID: "beta-large", Enabled: true, ContextWindow: 200000},
			},
		},
	}
	for _, s := range seed {
		p := s.p
		if err := st.CreateProvider(ctx, &p); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
		for _, m := range s.m {
			m := m
			if err := st.CreateUpstreamModel(ctx, &m); err != nil {
				t.Fatalf("seed model: %v", err)
			}
		}
	}
	return h, st
}

func doExport(t *testing.T, h *ConfigTransferHandler, body string) configExport {
	t.Helper()
	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/export", strings.NewReader(body)))
	w := httptest.NewRecorder()
	h.Export(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("export %d: %s", w.Code, w.Body.String())
	}
	var out configExport
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("导出非 JSON: %v", err)
	}
	return out
}

func doImport(t *testing.T, h *ConfigTransferHandler, req importRequest) (*httptest.ResponseRecorder, importResponse) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/import", bytes.NewReader(raw)))
	w := httptest.NewRecorder()
	h.Import(w, r)
	var resp importResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestE2E_EncryptedExportToFreshInstance 是最贴近真实使用的一次验证：
// 从 A 实例加密导出 → 在 B 实例（空库）导入 → 逐字段核对落地结果。
func TestE2E_EncryptedExportToFreshInstance(t *testing.T) {
	src, _ := e2eSource(t)

	const pass = "deploy-secret-2026"
	file := doExport(t, src, `{"passphrase":"`+pass+`","include_credentials":true}`)
	if !file.Encrypted {
		t.Fatalf("应产出加密文件")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dstStore, err := store.Open(t.TempDir()+"/dst.db", logger)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dstStore.Close()
	dst := NewConfigTransferHandler(dstStore, []byte("fedcba9876543210fedcba9876543210"), nil)

	w, resp := doImport(t, dst, importRequest{Data: file, Passphrase: pass})
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", w.Code, w.Body.String())
	}
	if resp.ProvidersCreated != 2 || resp.ModelsCreated != 3 {
		t.Fatalf("数量不对: %+v", resp)
	}

	ctx := context.Background()
	provs, _ := dstStore.ListProviders(ctx)
	if len(provs) != 2 {
		t.Fatalf("落地供应商数 = %d", len(provs))
	}
	//逐个核对字段，而不是只数数量 —— 数量对但参数错是这类功能最常见的失败。
	bySlug := map[string]store.Provider{}
	for _, p := range provs {
		bySlug[p.Slug] = p
	}
	if a := bySlug["alpha"]; a.Endpoint != "https://alpha.example.com/v1" ||
		a.Protocol != "openai-chat" || a.TimeoutMs != 90000 || a.MaxRetries != 3 || !a.Enabled {
		t.Fatalf("alpha 字段没对上: %+v", a)
	}
	if b := bySlug["beta"]; b.Protocol != "anthropic" || b.Endpoint != "https://beta.example.com" {
		t.Fatalf("beta 字段没对上: %+v", b)
	}

	alphaID := bySlug["alpha"].ID
	models, _ := dstStore.ListUpstreamModels(ctx, alphaID)
	if len(models) != 2 {
		t.Fatalf("alpha 下模型数 = %d", len(models))
	}
	mm := map[string]store.UpstreamModel{}
	for _, m := range models {
		mm[m.ModelID] = m
	}
	if a1 := mm["alpha-1"]; a1.ContextWindow != 131072 || a1.MaxOutputTokens != 32768 ||
		a1.PriceInput != 2 || a1.PriceOutput != 8 || a1.DisplayName != "Alpha 一号" {
		t.Fatalf("alpha-1 的定价/容量没对上: %+v", a1)
	}
	if a2 := mm["alpha-2"]; a2.Enabled {
		t.Fatalf("alpha-2 应保持停用（导入了 enabled=false）")
	}
}

// TestE2E_MergeIntoExistingDeployment 覆盖另一个高频场景：目标库已有配置，
// 导入另一个实例的导出文件。
//
// 关键断言是「原有的一个字都没被改动」—— 合并语义的核心承诺。
func TestE2E_MergeIntoExistingDeployment(t *testing.T) {
	src, _ := e2eSource(t)
	file := doExport(t, src, "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dstStore, err := store.Open(t.TempDir()+"/dst.db", logger)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dstStore.Close()
	dst := NewConfigTransferHandler(dstStore, []byte("0123456789abcdef0123456789abcdef"), nil)

	ctx := context.Background()
	// 目标库已有一个 alpha，配置完全不同
	local := store.Provider{ID: "local", Slug: "alpha", Name: "本地自建",
		Protocol: "openai-responses", Endpoint: "http://192.168.1.50:8000/v1",
		Enabled: true, TimeoutMs: 30000, MaxRetries: 1}
	if err := dstStore.CreateProvider(ctx, &local); err != nil {
		t.Fatalf("seed: %v", err)
	}
	localModel := store.UpstreamModel{ID: "lm", ProviderID: "local", ModelID: "alpha-1",
		Enabled: true, PriceInput: 99}
	if err := dstStore.CreateUpstreamModel(ctx, &localModel); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, resp := doImport(t, dst, importRequest{Data: file})
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", w.Code, w.Body.String())
	}

	// alpha 重命名为 alpha-2
	if got := resp.ProvidersRenamed["alpha"]; got != "alpha-2" {
		t.Fatalf("alpha 应改名 alpha-2，实际 %q", got)
	}

	// **原有的必须原封不动**
	after, _ := dstStore.GetProvider(ctx, "local")
	if after.Endpoint != local.Endpoint || after.Protocol != local.Protocol ||
		after.TimeoutMs != local.TimeoutMs || after.MaxRetries != local.MaxRetries ||
		after.Name != local.Name {
		t.Fatalf("原有 alpha 被改动了:\n原 %+v\n现 %+v", local, *after)
	}
	afterM, _ := dstStore.GetUpstreamModel(ctx, "lm")
	if afterM.PriceInput != 99 {
		t.Fatalf("原有模型的定价被改了: %v", afterM.PriceInput)
	}

	// 新的那条落在 alpha-2 下，并带着它的模型
	provs, _ := dstStore.ListProviders(ctx)
	var newID string
	for _, p := range provs {
		if p.Slug == "alpha-2" {
			newID = p.ID
		}
	}
	if newID == "" {
		t.Fatalf("没有 alpha-2: %v", resp.Notes)
	}
	if p, _ := dstStore.GetProvider(ctx, newID); p.Endpoint != "https://alpha.example.com/v1" {
		t.Fatalf("alpha-2 的 endpoint 不对: %s", p.Endpoint)
	}
	newModels, _ := dstStore.ListUpstreamModels(ctx, newID)
	if len(newModels) != 2 {
		t.Fatalf("alpha-2 下应有 2 个模型，实际 %d（notes=%v）", len(newModels), resp.Notes)
	}
}

// TestE2E_DryRunThenApplyMatches 确认「先预演后执行」两次调用的结果一致。
//
// 预演的价值全在于"和真做一致" —— 不一致的预演比没有预演更危险，
// 因为用户会照着一个错的预览做决定。
func TestE2E_DryRunThenApplyMatches(t *testing.T) {
	src, _ := e2eSource(t)
	file := doExport(t, src, `{"include_credentials":true}`)

	newDst := func(t *testing.T) (*ConfigTransferHandler, *store.Store) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		st, err := store.Open(t.TempDir()+"/d.db", logger)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		if err := st.CreateProvider(ctx, &store.Provider{ID: "x", Slug: "alpha",
			Name: "本地", Protocol: "openai-chat", Endpoint: "http://local/v1", Enabled: true}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return NewConfigTransferHandler(st, []byte("0123456789abcdef0123456789abcdef"), nil), st
	}

	// 只预演
	d1, s1 := newDst(t)
	w1, dry := doImport(t, d1, importRequest{Data: file, DryRun: true})
	if w1.Code != http.StatusOK || !dry.DryRun {
		t.Fatalf("预演失败: %d %+v", w1.Code, dry)
	}

	// 真做
	d2, s2 := newDst(t)
	w2, real := doImport(t, d2, importRequest{Data: file})
	if w2.Code != http.StatusOK {
		t.Fatalf("实跑失败: %d", w2.Code)
	}

	if dry.ProvidersCreated != real.ProvidersCreated ||
		dry.ModelsCreated != real.ModelsCreated ||
		dry.CredentialsAdded != real.CredentialsAdded {
		t.Fatalf("预演与实跑不一致:\n预演 %+v\n实跑 %+v", dry, real)
	}
	if dry.ProvidersRenamed["alpha"] != real.ProvidersRenamed["alpha"] {
		t.Fatalf("改名预览不一致: %v vs %v", dry.ProvidersRenamed, real.ProvidersRenamed)
	}

	// 预演之后库必须还是原样
	p1, _ := s1.ListProviders(context.Background())
	if len(p1) != 1 {
		t.Fatalf("预演写了库：供应商数 = %d", len(p1))
	}
	p2, _ := s2.ListProviders(context.Background())
	if len(p2) != 3 {
		t.Fatalf("实跑后应为 3 个供应商，实际 %d", len(p2))
	}
}

// TestE2E_PlaintextExportNeverLeaksKeysWhenUnchecked 复核最容易出事的一条：
// 用户什么选项都没改就点导出，文件里不能有任何凭据。
func TestE2E_PlaintextExportNeverLeaksKeysWhenUnchecked(t *testing.T) {
	src, _ := e2eSource(t)

	r := asAdmin(httptest.NewRequest(http.MethodPost, "/admin/api/config-export/export", nil))
	w := httptest.NewRecorder()
	src.Export(w, r)
	body := w.Body.String()

	for _, needle := range []string{"api_key", "sk-", "APIKey"} {
		if strings.Contains(body, needle) {
			t.Fatalf("默认导出里出现了 %q —— 凭据不该被带出", needle)
		}
	}
}
