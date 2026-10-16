//go:build integration

package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestBatchRecordExistsInScope_NarrowsToWhatTheCallerMayView proves queue #331 on real MySQL 8:
// the scoped probe sees a record exactly when GetRecord would (owner predicate), "not visible" is
// absent from the result like a missing id, a soft-deleted or cross-tenant id never resolves, and
// the tenant-wide BatchRecordExists (the system probe) is unchanged.
func TestBatchRecordExistsInScope_NarrowsToWhatTheCallerMayView(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_exists_scope"

	tenantA, tenantB := newID(), newID()
	rep, colleague, otherTenantUser := newID(), newID(), newID()
	seedTenant(t, db, tenantA, "it-exs-a-"+tenantA[:8])
	seedTenant(t, db, tenantB, "it-exs-b-"+tenantB[:8])
	seedUser(t, db, rep, tenantA)
	seedUser(t, db, colleague, tenantA)
	seedUser(t, db, otherTenantUser, tenantB)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, "", []string{rep, colleague}, nil)
		cleanupA5(db, tenantB, "", []string{otherTenantUser}, nil)
	})
	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	insert := func(tenant, user string) string {
		id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "x"}, nil)
		if err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		return id
	}
	mine, theirs, foreign, deleted := insert(tenantA, rep), insert(tenantA, colleague), insert(tenantB, otherTenantUser), insert(tenantA, rep)
	if err := repo.SoftDeleteRecord(ctx, entity, tenantA, deleted, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	ids := []string{mine, theirs, foreign, deleted, "00000000-0000-0000-0000-000000000000"}

	got, err := repo.BatchRecordExistsInScope(ctx, entity, tenantA, ids, RowScope{UserID: rep, Level: AccessOwner})
	if err != nil {
		t.Fatalf("BatchRecordExistsInScope(owner): %v", err)
	}
	if len(got) != 1 || !got[mine] {
		t.Errorf("owner scope sees %v; want only {%s}", got, mine)
	}

	got, err = repo.BatchRecordExistsInScope(ctx, entity, tenantA, ids, RowScope{UserID: rep, Level: AccessAll})
	if err != nil {
		t.Fatalf("BatchRecordExistsInScope(all): %v", err)
	}
	if len(got) != 2 || !got[mine] || !got[theirs] {
		t.Errorf("all scope sees %v; want {mine, theirs} (no foreign tenant, no soft-deleted)", got)
	}

	got, err = repo.BatchRecordExistsInScope(ctx, entity, tenantA, ids, RowScope{UserID: rep, Level: AccessNone})
	if err != nil {
		t.Fatalf("BatchRecordExistsInScope(none): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("none scope sees %v; want nothing", got)
	}

	// The system probe stays tenant-wide.
	got, err = repo.BatchRecordExists(ctx, entity, tenantA, ids)
	if err != nil {
		t.Fatalf("BatchRecordExists: %v", err)
	}
	if len(got) != 2 || !got[mine] || !got[theirs] {
		t.Errorf("tenant-wide probe sees %v; want {mine, theirs}", got)
	}
}
