package server

import "net/http"

// DenyAdminKeyCreate blocks administrators from minting data-plane API keys.
// The route-level policy is kept separate from KeyHandler.Create so ordinary
// user self-service behavior remains unchanged.
func DenyAdminKeyCreate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := UserFromContext(r.Context())
		if u == nil || u.ID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"需要登录","type":"auth_error"}}`))
			return
		}
		if u.IsAdmin() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"管理员账号不能创建 API key：请新建一个普通用户账户，由该用户自行创建 key 后用于调用模型","type":"auth_error"}}`))
			return
		}
		next(w, r)
	}
}
