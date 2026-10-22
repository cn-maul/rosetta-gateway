package gatewayapp

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
)

// captureLogs 造一个把所有级别都收进 buffer 的 logger。
//
// 必须收 WARN：安全契约对「通配监听」是告警（进程之外决定真实暴露面），
// 对「绑定到具体网卡」才是 ERROR。只收 ERROR 会让前者看不见。
func captureLogs() (*strings.Builder, *slog.Logger) {
	var logs strings.Builder
	return &logs, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestCheckTLSContract 钉住「公开监听且未声明 TLS」的进程级契约
// （2026-10-22 修 A10-P0-1 残留）。
//
// 网关自身不做 TLS。监听在回环地址上时暴露面只有本机；监听在非回环
// 地址上时，明文 HTTP 会让同网段的被动观察者完整截获管理员会话 cookie、
// JWT、上游凭据与下游 Authorization 头。
//
// 此前这个事实只写在 README 里，进程自己一声不吭。
func TestCheckTLSContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		listen     string
		origin     string
		requireTLS bool
		wantErr    bool
		wantLevel  string // 期望日志里出现的级别关键字，空 = 不期望任何日志
		wantMsg    string
	}{
		{
			name:   "回环监听不告警",
			listen: "127.0.0.1:8666",
		},
		{
			name:   "回环监听 + 非 https 公开源不告警",
			listen: "127.0.0.1:8666",
			origin: "http://gw.example.com",
		},
		{
			name:   "通配监听 + https 公开源不告警（反代终止 TLS）",
			listen: "0.0.0.0:8666",
			origin: "https://gw.example.com",
		},
		{
			// 容器内必须绑 0.0.0.0 才能被端口映射；真实暴露面由宿主决定，
			// 进程看不到 —— 所以是 WARN 而不是 ERROR。
			name:      "通配监听 + 无公开源 → WARN",
			listen:    "0.0.0.0:8666",
			wantLevel: "level=WARN",
			wantMsg:   "listening on all interfaces without declared TLS",
		},
		{
			name:      "通配监听 + http 公开源 → WARN",
			listen:    "0.0.0.0:8666",
			origin:    "http://gw.example.com",
			wantLevel: "level=WARN",
			wantMsg:   "listening on all interfaces without declared TLS",
		},
		{
			// 绑定到具体网卡是明确的暴露意图，那张网卡上的人一定能连上。
			name:      "指定网卡监听 + 无公开源 → ERROR",
			listen:    "192.168.1.10:8666",
			wantLevel: "level=ERROR",
			wantMsg:   "bound to a specific non-loopback interface without declared TLS",
		},
		{
			name:       "通配监听 + 强制要求 → 拒绝启动",
			listen:     "0.0.0.0:8666",
			requireTLS: true,
			wantErr:    true,
			wantLevel:  "level=ERROR",
			wantMsg:    "refusing to start",
		},
		{
			name:       "指定网卡 + 强制要求 → 拒绝启动",
			listen:     "192.168.1.10:8666",
			requireTLS: true,
			wantErr:    true,
			wantLevel:  "level=ERROR",
			wantMsg:    "refusing to start",
		},
		{
			name:       "通配监听 + https 公开源 + 强制要求 → 放行",
			listen:     "0.0.0.0:8666",
			origin:     "https://gw.example.com",
			requireTLS: true,
		},
		{
			name:       "回环监听 + 强制要求 → 放行",
			listen:     "127.0.0.1:8666",
			requireTLS: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, logger := captureLogs()
			cfg := &config.Config{
				Listen:                    tc.listen,
				PublicOrigin:              tc.origin,
				RequireTLSForPublicListen: tc.requireTLS,
			}

			err := checkTLSContract(cfg, logger)
			if tc.wantErr && err == nil {
				t.Fatalf("期望拒绝启动，实际放行")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行，实际拒绝：%v", err)
			}
			got := logs.String()
			if tc.wantMsg == "" {
				if got != "" {
					t.Fatalf("不该有任何日志，实际：%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantMsg) {
				t.Fatalf("缺少日志 %q，实际：%s", tc.wantMsg, got)
			}
			if !strings.Contains(got, tc.wantLevel) {
				t.Fatalf("期望级别 %q，实际：%s", tc.wantLevel, got)
			}
		})
	}
}

// TestCheckTLSContract_NilConfig 兜住「配置还没加载」的边界：契约检查
// 不该在 nil 配置上 panic —— 那是启动早期就崩，比漏报告警严重得多。
func TestCheckTLSContract_NilConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := checkTLSContract(nil, logger); err != nil {
		t.Fatalf("nil 配置不该报错：%v", err)
	}
}

// TestIsWildcardListen 钉住通配监听的分级判据。
//
// 分级本身是这次修复的取舍点：容器模板就是 0.0.0.0，若对它报 ERROR，
// 每次启动都刷一条红色安全日志，运维会学会无视 SECURITY 前缀。
func TestIsWildcardListen(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{"0.0.0.0:8666", true},
		{"[::]:8666", true},
		{":8666", true},
		{"127.0.0.1:8666", false},
		{"192.168.1.10:8666", false},
		{"[::1]:8666", false},
		{"not-an-address", false},
	} {
		if got := isWildcardListen(tc.listen); got != tc.want {
			t.Errorf("isWildcardListen(%q) = %v，期望 %v", tc.listen, got, tc.want)
		}
	}
}
