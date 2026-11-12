package store

import (
	"context"
	"testing"
)

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
	rows, err := st.read.Query(`SELECT ts FROM audit_log ORDER BY id DESC LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var newest []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			t.Fatal(err)
		}
		newest = append(newest, ts)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for i, ts := range newest {
		if want := int64(total - i); ts != want {
			t.Fatalf("第 %d 条应为最新的一条 ts=%d，实际 ts=%d（剪反了方向？）", i, want, ts)
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
