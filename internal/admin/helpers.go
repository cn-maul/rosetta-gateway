package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

// writeServerError 回 500，但**不**把底层错误原文回显给调用方。
//
// 旧实现一律 writeError(w, 500, err.Error())，等于把 SQLite 的约束名与语句片段、
// 乃至凭据文件的绝对路径，交给任何一个能碰到管理端点的人 —— 浏览器扩展、
// 站在屏幕后面的人、被截图转发的聊天记录，都算。
// 细节进日志；调用方只拿到一句通用错误。
func writeServerError(w http.ResponseWriter, context string, err error) {
	slog.Error("admin api internal error", "context", context, "error", err)
	writeError(w, http.StatusInternalServerError, "服务内部错误，请查看网关日志")
}

// requireProvider 确认路径里的 provider 存在；不存在时已写出响应并返回 false。
//
// 子资源端点不校验父资源会得到两种糟糕的结果：Create 撞外键 → 500 + 裸
// "FOREIGN KEY constraint failed"（本该是 404）；List 返回 200 + []，让一个拼错的
// provider_id 看起来像「这个上游还没配模型」。
func requireProvider(w http.ResponseWriter, r *http.Request, st *store.Store, providerID string) bool {
	p, err := st.GetProvider(r.Context(), providerID)
	if err != nil {
		writeServerError(w, "get provider", err)
		return false
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return false
	}
	return true
}

// writeNotFoundOrError 处理「按 id 操作」的失败：
// ErrNotFound → 404，其余 → 500（且不回显底层错误）。
//
// 旧实现把「查不到」与「查不动」混在一起写 `if err != nil || x == nil { 404 }`：
// 磁盘故障、数据库锁冲突都会让前端显示「资源不存在」，运维于是去删库重建配置。
func writeNotFoundOrError(w http.ResponseWriter, action, notFoundMsg string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, notFoundMsg)
	case err != nil:
		writeServerError(w, action, err)
	default:
		writeError(w, http.StatusNotFound, notFoundMsg)
	}
}

// writeDeleteError 把删除失败映射成合适的状态码。
//
// 外键 RESTRICT（还有 route / 上游模型引用它）不是服务器故障，是「先清依赖」。
// 旧实现一律 500 + 裸 SQL 错误，用户只知道「失败了」，不知道怎么解决。
func writeDeleteError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "记录不存在")
	case store.IsForeignKeyViolation(err):
		writeError(w, http.StatusConflict, "该记录仍被其它配置引用，请先删除引用它的上游模型与路由")
	default:
		writeServerError(w, action, err)
	}
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- PATCH 部分更新语义（2026-09-21 重构）----------
//
// PATCH 请求体里的标量字段一律用指针表示「是否提供」：
//   - nil  → 客户端未提供该字段，保持数据库原值
//   - 非nil → 显式赋新值，此时空串 / 0 都是**合法值**（会真正落库 / 落 NULL）
//
// 旧实现用 `if req.X != ""` / `if req.X != 0` 判断，导致
// 「priority 改回 0」「清空兜底路由」「超时重置为全局默认」等操作在界面上无效。
//
// derefStr / derefInt 只用于 Create 这类「缺省即零值」的场合；
// Update 必须写 `if req.X != nil`，否则显式置 0 会被误判为未提供。

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// derefBool 用于 Create 这类「缺省即 false」的场合；
// Update 里必须写 `if req.X != nil` 才能区分「未提供」与「显式置 false」。
func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// encryptSecret 在有主密钥时加密，否则退化为明文存储，
// 保证未配置主密钥时凭据仍可用。
func encryptSecret(plain string, masterKey []byte) ([]byte, error) {
	if masterKey == nil {
		return []byte(plain), nil
	}
	return crypto.Encrypt([]byte(plain), masterKey)
}

// decryptSecret 与 upstream.decryptCredentialKey 共用同一套兜底：
// 解不开时按明文处理，兼容早期无主密钥时代的落库数据。
func decryptSecret(enc []byte, masterKey []byte) (string, error) {
	return crypto.DecryptWithFallback(enc, masterKey)
}
