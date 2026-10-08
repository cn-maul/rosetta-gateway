package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-10-11：调用历史新增「速度(t/s)」列，并把首字/总耗时合并成一列。
//
// # 为什么速度必须由后端算、且必须用 output_tokens
//
// 表格里每行的速度与总览页的「平均速度」必须是同一套算法
// （store.GetRecentThroughput 的 `SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0)`），
// 否则同一批请求会在两个页面给出两个数，且没有任何迹象说明该信哪个。
// 而分子必须是 output_tokens 而不是 total_tokens：后者含输入 token，
// 输入是请求侧送进去的、不是模型生成的，用它算会把速度虚高好几倍。

// seedTpsHistory 造三条用于验证速度口径的记录。
//
// 三条各自对应一个必须成立的边界：
//   - "normal"  ：正常样本，tps 必须是精确的 输出/耗时；
//   - "nolat"   ：latency_ms=0（错误请求在触碰上游前就返回，没走计时）——
//     不许除零、不许产出 +Inf（Inf 会序列化成非法 JSON，前端整表打不开）；
//   - "noreport"：output_tokens=0（上游未报 usage）—— 无样本，
//     必须与「极慢」区分开。
//
// 返回 seed 时刻，供 from=0&to=<ts+1> 这类查询用。
func seedTpsHistory(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "kTps", KeyHash: "hashTps", KeyPrefix: "sk-t", Name: "T", Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	ts := time.Now().UnixMilli()
	rows := []struct {
		id           string
		stream       bool
		outputTokens int64
		latencyMs    int64
		ttfbMs       int64
		status       string
	}{
		// 200 输出 / 4000ms = 50 t/s。刻意选整除：断言可以用 ==，
		// 不必为浮点误差写容差，公式一旦被改动会立刻炸出来。
		{"normal", true, 200, 4000, 3600, "ok"},
		// 除零边界：latency_ms=0 且 output_tokens>0。
		{"nolat", false, 300, 0, 0, "ok"},
		// 无样本边界：output_tokens=0 且 latency_ms>0。
		{"noreport", true, 0, 5000, 1000, "ok"},
	}
	for _, r := range rows {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: r.id, Ts: ts, AccessKeyID: "kTps", PublicModel: "code",
			ProviderID: "p1", UpstreamModel: "cn:glm", Stream: r.stream,
			// input 刻意给非零值：这样 total_tokens != output_tokens，
			// 「用错分子」的回归会被下面 TestUsage_History_TpsUsesOutputTokensNotTotal 抓住。
			InputTokens:  900,
			OutputTokens: r.outputTokens,
			TotalTokens:  900 + r.outputTokens,
			TTFBMs:       r.ttfbMs,
			LatencyMs:    r.latencyMs,
			Status:       r.status, HTTPStatus: http.StatusOK,
			IngressProtocol: "openai-chat", UsageState: "reported",
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	return ts
}

// fetchHistory 拉一次调用历史并解出响应。
func fetchHistory(t *testing.T, st *store.Store, query string) usageHistoryResponse {
	t.Helper()
	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.History(rec, asAdmin(httptest.NewRequest(http.MethodGet, "/admin/api/usage/history"+query, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp usageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// latency_ms=0 的记录**不能**把整个响应打坏。
//
// 这是 tpsFor 里那道守卫真正防的东西，而且比"某一格显示错"严重得多：
// 未守卫的除法得到 +Inf，encoding/json **拒绝**序列化它
// （json: unsupported value: +Inf），而 writeJSON 用的是
// `json.NewEncoder(w).Encode(v)` —— 状态码与 Content-Type 早已写出，
// 编码却在写到一半时失败。表现是**整张调用历史表打不开**，
// 而库里有任意一条 latency_ms=0 的记录（错误请求在触碰上游前就返回、
// 没走计时，实测可达）就足以触发。
//
// 断言的是「响应能解出全部记录」，而不只是「那一条的 tps 是 0」——
// 前者才是用户能感知到的回归。
func TestUsage_History_LatencyZeroDoesNotBreakWholeResponse(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.History(rec, asAdmin(httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/admin/api/usage/history?from=0&to=%d&limit=100", ts+1), nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("响应体为空 —— 说明 Encode 在 +Inf 上失败，整张表打不开")
	}
	// 原样断言响应体里不出现 Infinity/NaN 字面量：它们是非法 JSON，
	// 前端 JSON.parse 会当场抛错。
	for _, bad := range []string{"Inf", "NaN", "inf"} {
		if strings.Contains(string(body), bad) {
			t.Errorf("响应体含非法的 JSON 数值字面量 %q：%s", bad, body)
		}
	}

	var resp usageHistoryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON（整表会打不开）：%v body=%s", err, body)
	}
	// 三条记录必须都在：一条坏数据不许把别的行一起吞掉。
	if len(resp.Records) != 3 {
		t.Fatalf("records=%d, want 3 —— latency=0 的那条把别的行也弄丢了吗？body=%s",
			len(resp.Records), body)
	}
}

// tpsFor 是纯函数，直接单测比走 HTTP 更能锁住口径。
func TestTpsFor_Formula(t *testing.T) {
	cases := []struct {
		name         string
		outputTokens int64
		latencyMs    int64
		want         float64
	}{
		// 公式：output_tokens * 1000 / latency_ms（GetRecentThroughput 的单条退化形式）。
		{"50 t/s", 200, 4000, 50},
		{"1000 t/s（1 秒吐 1000）", 1000, 1000, 1000},
		// 亚毫秒级不现实，但公式本身要成立：1 token / 1ms = 1000 t/s。
		{"1 token 用 1ms", 1, 1, 1000},
		// 真实流式量级：800 输出 / 30 秒 ≈ 26.67 t/s。
		{"800 token / 30s", 800, 30000, 800.0 * 1000 / 30000},

		// —— 以下全是「没有可算的样本」：必须返回 0，而不是 Inf/NaN/负值 ——
		{"latency=0 不除零", 300, 0, 0},
		{"latency<0 视为无样本", 300, -5, 0},
		{"output=0 无样本", 0, 5000, 0},
		{"output<0 视为无样本", -1, 5000, 0},
		{"两者皆 0", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tpsFor(c.outputTokens, c.latencyMs)
			if math.IsInf(got, 0) || math.IsNaN(got) {
				t.Fatalf("tpsFor(%d, %d) = %v —— 非有限值会序列化成非法 JSON，前端整表打不开",
					c.outputTokens, c.latencyMs, got)
			}
			if got != c.want {
				t.Fatalf("tpsFor(%d, %d) = %v, want %v", c.outputTokens, c.latencyMs, got, c.want)
			}
			// 无样本时必须是**精确的 0**：JSON 里 0 与 -0/1e-9 在前端
			// `tps > 0` 判断下行为一致，但精确 0 让契约更好断言。
			if got < 0 {
				t.Fatalf("tpsFor(%d, %d) = %v —— 速度不可能为负", c.outputTokens, c.latencyMs, got)
			}
		})
	}
}

// History 必须下发 output_tokens 与算好的 tps：前端不再持有公式。
func TestUsage_History_ExposesOutputTokensAndTps(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	resp := fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1))
	if len(resp.Records) != 3 {
		t.Fatalf("records=%d, want 3 body=%+v", len(resp.Records), resp.Records)
	}

	// 以下按 (output,latency) 组合定位三条记录：它们的 public_model 相同，
	// 只能靠这两个数区分。
	type key struct{ out, lat int64 }
	got := map[key]usageHistoryEntry{}
	for _, r := range resp.Records {
		got[key{r.OutputTokens, r.LatencyMs}] = r
	}

	normal, ok := got[key{200, 4000}]
	if !ok {
		t.Fatalf("找不到正常样本（output=200,latency=4000）：%+v", resp.Records)
	}
	if normal.OutputTokens != 200 {
		t.Errorf("output_tokens=%d, want 200", normal.OutputTokens)
	}
	if normal.Tps != 50 {
		t.Errorf("tps=%v, want 50（= 200*1000/4000）", normal.Tps)
	}

	// latency=0 → 无样本，tps 必须是 0 而不是 Inf。
	nolat, ok := got[key{300, 0}]
	if !ok {
		t.Fatalf("找不到 latency=0 的样本：%+v", resp.Records)
	}
	if nolat.Tps != 0 {
		t.Errorf("latency=0 时 tps=%v, want 0（无样本，且不许是 Inf）", nolat.Tps)
	}

	// output=0 → 无样本，tps 必须是 0。
	noreport, ok := got[key{0, 5000}]
	if !ok {
		t.Fatalf("找不到 output=0 的样本：%+v", resp.Records)
	}
	if noreport.Tps != 0 {
		t.Errorf("output_tokens=0 时 tps=%v, want 0（无样本）", noreport.Tps)
	}
}

// 分子必须是 output_tokens，不能是 total_tokens。
//
// 这是本列最容易犯、也最隐蔽的错：total_tokens 含输入 token，用它算速度
// 会让数字虚高数倍，而表格里只有一个数字、看不出错。seed 里 input=900、
// output=200，两者算出来差 5.5 倍 —— 用 == 断言就能把回归钉死。
func TestUsage_History_TpsUsesOutputTokensNotTotal(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	resp := fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1))
	var normal *usageHistoryEntry
	for i := range resp.Records {
		if resp.Records[i].OutputTokens == 200 {
			normal = &resp.Records[i]
			break
		}
	}
	if normal == nil {
		t.Fatalf("找不到 output=200 的样本：%+v", resp.Records)
	}

	if normal.TotalTokens != 1100 {
		t.Fatalf("前置条件不成立：total_tokens=%d, want 1100（seed 的 900 输入 + 200 输出）",
			normal.TotalTokens)
	}
	wantWithOutput := 50.0                                       // 200 * 1000 / 4000
	wantWithTotal := float64(normal.TotalTokens) * 1000.0 / 4000 // 275 —— 用错分子会得到这个
	if normal.Tps != wantWithOutput {
		t.Errorf("tps=%v, want %v（output_tokens 口径）", normal.Tps, wantWithOutput)
	}
	if normal.Tps == wantWithTotal {
		t.Errorf("tps=%v 恰好等于用 total_tokens 算出的 %v —— 分子用错了，速度被虚高 %.1f 倍",
			normal.Tps, wantWithTotal, wantWithTotal/wantWithOutput)
	}
}

// 与总览页口径一致：同一批请求，「表格里逐条求和后再算」与
// GetRecentThroughput 的聚合公式必须给出同一个数。
//
// 这条是本次改造的核心不变量：两个页面显示的速度若不同，用户无从判断
// 该信哪个，而这种偏差不会自己暴露。单条样本下 SUM 退化为列本身，
// 所以逐条 tps 的加权平均（按耗时加权）= 聚合值。
func TestUsage_History_TpsMatchesRecentThroughputAggregate(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	resp := fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1))

	// 只取「有样本」的记录（output>0 且 latency>0），与 GetRecentThroughput
	// 的 WHERE 子句同一口径 —— 那里也是 status='ok' AND output>0 AND latency>0。
	var sumOut, sumLat int64
	for _, r := range resp.Records {
		if r.Status == "ok" && r.OutputTokens > 0 && r.LatencyMs > 0 {
			sumOut += r.OutputTokens
			sumLat += r.LatencyMs
		}
	}

	agg, err := st.GetRecentThroughput(t.Context(), 50, "")
	if err != nil {
		t.Fatalf("GetRecentThroughput: %v", err)
	}

	// 两者都必须等于 output*1000/latency 的聚合形式。
	want := float64(sumOut) * 1000 / float64(sumLat)
	if agg != want {
		t.Fatalf("GetRecentThroughput=%v, 按表格样本手算=%v —— 聚合口径漂移", agg, want)
	}
	// 表格侧：逐条按耗时加权平均，等于聚合值（这正是「同一条公式」的含义）。
	var weighted float64
	for _, r := range resp.Records {
		if r.Status == "ok" && r.OutputTokens > 0 && r.LatencyMs > 0 {
			weighted += r.Tps * float64(r.LatencyMs)
		}
	}
	weighted /= float64(sumLat)
	if math.Abs(weighted-agg) > 1e-9 {
		t.Errorf("表格逐条 tps 的耗时加权平均=%v, GetRecentThroughput=%v —— 两个页面会显示不同的速度",
			weighted, agg)
	}
}

// output_tokens 必须真的出现在 JSON 里（omitempty 之类会把 0 吃掉，
// 而 0 是「上游未报 usage」这个有意义的状态，不能被省略成字段缺失）。
func TestUsage_History_OutputTokensIsPresentEvenWhenZero(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.History(rec, asAdmin(httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/admin/api/usage/history?from=0&to=%d&limit=100", ts+1), nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}

	// 直接在原始 JSON 上断言键存在：解成 struct 后无法区分
	// 「字段缺失」与「字段是 0」。
	var raw struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, r := range raw.Records {
		for _, k := range []string{"output_tokens", "tps"} {
			if _, ok := r[k]; !ok {
				t.Errorf("第 %d 行缺少 %q 字段 —— 前端读它会得到 undefined", i, k)
			}
		}
	}
}

// CSV 必须与页面表格同字段，否则导出件无法独立解读
// （拿出去算不出速度这一列，而 CSV 的常见用途恰恰是单独分析）。
func TestUsage_ExportCSV_IncludesOutputTokensAndTps(t *testing.T) {
	st := newScopeStore(t)
	ts := seedTpsHistory(t, st)

	h := NewUsageHandler(st)
	rec := httptest.NewRecorder()
	h.ExportCSV(rec, asAdmin(httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/admin/api/usage/history.csv?from=0&to=%d", ts+1), nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	header := strings.SplitN(body, "\n", 2)[0]
	for _, col := range []string{"output_tokens", "tps"} {
		if !strings.Contains(header, col) {
			t.Errorf("CSV 表头缺 %q 列：%s", col, header)
		}
	}
	// 新增列必须**追加在末尾**：原有 11 列的下标不能变，
	// 否则任何按下标取值的既有消费方（脚本、Excel 模板）会静默错位。
	want := "ts,public_model,upstream_model,key_name,key_id,total_tokens,ttfb_ms,latency_ms,status,stream,cost,output_tokens,tps"
	if strings.TrimSpace(header) != want {
		t.Errorf("CSV 表头 = %q\nwant %q\n（新增列必须追加在末尾，不得插入中间）",
			strings.TrimSpace(header), want)
	}
	// 正常样本那一行必须写出 50（200*1000/4000）。
	if !strings.Contains(body, ",50\n") {
		t.Errorf("CSV 未写出正常样本的 tps=50：%s", body)
	}
	// 无样本的两条必须留空而不是写 0：0 会被表格工具算进平均值、
	// 把整体速度往下拽；空单元格才是"缺失"。
	if strings.Contains(body, ",0\n") {
		t.Errorf("CSV 把无样本的 tps 写成了 0（应留空）：%s", body)
	}
}
