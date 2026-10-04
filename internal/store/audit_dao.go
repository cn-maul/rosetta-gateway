package store

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// AuditEntry 是一条管理后台写操作的审计记录（DESIGN §13.3）。
//
// 刻意只存**字段名**而不存请求体值：body 里会出现 api_key（上游凭据明文）、
// 管理密码（password/set）——审计的价值在「谁在什么时候动了什么资源」，
// 不在值本身；值泄露的风险远大于排查收益。actor 目前恒为 "admin"
// （管理鉴权是单密码模型，没有多用户身份），remote 记来源 IP。
type AuditEntry struct {
	ID     int64
	Ts     int64
	Actor  string
	Remote string
	Method string
	Path   string
	Status int
	Fields string // 请求体顶层字段名，逗号分隔；GET/无 body 时为空
}

// CreateAuditEntry 追加一条审计记录（写池）。幂等性不要求 —— 审计失败只留
// 日志，不阻塞管理操作（见 server.AutoReload 的调用方）。
func (s *Store) CreateAuditEntry(ctx context.Context, e *AuditEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, remote, method, path, status, fields) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Ts, e.Actor, e.Remote, e.Method, e.Path, e.Status, nullIfEmpty(e.Fields))
	return err
}

// ListAuditEntries 返回最近 limit 条审计记录（新→旧）。
func (s *Store) ListAuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, ts, actor, remote, method, path, status, fields FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AuditEntry, 0)
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Ts, &e.Actor, &e.Remote, &e.Method, &e.Path, &e.Status, &e.Fields); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// AuditFieldNames 从管理请求的 JSON body 提取顶层字段名（排序后逗号分隔）。
// 解析失败（非 JSON、空 body）返回空串 —— 字段名是尽力而为的补充信息，
// 不是审计成立的条件。
func AuditFieldNames(body []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
