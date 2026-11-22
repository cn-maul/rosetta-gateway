package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	// 把 IANA 时区库编进二进制。Windows 没有 /usr/share/zoneinfo，
	// 而真实部署的 config.json 常写 timezone: "Asia/Shanghai" —— 不内嵌时
	// time.LoadLocation 会直接失败，网关在配置校验阶段就退出。
	_ "time/tzdata"

	"github.com/cn-maul/rosetta-gateway/internal/gatewayapp"
)

// buildVersion is injected by -ldflags "-X main.buildVersion=x.y.z".
var buildVersion = "dev"

func main() {
	if err := gatewayapp.Run(gatewayapp.Options{
		Version:   buildVersion,
		Bootstrap: initializeAdminBootstrap,
	}); err != nil {
		slog.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
