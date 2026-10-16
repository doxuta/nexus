//go:build integration

// queue #142 end-to-end over real MySQL: EditField reads sys_field_acl through the REAL
// auth.FieldACLRepository.ListFieldRestrictions (tenant-pinned JOIN to acl_roles) and refuses to
// make a locked field required-without-fill; lifting the lock lets the same edit through.
package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func TestFieldEditLockGuard_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	aclRepo := auth.NewFieldACLRepository(gdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo).WithFieldRestrictions(aclRepo)

	const tenant, other = o3aTenantA, o3aTenantB
	flsSeedTenant(t, sdb, tenant, "tenant-a-142")
	flsSeedTenant(t, sdb, other, "tenant-b-142")
	sales, viewer, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	flsSeedRole(t, sdb, sales, tenant, "crm_sales_rep")
	flsSeedRole(t, sdb, viewer, tenant, "viewer")
	flsSeedRole(t, sdb, foreign, other, "foreign_role")

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "deals", Label: "Deals",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}, {Name: "amount", Label: "Giá trị", FieldType: "number"}},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	ent, err := metaRepo.GetEntityByName(ctx, tenant, "deals")
	if err != nil {
		t.Fatalf("GetEntityByName: %v", err)
	}
	fields, err := metaRepo.ListFields(ctx, tenant, ent.ID)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}
	var amountID string
	for _, f := range fields {
		if f.Name == "amount" {
			amountID = f.ID
		}
	}

	// sales: read-only (the restriction that strands a role) · viewer: a stored full-access row
	// (restricts nothing, must not be listed) · a row carrying tenant A's id but tenant B's role
	// (a forged/stale row — the JOIN pin must drop it).
	flsSeedFieldGrant(t, sdb, tenant, sales, "deals", "amount", true, false)
	flsSeedFieldGrant(t, sdb, tenant, viewer, "deals", "amount", true, true)
	flsSeedFieldGrant(t, sdb, tenant, foreign, "deals", "amount", false, false)

	rows, err := aclRepo.ListFieldRestrictions(ctx, tenant, "deals", "amount")
	if err != nil {
		t.Fatalf("ListFieldRestrictions: %v", err)
	}
	if len(rows) != 1 || rows[0].RoleID != sales || rows[0].RoleName != "crm_sales_rep" || rows[0].CanEdit {
		t.Fatalf("restrictions = %+v; want only tenant A's read-only sales row", rows)
	}
	if rows, _ := aclRepo.ListFieldRestrictions(ctx, other, "", ""); len(rows) != 0 {
		t.Fatalf("tenant B sees %+v; want nothing (rows carry tenant A's id)", rows)
	}

	required := true
	_, err = fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: amountID, IsRequired: &required})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FIELD_REQUIRED_WHILE_LOCKED" {
		t.Fatalf("err = %v; want FIELD_REQUIRED_WHILE_LOCKED", err)
	}
	if roles, _ := ve.Details["roles"].([]map[string]any); len(roles) != 1 || roles[0]["id"] != sales {
		t.Fatalf("details.roles = %v; want only the sales role", ve.Details["roles"])
	}
	stored, err := metaRepo.GetFieldByID(ctx, tenant, amountID)
	if err != nil || stored.IsRequired {
		t.Fatalf("refused edit persisted: %+v (err %v)", stored, err)
	}

	// Lift the lock first (the exit the message names) → the same edit goes through.
	if _, err := aclRepo.DeleteFieldACLWhere(ctx, tenant, sales, "deals", "amount"); err != nil {
		t.Fatalf("DeleteFieldACLWhere: %v", err)
	}
	if _, err := fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: amountID, IsRequired: &required}); err != nil {
		t.Fatalf("EditField after unlock: %v", err)
	}
}
