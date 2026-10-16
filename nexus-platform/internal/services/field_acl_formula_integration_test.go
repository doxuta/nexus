//go:build integration

// Queue #316 on a live engine: a role that may not view `amount` must not be served
// `weighted_value = amount * probability / 100` either — probability is visible, so the pair would
// give the hidden number back. Drives the REAL repository + the cached MetaCache catalog (the same
// wiring main.go builds with newFieldACL), through RecordService Get/List/Aggregate.
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func TestFLS_Integration_FormulaOverHiddenOperandIsHidden(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(newCountingCatalog(metaRepo), DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	recordSvc := NewRecordService(mc, dynRepo).
		WithFieldACL(NewFieldACLService(auth.NewFieldACLRepository(gdb)).WithCatalog(mc))
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000aa"
	roleID := uuid.NewString()
	flsSeedTenant(t, sdb, tenant, "tenant-a-fls-formula")
	flsSeedRole(t, sdb, roleID, tenant, "sales")

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "opps", Label: "Opps",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "probability", Label: "Probability", FieldType: "number"},
			{Name: "weighted_value", Label: "Weighted", FieldType: "formula",
				Options: map[string]any{"formula": "amount * probability / 100", "resultType": "number"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	flsSeedFieldGrant(t, sdb, tenant, roleID, "opps", "amount", false, false)

	admin := flsAdmin(user)
	id, err := recordSvc.Create(ctx, admin, tenant, user, "opps",
		map[string]any{"name": "Deal", "amount": float64(140000000), "probability": float64(10)})
	if err != nil {
		t.Fatalf("admin Create: %v", err)
	}

	role := flsRoleScope(user, roleID)
	rec, err := recordSvc.Get(ctx, role, tenant, "opps", id, nil, nil)
	if err != nil {
		t.Fatalf("role Get: %v", err)
	}
	for _, leaked := range []string{"amount", "weighted_value"} {
		if _, ok := rec.Data[leaked]; ok {
			t.Fatalf("role Get leaks %q: %v", leaked, rec.Data)
		}
	}
	if rec.Data["probability"] == nil {
		t.Fatalf("visible probability must survive: %v", rec.Data)
	}

	list, err := recordSvc.List(ctx, role, tenant, "opps", ListQuery{Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("role List: %v", err)
	}
	for _, r := range list.Records {
		if _, ok := r.Data["weighted_value"]; ok {
			t.Fatalf("role List leaks weighted_value: %v", r.Data)
		}
	}
	if list.HiddenColumns != 2 {
		t.Fatalf("HiddenColumns = %d, want 2 (amount + the formula derived from it)", list.HiddenColumns)
	}

	// Admin keeps the whole picture.
	arec, err := recordSvc.Get(ctx, admin, tenant, "opps", id, nil, nil)
	if err != nil {
		t.Fatalf("admin Get: %v", err)
	}
	if arec.Data["weighted_value"] == nil || arec.Data["amount"] == nil {
		t.Fatalf("admin must see amount and weighted_value: %v", arec.Data)
	}
}
