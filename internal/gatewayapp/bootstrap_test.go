package gatewayapp

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestR2_M02_01BootstrapAPIKeyWinsOverUnsetEnvironment(t *testing.T) {
	const envName = "ROSETTA_GATEWAY_TEST_M02_01_UNSET"
	t.Setenv(envName, "")

	cfg := config.Default()
	cfg.Bootstrap.Providers = []config.BootstrapProvider{{
		Slug:     "prov",
		Name:     "Provider",
		Protocol: "chat",
		Endpoint: "https://example.invalid/v1",
		Credentials: []config.BootstrapCredential{{
			Label:     "main",
			APIKey:    "explicit-config-key",
			APIKeyEnv: envName,
		}},
	}}
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := bootstrapDB(context.Background(), db, cfg, logger, masterKey); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	creds, err := db.ListCredentials(context.Background(), "prov")
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("credentials = %d, want 1: explicit api_key must remain usable", len(creds))
	}
	plain, err := crypto.Decrypt(creds[0].APIKeyEnc, masterKey)
	if err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	if got := string(plain); got != "explicit-config-key" {
		t.Fatalf("stored API key = %q, want explicit-config-key", got)
	}
}
