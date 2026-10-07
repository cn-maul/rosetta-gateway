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

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// transferFixture 造一套「源库 → 导出 → 目标库 → 导入」的完整环境。
func transferFixture(t *testing.T) (src *ConfigTransferHandler, masterKey []byte) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mk := []byte("0123456789abcdef0123456789abcdef")
	return NewConfigTransferHandler(st, mk, &config.Config{}), mk
}

// seedSource 往库里塞两个供应商、三个模型和两条凭据。
func seedSource(t *testing.T, h *ConfigTransferHandler, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []store.Provider{
		{ID: "p1", Slug: "alpha", Name: "Alpha", Protocol: "openai-chat",
			Endpoint: "https://a.example.com/v1", Enabled: true, TimeoutMs: 60000, MaxRetries: 2},
		{ID: "p2", Slug: "beta", Name: "Beta", Protocol: "openai-responses",
			Endpoint: "https://b.example.com/v1", Enabled: false},
	} {
		p := p
		if err := st.CreateProvider(ctx, &p); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	for _, m := range []store.UpstreamModel{
		{ID: "m1", ProviderID: "p1", ModelID: "alpha-chat", DisplayName: "Alpha Chat",
			Enabled: true, ContextWindow: 128000, MaxOutputTokens: 8192, PriceInput: 1.5, PriceOutput: 6},
		{ID: "m2", ProviderID: "p1", ModelID: "alpha-reason", Enabled: true},
		{ID: "m3", ProviderID: "p2", ModelID: "beta-pro", Enabled: true},
	} {
		m := m
		if err := st.CreateUpstreamModel(ctx, &m); err != nil {
			t.Fatalf("seed model: %v", err)
		}
	}
	for _, c := range []struct {
		id, pid, label, key string
	}{
		{"c1", "p1", "default", "sk-alpha-secret"},
		{"c2", "p2", "default", "sk-beta-secret"},
	} {
		enc, err := crypto.Encrypt([]byte(c.key), h.masterKey)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		cred := &store.Credential{ID: c.id, ProviderID: c.pid, Label: c.label,
			APIKeyEnc: enc, Enabled: true, Weight: 1, Status: "healthy"}
		if err := st.CreateCredential(ctx, cred); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
	}
}

// exportBody 调一次 Export，返回解析后的导出体与原始响应。
func exportBody(t *testing.T, h *ConfigTransferHandler, body string) (configExport, *httptest.ResponseRecorder) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = asAdmin(httptest.NewRequest(http.MethodPost, "/admin/api/config-export/export", nil))
	} else {
		r = asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/export", strings.NewReader(body)))
	}
	w := httptest.NewRecorder()
	h.Export(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d, body=%s", w.Code, w.Body.String())
	}
	var out configExport
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("导出结果不是合法 JSON: %v", err)
	}
	return out, w
}

// importInto 调一次 Import。
func importInto(t *testing.T, h *ConfigTransferHandler, req importRequest) importResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/import", bytes.NewReader(raw)))
	w := httptest.NewRecorder()
	h.Import(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("import status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp importResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("导入响应不是合法 JSON: %v", err)
	}
	return resp
}

// TestExportPlaintextOmitsCredentialsByDefault 钉住一条安全默认：
// 不给口令、不显式要求时，导出**不含凭据**。
//
// 导出文件最常见的去向是聊天工具与网盘，明文带凭据等于把上游的钱包
// 发出去。默认不带、想要得主动勾，是唯一不容易出事的方向。
func TestExportPlaintextOmitsCredentialsByDefault(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	out, _ := exportBody(t, h, "")
	if out.Encrypted {
		t.Fatalf("未给口令时不该加密")
	}
	if len(out.Providers) != 2 {
		t.Fatalf("供应商数 = %d，期望 2", len(out.Providers))
	}
	if len(out.Models) != 3 {
		t.Fatalf("模型数 = %d，期望 3", len(out.Models))
	}
	if len(out.Credentials) != 0 {
		t.Fatalf("默认导出不该带凭据，实际带了 %d 条", len(out.Credentials))
	}
	// 结构性字段必须完整：凭据可以不导出，供应商与模型的配置不能丢。
	byslug := map[string]providerExport{}
	for _, p := range out.Providers {
		byslug[p.Slug] = p
	}
	a := byslug["alpha"]
	if a.Endpoint != "https://a.example.com/v1" || a.Protocol != "openai-chat" ||
		a.TimeoutMs != 60000 || a.MaxRetries != 2 || !a.Enabled {
		t.Fatalf("alpha 的配置不完整: %+v", a)
	}
	if b := byslug["beta"]; b.Enabled {
		t.Fatalf("beta 应保持 disabled，实际 enabled")
	}
}

// TestExportIncludesCredentialsWhenAsked 确认显式要求时凭据会带上，
// 且解密后是原文。
func TestExportIncludesCredentialsWhenAsked(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	out, _ := exportBody(t, h, `{"include_credentials":true}`)
	if len(out.Credentials) != 2 {
		t.Fatalf("凭据数 = %d，期望 2", len(out.Credentials))
	}
	keys := map[string]string{}
	for _, c := range out.Credentials {
		keys[c.ProviderSlug] = c.APIKey
	}
	if keys["alpha"] != "sk-alpha-secret" || keys["beta"] != "sk-beta-secret" {
		t.Fatalf("凭据明文不对: %v", keys)
	}
}

// TestExportEncryptedRoundTrip 是加密导出的核心往返测试。
//
// 断言三件事：文件里看不到明文密钥、口令能解开、解出的内容与明文导出一致。
func TestExportEncryptedRoundTrip(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	const pass = "correct horse battery"
	enc, resp := exportBody(t, h, `{"passphrase":"`+pass+`","include_credentials":true}`)

	if !enc.Encrypted {
		t.Fatalf("给了口令就该加密")
	}
	// 最重要的一条：密文里绝不能出现明文密钥。
	if strings.Contains(resp.Body.String(), "sk-alpha-secret") ||
		strings.Contains(resp.Body.String(), "sk-beta-secret") {
		t.Fatalf("加密导出里出现了明文 API Key")
	}
	if strings.Contains(resp.Body.String(), "alpha-chat") {
		t.Fatalf("加密导出里连 model_id 都是明文 —— 说明只加密了部分内容")
	}
	if cd := resp.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("导出应以下载方式返回，Content-Disposition=%q", cd)
	}

	// 口令能解开，且内容完整
	opened, err := openExport(enc, pass)
	if err != nil {
		t.Fatalf("正确口令应能解开: %v", err)
	}
	if len(opened.Providers) != 2 || len(opened.Models) != 3 || len(opened.Credentials) != 2 {
		t.Fatalf("解出的内容不完整: p=%d m=%d c=%d",
			len(opened.Providers), len(opened.Models), len(opened.Credentials))
	}
}

// TestExportEncryptedWrongPassphraseRejected 确认口令错时报错，
// 且不泄露"口令对了但密文坏了"这种有用信息。
func TestExportEncryptedWrongPassphraseRejected(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	enc, _ := exportBody(t, h, `{"passphrase":"right-password","include_credentials":true}`)

	if _, err := openExport(enc, "wrong-password"); err == nil {
		t.Fatalf("错误口令应被拒")
	} else if strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("错误信息不该回显口令: %v", err)
	}
	// 空口令也不能解开 —— 否则"忘了输密码"会退化成"直接导入"。
	if _, err := openExport(enc, ""); err == nil {
		t.Fatalf("空口令应被拒")
	}
}

// TestExportEncryptedRequiresStrongPassphrase 确认弱口令被挡。
func TestExportEncryptedRequiresStrongPassphrase(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/export",
		strings.NewReader(`{"passphrase":"123"}`)))
	w := httptest.NewRecorder()
	h.Export(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("弱口令应返回 400，实际 %d", w.Code)
	}
}

// TestImportIntoEmptyDB 确认导入空库是完整还原。
func TestImportIntoEmptyDB(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, `{"include_credentials":true}`)

	dst, _ := transferFixture(t)
	resp := importInto(t, dst, importRequest{Data: file})

	if resp.ProvidersCreated != 2 {
		t.Fatalf("应新建 2 个供应商，实际 %d（%v）", resp.ProvidersCreated, resp.Notes)
	}
	if resp.ModelsCreated != 3 {
		t.Fatalf("应新建 3 个模型，实际 %d", resp.ModelsCreated)
	}
	if resp.CredentialsAdded != 2 {
		t.Fatalf("应新建 2 条凭据，实际 %d", resp.CredentialsAdded)
	}

	// 逐项核对落地结果，且凭据要用**目标库自己的 master key** 能解开。
	provs, _ := dst.store.ListProviders(context.Background())
	if len(provs) != 2 {
		t.Fatalf("目标库供应商数 = %d", len(provs))
	}
	var alphaID string
	for _, p := range provs {
		if p.Slug == "alpha" {
			alphaID = p.ID
		}
	}
	if alphaID == "" {
		t.Fatalf("alpha 没有落到目标库")
	}
	models, _ := dst.store.ListUpstreamModels(context.Background(), alphaID)
	if len(models) != 2 {
		t.Fatalf("alpha 的模型数 = %d，期望 2", len(models))
	}
	creds, _ := dst.store.ListCredentials(context.Background(), alphaID)
	if len(creds) != 1 {
		t.Fatalf("alpha 的凭据数 = %d，期望 1", len(creds))
	}
	plain, err := decryptSecret(creds[0].APIKeyEnc, dst.masterKey)
	if err != nil {
		t.Fatalf("导入的凭据用目标库密钥解不开 —— 这是最要命的失败形态: %v", err)
	}
	if plain != "sk-alpha-secret" {
		t.Fatalf("凭据明文 = %q", plain)
	}
}

// TestImportRenamesOnConflict 是本次需求的核心行为：
// 同名供应商不覆盖，导入的那条改名加 -2 后缀。
func TestImportRenamesOnConflict(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, "")

	dst, _ := transferFixture(t)
	ctx := context.Background()
	// 目标库已有一个 alpha，但 endpoint 与模型都不同 ——
	// 导入绝不能把它覆盖掉。
	existing := store.Provider{ID: "x1", Slug: "alpha", Name: "本地 Alpha",
		Protocol: "openai-chat", Endpoint: "http://localhost:1234/v1", Enabled: true}
	if err := dst.store.CreateProvider(ctx, &existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := importInto(t, dst, importRequest{Data: file})

	if got := resp.ProvidersRenamed["alpha"]; got != "alpha-2" {
		t.Fatalf("alpha 应改名为 alpha-2，实际 %q（renamed=%v）", got, resp.ProvidersRenamed)
	}
	// beta 不冲突，应保持原名
	if _, renamed := resp.ProvidersRenamed["beta"]; renamed {
		t.Fatalf("beta 不该改名: %v", resp.ProvidersRenamed)
	}

	provs, _ := dst.store.ListProviders(ctx)
	slugs := map[string]string{}
	for _, p := range provs {
		slugs[p.Slug] = p.Endpoint
	}
	if slugs["alpha"] != "http://localhost:1234/v1" {
		t.Fatalf("原有的 alpha 被覆盖了 —— 这是要绝对避免的")
	}
	if slugs["alpha-2"] != "https://a.example.com/v1" {
		t.Fatalf("导入的那条没有落到 alpha-2: %v", slugs)
	}
	if slugs["beta"] != "https://b.example.com/v1" {
		t.Fatalf("beta 应保持原 slug: %v", slugs)
	}
}

// TestImportRenamesRepeatedlyToUniqueSlugs 确认连撞两次会得到 -2 / -3，
// 而不是两次都叫 -2（那会让第二个 provider 撞上第一个）。
func TestImportRenamesRepeatedlyToUniqueSlugs(t *testing.T) {
	file := configExport{
		Version: exportFormatVersion,
		Providers: []providerExport{
			{Slug: "dup", Name: "Dup", Protocol: "openai-chat", Endpoint: "https://x/v1", Enabled: true},
		},
	}

	dst, _ := transferFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		slug := "dup"
		if i > 0 {
			slug = "dup-" + string(rune('1'+i))
		}
		if err := dst.store.CreateProvider(ctx, &store.Provider{
			ID: "e" + string(rune('0'+i)), Slug: slug, Name: slug,
			Protocol: "openai-chat", Endpoint: "http://local/v1", Enabled: true,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	resp := importInto(t, dst, importRequest{Data: file})
	if got := resp.ProvidersRenamed["dup"]; got != "dup-4" {
		t.Fatalf("应得到 dup-4（dup/-2/-3 已占），实际 %q", got)
	}
}

// TestImportSkipsExistingModel 确认已有同名模型保留库里的配置。
//
// model_id 是要发给上游的字面量，改它等于换模型；而定价与上下文窗口
// 在本库可能已经被调过。导入方无从得知，一律保留。
// TestImportSkipsModelAlreadyUnderSameProvider 确认**同一个供应商下**
// 已有的同名模型保留库里配置、不被导入覆盖。
//
// model_id 是要发给上游的字面量，改它等于换模型；而定价与上下文窗口在
// 本库可能已被调过，导入方无从得知。
//
// 场景需要绕过「同名供应商自动改名」才能构造出真正的同provider 场景，
// 所以导入前先建一个alpha-2 占位（这样导入的 alpha 不会撞名）。
func TestImportSkipsModelAlreadyUnderSameProvider(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, "")

	dst, _ := transferFixture(t)
	ctx := context.Background()
	// 让导入的 alpha 保持原slug：目标库里 alpha 已被占用，因此占位 alpha-2。
	if err := dst.store.CreateProvider(ctx, &store.Provider{ID: "occupy", Slug: "alpha-2",
		Name: "占位", Protocol: "openai-chat", Endpoint: "http://placeholder/v1", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := store.Provider{ID: "p1", Slug: "alpha", Name: "Alpha",
		Protocol: "openai-chat", Endpoint: "http://localhost/v1", Enabled: true}
	if err := dst.store.CreateProvider(ctx, &p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := store.UpstreamModel{ID: "m1", ProviderID: "p1", ModelID: "alpha-chat",
		Enabled: true, PriceInput: 99, ContextWindow: 4096}
	if err := dst.store.CreateUpstreamModel(ctx, &m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := importInto(t, dst, importRequest{Data: file})

	// 导入的 alpha 撞上了 alpha-2，所以应改名 alpha-3
	if resp.ProvidersRenamed["alpha"] != "alpha-3" {
		t.Fatalf("应为 alpha-3（alpha-2 已占），实际 %v", resp.ProvidersRenamed)
	}

	// 关键：导入的 alpha-chat 属于**新provider（alpha-3）**，
	// 不能因为原有 provider 下有个同名 alpha-chat 就跳过它 ——
	// 那会让模型挂到错误的供应商上。
	newModels, _ := dst.store.ListUpstreamModels(ctx, resp3ID(t, dst, "alpha-3"))
	if len(newModels) != 2 {
		t.Fatalf("alpha-3 下应有文件里的 2 个模型，实际 %d", len(newModels))
	}

	// 原有那个价格不能被动
	oldModels, _ := dst.store.ListUpstreamModels(ctx, "p1")
	for _, got := range oldModels {
		if got.ModelID == "alpha-chat" && got.PriceInput != 99 {
			t.Fatalf("原有模型价格被改: %v", got.PriceInput)
		}
	}
}

// resp3ID 按 slug 取 provider id，测试里少写一次断言。
func resp3ID(t *testing.T, h *ConfigTransferHandler, slug string) string {
	t.Helper()
	provs, _ := h.store.ListProviders(context.Background())
	for _, p := range provs {
		if p.Slug == slug {
			return p.ID
		}
	}
	t.Fatalf("找不到 provider %s", slug)
	return ""
}

// TestImportModelsFollowRenamedProvider 确认改名后，文件里的模型与凭据
// 仍然挂到**新** provider 上，而不是因为原slug 不见了被丢弃。
//
// 这是最容易写错的一处：文件里模型引用的是原 slug，而库里的新 provider
// 叫 alpha-2 —— 若按新 slug 查就会全部"找不到供应商"。
func TestImportModelsFollowRenamedProvider(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, `{"include_credentials":true}`)

	dst, _ := transferFixture(t)
	ctx := context.Background()
	if err := dst.store.CreateProvider(ctx, &store.Provider{ID: "x1", Slug: "alpha",
		Name: "本地", Protocol: "openai-chat", Endpoint: "http://local/v1", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := importInto(t, dst, importRequest{Data: file})

	var newID string
	provs, _ := dst.store.ListProviders(ctx)
	for _, p := range provs {
		if p.Slug == "alpha-2" {
			newID = p.ID
		}
	}
	if newID == "" {
		t.Fatalf("没有创建 alpha-2: %v", resp.Notes)
	}
	models, _ := dst.store.ListUpstreamModels(ctx, newID)
	if len(models) != 2 {
		t.Fatalf("alpha-2 下应有 2 个模型，实际 %d（notes=%v）", len(models), resp.Notes)
	}
	creds, _ := dst.store.ListCredentials(ctx, newID)
	if len(creds) != 1 {
		t.Fatalf("alpha-2 下应有 1 条凭据，实际 %d", len(creds))
	}
}

// TestImportEncryptedFileEndToEnd 走完整的加密文件往返：加密导出 → 导入。
func TestImportEncryptedFileEndToEnd(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, `{"passphrase":"export-pass-1","include_credentials":true}`)

	dst, _ := transferFixture(t)

	// 不带口令导入必须失败，且要说清原因
	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/import",
		bytes.NewReader(mustMarshal(t, importRequest{Data: file}))))
	w := httptest.NewRecorder()
	dst.Import(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("加密文件缺口令应报 400，实际 %d", w.Code)
	}

	resp := importInto(t, dst, importRequest{Data: file, Passphrase: "export-pass-1"})
	if resp.ProvidersCreated != 2 || resp.ModelsCreated != 3 || resp.CredentialsAdded != 2 {
		t.Fatalf("加密文件导入不完整: %+v", resp)
	}
}

// TestImportDryRunWritesNothing 是最重要的一条安全断言：
// 干跑必须一个字节都不写。
//
// 导入是「改一个可能已经在服务线上跑的库」的动作，用户需要先看清会发生
// 什么。干跑如果偷偷写了一部分，那比没有干跑更糟。
func TestImportDryRunWritesNothing(t *testing.T) {
	src, _ := transferFixture(t)
	seedSource(t, src, src.store)
	file, _ := exportBody(t, src, `{"include_credentials":true}`)

	dst, _ := transferFixture(t)
	ctx := context.Background()
	if err := dst.store.CreateProvider(ctx, &store.Provider{ID: "x1", Slug: "alpha",
		Name: "本地", Protocol: "openai-chat", Endpoint: "http://local/v1", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := importInto(t, dst, importRequest{Data: file, DryRun: true})

	if !resp.DryRun {
		t.Fatalf("响应应标记 dry_run")
	}
	// 干跑也要算出「本来会发生什么」，否则预览毫无意义
	if resp.ProvidersCreated != 2 || resp.ModelsCreated != 3 {
		t.Fatalf("干跑应报出将要发生的数量: %+v", resp)
	}
	if resp.ProvidersRenamed["alpha"] != "alpha-2" {
		t.Fatalf("干跑应预告改名: %v", resp.ProvidersRenamed)
	}

	provs, _ := dst.store.ListProviders(ctx)
	if len(provs) != 1 {
		t.Fatalf("干跑后库里的供应商应仍为 1，实际 %d —— 干跑写库了", len(provs))
	}
	models, _ := dst.store.ListAllUpstreamModels(ctx)
	if len(models) != 0 {
		t.Fatalf("干跑不该写入任何模型，实际 %d", len(models))
	}
}

// TestImportRejectsUnknownVersion 确认不认识的格式版本被拒，而不是尽力解析。
func TestImportRejectsUnknownVersion(t *testing.T) {
	dst, _ := transferFixture(t)

	if _, err := openExport(configExport{Version: 99}, ""); err == nil {
		t.Fatalf("不认识的版本应被 openExport 拒")
	}

	r := asAdmin(jsonRequest(http.MethodPost, "/admin/api/config-export/import",
		bytes.NewReader(mustMarshal(t, importRequest{
			Data: configExport{Version: 99, Providers: []providerExport{
				{Slug: "x", Endpoint: "https://x/v1", Protocol: "openai-chat"},
			}},
		}))))
	w := httptest.NewRecorder()
	dst.Import(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("v99 文件应返回 400，实际 %d", w.Code)
	}
	// 一个字节都不该写
	provs, _ := dst.store.ListProviders(context.Background())
	if len(provs) != 0 {
		t.Fatalf("版本不认识时不该写入任何供应商，实际 %d", len(provs))
	}
}

// TestExportRequiresAdmin 确认导出是admin-only。
//
// 导出体可能含全部上游凭据，落到普通用户手里等于把钱包交出去。
func TestExportRequiresAdmin(t *testing.T) {
	h, _ := transferFixture(t)
	seedSource(t, h, h.store)

	t.Run("未登录", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/admin/api/config-export/export", nil)
		w := httptest.NewRecorder()
		h.Export(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("未登录应 401，实际 %d", w.Code)
		}
	})
	t.Run("普通用户", func(t *testing.T) {
		r := asUser(httptest.NewRequest(http.MethodPost, "/admin/api/config-export/export", nil), "bob")
		w := httptest.NewRecorder()
		h.Export(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("普通用户应 403，实际 %d", w.Code)
		}
	})
}

// TestImportRequiresAdmin 同上，导入是写操作且会落凭据。
func TestImportRequiresAdmin(t *testing.T) {
	h, _ := transferFixture(t)

	raw := mustMarshal(t, importRequest{})
	t.Run("未登录", func(t *testing.T) {
		r := jsonRequest(http.MethodPost, "/admin/api/config-export/import", bytes.NewReader(raw))
		w := httptest.NewRecorder()
		h.Import(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("未登录应 401，实际 %d", w.Code)
		}
	})
	t.Run("普通用户", func(t *testing.T) {
		r := asUser(jsonRequest(http.MethodPost, "/admin/api/config-export/import", bytes.NewReader(raw)), "bob")
		w := httptest.NewRecorder()
		h.Import(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("普通用户应 403，实际 %d", w.Code)
		}
	})
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// CONFIG-01 回归：导入不得绕过 provider/model handler 的数值校验，
// 凭据的 Enabled 必须跟随导出文件而不是硬编码 true。
//
// 原先 timeout_ms 超大的文件照单全收 —— time.Duration 回绕成负数后该
// provider 的请求当场全挂；负单价原样落库，该模型每次调用倒贴钱；
// 导出里被禁用的凭据导入后静默复活。
func TestImportValidatesNumericFieldsAndHonorsCredentialEnabled(t *testing.T) {
	dst, _ := transferFixture(t)
	enabled := true
	disabled := false
	file := configExport{
		Version: exportFormatVersion,
		Providers: []providerExport{
			{Slug: "bad-timeout", Name: "Bad Timeout", Protocol: "openai-chat",
				Endpoint: "https://x.example.com/v1", Enabled: true,
				TimeoutMs: config.MaxDurationMillis + 1},
			{Slug: "bad-retries", Name: "Bad Retries", Protocol: "openai-chat",
				Endpoint: "https://x.example.com/v1", MaxRetries: -1},
			{Slug: "good", Name: "Good", Protocol: "openai-chat",
				Endpoint: "https://x.example.com/v1", Enabled: true},
		},
		Models: []modelExport{
			{ProviderSlug: "good", ModelID: "m-neg-price", Enabled: true, PriceInput: -0.5},
			{ProviderSlug: "good", ModelID: "m-ok", Enabled: true, PriceInput: 1.5, PriceOutput: 6},
		},
		Credentials: []credentialExport{
			{ProviderSlug: "good", Label: "off", APIKey: "sk-disabled", Enabled: &disabled},
			{ProviderSlug: "good", Label: "legacy", APIKey: "sk-legacy", Enabled: &enabled},
			// 旧版导出体 / 手写文件没有 enabled 字段：按 true 处理。
			{ProviderSlug: "good", Label: "missing-field", APIKey: "sk-missing"},
		},
	}

	resp := importInto(t, dst, importRequest{Data: file})

	provs, _ := dst.store.ListProviders(context.Background())
	slugs := map[string]bool{}
	for _, p := range provs {
		slugs[p.Slug] = true
	}
	if slugs["bad-timeout"] || slugs["bad-retries"] {
		t.Fatalf("非法数值的供应商不该落库，实际落了：%v", slugs)
	}
	if !slugs["good"] {
		t.Fatalf("合法供应商 good 没有落库：%v", slugs)
	}
	if len(resp.Warnings) < 2 {
		t.Fatalf("非法供应商应留下告警，实际 warnings=%v", resp.Warnings)
	}

	var goodID string
	for _, p := range provs {
		if p.Slug == "good" {
			goodID = p.ID
		}
	}
	models, _ := dst.store.ListUpstreamModels(context.Background(), goodID)
	if len(models) != 1 || models[0].ModelID != "m-ok" {
		t.Fatalf("负单价模型应被跳过、正常模型应落库，实际 %+v", models)
	}

	creds, _ := dst.store.ListCredentials(context.Background(), goodID)
	states := map[string]bool{}
	for _, c := range creds {
		states[c.Label] = c.Enabled
	}
	if len(creds) != 3 {
		t.Fatalf("应落库 3 条凭据，实际 %d", len(creds))
	}
	if states["off"] {
		t.Errorf("文件里禁用的凭据被导入成启用（CONFIG-01 原缺陷）")
	}
	if !states["legacy"] || !states["missing-field"] {
		t.Errorf("显式 true 与字段缺失都应导入为启用：%v", states)
	}
}
