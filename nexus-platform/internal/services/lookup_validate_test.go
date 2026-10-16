package services

// ENG-COMPUTED #2 (lookup) validation tests — config-time (validateLookupConfig, pure/DB-free)
// and cross-entity (validateLookupRefs + validateLookupSourceField, needs sibling/target
// metadata) exercised through CreateEntity/AddField with fakeMeta/fakeFieldMeta test doubles
// (no DB). Mirrors the existing relation-target test style in entity_orchestration_test.go.

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// ---------------------------------------------------------------------------
// validateLookupConfig — pure structural (DB-free) checks.
// ---------------------------------------------------------------------------

func TestValidateLookupConfig_Table(t *testing.T) {
	cases := []struct {
		name     string
		options  map[string]any
		wantCode string // "" = expect success
	}{
		{"valid", map[string]any{"via": "account", "sourceField": "industry"}, ""},
		{"nil options", nil, "FIELD_LOOKUP_VIA_REQUIRED"},
		{"missing via", map[string]any{"sourceField": "industry"}, "FIELD_LOOKUP_VIA_REQUIRED"},
		{"blank via", map[string]any{"via": "  ", "sourceField": "industry"}, "FIELD_LOOKUP_VIA_REQUIRED"},
		{"via wrong type", map[string]any{"via": 42, "sourceField": "industry"}, "FIELD_LOOKUP_VIA_REQUIRED"},
		{"missing sourceField", map[string]any{"via": "account"}, "FIELD_LOOKUP_SOURCE_FIELD_REQUIRED"},
		{"blank sourceField", map[string]any{"via": "account", "sourceField": "  "}, "FIELD_LOOKUP_SOURCE_FIELD_REQUIRED"},
		{"sourceField wrong type", map[string]any{"via": "account", "sourceField": 7}, "FIELD_LOOKUP_SOURCE_FIELD_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLookupConfig("industry_lookup", tc.options)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if codeOf(err) != tc.wantCode {
				t.Fatalf("code = %q; want %q (err=%v)", codeOf(err), tc.wantCode, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateLookupRefs (via CreateEntity) — cross-field, same-entity checks.
// ---------------------------------------------------------------------------

func lookupEntityFixture(lookupOptions map[string]any) CreateEntityInput {
	return CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "accounts"}},
			{Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
				Options: lookupOptions},
		},
	}
}

// TestCreateEntity_Lookup_ViaNotARelation: options.via names a sibling field that exists but
// is NOT type "relation" → FIELD_LOOKUP_VIA_NOT_RELATION.
func TestCreateEntity_Lookup_ViaNotARelation(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "industry", FieldType: "text"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
				Options: map[string]any{"via": "name", "sourceField": "industry"}},
		},
	}
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_LOOKUP_VIA_NOT_RELATION" {
		t.Fatalf("code = %q; want FIELD_LOOKUP_VIA_NOT_RELATION (err=%v)", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_ViaUnknown: options.via names a field that does not exist at all on
// this entity → FIELD_LOOKUP_VIA_UNKNOWN.
func TestCreateEntity_Lookup_ViaUnknown(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"accounts": true}}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "ghost_relation", "sourceField": "industry"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_LOOKUP_VIA_UNKNOWN" {
		t.Fatalf("code = %q; want FIELD_LOOKUP_VIA_UNKNOWN (err=%v)", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_ViaRelationCardinalityInvalid: declaring a relation field with an
// explicitly multi-target options.cardinality ("N-N") is now rejected at field-config time
// (ENG-REL2 #5's validateRelationCardinality, config-time single source of truth: only
// absent/"1-N"/"1-1" are accepted) — BEFORE CreateEntity ever reaches the lookup ref check.
// This supersedes the pre-#5 version of this test (which declared the same "N-N" relation
// field in the SAME CreateEntity call as a lookup via it, expecting the LATER
// FIELD_LOOKUP_VIA_MUST_BE_TO_ONE check to fire): the earlier, clearer error is a strict
// improvement — an admin who mistypes cardinality now finds out immediately, regardless of
// whether a lookup ever references the field.
func TestCreateEntity_Lookup_ViaRelationCardinalityInvalid(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "industry", FieldType: "text"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "account", Label: "Account", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "accounts", "cardinality": "N-N"}},
			{Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
				Options: map[string]any{"via": "account", "sourceField": "industry"}},
		},
	}
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_INVALID" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_INVALID (err=%v)", codeOf(err), err)
	}
}

// TestAddField_Lookup_ViaMustBeToOne_LegacyStoredCardinality is the belt-and-suspenders
// counterpart of the config-time gate above: validateRelationCardinality only guards
// NEWLY declared/edited relation fields, so a sibling relation field whose options were
// persisted BEFORE that lock existed (or, in principle, any other legacy multi-target
// marker) can still carry an out-of-range options.cardinality in storage. AddField's
// lookup ref check must still reject a `via` pointing at such a field —
// FIELD_LOOKUP_VIA_MUST_BE_TO_ONE — instead of trusting stale/legacy data.
func TestAddField_Lookup_ViaMustBeToOne_LegacyStoredCardinality(t *testing.T) {
	schema := &fakeFieldSchema{}
	accountOpts, err := optionsJSON(map[string]any{"relatedEntity": "accounts", "cardinality": "N-N"})
	if err != nil {
		t.Fatalf("optionsJSON: %v", err)
	}
	meta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"courses":  entityFixture(),
			"accounts": relatedEntityFixture(),
		},
		existing: []metadata.FieldDefinition{
			{Name: "account", FieldType: "relation", Options: accountOpts},
		},
	}
	svc := NewFieldService(schema, meta)

	_, err = svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{
			Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
			Options: map[string]any{"via": "account", "sourceField": "industry"},
		},
	})
	if codeOf(err) != "FIELD_LOOKUP_VIA_MUST_BE_TO_ONE" {
		t.Fatalf("code = %q; want FIELD_LOOKUP_VIA_MUST_BE_TO_ONE (err=%v)", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_HappyPath: via names a valid to-one relation sibling, sourceField
// names an ordinary (non-computed) field on the target entity → succeeds.
func TestCreateEntity_Lookup_HappyPath(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "industry", FieldType: "text"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "account", "sourceField": "industry"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("valid lookup rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validateLookupSourceField (nested-guard, bug fix) — sourceField naming another computed
// field on the TARGET entity must be rejected, never silently accepted-then-null-forever.
// ---------------------------------------------------------------------------

// TestCreateEntity_Lookup_SourceFieldNestedLookup_Rejected: sourceField names ANOTHER lookup
// field on the target entity → FIELD_FORMULA_NESTED (shared "computed of computed" code,
// mirrors validateRollupCrossEntity's aggField cycle guard).
func TestCreateEntity_Lookup_SourceFieldNestedLookup_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {
				{Name: "region", FieldType: "text"},
				{Name: "derived_region", FieldType: "lookup"}, // itself a computed field
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "account", "sourceField": "derived_region"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_FORMULA_NESTED" {
		t.Fatalf("code = %q; want FIELD_FORMULA_NESTED (nested-lookup cycle guard); err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_SourceFieldNestedFormula_Rejected: sourceField names a formula field
// on the target entity → also rejected (same cycle guard, different computed type).
func TestCreateEntity_Lookup_SourceFieldNestedFormula_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "total", FieldType: "formula"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "account", "sourceField": "total"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_FORMULA_NESTED" {
		t.Fatalf("code = %q; want FIELD_FORMULA_NESTED; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_SourceFieldNestedRollup_Rejected: sourceField names a rollup field on
// the target entity → also rejected.
func TestCreateEntity_Lookup_SourceFieldNestedRollup_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "deal_total", FieldType: "rollup"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "account", "sourceField": "deal_total"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_FORMULA_NESTED" {
		t.Fatalf("code = %q; want FIELD_FORMULA_NESTED; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Lookup_SourceFieldUnknown_Rejected: sourceField names a field that does not
// exist at all on the target entity → FIELD_LOOKUP_SOURCE_FIELD_UNKNOWN (not silently accepted
// to resolve null forever).
func TestCreateEntity_Lookup_SourceFieldUnknown_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"accounts": {{Name: "industry", FieldType: "text"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := lookupEntityFixture(map[string]any{"via": "account", "sourceField": "ghost_field"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_LOOKUP_SOURCE_FIELD_UNKNOWN" {
		t.Fatalf("code = %q; want FIELD_LOOKUP_SOURCE_FIELD_UNKNOWN; err=%v", codeOf(err), err)
	}
}

// TestAddField_Lookup_SourceFieldNestedLookup_Rejected: the AddField path (existing,
// already-persisted entity) enforces the SAME nested-guard as CreateEntity.
func TestAddField_Lookup_SourceFieldNestedLookup_Rejected(t *testing.T) {
	schema := &fakeFieldSchema{}
	hostEntity := &metadata.EntityDefinition{ID: "ent-contacts", Name: "contacts"}
	targetEntity := &metadata.EntityDefinition{ID: "ent-accounts", Name: "accounts"}
	fmeta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"contacts": hostEntity,
			"accounts": targetEntity,
		},
		existing: []metadata.FieldDefinition{
			{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
		},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-accounts": {{Name: "derived_region", FieldType: "lookup"}},
		},
	}
	svc := NewFieldService(schema, fmeta)

	_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "contacts",
		Field: FieldInput{
			Name: "account_region", Label: "Account Region", FieldType: "lookup",
			Options: map[string]any{"via": "account", "sourceField": "derived_region"},
		},
	})
	if codeOf(err) != "FIELD_FORMULA_NESTED" {
		t.Fatalf("code = %q; want FIELD_FORMULA_NESTED; err=%v", codeOf(err), err)
	}
}

// TestAddField_Lookup_HappyPath: AddField accepts a well-formed lookup pointing at an ordinary
// field on the target entity.
func TestAddField_Lookup_HappyPath(t *testing.T) {
	schema := &fakeFieldSchema{}
	hostEntity := &metadata.EntityDefinition{ID: "ent-contacts", Name: "contacts"}
	targetEntity := &metadata.EntityDefinition{ID: "ent-accounts", Name: "accounts"}
	fmeta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"contacts": hostEntity,
			"accounts": targetEntity,
		},
		existing: []metadata.FieldDefinition{
			{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
		},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-accounts": {{Name: "industry", FieldType: "text"}},
		},
	}
	svc := NewFieldService(schema, fmeta)

	_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "contacts",
		Field: FieldInput{
			Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
			Options: map[string]any{"via": "account", "sourceField": "industry"},
		},
	})
	if err != nil {
		t.Fatalf("valid AddField lookup rejected: %v", err)
	}
}

// jsonOptsPtr marshals options to the *string shape metadata.FieldDefinition.Options expects.
func jsonOptsPtr(t *testing.T, options map[string]any) *string {
	t.Helper()
	s, err := optionsJSON(options)
	if err != nil {
		t.Fatalf("optionsJSON: %v", err)
	}
	return s
}
