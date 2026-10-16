//go:build integration

// EXP-01 integration: ?expand must gate the resolved labelField against the caller's FIELD-level
// ACL on the TARGET entity, end-to-end over a freshly-migrated throwaway MySQL DB. This drives the
// REAL FieldACLService (sys_field_acl JOIN) + the REAL dynrepo BatchFetchLabels, wired into
// RecordService exactly as main.go does (WithLabelFetcher + WithFieldACL). It proves the wiring the
// unit tests (applyExpand with fakes) cannot: that HiddenFields on the TARGET entity really carries
// the labelField, and that RecordService passes s.fieldACL into applyExpand on both Get and List.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run EXP01_Integration
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// exp01Wire builds a RecordService with the REAL field-ACL repository AND the label fetcher wired
// over the same throwaway DB — the composition ?expand needs to resolve labels AND gate them by
// field-ACL. It deliberately leaves the entity-level authorizer (WithACLAuthorizer) UNWIRED so the
// test isolates the FIELD-level gate: entity view is implicitly allowed, and the ONLY thing that
// can narrow the label is the sys_field_acl grant on the target's labelField.
func exp01Wire(t *testing.T) (*EntityService, *RecordService, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	fieldACLRepo := auth.NewFieldACLRepository(gdb)
	recordSvc := NewRecordService(mc, dynRepo).
		WithLabelFetcher(dynRepo).
		WithFieldACL(NewFieldACLService(fieldACLRepo))
	return entitySvc, recordSvc, sdb
}

func TestEXP01_Integration_ExpandLabelGatedByFieldACL(t *testing.T) {
	entitySvc, recordSvc, sdb := exp01Wire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000e1"
	roleID := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-exp01")
	flsSeedRole(t, sdb, roleID, tenant, "sales")

	// accounts{name}; contacts{name, account→accounts}. The relation's labelField defaults to
	// "name" (the target entity's label field).
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation", Options: map[string]any{"relatedEntity": "accounts"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity contacts: %v", err)
	}

	// Restrict the TARGET labelField: role can_view=0 on accounts.name.
	flsSeedFieldGrant(t, sdb, tenant, roleID, "accounts", "name", false, false)

	admin := flsAdmin(user)
	accID, err := recordSvc.Create(ctx, admin, tenant, user, "accounts", map[string]any{"name": "Acme Corp"})
	if err != nil {
		t.Fatalf("admin Create account: %v", err)
	}
	contactID, err := recordSvc.Create(ctx, admin, tenant, user, "contacts", map[string]any{"name": "Jane Doe", "account": accID})
	if err != nil {
		t.Fatalf("admin Create contact: %v", err)
	}

	role := flsRoleScope(user, roleID)

	// (1) Get as the restricted role with ?expand=account → the label (accounts.name) must be
	// withheld: the expand resolves to {id} only, never {id, "Acme Corp"}.
	rec, err := recordSvc.Get(ctx, role, tenant, "contacts", contactID, []string{"account"}, nil)
	if err != nil {
		t.Fatalf("role Get: %v", err)
	}
	switch v := rec.Data["account"].(type) {
	case expandedIDOnly:
		if v.ID != accID {
			t.Fatalf("expandedIDOnly.ID = %q; want %q", v.ID, accID)
		}
	case expandedValue:
		t.Fatalf("LEAK: role received the denied labelField via expand: %+v", v)
	default:
		t.Fatalf("account expanded to unexpected shape %T: %v", v, v)
	}

	// (2) List as the restricted role (bulk harvester) → same {id}-only shape per row.
	lr, err := recordSvc.List(ctx, role, tenant, "contacts", ListQuery{Page: 1, Limit: 20, Expand: []string{"account"}})
	if err != nil {
		t.Fatalf("role List: %v", err)
	}
	if len(lr.Records) != 1 {
		t.Fatalf("role List want 1 record, got %d", len(lr.Records))
	}
	if v, ok := lr.Records[0].Data["account"].(expandedIDOnly); !ok || v.ID != accID {
		t.Fatalf("LEAK on List: account=%#v; want expandedIDOnly{%q}", lr.Records[0].Data["account"], accID)
	}

	// (3) Admin bypasses field-ACL → the label resolves in full.
	arec, err := recordSvc.Get(ctx, admin, tenant, "contacts", contactID, []string{"account"}, nil)
	if err != nil {
		t.Fatalf("admin Get: %v", err)
	}
	ev, ok := arec.Data["account"].(expandedValue)
	if !ok || ev.Label != "Acme Corp" {
		t.Fatalf("admin expand: account=%#v; want expandedValue{label:\"Acme Corp\"}", arec.Data["account"])
	}
}
