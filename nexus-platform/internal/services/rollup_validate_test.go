package services

// ENG-COMPUTED #3 (rollup) validation tests — config-time (validateRollupConfig, pure/DB-free)
// and cross-entity (validateRollupCrossEntity, needs child-entity/relationship metadata)
// exercised through CreateEntity/AddField with fakeMeta/fakeFieldMeta test doubles (no DB).

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// ---------------------------------------------------------------------------
// validateRollupConfig — pure structural (DB-free) checks.
// ---------------------------------------------------------------------------

func TestValidateRollupConfig_Table(t *testing.T) {
	cases := []struct {
		name     string
		options  map[string]any
		wantCode string // "" = expect success
	}{
		{"valid FK-on-child sum", map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount"}, ""},
		{"valid N-N count (no aggField needed)", map[string]any{"childEntity": "deals", "relationship": "accounts_deals", "metric": "count"}, ""},
		{"nil options", nil, "FIELD_ROLLUP_CHILD_ENTITY_REQUIRED"},
		{"missing childEntity", map[string]any{"viaRelation": "account", "metric": "sum", "aggField": "amount"}, "FIELD_ROLLUP_CHILD_ENTITY_REQUIRED"},
		{"blank childEntity", map[string]any{"childEntity": "  ", "viaRelation": "account", "metric": "sum", "aggField": "amount"}, "FIELD_ROLLUP_CHILD_ENTITY_REQUIRED"},
		{"invalid childEntity identifier", map[string]any{"childEntity": "bad name!", "viaRelation": "account", "metric": "sum", "aggField": "amount"}, "FIELD_ROLLUP_CHILD_ENTITY_INVALID"},
		{"bad metric", map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "median", "aggField": "amount"}, "FIELD_ROLLUP_METRIC_INVALID"},
		{"missing aggField for sum", map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum"}, "FIELD_ROLLUP_AGG_FIELD_REQUIRED"},
		{"missing aggField for avg", map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "avg"}, "FIELD_ROLLUP_AGG_FIELD_REQUIRED"},
		{"missing viaRelation and relationship", map[string]any{"childEntity": "deals", "metric": "count"}, "FIELD_ROLLUP_VIA_RELATION_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRollupConfig("deal_total", tc.options)
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
// validateRollupCrossEntity (via CreateEntity, FK-on-child mode) — child entity, viaRelation,
// aggField cross-entity checks.
// ---------------------------------------------------------------------------

func accountsWithRollupFixture(rollupOptions map[string]any) CreateEntityInput {
	return CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "deal_total", Label: "Deal Total", FieldType: "rollup", Options: rollupOptions},
		},
	}
}

// TestCreateEntity_Rollup_ViaRelationUnknown: viaRelation names a field that does not exist on
// the child entity → FIELD_ROLLUP_VIA_RELATION_UNKNOWN.
func TestCreateEntity_Rollup_ViaRelationUnknown(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {{Name: "amount", FieldType: "currency"}},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "ghost_account_ref", "metric": "sum", "aggField": "amount",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_VIA_RELATION_UNKNOWN" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_VIA_RELATION_UNKNOWN; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_ViaRelationNotRelation: viaRelation names a field on the child that
// exists but is not type "relation" → FIELD_ROLLUP_VIA_RELATION_NOT_RELATION.
func TestCreateEntity_Rollup_ViaRelationNotRelation(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "amount", FieldType: "currency"},
				{Name: "account", FieldType: "text"}, // wrong type: not a relation
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_VIA_RELATION_NOT_RELATION" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_VIA_RELATION_NOT_RELATION; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_ViaRelationWrongTarget: viaRelation IS a relation field on the child,
// but it points at a DIFFERENT entity than the one declaring the rollup.
func TestCreateEntity_Rollup_ViaRelationWrongTarget(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true, "other": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "amount", FieldType: "currency"},
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "other"})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_VIA_RELATION_WRONG_TARGET" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_VIA_RELATION_WRONG_TARGET; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_AggFieldUnknown: aggField names a field absent from the child entity.
func TestCreateEntity_Rollup_AggFieldUnknown(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "ghost_field",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_AGG_FIELD_UNKNOWN" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_AGG_FIELD_UNKNOWN; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_AggFieldWrongType: aggField exists but is not a numeric/aggregatable
// type (number/currency/rating) → FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID.
func TestCreateEntity_Rollup_AggFieldWrongType(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "notes", FieldType: "text"},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "notes",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_AggFieldNestedComputed_Rejected: aggField itself names another
// rollup/lookup field on the child → FIELD_FORMULA_NESTED ("rollup of rollup" cycle guard),
// never silently accepted. `formula` is NOT in this table — a number-resultType formula aggField
// is now ALLOWED (see TestCreateEntity_Rollup_AggFieldFormula* below); its still-rejected
// currency-resultType case gets its own dedicated test with its own code.
func TestCreateEntity_Rollup_AggFieldNestedComputed_Rejected(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
	}{
		{"nested rollup", "rollup"},
		{"nested lookup", "lookup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := &fakeSchema{}
			meta := &fakeMeta{
				knownEntities: map[string]bool{"accounts": true, "deals": true},
				fieldsByEntityName: map[string][]metadata.FieldDefinition{
					"deals": {
						{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
						{Name: "computed_amount", FieldType: tc.fieldType},
					},
				},
			}
			svc := NewEntityService(schema, meta)

			in := accountsWithRollupFixture(map[string]any{
				"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "computed_amount",
			})
			_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
			if codeOf(err) != "FIELD_FORMULA_NESTED" {
				t.Fatalf("code = %q; want FIELD_FORMULA_NESTED; err=%v", codeOf(err), err)
			}
		})
	}
}

// TestCreateEntity_Rollup_AggFieldFormulaNumberResultType_Allowed (2026-07-12 relaxation): a
// rollup aggField pointing at a sibling `formula` field with an EXPLICIT resultType:"number" is
// ALLOWED — that formula's result is a plain float64 persisted into obj_*.data at write time, so
// SUM reads it exactly like a real number field.
func TestCreateEntity_Rollup_AggFieldFormulaNumberResultType_Allowed(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "quantity", FieldType: "number"},
				{Name: "unit_price", FieldType: "number"},
				{Name: "line_total", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{
					"formula": "quantity * unit_price", "resultType": "number",
				})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "line_total",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("rollup over a number-resultType formula aggField rejected: %v", err)
	}
}

// TestCreateEntity_Rollup_AggFieldFormulaDefaultResultType_Allowed: same as above but the
// formula OMITS options.resultType entirely — the default is "number" (mirrors
// applyComputedFields/validateFormulaConfig), so the guard must treat the default the same as an
// explicit "number", not reject it as "unknown resultType".
func TestCreateEntity_Rollup_AggFieldFormulaDefaultResultType_Allowed(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "quantity", FieldType: "number"},
				{Name: "unit_price", FieldType: "number"},
				{Name: "line_total", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{
					"formula": "quantity * unit_price", // no resultType key
				})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "line_total",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("rollup over a default (omitted) resultType formula aggField rejected: %v", err)
	}
}

// TestCreateEntity_Rollup_AggFieldFormulaCurrencyResultType_Allowed (#262, 2026-09-30): a rollup
// aggField pointing at a `formula` field with resultType:"currency" is ALLOWED. That formula
// persists {amount,currencyCode} into obj_*.data exactly like a real currency field, so the
// rollup reads its ".amount" sub-path (the same AggregateScalar path as currency) and keeps the
// currency code. Before #262 it was rejected FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER, which
// forced both 09-30 journeys (clinic invoice total, repair parts total) to curl a number-typed
// shadow field.
func TestCreateEntity_Rollup_AggFieldFormulaCurrencyResultType_Allowed(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "amount", FieldType: "currency"},
				{Name: "probability", FieldType: "number"},
				{Name: "weighted_value", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{
					"formula": "amount * probability / 100", "resultType": "currency", "currencyCode": "USD",
				})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "weighted_value",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("rollup over a currency-resultType formula aggField rejected: %v", err)
	}
}

// TestCreateEntity_Rollup_AggFieldFormulaTextResultType_Rejected (specs/053): a text formula
// stores a string — SUM over it is meaningless — so it takes the same not-a-number reject.
func TestCreateEntity_Rollup_AggFieldFormulaTextResultType_Rejected(t *testing.T) {
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "amount", FieldType: "number"},
				{Name: "tier", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{
					"formula": `IF(amount >= 100, "Lớn", "Nhỏ")`, "resultType": "text",
				})},
			},
		},
	}
	svc := NewEntityService(&fakeSchema{}, meta)
	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "tier",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_ChildEntityNotFound: childEntity does not exist for this tenant.
func TestCreateEntity_Rollup_ChildEntityNotFound(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"accounts": true}}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "ghost_entity", "viaRelation": "account", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_CountNeedsNoAggField: metric=count validates without aggField even
// when the child has no numeric fields at all.
func TestCreateEntity_Rollup_CountNeedsNoAggField(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("count rollup with no aggField rejected: %v", err)
	}
}

// TestCreateEntity_Rollup_HappyPath_FKOnChild: a well-formed FK-on-child sum rollup succeeds.
func TestCreateEntity_Rollup_HappyPath_FKOnChild(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "amount", FieldType: "currency"},
			},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("valid FK-on-child rollup rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validateRollupCrossEntity — N-N mode (options.relationship).
// ---------------------------------------------------------------------------

// TestCreateEntity_Rollup_RelationshipNotFound: options.relationship names an N-N relationship
// that is not registered for this tenant.
func TestCreateEntity_Rollup_RelationshipNotFound(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "relationship": "ghost_rel", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_RelationshipWrongEntities: the named N-N relationship exists but does
// NOT connect this entity with childEntity.
func TestCreateEntity_Rollup_RelationshipWrongEntities(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"contacts_deals": {Name: "contacts_deals", ParentEntityName: "contacts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "relationship": "contacts_deals", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_RELATIONSHIP_WRONG_ENTITIES" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_RELATIONSHIP_WRONG_ENTITIES; err=%v", codeOf(err), err)
	}
}

// TestCreateEntity_Rollup_HappyPath_NN: a well-formed N-N count rollup succeeds.
func TestCreateEntity_Rollup_HappyPath_NN(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "relationship": "accounts_deals", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("valid N-N rollup rejected: %v", err)
	}
}

// TestCreateEntity_Rollup_RelationshipIsOneToN_Rejected (ENG-REL2 #4): GetRelationshipByName
// was broadened (migration 000028) to also resolve 1-N registry rows. A rollup's
// options.relationship must name an N-N join-table relation ONLY (the 1-N case is expressed
// via options.viaRelation) — a name that resolves to a 1-N row must be rejected exactly as it
// was invisible before the broadening (FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND, not a silent
// misapplication of N-N pivot semantics to an FK-on-child relation).
func TestCreateEntity_Rollup_RelationshipIsOneToN_Rejected(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{
		knownEntities: map[string]bool{"accounts": true, "deals": true},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "1-N"},
		},
	}
	svc := NewEntityService(schema, meta)

	in := accountsWithRollupFixture(map[string]any{
		"childEntity": "deals", "relationship": "accounts_deals", "metric": "count",
	})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND (1-N relationship name must not be usable for options.relationship)", codeOf(err))
	}
}

// ---------------------------------------------------------------------------
// AddField path — same cross-entity checks apply to an already-persisted entity.
// ---------------------------------------------------------------------------

func TestAddField_Rollup_HappyPath(t *testing.T) {
	schema := &fakeFieldSchema{}
	hostEntity := &metadata.EntityDefinition{ID: "ent-accounts", Name: "accounts"}
	childEntity := &metadata.EntityDefinition{ID: "ent-deals", Name: "deals"}
	fmeta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"accounts": hostEntity,
			"deals":    childEntity,
		},
		existing: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "amount", FieldType: "currency"},
			},
		},
	}
	svc := NewFieldService(schema, fmeta)

	_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_total", Label: "Deal Total", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount"},
		},
	})
	if err != nil {
		t.Fatalf("valid AddField rollup rejected: %v", err)
	}
}

// TestAddField_Rollup_AggFieldFormulaCurrency_Allowed (#262): the AddField-layer counterpart of
// TestCreateEntity_Rollup_AggFieldFormulaCurrencyResultType_Allowed — the path Studio's "Thêm
// trường" dialog takes (POST /admin/fields → field_orchestration.go → validateRollupCrossEntity).
func TestAddField_Rollup_AggFieldFormulaCurrency_Allowed(t *testing.T) {
	schema := &fakeFieldSchema{}
	hostEntity := &metadata.EntityDefinition{ID: "ent-accounts", Name: "accounts"}
	childEntity := &metadata.EntityDefinition{ID: "ent-deals", Name: "deals"}
	fmeta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"accounts": hostEntity,
			"deals":    childEntity,
		},
		existing: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {
				{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
				{Name: "computed_amount", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{"resultType": "currency"})},
			},
		},
	}
	svc := NewFieldService(schema, fmeta)

	_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_total", Label: "Deal Total", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "computed_amount"},
		},
	})
	if err != nil {
		t.Fatalf("AddField rollup over a currency-resultType formula rejected: %v", err)
	}
}

// TestAddField_Rollup_AggFieldFormulaNumberResultType_Allowed: the AddField-layer counterpart of
// TestCreateEntity_Rollup_AggFieldFormulaNumberResultType_Allowed — proves the relaxation is
// wired on BOTH orchestration paths (field_orchestration.go's AddField, not just
// entity_orchestration.go's CreateEntity), which is exactly what a deferred-field pack rollup
// (crmPack's crm_quotes.total) goes through.
func TestAddField_Rollup_AggFieldFormulaNumberResultType_Allowed(t *testing.T) {
	schema := &fakeFieldSchema{}
	hostEntity := &metadata.EntityDefinition{ID: "ent-quotes", Name: "quotes"}
	childEntity := &metadata.EntityDefinition{ID: "ent-lines", Name: "quote_lines"}
	fmeta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"quotes":      hostEntity,
			"quote_lines": childEntity,
		},
		existing: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-lines": {
				{Name: "quote", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "quotes"})},
				{Name: "quantity", FieldType: "number"},
				{Name: "unit_price", FieldType: "number"},
				{Name: "line_total", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{
					"formula": "quantity * unit_price", "resultType": "number",
				})},
			},
		},
	}
	svc := NewFieldService(schema, fmeta)

	_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
		EntityName: "quotes",
		Field: FieldInput{
			Name: "total", Label: "Total", FieldType: "rollup",
			Options: map[string]any{"childEntity": "quote_lines", "viaRelation": "quote", "metric": "sum", "aggField": "line_total"},
		},
	})
	if err != nil {
		t.Fatalf("valid AddField rollup over number-resultType formula rejected: %v", err)
	}
}
