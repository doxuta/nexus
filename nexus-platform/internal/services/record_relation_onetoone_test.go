package services

import (
	"testing"

	"nexus/platform/internal/dynrepo"
)

// ENG-REL2 #5 asDuplicate unit tests: a 1-1 relation field's tenant-scoped UNIQUE key
// (uq_obj_<entity>_<field>_id) trips DuplicateUniqueError exactly like an ordinary unique
// field's index — asDuplicate disambiguates by METADATA (review MED-1 fix): the extracted
// column maps to RECORD_RELATION_ALREADY_BOUND only when the entity's specs actually contain
// a relation field with cardinality "1-1" owning that column. An ordinary unique field —
// even one literally named "*_id" — keeps the pre-#5 RECORD_DUPLICATE contract untouched.

// oneToOneSpecs returns the specs of an entity with a 1-1 relation field "user" (physical
// column user_id) plus an ordinary unique text field "email".
func oneToOneSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "email", FieldType: "text", IsUnique: true},
		{Name: "user", FieldType: "relation",
			Options: map[string]any{"relatedEntity": "users", "cardinality": "1-1"}},
	}
}

// TestAsDuplicate_OneToOneRelation_MapsToRelationAlreadyBound: a duplicate on the 1-1
// relation's UNIQUE index maps to RECORD_RELATION_ALREADY_BOUND with details.field = the
// relation FIELD name (column name stripped of "_id") — resolved via the specs, not by
// name shape alone.
func TestAsDuplicate_OneToOneRelation_MapsToRelationAlreadyBound(t *testing.T) {
	err := asDuplicate(&dynrepo.DuplicateUniqueError{IndexName: "uq_obj_profiles_user_id"}, "profiles", oneToOneSpecs())
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_RELATION_ALREADY_BOUND" {
		t.Errorf("Code = %q; want RECORD_RELATION_ALREADY_BOUND", ve.Code)
	}
	if ve.Field != "user" {
		t.Errorf("Field = %q; want user (the relation field owning column user_id)", ve.Field)
	}
	if ve.Details["field"] != "user" {
		t.Errorf("Details[field] = %v; want user", ve.Details["field"])
	}
}

// TestAsDuplicate_OrdinaryUniqueField_StillMapsToRecordDuplicate: an ordinary unique field
// (e.g. "email") is unaffected — still 409 RECORD_DUPLICATE with the untouched field name.
func TestAsDuplicate_OrdinaryUniqueField_StillMapsToRecordDuplicate(t *testing.T) {
	err := asDuplicate(&dynrepo.DuplicateUniqueError{IndexName: "uq_obj_accounts_email"}, "accounts", oneToOneSpecs())
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_DUPLICATE" {
		t.Errorf("Code = %q; want RECORD_DUPLICATE", ve.Code)
	}
	if ve.Field != "email" {
		t.Errorf("Field = %q; want email", ve.Field)
	}
}

// TestAsDuplicate_UnparseableIndexName_FallsBackToGenericDuplicate: an index name that does
// not match the Nexus uq_obj_<entity>_ convention (e.g. PRIMARY) still returns a generic
// RECORD_DUPLICATE with no details.field (extractDuplicateField returns "" before any
// spec lookup runs).
func TestAsDuplicate_UnparseableIndexName_FallsBackToGenericDuplicate(t *testing.T) {
	err := asDuplicate(&dynrepo.DuplicateUniqueError{IndexName: "PRIMARY"}, "accounts", oneToOneSpecs())
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_DUPLICATE" {
		t.Errorf("Code = %q; want RECORD_DUPLICATE", ve.Code)
	}
	if ve.Field != "" {
		t.Errorf("Field = %q; want empty (unparseable index name)", ve.Field)
	}
}

// TestAsDuplicate_OrdinaryFieldNamedWithIdSuffix_KeepsRecordDuplicate (review MED-1 fix):
// an ORDINARY unique field whose OWN name ends in "_id" (e.g. "customer_id" — a common
// external-dedup key) is NOT misclassified as a 1-1 conflict: no relation spec named
// "customer" with cardinality "1-1" exists, so the pre-#5 contract holds — RECORD_DUPLICATE
// with the FULL, unstripped field name. This replaces the earlier pinned known-limitation
// (the "_id" name-shape heuristic) with a metadata lookup against specs already in scope.
func TestAsDuplicate_OrdinaryFieldNamedWithIdSuffix_KeepsRecordDuplicate(t *testing.T) {
	specs := []FieldSpec{{Name: "customer_id", FieldType: "text", IsUnique: true}}
	err := asDuplicate(&dynrepo.DuplicateUniqueError{IndexName: "uq_obj_orders_customer_id"}, "orders", specs)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_DUPLICATE" {
		t.Errorf("Code = %q; want RECORD_DUPLICATE (ordinary unique field, no 1-1 relation named customer)", ve.Code)
	}
	if ve.Field != "customer_id" {
		t.Errorf("Field = %q; want customer_id (full name, no _id strip)", ve.Field)
	}
}

// TestAsDuplicate_NonOneToOneRelationColumn_KeepsRecordDuplicate: a "<f>_id" column whose
// owning relation field is NOT cardinality "1-1" (plain 1-N — its index is non-unique so
// this shouldn't arise, but a same-named unique column could) stays RECORD_DUPLICATE.
func TestAsDuplicate_NonOneToOneRelationColumn_KeepsRecordDuplicate(t *testing.T) {
	specs := []FieldSpec{{Name: "user", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users"}}} // cardinality absent -> "1-N"
	err := asDuplicate(&dynrepo.DuplicateUniqueError{IndexName: "uq_obj_profiles_user_id"}, "profiles", specs)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_DUPLICATE" {
		t.Errorf("Code = %q; want RECORD_DUPLICATE (relation is 1-N, not 1-1)", ve.Code)
	}
}

// TestAsDuplicate_NonDuplicateError_PassesThrough: an error that is not a
// DuplicateUniqueError passes through unchanged.
func TestAsDuplicate_NonDuplicateError_PassesThrough(t *testing.T) {
	sentinel := dynrepo.ErrRecordNotFound
	if got := asDuplicate(sentinel, "accounts", nil); got != sentinel {
		t.Errorf("asDuplicate must pass through non-duplicate errors unchanged; got %v", got)
	}
}
