// 一次性迁移工具：把 v1.4.1 的 gateway.db 里的 provider / 模型 / 路由
// 搬到当前版本的空库。
//
// 两个关键点让它不能简单复制 db 文件：
//
//  1. master.key 不同。凭据是 AES-256-GCM 加密的，密钥 = sha256(master.key
//     文件内容)，直接搬密文会导致 12 个 provider 全部解不开凭据 —— 表现为
//     /v1 全站 404，且无任何告警。所以必须「用旧钥解、用新钥加密」。
//  2. v1.4.1 → 当前版之间 routes / access_keys 有过ALTER，列顺序不同。
//     按列名显式 INSERT，不依赖列序。
//
// 不迁移：access_keys（API key 的 hash 只在旧库有意义）、usage_records、
// audit_log、app_settings —— 用户只要 provider/模型/路由这些配置资产。
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	_ "modernc.org/sqlite"
)

type stats struct {
	Providers   int
	Models      int
	Routes      int
	Targets     int
	Credentials int
	Skipped     int
}

func main() {
	oldDB := flag.String("old-db", "", "v1.4.1 的 gateway.db 路径")
	oldKey := flag.String("old-key", "", "v1.4.1 的 master.key 路径")
	newDB := flag.String("new-db", "", "当前版本的 gateway.db 路径")
	newKey := flag.String("new-key", "", "当前版本的 master.key 路径")
	dryRun := flag.Bool("dry-run", false, "只读校验并打印计划，不写入目标库")
	flag.Parse()

	if *oldDB == "" || *oldKey == "" || *newDB == "" || *newKey == "" {
		log.Fatal("四个路径参数都是必填：--old-db --old-key --new-db --new-key")
	}

	oldMK, err := loadKey(*oldKey)
	if err != nil {
		log.Fatalf("旧 master.key: %v", err)
	}
	newMK, err := loadKey(*newKey)
	if err != nil {
		log.Fatalf("新 master.key: %v", err)
	}
	if oldMK != nil && newMK != nil && string(oldMK) == string(newMK) {
		fmt.Println("提示：两库 master.key 相同，凭据可原样搬运，无需重加密。")
	}

	src, err := sql.Open("sqlite", "file:"+*oldDB+"?mode=ro")
	if err != nil {
		log.Fatalf("打开旧库（只读）: %v", err)
	}
	defer src.Close()

	ctx := context.Background()
	pre, err := audit(ctx, src, oldMK, newMK)
	if err != nil {
		log.Fatalf("校验旧库: %v", err)
	}
	printPlan(pre)

	if *dryRun {
		fmt.Println("\n[dry-run] 未写入任何数据。")
		return
	}
	if pre.Credentials == 0 {
		log.Fatal("旧库凭据一条都解不开，拒绝迁移 —— 否则会导入 12 个零凭据的 provider。")
	}

	dst, err := sql.Open("sqlite", *newDB)
	if err != nil {
		log.Fatalf("打开目标库: %v", err)
	}
	defer dst.Close()
	dst.Exec(`PRAGMA foreign_keys=ON`)

	if err := mustNonEmpty(ctx, dst); err != nil {
		log.Fatalf("目标库不是空库: %v", err)
	}

	st, err := migrate(ctx, src, dst, oldMK, newMK)
	if err != nil {
		log.Fatalf("迁移失败: %v", err)
	}
	fmt.Printf("\n迁移完成：provider %d，模型 %d，路由 %d，链成员 %d，凭据 %d\n",
		st.Providers, st.Models, st.Routes, st.Targets, st.Credentials)
	fmt.Println("已跳过：access_keys / usage_records / audit_log / app_settings")
}

// loadKey 复刻 crypto.LoadMasterKey 的文件分支：读文件、去掉尾部空白、
// sha256 派生。格式非法直接报错 —— 半截密钥会让凭据永久解不开。
func loadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, fmt.Errorf("%s 内容为空", path)
	}
	if len(s) != 44 {
		return nil, fmt.Errorf("%s 长度 %d，期望 44 字符 base64url", path, len(s))
	}
	sum := sha256.Sum256([]byte(s))
	return sum[:], nil
}

// audit 在写入前把「会发生什么」算清楚，并确认每条凭据真的可解。
func audit(ctx context.Context, src *sql.DB, oldMK, newMK []byte) (*stats, error) {
	s := &stats{}
	for _, t := range []struct {
		name string
		into *int
	}{
		{"providers", &s.Providers},
		{"upstream_models", &s.Models},
		{"routes", &s.Routes},
		{"route_targets", &s.Targets},
	} {
		if err := src.QueryRowContext(ctx, "select count(*) from "+t.name).Scan(t.into); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
	}

	rows, err := src.QueryContext(ctx, `select provider_id, api_key_enc from provider_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		var pid string
		if err := rows.Scan(&pid, &blob); err != nil {
			return nil, err
		}
		if _, err := crypto.Decrypt(blob, oldMK); err != nil {
			s.Skipped++
			continue
		}
		s.Credentials++
	}
	return s, rows.Err()
}

func printPlan(s *stats) {
	fmt.Println("迁移计划（旧 v1.4.1 → 当前版本）")
	fmt.Printf("  provider      %d\n", s.Providers)
	fmt.Printf("  上游模型      %d\n", s.Models)
	fmt.Printf("  路由          %d\n", s.Routes)
	fmt.Printf("  故障转移链成员 %d\n", s.Targets)
	fmt.Printf("  凭据          %d 条可解密", s.Credentials)
	if s.Skipped > 0 {
		fmt.Printf("，%d 条解不开（将被跳过）", s.Skipped)
	}
	fmt.Println()
}

// mustNonEmpty 拒绝往非空库写。目标库若有数据，混在一起后无法区分谁是谁。
func mustNonEmpty(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`select (select count(*) from providers) + (select count(*) from upstream_models)
		      + (select count(*) from routes) + (select count(*) from route_targets)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("已有 %d 行配置数据，请先备份并清空", n)
	}
	return nil
}

func migrate(ctx context.Context, src, dst *sql.DB, oldMK, newMK []byte) (*stats, error) {
	st := &stats{}
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// provider：保留原 ID，routes/route_targets 的外键依赖它。
	pcols := "id, slug, name, protocol, endpoint, enabled, timeout_ms, max_retries, quirks_json, created_at, updated_at"
	rows, err := src.QueryContext(ctx, "select "+pcols+" from providers")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, slug, name, protocol, endpoint string
		var enabled, timeout, retries int
		var quirks sql.NullString
		var created, updated int64
		if err := rows.Scan(&id, &slug, &name, &protocol, &endpoint, &enabled,
			&timeout, &retries, &quirks, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		var q any
		if quirks.Valid && quirks.String != "" {
			q = quirks.String
		}
		if _, err := tx.ExecContext(ctx, "insert into providers ("+pcols+") values (?,?,?,?,?,?,?,?,?,?,?)",
			id, slug, name, protocol, endpoint, enabled, timeout, retries, q, created, updated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("provider %s: %w", slug, err)
		}
		st.Providers++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 上游模型：supports_thinking 是可空三态，必须用 sql.NullBool，
	// 直接扫 bool 会在 NULL 行上报错。
	mrows, err := src.QueryContext(ctx, `select id, provider_id, model_id, display_name, enabled,
		context_window, max_output_tokens, supports_thinking,
		price_input, price_cache_hit, price_output from upstream_models`)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var id, pid, modelID string
		var display sql.NullString
		var enabled, ctxWin, maxOut int
		var thinking sql.NullBool
		var pi, pch, po sql.NullFloat64
		if err := mrows.Scan(&id, &pid, &modelID, &display, &enabled, &ctxWin, &maxOut,
			&thinking, &pi, &pch, &po); err != nil {
			mrows.Close()
			return nil, err
		}
		var disp any
		if display.Valid {
			disp = display.String
		}
		var th any
		if thinking.Valid {
			th = thinking.Bool
		}
		if _, err := tx.ExecContext(ctx, `insert into upstream_models
			(id, provider_id, model_id, display_name, enabled, context_window,
			 max_output_tokens, supports_thinking, price_input, price_cache_hit, price_output)
			values (?,?,?,?,?,?,?,?,?,?,?)`,
			id, pid, modelID, disp, enabled, ctxWin, maxOut, th, nullF(pi), nullF(pch), nullF(po)); err != nil {
			mrows.Close()
			return nil, fmt.Errorf("model %s: %w", modelID, err)
		}
		st.Models++
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	// 路由：v1.4.1 的列序是 (...enabled, created_at, failover_enabled)，
	// 当前版是 (...enabled, failover_enabled, created_at)。按列名写，不吃列序。
	rrows, err := src.QueryContext(ctx, `select id, public_name, provider_id, upstream_model_id,
		enabled, failover_enabled, created_at from routes`)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var id, pub, pid, umid string
		var enabled, failover int
		var created int64
		if err := rrows.Scan(&id, &pub, &pid, &umid, &enabled, &failover, &created); err != nil {
			rrows.Close()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `insert into routes
			(id, public_name, provider_id, upstream_model_id, enabled, failover_enabled, created_at)
			values (?,?,?,?,?,?,?)`, id, pub, pid, umid, enabled, failover, created); err != nil {
			rrows.Close()
			return nil, fmt.Errorf("route %s: %w", pub, err)
		}
		st.Routes++
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	// 故障转移链
	trows, err := src.QueryContext(ctx, `select id, route_id, provider_id, upstream_model_id,
		position, enabled, created_at from route_targets`)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var id, rid, pid, umid string
		var pos, enabled int
		var created int64
		if err := trows.Scan(&id, &rid, &pid, &umid, &pos, &enabled, &created); err != nil {
			trows.Close()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `insert into route_targets
			(id, route_id, provider_id, upstream_model_id, position, enabled, created_at)
			values (?,?,?,?,?,?,?)`, id, rid, pid, umid, pos, enabled, created); err != nil {
			trows.Close()
			return nil, fmt.Errorf("target %s: %w", id, err)
		}
		st.Targets++
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}

	// 凭据：用旧钥解密 → 用新钥重新加密。这是整个迁移里唯一必须转换的数据。
	crows, err := src.QueryContext(ctx, `select id, provider_id, label, api_key_enc, enabled,
		weight, status, cooldown_until, last_error, created_at from provider_credentials`)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id, pid, label string
		var blob []byte
		var enabled, weight int
		var status string
		var cooldown int64
		var lastErr sql.NullString
		var created int64
		if err := crows.Scan(&id, &pid, &label, &blob, &enabled, &weight, &status,
			&cooldown, &lastErr, &created); err != nil {
			crows.Close()
			return nil, err
		}
		plain, err := crypto.Decrypt(blob, oldMK)
		if err != nil {
			fmt.Printf("  跳过凭据 %s（%s）：旧密钥解不开\n", label, pid)
			continue
		}
		reenc, err := crypto.Encrypt(plain, newMK)
		if err != nil {
			crows.Close()
			return nil, err
		}
		// 冷却与错误状态不迁移：那是旧库当时的运行时快照，
		// 带过来会让新库凭空认为某个凭据刚失败过。
		var le any
		if lastErr.Valid && lastErr.String != "" {
			le = lastErr.String
		}
		if _, err := tx.ExecContext(ctx, `insert into provider_credentials
			(id, provider_id, label, api_key_enc, enabled, weight, status, cooldown_until, last_error, created_at)
			values (?,?,?,?,?,?,?,?,?,?)`,
			id, pid, label, reenc, enabled, weight, "healthy", int64(0), le, created); err != nil {
			crows.Close()
			return nil, fmt.Errorf("credential %s: %w", label, err)
		}
		st.Credentials++
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return st, nil
}

func nullF(f sql.NullFloat64) any {
	if f.Valid {
		return f.Float64
	}
	return nil
}
