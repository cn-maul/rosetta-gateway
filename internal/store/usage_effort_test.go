package store

import (
	"context"
	"testing"
)

// 2026-10-31：usage_records 新增 effort 列（实际下发的思考档位）。
//
// # 为什么这个字段值得两条独立的用例
//
// 写入路径有两条（单条 CreateUsageRecordWithCost / 批量
// CreateUsageRecordsBatched），各有一份 28→29 个占位符的列清单与参数表。
// 两者只按**位置**对应，加列时漏改一处不会编译失败：SQL 能跑、行能进库，
// 只有 effort 悄悄变成别的列的值（通常是 NULL 或 route_trace 的一截）。
// 所以这里对两条路径分别断言「写进去什么、读出来就是什么」，并且刻意让
// effort 的值与相邻列（request_ip / route_trace）不同，否则错位看不出来。

// seedEffortKey 造一条 access_key：usage_records.access_key_id 有外键。
func seedEffortKey(t *testing.T, st *Store) {
	t.Helper()
	if err := st.CreateAccessKey(context.Background(), &AccessKey{
		ID: "kEff", KeyHash: "hashEffort", KeyPrefix: "sk-eff", Name: "E", Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
}

// effortOf 读回一条记录的 effort 列。
func effortOf(t *testing.T, st *Store, id string) string {
	t.Helper()
	var got string
	if err := st.read.QueryRow(
		`SELECT COALESCE(effort, '') FROM usage_records WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read effort of %s: %v", id, err)
	}
	return got
}

// effortRec 造一条 effort 与相邻列都不同的记录，错位时一眼可见。
func effortRec(id, effort string) *UsageRecord {
	return &UsageRecord{
		ID: id, AccessKeyID: "kEff", PublicModel: "code", ProviderID: "p1",
		UpstreamModel: "cn:glm", IngressProtocol: "chat",
		TotalTokens: 10, UsageState: "reported", Status: "ok", HTTPStatus: 200,
		RequestID:  "rq-" + id,
		RequestIP:  "192.168.1.25",
		Effort:     effort,
		RouteTrace: "1:p1/cn:glm -> ok",
	}
}

func TestUsageRecord_EffortRoundTripsSingleRow(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)

	if err := st.CreateUsageRecord(context.Background(), effortRec("single", "high")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := effortOf(t, st, "single"); got != "high" {
		t.Fatalf("effort = %q，期望 high —— 单条 INSERT 的列顺序与参数表错位了", got)
	}
}

func TestUsageRecord_EffortRoundTripsBatched(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)

	recs := []*UsageRecord{
		effortRec("b1", "low"),
		effortRec("b2", ""), // 空串必须落成 NULL/空，不能借用上一行的值
		effortRec("b3", "budget:8192"),
	}
	if _, err := st.CreateUsageRecordsBatched(context.Background(), recs); err != nil {
		t.Fatalf("batched insert: %v", err)
	}
	want := map[string]string{"b1": "low", "b2": "", "b3": "budget:8192"}
	for id, w := range want {
		if got := effortOf(t, st, id); got != w {
			t.Fatalf("%s 的 effort = %q，期望 %q —— 批量 INSERT 的列顺序与参数表错位了", id, got, w)
		}
	}
}

// 两条写入路径写同一个值必须得到同一个结果：这是「批量与单条等价」这条
// 不变量在新增列上的具体形态。
func TestUsageRecord_EffortAgreesBetweenInsertPaths(t *testing.T) {
	st := testStore(t, t.TempDir()+"/gw.db")
	seedEffortKey(t, st)
	ctx := context.Background()

	if err := st.CreateUsageRecord(ctx, effortRec("one", "medium")); err != nil {
		t.Fatalf("single: %v", err)
	}
	if _, err := st.CreateUsageRecordsBatched(ctx, []*UsageRecord{effortRec("many", "medium")}); err != nil {
		t.Fatalf("batched: %v", err)
	}
	if a, b := effortOf(t, st, "one"), effortOf(t, st, "many"); a != b {
		t.Fatalf("单条写入 effort=%q 而批量写入 effort=%q —— 两条路径的列顺序不一致", a, b)
	}
}
