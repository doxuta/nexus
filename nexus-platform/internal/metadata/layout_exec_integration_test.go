//go:build integration

package metadata

// layout_exec_integration_test.go — specs/063-draft-publish T064: the layout *Exec statements (what
// publish runs on its transaction) write exactly the rows LayoutRepository writes, and an explicit
// updated_at beats the column's ON UPDATE clause.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type layoutRow struct {
	ID        string    `db:"id"`
	JSON      string    `db:"j"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func layoutRead(t *testing.T, db *sqlx.DB, tenant, entity, view string) (layoutRow, bool) {
	t.Helper()
	var r layoutRow
	err := db.Get(&r, `SELECT id, CAST(layout_json AS CHAR) AS j, created_at, updated_at FROM sys_layouts
		WHERE tenant_id = ? AND entity_name = ? AND view_type = ?`, tenant, entity, view)
	if err != nil {
		return layoutRow{}, false
	}
	return r, true
}

func layoutConn(t *testing.T) (*sqlx.DB, *gorm.DB) {
	t.Helper()
	sdb, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sdb.DB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm: %v", err)
	}
	return sdb, gdb
}

func TestUpsertLayoutExec_WritesWhatTheRepositoryWrites(t *testing.T) {
	sdb, gdb := layoutConn(t)
	repo := NewLayoutRepository(gdb)
	ctx := context.Background()
	tenant := uuid.NewString()
	mk := func(entity, js string) *LayoutDefinition {
		return &LayoutDefinition{ID: uuid.NewString(), TenantID: tenant, EntityName: entity, ViewType: "edit", LayoutJSON: js}
	}

	// repository path (also the through-ConnPool path: Upsert calls the statement on it)
	if err := repo.Upsert(ctx, mk("e_repo", `{"sections":[{"title":"A","fields":["x"]}]}`)); err != nil {
		t.Fatalf("repo.Upsert: %v", err)
	}
	// tx path (what publish does)
	tx, err := sdb.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpsertLayoutExec(ctx, tx, mk("e_tx", `{"sections":[{"title":"A","fields":["x"]}]}`), time.Time{}); err != nil {
		t.Fatalf("UpsertLayoutExec(tx): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a, okA := layoutRead(t, sdb, tenant, "e_repo", "edit")
	b, okB := layoutRead(t, sdb, tenant, "e_tx", "edit")
	if !okA || !okB {
		t.Fatalf("rows missing: repo=%v tx=%v", okA, okB)
	}
	if a.JSON != b.JSON {
		t.Errorf("layout_json differs: repo %s, tx %s", a.JSON, b.JSON)
	}
	if d := b.UpdatedAt.Sub(a.UpdatedAt); d > 5*time.Second || d < -5*time.Second {
		t.Errorf("zero `at` must stamp NOW(): repo %v tx %v", a.UpdatedAt, b.UpdatedAt)
	}

	// re-save keeps the existing id (unique key), changes the body
	again := mk("e_tx", `{"sections":[]}`)
	if err := UpsertLayoutExec(ctx, sdb, again, time.Time{}); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	c, _ := layoutRead(t, sdb, tenant, "e_tx", "edit")
	if c.ID != b.ID || c.JSON == b.JSON {
		t.Errorf("re-save must keep id %s and replace the body, got id %s json %s", b.ID, c.ID, c.JSON)
	}
}

func TestUpsertLayoutExec_ExplicitUpdatedAtBeatsOnUpdate(t *testing.T) {
	sdb, _ := layoutConn(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	mark := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	l := &LayoutDefinition{ID: uuid.NewString(), TenantID: tenant, EntityName: "e", ViewType: "edit", LayoutJSON: `{"sections":[]}`}

	if err := UpsertLayoutExec(ctx, sdb, l, mark); err != nil { // insert path
		t.Fatalf("insert: %v", err)
	}
	r, _ := layoutRead(t, sdb, tenant, "e", "edit")
	if !r.UpdatedAt.Equal(mark) {
		t.Errorf("insert updated_at = %v, want the explicit mark %v", r.UpdatedAt, mark)
	}
	l.LayoutJSON = `{"sections":[{"title":"B","fields":["y"]}]}`
	mark2 := mark.Add(10 * time.Minute)
	if err := UpsertLayoutExec(ctx, sdb, l, mark2); err != nil { // ON DUPLICATE KEY path: ON UPDATE would say NOW()
		t.Fatalf("update: %v", err)
	}
	r, _ = layoutRead(t, sdb, tenant, "e", "edit")
	if !r.UpdatedAt.Equal(mark2) {
		t.Errorf("update updated_at = %v, want the explicit mark %v (ON UPDATE CURRENT_TIMESTAMP must not win)", r.UpdatedAt, mark2)
	}
}

func TestDeleteLayoutExec_TenantPinned(t *testing.T) {
	sdb, gdb := layoutConn(t)
	repo := NewLayoutRepository(gdb)
	ctx := context.Background()
	tenant, other := uuid.NewString(), uuid.NewString()
	l := &LayoutDefinition{ID: uuid.NewString(), TenantID: tenant, EntityName: "e", ViewType: "list", LayoutJSON: `{"columns":[]}`}
	if err := repo.Upsert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if n, err := DeleteLayoutExec(ctx, sdb, other, l.ID); err != nil || n != 0 {
		t.Fatalf("another tenant's delete: n=%d err=%v, want 0 rows", n, err)
	}
	if n, err := DeleteLayoutByViewExec(ctx, sdb, other, "e", "list"); err != nil || n != 0 {
		t.Fatalf("another tenant's delete by view: n=%d err=%v, want 0 rows", n, err)
	}
	if _, ok := layoutRead(t, sdb, tenant, "e", "list"); !ok {
		t.Fatal("row vanished after a cross-tenant delete")
	}
	if n, err := DeleteLayoutByViewExec(ctx, sdb, tenant, "e", "list"); err != nil || n != 1 {
		t.Fatalf("delete by view: n=%d err=%v", n, err)
	}
	if err := repo.HardDelete(ctx, tenant, l.ID); err != ErrLayoutNotFound {
		t.Fatalf("HardDelete of a gone row: %v, want ErrLayoutNotFound", err)
	}
}
