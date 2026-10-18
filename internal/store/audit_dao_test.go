package store

import (
	"context"
	"testing"
)

// TestAudit_ListHandlesNullFields 覆盖一个会**整页 500** 的线上缺陷。
//
// CreateAuditEntry 对空 fields 写的是 NULL（nullIfEmpty），而没有请求体的
// 管理请求（GET / DELETE、空 body 的写）全都落这一类。此前 ListAuditEntries
// 直接把 fields 扫进 string，遇到任何一条 NULL 行就返回
// "converting NULL to string is unsupported" —— 只要表里存在一条这样的记录，
// 审计日志页对所有部署恒定失败。
func TestAudit_ListHandlesNullFields(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/audit.db")

	// 一条有 fields、一条没有（触发 NULL），顺序不影响结果。
	if err := st.CreateAuditEntry(ctx, &AuditEntry{Ts: 1, Actor: "admin", Method: "POST", Path: "/admin/x", Status: 200, Fields: "a,b"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAuditEntry(ctx, &AuditEntry{Ts: 2, Actor: "admin", Method: "DELETE", Path: "/admin/y", Status: 204}); err != nil {
		t.Fatal(err)
	}

	entries, err := st.ListAuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("含 NULL fields 的记录不应让整页查询失败：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(entries))
	}
	// 新→旧：空 fields 的那条在前面，且 Fields 被归一化成空串（而不是残留）。
	if entries[0].Fields != "" {
		t.Fatalf("NULL fields 应归一化为空串，实际 %q", entries[0].Fields)
	}
	if entries[0].Method != "DELETE" {
		t.Fatalf("未按 id 倒序，第一条应是较新的 DELETE，实际 %q", entries[0].Method)
	}
	if entries[1].Fields != "a,b" {
		t.Fatalf("有 fields 的记录应原样返回，实际 %q", entries[1].Fields)
	}
}

// TestAudit_PruneKeepsNewest 钉住 PruneAuditLog 的方向与 off-by-one。
//
// 两次写反都真实发生过：OFFSET 传 keepRows 会多留一条（100 → 101 行），
// 而把 `<` 写成 `>` 保留旧数据、删掉最新记录 —— 后者比不剪更糟，
// 因为审计日志的价值全在最近的记录上。
func TestAudit_PruneKeepsNewest(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/audit.db")

	const total = 500
	for i := 1; i <= total; i++ {
		if err := st.CreateAuditEntry(ctx, &AuditEntry{Ts: int64(i), Actor: "admin", Method: "POST", Path: "/admin/x", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	const keep = 100
	n, err := st.PruneAuditLog(ctx, keep)
	if err != nil {
		t.Fatal(err)
	}
	if n != total-keep {
		t.Fatalf("应删 %d 条，实际 %d", total-keep, n)
	}

	var cnt int
	if err := st.read.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != keep {
		t.Fatalf("剪完应恰好剩 %d 行，实际 %d（off-by-one 会剩 %d）", keep, cnt, keep+1)
	}

	// 留下的必须是**最新**的那些。
	entries, err := st.ListAuditEntries(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		if want := total - i; e.Ts != int64(want) {
			t.Fatalf("第 %d 条应为最新的一条 ts=%d，实际 ts=%d（剪反了方向？）", i, want, e.Ts)
		}
	}

	// 幂等：再剪一次不删任何行（行数已等于 keepRows 时子查询无结果）。
	n2, err := st.PruneAuditLog(ctx, keep)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("重复剪枝应删 0 行，实际 %d", n2)
	}

	// keepRows <= 0 是显式「永久留存」，不删任何行。
	if n3, err := st.PruneAuditLog(ctx, -1); err != nil || n3 != 0 {
		t.Fatalf("keepRows=-1 应不删任何行，实际 n=%d err=%v", n3, err)
	}
}
