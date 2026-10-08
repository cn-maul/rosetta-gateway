package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// 按天分桶的日界测试（2026-10-09 修复「过了 0 点按天数据不切日」）。
//
// # 缺陷的形状
//
// dayExpr 用 SQLite 的 'localtime'；容器里没设 TZ 时 Go 的 time.Local
// 是 UTC，北京时间 0~8 点的调用被算进「昨天」（实测：用户的 909 条
// 清晨调用全落进前一天的桶）。
//
// # 为什么在 store 包里再测一遍 —— config 包只证明了 time.Local 改了，
// 没证明 SQLite 的 'localtime' 跟着走
//
// 那一步是整个修复的**关键假设**：modernc.org/sqlite 读的是 Go 的
// time.Local，而不是 C 层缓存好的 tzset 结果。这个假设一旦不成立
// （比如某次升级换了 SQLite 绑定），修复整体失效且无任何报错 ——
// 按天数字默默回到 UTC 日界。所以必须用**真实的 SQLite** 把
// 「改 time.Local → dayExpr 输出变化」这条链路端到端钉住。
func TestDayExprFollowsTimeLocal(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })

	// 时刻的选取：东八区 2026-10-09 凌晨 1 点。
	//   - 按 UTC     分桶 → 2026-10-08（前一天）
	//   - 按东八区   分桶 → 2026-10-09（当天）
	// 这正是用户报的那条记录的形状：清晨调用被算进昨天。
	cst := time.FixedZone("Asia/Shanghai", 8*3600)
	ts := time.Date(2026, 10, 9, 1, 0, 0, 0, cst).UnixMilli()

	for _, tc := range []struct {
		tz   *time.Location
		want string
	}{
		{cst, "2026-10-09"},                           // 修复后应有的口径
		{time.UTC, "2026-10-08"},                      // 缺陷口径：还原现场，防回归用
		{time.FixedZone("NY", -5*3600), "2026-10-08"}, // 西五区下该时刻还是 10-08
	} {
		time.Local = tc.tz
		var got string
		// FROM (SELECT ? AS ts) 而不是建表：dayExpr 里硬编码了列名 ts，
		// 用一个内联子查询喂值即可，不必为这条纯 SQL 测试维护一张表
		// （共享的 :memory: 库会让测试之间互相污染）。
		if err := testDB.QueryRow(`SELECT `+dayExpr+` FROM (SELECT ? AS ts)`, ts).Scan(&got); err != nil {
			t.Fatalf("dayExpr query (time.Local=%v): %v", tc.tz, err)
		}
		if got != tc.want {
			t.Errorf("time.Local=%v 时 dayExpr(该时刻)=%s，期望 %s", tc.tz, got, tc.want)
		}
	}
}

// 归档写入与查询读取的 day 必须在**同一个时区口径**下工作 —— 即使
// 两次调用之间 time.Local 被改过（升级配置后重启前的窗口）也不会出现
// 「同一条记录落进两个桶」。
//
// 实现上这由「dayExpr 是常量、读 time.Local」天然保证；这条测试钉的是
// UsageSource 的归一化来源在东八区下把「今晨 1 点」归到「今天」——
// 即用户缺陷的端到端复现与修复验证。
func TestUsageSourceBucketsMorningCallIntoToday(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	time.Local = time.FixedZone("Asia/Shanghai", 8*3600)

	ctx := context.Background()
	st := newStore(t)

	// 「今天」在东八区下的日界：拿一个确定的日期做锚，避免测试跨午夜跑时
	// 「今天」变成另一个值。锚定 2026-10-09，记录落在 09 凌晨 1 点与
	// 09 上午 9 点 —— 两者的 day 都必须是 2026-10-09。
	cst := time.Local
	morning1 := time.Date(2026, 10, 9, 1, 0, 0, 0, cst) // UTC 还是前一天 17:00
	morning9 := time.Date(2026, 10, 9, 9, 0, 0, 0, cst) // UTC 已是当天 01:00
	for i, m := range []time.Time{morning1, morning9} {
		if err := st.CreateUsageRecord(ctx, &UsageRecord{
			ID:          "tz-probe-" + string(rune('a'+i)),
			Ts:          m.UnixMilli(),
			AccessKeyID: "k1",
			PublicModel: "m",
			ProviderID:  "p1",
			TotalTokens: 10,
			Status:      "ok",
			HTTPStatus:  200,
		}); err != nil {
			t.Fatalf("create usage: %v", err)
		}
	}

	// 查询窗口覆盖这两个时刻，按天分组的 day 必须只有 2026-10-09 一组，
	// 且 count=2 —— 凌晨 1 点那条若被算进 10-08，会出现两组或 count=1。
	rows, err := st.Reader().QueryContext(ctx,
		`SELECT `+dayExpr+` AS day, COUNT(*) FROM usage_records
		 WHERE ts >= ? AND ts <= ? GROUP BY day`,
		morning1.UnixMilli(), morning9.UnixMilli())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var groups []string
	var total int
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		groups = append(groups, d)
		total += n
	}
	if len(groups) != 1 || groups[0] != "2026-10-09" || total != 2 {
		t.Fatalf("东八区下两条记录应同落 2026-10-09（count=2），实际 groups=%v total=%d —— 清晨调用被切进了别的天", groups, total)
	}
}

// testDB 是本文件用的共享内存库：dayExpr 的纯 SQL 测试不需要完整 Store。
var testDB = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	return db
}()
