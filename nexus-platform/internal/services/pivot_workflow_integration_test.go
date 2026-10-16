//go:build integration

// RM-28 Slice A — the workflow field_update BYPASS test. workflow field_update mutates obj_ through
// store.UpdateRecord WITHOUT going through RecordService.Update, so the pivot maintenance MUST live
// at the repository choke-point (dynrepo), not in RecordService. This wires a real
// WorkflowRecordMutator over the real *dynrepo.Repository + real metadata catalog (exactly like
// production, minus the TTL cache) and proves that a unique-field field_update maintains the pivot —
// if pivot maintenance had been placed in RecordService, this path would silently skip it.
//
// Run with a freshly-migrated throwaway MySQL (o3aSetup):
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run TestPivotWorkflowBypass -race -count=1 -v
package services

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// pivotEmailValues reads the sorted pivot values for (tenant, entity, "email") straight from
// sys_unique_index — the source of truth for what uniqueness the app is enforcing per tenant.
func pivotEmailValues(t *testing.T, sdb *sqlx.DB, tenant, entity string) []string {
	t.Helper()
	var vals []string
	if err := sdb.Select(&vals,
		"SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? ORDER BY `value`",
		tenant, entity, "email"); err != nil {
		t.Fatalf("pivot query: %v", err)
	}
	return vals
}

func TestPivotWorkflowBypass(t *testing.T) {
	ctx := context.Background()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo)
	// The workflow adapter as production wires it: real store, real catalog (production uses the
	// TTL'd MetaCache, but metaRepo satisfies EntityCatalog identically for this test).
	mutator := NewWorkflowRecordMutator(dynRepo).WithCatalog(metaRepo)

	const tenant = engTenantA
	const user = "00000000-0000-0000-0000-0000000000w1"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "email", Label: "Email", FieldType: "text", IsUnique: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Seed a record through the normal service path — the pivot starts at [orig@x.com].
	id, err := recordSvc.Create(ctx, engAdmin(user), tenant, user, "contacts",
		map[string]any{"name": "Orig", "email": "orig@x.com"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := pivotEmailValues(t, sdb, tenant, "contacts"); len(got) != 1 || got[0] != "orig@x.com" {
		t.Fatalf("after Create: pivot = %v; want [orig@x.com]", got)
	}

	// The BYPASS: workflow field_update sets email through store.UpdateRecord directly. If pivot
	// maintenance lived in RecordService this would leave the pivot stale at orig@x.com.
	if err := mutator.UpdateField(ctx, "contacts", tenant, id, "system-actor", "email", "wf@x.com"); err != nil {
		t.Fatalf("workflow UpdateField: %v", err)
	}
	if got := pivotEmailValues(t, sdb, tenant, "contacts"); len(got) != 1 || got[0] != "wf@x.com" {
		t.Fatalf("after workflow field_update: pivot = %v; want [wf@x.com] (bypass maintained the pivot)", got)
	}
}
