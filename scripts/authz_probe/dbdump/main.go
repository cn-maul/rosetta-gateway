// dbdump 是权限探针的**直接查库**助手。
//
// # 为什么需要它（而不是 sqlite3 CLI）
//
// 本机没有 sqlite3 CLI（实测 `Get-Command sqlite3` 为空）。而「越权被拒后
// 库里没留下痕迹」这条断言**必须**直接查库，不能只信 API 自述 —— 一个
// 「先建了再回 403」的实现，API 侧的状态码断言照样绿，只有查库能揭穿它
// （internal/admin/csrf_test.go 与 cmd/gateway/adminlock_test.go 都反复
// 强调过这一点）。
//
// 所以这里用 Go 直接读那个库。**只读**打开（`?mode=ro`）：助手本身绝不允许
// 成为第二个写入者，否则「查库」这个动作就会污染被查的对象。
//
// # 用法
//
//	scripts/authz_probe/dbdump <db-path> <admin-id> <user-id>
//
// 输出一行行 `key=value`，由 probe.ps1 解析并断言。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "用法: dbdump <db-path> <admin-id> <user-id>")
		os.Exit(2)
	}
	dbPath, adminID, userID := os.Args[1], os.Args[2], os.Args[3]

	// mode=ro：只读打开。探针绝不能在「检查副作用」时自己制造副作用。
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	defer db.Close()

	count := func(query string, args ...any) int64 {
		var n int64
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			fmt.Fprintf(os.Stderr, "query %q: %v\n", query, err)
			os.Exit(1)
		}
		return n
	}

	// 管理员的行是否存在、余额是否为 NULL（不限额）。
	var adminBalance sql.NullInt64
	err = db.QueryRow(`SELECT balance_cents FROM users WHERE id = ?`, adminID).Scan(&adminBalance)
	adminExists := true
	if err == sql.ErrNoRows {
		adminExists = false
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "read admin balance: %v\n", err)
		os.Exit(1)
	}
	balanceStr := "NULL"
	if adminBalance.Valid {
		balanceStr = fmt.Sprint(adminBalance.Int64)
	}

	fmt.Printf("access_keys_total=%d\n", count(`SELECT COUNT(*) FROM access_keys`))
	fmt.Printf("access_keys_owned_by_admin=%d\n", count(`SELECT COUNT(*) FROM access_keys WHERE user_id = ?`, adminID))
	fmt.Printf("access_keys_named_probe=%d\n", count(`SELECT COUNT(*) FROM access_keys WHERE name = 'admin-self-key-probe'`))
	fmt.Printf("access_keys_owned_by_user=%d\n", count(`SELECT COUNT(*) FROM access_keys WHERE user_id = ?`, userID))
	fmt.Printf("admin_exists=%v\n", adminExists)
	fmt.Printf("admin_balance_cents=%s\n", balanceStr)
	fmt.Printf("balance_topups_total=%d\n", count(`SELECT COUNT(*) FROM balance_topups`))
	fmt.Printf("balance_topups_for_admin=%d\n", count(`SELECT COUNT(*) FROM balance_topups WHERE user_id = ?`, adminID))
	fmt.Printf("users_total=%d\n", count(`SELECT COUNT(*) FROM users`))
}
