package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"

	"github.com/cn-maul/rosetta-gateway/internal/catalog"
	"github.com/cn-maul/rosetta-gateway/internal/gatewayapp"
	"github.com/cn-maul/rosetta-gateway/internal/runtime"
	"github.com/cn-maul/rosetta-gateway/internal/server"
)

// homeEnvVar redirects all process state to a mounted volume.
const homeEnvVar = gatewayapp.HomeEnvVar

// buildVersion is injected by -ldflags "-X main.buildVersion=x.y.z".
var buildVersion = "dev"

func main() {
	if err := gatewayapp.Run(gatewayapp.Options{
		Version:      buildVersion,
		BuildHandler: buildHandlerFromApp,
		Bootstrap:    initializeAdminBootstrap,
	}); err != nil {
		slog.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

// Compatibility aliases keep package-local tests and route wiring on the
// extracted implementations.
type runtimeReloader = runtime.Reloader

func healthzHandler(db gatewayapp.HealthzPinger, reloader gatewayapp.HealthzDirty, logger *slog.Logger) http.HandlerFunc {
	return gatewayapp.HealthzHandler(db, reloader, logger)
}

func denyAdminKeyCreate(next http.HandlerFunc) http.HandlerFunc {
	return server.DenyAdminKeyCreate(next)
}

func handleListModels(forceShape string) http.HandlerFunc {
	return catalog.List(forceShape)
}

func handleGetModel(forceShape string) http.HandlerFunc {
	return catalog.Get(forceShape)
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
