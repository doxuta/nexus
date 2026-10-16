//go:build integration

// Real-MySQL test of the sys_files sha256 backfill (#54, specs/036 §2.5), over the schema
// golang-migrate builds (migration 000068 included) and a real LocalBackend on a temp dir. Run with:
//
//	go test -tags=integration -run TestBackfillHashes ./internal/files/
package files_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/files"
	"nexus/platform/internal/storage"
)

const (
	bfTenantA = "aaaa1111-0000-0000-0000-00000000bf01"
	bfTenantB = "bbbb2222-0000-0000-0000-00000000bf02"
	// sha256("hello world") / sha256("hello World").
	bfSumHello = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	bfSumHELLO = "db4067cec62c58bf8b2f8982071e77c082da9e00924bf3631f3b024fa54e7d7e"
	bfPreset   = "0000000000000000000000000000000000000000000000000000000000000000"
)

func TestBackfillHashes_RealMySQL(t *testing.T) {
	db, err := sqlx.Connect("mysql", packageTestDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	clean := func() {
		if _, err := db.ExecContext(ctx, "DELETE FROM sys_files WHERE tenant_id IN (?, ?)", bfTenantA, bfTenantB); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	backend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	put := func(key, data string) {
		if _, err := backend.Put(ctx, key, strings.NewReader(data)); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	// Rows as a pre-000068 server (or an older image after it) wrote them: no sha256 column value.
	insert := func(tenant, id, sum string, deleted bool) {
		q := `INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, sha256, deleted_at)
		      VALUES (?, ?, 'docs', 'rec-1', 'doc', 'a.txt', 11, 'text/plain', ?, 'user-1', NULLIF(?, ''), IF(?, NOW(3), NULL))`
		if _, err := db.ExecContext(ctx, q, id, tenant, tenant+"/"+id, sum, deleted); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert(bfTenantA, "a1", "", false) // live, blob present → hashed
	put(bfTenantA+"/a1", "hello world")
	insert(bfTenantA, "a2", "", false) // live, blob gone → reported, stays NULL
	insert(bfTenantA, "a3", "", true)  // soft-deleted → never touched
	put(bfTenantA+"/a3", "hello world")
	insert(bfTenantA, "a4", bfPreset, false) // already has a digest → never overwritten
	put(bfTenantA+"/a4", "hello world")
	insert(bfTenantB, "b1", "", false) // other tenant → hashed onto ITS row
	put(bfTenantB+"/b1", "hello World")

	svc := files.NewService(files.NewRepository(db), backend, nil, files.Limits{}, nil)
	st, err := svc.BackfillHashes(ctx)
	if err != nil {
		t.Fatalf("BackfillHashes: %v", err)
	}
	if st.Hashed != 2 || !reflect.DeepEqual(st.Unreadable, []string{bfTenantA + "/a2"}) {
		t.Errorf("stats = %+v; want Hashed 2, Unreadable [%s/a2]", st, bfTenantA)
	}

	sumOf := func(tenant, id string) sql.NullString {
		var s sql.NullString
		if err := db.GetContext(ctx, &s, "SELECT sha256 FROM sys_files WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return s
	}
	for _, c := range []struct {
		tenant, id string
		want       sql.NullString
	}{
		{bfTenantA, "a1", sql.NullString{String: bfSumHello, Valid: true}},
		{bfTenantA, "a2", sql.NullString{}},
		{bfTenantA, "a3", sql.NullString{}},
		{bfTenantA, "a4", sql.NullString{String: bfPreset, Valid: true}},
		{bfTenantB, "b1", sql.NullString{String: bfSumHELLO, Valid: true}},
	} {
		if got := sumOf(c.tenant, c.id); got != c.want {
			t.Errorf("%s sha256 = %+v; want %+v", c.id, got, c.want)
		}
	}

	// Second pass (every server start runs one): nothing left to hash, the lost blob still reported.
	st, err = svc.BackfillHashes(ctx)
	if err != nil {
		t.Fatalf("BackfillHashes (2nd): %v", err)
	}
	if st.Hashed != 0 || !reflect.DeepEqual(st.Unreadable, []string{bfTenantA + "/a2"}) {
		t.Errorf("2nd stats = %+v; want Hashed 0, Unreadable [%s/a2]", st, bfTenantA)
	}
}
