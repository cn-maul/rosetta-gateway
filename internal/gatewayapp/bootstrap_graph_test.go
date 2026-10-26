package gatewayapp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func bootstrapGraphFixture(t *testing.T) (*store.Store, *config.Config, *slog.Logger, []byte) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	cfg.Bootstrap.Providers = []config.BootstrapProvider{{
		Slug:     "alpha",
		Name:     "Alpha",
		Protocol: "chat",
		Endpoint: "https://alpha.example/v1",
		Credentials: []config.BootstrapCredential{{
			Label:  "default",
			APIKey: "sk-alpha",
		}},
		Models: []string{"alpha-model"},
	}}
	cfg.Bootstrap.Routes = []config.BootstrapRoute{{
		PublicName: "alpha-public",
		Provider:   "alpha",
		Model:      "alpha-model",
	}}
	return db, cfg, logger, []byte("0123456789abcdef0123456789abcdef")
}

// R7 failure-first: a provider-list read failure must not be treated as an
// empty database and followed by bootstrap writes.
func TestR7_BootstrapReadFailureDoesNotWritePartialGraph(t *testing.T) {
	db, cfg, logger, key := bootstrapGraphFixture(t)
	ctx := context.Background()
	if _, err := db.DB().ExecContext(ctx,
		`ALTER TABLE providers RENAME TO providers_bootstrap_read_failure`); err != nil {
		t.Fatalf("inject read failure: %v", err)
	}
	// Keep the foreign-key target intact while making the production read fail:
	// the bootstrap command uses the write pool so the renamed table is the
	// same state a damaged schema presents to startup.
	err := bootstrapDB(ctx, db, cfg, logger, key)
	if err == nil {
		t.Fatal("bootstrap succeeded after its provider-state read failed")
	}
	if !strings.Contains(err.Error(), "read providers") {
		t.Fatalf("bootstrap error = %v, want explicit read failure", err)
	}
}

// R7 failure-first: failure at any child write must roll back the whole graph.
func TestR7_BootstrapChildFailureLeavesNoPartialGraph(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
	}{
		{"network", `CREATE TRIGGER fail_network BEFORE INSERT ON networks
			BEGIN SELECT RAISE(ABORT, 'injected network failure'); END`},
		{"provider", `CREATE TRIGGER fail_provider BEFORE INSERT ON providers
			BEGIN SELECT RAISE(ABORT, 'injected provider failure'); END`},
		{"credential", `CREATE TRIGGER fail_credential BEFORE INSERT ON provider_credentials
			BEGIN SELECT RAISE(ABORT, 'injected credential failure'); END`},
		{"model", `CREATE TRIGGER fail_model BEFORE INSERT ON upstream_models
			BEGIN SELECT RAISE(ABORT, 'injected model failure'); END`},
		{"route", `CREATE TRIGGER fail_route BEFORE INSERT ON routes
			BEGIN SELECT RAISE(ABORT, 'injected route failure'); END`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cfg, logger, key := bootstrapGraphFixture(t)
			if tc.name == "network" {
				cfg.Bootstrap.Providers[0].ProxyURL = "http://127.0.0.1:7897"
			}
			ctx := context.Background()
			if _, err := db.DB().ExecContext(ctx, tc.ddl); err != nil {
				t.Fatalf("inject child failure: %v", err)
			}
			if err := bootstrapDB(ctx, db, cfg, logger, key); err == nil {
				t.Fatalf("bootstrap succeeded despite injected %s failure", tc.name)
			}

			providers, err := db.ListProviders(ctx)
			if err != nil {
				t.Fatalf("list providers after rollback: %v", err)
			}
			routes, err := db.ListRoutes(ctx)
			if err != nil {
				t.Fatalf("list routes after rollback: %v", err)
			}
			models, err := db.ListAllUpstreamModels(ctx)
			if err != nil {
				t.Fatalf("list models after rollback: %v", err)
			}
			if len(providers) != 0 || len(routes) != 0 || len(models) != 0 {
				t.Fatalf("partial graph survived %s failure: providers=%d routes=%d models=%d",
					tc.name, len(providers), len(routes), len(models))
			}
			completed, err := db.ConfigBootstrapCompleted(ctx)
			if err != nil {
				t.Fatalf("read bootstrap marker: %v", err)
			}
			if completed {
				t.Fatalf("bootstrap completion marker committed after %s failure", tc.name)
			}
		})
	}
}

// R7 acceptance: a complete graph is committed with one marker, and a restart
// with an existing complete marker is an idempotent no-op.
func TestR7_BootstrapCommitsCompleteGraphAndCompletionMarker(t *testing.T) {
	db, cfg, logger, key := bootstrapGraphFixture(t)
	ctx := context.Background()
	if err := bootstrapDB(ctx, db, cfg, logger, key); err != nil {
		t.Fatalf("bootstrap complete graph: %v", err)
	}
	completed, err := db.ConfigBootstrapCompleted(ctx)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !completed {
		t.Fatal("complete bootstrap did not write its completion marker")
	}
	adminSetup, err := db.BootstrapCompleted(ctx)
	if err != nil {
		t.Fatalf("read administrator setup marker: %v", err)
	}
	if adminSetup {
		t.Fatal("provider bootstrap wrote the unrelated administrator setup marker")
	}

	providers, err := db.ListProviders(ctx)
	if err != nil || len(providers) != 1 {
		t.Fatalf("providers after bootstrap = %d, err=%v", len(providers), err)
	}
	models, err := db.ListAllUpstreamModels(ctx)
	if err != nil || len(models) != 1 {
		t.Fatalf("models after bootstrap = %d, err=%v", len(models), err)
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes after bootstrap = %d, err=%v", len(routes), err)
	}
	creds, err := db.ListCredentials(ctx, providers[0].ID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("credentials after bootstrap = %d, err=%v", len(creds), err)
	}

	before := len(providers) + len(models) + len(routes) + len(creds)
	if err := bootstrapDB(ctx, db, cfg, logger, key); err != nil {
		t.Fatalf("idempotent bootstrap restart: %v", err)
	}
	providers, _ = db.ListProviders(ctx)
	models, _ = db.ListAllUpstreamModels(ctx)
	routes, _ = db.ListRoutes(ctx)
	if got := len(providers) + len(models) + len(routes); got != before-1 {
		t.Fatalf("bootstrap restart duplicated the graph: before=%d after=%d", before, got)
	}
}

// R7 acceptance: an existing non-empty graph without a completion marker is
// incomplete, not a reason to silently skip repair.
func TestR7_ExistingGraphWithoutMarkerIsNotReportedComplete(t *testing.T) {
	db, cfg, logger, key := bootstrapGraphFixture(t)
	ctx := context.Background()
	p := &store.Provider{
		ID: "legacy", Slug: "legacy", Name: "Legacy", Protocol: "chat",
		Endpoint: "https://legacy.example/v1", Enabled: true,
	}
	if err := db.CreateProvider(ctx, p); err != nil {
		t.Fatalf("seed legacy provider: %v", err)
	}

	err := bootstrapDB(ctx, db, cfg, logger, key)
	if err == nil {
		t.Fatal("bootstrap treated a non-empty graph without a marker as complete")
	}
	if !strings.Contains(err.Error(), "completion marker") {
		t.Fatalf("bootstrap error = %v, want incomplete-completion-marker diagnosis", err)
	}
}

// R7 upgrade fixture: a graph written by the old non-transactional bootstrap
// is complete but lacks the new marker. Startup verifies it and backfills only
// the config marker; it must not rewrite the graph or touch admin setup state.
func TestR7_CompleteLegacyGraphIsVerifiedAndMarked(t *testing.T) {
	db, cfg, logger, _ := bootstrapGraphFixture(t)
	ctx := context.Background()
	if err := db.CreateProvider(ctx, &store.Provider{
		ID: "alpha", Slug: "alpha", Name: "Alpha", Protocol: "chat",
		Endpoint: "https://alpha.example/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := db.CreateCredential(ctx, &store.Credential{
		ID: "cred", ProviderID: "alpha", Label: "default",
		APIKeyEnc: []byte("encrypted"), Enabled: true,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if err := db.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "model", ProviderID: "alpha", ModelID: "alpha-model",
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := db.CreateRoute(ctx, &store.Route{
		ID: "route", PublicName: "alpha-public", ProviderID: "alpha",
		UpstreamModelID: "model", Enabled: true,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	if err := bootstrapDB(ctx, db, cfg, logger, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("verify legacy graph: %v", err)
	}
	completed, err := db.ConfigBootstrapCompleted(ctx)
	if err != nil || !completed {
		t.Fatalf("config marker = %v, err=%v; want verified backfill", completed, err)
	}
	adminSetup, err := db.BootstrapCompleted(ctx)
	if err != nil {
		t.Fatalf("read admin marker: %v", err)
	}
	if adminSetup {
		t.Fatal("legacy provider verification touched the admin bootstrap marker")
	}
	providers, err := db.ListProviders(ctx)
	if err != nil || len(providers) != 1 {
		t.Fatalf("legacy graph rewritten: %d providers, err=%v", len(providers), err)
	}
}

func TestR7_BootstrapEncryptFailureRollsBackEarlierWrites(t *testing.T) {
	db, cfg, logger, _ := bootstrapGraphFixture(t)
	// A wrong-sized master key is rejected before any graph write.
	err := bootstrapDB(context.Background(), db, cfg, logger, []byte("short"))
	if err == nil {
		t.Fatal("bootstrap accepted an unusable master key")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Fatalf("bootstrap error = %v, want a key-material failure", err)
	}
	providers, listErr := db.ListProviders(context.Background())
	if listErr != nil {
		t.Fatalf("list providers: %v", listErr)
	}
	if len(providers) != 0 {
		t.Fatalf("providers = %d after key failure, want 0", len(providers))
	}
}
