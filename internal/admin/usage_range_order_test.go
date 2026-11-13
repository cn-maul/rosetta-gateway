package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// TestQueryRange_RejectsReversedRange 坐实「反区间必须报错」。
//
// from > to 在 SQL 里恒为空集。此前只有 /stats 拦这条，多个 usage 端点
// 全放行：同一个错误参数在 /stats 报 400、在别处报 200 空表、
// 在 /usage/history.csv 只给一行表头。排查的人会先去怀疑采集、剪枝、时区，
// 唯独不会怀疑自己把两个时间戳填反了 —— 所以这条必须在解析层拦掉。
func TestQueryRange_RejectsReversedRange(t *testing.T) {
	q, _ := url.ParseQuery("from=2000&to=1000")
	if _, _, _, err := queryRangeExplicit(q, time.Hour); err == nil {
		t.Fatal("from>to 必须报错，实际静默返回空集")
	}

	// 只给未来起点、缺 to（=now）同样是反区间。
	future := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	q2, _ := url.ParseQuery("from=" + future)
	if _, _, _, err := queryRangeExplicit(q2, time.Hour); err == nil {
		t.Fatal("起点在未来（to 缺省为 now）也是反区间，必须报错")
	}

	// from=0 是「不设下界」（全部历史），不是时间点，不参与比较。
	q3, _ := url.ParseQuery("from=0&to=1")
	if _, _, _, err := queryRangeExplicit(q3, time.Hour); err != nil {
		t.Fatalf("from=0 表示全部历史，不应被判成反区间: %v", err)
	}
}

// TestUsageEndpoints_RejectReversedRangeWithSingleEnvelope 逐个打端点，
// 要求全部返回 400 且 body 只有一个错误信封（不是空集、不是两个 JSON）。
//
// 覆盖意义在于「一处拦截，全端点生效」：这些 handler 各自解析参数，
// 只有 queryRangeExplicit 这一个公共入口能一次盖住它们。
func TestUsageEndpoints_RejectReversedRangeWithSingleEnvelope(t *testing.T) {
	st := newTestStore(t)
	h := NewUsageHandler(st)
	stats := NewStatsHandler(st)

	// from=2000&to=1000 都是合法时间戳，只是顺序反了。
	const qs = "?from=2000&to=1000"
	hooks := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"usage/history", h.History},
		{"usage/history.csv", h.ExportCSV},
		{"usage/by-day", h.GroupByDay},
		{"usage/by-key", h.GroupByKey},
		{"usage/by-model", h.GroupByModel},
		{"usage/by-provider", h.GroupByProvider},
		{"usage/by-user", h.GroupByUser},
		{"stats", stats.Get},
	}
	for _, hook := range hooks {
		t.Run(hook.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			hook.call(rec, asAdmin(httptest.NewRequest(http.MethodGet,
				"/admin/api/"+hook.name+qs, nil)))
			assertSingleErrorEnvelope(t, rec, http.StatusBadRequest)
		})
	}
}
