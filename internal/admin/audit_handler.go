package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// AuditHandler 提供管理后台写操作的审计查询（DESIGN §13.3）。
// 审计由 server.AutoReload 在每个成功的写操作上落库，这里只读。
type AuditHandler struct {
	store *store.Store
}

func NewAuditHandler(st *store.Store) *AuditHandler {
	return &AuditHandler{store: st}
}

type auditEntryResponse struct {
	ID     int64  `json:"id"`
	Ts     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Remote string `json:"remote"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Fields string `json:"fields"`
}

// List 返回最近的审计记录，limit 上限 500（审计是排查工具，不是全量导出口）。
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	entries, err := h.store.ListAuditEntries(r.Context(), limit)
	if err != nil {
		writeServerError(w, "list audit", err)
		return
	}
	result := make([]auditEntryResponse, 0, len(entries))
	for _, e := range entries {
		result = append(result, auditEntryResponse{
			ID:     e.ID,
			Ts:     e.Ts,
			Actor:  e.Actor,
			Remote: e.Remote,
			Method: e.Method,
			Path:   e.Path,
			Status: e.Status,
			Fields: e.Fields,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   result,
		"server_ts": time.Now().UnixMilli(),
	})
}
