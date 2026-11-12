package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"

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
