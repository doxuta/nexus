//go:build integration

// queue #259 end-to-end over real MySQL: EditField reads the edit layout through the REAL
// metadata.LayoutRepository (tenant-pinned) and refuses isRequired=true on a field the layout shows
// only under a visibleWhen condition; another tenant's layout for a same-named entity is not read;
// dropping the condition lets the same edit through.
package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func TestFieldEditVisibleWhenGuard_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	entitySvc := NewEntityService(dynrepo.New(sdb), metaRepo)
	fieldSvc := NewFieldService(dynrepo.New(sdb), metaRepo).WithLayoutReader(layoutRepo)

	const tenant, other = o3aTenantA, o3aTenantB
	flsSeedTenant(t, sdb, tenant, "tenant-a-259")
	flsSeedTenant(t, sdb, other, "tenant-b-259")

	fieldsOf := []FieldInput{
		{Name: "customer_type", Label: "Loại khách", FieldType: "picklist", Options: map[string]any{"values": []any{"individual", "business"}}},
		{Name: "tax_code", Label: "Mã số thuế", FieldType: "text"},
	}
	for _, tn := range []string{tenant, other} {
		if _, _, err := entitySvc.CreateEntity(ctx, tn, CreateEntityInput{Name: "leads", Label: "Leads", Fields: fieldsOf}); err != nil {
			t.Fatalf("CreateEntity(%s): %v", tn, err)
		}
	}
	ent, err := metaRepo.GetEntityByName(ctx, tenant, "leads")
	if err != nil {
		t.Fatalf("GetEntityByName: %v", err)
	}
	fields, err := metaRepo.ListFields(ctx, tenant, ent.ID)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}
	var taxID string
	for _, f := range fields {
		if f.Name == "tax_code" {
			taxID = f.ID
		}
	}

	save := func(tn, layoutJSON string) {
		t.Helper()
		if err := layoutRepo.Upsert(ctx, &metadata.LayoutDefinition{ID: uuid.NewString(), TenantID: tn, EntityName: "leads", ViewType: LayoutViewEdit, LayoutJSON: layoutJSON}); err != nil {
			t.Fatalf("Upsert layout(%s): %v", tn, err)
		}
	}
	// Tenant B hides tax_code behind a condition; tenant A's layout has none: A's edit must not be
	// refused because of B's layout.
	save(other, vwEditLayout)
	save(tenant, `{"sections":[{"title":"A","columns":1,"fields":["customer_type","tax_code"]}]}`)
	required := true
	if _, err := fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: taxID, IsRequired: &required}); err != nil {
		t.Fatalf("EditField with an unconditional cell (and a foreign tenant's conditional layout): %v", err)
	}
	notRequired := false
	if _, err := fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: taxID, IsRequired: &notRequired}); err != nil {
		t.Fatalf("EditField back to optional: %v", err)
	}

	// Now A's own layout hides it: refused, nothing persisted.
	save(tenant, vwEditLayout)
	_, err = fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: taxID, IsRequired: &required})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FIELD_REQUIRED_HAS_VISIBLE_WHEN" || ve.Details["controllerField"] != "customer_type" {
		t.Fatalf("err = %v; want FIELD_REQUIRED_HAS_VISIBLE_WHEN naming customer_type", err)
	}
	if stored, err := metaRepo.GetFieldByID(ctx, tenant, taxID); err != nil || stored.IsRequired {
		t.Fatalf("refused edit persisted: %+v (err %v)", stored, err)
	}

	// The exit the message names: drop the condition, then the same edit goes through.
	save(tenant, `{"sections":[{"title":"A","columns":1,"fields":["customer_type","tax_code"]}]}`)
	if _, err := fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: taxID, IsRequired: &required}); err != nil {
		t.Fatalf("EditField after dropping the condition: %v", err)
	}
}
