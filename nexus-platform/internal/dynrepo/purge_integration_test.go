//go:build integration

// specs/056-record-hard-delete — PurgeRecord on the REAL repository path (MySQL 8).
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/ -run 'TestPurgeRecord'
package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// purgeFixtureFile inserts one sys_files catalog row for (tenant, entity, record) and returns its id.
// tombstoned marks it already soft-deleted (a file the user removed earlier — its row still sits in
// the catalog and must go with the record).
func purgeFixtureFile(t *testing.T, db *sqlx.DB, tenant, entity, record string, tombstoned bool) string {
	t.Helper()
	id := uuid.NewString()
	q := `INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, deleted_at)
	      VALUES (?, ?, ?, ?, 'attachment', 'a.pdf', 3, 'application/pdf', ?, ?, IF(?, NOW(3), NULL))`
	if _, err := db.Exec(q, id, tenant, entity, record, tenant+"/"+id, "00000000-0000-0000-0000-0000000000aa", tombstoned); err != nil {
		t.Fatalf("insert sys_files: %v", err)
	}
	return id
}

func purgeCount(t *testing.T, db *sqlx.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Get(&n, q, args...); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func purgeTableRows(res *PurgeResult, table string) int64 {
	for _, tc := range res.Tables {
		if tc.Table == table {
			return tc.Rows
		}
	}
	return 0
}

// TestPurgeRecord_BinnedRecordAndItsFiles: a soft-deleted record and every sys_files row attached
// to it (live AND tombstoned) leave in one transaction; a dry run reports the exact same numbers
// and leaves everything in place; a neighbour record and its file are untouched.
func TestPurgeRecord_BinnedRecordAndItsFiles(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_purge_a"
	const tenant = "56000000-0000-0000-0000-0000000000a1"
	const user = "00000000-0000-0000-0000-0000000000aa"

	clean := func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = db.Exec("DELETE FROM sys_files WHERE entity_name = ?", entity)
		_, _ = db.Exec("DELETE FROM sys_unique_index WHERE entity_name = ?", entity)
	}
	clean()
	t.Cleanup(clean)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "email", IsUnique: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	x, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "X", "email": "x@example.com"}, emailUnique)
	if err != nil {
		t.Fatalf("insert X: %v", err)
	}
	y, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Y", "email": "y@example.com"}, emailUnique)
	if err != nil {
		t.Fatalf("insert Y: %v", err)
	}
	fx1 := purgeFixtureFile(t, db, tenant, entity, x, false)
	fx2 := purgeFixtureFile(t, db, tenant, entity, x, true)
	fy := purgeFixtureFile(t, db, tenant, entity, y, false)
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, x, AdminScope()); err != nil {
		t.Fatalf("soft-delete X: %v", err)
	}

	check := func(label string, res *PurgeResult) {
		t.Helper()
		if len(res.Records) != 1 || res.Records[0].Entity != entity || res.Records[0].ID != x {
			t.Fatalf("%s: records = %+v; want exactly X", label, res.Records)
		}
		if res.Records[0].Data["name"] != "X" {
			t.Fatalf("%s: before-image lost: %v", label, res.Records[0].Data)
		}
		got := map[string]bool{}
		for _, id := range res.FileIDs {
			got[id] = true
		}
		if len(res.FileIDs) != 2 || !got[fx1] || !got[fx2] {
			t.Fatalf("%s: file ids = %v; want X's two files %s %s", label, res.FileIDs, fx1, fx2)
		}
		if n := purgeTableRows(res, "obj_"+entity); n != 1 {
			t.Fatalf("%s: obj rows = %d; want 1 (tables=%+v)", label, n, res.Tables)
		}
		if n := purgeTableRows(res, "sys_files"); n != 2 {
			t.Fatalf("%s: sys_files rows = %d; want 2 (tables=%+v)", label, n, res.Tables)
		}
	}

	// Dry run: same numbers, nothing gone.
	res, err := repo.PurgeRecord(ctx, PurgeParams{Entity: entity, TenantID: tenant, ID: x, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	check("dry run", res)
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM obj_"+entity+" WHERE tenant_id = ? AND id = ?", tenant, x); n != 1 {
		t.Fatalf("dry run deleted the record (count=%d)", n)
	}
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM sys_files WHERE tenant_id = ? AND record_id = ?", tenant, x); n != 2 {
		t.Fatalf("dry run deleted file rows (count=%d)", n)
	}

	// Real purge.
	res, err = repo.PurgeRecord(ctx, PurgeParams{Entity: entity, TenantID: tenant, ID: x})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	check("purge", res)
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM obj_"+entity+" WHERE tenant_id = ? AND id = ?", tenant, x); n != 0 {
		t.Fatalf("record still present after purge (count=%d)", n)
	}
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM sys_files WHERE tenant_id = ? AND record_id = ?", tenant, x); n != 0 {
		t.Fatalf("file rows still present after purge (count=%d)", n)
	}
	// The neighbour and its file are untouched.
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM obj_"+entity+" WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL", tenant, y); n != 1 {
		t.Fatalf("neighbour Y touched (count=%d)", n)
	}
	if n := purgeCount(t, db, "SELECT COUNT(*) FROM sys_files WHERE id = ?", fy); n != 1 {
		t.Fatalf("neighbour's file touched (count=%d)", n)
	}

	// Twice = nothing left to purge.
	if _, err := repo.PurgeRecord(ctx, PurgeParams{Entity: entity, TenantID: tenant, ID: x}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("second purge: want ErrRecordNotFound, got %v", err)
	}
}

// TestPurgeRecord_RefusesLiveUnknownAndCrossTenant: only a record in the tenant's recycle bin can be
// purged. A live record is its own error (the operator must bin it first); an unknown id and the
// same id asked from another tenant are plain not-found — and none of them deletes anything.
func TestPurgeRecord_RefusesLiveUnknownAndCrossTenant(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_purge_b"
	const tenant = "56000000-0000-0000-0000-0000000000b1"
	const other = "56000000-0000-0000-0000-0000000000b2"
	const user = "00000000-0000-0000-0000-0000000000bb"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	live, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Live"}, nil)
	if err != nil {
		t.Fatalf("insert live: %v", err)
	}
	binned, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Binned"}, nil)
	if err != nil {
		t.Fatalf("insert binned: %v", err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, binned, AdminScope()); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	tests := []struct {
		name   string
		tenant string
		id     string
		want   error
		dryRun bool
		keptID string // a row that must survive the refused purge
	}{
		{"live record", tenant, live, ErrRecordNotInBin, false, live},
		{"live record, dry run", tenant, live, ErrRecordNotInBin, true, live},
		{"unknown id", tenant, uuid.NewString(), ErrRecordNotFound, false, ""},
		{"other tenant asks for a binned id", other, binned, ErrRecordNotFound, false, binned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repo.PurgeRecord(ctx, PurgeParams{Entity: entity, TenantID: tt.tenant, ID: tt.id, DryRun: tt.dryRun})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v; want %v", err, tt.want)
			}
			if tt.keptID != "" {
				if n := purgeCount(t, db, "SELECT COUNT(*) FROM obj_"+entity+" WHERE tenant_id = ? AND id = ?", tenant, tt.keptID); n != 1 {
					t.Fatalf("refused purge still deleted %s (count=%d)", tt.keptID, n)
				}
			}
		})
	}
}
