package probe

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func TestR1_ProbeRunnerStopsDuringStaggerBeforeWriting(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir()+"/gw.db", logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	provider := &store.Provider{
		ID: "p1", Slug: "p1", Name: "p1", Protocol: "chat",
		Endpoint: "http://127.0.0.1:1/v1", Enabled: true,
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO providers (id, slug, name, protocol, endpoint, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
		provider.ID, provider.Slug, provider.Name, provider.Protocol,
		provider.Endpoint, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if err := st.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "m1", ProviderID: "p1", ModelID: "model",
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}

	cfg := config.Default()
	cfg.Probes.Enabled = true
	cfg.Probes.Concurrency = 1
	runner := NewRunner(st, cfg, []byte("0123456789abcdef0123456789abcdef"), logger)
	runner.stagger = time.Hour

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(runCtx)
	}()

	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("probe runner remained in stagger sleep after cancellation")
	}

	var count int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_probes`).Scan(&count); err != nil {
		t.Fatalf("count probes: %v", err)
	}
	if count != 0 {
		t.Fatalf("probe rows = %d after cancellation during stagger, want 0", count)
	}
}
