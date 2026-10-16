//go:build integration

package metaversion

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

func mvDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("metaversion: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// T005: the migration (applied from zero by TestMain) made the three tables, the columns the code
// reads, and the four indexes the contract names.
func TestMigration_TablesColumnsAndIndexes(t *testing.T) {
	db := mvDB(t)
	cols := func(table string) map[string]bool {
		var names []string
		if err := db.Select(&names, `SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, table); err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	for table, want := range map[string][]string{
		"sys_meta_drafts":       {"tenant_id", "doc_type", "doc_key", "op", "draft_json", "base_hash", "label", "deleted_at"},
		"sys_meta_versions":     {"tenant_id", "seq", "version_no", "kind", "covered_types", "created_by_label", "schema_cutoff_at", "schema_status"},
		"sys_meta_version_docs": {"tenant_id", "version_id", "seq", "doc_type", "doc_key", "op", "before_json", "after_json", "meta", "apply_status"},
	} {
		got := cols(table)
		for _, c := range want {
			if !got[c] {
				t.Errorf("%s lacks column %s", table, c)
			}
		}
	}
	for _, idx := range []struct{ table, name string }{
		{"sys_meta_drafts", "uq_meta_drafts_doc"},
		{"sys_meta_versions", "uq_meta_versions_seq"},
		{"sys_meta_versions", "uq_meta_versions_no"},
		{"sys_meta_version_docs", "idx_mvd_doc"},
	} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`, idx.table, idx.name); err != nil || n == 0 {
			t.Errorf("index %s.%s missing (err %v)", idx.table, idx.name, err)
		}
	}
}

// T005: the down file drops all three tables and the up file is re-runnable (IF NOT EXISTS). Found
// by name, not number: the number is assigned at merge. Runs on a connection with multiStatements
// of its own so the shared throwaway DB is left migrated.
func TestMigration_DownThenUpAgain(t *testing.T) {
	db := mvDB(t)
	read := func(suffix string) string {
		m, err := filepath.Glob("../../migrations/*_create_sys_meta_drafts_versions." + suffix + ".sql")
		if err != nil || len(m) != 1 {
			t.Fatalf("migration %s: glob = %v, %v", suffix, m, err)
		}
		b, err := os.ReadFile(m[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	exec := func(script string) {
		t.Helper()
		conn, err := sqlx.Connect("mysql", testDSN+"&multiStatements=true")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(script); err != nil {
			t.Fatalf("exec migration: %v", err)
		}
	}
	count := func() int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name LIKE 'sys_meta_%'`); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 3 {
		t.Fatalf("after up: %d sys_meta_* tables, want 3", got)
	}
	exec(read("down"))
	if got := count(); got != 0 {
		t.Fatalf("after down: %d sys_meta_* tables, want 0", got)
	}
	exec(read("up"))
	exec(read("up")) // a second up is a no-op
	if got := count(); got != 3 {
		t.Fatalf("after up again: %d tables, want 3", got)
	}
}
