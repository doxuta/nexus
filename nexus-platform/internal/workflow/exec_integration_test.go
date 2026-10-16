//go:build integration

package workflow

// exec_integration_test.go — specs/063-draft-publish T023: the *Exec statements (what publish runs
// on its transaction) write exactly the rows the admin repository writes, tenant-pinned.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func execDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("workflow: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// columns are the persisted content columns a write decides (no id, no timestamps).
type wfColumns struct {
	Name            string  `db:"name"`
	EntityName      string  `db:"entity_name"`
	Trigger         string  `db:"trigger_event"`
	Condition       *string `db:"condition"`
	Actions         string  `db:"actions"`
	IsActive        bool    `db:"is_active"`
	IntervalSeconds int     `db:"interval_seconds"`
	CreatedBy       *string `db:"created_by"`
	UpdatedBy       *string `db:"updated_by"`
	Deleted         bool    `db:"del"`
}

func readCols(t *testing.T, db *sqlx.DB, tenant, id string) wfColumns {
	t.Helper()
	var c wfColumns
	if err := db.Get(&c, "SELECT name, entity_name, trigger_event, `condition`, actions, is_active, interval_seconds, created_by, updated_by, deleted_at IS NOT NULL AS del FROM sys_workflows WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return c
}

func sameCols(t *testing.T, label string, a, b wfColumns) {
	t.Helper()
	deref := func(s *string) string {
		if s == nil {
			return "<NULL>"
		}
		return *s
	}
	if a.Name != b.Name || a.EntityName != b.EntityName || a.Trigger != b.Trigger || deref(a.Condition) != deref(b.Condition) ||
		a.Actions != b.Actions || a.IsActive != b.IsActive || a.IntervalSeconds != b.IntervalSeconds ||
		deref(a.CreatedBy) != deref(b.CreatedBy) || deref(a.UpdatedBy) != deref(b.UpdatedBy) || a.Deleted != b.Deleted {
		t.Errorf("%s: repository wrote %+v, *Exec wrote %+v", label, a, b)
	}
}

func TestExec_WritesWhatTheRepositoryWrites(t *testing.T) {
	db := execDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()
	tenant := uuid.NewString()
	actor := uuid.NewString()

	in := CreateInput{TenantID: tenant, Name: "r", EntityName: "crm_x", Trigger: TriggerAfterUpdate,
		Condition: []byte(`{"rules":[{"field":"a","op":"eq","value":1}]}`), Actions: []byte(`[{"type":"in_app_notification"}]`),
		IsActive: true, IntervalSeconds: 0, CreatedBy: actor}

	// Create ⇄ InsertWithIDExec (on a tx, with a caller-chosen id).
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
	sameCols(t, "create", readCols(t, db, tenant, viaRepo), readCols(t, db, tenant, execID))

	// A rule with NO condition stores SQL NULL on both paths.
	noCond := in
	noCond.Condition = nil
	a, _ := repo.Create(ctx, noCond)
	b := uuid.NewString()
	if err := InsertWithIDExec(ctx, db, b, noCond, time.Now()); err != nil {
		t.Fatal(err)
	}
	ca, cb := readCols(t, db, tenant, a), readCols(t, db, tenant, b)
	if ca.Condition != nil || cb.Condition != nil {
		t.Fatalf("a nil condition must be SQL NULL on both paths: %v / %v", ca.Condition, cb.Condition)
	}
	sameCols(t, "create without condition", ca, cb)

	// Update ⇄ UpdateExec.
	up := UpdateInput{Name: "r2", EntityName: "crm_y", Trigger: TriggerScheduled, Condition: nil, Actions: []byte(`[{"type":"x"}]`),
		IsActive: false, IntervalSeconds: 300, UpdatedBy: actor}
	if err := repo.Update(ctx, tenant, viaRepo, up); err != nil {
		t.Fatal(err)
	}
	if err := UpdateExec(ctx, db, tenant, execID, up); err != nil {
		t.Fatal(err)
	}
	sameCols(t, "update", readCols(t, db, tenant, viaRepo), readCols(t, db, tenant, execID))

	// Soft delete ⇄ SoftDeleteExec; a second one finds nothing.
	if err := repo.Delete(ctx, tenant, viaRepo); err != nil {
		t.Fatal(err)
	}
	if err := SoftDeleteExec(ctx, db, tenant, execID, time.Now()); err != nil {
		t.Fatal(err)
	}
	sameCols(t, "delete", readCols(t, db, tenant, viaRepo), readCols(t, db, tenant, execID))
	if err := SoftDeleteExec(ctx, db, tenant, execID, time.Now()); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("second SoftDeleteExec: %v, want ErrWorkflowNotFound", err)
	}
	if err := UpdateExec(ctx, db, tenant, execID, up); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("UpdateExec on a deleted rule: %v, want ErrWorkflowNotFound (an update never resurrects)", err)
	}

	// RestoreExec brings the deleted row back under the same id with new content; a live row is not "restored".
	restored, err := RestoreExec(ctx, db, tenant, execID, UpdateInput{Name: "back", EntityName: "crm_x", Trigger: TriggerAfterCreate,
		Actions: []byte(`[{"type":"y"}]`), IsActive: true, UpdatedBy: actor})
	if err != nil || !restored {
		t.Fatalf("RestoreExec = %v, %v", restored, err)
	}
	if c := readCols(t, db, tenant, execID); c.Deleted || c.Name != "back" || !c.IsActive {
		t.Fatalf("restored row = %+v", c)
	}
	if again, _ := RestoreExec(ctx, db, tenant, execID, up); again {
		t.Fatal("RestoreExec restored a row that is live")
	}
}

// Every statement pins tenant_id: another tenant's id is "not found" and nothing changes.
func TestExec_TenantPinned(t *testing.T) {
	db := execDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()
	owner, intruder := uuid.NewString(), uuid.NewString()
	id, err := repo.Create(ctx, CreateInput{TenantID: owner, Name: "mine", EntityName: "e", Trigger: TriggerAfterCreate, Actions: []byte(`[]`), IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	up := UpdateInput{Name: "hijacked", EntityName: "e", Trigger: TriggerAfterCreate, Actions: []byte(`[]`), IsActive: false}
	if err := UpdateExec(ctx, db, intruder, id, up); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("UpdateExec across tenants: %v", err)
	}
	if err := SoftDeleteExec(ctx, db, intruder, id, time.Now()); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("SoftDeleteExec across tenants: %v", err)
	}
	if err := SetInactiveExec(ctx, db, intruder, id, ""); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("SetInactiveExec across tenants: %v", err)
	}
	if c := readCols(t, db, owner, id); c.Name != "mine" || !c.IsActive || c.Deleted {
		t.Fatalf("another tenant changed the row: %+v", c)
	}
	// Only the owner's SetInactiveExec switches it off, touching nothing else.
	actor := uuid.NewString()
	if err := SetInactiveExec(ctx, db, owner, id, actor); err != nil {
		t.Fatal(err)
	}
	c := readCols(t, db, owner, id)
	if c.IsActive || c.Name != "mine" || c.Trigger != string(TriggerAfterCreate) || c.UpdatedBy == nil || *c.UpdatedBy != actor {
		t.Fatalf("SetInactiveExec = %+v: must only switch off (and note who)", c)
	}
}
