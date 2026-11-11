package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// seedFailRow 往 usage_records 里插一条可指定失败三件套的记录。
//
// 不走 store.CreateUsageRecord：那条路径会带上计费与触发器副作用，
// 而这里要测的只是聚合口径，直接用 SQL 把「status / http_status /
// error_code」三个变量摆到位即可。
func seedFailRow(t *testing.T, st *store.Store, id, userID, providerID, providerName, status string, httpStatus int, errorCode string, ts int64) {
	t.Helper()
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err := st.DB().Exec(`INSERT INTO usage_records
		(id, ts, access_key_id, user_id, public_model, provider_id, provider_name, upstream_model,
		 ingress_protocol, stream, input_tokens, output_tokens, total_tokens, usage_state,
		 status, http_status, error_code, latency_ms)
		VALUES (?,?, 'k1', ?, 'm', ?, ?, 'up', 'chat', 0, 1, 0, 1, 'reported', ?, ?, ?, 10)`,
		id, ts, uid, providerID, providerName, status, httpStatus, errorCode)
	if err != nil {
		t.Fatalf("seed failure row %s: %v", id, err)
	}
}

func fetchFailures(t *testing.T, st *store.Store, query string, admin bool, userID string) usageFailuresResponse {
	t.Helper()
	h := &UsageHandler{store: st}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/failures"+query, nil)
	if admin {
		req = asAdmin(req)
	} else {
		req = asUser(req, userID)
	}
	rec := httptest.NewRecorder()
	h.Failures(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("failures 返回 %d，body=%s", rec.Code, rec.Body.String())
	}
	var out usageFailuresResponse
	decodeBody(t, rec, &out)
	return out
}

// 聚合要同时回答「什么错最多」和「哪个上游在坏」，且两者不能互相污染。
func TestUsageFailures_BreaksDownByCodeAndProvider(t *testing.T) {
	st := newScopeStore(t)
	ts := int64(1_700_000_000_000)

	seedFailRow(t, st, "ok-1", "u1", "p1", "deepseek", store.UsageStatusOK, 200, "", ts)
	seedFailRow(t, st, "bad-1", "u1", "p1", "deepseek", store.UsageStatusError, 502, "upstream_error", ts+10)
	seedFailRow(t, st, "bad-2", "u1", "p1", "deepseek", store.UsageStatusError, 502, "upstream_error", ts+20)
	seedFailRow(t, st, "bad-3", "u2", "p2", "backup", store.UsageStatusTimeout, 504, "gateway_timeout", ts+30)
	// canceled 是客户端自己断开，不是上游问题：计入总数、不计入失败。
	seedFailRow(t, st, "cancel-1", "u1", "p1", "deepseek", store.UsageStatusCanceled, 499, "", ts+40)

	got := fetchFailures(t, st, fmt.Sprintf("?from=0&to=%d", ts+1000), true, "")

	if got.Total != 5 {
		t.Errorf("total = %d，期望 5（含 canceled）", got.Total)
	}
	if got.Failed != 3 {
		t.Errorf("failed = %d，期望 3（不含 ok 与 canceled）", got.Failed)
	}
	if want := 3.0 / 5.0; got.Rate != want {
		t.Errorf("rate = %v，期望 %v", got.Rate, want)
	}

	// 按错误码：两个 502 upstream_error 合成一行，504 单独一行。
	if len(got.Codes) != 2 {
		t.Fatalf("codes 行数 = %d，期望 2：%+v", len(got.Codes), got.Codes)
	}
	top := got.Codes[0]
	if top.Code != "upstream_error" || top.HTTPStatus != 502 || top.Count != 2 {
		t.Errorf("第一行 = %+v，期望 upstream_error/502/2", top)
	}
	if top.LastTs != ts+20 {
		t.Errorf("last_ts = %d，期望 %d（该组最近一次）", top.LastTs, ts+20)
	}

	// 按上游：p1 有 3 次请求（1 ok + 2 失败），失败率 2/3；
	// p2 有 1 次请求且失败，失败率 1。
	//
	// 分母必须是「该上游全部请求」而不是失败数 —— 拿失败数当分母恒等于 1，
	// 那样这一列就没有任何信息量。
	if len(got.Upstreams) != 2 {
		t.Fatalf("upstreams 行数 = %d，期望 2：%+v", len(got.Upstreams), got.Upstreams)
	}
	first := got.Upstreams[0]
	if first.ProviderID != "p1" || first.Total != 4 || first.Failed != 2 {
		t.Errorf("第一个上游 = %+v，期望 p1/4 次/2 失败（canceled 计入 total）", first)
	}
	if want := 2.0 / 4.0; first.Rate != want {
		t.Errorf("p1 rate = %v，期望 %v", first.Rate, want)
	}
	if second := got.Upstreams[1]; second.ProviderID != "p2" || second.Rate != 1 {
		t.Errorf("第二个上游 = %+v，期望 p2 且失败率 1", second)
	}
}

// error_code 为空的老记录不能挤成一行空 key：回退成 status，仍然成组可读。
func TestUsageFailures_FallsBackToStatusWhenCodeMissing(t *testing.T) {
	st := newScopeStore(t)
	ts := int64(1_700_000_000_000)

	seedFailRow(t, st, "old-1", "u1", "p1", "deepseek", store.UsageStatusTruncated, 200, "", ts)
	seedFailRow(t, st, "old-2", "u1", "p1", "deepseek", store.UsageStatusTruncated, 200, "", ts+1)

	got := fetchFailures(t, st, fmt.Sprintf("?from=0&to=%d", ts+100), true, "")
	if len(got.Codes) != 1 || got.Codes[0].Code != store.UsageStatusTruncated || got.Codes[0].Count != 2 {
		t.Fatalf("codes = %+v，期望一行 truncated/2", got.Codes)
	}
}

// 普通用户只看得到自己的失败，管理员看全站。
func TestUsageFailures_ScopesToCaller(t *testing.T) {
	st := newScopeStore(t)
	ts := int64(1_700_000_000_000)
	seedFailRow(t, st, "a", "u1", "p1", "deepseek", store.UsageStatusError, 502, "upstream_error", ts)
	seedFailRow(t, st, "b", "u2", "p2", "backup", store.UsageStatusError, 500, "internal_error", ts+1)

	query := fmt.Sprintf("?from=0&to=%d", ts+100)
	mine := fetchFailures(t, st, query, false, "u1")
	if mine.Total != 1 || mine.Failed != 1 {
		t.Errorf("普通用户看到 total=%d failed=%d，期望 1/1", mine.Total, mine.Failed)
	}
	if len(mine.Upstreams) != 1 || mine.Upstreams[0].ProviderID != "p1" {
		t.Errorf("普通用户看到了别人的上游：%+v", mine.Upstreams)
	}

	all := fetchFailures(t, st, query, true, "")
	if all.Total != 2 {
		t.Errorf("管理员 total = %d，期望 2", all.Total)
	}
}
