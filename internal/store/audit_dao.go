package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AuditEntry 是一条管理后台写操作的审计记录（DESIGN §13.3）。
//
// 刻意只存**字段名**而不存请求体值：body 里会出现 api_key（上游凭据明文）、
// 管理密码（密码修改）——审计的价值在「谁在什么时候动了什么资源」，
// 不在值本身；值泄露的风险远大于排查收益。
// actor 目前恒为 "admin"：审计回调（WriteAuditor）没有透传会话身份，
// 多用户后普通用户对自己资源的写操作也会落一条 "admin" 记录 ——
// 已知的不精确；要修就让 WriteAuditor 把调用者身份带进来。
// remote 记来源 IP。
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
		// fields 落库时走 nullIfEmpty：没有请求体（GET / DELETE，或 body 为
		// 空）的记录存的是 NULL 而不是 ''。直接扫进 string 会报
		// "converting NULL to string is unsupported"，**整页查询失败** ——
		// 而只要有任意一条这类记录就会触发，等于审计日志页对所有部署恒 500。
		var fields sql.NullString
		if err := rows.Scan(&e.ID, &e.Ts, &e.Actor, &e.Remote, &e.Method, &e.Path, &e.Status, &fields); err != nil {
			return nil, err
		}
		e.Fields = fields.String
		result = append(result, e)
	}
	return result, rows.Err()
}

// PruneAuditLog 只保留最近 keepRows 条审计记录，返回删除行数。
//
// **按行数而不是按天**：本表的唯一读路径 ListAuditEntries 是
// `ORDER BY id DESC LIMIT ?`，管理面永远只看最近几十条，保留更早的行对
// 可观测性零贡献，却让表无界增长（写操作每次一条，配上自动重试与批量导入
// 配置，几十 MB 起步）。用行数封顶同时也让磁盘占用有确定上界。
//
// 与 PruneOldUsage / PruneOldProbes 分开：这两张表的下游还有聚合与
// 连通率趋势图，超期数据会被折叠进 rollup，而审计日志没有下游消费者。
func (s *Store) PruneAuditLog(ctx context.Context, keepRows int) (int64, error) {
	if keepRows <= 0 {
		return 0, nil
	}
	// 保留**最新**的 keepRows 条，删掉更早的。
	//
	// 阈值取第 keepRows 条（从新往旧数）的 id，删掉 `id <` 阈值的行。
	// 两个容易写错的点：
	//
	//  1. **参数必须是 keepRows-1**。`LIMIT 1 OFFSET k` 取的是第 k+1 条
	//     （第 1 条是 OFFSET 0），所以第 keepRows 条对应 OFFSET keepRows-1。
	//     写成 keepRows 会取到第 keepRows+1 条，多留下一条 —— 实测
	//     keepRows=100 剪完剩 101 行，正是这个 off-by-one。
	//  2. 比较必须是 `<` 而不是 `>`。反过来写会把**最新**的 keepRows 条删掉，
	//     保留旧数据 —— 审计日志要留的恰恰是新记录，这个方向的 bug 比不剪更糟。
	//
	// 行数**正好等于** keepRows 时子查询无行 → `id < NULL` 为 NULL → 不匹配
	// 任何行 → 删 0 行；不足 keepRows 时同理。两种情况都天然幂等。
	off := keepRows - 1
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM audit_log WHERE id < (
			SELECT id FROM audit_log ORDER BY id DESC LIMIT 1 OFFSET ?
		)`, off)
	if err != nil {
		return 0, fmt.Errorf("prune audit_log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
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
