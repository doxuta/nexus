//go:build integration

package dashboard

// exec_integration_test.go — specs/063-draft-publish T052: the *Exec statements write exactly the rows
// the admin repository writes, and deleting a dashboard on a tx purges its app-nav rows in that tx.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func TestExec_WritesWhatTheRepositoryWrites_AndPurgesNavOnTx(t *testing.T) {
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("dashboard: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := NewSQLRepository(db)
	ctx := context.Background()
	tenant, actor := uuid.NewString(), uuid.NewString()

	type cols struct {
		Name    string  `db:"name"`
		Desc    *string `db:"description"`
		Widgets string  `db:"widgets"`
		By      *string `db:"created_by"`
		Del     bool    `db:"del"`
	}
	read := func(id string) cols {
		var c cols
		if err := db.Get(&c, "SELECT name, description, CAST(widgets AS CHAR) AS widgets, created_by, deleted_at IS NOT NULL AS del FROM sys_dashboards WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return c
	}
	same := func(label string, a, b cols) {
		t.Helper()
		d := func(s *string) string {
			if s == nil {
				return "<NULL>"
			}
			return *s
		}
		if a.Name != b.Name || d(a.Desc) != d(b.Desc) || a.Widgets != b.Widgets || d(a.By) != d(b.By) || a.Del != b.Del {
			t.Errorf("%s: repository wrote %+v, *Exec wrote %+v", label, a, b)
		}
	}

	in := CreateInput{TenantID: tenant, Name: "d", Description: "", Widgets: []byte(`[{"id":"w1"}]`), CreatedBy: actor}
	viaRepo, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	execID := uuid.NewString()
	if err := InsertWithIDExec(ctx, db, execID, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	same("create (blank description = NULL)", read(viaRepo), read(execID))

	up := UpdateInput{Name: "d2", Description: "note", Widgets: []byte(`[]`)}
	if err := repo.Update(ctx, tenant, viaRepo, up); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExec(ctx, db, tenant, execID, up); err != nil {
		t.Fatal(err)
	}
	same("update", read(viaRepo), read(execID))
	if err := UpdateExec(ctx, db, uuid.NewString(), execID, up); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("cross-tenant UpdateExec: %v", err)
	}

	// Delete on a tx removes the dashboard AND its nav row together; a rollback keeps both.
	appID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO sys_apps (id, tenant_id, name, label) VALUES (?, ?, 'a', 'A')`, appID, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sys_app_nav_items (id, tenant_id, app_id, item_type, ref_id) VALUES (?, ?, ?, 'dashboard', ?)`,
		uuid.NewString(), tenant, appID, execID); err != nil {
		t.Fatal(err)
	}
	nav := func() int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND ref_id = ?`, tenant, execID); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tx, _ := db.BeginTxx(ctx, nil)
	if err := SoftDeleteExec(ctx, tx, tenant, execID, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if nav() != 1 || read(execID).Del {
		t.Fatal("a rolled-back delete left a trace")
	}
	tx, _ = db.BeginTxx(ctx, nil)
	if err := SoftDeleteExec(ctx, tx, tenant, execID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if nav() != 0 || !read(execID).Del {
		t.Fatal("delete on a tx must remove the dashboard and its nav row together")
	}
	if err := SoftDeleteExec(ctx, db, tenant, execID, time.Now()); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := repo.Delete(ctx, tenant, viaRepo); err != nil {
		t.Fatal(err)
	}
	same("delete", read(viaRepo), read(execID))
	if ok, err := RestoreExec(ctx, db, tenant, execID, up); err != nil || !ok {
		t.Fatalf("RestoreExec = %v, %v", ok, err)
	}
}
