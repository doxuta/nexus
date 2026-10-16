//go:build integration

// HIDDEN-COL-SIGNAL integration: List's HiddenColumns count must ride the REAL FieldACLService
// (sys_field_acl JOIN over a freshly-migrated throwaway MySQL DB), wired into RecordService
// exactly as main.go does (WithFieldACL). It proves what the unit tests (fakes) cannot:
//   - the real loadRestrictions set (can_view=0 rows) drives the count,
//   - a STALE sys_field_acl row naming a field never declared on the entity does NOT inflate it
//     (the count intersects with declared specs — the chip must match the strip exactly),
//   - the SAME request that reports the count also strips those fields from every row
//     (one ACL path — count and strip can never disagree),
//   - admin bypass and an unrestricted role both yield 0 (handler then omits meta entirely).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run HiddenColSignal_Integration
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestHiddenColSignal_Integration_ListCountsRealFieldACL(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000c1"
	restrictedRole := uuid.NewString()
	openRole := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-hidsig")
	flsSeedRole(t, sdb, restrictedRole, tenant, "sales-restricted")
	flsSeedRole(t, sdb, openRole, tenant, "sales-open")

	// deals{name, amount, stage} — amount+stage will be hidden from the restricted role.
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "deals",
		Label: "Deals",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "stage", Label: "Stage", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity deals: %v", err)
	}

	// can_view=0 on TWO declared fields + ONE stale row naming a field that does not exist on
	// the entity (e.g. left behind by a later field delete). The stale row must not count.
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "deals", "amount", false, false)
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "deals", "stage", false, false)
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "deals", "ghost_deleted", false, false)

	admin := flsAdmin(user)
	if _, err := recordSvc.Create(ctx, admin, tenant, user, "deals", map[string]any{
		"name": "Deal One", "amount": 1200.5, "stage": "open",
	}); err != nil {
		t.Fatalf("admin Create deal: %v", err)
	}

	// (1) Restricted role: HiddenColumns == 2 (amount+stage; ghost_deleted ignored) AND the
	// same request's rows have those exact fields stripped — count and strip share one set.
	lr, err := recordSvc.List(ctx, flsRoleScope(user, restrictedRole), tenant, "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("restricted List: %v", err)
	}
	if lr.HiddenColumns != 2 {
		t.Errorf("restricted HiddenColumns = %d; want 2 (amount+stage; stale ghost_deleted excluded)", lr.HiddenColumns)
	}
	if len(lr.Records) != 1 {
		t.Fatalf("restricted List records = %d; want 1", len(lr.Records))
	}
	row := lr.Records[0].Data
	if _, leaked := row["amount"]; leaked {
		t.Errorf("hidden field 'amount' leaked into row data: %v", row)
	}
	if _, leaked := row["stage"]; leaked {
		t.Errorf("hidden field 'stage' leaked into row data: %v", row)
	}
	if _, ok := row["name"]; !ok {
		t.Errorf("visible field 'name' missing from row data: %v", row)
	}

	// (2) A role with NO field restrictions → 0 (handler omits meta).
	lrOpen, err := recordSvc.List(ctx, flsRoleScope(user, openRole), tenant, "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("open List: %v", err)
	}
	if lrOpen.HiddenColumns != 0 {
		t.Errorf("open-role HiddenColumns = %d; want 0", lrOpen.HiddenColumns)
	}

	// (3) Admin bypasses field-ACL → 0 despite the seeded restrictions (no false-positive chip).
	lrAdmin, err := recordSvc.List(ctx, admin, tenant, "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("admin List: %v", err)
	}
	if lrAdmin.HiddenColumns != 0 {
		t.Errorf("admin HiddenColumns = %d; want 0", lrAdmin.HiddenColumns)
	}
	if _, ok := lrAdmin.Records[0].Data["amount"]; !ok {
		t.Errorf("admin must still see 'amount': %v", lrAdmin.Records[0].Data)
	}
}
