package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestParseBalanceSpec 覆盖显式配置的解析与校验。
//
// 重点是「未配置」与「配错」必须分开：前者回落自动判定（正常形态），
// 后者是管理员写错了东西，必须报错而不是静默回落 —— 静默回落会让
// 管理员以为改的配置生效了，从而反复排查一个看起来被忽略的问题。
func TestParseBalanceSpec(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantNil bool
		wantErr string
		check   func(t *testing.T, s *BalanceSpec)
	}{
		{name: "未配置-空串", raw: "", wantNil: true},
		{name: "未配置-纯空白", raw: "   \n ", wantNil: true},
		{name: "非法JSON", raw: "{not json", wantErr: "不是合法 JSON"},
		{name: "path缺失", raw: `{"field":"a"}`, wantErr: "path 不能为空"},
		{name: "path非斜杠开头", raw: `{"path":"credits"}`, wantErr: "必须以 / 开头"},
		{name: "path是绝对URL", raw: `{"path":"https://evil.com/x"}`, wantErr: "必须以 / 开头"},
		{name: "auth非法", raw: `{"path":"/x","auth":"cookie"}`, wantErr: "auth 只支持"},
		// 路径规范化与「不许穿越」（2026-10-21 修 A5-P1-1）。
		{name: "path含..要拒绝", raw: `{"path":"/a/../../etc"}`, wantErr: "不允许包含 .."},
		{name: "path含反斜杠要拒绝", raw: `{"path":"/a\\b"}`, wantErr: "反斜杠"},
		{name: "path里的..要拒绝而不是归一", raw: `{"path":"/v1/dashboard/../balance"}`,
			wantErr: "不允许包含 .."},
		{name: "path末尾..要拒绝", raw: `{"path":"/v1/.."}`,
			wantErr: "不允许包含 .."},
		{name: "最小配置", raw: `{"path":"/credits"}`,
			check: func(t *testing.T, s *BalanceSpec) {
				if s.Field != "balance" || s.Currency != "USD" || s.Auth != "bearer" {
					t.Fatalf("默认值不符：%+v", s)
				}
			}},
		{name: "全字段", raw: `{"path":"/x","field":"a.b","currency":"CNY","auth":"anthropic"}`,
			check: func(t *testing.T, s *BalanceSpec) {
				if s.Field != "a.b" || s.Currency != "CNY" || s.Auth != "anthropic" {
					t.Fatalf("字段未被读入：%+v", s)
				}
			}},
		{name: "path两端空白被Trim", raw: `{"path":"  /x  "}`,
			check: func(t *testing.T, s *BalanceSpec) {
				if s.Path != "/x" {
					t.Fatalf("path 未 trim：%q", s.Path)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseBalanceSpec(c.raw)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错 %q，实际通过", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误信息应含 %q，实际是 %q", c.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if c.wantNil {
				if got != nil {
					t.Fatalf("期望 nil，实际 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("期望非 nil")
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// TestFetchCustomBalance 端到端验证自定义余额查询：真实 HTTP + 点号取值。
func TestFetchCustomBalance(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		switch r.URL.Path {
		case "/credits":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"remaining": 42.5}})
		case "/list":
			// 数组下标取值：余额响应偶尔把记录放在数组里。
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"amount": "7.25"}}})
		case "/as-string":
			_ = json.NewEncoder(w).Encode(map[string]any{"balance": "19.99"})
		case "/missing":
			_ = json.NewEncoder(w).Encode(map[string]any{"other": 1})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := &store.Provider{ID: "p1", Endpoint: srv.URL, Protocol: "chat"}

	t.Run("嵌套字段", func(t *testing.T) {
		spec := &BalanceSpec{Path: "/credits", Field: "data.remaining", Currency: "USD", Auth: "bearer"}
		amt, cur, detail, err := fetchCustomBalance(ctx, p, "sk-x", spec)
		if err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		if amt != 42.5 || cur != "USD" {
			t.Fatalf("金额/币种不对：%v %s", amt, cur)
		}
		if gotAuth != "Bearer sk-x" {
			t.Fatalf("鉴权头不对：%q", gotAuth)
		}
		if gotPath != "/credits" {
			t.Fatalf("路径不对：%q", gotPath)
		}
		if detail == "" {
			t.Fatal("detail 应带上取值路径")
		}
	})

	t.Run("数组下标", func(t *testing.T) {
		spec := &BalanceSpec{Path: "/list", Field: "items.0.amount", Currency: "USD", Auth: "bearer"}
		amt, _, _, err := fetchCustomBalance(ctx, p, "sk-x", spec)
		if err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		if amt != 7.25 {
			t.Fatalf("字符串数值未被解析：%v", amt)
		}
	})

	t.Run("字符串数值", func(t *testing.T) {
		spec := &BalanceSpec{Path: "/as-string", Field: "balance", Currency: "CNY", Auth: "bearer"}
		amt, _, _, err := fetchCustomBalance(ctx, p, "sk-x", spec)
		if err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		if amt != 19.99 {
			t.Fatalf("金额不对：%v", amt)
		}
	})

	t.Run("字段不存在", func(t *testing.T) {
		spec := &BalanceSpec{Path: "/missing", Field: "balance", Currency: "USD", Auth: "bearer"}
		if _, _, _, err := fetchCustomBalance(ctx, p, "sk-x", spec); err == nil {
			t.Fatal("应当报错并指出缺哪个字段")
		}
	})

	t.Run("404归为不支持", func(t *testing.T) {
		spec := &BalanceSpec{Path: "/nope", Field: "balance", Currency: "USD", Auth: "bearer"}
		_, _, _, err := fetchCustomBalance(ctx, p, "sk-x", spec)
		if err == nil || !isUnsupportedBalanceErr(err) {
			t.Fatalf("404 应归为 unsupported，实际 %v", err)
		}
	})
}

// TestHostIs 守住主机名判定的收紧：Contains 曾把
// `https://api.openai.com.evil.example` 判成 OpenAI 官方，那会把 key
// 与账单信息送到第三方。
func TestHostIs(t *testing.T) {
	cases := []struct {
		base, want string
		expect     bool
	}{
		{"https://api.openai.com", "api.openai.com", true},
		{"https://API.OpenAI.COM/v1", "api.openai.com", true},
		{"https://api.openai.com.evil.example", "api.openai.com", false},
		{"https://evil-api.deepseek.com", "api.deepseek.com", false},
		{"https://api.deepseek.com", "api.openai.com", false},
		{"not a url", "api.openai.com", false},
		{"https://corp-mirror.example/api.deepseek.com", "api.deepseek.com", false},
	}
	for _, c := range cases {
		if got := hostIs(c.base, c.want); got != c.expect {
			t.Errorf("hostIs(%q, %q) = %v, want %v", c.base, c.want, got, c.expect)
		}
	}
}
