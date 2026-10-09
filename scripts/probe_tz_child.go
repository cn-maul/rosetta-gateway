//go:build ignore

// 子探针：在一个**全新进程**里，于任何 SQLite 调用之前带 TZ 启动，
// 输出 dayExpr 对该时刻算出的日界。
//
// 被 internal/store 的 TestDayExprFollowsTZEnv 用 `go run` 调用。
//
// 为什么必须是独立进程：modernc SQLite 在 Linux 上走 C 的 tzset 并缓存
// 结果，本进程内 os.Setenv 改不了已经缓存的状态。只有全新进程在第一次
// 查询前就带着 TZ，才是生产的真实形状（main 早期 ApplyTimezone → 再开库）。
//
// 用法：TZ=Asia/Shanghai go run scripts/probe_tz_child.go -ts <毫秒时间戳>
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const dayExpr = `strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime')`

func main() {
	tsRaw := flag.String("ts", "", "毫秒时间戳")
	e2e := flag.Bool("e2e", false, "端到端模式：建库插两条跨午夜记录，按天分组后输出 'day:count,...'")
	flag.Parse()

	// TZ 由父进程经环境变量注入，本进程什么也不设 —— 保证「进程一开始
	// 就是这个 TZ」，与生产一致。
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	if *e2e {
		fmt.Print(runE2E(db))
		return
	}

	ts, err := strconv.ParseInt(*tsRaw, 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-ts 必须是毫秒时间戳:", err)
		os.Exit(2)
	}

	var got string
	if err := db.QueryRow(`SELECT `+dayExpr+` FROM (SELECT ? AS ts)`, ts).Scan(&got); err != nil {
		panic(err)
	}
	// 只输出日期串：父进程靠 stdout 取值，别混进任何别的信息。
	fmt.Print(got)
}

// runE2E 建库、插两条跨午夜的记录、按天分组，输出形如 "2026-10-09:2"
// 或 "2026-10-08:1,2026-10-09:1"。
//
// 两条记录锚定东八区 2026-10-09 的凌晨 1 点与上午 9 点：
//   - 东八区口径 → 都在 10-09（一组）
//   - UTC 口径   → 01:00 是 10-08、09:00 是 10-09（两组，缺陷形状）
func runE2E(db *sql.DB) string {
	if _, err := db.Exec(`CREATE TABLE usage_records (id TEXT, ts INTEGER)`); err != nil {
		panic(err)
	}
	cst := time.FixedZone("CST", 8*3600)
	moments := []time.Time{
		time.Date(2026, 10, 9, 1, 0, 0, 0, cst), // UTC: 10-08 17:00
		time.Date(2026, 10, 9, 9, 0, 0, 0, cst), // UTC: 10-09 01:00
	}
	for i, m := range moments {
		if _, err := db.Exec(`INSERT INTO usage_records (id, ts) VALUES (?, ?)`,
			string(rune('a'+i)), m.UnixMilli()); err != nil {
			panic(err)
		}
	}
	rows, err := db.Query(`SELECT ` + dayExpr + ` AS day, COUNT(*) FROM usage_records GROUP BY day ORDER BY day`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	parts := []string{}
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			panic(err)
		}
		parts = append(parts, fmt.Sprintf("%s:%d", d, n))
	}
	return strings.Join(parts, ",")
}
