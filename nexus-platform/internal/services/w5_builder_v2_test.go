package services

// W5 Builder core v2 unit tests (docs/nexus-ux-revolution-plan.md §3.2/§3.3):
//   - fieldOrder / description validation (create + add + edit share one gate).
//   - fieldOrder assignment: slice position at entity-create, append (max+10) at add-field,
//     explicit value honored everywhere, mutable via edit-field.
//   - PUT /admin/fields/reorder service contract (membership, dup, empty, tenant scope).
//   - POST /admin/formula/validate dry-run — SAME codes as the field-create path.
//   - Layout cell schema v2: helpText (≤200 runes) + section columns widened to 1..4.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

func intPtr(n int) *int { return &n }

// w5Code extracts the ValidationError code, failing the test when err is not a typed
// ValidationError.
func w5Code(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	return ve.Code
}

// --- fieldOrder / description validation ---------------------------------------------------

func TestValidateAddField_OrderDescription(t *testing.T) {
	tests := []struct {
		name     string
		order    *int
		desc     string
		wantCode string // "" = valid
	}{
		{name: "nil order + no description is valid"},
		{name: "explicit zero order is valid", order: intPtr(0)},
		{name: "positive order is valid", order: intPtr(70)},
		{name: "negative order rejected", order: intPtr(-1), wantCode: "FIELD_ORDER_INVALID"},
		{name: "500-rune description is valid", desc: strings.Repeat("ế", 500)},
		{name: "501-rune description rejected (runes not bytes)", desc: strings.Repeat("ế", 501), wantCode: "FIELD_DESCRIPTION_TOO_LONG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := AddFieldInput{
				EntityName: "courses",
				Field: FieldInput{
					Name: "summary", Label: "Tóm tắt", FieldType: "text",
					FieldOrder: tt.order, Description: tt.desc,
				},
			}
			err := ValidateAddField(in)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if got := w5Code(t, err); got != tt.wantCode {
				t.Errorf("code = %q; want %q", got, tt.wantCode)
			}
		})
	}
}

func TestValidateCreateEntity_OrderDescriptionGate(t *testing.T) {
	in := CreateEntityInput{
		Name: "courses", Label: "Khoá học",
		Fields: []FieldInput{{Name: "title", Label: "Tên", FieldType: "text", FieldOrder: intPtr(-5)}},
	}
	if got := w5Code(t, ValidateCreateEntity(in)); got != "FIELD_ORDER_INVALID" {
		t.Errorf("code = %q; want FIELD_ORDER_INVALID", got)
	}
}

// --- fieldOrder assignment ------------------------------------------------------------------

func TestCreateEntity_FieldOrderFromSlicePosition(t *testing.T) {
	svc := NewEntityService(&fakeSchema{}, &fakeMeta{})
	_, fields, err := svc.CreateEntity(context.Background(), "tenant-1", CreateEntityInput{
		Name: "courses", Label: "Khoá học",
		Fields: []FieldInput{
			{Name: "title", Label: "Tên", FieldType: "text"},
			{Name: "level", Label: "Cấp", FieldType: "number"},
			{Name: "note", Label: "Ghi chú", FieldType: "text", FieldOrder: intPtr(5), Description: "mô tả W5"},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if fields[0].FieldOrder != 10 || fields[1].FieldOrder != 20 {
		t.Errorf("auto orders = %d,%d; want 10,20 (slice position × 10)", fields[0].FieldOrder, fields[1].FieldOrder)
	}
	if fields[2].FieldOrder != 5 {
		t.Errorf("explicit order = %d; want 5 (explicit wins over position)", fields[2].FieldOrder)
	}
	if fields[2].Description != "mô tả W5" {
		t.Errorf("description = %q; want persisted", fields[2].Description)
	}
}

func TestAddField_DefaultOrderAppendsAfterActiveMax(t *testing.T) {
	meta := &fakeFieldMeta{
		entity: entityFixture(),
		existing: []metadata.FieldDefinition{
			{Name: "a", FieldType: "text", FieldOrder: 10},
			{Name: "b", FieldType: "text", FieldOrder: 40},
		},
	}
	svc := NewFieldService(&fakeFieldSchema{}, meta)
	fd, err := svc.AddField(context.Background(), "tenant-1", ordinaryField())
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if fd.FieldOrder != 50 {
		t.Errorf("FieldOrder = %d; want 50 (max active 40 + 10)", fd.FieldOrder)
	}
}

func TestAddField_ExplicitOrderAndDescriptionPersisted(t *testing.T) {
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(&fakeFieldSchema{}, meta)
	in := ordinaryField()
	in.Field.FieldOrder = intPtr(7)
	in.Field.Description = "ghi chú admin"
	fd, err := svc.AddField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if fd.FieldOrder != 7 || fd.Description != "ghi chú admin" {
		t.Errorf("got order=%d desc=%q; want 7 / %q", fd.FieldOrder, fd.Description, "ghi chú admin")
	}
	if len(meta.created) != 1 || meta.created[0].FieldOrder != 7 {
		t.Errorf("persisted order = %v; want 7", meta.created)
	}
}

func TestEditField_OrderDescriptionMutable(t *testing.T) {
	stored := storedField()
	stored.FieldOrder = 30
	stored.Description = "cũ"
	meta := &fakeEditMeta{field: stored}
	svc := NewFieldService(&fakeFieldSchema{}, meta)

	// Apply-when-non-nil: set both.
	fd, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-1", FieldOrder: intPtr(70), Description: strPtr("mới"),
	})
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if fd.FieldOrder != 70 || fd.Description != "mới" {
		t.Errorf("got order=%d desc=%q; want 70/mới", fd.FieldOrder, fd.Description)
	}

	// Omitted pointers keep stored values.
	meta2 := &fakeEditMeta{field: stored}
	svc2 := NewFieldService(&fakeFieldSchema{}, meta2)
	fd2, err := svc2.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "fld-1", Label: strPtr("Nhãn mới"),
	})
	if err != nil {
		t.Fatalf("EditField(partial): %v", err)
	}
	if fd2.FieldOrder != 30 || fd2.Description != "cũ" {
		t.Errorf("partial edit wiped order/description: order=%d desc=%q; want 30/cũ", fd2.FieldOrder, fd2.Description)
	}
}

func TestEditField_OrderDescriptionInvalid(t *testing.T) {
	meta := &fakeEditMeta{field: storedField()}
	svc := NewFieldService(&fakeFieldSchema{}, meta)

	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", FieldOrder: intPtr(-2)})
	if got := w5Code(t, err); got != "FIELD_ORDER_INVALID" {
		t.Errorf("code = %q; want FIELD_ORDER_INVALID", got)
	}
	_, err = svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", Description: strPtr(strings.Repeat("ậ", 501))})
	if got := w5Code(t, err); got != "FIELD_DESCRIPTION_TOO_LONG" {
		t.Errorf("code = %q; want FIELD_DESCRIPTION_TOO_LONG", got)
	}
	if len(meta.updated) != 0 {
		t.Errorf("invalid edits must not persist; updated=%v", meta.updated)
	}
}

// --- reorder ---------------------------------------------------------------------------------

// fakeReorderRepo records the renumber call (the W5 FieldReorderRepo seam).
type fakeReorderRepo struct {
	gotTenant string
	gotEntity string
	gotIDs    []string
	err       error
}

func (f *fakeReorderRepo) ReorderFields(_ context.Context, tenantID, entityID string, ids []string) error {
	if f.err != nil {
		return f.err
	}
	f.gotTenant, f.gotEntity, f.gotIDs = tenantID, entityID, ids
	return nil
}

func reorderFixtureMeta() *fakeFieldMeta {
	return &fakeFieldMeta{
		entity: entityFixture(), // ID "ent-1", tenant-1, name "courses"
		existing: []metadata.FieldDefinition{
			{ID: "f1", Name: "a", FieldType: "text", FieldOrder: 10},
			{ID: "f2", Name: "b", FieldType: "text", FieldOrder: 20},
			{ID: "f3", Name: "c", FieldType: "text", FieldOrder: 30},
		},
	}
}

func TestReorderFields(t *testing.T) {
	tests := []struct {
		name       string
		entityName string
		order      []string
		repoErr    error
		entityErr  error
		wantCode   string // "" = success
		wantIDs    []string
	}{
		{name: "happy path renumbers in given order", entityName: "courses", order: []string{"f3", "f1", "f2"}, wantIDs: []string{"f3", "f1", "f2"}},
		{name: "empty order rejected", entityName: "courses", order: nil, wantCode: "FIELD_REORDER_INVALID"},
		{name: "duplicate id rejected", entityName: "courses", order: []string{"f1", "f1"}, wantCode: "FIELD_REORDER_INVALID"},
		{name: "blank id rejected", entityName: "courses", order: []string{"f1", "  "}, wantCode: "FIELD_REORDER_INVALID"},
		{name: "id of another entity/tenant rejected", entityName: "courses", order: []string{"f1", "forged"}, wantCode: "FIELD_REORDER_UNKNOWN_FIELD"},
		{name: "unknown entity is 404", entityName: "ghost", order: []string{"f1"}, entityErr: metadata.ErrEntityNotFound, wantCode: "ENTITY_NOT_FOUND"},
		{name: "concurrent delete surfaces as unknown field", entityName: "courses", order: []string{"f1", "f2", "f3"}, repoErr: metadata.ErrFieldNotFound, wantCode: "FIELD_REORDER_UNKNOWN_FIELD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := reorderFixtureMeta()
			if tt.entityErr != nil {
				meta.entityErr = tt.entityErr
			}
			repo := &fakeReorderRepo{err: tt.repoErr}
			svc := NewFieldService(&fakeFieldSchema{}, meta).WithReorderRepo(repo)

			updated, err := svc.ReorderFields(context.Background(), "tenant-1", ReorderFieldsInput{
				EntityName: tt.entityName, Order: tt.order,
			})
			if tt.wantCode != "" {
				if got := w5Code(t, err); got != tt.wantCode {
					t.Fatalf("code = %q; want %q", got, tt.wantCode)
				}
				if tt.repoErr == nil && repo.gotIDs != nil {
					t.Errorf("repo must not be called on validation reject; got %v", repo.gotIDs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReorderFields: %v", err)
			}
			if updated != len(tt.wantIDs) {
				t.Errorf("updated = %d; want %d", updated, len(tt.wantIDs))
			}
			if repo.gotTenant != "tenant-1" || repo.gotEntity != "ent-1" {
				t.Errorf("repo scoped to (%q,%q); want (tenant-1, ent-1)", repo.gotTenant, repo.gotEntity)
			}
			if strings.Join(repo.gotIDs, ",") != strings.Join(tt.wantIDs, ",") {
				t.Errorf("repo ids = %v; want %v", repo.gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestReorderFields_NoRepoWiredIsInternalError(t *testing.T) {
	svc := NewFieldService(&fakeFieldSchema{}, reorderFixtureMeta()) // fakeFieldMeta does NOT implement FieldReorderRepo
	_, err := svc.ReorderFields(context.Background(), "tenant-1", ReorderFieldsInput{EntityName: "courses", Order: []string{"f1"}})
	if err == nil {
		t.Fatal("want error when no reorder repo is wired")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("want internal (non-validation) error, got ValidationError %q", ve.Code)
	}
}

// --- formula validate (dry-run) ---------------------------------------------------------------

func formulaFixtureMeta() *fakeFieldMeta {
	return &fakeFieldMeta{
		entity: entityFixture(),
		existing: []metadata.FieldDefinition{
			{Name: "price", FieldType: "number"},
			{Name: "qty", FieldType: "number"},
			{Name: "note", FieldType: "text"},
			{Name: "total", FieldType: "formula"},
		},
	}
}

func TestValidateFormula(t *testing.T) {
	tests := []struct {
		name       string
		entityName string
		formula    string
		resultType string
		entityErr  error
		wantCode   string   // "" = valid
		wantFields []string // referenced field names on success
		wantRT     string
	}{
		{name: "valid formula returns refs + default resultType", entityName: "courses", formula: "price * qty", wantFields: []string{"price", "qty"}, wantRT: "number"},
		{name: "explicit currency resultType echoed", entityName: "courses", formula: "price * 1.1", resultType: "currency", wantFields: []string{"price"}, wantRT: "currency"},
		{name: "literal-only formula has no refs", entityName: "courses", formula: "1 + 2", wantFields: []string{}, wantRT: "number"},
		{name: "unknown ref", entityName: "courses", formula: "price * missing", wantCode: "FIELD_FORMULA_UNKNOWN_REF"},
		{name: "non-numeric ref", entityName: "courses", formula: "note + 1", wantCode: "FIELD_FORMULA_NON_NUMERIC_REF"},
		{name: "nested formula ref", entityName: "courses", formula: "total + 1", wantCode: "FIELD_FORMULA_NESTED"},
		{name: "syntax error", entityName: "courses", formula: "price +", wantCode: "FIELD_FORMULA_INVALID"},
		{name: "empty formula", entityName: "courses", formula: "  ", wantCode: "FIELD_FORMULA_INVALID"},
		{name: "bad resultType", entityName: "courses", formula: "price", resultType: "boolean", wantCode: "FIELD_FORMULA_INVALID"},
		{name: "number formula with resultType text (specs/053)", entityName: "courses", formula: "price", resultType: "text", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "unknown entity is 404", entityName: "ghost", formula: "price", entityErr: metadata.ErrEntityNotFound, wantCode: "ENTITY_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := formulaFixtureMeta()
			if tt.entityErr != nil {
				meta.entityErr = tt.entityErr
			}
			svc := NewFieldService(&fakeFieldSchema{}, meta)
			res, err := svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
				EntityName: tt.entityName, Formula: tt.formula, ResultType: tt.resultType,
			})
			if tt.wantCode != "" {
				if got := w5Code(t, err); got != tt.wantCode {
					t.Fatalf("code = %q; want %q", got, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFormula: %v", err)
			}
			if !res.Valid {
				t.Error("Valid = false; want true")
			}
			if strings.Join(res.Fields, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("Fields = %v; want %v", res.Fields, tt.wantFields)
			}
			if res.ResultType != tt.wantRT {
				t.Errorf("ResultType = %q; want %q", res.ResultType, tt.wantRT)
			}
		})
	}
}

// --- layout cell schema v2 (helpText + 4 columns) ----------------------------------------------

func w5LayoutFields() []metadata.FieldDefinition {
	return []metadata.FieldDefinition{
		{Name: "title", FieldType: "text"},
		{Name: "level", FieldType: "number"},
	}
}

func w5EditLayout(sectionCols int, cell map[string]any) map[string]any {
	return map[string]any{
		"version": 1,
		"sections": []any{
			map[string]any{
				"columns": sectionCols,
				"fields":  []any{cell, "level"},
			},
		},
	}
}

func TestValidateLayout_CellHelpTextAndFourColumns(t *testing.T) {
	tests := []struct {
		name    string
		layout  map[string]any
		wantErr bool
	}{
		{
			name:   "helpText string is valid",
			layout: w5EditLayout(2, map[string]any{"field": "title", "helpText": "Tên hiển thị với khách"}),
		},
		{
			name:    "helpText non-string rejected",
			layout:  w5EditLayout(2, map[string]any{"field": "title", "helpText": 42}),
			wantErr: true,
		},
		{
			name:    "helpText over 200 runes rejected",
			layout:  w5EditLayout(2, map[string]any{"field": "title", "helpText": strings.Repeat("ệ", 201)}),
			wantErr: true,
		},
		{
			name:   "helpText exactly 200 runes is valid (rune-counted)",
			layout: w5EditLayout(2, map[string]any{"field": "title", "helpText": strings.Repeat("ệ", 200)}),
		},
		{
			name:   "section columns 4 with colspan 4 is valid",
			layout: w5EditLayout(4, map[string]any{"field": "title", "colspan": 4}),
		},
		{
			name:    "section columns 5 rejected",
			layout:  w5EditLayout(5, map[string]any{"field": "title"}),
			wantErr: true,
		},
		{
			name:    "colspan above section columns still rejected",
			layout:  w5EditLayout(3, map[string]any{"field": "title", "colspan": 4}),
			wantErr: true,
		},
		{
			name: "helpText on spacer cell rejected (unknown key)",
			layout: map[string]any{
				"version": 1,
				"sections": []any{map[string]any{
					"columns": 2,
					"fields":  []any{map[string]any{"spacer": true, "helpText": "x"}, "title"},
				}},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLayout("edit", tt.layout, w5LayoutFields())
			if tt.wantErr {
				if err == nil {
					t.Fatal("want LAYOUT_INVALID, got nil")
				}
				if got := w5Code(t, err); got != "LAYOUT_INVALID" {
					t.Errorf("code = %q; want LAYOUT_INVALID", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("want valid, got %v", err)
			}
		})
	}
}
