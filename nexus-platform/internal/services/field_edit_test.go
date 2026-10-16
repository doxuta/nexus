package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
)

// fakeEditMeta stubs the metadata reads/writes EditField depends on: a tenant-scoped
// field lookup by id, and the mutable-only update write. It records the field passed to
// UpdateFieldMutable so tests can assert exactly which attributes were persisted.
type fakeEditMeta struct {
	field     *metadata.FieldDefinition
	getErr    error
	updated   []metadata.FieldDefinition
	updateErr error

	gotUpdateTenant string // tenant id forwarded to UpdateFieldMutable (asserts re-pin)

	// siblings backs ListFields — the entity's active fields, which the formula edit path reads
	// for its ref check (queue #107). nil keeps the old "unused" error for every other test.
	siblings []metadata.FieldDefinition
}

// The edit path only needs the field getter + the mutable updater; the entity lookup,
// list, and create methods are unused here but required to satisfy the interface.
func (f *fakeEditMeta) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	return nil, errors.New("unused")
}

func (f *fakeEditMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	if f.siblings != nil {
		return f.siblings, nil
	}
	return nil, errors.New("unused")
}

func (f *fakeEditMeta) ListFieldsIncludingDeleted(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, errors.New("unused")
}

func (f *fakeEditMeta) CreateField(_ context.Context, _ *metadata.FieldDefinition) error {
	return errors.New("unused")
}

func (f *fakeEditMeta) GetFieldByID(_ context.Context, _, _ string) (*metadata.FieldDefinition, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	// Return a copy so the service mutating it cannot retroactively change the fixture.
	cp := *f.field
	return &cp, nil
}

func (f *fakeEditMeta) UpdateFieldMutable(_ context.Context, tenantID string, fd *metadata.FieldDefinition) error {
	f.gotUpdateTenant = tenantID
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, *fd)
	return nil
}

// SoftDeleteField satisfies the extended FieldMetadataRepo interface; the EditField path
// never calls it, so it is inert here.
func (f *fakeEditMeta) SoftDeleteField(_ context.Context, _, _ string) error {
	return nil
}

// GetRelationshipByName satisfies the extended FieldMetadataRepo interface (ENG-COMPUTED #3);
// the EditField path never calls it, so it is inert here.
func (f *fakeEditMeta) GetRelationshipByName(_ context.Context, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

// GetRelationshipByInverseField / CreateFieldWithRelationship / SoftDeleteRelationshipByChildFK
// satisfy the extended FieldMetadataRepo interface (ENG-REL2 #4); EditField never adds/deletes
// a field or touches the registry, so all three are inert here.
func (f *fakeEditMeta) GetRelationshipByInverseField(_ context.Context, _, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

func (f *fakeEditMeta) CreateFieldWithRelationship(_ context.Context, _ *metadata.FieldDefinition, _ *metadata.RelationshipDefinition) error {
	return errors.New("unused")
}

func (f *fakeEditMeta) SoftDeleteRelationshipByChildFK(_ context.Context, _, _, _ string) error {
	return nil
}

// GetEntityNameByID satisfies the extended FieldMetadataRepo interface (D1 indexed toggle).
// Returns "courses" (matching the storedField fixture's entity) unless getErr is set.
func (f *fakeEditMeta) GetEntityNameByID(_ context.Context, _, _ string) (string, error) {
	if f.getErr != nil {
		return "", metadata.ErrEntityNotFound
	}
	return "courses", nil
}

func storedField() *metadata.FieldDefinition {
	return &metadata.FieldDefinition{
		ID:         "fld-1",
		EntityID:   "ent-1",
		Name:       "status",
		Label:      "Trạng thái",
		FieldType:  "text",
		IsRequired: false,
		DefaultVal: "",
	}
}

func editInput() EditFieldInput {
	return EditFieldInput{
		FieldID:   "fld-1",
		Label:     strPtr("Trạng thái"),
		FieldType: "text", // unchanged
	}
}

// boolPtr / strPtr build pointers for the apply-when-non-nil edit semantics: a nil pointer
// means "field omitted, keep stored value", a non-nil pointer means "explicitly set".
func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// editService wires a FieldService whose schema repo must never be touched: editing a
// field performs NO DDL (only mutable metadata changes), so any schema call is a bug.
func editService(meta FieldMetadataRepo) (*FieldService, *fakeFieldSchema) {
	schema := &fakeFieldSchema{}
	return NewFieldService(schema, meta), schema
}

func TestEditField_UpdatesLabel(t *testing.T) {
	meta := &fakeEditMeta{field: storedField()}
	svc, schema := editService(meta)

	in := editInput()
	in.Label = strPtr("Tình trạng")
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.Label != "Tình trạng" {
		t.Errorf("Label = %q; want %q", fd.Label, "Tình trạng")
	}
	if len(meta.updated) != 1 || meta.updated[0].Label != "Tình trạng" {
		t.Errorf("update not persisted; updated=%v", meta.updated)
	}
	if len(schema.added) != 0 || len(schema.dropped) != 0 {
		t.Errorf("editing a field must NOT touch schema; added=%v dropped=%v", schema.added, schema.dropped)
	}
	// Defense-in-depth: the tenant is re-pinned at the update write, not only the read.
	if meta.gotUpdateTenant != "tenant-1" {
		t.Errorf("update must re-pin tenant; gotUpdateTenant=%q", meta.gotUpdateTenant)
	}
}

func TestEditField_UpdatesRequired(t *testing.T) {
	meta := &fakeEditMeta{field: storedField()}
	svc, _ := editService(meta)

	in := editInput()
	in.IsRequired = boolPtr(true)
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if !fd.IsRequired {
		t.Errorf("IsRequired = %v; want true", fd.IsRequired)
	}
	if len(meta.updated) != 1 || !meta.updated[0].IsRequired {
		t.Errorf("required not persisted; updated=%v", meta.updated)
	}
}

func TestEditField_UpdatesDefault(t *testing.T) {
	meta := &fakeEditMeta{field: storedField()}
	svc, _ := editService(meta)

	in := editInput()
	in.DefaultVal = strPtr("active")
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.DefaultVal != "active" {
		t.Errorf("DefaultVal = %q; want %q", fd.DefaultVal, "active")
	}
	if len(meta.updated) != 1 || meta.updated[0].DefaultVal != "active" {
		t.Errorf("default not persisted; updated=%v", meta.updated)
	}
}

// TestEditField_RejectsInvalidDefault is the #4 regression (round-4 MEDIUM): EditField must
// re-validate the EFFECTIVE default against the field's type+options at edit time (422),
// instead of silently persisting a junk default that only blows up later when a record is
// created. Each case mutates a typed stored field, sends a bad DefaultVal, and asserts the
// edit is rejected with the expected RECORD_* code AND that nothing was persisted.
func TestEditField_RejectsInvalidDefault(t *testing.T) {
	optsJSON := func(m map[string]any) *string {
		p, err := optionsJSON(m)
		if err != nil {
			t.Fatalf("optionsJSON: %v", err)
		}
		return p
	}
	cases := []struct {
		name       string
		fieldType  string
		options    *string
		newDefault string
		wantCode   string
	}{
		{"number default not a number", "number", nil, "abc", "RECORD_INVALID_TYPE"},
		{"rating default out of range", "rating",
			optsJSON(map[string]any{"min": float64(1), "max": float64(5)}), "9", "RECORD_INVALID_RANGE"},
		{"rating default non-integer", "rating",
			optsJSON(map[string]any{"min": float64(1), "max": float64(5)}), "2.5", "RECORD_INVALID_TYPE"},
		{"email default not an email", "email", nil, "not-an-email", "RECORD_INVALID_EMAIL"},
		{"picklist default outside options", "picklist",
			optsJSON(map[string]any{"values": []any{"a", "b"}}), "c", "RECORD_INVALID_PICKLIST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := storedField()
			stored.FieldType = tc.fieldType
			stored.Options = tc.options
			meta := &fakeEditMeta{field: stored}
			svc, _ := editService(meta)

			in := EditFieldInput{
				FieldID:    "fld-1",
				FieldType:  tc.fieldType, // unchanged (immutable)
				DefaultVal: strPtr(tc.newDefault),
			}
			_, err := svc.EditField(context.Background(), "tenant-1", in)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("EditField code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			// The invalid default must be rejected BEFORE the persist write.
			if len(meta.updated) != 0 {
				t.Errorf("invalid default must not be persisted; updated=%v", meta.updated)
			}
		})
	}
}

// TestEditField_AcceptsValidTypedDefault confirms the #4 guard does not over-reject: a
// well-formed typed default (a valid number, a valid picklist value) is accepted and persisted.
func TestEditField_AcceptsValidTypedDefault(t *testing.T) {
	t.Run("valid number default", func(t *testing.T) {
		stored := storedField()
		stored.FieldType = "number"
		meta := &fakeEditMeta{field: stored}
		svc, _ := editService(meta)

		in := EditFieldInput{FieldID: "fld-1", FieldType: "number", DefaultVal: strPtr("42")}
		fd, err := svc.EditField(context.Background(), "tenant-1", in)
		if err != nil {
			t.Fatalf("EditField: %v", err)
		}
		if fd.DefaultVal != "42" || len(meta.updated) != 1 {
			t.Errorf("valid number default not persisted; fd.DefaultVal=%q updated=%v", fd.DefaultVal, meta.updated)
		}
	})

	t.Run("valid picklist default in options", func(t *testing.T) {
		opts, _ := optionsJSON(map[string]any{"values": []any{"a", "b"}})
		stored := storedField()
		stored.FieldType = "picklist"
		stored.Options = opts
		meta := &fakeEditMeta{field: stored}
		svc, _ := editService(meta)

		in := EditFieldInput{FieldID: "fld-1", FieldType: "picklist", DefaultVal: strPtr("b")}
		fd, err := svc.EditField(context.Background(), "tenant-1", in)
		if err != nil {
			t.Fatalf("EditField: %v", err)
		}
		if fd.DefaultVal != "b" || len(meta.updated) != 1 {
			t.Errorf("valid picklist default not persisted; fd.DefaultVal=%q updated=%v", fd.DefaultVal, meta.updated)
		}
	})
}

// TestEditField_PartialBodyKeepsUnsetFields is the B-2 regression (spec §7.2): a partial
// PUT body that supplies only `label` must NOT wipe isRequired/defaultValue. Before the
// fix, value-type IsRequired bool / DefaultVal string always overwrote the stored values,
// so {"label":"X"} silently reset them to their zero values (false / ""). With
// apply-when-non-nil pointer semantics, omitted (nil) fields keep the stored value.
func TestEditField_PartialBodyKeepsUnsetFields(t *testing.T) {
	stored := storedField()
	stored.IsRequired = true // non-zero stored values that must survive a partial edit
	stored.DefaultVal = "active"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	// Partial body: change only the label. IsRequired / DefaultVal are omitted (nil).
	in := EditFieldInput{
		FieldID:   "fld-1",
		Label:     strPtr("Nhãn mới"),
		FieldType: "text",
		// IsRequired and DefaultVal intentionally left nil (not present in the PUT body).
	}
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if !fd.IsRequired {
		t.Errorf("IsRequired was wiped by partial edit; got %v want true (B-2)", fd.IsRequired)
	}
	if fd.DefaultVal != "active" {
		t.Errorf("DefaultVal was wiped by partial edit; got %q want %q (B-2)", fd.DefaultVal, "active")
	}
	if len(meta.updated) != 1 {
		t.Fatalf("expected exactly one persisted update; updated=%v", meta.updated)
	}
	if !meta.updated[0].IsRequired || meta.updated[0].DefaultVal != "active" {
		t.Errorf("partial edit persisted wiped values; updated=%+v (B-2)", meta.updated[0])
	}
	if meta.updated[0].Label != "Nhãn mới" {
		t.Errorf("label not persisted; got %q", meta.updated[0].Label)
	}
}

// TestEditField_PartialBodyAppliesExplicitFalse verifies the other half of pointer
// semantics: an explicit `isRequired:false` / `defaultValue:""` (non-nil pointer to the
// zero value) MUST be applied, clearing a previously-true/non-empty stored value.
func TestEditField_PartialBodyAppliesExplicitFalse(t *testing.T) {
	stored := storedField()
	stored.IsRequired = true
	stored.DefaultVal = "active"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.IsRequired = boolPtr(false) // explicit false must clear the stored true
	in.DefaultVal = strPtr("")     // explicit empty string must clear the stored default
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.IsRequired {
		t.Errorf("explicit isRequired:false not applied; got %v want false", fd.IsRequired)
	}
	if fd.DefaultVal != "" {
		t.Errorf("explicit defaultValue:\"\" not applied; got %q want empty", fd.DefaultVal)
	}
}

func TestEditField_UpdatesOptions(t *testing.T) {
	// A picklist field whose option list is being changed.
	stored := storedField()
	stored.FieldType = "picklist"
	existing := `{"values":["a","b"]}`
	stored.Options = &existing
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "picklist"
	in.Options = map[string]any{"values": []any{"a", "b", "c"}}
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.Options == nil || *fd.Options != `{"values":["a","b","c"]}` {
		t.Errorf("Options = %v; want updated values json", fd.Options)
	}
	if len(meta.updated) != 1 {
		t.Errorf("options not persisted; updated=%v", meta.updated)
	}
}

func TestEditField_RejectsFieldTypeChange(t *testing.T) {
	// Stored field is "text"; caller attempts to change it to "number" -> 422 code.
	meta := &fakeEditMeta{field: storedField()}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "number"
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_TYPE_IMMUTABLE" {
		t.Fatalf("code = %q; want FIELD_TYPE_IMMUTABLE", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("a rejected fieldType change must NOT persist; updated=%v", meta.updated)
	}
}

func TestEditField_EmptyFieldTypeKeepsStored(t *testing.T) {
	// An omitted fieldType (empty string) is not a change attempt — it keeps the stored
	// type and proceeds, so partial updates that don't echo fieldType still work.
	meta := &fakeEditMeta{field: storedField()}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = ""
	in.Label = strPtr("Mới")
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.FieldType != "text" {
		t.Errorf("FieldType = %q; want stored text", fd.FieldType)
	}
}

func TestEditField_UnknownFieldIsNotFound(t *testing.T) {
	meta := &fakeEditMeta{getErr: metadata.ErrFieldNotFound}
	svc, _ := editService(meta)

	_, err := svc.EditField(context.Background(), "tenant-1", editInput())
	if codeOf(err) != "FIELD_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_NOT_FOUND", codeOf(err))
	}
}

func TestEditField_TenantIsolation(t *testing.T) {
	// Tenant-B asking for tenant-A's field id: the tenant-scoped getter returns
	// ErrFieldNotFound, so the edit is a 404 — never leaking or mutating cross-tenant data.
	meta := &fakeEditMeta{getErr: metadata.ErrFieldNotFound}
	svc, _ := editService(meta)

	in := editInput()
	in.Label = strPtr("hijack")
	_, err := svc.EditField(context.Background(), "tenant-B", in)
	if codeOf(err) != "FIELD_NOT_FOUND" {
		t.Fatalf("cross-tenant edit code = %q; want FIELD_NOT_FOUND", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("cross-tenant edit must persist nothing; updated=%v", meta.updated)
	}
}

func TestEditField_PicklistWithoutOptionsRejected(t *testing.T) {
	// Changing a picklist's options to an empty/invalid set must be rejected with the
	// shared picklist rule code, before any persistence.
	stored := storedField()
	stored.FieldType = "picklist"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "picklist"
	in.Options = map[string]any{"values": []any{}}
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_PICKLIST_NO_OPTIONS" {
		t.Fatalf("code = %q; want FIELD_PICKLIST_NO_OPTIONS", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("invalid picklist must persist nothing; updated=%v", meta.updated)
	}
}

func TestEditField_PersistFailureSurfaced(t *testing.T) {
	meta := &fakeEditMeta{field: storedField(), updateErr: errors.New("db boom")}
	svc, _ := editService(meta)

	if _, err := svc.EditField(context.Background(), "tenant-1", editInput()); err == nil {
		t.Fatal("expected error when update write fails")
	}
}

// TestEditField_PartialBodyKeepsLabel is the B-2 regression for `label` specifically (bug
// #5 in GD1-review-bugs): a partial PUT body that omits "label" (nil pointer) must NOT wipe
// the stored label to "". Before the fix Label was a value-type string, so {"isRequired":
// true} silently reset the label to empty. With *string apply-when-non-nil, an omitted
// label keeps the stored value.
func TestEditField_PartialBodyKeepsLabel(t *testing.T) {
	stored := storedField() // Label = "Trạng thái"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	// Body changes only isRequired; label is omitted (nil) and must survive.
	in := EditFieldInput{
		FieldID:    "fld-1",
		FieldType:  "text",
		IsRequired: boolPtr(true),
		// Label intentionally nil (not present in the PUT body).
	}
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.Label != "Trạng thái" {
		t.Errorf("Label was wiped by partial edit; got %q want %q (B-2 label)", fd.Label, "Trạng thái")
	}
	if len(meta.updated) != 1 || meta.updated[0].Label != "Trạng thái" {
		t.Errorf("partial edit persisted a wiped label; updated=%+v (B-2 label)", meta.updated)
	}
}

// TestEditField_ExplicitEmptyLabelApplied verifies the other half of pointer semantics for
// label: an explicit `label:""` (non-nil pointer to the zero value) IS applied, clearing a
// previously non-empty stored label.
func TestEditField_ExplicitEmptyLabelApplied(t *testing.T) {
	stored := storedField() // Label = "Trạng thái"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.Label = strPtr("") // explicit empty string must clear the stored label
	fd, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.Label != "" {
		t.Errorf("explicit label:\"\" not applied; got %q want empty", fd.Label)
	}
}

// TestEditField_MultiSelectEmptyOptionsRejected covers bug #8 (GD1-review-bugs): EditField
// must run the single-source structural validator (validateFieldConfig), not just the
// picklist guard, so editing a multi_select field to empty options.values is rejected with
// the shared code — proving create/edit no longer drift on structural rules.
func TestEditField_MultiSelectEmptyOptionsRejected(t *testing.T) {
	stored := storedField()
	stored.FieldType = "multi_select"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "multi_select"
	in.Options = map[string]any{"values": []any{}}
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_MULTI_SELECT_NO_OPTIONS" {
		t.Fatalf("code = %q; want FIELD_MULTI_SELECT_NO_OPTIONS", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("invalid multi_select edit must persist nothing; updated=%v", meta.updated)
	}
}

// TestEditField_CompositeEmptySubFieldsRejected: editing a composite field to empty
// options.subFields must be rejected by validateFieldConfig (G3 structural rule) at edit
// time, exactly as at create time.
func TestEditField_CompositeEmptySubFieldsRejected(t *testing.T) {
	stored := storedField()
	stored.FieldType = "composite"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "composite"
	in.Options = map[string]any{"subFields": []any{}}
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_COMPOSITE_NO_SUBFIELDS" {
		t.Fatalf("code = %q; want FIELD_COMPOSITE_NO_SUBFIELDS", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("invalid composite edit must persist nothing; updated=%v", meta.updated)
	}
}

// TestEditField_ValidCompositeOptionsAccepted: a well-formed composite options edit passes
// validateFieldConfig and persists — confirming the new edit-time validation does not block
// legitimate option changes.
func TestEditField_ValidCompositeOptionsAccepted(t *testing.T) {
	stored := storedField()
	stored.FieldType = "composite"
	meta := &fakeEditMeta{field: stored}
	svc, _ := editService(meta)

	in := editInput()
	in.FieldType = "composite"
	in.Options = map[string]any{"subFields": []any{
		map[string]any{"name": "first_name", "fieldType": "text"},
		map[string]any{"name": "last_name", "fieldType": "text"},
	}}
	if _, err := svc.EditField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("valid composite edit should pass; got %v", err)
	}
	if len(meta.updated) != 1 {
		t.Errorf("valid composite edit must persist; updated=%v", meta.updated)
	}
}

// --- ENG-REL2 #4 review MED-2: relation options.relatedEntity / inverseField are IMMUTABLE --

// storedRelationField returns a persisted relation field whose registry row (parent
// "accounts", inverse "contacts") was written at AddField time. EditField must not let the
// stored options drift away from that registry row.
func storedRelationField() *metadata.FieldDefinition {
	opts := `{"relatedEntity":"accounts","inverseField":"contacts"}`
	return &metadata.FieldDefinition{
		ID:        "fld-rel",
		EntityID:  "ent-1",
		Name:      "account",
		Label:     "Account",
		FieldType: "relation",
		Options:   &opts,
	}
}

// TestEditField_RelationRelatedEntityImmutable: changing options.relatedEntity on a relation
// field would silently desync sys_fields from the 1-N registry row (parent_entity_name) AND
// from the physical <field>_id column semantics — reject 422, like FIELD_TYPE_IMMUTABLE.
func TestEditField_RelationRelatedEntityImmutable(t *testing.T) {
	meta := &fakeEditMeta{field: storedRelationField()}
	svc, _ := editService(meta)

	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel",
		Options: map[string]any{"relatedEntity": "companies", "inverseField": "contacts"},
	})
	if codeOf(err) != "FIELD_RELATION_OPTIONS_IMMUTABLE" {
		t.Fatalf("code = %q; want FIELD_RELATION_OPTIONS_IMMUTABLE", codeOf(err))
	}
	if len(meta.updated) != 0 {
		t.Errorf("rejected edit must not persist; updated=%v", meta.updated)
	}
}

// TestEditField_RelationInverseFieldImmutable: same for options.inverseField — the registry
// row's inverse_field was resolved and claimed at AddField time; editing it here would leave
// the claim stale (and the collision pre-check checking the wrong name).
func TestEditField_RelationInverseFieldImmutable(t *testing.T) {
	meta := &fakeEditMeta{field: storedRelationField()}
	svc, _ := editService(meta)

	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel",
		Options: map[string]any{"relatedEntity": "accounts", "inverseField": "members"},
	})
	if codeOf(err) != "FIELD_RELATION_OPTIONS_IMMUTABLE" {
		t.Fatalf("code = %q; want FIELD_RELATION_OPTIONS_IMMUTABLE", codeOf(err))
	}
}

// TestEditField_RelationInverseFieldOmittedIsAChange: dropping the inverseField key (present
// in storage) is also a change — apply-full-options semantics would silently erase it.
func TestEditField_RelationInverseFieldOmittedIsAChange(t *testing.T) {
	meta := &fakeEditMeta{field: storedRelationField()}
	svc, _ := editService(meta)

	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel",
		Options: map[string]any{"relatedEntity": "accounts"},
	})
	if codeOf(err) != "FIELD_RELATION_OPTIONS_IMMUTABLE" {
		t.Fatalf("code = %q; want FIELD_RELATION_OPTIONS_IMMUTABLE", codeOf(err))
	}
}

// TestEditField_RelationSameOptionsAllowed: resending the SAME relatedEntity/inverseField is
// not a change — label edits alongside untouched relation options must keep working.
func TestEditField_RelationSameOptionsAllowed(t *testing.T) {
	meta := &fakeEditMeta{field: storedRelationField()}
	svc, _ := editService(meta)

	fd, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel",
		Label:   strPtr("Công ty"),
		Options: map[string]any{"relatedEntity": "accounts", "inverseField": "contacts"},
	})
	if err != nil {
		t.Fatalf("EditField(same relation options): %v", err)
	}
	if fd.Label != "Công ty" {
		t.Errorf("Label = %q; want %q", fd.Label, "Công ty")
	}
}

// TestEditField_RelationOptionsUntouchedWhenNil: an edit that does not send options at all
// (nil) never trips the immutability guard — apply-when-non-nil semantics.
func TestEditField_RelationOptionsUntouchedWhenNil(t *testing.T) {
	meta := &fakeEditMeta{field: storedRelationField()}
	svc, _ := editService(meta)

	if _, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-rel",
		Label:   strPtr("Công ty"),
	}); err != nil {
		t.Fatalf("EditField(label only): %v", err)
	}
}
