// Command dbcompact rebuilds a rosetta-gateway SQLite database into the schema
// this build writes, dropping objects and columns that no longer exist and
// copying every compatible row verbatim.
//
// Usage:
//
//	dbcompact -src old.db -dst new.db [-force] [-dry-run] [-no-vacuum]
//
// It never touches the source database. Keep the gateway stopped (or use a
// VACUUM INTO snapshot) so the source has no live -wal writer.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	_ "modernc.org/sqlite"
)

const deadSettingKey = "model_group_orphan_repair_v1"

func main() {
	var (
		src      = flag.String("src", "", "source SQLite file (read-only)")
		dst      = flag.String("dst", "", "destination SQLite file (created)")
		force    = flag.Bool("force", false, "overwrite destination if it exists")
		dryRun   = flag.Bool("dry-run", false, "report what would change, write nothing")
		noVac    = flag.Bool("no-vacuum", false, "skip the final VACUUM")
		allowWAL = flag.Bool("allow-wal", false, "proceed even if a -wal file exists next to the source")
	)
	flag.Parse()
	if *src == "" || *dst == "" {
		fail("both -src and -dst are required")
	}
	if abs1, _ := filepath.Abs(*src); abs1 != "" {
		if abs2, _ := filepath.Abs(*dst); abs2 == abs1 {
			fail("source and destination must differ")
		}
	}
	if !*dryRun {
		if _, err := os.Stat(*dst); err == nil && !*force {
			fail("destination exists; pass -force to overwrite")
		}
	}
	if _, err := os.Stat(*src); err != nil {
		fail("source not found: %v", err)
	}
	if st, err := os.Stat(*src + "-wal"); err == nil && st.Size() > 0 && !*allowWAL {
		fail("source has a live -wal file; stop the gateway or take a VACUUM INTO snapshot, or pass -allow-wal")
	}

	// Work on a private copy so reading the source (WAL databases create -shm
	// side files) never touches the user's directory.
	workDir, err := os.MkdirTemp("", "dbcompact-*")
	if err != nil {
		fail("create temp dir: %v", err)
	}
	defer os.RemoveAll(workDir)
	srcPath := filepath.Join(workDir, "source.db")
	if err := copyWithSiblings(*src, srcPath); err != nil {
		fail("copy source: %v", err)
	}

	srcDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(srcPath)+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		fail("open source: %v", err)
	}
	defer srcDB.Close()
	if err := srcDB.Ping(); err != nil {
		fail("source is not readable: %v", err)
	}

	srcTables, srcCols, err := readSchema(srcDB, "main")
	if err != nil {
		fail("read source schema: %v", err)
	}

	targetPath := *dst
	if *dryRun {
		targetPath = filepath.Join(workDir, "target.db")
	} else if *force {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(*dst + suffix); err != nil && !os.IsNotExist(err) {
				fail("remove old destination%s: %v", suffix, err)
			}
		}
	}

	if err := buildCanonical(targetPath); err != nil {
		fail("create canonical schema: %v", err)
	}

	tgtDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(targetPath)+"?_busy_timeout=5000")
	if err != nil {
		fail("open destination: %v", err)
	}
	defer tgtDB.Close()
	tgtTables, tgtCols, err := readSchema(tgtDB, "main")
	if err != nil {
		fail("read target schema: %v", err)
	}

	report(srcTables, srcCols, tgtTables, tgtCols)

	if *dryRun {
		fmt.Println("\n[ dry-run ] no destination was written")
		return
	}

	if err := copyAll(srcDB, tgtDB, srcPath, srcCols, tgtTables, tgtCols); err != nil {
		fail("copy: %v", err)
	}
	if !*noVac {
		if _, err := tgtDB.Exec(`VACUUM`); err != nil {
			fail("vacuum: %v", err)
		}
	}
	if _, err := tgtDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		fmt.Fprintln(os.Stderr, "warning: wal checkpoint:", err)
	}
	if _, err := tgtDB.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		fmt.Fprintln(os.Stderr, "warning: journal_mode:", err)
	}
	tgtDB.Close()

	var srcSize, dstSize int64
	if fi, err := os.Stat(*src); err == nil {
		srcSize = fi.Size()
	}
	if fi, err := os.Stat(*dst); err == nil {
		dstSize = fi.Size()
	}
	fmt.Printf("\nwritten: %s\nsize: %s -> %s (%.1f%% of original)\n",
		*dst, human(srcSize), human(dstSize), float64(dstSize)*100/float64(srcSize))
	fmt.Println("done")
}

func buildCanonical(path string) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(path, logger)
	if err != nil {
		return err
	}
	return s.Close()
}

type tableDef struct {
	name string
	cols []string
}

func readSchema(db *sql.DB, schema string) ([]string, map[string][]string, error) {
	rows, err := db.Query(`SELECT name FROM ` + schema + `.sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, nil, err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	cols := make(map[string][]string, len(tables))
	for _, t := range tables {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?, ?)`, t, schema)
		if err != nil {
			return nil, nil, fmt.Errorf("columns of %s.%s: %w", schema, t, err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return nil, nil, err
			}
			cols[t] = append(cols[t], c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return tables, cols, nil
}

func report(srcTables []string, srcCols map[string][]string, tgtTables []string, tgtCols map[string][]string) {
	tgt := make(map[string]bool, len(tgtTables))
	for _, t := range tgtTables {
		tgt[t] = true
	}
	src := make(map[string]bool, len(srcTables))
	for _, t := range srcTables {
		src[t] = true
	}

	var removedTables []string
	for _, t := range srcTables {
		if !tgt[t] {
			removedTables = append(removedTables, t)
		}
	}
	fmt.Println("== tables dropped ==")
	if len(removedTables) == 0 {
		fmt.Println("  (none)")
	}
	for _, t := range removedTables {
		fmt.Printf("  %s (%d columns)\n", t, len(srcCols[t]))
	}

	fmt.Println("\n== columns dropped ==")
	anyCol := false
	for _, t := range srcTables {
		if !tgt[t] {
			continue
		}
		target := make(map[string]bool, len(tgtCols[t]))
		for _, c := range tgtCols[t] {
			target[c] = true
		}
		var gone []string
		for _, c := range srcCols[t] {
			if !target[c] {
				gone = append(gone, c)
			}
		}
		if len(gone) > 0 {
			anyCol = true
			fmt.Printf("  %s: %s\n", t, strings.Join(gone, ", "))
		}
	}
	if !anyCol {
		fmt.Println("  (none)")
	}

	fmt.Println("\n== tables copied ==")
	var copied []string
	for _, t := range srcTables {
		if tgt[t] {
			copied = append(copied, t)
		}
	}
	fmt.Println("  " + strings.Join(copied, ", "))
}

func copyAll(srcDB, tgtDB *sql.DB, srcPath string, srcCols map[string][]string, tgtTables []string, tgtCols map[string][]string) error {
	if _, err := tgtDB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	if _, err := tgtDB.Exec(`ATTACH DATABASE ? AS old`, filepath.ToSlash(srcPath)); err != nil {
		return fmt.Errorf("attach source: %w", err)
	}
	defer tgtDB.Exec(`DETACH DATABASE old`)

	rows, err := tgtDB.Query(`SELECT name, sql FROM main.sqlite_master WHERE type='trigger' AND sql IS NOT NULL`)
	if err != nil {
		return err
	}
	type trigger struct{ name, sql string }
	var triggers []trigger
	for rows.Next() {
		var tr trigger
		if err := rows.Scan(&tr.name, &tr.sql); err != nil {
			rows.Close()
			return err
		}
		triggers = append(triggers, tr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := tgtDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, tr := range triggers {
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS main.` + quoteIdent(tr.name)); err != nil {
			return fmt.Errorf("drop trigger %s: %w", tr.name, err)
		}
	}

	order := copyOrder(tgtTables, srcCols)
	for _, t := range order {
		common := intersect(tgtCols[t], srcCols[t])
		if len(common) == 0 {
			return fmt.Errorf("table %s has no columns in common", t)
		}
		if _, err := tx.Exec(`DELETE FROM main.` + quoteIdent(t)); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
		insert := fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM old.%s`,
			quoteIdent(t), quoteList(common), quoteList(common), quoteIdent(t))
		if _, err := tx.Exec(insert); err != nil {
			return fmt.Errorf("copy %s: %w", t, err)
		}
	}

	if err := copySequence(tx, tgtTables); err != nil {
		return err
	}

	for _, tr := range triggers {
		if _, err := tx.Exec(tr.sql); err != nil {
			return fmt.Errorf("recreate trigger %s: %w", tr.name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if err := verifyCounts(tgtDB, srcCols, tgtTables); err != nil {
		return err
	}
	if err := checkIntegrity(tgtDB); err != nil {
		return err
	}

	res, err := tgtDB.Exec(`DELETE FROM main.app_settings WHERE key = ?`, deadSettingKey)
	if err != nil {
		return fmt.Errorf("drop dead setting: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("\ndropped dead setting: %s\n", deadSettingKey)
	}
	return nil
}

func copySequence(tx *sql.Tx, tgtTables []string) error {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM main.sqlite_master WHERE type='table' AND name='sqlite_sequence'`).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	var srcHas int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM old.sqlite_master WHERE type='table' AND name='sqlite_sequence'`).Scan(&srcHas); err != nil || srcHas == 0 {
		return err
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO main.sqlite_sequence(name, seq)
		SELECT name, seq FROM old.sqlite_sequence
		 WHERE name IN (SELECT name FROM main.sqlite_master WHERE type='table')`)
	return err
}

func verifyCounts(db *sql.DB, srcCols map[string][]string, tgtTables []string) error {
	var problems []string
	for _, t := range tgtTables {
		var a, b int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM old.` + quoteIdent(t)).Scan(&a); err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM main.` + quoteIdent(t)).Scan(&b); err != nil {
			return err
		}
		if a != b {
			problems = append(problems, fmt.Sprintf("%s: source=%d copied=%d", t, a, b))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("row count mismatch: %s", strings.Join(problems, "; "))
	}
	return nil
}

func checkIntegrity(db *sql.DB) error {
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("integrity_check: " + integrity)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var violations int
	for rows.Next() {
		violations++
	}
	if violations > 0 {
		return fmt.Errorf("foreign_key_check found %d violations", violations)
	}
	return nil
}

func copyOrder(tables []string, srcCols map[string][]string) []string {
	priority := []string{
		"networks", "providers", "provider_credentials", "upstream_models",
		"groups", "users", "access_keys", "user_group_models",
		"routes", "route_targets", "balance_topups", "balance_pending",
		"balance_charges", "usage_daily_rollups", "usage_totals",
		"user_usage_totals", "usage_records", "audit_log", "app_settings",
		"quota_reservation_leases",
	}
	present := make(map[string]bool, len(tables))
	for _, t := range tables {
		present[t] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range priority {
		if present[t] {
			out = append(out, t)
			seen[t] = true
		}
	}
	var rest []string
	for _, t := range tables {
		if !seen[t] {
			rest = append(rest, t)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func intersect(a, b []string) []string {
	bs := make(map[string]bool, len(b))
	for _, c := range b {
		bs[c] = true
	}
	var out []string
	for _, c := range a {
		if bs[c] {
			out = append(out, c)
		}
	}
	return out
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c)
	}
	return strings.Join(out, ", ")
}

// copyWithSiblings copies the database and its -wal/-shm companions (when
// present) into workDir. The tool then reads only this private copy, so the
// original directory is never modified.
func copyWithSiblings(src, dst string) error {
	copyFile := func(from, to string) error {
		in, err := os.Open(from)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(to)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	// A -shm file is meaningless without its -wal; copy the pair only together.
	if _, err := os.Stat(src + "-wal"); err != nil {
		return nil
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(src + suffix); err != nil {
			continue
		}
		if err := copyFile(src+suffix, dst+suffix); err != nil {
			return err
		}
	}
	return nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dbcompact: "+format+"\n", args...)
	os.Exit(1)
}
