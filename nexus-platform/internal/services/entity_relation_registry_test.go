package services

import (
	"context"
	"fmt"
	"testing"

	"nexus/platform/internal/metadata"
)

// ENG-REL2 #4 unit tests: CreateEntity's 1-N relationship registry write for `relation`
// fields declared inline with a brand-new entity. Mirrors the AddField-side tests in
// field_relation_registry_test.go, but exercises the entity-create-with-fields path
// (entity_orchestration.go), including the batch-local inverse-field collision case that can
// ONLY happen here (AddField only ever registers one relation field per call).

// TestCreateEntity_RelationField_RegistersOneToNRow: a single relation field declared inline
// with a new entity registers a companion 1-N sys_relationships row in the SAME
// CreateEntityWithFields transaction, with the same name/fk/inverse-default rules as AddField.
func TestCreateEntity_RelationField_RegistersOneToNRow(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"customers": true}}
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "orders",
		Label: "Orders",
		Fields: []FieldInput{
			{Name: "total", Label: "Total", FieldType: "number"},
			{Name: "customer", Label: "Customer", FieldType: "relation", Options: map[string]any{"relatedEntity": "customers"}},
		},
	}
	if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("CreateEntity(relation field): %v", err)
	}
	if len(meta.createdRels) != 1 {
		t.Fatalf("expected 1 relationship row; got %d: %+v", len(meta.createdRels), meta.createdRels)
	}
	rel := meta.createdRels[0]
	if rel.Name != "orders_customer" {
		t.Errorf("rel.Name = %q; want orders_customer", rel.Name)
	}
	if rel.ParentEntityName != "customers" || rel.ChildEntityName != "orders" {
		t.Errorf("rel.Parent/Child = %q/%q; want customers/orders", rel.ParentEntityName, rel.ChildEntityName)
	}
	if rel.RelationshipType != "1-N" {
		t.Errorf("rel.RelationshipType = %q; want 1-N", rel.RelationshipType)
	}
	if rel.ChildFKColumn != "customer_id" {
		t.Errorf("rel.ChildFKColumn = %q; want customer_id", rel.ChildFKColumn)
	}
	if rel.InverseField != "orders" {
		t.Errorf("rel.InverseField = %q; want orders (default = child entity name)", rel.InverseField)
	}
	if rel.TenantID != "tenant-1" {
		t.Errorf("rel.TenantID = %q; want tenant-1", rel.TenantID)
	}
}

// TestCreateEntity_TwoRelationFieldsSameParent_BatchLocalCollisionFallsBack: an entity
// declaring TWO relation fields that both point at the SAME parent entity would, without
// batch-local tracking, both silently default to the same inverse name (neither is persisted
// yet, so a DB-only check would see both as "available"). The claims map must catch this
// in-memory: the SECOND field falls back to "<childEntity>_<fieldName>".
func TestCreateEntity_TwoRelationFieldsSameParent_BatchLocalCollisionFallsBack(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"customers": true}}
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "total", Label: "Total", FieldType: "number"},
			{Name: "customer", Label: "Customer", FieldType: "relation", Options: map[string]any{"relatedEntity": "customers"}},
			{Name: "billto", Label: "Bill To", FieldType: "relation", Options: map[string]any{"relatedEntity": "customers"}},
		},
	}
	if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("CreateEntity(two relation fields, same parent): %v", err)
	}
	if len(meta.createdRels) != 2 {
		t.Fatalf("expected 2 relationship rows; got %d: %+v", len(meta.createdRels), meta.createdRels)
	}
	first, second := meta.createdRels[0], meta.createdRels[1]
	if first.InverseField != "invoices" {
		t.Errorf("first rel (customer).InverseField = %q; want invoices (default)", first.InverseField)
	}
	if second.InverseField != "invoices_billto" {
		t.Errorf("second rel (billto).InverseField = %q; want invoices_billto (batch-local fallback)", second.InverseField)
	}
	if first.Name == second.Name {
		t.Errorf("both relationship rows have the same name %q; must be distinct per field", first.Name)
	}
}

// TestCreateEntity_RelationField_InverseFieldCollisionWithExistingRow_Rejected proves the
// registry write consults ALREADY-PERSISTED rows too (not just the in-batch claims): a
// relation field whose explicit options.inverseField collides with an existing active row is
// rejected 422 FIELD_RELATION_INVERSE_TAKEN before any DDL/metadata write, and the whole
// CreateEntity call is aborted (no table created).
func TestCreateEntity_RelationField_InverseFieldCollisionWithExistingRow_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"customers": true},
		claimedInverse: map[string]*metadata.RelationshipDefinition{
			"customers|billing_contact": {Name: "some_other_rel", ParentEntityName: "customers", InverseField: "billing_contact", RelationshipType: "1-N"},
		},
	}
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "total", Label: "Total", FieldType: "number"},
			{Name: "customer", Label: "Customer", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "customers", "inverseField": "billing_contact"}},
		},
	}
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_INVERSE_TAKEN" {
		t.Fatalf("code = %q; want FIELD_RELATION_INVERSE_TAKEN", codeOf(err))
	}
	if len(schema.created) != 0 {
		t.Errorf("rejected create must not create the obj_* table; created=%v", schema.created)
	}
	if meta.called {
		t.Errorf("rejected create must not reach CreateEntityWithFields")
	}
}

// --- Review MED-1 / security LOW: config-time guards added post-review -------------------

// TestValidateCreateEntity_RelationUniqueRejected (review MED-1): a relation field declared
// IsUnique=true must be rejected at config time. AddField's DDL switch tests `case IsUnique`
// BEFORE `case relation`, so an allowed relation+unique would materialize a VARCHAR(255)
// unique column named <field> (no <field>_id) while the registry row still advertises
// child_fk_column="<field>_id" — a physical/registry drift this slice exists to prevent.
func TestValidateCreateEntity_RelationUniqueRejected(t *testing.T) {
	err := ValidateCreateEntity(CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "account", Label: "Account", FieldType: "relation", IsUnique: true,
				Options: map[string]any{"relatedEntity": "accounts"}},
		},
	})
	if codeOf(err) != "FIELD_RELATION_UNIQUE_UNSUPPORTED" {
		t.Fatalf("code = %q; want FIELD_RELATION_UNIQUE_UNSUPPORTED", codeOf(err))
	}
}

// TestValidateField_RelationUniqueRejected (review MED-1): the same guard must hold on the
// add-field path (validateField is the single gate shared with entity-create).
func TestValidateField_RelationUniqueRejected(t *testing.T) {
	err := validateField(FieldInput{
		Name: "account", Label: "Account", FieldType: "relation", IsUnique: true,
		Options: map[string]any{"relatedEntity": "accounts"},
	})
	if codeOf(err) != "FIELD_RELATION_UNIQUE_UNSUPPORTED" {
		t.Fatalf("code = %q; want FIELD_RELATION_UNIQUE_UNSUPPORTED", codeOf(err))
	}
}

// TestValidateCreateEntity_TooManyFields (security audit LOW, defense-in-depth): a single
// CreateEntity request is bounded — each relation field costs pre-flight metadata queries,
// so an unbounded field list inside one 1 MiB body must not translate into tens of
// thousands of sequential queries. The cap is generous (no real vertical comes close).
func TestValidateCreateEntity_TooManyFields(t *testing.T) {
	fields := make([]FieldInput, maxFieldsPerEntity+1)
	for i := range fields {
		fields[i] = FieldInput{Name: fmt.Sprintf("f%d", i), Label: "F", FieldType: "text"}
	}
	err := ValidateCreateEntity(CreateEntityInput{Name: "bulk", Label: "Bulk", Fields: fields})
	if codeOf(err) != "ENTITY_TOO_MANY_FIELDS" {
		t.Fatalf("code = %q; want ENTITY_TOO_MANY_FIELDS", codeOf(err))
	}
	// Exactly at the cap is allowed.
	if err := ValidateCreateEntity(CreateEntityInput{Name: "bulk", Label: "Bulk", Fields: fields[:maxFieldsPerEntity]}); err != nil {
		t.Fatalf("exactly maxFieldsPerEntity fields must be allowed: %v", err)
	}
}
