package admin

import (
	"context"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// R7 / M12-02 regression: preview and actual import must agree when the same
// model_id appears under two new providers.
func TestR7_DryRunAndImportAgreeOnDuplicateModelsAcrossNewProviders(t *testing.T) {
	h, _ := transferFixture(t)
	data := configExport{
		Version: exportFormatVersion,
		Providers: []providerExport{
			{Slug: "alpha", Name: "Alpha", Protocol: "chat", Endpoint: "https://alpha.example/v1", Enabled: true},
			{Slug: "beta", Name: "Beta", Protocol: "chat", Endpoint: "https://beta.example/v1", Enabled: true},
		},
		Models: []modelExport{
			{ProviderSlug: "alpha", ModelID: "shared-model"},
			{ProviderSlug: "beta", ModelID: "shared-model"},
		},
	}

	preview := importInto(t, h, importRequest{Data: data, DryRun: true})
	if preview.ProvidersCreated != 2 || preview.ModelsCreated != 2 {
		t.Fatalf("dry-run cardinality = providers=%d models=%d, want 2/2",
			preview.ProvidersCreated, preview.ModelsCreated)
	}
	providers, err := h.store.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("list providers after dry-run: %v", err)
	}
	models, err := h.store.ListAllUpstreamModels(context.Background())
	if err != nil {
		t.Fatalf("list models after dry-run: %v", err)
	}
	if len(providers) != 0 || len(models) != 0 {
		t.Fatalf("dry-run wrote state: providers=%d models=%d", len(providers), len(models))
	}

	actual := importInto(t, h, importRequest{Data: data})
	if actual.ProvidersCreated != preview.ProvidersCreated ||
		actual.ModelsCreated != preview.ModelsCreated {
		t.Fatalf("preview/import divergence: preview=%+v actual=%+v", preview, actual)
	}
	models, err = h.store.ListAllUpstreamModels(context.Background())
	if err != nil {
		t.Fatalf("list models after import: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("imported models = %d, want 2", len(models))
	}
}

// R7 shared cardinality rule: an existing provider/model pair is skipped in
// both preview and import, while a same-named model under a different provider
// remains a distinct candidate.
func TestR7_ModelCardinalityIsProviderScoped(t *testing.T) {
	h, _ := transferFixture(t)
	ctx := context.Background()
	existing := &store.Provider{
		ID: "existing", Slug: "alpha", Name: "Alpha", Protocol: "chat",
		Endpoint: "https://alpha.example/v1", Enabled: true,
	}
	if err := h.store.CreateProvider(ctx, existing); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := h.store.CreateUpstreamModel(ctx, &store.UpstreamModel{
		ID: "existing-model", ProviderID: existing.ID, ModelID: "shared-model",
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	data := configExport{
		Version: exportFormatVersion,
		Providers: []providerExport{
			{Slug: "alpha", Name: "Alpha", Protocol: "chat", Endpoint: "https://alpha.example/v1", Enabled: true},
			{Slug: "beta", Name: "Beta", Protocol: "chat", Endpoint: "https://beta.example/v1", Enabled: true},
		},
		Models: []modelExport{
			{ProviderSlug: "alpha", ModelID: "shared-model"},
			{ProviderSlug: "beta", ModelID: "shared-model"},
			{ProviderSlug: "beta", ModelID: "shared-model"},
		},
	}
	preview := importInto(t, h, importRequest{Data: data, DryRun: true})
	if preview.ModelsCreated != 2 {
		t.Fatalf("dry-run models = %d, want 2 (renamed alpha is a new provider; duplicate beta counted once)",
			preview.ModelsCreated)
	}
	actual := importInto(t, h, importRequest{Data: data})
	if actual.ModelsCreated != preview.ModelsCreated {
		t.Fatalf("preview/import divergence: preview=%d actual=%d",
			preview.ModelsCreated, actual.ModelsCreated)
	}
}
