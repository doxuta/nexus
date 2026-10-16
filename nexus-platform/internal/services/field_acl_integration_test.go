//go:build integration

// Integration test for Field-Level-Security M1 (RM-42 / A6), exercised end-to-end through the
// RecordService over a freshly-migrated throwaway MySQL DB (o3aSetup). Unlike the unit test
// (field_acl_test.go, in-memory fake loader) this drives the REAL auth.FieldACLRepository against
// real sys_field_acl + acl_roles rows, so it proves the tenant-pinned JOIN query and the wired
// read-strip / write-guard / anti-oracle paths behave correctly on a live engine.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run FLS_Integration
//
// Covered (spec acceptance criteria):
//   - A non-admin role with sys_field_acl(can_view=0) on 'revenue' does NOT see 'revenue' on
//     Get and List (the field is stripped from every record's data).
//   - That same role cannot write 'revenue' (Create or Update; can_view=0 implies un-editable) —
//     refused as RECORD_UNKNOWN_FIELD, the answer an undeclared name gets (queue #317, §4.4b).
//   - Anti-oracle: the role cannot FILTER on 'revenue' — LIST_UNKNOWN_FILTER, same reason.
//   - An admin caller sees 'revenue' in full and may edit it (bypass).
//   - Tenant isolation: tenant B's restriction on 'revenue' does NOT leak into tenant A — A's
//     (differently-restricted) role sees only what A's grants dictate, never B's.
package services

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// flsWire builds a full RecordService with the REAL field-ACL repository wired over the same
// throwaway DB (gorm side), plus an EntityService for creating entities. It returns the wired
// services and the sqlx handle so the test can seed acl_roles / sys_field_acl rows directly.
func flsWire(t *testing.T) (*EntityService, *RecordService, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(newCountingCatalog(metaRepo), DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	fieldACLRepo := auth.NewFieldACLRepository(gdb)
	// WithLabelFetcher: the same *dynrepo.Repository main.go wires, so a report/list expand in
	// these tests resolves REAL labels under the REAL field-ACL (REPORT-RELATION-LABELS).
	recordSvc := NewRecordService(mc, dynRepo).
		WithLabelFetcher(dynRepo).
		WithFieldACL(NewFieldACLService(fieldACLRepo))
	return entitySvc, recordSvc, sdb
}

// flsSeedTenant inserts a tenants row (FK target for acl_roles). Defined locally so this file
// does not depend on a seeder from another package's test.
func flsSeedTenant(t *testing.T, db *sqlx.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', NOW(3), NOW(3))`,
		id, slug, slug); err != nil {
		t.Fatalf("flsSeedTenant: %v", err)
	}
}

// flsSeedRole inserts an acl_roles row (tenant-scoped). The role is the principal whose field
// grants the test exercises.
func flsSeedRole(t *testing.T, db *sqlx.DB, roleID, tenantID, name string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at) VALUES (?, ?, ?, ?, NOW(3), NOW(3))`,
		roleID, tenantID, name, name); err != nil {
		t.Fatalf("flsSeedRole: %v", err)
	}
}

// flsSeedFieldGrant inserts a sys_field_acl row restricting (tenant, role, entity, field).
func flsSeedFieldGrant(t *testing.T, db *sqlx.DB, tenantID, roleID, entity, field string, canView, canEdit bool) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NOW(3), NOW(3))`,
		uuid.NewString(), tenantID, roleID, entity, field, canView, canEdit); err != nil {
		t.Fatalf("flsSeedFieldGrant: %v", err)
	}
}

// flsRole builds a non-admin record scope bound to roleID with full row visibility (so the test
// isolates the FIELD dimension from the A5 ROW dimension).
func flsRoleScope(userID, roleID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: false, RoleID: roleID}
}

// flsAdmin builds an admin record scope (bypasses field-level security entirely).
func flsAdmin(userID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: true}
}

// fieldForbidden reports whether err is a *ValidationError with code FIELD_FORBIDDEN (→ 403).
func fieldForbidden(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Code == "FIELD_FORBIDDEN"
}

func TestFLS_Integration_HideAndLockRevenue(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000aa"
	roleID := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-fls")
	flsSeedRole(t, sdb, roleID, tenant, "sales")

	// Entity 'accounts' with a public 'name' and a sensitive 'revenue' field.
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "revenue", Label: "Revenue", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Restrict 'revenue' for the sales role: can_view=0 (also implies un-editable).
	flsSeedFieldGrant(t, sdb, tenant, roleID, "accounts", "revenue", false, false)

	// Admin seeds a record WITH revenue (admin bypasses the write guard).
	admin := flsAdmin(user)
	id, err := recordSvc.Create(ctx, admin, tenant, user, "accounts",
		map[string]any{"name": "Acme", "revenue": float64(9000)})
	if err != nil {
		t.Fatalf("admin Create: %v", err)
	}

	role := flsRoleScope(user, roleID)

	// (1a) READ via Get: the role must NOT see 'revenue'.
	rec, err := recordSvc.Get(ctx, role, tenant, "accounts", id, nil, nil)
	if err != nil {
		t.Fatalf("role Get: %v", err)
	}
	if _, ok := rec.Data["revenue"]; ok {
		t.Fatalf("BLEED: role sees hidden 'revenue' on Get: %v", rec.Data)
	}
	if v, ok := rec.Data["name"]; !ok || v != "Acme" {
		t.Fatalf("role must still see 'name': %v", rec.Data)
	}

	// (1b) READ via List: same strip applies per row.
	lr, err := recordSvc.List(ctx, role, tenant, "accounts", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("role List: %v", err)
	}
	if len(lr.Records) != 1 {
		t.Fatalf("role List want 1 record, got %d", len(lr.Records))
	}
	if _, ok := lr.Records[0].Data["revenue"]; ok {
		t.Fatalf("BLEED: role sees hidden 'revenue' on List: %v", lr.Records[0].Data)
	}

	// (2a) WRITE via Update: touching 'revenue' → refused as an UNKNOWN field (queue #317: a 403
	// FIELD_FORBIDDEN would confirm the hidden name exists).
	if err := recordSvc.Update(ctx, role, tenant, "accounts", id,
		map[string]any{"revenue": float64(1)}, nil); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("role Update revenue must be RECORD_UNKNOWN_FIELD, got: %v", err)
	}
	// The role CAN update a non-restricted field.
	if err := recordSvc.Update(ctx, role, tenant, "accounts", id,
		map[string]any{"name": "Acme Renamed"}, nil); err != nil {
		t.Fatalf("role Update name must succeed, got: %v", err)
	}

	// (2b) WRITE via Create: a new record carrying 'revenue' → 403.
	if _, err := recordSvc.Create(ctx, role, tenant, user, "accounts",
		map[string]any{"name": "Beta", "revenue": float64(5)}); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("role Create with revenue must be RECORD_UNKNOWN_FIELD, got: %v", err)
	}

	// (3) ANTI-ORACLE: filtering on the hidden field → 403 (no count-based reconstruction).
	if _, err := recordSvc.List(ctx, role, tenant, "accounts", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "revenue", Operator: "gt", Values: []string{"100"}}},
	}); codeOf(err) != "LIST_UNKNOWN_FILTER" { // same answer as a field that does not exist (queue #317)
		t.Fatalf("role filtering on hidden 'revenue' must be LIST_UNKNOWN_FILTER, got: %v", err)
	}

	// (4) ADMIN bypass: admin sees revenue in full and may edit it.
	arec, err := recordSvc.Get(ctx, admin, tenant, "accounts", id, nil, nil)
	if err != nil {
		t.Fatalf("admin Get: %v", err)
	}
	if _, ok := arec.Data["revenue"]; !ok {
		t.Fatalf("admin must see 'revenue': %v", arec.Data)
	}
	if err := recordSvc.Update(ctx, admin, tenant, "accounts", id,
		map[string]any{"revenue": float64(12345)}, nil); err != nil {
		t.Fatalf("admin Update revenue must succeed, got: %v", err)
	}
}

// TestFLS_Integration_TenantIsolation proves a 'revenue' restriction defined for a role in tenant
// B does NOT leak into tenant A: A's role (with its OWN, different grants) sees exactly what A's
// grants dictate, never B's. Both tenants share the physical obj_accounts table (shared-table
// §8), so this also guards the read-strip against cross-tenant grant bleed.
func TestFLS_Integration_TenantIsolation(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenantA = o3aTenantA
	const tenantB = o3aTenantB
	const userA = "11111111-0000-0000-0000-0000000000a1"
	const userB = "11111111-0000-0000-0000-0000000000b1"
	roleA := uuid.NewString()
	roleB := uuid.NewString()

	flsSeedTenant(t, sdb, tenantA, "tenant-a-iso")
	flsSeedTenant(t, sdb, tenantB, "tenant-b-iso")
	flsSeedRole(t, sdb, roleA, tenantA, "sales-a")
	flsSeedRole(t, sdb, roleB, tenantB, "sales-b")

	// Both tenants define 'accounts' with name + revenue (shared obj_accounts table).
	for _, tn := range []string{tenantA, tenantB} {
		if _, _, err := entitySvc.CreateEntity(ctx, tn, CreateEntityInput{
			Name:  "accounts",
			Label: "Accounts",
			Fields: []FieldInput{
				{Name: "name", Label: "Name", FieldType: "text"},
				{Name: "revenue", Label: "Revenue", FieldType: "number"},
			},
		}); err != nil {
			t.Fatalf("CreateEntity tenant=%s: %v", tn, err)
		}
	}

	// ONLY tenant B hides 'revenue' for roleB. Tenant A defines NO restriction → default-allow.
	flsSeedFieldGrant(t, sdb, tenantB, roleB, "accounts", "revenue", false, false)

	// Admin seeds one record in EACH tenant (separate rows, same shared table).
	idA, err := recordSvc.Create(ctx, flsAdmin(userA), tenantA, userA, "accounts",
		map[string]any{"name": "A-Corp", "revenue": float64(111)})
	if err != nil {
		t.Fatalf("seed A record: %v", err)
	}
	if _, err := recordSvc.Create(ctx, flsAdmin(userB), tenantB, userB, "accounts",
		map[string]any{"name": "B-Corp", "revenue": float64(222)}); err != nil {
		t.Fatalf("seed B record: %v", err)
	}

	// Tenant A's role has NO restriction → it MUST see 'revenue' (B's grant must not bleed in).
	recA, err := recordSvc.Get(ctx, flsRoleScope(userA, roleA), tenantA, "accounts", idA, nil, nil)
	if err != nil {
		t.Fatalf("A role Get: %v", err)
	}
	if v, ok := recA.Data["revenue"]; !ok || v == nil {
		t.Fatalf("TENANT BLEED: A's role lost 'revenue' due to B's grant: %v", recA.Data)
	}

	// Tenant B's role (with the hide grant) must NOT see 'revenue' on B's own record — and only
	// B's records exist in B's tenant scope, never A's row (tenant_id filter on the shared table).
	lrB, err := recordSvc.List(ctx, flsRoleScope(userB, roleB), tenantB, "accounts", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("B role List: %v", err)
	}
	if len(lrB.Records) != 1 {
		t.Fatalf("B tenant scope must show exactly its 1 record (no A bleed), got %d", len(lrB.Records))
	}
	if _, ok := lrB.Records[0].Data["revenue"]; ok {
		t.Fatalf("B's role must NOT see hidden 'revenue': %v", lrB.Records[0].Data)
	}
	if name, _ := lrB.Records[0].Data["name"].(string); name != "B-Corp" {
		t.Fatalf("CROSS-TENANT ROW BLEED: B saw a non-B record: %v", lrB.Records[0].Data)
	}
}

// TestFLS_Integration_HiddenFieldFormResendSaves pins the FORM-SHAPED update. The edit form seeds
// a hidden field EMPTY (the read strips it) and buildPayload's update mode resends every empty
// optional field as an explicit null — so a role with 'revenue' hidden used to get 403
// FIELD_FORBIDDEN on EVERY save of a record whose revenue held a value (nil ≠ 9000): hiding one
// field took the whole record away. A null for a field the caller never saw is the absence of a
// value, not an intent to clear it — the save must succeed, the stored value must survive
// untouched (the key is dropped before the UPDATE, never NULLed), and a NON-nil value for the
// hidden field must still be refused.
func TestFLS_Integration_HiddenFieldFormResendSaves(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000ab"
	roleID := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-fls-resend")
	flsSeedRole(t, sdb, roleID, tenant, "sales")

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "revenue", Label: "Revenue", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	flsSeedFieldGrant(t, sdb, tenant, roleID, "accounts", "revenue", false, false)

	admin := flsAdmin(user)
	id, err := recordSvc.Create(ctx, admin, tenant, user, "accounts",
		map[string]any{"name": "Acme", "revenue": float64(9000)})
	if err != nil {
		t.Fatalf("admin Create: %v", err)
	}

	role := flsRoleScope(user, roleID)

	// The form-shaped payload: EVERY field, the hidden one as null because the form never saw it.
	if err := recordSvc.Update(ctx, role, tenant, "accounts", id,
		map[string]any{"name": "Acme 2", "revenue": nil}, nil); err != nil {
		t.Fatalf("role Update with form-shaped payload (hidden 'revenue' resent as null) must succeed, got: %v", err)
	}

	// The hidden value is intact (not NULLed by the resend) and the visible edit landed.
	rec, err := recordSvc.Get(ctx, admin, tenant, "accounts", id, nil, nil)
	if err != nil {
		t.Fatalf("admin Get: %v", err)
	}
	if got := fmt.Sprint(rec.Data["revenue"]); got != "9000" {
		t.Fatalf("hidden 'revenue' must be untouched (9000) after the form resend, got %q (data=%v)", got, rec.Data)
	}
	if rec.Data["name"] != "Acme 2" {
		t.Fatalf("visible 'name' must be updated to 'Acme 2', got %v", rec.Data["name"])
	}

	// A NON-nil value for the hidden field is an edit the role may not make → still refused.
	if err := recordSvc.Update(ctx, role, tenant, "accounts", id,
		map[string]any{"revenue": float64(1)}, nil); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("role Update with a value for hidden 'revenue' must be RECORD_UNKNOWN_FIELD, got: %v", err)
	}

	// The STORED value is refused too. Update reads current UNSTRIPPED, so accepting an equal
	// guess (200) while refusing every other value (403) would be an equality oracle on the very
	// field the role may not see.
	if err := recordSvc.Update(ctx, role, tenant, "accounts", id,
		map[string]any{"revenue": float64(9000)}, nil); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("role Update resending the stored value for hidden 'revenue' must be RECORD_UNKNOWN_FIELD (equality oracle), got: %v", err)
	}
}
