package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// ENG-REL2 #5 (1-1 relationship, FK-on-child + tenant-scoped UNIQUE) unit tests: AddField's
// DDL dispatch (1-1 vs plain 1-N), the sys_relationships registry row type, EditField's
// cardinality immutability, and config-time cardinality validation. Mirrors the
// field_relation_orchestration_test.go / field_relation_registry_test.go harness style
// (fakeFieldSchema / fakeFieldMeta / relMeta()).

// addOneToOneRelationField is a valid 1-1 relation AddFieldInput (FK to "accounts").
func addOneToOneRelationField() AddFieldInput {
	return AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{
			Name:      "owner",
			Label:     "Chủ",
			FieldType: "relation",
			Options:   map[string]any{"relatedEntity": "accounts", "cardinality": "1-1"},
		},
	}
}

// TestAddField_OneToOne_UsesOneToOnePair is the core acceptance: a relation field with
// options.cardinality "1-1" ALTERs via AddOneToOneRelationColumn — NEVER the plain
// AddRelationColumn — and registers a sys_relationships row with RelationshipType "1-1".
func TestAddField_OneToOne_UsesOneToOnePair(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", addOneToOneRelationField())
	if err != nil {
		t.Fatalf("AddField(1-1 relation): %v", err)
	}
	if len(schema.oneToOneAdded) != 1 || schema.oneToOneAdded[0] != "owner" {
		t.Errorf("1-1 relation field should ALTER once via AddOneToOneRelationColumn; oneToOneAdded=%v", schema.oneToOneAdded)
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("1-1 relation field must NOT use the plain 1-N ALTER; relAdded=%v", schema.relAdded)
	}
	if len(schema.added) != 0 {
		t.Errorf("1-1 relation field must NOT use the unique-column ALTER; added=%v", schema.added)
	}
	if fd == nil || fd.FieldType != "relation" {
		t.Fatalf("returned field def should be the persisted relation field; got %v", fd)
	}
	if len(meta.createdWithRel) != 1 {
		t.Fatalf("expected 1 CreateFieldWithRelationship call; got %d", len(meta.createdWithRel))
	}
	rel := meta.createdWithRel[0].rel
	if rel.RelationshipType != "1-1" {
		t.Errorf("rel.RelationshipType = %q; want 1-1", rel.RelationshipType)
	}
	if rel.ChildFKColumn != "owner_id" {
		t.Errorf("rel.ChildFKColumn = %q; want owner_id", rel.ChildFKColumn)
	}
	if rel.ParentEntityName != "accounts" || rel.ChildEntityName != "courses" {
		t.Errorf("Parent/Child = %q/%q; want accounts/courses", rel.ParentEntityName, rel.ChildEntityName)
	}
}

// TestAddField_OneToOne_ColumnConflictSameGuardAsOneToN: the `<field>_id` column-conflict
// precheck runs identically for a 1-1 relation field — a pre-existing column at that name is
// rejected 422 FIELD_RELATION_COLUMN_CONFLICT, and neither ALTER pair runs.
func TestAddField_OneToOne_ColumnConflictSameGuardAsOneToN(t *testing.T) {
	schema := &fakeFieldSchema{colExists: true} // pretend owner_id already exists
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addOneToOneRelationField())
	if codeOf(err) != "FIELD_RELATION_COLUMN_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_COLUMN_CONFLICT", codeOf(err))
	}
	if len(schema.colChecked) != 1 || schema.colChecked[0] != "owner_id" {
		t.Errorf("expected ColumnExists(owner_id) precheck; colChecked=%v", schema.colChecked)
	}
	if len(schema.oneToOneAdded) != 0 || len(schema.relAdded) != 0 {
		t.Errorf("conflict must not run either relation ALTER; oneToOneAdded=%v relAdded=%v", schema.oneToOneAdded, schema.relAdded)
	}
	if len(meta.created) != 0 {
		t.Errorf("conflict must not persist metadata; created=%v", meta.created)
	}
}

// TestAddField_OneToOne_CompensatesWithOneToOneDrop: the FK ALTER auto-commits via
// AddOneToOneRelationColumn, then the metadata write fails -> compensation MUST call
// DropOneToOneRelationColumn (the matching pair — the index NAME differs from the plain 1-N
// drop, so using the wrong one would error against a live DB).
func TestAddField_OneToOne_CompensatesWithOneToOneDrop(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.createErr = errors.New("meta boom")
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", addOneToOneRelationField()); err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.oneToOneAdded) != 1 {
		t.Errorf("1-1 FK ALTER should have run before metadata; oneToOneAdded=%v", schema.oneToOneAdded)
	}
	if len(schema.oneToOneDropped) != 1 || schema.oneToOneDropped[0] != "owner" {
		t.Errorf("metadata failure must DROP via DropOneToOneRelationColumn; oneToOneDropped=%v", schema.oneToOneDropped)
	}
	if len(schema.relDropped) != 0 {
		t.Errorf("must NOT call the plain 1-N drop for a 1-1 field; relDropped=%v", schema.relDropped)
	}
}

// TestAddField_OneToOne_DuplicateUniqueMapsToCardinalityConflict: when
// AddOneToOneRelationColumn's ALTER trips ER_DUP_ENTRY (dynrepo.ErrDuplicateUnique — existing
// rows on a SHARED obj_* table already violate 1-1 exclusivity for this tenant), AddField
// must return 409 FIELD_RELATION_CARDINALITY_CONFLICT, NOT an opaque 500. No column was
// added (the ALTER failed atomically), so no compensation drop should run.
func TestAddField_OneToOne_DuplicateUniqueMapsToCardinalityConflict(t *testing.T) {
	schema := &fakeFieldSchema{oneToOneAddErr: dynrepo.ErrDuplicateUnique}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addOneToOneRelationField())
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_CONFLICT (err=%v)", codeOf(err), err)
	}
	if len(schema.oneToOneDropped) != 0 {
		t.Errorf("ALTER failed atomically; nothing to compensate. oneToOneDropped=%v", schema.oneToOneDropped)
	}
	if len(meta.created) != 0 || len(meta.createdWithRel) != 0 {
		t.Errorf("metadata must not be persisted when the ALTER fails; created=%v createdWithRel=%v", meta.created, meta.createdWithRel)
	}
}

// TestAddField_OneToOne_DuplicateKeyNameMapsTo409: a concurrent AddField for the SAME 1-1
// field (RM-25 DDL race) trips ER_DUP_KEYNAME/FIELDNAME -> 409 FIELD_ALREADY_EXISTS, exactly
// like the plain 1-N and unique-field races.
func TestAddField_OneToOne_DuplicateKeyNameMapsTo409(t *testing.T) {
	schema := &fakeFieldSchema{oneToOneAddErr: dynrepo.ErrDuplicateKeyName}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addOneToOneRelationField())
	if codeOf(err) != "FIELD_ALREADY_EXISTS" {
		t.Fatalf("code = %q; want FIELD_ALREADY_EXISTS", codeOf(err))
	}
}

// TestAddField_RelationCardinalityInvalid_RejectsBeforeDDL: an out-of-range
// options.cardinality (anything but absent/"1-N"/"1-1") is rejected 422
// FIELD_RELATION_CARDINALITY_INVALID before any DDL or metadata write runs.
func TestAddField_RelationCardinalityInvalid_RejectsBeforeDDL(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addOneToOneRelationField()
	in.Field.Options["cardinality"] = "1-1-inverse-typo"

	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_INVALID" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_INVALID", codeOf(err))
	}
	if len(schema.oneToOneAdded) != 0 || len(schema.relAdded) != 0 {
		t.Errorf("invalid cardinality must not run DDL; oneToOneAdded=%v relAdded=%v", schema.oneToOneAdded, schema.relAdded)
	}
	if len(meta.created) != 0 || len(meta.createdWithRel) != 0 {
		t.Errorf("invalid cardinality must not persist metadata; created=%v createdWithRel=%v", meta.created, meta.createdWithRel)
	}
}

// TestAddField_RelationCardinalityInvalid_NonStringRejected: a non-string cardinality value
// (e.g. a number) is rejected the same way.
func TestAddField_RelationCardinalityInvalid_NonStringRejected(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addOneToOneRelationField()
	in.Field.Options["cardinality"] = 42

	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_INVALID" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_INVALID", codeOf(err))
	}
}

// TestAddField_RelationCardinality_ExplicitOneToN_BehavesLikeAbsent: an explicit
// options.cardinality "1-N" (same meaning as omitting it) still uses the plain 1-N ALTER
// pair and registers a "1-N" registry row — cardinality "1-N" is not rejected, and does not
// accidentally take the 1-1 branch.
func TestAddField_RelationCardinality_ExplicitOneToN_BehavesLikeAbsent(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addRelationField() // no cardinality key at all (default)
	in.Field.Options["cardinality"] = "1-N"

	if _, err := svc.AddField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("AddField(explicit 1-N): %v", err)
	}
	if len(schema.relAdded) != 1 || schema.relAdded[0] != "owner" {
		t.Errorf("explicit 1-N should use the plain relation ALTER; relAdded=%v", schema.relAdded)
	}
	if len(schema.oneToOneAdded) != 0 {
		t.Errorf("explicit 1-N must NOT use the 1-1 ALTER; oneToOneAdded=%v", schema.oneToOneAdded)
	}
	if len(meta.createdWithRel) != 1 || meta.createdWithRel[0].rel.RelationshipType != "1-N" {
		t.Fatalf("registry row must be 1-N; createdWithRel=%v", meta.createdWithRel)
	}
}

// storedOneToOneRelationField mirrors field_edit_test.go's storedRelationField() but with a
// stored options.cardinality "1-1" and no inverseField (so the cardinality-only tests below
// aren't muddied by the pre-existing inverseField-omission-is-a-change rule).
func storedOneToOneRelationField() *metadata.FieldDefinition {
	opts := `{"relatedEntity":"accounts","cardinality":"1-1"}`
	return &metadata.FieldDefinition{
		ID:        "fld-rel-11",
		EntityID:  "ent-1",
		Name:      "owner",
		Label:     "Chủ",
		FieldType: "relation",
		Options:   &opts,
	}
}

// TestEditField_CardinalityImmutable: PUT on a relation field's options.cardinality — even
// changing "1-1" to "1-N" — is rejected 422 FIELD_RELATION_OPTIONS_IMMUTABLE. EditField runs
// no DDL, so allowing this would silently desync sys_fields.options from the actual obj_*
// column shape (guarded+unique vs plain).
func TestEditField_CardinalityImmutable(t *testing.T) {
	meta := &fakeEditMeta{field: storedOneToOneRelationField()}
	svc, _ := editService(meta)

	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel-11",
		Options: map[string]any{"relatedEntity": "accounts", "cardinality": "1-N"},
	})
	if codeOf(err) != "FIELD_RELATION_OPTIONS_IMMUTABLE" {
		t.Fatalf("code = %q; want FIELD_RELATION_OPTIONS_IMMUTABLE (err=%v)", codeOf(err), err)
	}
	if len(meta.updated) != 0 {
		t.Errorf("rejected edit must not persist; updated=%v", meta.updated)
	}
}

// TestEditField_CardinalityUnchangedIsAllowed: an edit that keeps options.cardinality
// identical to the stored value (and only changes an unrelated attribute) succeeds.
func TestEditField_CardinalityUnchangedIsAllowed(t *testing.T) {
	meta := &fakeEditMeta{field: storedOneToOneRelationField()}
	svc, _ := editService(meta)

	fd, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel-11",
		Label:   strPtr("Chủ sở hữu"),
		Options: map[string]any{"relatedEntity": "accounts", "cardinality": "1-1"},
	})
	if err != nil {
		t.Fatalf("EditField with unchanged cardinality should succeed: %v", err)
	}
	if fd.Label != "Chủ sở hữu" {
		t.Errorf("Label = %q; want Chủ sở hữu", fd.Label)
	}
}
