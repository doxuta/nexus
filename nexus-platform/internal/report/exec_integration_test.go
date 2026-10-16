//go:build integration

package report

// exec_integration_test.go — specs/063-draft-publish T052: the *Exec statements (what publish runs on
// its transaction) write exactly the rows the admin repository writes, tenant-pinned.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type rpCols struct {
	Name       string  `db:"name"`
	EntityName string  `db:"entity_name"`
	Type       string  `db:"type"`
	Config     string  `db:"config"`
	IsActive   bool    `db:"is_active"`
	CreatedBy  *string `db:"created_by"`
	UpdatedBy  *string `db:"updated_by"`
	Deleted    bool    `db:"del"`
}

func rpRead(t *testing.T, db *sqlx.DB, tenant, id string) rpCols {
	t.Helper()
	var c rpCols
	if err := db.Get(&c, "SELECT name, entity_name, type, CAST(config AS CHAR) AS config, is_active, created_by, updated_by, deleted_at IS NOT NULL AS del FROM sys_reports WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return c
}

func rpSame(t *testing.T, label string, a, b rpCols) {
	t.Helper()
	d := func(s *string) string {
		if s == nil {
			return "<NULL>"
		}
		return *s
	}
	if a.Name != b.Name || a.EntityName != b.EntityName || a.Type != b.Type || a.Config != b.Config || a.IsActive != b.IsActive ||
		d(a.CreatedBy) != d(b.CreatedBy) || d(a.UpdatedBy) != d(b.UpdatedBy) || a.Deleted != b.Deleted {
		t.Errorf("%s: repository wrote %+v, *Exec wrote %+v", label, a, b)
	}
}

func TestExec_WritesWhatTheRepositoryWrites(t *testing.T) {
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("report: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := NewSQLRepository(db)
	ctx := context.Background()
	tenant, actor := uuid.NewString(), uuid.NewString()

	in := CreateInput{TenantID: tenant, Name: "r", EntityName: "crm_x", Type: TypeTabular, Config: []byte(`{"columns":["a"]}`), IsActive: true, CreatedBy: actor}
	viaRepo, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	execID := uuid.NewString()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := InsertWithIDExec(ctx, tx, execID, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rpSame(t, "create", rpRead(t, db, tenant, viaRepo), rpRead(t, db, tenant, execID))

	up := UpdateInput{Name: "r2", EntityName: "crm_y", Type: TypeSummary, Config: []byte(`{"groupBy":"s","metric":"count"}`), IsActive: false, UpdatedBy: actor}
	if err := repo.Update(ctx, tenant, viaRepo, up); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExec(ctx, db, tenant, execID, up); err != nil {
		t.Fatal(err)
	}
	rpSame(t, "update", rpRead(t, db, tenant, viaRepo), rpRead(t, db, tenant, execID))

	// Another tenant's id is never touched.
	if err := UpdateExec(ctx, db, uuid.NewString(), execID, up); !errors.Is(err, ErrReportNotFound) {
		t.Fatalf("cross-tenant UpdateExec: %v", err)
	}

	if err := repo.Delete(ctx, tenant, viaRepo); err != nil {
		t.Fatal(err)
	}
	if err := SoftDeleteExec(ctx, db, tenant, execID, time.Now()); err != nil {
		t.Fatal(err)
	}
	rpSame(t, "delete", rpRead(t, db, tenant, viaRepo), rpRead(t, db, tenant, execID))
	if err := SoftDeleteExec(ctx, db, tenant, execID, time.Now()); !errors.Is(err, ErrReportNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	restored, err := RestoreExec(ctx, db, tenant, execID, up)
	if err != nil || !restored {
		t.Fatalf("RestoreExec = %v, %v", restored, err)
	}
	if rpRead(t, db, tenant, execID).Deleted {
		t.Fatal("restored report is still deleted")
	}
	if again, _ := RestoreExec(ctx, db, tenant, execID, up); again {
		t.Fatal("RestoreExec restored a live report")
	}
}
