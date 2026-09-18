package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type ProviderHandler struct {
	store *store.Store
}

func NewProviderHandler(st *store.Store) *ProviderHandler {
	return &ProviderHandler{store: st}
}

type providerRequest struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	Enabled    *bool  `json:"enabled"`
	TimeoutMs  int    `json:"timeout_ms"`
	MaxRetries int    `json:"max_retries"`
	QuirksJSON string `json:"quirks_json"`
}

type providerResponse struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	Enabled    bool   `json:"enabled"`
	TimeoutMs  int    `json:"timeout_ms"`
	MaxRetries int    `json:"max_retries"`
	QuirksJSON string `json:"quirks_json"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (h *ProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	providers, err := h.store.ListProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]providerResponse, 0, len(providers))
	for _, p := range providers {
		result = append(result, toProviderResponse(p))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ProviderHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Slug == "" || req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "slug and endpoint are required")
		return
	}

	id := generateID()
	now := time.Now().UnixMilli()
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	p := &store.Provider{
		ID:         id,
		Slug:       req.Slug,
		Name:       req.Name,
		Protocol:   req.Protocol,
		Endpoint:   req.Endpoint,
		Enabled:    enabled,
		TimeoutMs:  req.TimeoutMs,
		MaxRetries: req.MaxRetries,
		QuirksJSON: req.QuirksJSON,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := h.store.CreateProvider(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, toProviderResponse(*p))
}

func (h *ProviderHandler) Get(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, toProviderResponse(*p))
}

func (h *ProviderHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	var req providerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Slug != "" {
		existing.Slug = req.Slug
	}
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Protocol != "" {
		existing.Protocol = req.Protocol
	}
	if req.Endpoint != "" {
		existing.Endpoint = req.Endpoint
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.TimeoutMs != 0 {
		existing.TimeoutMs = req.TimeoutMs
	}
	if req.MaxRetries != 0 {
		existing.MaxRetries = req.MaxRetries
	}
	if req.QuirksJSON != "" {
		existing.QuirksJSON = req.QuirksJSON
	}

	if err := h.store.UpdateProvider(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toProviderResponse(*existing))
}

func (h *ProviderHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteProvider(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *ProviderHandler) Test(w http.ResponseWriter, r *http.Request, id string) {
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "connectivity test requires upstream pool"})
}

func toProviderResponse(p store.Provider) providerResponse {
	return providerResponse{
		ID:         p.ID,
		Slug:       p.Slug,
		Name:       p.Name,
		Protocol:   p.Protocol,
		Endpoint:   p.Endpoint,
		Enabled:    p.Enabled,
		TimeoutMs:  p.TimeoutMs,
		MaxRetries: p.MaxRetries,
		QuirksJSON: p.QuirksJSON,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = context.Background
