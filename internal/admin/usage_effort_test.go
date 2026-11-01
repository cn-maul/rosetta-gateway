package admin

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-31：调用历史要能看出「这次到底用了多强的思考」。
//
// # 为什么要落库而不是从请求参数推
//
// 档位会被模型能力夹取（客户端要 xhigh、模型只支持到 low 时实发 low），
// 也会被 supports_thinking=false 整个剥掉。事后拿原始请求参数去推，得到的
// 是「客户端想要什么」而不是「上游真正收到了什么」—— 而排障时要回答的
// 恰恰是后者。
func seedEffortHistory(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "kEff", KeyHash: "hashEff", KeyPrefix: "sk-e", Name: "E", Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	ts := time.Now().UnixMilli()
	rows := []struct {
		id     string
		effort string
	}{
		// 三档 + 一个「没思考」：空串是最容易在扫描顺序错位时被邻居污染的值。
		{"e-low", "low"},
		{"e-high", "high"},
		{"e-none", ""},
	}
	for _, r := range rows {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: r.id, Ts: ts, AccessKeyID: "kEff", PublicModel: "code",
			ProviderID: "p1", UpstreamModel: "cn:glm", IngressProtocol: "chat",
			Effort: r.effort, TotalTokens: 10, Status: "ok", HTTPStatus: http.StatusOK,
			RequestID:  r.id,
			UsageState: "reported",
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	return ts
}

// 历史接口必须下发 effort，且空串与有值都要能区分。
func TestUsage_History_ExposesEffort(t *testing.T) {
	st := newScopeStore(t)
	ts := seedEffortHistory(t, st)

	resp := fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1))
	if len(resp.Records) != 3 {
		t.Fatalf("期望 3 条记录，实际 %d", len(resp.Records))
	}
	got := map[string]string{}
	for _, e := range resp.Records {
		got[e.RequestID] = e.Effort
	}
	want := map[string]string{"e-low": "low", "e-high": "high", "e-none": ""}
	for id, w := range want {
		gotEffort, ok := got[id]
		if !ok {
			t.Errorf("记录 %s 不在响应里（实际 request_id 集合 %v）", id, keysOf(got))
			continue
		}
		if gotEffort != w {
			t.Errorf("记录 %s 的 effort = %q，期望 %q", id, gotEffort, w)
		}
	}
}

// CSV 导出必须带上 effort 列，且列数要与表头一致。
//
// 断言下标而不是只看有没有：表头与行数据分两处写，加列时只改一处的错法
// 会让每一行少一格，Excel 打开后整表错位而没有任何报错。
func TestUsage_ExportCSV_IncludesEffortColumn(t *testing.T) {
	st := newScopeStore(t)
	ts := seedEffortHistory(t, st)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.ExportCSV(rec, asAdmin(httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/admin/api/usage/history.csv?from=0&to=%d", ts+1), nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("解析 CSV: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("CSV 只有 %d 行（至少要表头 + 数据）", len(rows))
	}
	header := rows[0]
	effortIdx := -1
	for i, name := range header {
		if name == "effort" {
			effortIdx = i
			break
		}
	}
	if effortIdx < 0 {
		t.Fatalf("CSV 表头没有 effort 列：%v", header)
	}
	// effort 曾是最后一列；2026-11-01 之后又追加了 key_deleted /
	// provider_deleted。规则没变 —— **新列一律追加在末尾**，所以这里断言
	// effort 之后只剩下后来新增的那两列，而不是死认「effort 就是最后一列」。
	if len(header)-effortIdx-1 != 2 || header[effortIdx+1] != "key_deleted" || header[effortIdx+2] != "provider_deleted" {
		t.Errorf("effort 之后应只跟着后来追加的 key_deleted/provider_deleted，实际 header 尾部 %v（effort 下标 %d）",
			header[max(0, effortIdx):], effortIdx)
	}
	seen := map[string]bool{}
	for _, row := range rows[1:] {
		if len(row) != len(header) {
			t.Fatalf("数据行列数 %d 与表头 %d 不一致：%v", len(row), len(header), row)
		}
		seen[row[effortIdx]] = true
	}
	if !seen["low"] || !seen["high"] {
		t.Errorf("CSV 的 effort 列没读到 low/high，实际值集合 %v", seen)
	}
}

// keysOf 让失败信息里能看见实际拿到了哪些 request_id。
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
