//go:build integration

// specs/063-draft-publish S7-9 (T127): SoftDeleteRecordsAt stamps ONE caller-chosen instant on every
// row of ONE tenant, reports how many rows moved, and keeps the RM-28 pivot release in the same tx.
package dynrepo

import (
	"context"
	"testing"
	"time"
)

func TestSoftDeleteRecordsAt(t *testing.T) {
	const entity = "it_sweep_at"
	const tenant = "11111111-1111-1111-1111-1111111111b1"
	const other = "11111111-1111-1111-1111-1111111111b2"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()
	for _, e := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": e}, emailUnique); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	if _, err := repo.InsertRecord(ctx, entity, other, user, map[string]any{"email": "z@x.com"}, emailUnique); err != nil {
		t.Fatalf("InsertRecord other tenant: %v", err)
	}
	// one row already in the trash: it must keep its own stamp (WHERE deleted_at IS NULL)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := h.Exec("UPDATE `obj_"+entity+"` SET deleted_at = ? WHERE tenant_id = ? AND `email` = 'c@x.com'", old, tenant); err != nil {
		t.Fatalf("pre-trash: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Second).Add(-90 * time.Minute)
	n, err := repo.SoftDeleteRecordsAt(ctx, entity, tenant, at)
	if err != nil {
		t.Fatalf("SoftDeleteRecordsAt: %v", err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want 2 (the live rows of THIS tenant only)", n)
	}
	var stamped, otherLive, oldKept int
	_ = h.Get(&stamped, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE tenant_id = ? AND deleted_at = ?", tenant, at)
	_ = h.Get(&otherLive, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE tenant_id = ? AND deleted_at IS NULL", other)
	_ = h.Get(&oldKept, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE tenant_id = ? AND deleted_at = ?", tenant, old)
	if stamped != 2 {
		t.Errorf("rows stamped with the given instant = %d, want 2", stamped)
	}
	if otherLive != 1 {
		t.Errorf("other tenant's live rows = %d, want 1 (cross-tenant sweep)", otherLive)
	}
	if oldKept != 1 {
		t.Errorf("already-trashed row kept its stamp = %d, want 1", oldKept)
	}
	if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
		t.Errorf("pivot after sweep = %v, want [] (same tx as the sweep)", got)
	}
	// a second pass with a different instant is a no-op: nothing live is left (idempotent retry)
	n2, err := repo.SoftDeleteRecordsAt(ctx, entity, tenant, at.Add(time.Hour))
	if err != nil || n2 != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
	}

	// the legacy entry point still sweeps with the DB clock
	if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "d@x.com"}, emailUnique); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	if err := repo.SoftDeleteRecords(ctx, entity, tenant); err != nil {
		t.Fatalf("SoftDeleteRecords: %v", err)
	}
	var left int
	_ = h.Get(&left, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE tenant_id = ? AND deleted_at IS NULL", tenant)
	if left != 0 {
		t.Errorf("live rows after SoftDeleteRecords = %d, want 0", left)
	}
}
