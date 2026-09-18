package admin

import (
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type CredentialHandler struct {
	store     *store.Store
	masterKey []byte
}

func NewCredentialHandler(st *store.Store, masterKey []byte) *CredentialHandler {
	return &CredentialHandler{store: st, masterKey: masterKey}
}

type credentialRequest struct {
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Weight   int    `json:"weight"`
	Enabled  *bool  `json:"enabled"`
}

type credentialResponse struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Label      string `json:"label"`
	APIKeyMask string `json:"api_key_mask"`
	Enabled    bool   `json:"enabled"`
	Weight     int    `json:"weight"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
}

func (h *CredentialHandler) List(w http.ResponseWriter, r *http.Request, providerID string) {
	creds, err := h.store.ListCredentials(r.Context(), providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		result = append(result, toCredentialResponse(c))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *CredentialHandler) Create(w http.ResponseWriter, r *http.Request, providerID string) {
	var req credentialRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}

	enc, err := crypto.Encrypt([]byte(req.APIKey), h.masterKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt key: "+err.Error())
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	weight := req.Weight
	if weight == 0 {
		weight = 1
	}

	c := &store.Credential{
		ID:         generateID(),
		ProviderID: providerID,
		Label:      req.Label,
		APIKeyEnc:  enc,
		Enabled:    enabled,
		Weight:     weight,
		Status:     "healthy",
	}

	if err := h.store.CreateCredential(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, toCredentialResponse(*c))
}

func (h *CredentialHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetCredential(r.Context(), id)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}

	var req credentialRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Label != "" {
		existing.Label = req.Label
	}
	if req.APIKey != "" {
		enc, err := crypto.Encrypt([]byte(req.APIKey), h.masterKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt key")
			return
		}
		existing.APIKeyEnc = enc
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Weight != 0 {
		existing.Weight = req.Weight
	}

	if err := h.store.UpdateCredential(r.Context(), id, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toCredentialResponse(*existing))
}

func (h *CredentialHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.DeleteCredential(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func toCredentialResponse(c store.Credential) credentialResponse {
	mask := ""
	if len(c.APIKeyEnc) > 0 {
		mask = "encrypted"
	}
	return credentialResponse{
		ID:         c.ID,
		ProviderID: c.ProviderID,
		Label:      c.Label,
		APIKeyMask: mask,
		Enabled:    c.Enabled,
		Weight:     c.Weight,
		Status:     c.Status,
		CreatedAt:  c.CreatedAt,
	}
}
