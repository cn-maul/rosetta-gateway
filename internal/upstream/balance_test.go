package upstream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 余额查询的测试全部走真实的 store（建库 + 加密凭据），而不是 mock：
// 这个功能的一半复杂度在于「凭据解密 + 协议 + 端点形状」的组合，
// mock 掉前两者就等于没测。

func balanceFixture(t *testing.T) (*store.Store, string, []byte) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/bal.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	master := []byte("0123456789abcdef0123456789abcdef")
	p := &store.Provider{
		ID: "p1", Slug: "p1", Name: "P1", Protocol: "openai-chat",
		Endpoint: "http://127.0.0.1:0", Enabled: true,
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	enc, err := crypto.Encrypt([]byte("sk-secret"), master)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cred := &store.Credential{
		ID: "c1", ProviderID: "p1", Label: "主号",
		APIKeyEnc: enc, Enabled: true, Weight: 1, Status: "healthy",
	}
	if err := st.CreateCredential(context.Background(), cred); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return st, p.ID, master
}

func setEndpoint(t *testing.T, st *store.Store, id, endpoint string) {
	t.Helper()
	p, err := st.GetProvider(context.Background(), id)
	if err != nil || p == nil {
		t.Fatalf("get provider: %v", err)
	}
	p.Endpoint = endpoint
	if err := st.UpdateProvider(context.Background(), id, p); err != nil {
		t.Fatalf("update provider: %v", err)
	}
}

func balanceFor(t *testing.T, st *store.Store, pid string, master []byte) *ProviderBalance {
	t.Helper()
	res, err := QueryProviderBalance(context.Background(), st, pid, master)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return res
}

// asDialect 让本进程内的 provider 走指定方言（见 balanceDialect 的注释）。
// 用 t.Cleanup 复原，保证测试之间互不污染。
func asDialect(t *testing.T, f func(context.Context, *store.Provider, string) (float64, string, string, error)) {
	t.Helper()
	prev := balanceDialect
	balanceDialect = func() func(context.Context, *store.Provider, string) (float64, string, string, error) {
		return f
	}
	t.Cleanup(func() { balanceDialect = prev })
}

// TestQueryProviderBalance_OpenAI 覆盖最常见的一种形状：额度上限 + 已用，
// 结果是「还能花多少」。
func TestQueryProviderBalance_OpenAI(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/dashboard/billing/subscription":
			fmt.Fprint(w, `{"hard_limit_usd":100,"soft_limit_usd":90}`)
		case "/dashboard/billing/usage":
			fmt.Fprint(w, `{"total_usage":25}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, srv.URL)
	asDialect(t, func(ctx context.Context, p *store.Provider, key string) (float64, string, string, error) {
		return fetchOpenAIBalance(ctx, srv.URL, key)
	})

	res := balanceFor(t, st, pid, master)
	if res.Status != "ok" || len(res.Results) != 1 {
		t.Fatalf("status=%s results=%+v", res.Status, res.Results)
	}
	r := res.Results[0]
	if r.Status != "ok" {
		t.Fatalf("凭据状态 = %s（%s）", r.Status, r.Message)
	}
	if r.Amount != 75 || r.Currency != "USD" {
		t.Fatalf("可用额度 = %v %v，期望 75 USD", r.Amount, r.Currency)
	}
	if r.Detail == "" {
		t.Fatal("detail 应带上总额度/已用，否则管理员无法核对口径")
	}
	if r.FetchedAt == 0 {
		t.Fatal("必须带回取数时刻，界面要靠它说明这个数字有多旧")
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("凭据没带上：%q", gotAuth)
	}
}

// TestQueryProviderBalance_DeepSeek 与 OpenAI 的关键差别是「上游自己说了
// 余额不足」：is_available=false 是明确事实，不能被 balance_infos 里的
// 数字盖过去。
func TestQueryProviderBalance_DeepSeek(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"is_available":false,"balance_infos":{"CNY":"12.34"}}`)
	}))
	defer srv.Close()

	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, srv.URL)
	asDialect(t, func(ctx context.Context, p *store.Provider, key string) (float64, string, string, error) {
		return fetchDeepSeekBalance(ctx, srv.URL, key)
	})

	r := balanceFor(t, st, pid, master).Results[0]
	if r.Status != "ok" {
		t.Fatalf("状态 = %s（%s）", r.Status, r.Message)
	}
	if r.Amount != 0 {
		t.Fatalf("is_available=false 时可用额度应为 0，实际 %v", r.Amount)
	}
	if r.Detail == "" {
		t.Fatal("必须说明上游标记了余额不足 —— 否则 0 会被读成「网关算错了」")
	}
}

// TestQueryProviderBalance_不支持的上游 如实说「不支持」，而不是打一枪
// 拿到 404 再让管理员去排查凭据 —— 那会把人引向错误的排查方向。
func TestQueryProviderBalance_不支持的上游(t *testing.T) {
	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, "https://api.anthropic.com")

	res := balanceFor(t, st, pid, master)
	if res.Status != "unsupported" {
		t.Fatalf("status = %s，期望 unsupported", res.Status)
	}
	if res.Results[0].Status != "unsupported" {
		t.Fatalf("凭据状态 = %s", res.Results[0].Status)
	}
}

// TestQueryProviderBalance_凭据级失败互不影响 守住「一把查不到、另一把
// 照常显示」。故障转移恰恰会让请求悄悄从欠费那把切到好的那把，管理员
// 若看不到「哪一把快没钱了」，就会等到真正欠费拒付才发现。
func TestQueryProviderBalance_凭据级失败互不影响(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 第二把 key 故意无权限。
		if r.Header.Get("Authorization") == "Bearer sk-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"hard_limit_usd":50}`)
	}))
	defer srv.Close()

	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, srv.URL)
	asDialect(t, func(ctx context.Context, p *store.Provider, key string) (float64, string, string, error) {
		return fetchOpenAIBalance(ctx, srv.URL, key)
	})

	enc, err := crypto.Encrypt([]byte("sk-other"), master)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.CreateCredential(context.Background(), &store.Credential{
		ID: "c2", ProviderID: "p1", Label: "备用", APIKeyEnc: enc,
		Enabled: true, Weight: 1, Status: "healthy",
	}); err != nil {
		t.Fatalf("create cred 2: %v", err)
	}

	res := balanceFor(t, st, pid, master)
	if res.Status != "partial" {
		t.Fatalf("status = %s，期望 partial（一成功一失败）", res.Status)
	}
	byID := map[string]CredentialBalance{}
	for _, r := range res.Results {
		byID[r.CredentialID] = r
	}
	if byID["c1"].Status != "error" {
		t.Fatalf("c1 应为 error，实际 %+v", byID["c1"])
	}
	if byID["c2"].Status != "ok" || byID["c2"].Amount != 50 {
		t.Fatalf("c2 应查到 50，实际 %+v", byID["c2"])
	}
}

// TestQueryProviderBalance_停用凭据不查 但**必须出现在结果里**：
// 它不出现的话界面上那行会「凭空消失」，看起来像被删了。
func TestQueryProviderBalance_停用凭据不查(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("停用的凭据不该发起请求")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, srv.URL)
	asDialect(t, func(ctx context.Context, p *store.Provider, key string) (float64, string, string, error) {
		return fetchOpenAIBalance(ctx, srv.URL, key)
	})

	cred, _ := st.GetCredential(context.Background(), "c1")
	cred.Enabled = false
	if err := st.UpdateCredential(context.Background(), "c1", cred); err != nil {
		t.Fatalf("disable: %v", err)
	}

	res := balanceFor(t, st, pid, master)
	if len(res.Results) != 1 || res.Results[0].Status != "unsupported" {
		t.Fatalf("停用凭据应回一条 unsupported，实际 %+v", res.Results)
	}
}

// TestQueryProviderBalance_不跟随重定向 带着 API key 的一次 302 就能把
// 凭据与账户信息送到管理员填的 endpoint 之外的主机上。
func TestQueryProviderBalance_不跟随重定向(t *testing.T) {
	var leaked string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"hard_limit_usd":9999}`)
	}))
	defer elsewhere.Close()

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/dashboard/billing/subscription", http.StatusFound)
	}))
	defer redir.Close()

	st, pid, master := balanceFixture(t)
	setEndpoint(t, st, pid, redir.URL)
	asDialect(t, func(ctx context.Context, p *store.Provider, key string) (float64, string, string, error) {
		return fetchOpenAIBalance(ctx, redir.URL, key)
	})

	res := balanceFor(t, st, pid, master)
	if leaked != "" {
		t.Fatalf("凭据被带去了重定向目标：%q", leaked)
	}
	if res.Results[0].Status != "error" {
		t.Fatalf("302 应被报成失败而不是跟随，实际 %+v", res.Results[0])
	}
}
