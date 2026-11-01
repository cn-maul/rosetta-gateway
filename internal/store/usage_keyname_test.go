package store

import (
	"context"
	"testing"
)

// 2026-11-01：usage_records 新增 key_name 列（调用密钥名，落库时固化）。
//
// # 为什么不能靠 JOIN 现取
//
// 密钥名原先由 `LEFT JOIN access_keys a ON a.id = u.access_key_id` 取
// `a.name`。密钥一删，JOIN 落空，几百行历史从此只剩一串 sha256 id ——
// 而「这个 key 是谁的、叫什么」正是事后追账时要回答的第一个问题。
// provider_name 早就是落库固化的，密钥名照同一口径补上。
//
// # 为什么也要两条路径各测一遍
//
// 同 effort：单条与批量各有一份「列清单 + 参数表」，只按位置对应，加列
// 时漏改一处不会编译失败，只会把这列的值写到隔壁去（这里是 request_ip /
// effort 被挤出来）。所以刻意让 key_name 与相邻列取值互不相同。

func keyNameOf(t *testing.T, st *Store, id string) string {
	t.Helper()
	var got string
	if err := st.read.QueryRow(
		`SELECT COALESCE(key_name, '') FROM usage_records WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read key_name of %s: %v", id, err)
	}
	return got
}

// keyNameRec 造一条 key_name 与相邻列（request_ip / effort / route_trace）
// 都不同的记录，列错位时一眼可见。
func keyNameRec(id, keyName string) *UsageRecord {
	r := effortRec(id, "effort-"+id)
	r.KeyName = keyName
	r.RequestIP = "10.0.0." + id[len(id)-1:]
	r.RouteTrace = "trace-" + id
	return r
}

func TestUsageRecord_KeyNameRoundTripsSingleRow(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)

	if err := st.CreateUsageRecord(context.Background(), keyNameRec("s1", "生产密钥")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := keyNameOf(t, st, "s1"); got != "生产密钥" {
		t.Fatalf("key_name = %q，期望「生产密钥」—— 单条 INSERT 的列顺序与参数表错位了", got)
	}
}

func TestUsageRecord_KeyNameRoundTripsBatched(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)

	recs := []*UsageRecord{
		keyNameRec("k1", "生产密钥"),
		keyNameRec("k2", ""), // 空串必须落成 NULL，不能借用上一行的名字
		keyNameRec("k3", "临时测试"),
	}
	if _, err := st.CreateUsageRecordsBatched(context.Background(), recs); err != nil {
		t.Fatalf("batched insert: %v", err)
	}
	want := map[string]string{"k1": "生产密钥", "k2": "", "k3": "临时测试"}
	for id, w := range want {
		if got := keyNameOf(t, st, id); got != w {
			t.Fatalf("%s 的 key_name = %q，期望 %q —— 批量 INSERT 的列顺序与参数表错位了", id, got, w)
		}
	}
	// 顺带守住邻居列没被挤动：key_name 插在 effort 与 route_trace 之间。
	if got := effortOf(t, st, "k1"); got != "effort-k1" {
		t.Fatalf("加 key_name 之后 effort 读成了 %q —— 列顺序被挤动了", got)
	}
}

// 两条写入路径写同一个名字必须得到同一个结果。
func TestUsageRecord_KeyNameAgreesBetweenInsertPaths(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)
	ctx := context.Background()

	if err := st.CreateUsageRecord(ctx, keyNameRec("a1", "同名")); err != nil {
		t.Fatalf("single: %v", err)
	}
	if _, err := st.CreateUsageRecordsBatched(ctx, []*UsageRecord{keyNameRec("a2", "同名")}); err != nil {
		t.Fatalf("batched: %v", err)
	}
	if a, b := keyNameOf(t, st, "a1"), keyNameOf(t, st, "a2"); a != b {
		t.Fatalf("单条写入 key_name=%q 而批量写入 key_name=%q —— 两条路径的列顺序不一致", a, b)
	}
}
