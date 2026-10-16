package services

// queue #259 (visible-when-required-flip-guard, specs/054 §7): PUT /admin/fields/:id with
// isRequired=true on a field that the edit layout hides behind a visibleWhen condition used to save
// clean, then the NEXT layout save failed FR-6 (LAYOUT_VISIBLE_WHEN_INVALID) with no hint why. The
// edit now says it at the door: 422 FIELD_REQUIRED_HAS_VISIBLE_WHEN.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
)

type fakeLayoutRows struct {
	rows      []metadata.LayoutDefinition
	err       error
	calls     int
	gotTenant string
	gotEntity string
}

func (f *fakeLayoutRows) GetByEntity(_ context.Context, tenantID, entity string) ([]metadata.LayoutDefinition, error) {
	f.calls++
	f.gotTenant, f.gotEntity = tenantID, entity
	return f.rows, f.err
}

const vwEditLayout = `{"sections":[{"title":"A","columns":1,"fields":[
  "customer_type",
  {"field":"tax_code","visibleWhen":{"field":"customer_type","op":"eq","value":"business"}}]}]}`

func TestEditField_RequiredFlipOnConditionalField_Guard(t *testing.T) {
	yes, no := true, false
	editRow := metadata.LayoutDefinition{ViewType: LayoutViewEdit, LayoutJSON: vwEditLayout}
	siblings := []metadata.FieldDefinition{
		{Name: "customer_type", FieldType: "picklist"},
		{Name: "tax_code", FieldType: "text"},
	}

	tests := []struct {
		name       string
		target     string
		required   bool // stored
		in         EditFieldInput
		rows       []metadata.LayoutDefinition
		layoutErr  error
		sibs       []metadata.FieldDefinition
		wantCode   string
		wantErr    bool // infra error, nothing persisted
		wantRead   bool
		wantFields string
	}{
		{name: "flip ON for a field the edit layout hides conditionally -> 422 naming it",
			target: "tax_code", in: EditFieldInput{IsRequired: &yes}, rows: []metadata.LayoutDefinition{editRow}, sibs: siblings,
			wantCode: "FIELD_REQUIRED_HAS_VISIBLE_WHEN", wantRead: true, wantFields: "tax_code"},
		{name: "the controller itself has no condition -> allowed",
			target: "customer_type", in: EditFieldInput{IsRequired: &yes}, rows: []metadata.LayoutDefinition{editRow}, sibs: siblings, wantRead: true},
		{name: "no layout rows -> allowed",
			target: "tax_code", in: EditFieldInput{IsRequired: &yes}, sibs: siblings, wantRead: true},
		{name: "a condition only on the detail view does not hide the form cell -> allowed",
			target: "tax_code", in: EditFieldInput{IsRequired: &yes}, sibs: siblings, wantRead: true,
			rows: []metadata.LayoutDefinition{{ViewType: "detail", LayoutJSON: vwEditLayout}}},
		{name: "condition whose controller was deleted is ignored (serve-time prune) -> allowed",
			target: "tax_code", in: EditFieldInput{IsRequired: &yes}, rows: []metadata.LayoutDefinition{editRow}, wantRead: true,
			sibs: []metadata.FieldDefinition{{Name: "tax_code", FieldType: "text"}}},
		{name: "turning required OFF is the exit -> never judged, layout not read",
			target: "tax_code", required: true, in: EditFieldInput{IsRequired: &no}, rows: []metadata.LayoutDefinition{editRow}, sibs: siblings},
		{name: "already required (legacy) + relabel -> allowed, layout not read",
			target: "tax_code", required: true, in: EditFieldInput{Label: strPtr("MST")}, rows: []metadata.LayoutDefinition{editRow}, sibs: siblings},
		{name: "edit that does not touch isRequired -> layout not read",
			target: "tax_code", in: EditFieldInput{Label: strPtr("MST")}, rows: []metadata.LayoutDefinition{editRow}, sibs: siblings},
		{name: "layout read fails -> refuse (fail closed), nothing persisted",
			target: "tax_code", in: EditFieldInput{IsRequired: &yes}, layoutErr: errors.New("db down"), sibs: siblings, wantErr: true, wantRead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := metadata.FieldDefinition{ID: "fld-a", EntityID: "ent-1", Name: tt.target, Label: "Trường", FieldType: "text", IsRequired: tt.required}
			meta := &fakeEditMeta{field: &stored, siblings: tt.sibs}
			layouts := &fakeLayoutRows{rows: tt.rows, err: tt.layoutErr}
			svc, _ := editService(meta)
			svc.WithLayoutReader(layouts)

			in := tt.in
			in.FieldID = "fld-a"
			_, err := svc.EditField(context.Background(), "tenant-1", in)

			if (layouts.calls > 0) != tt.wantRead {
				t.Fatalf("layout reads = %d; want read=%v", layouts.calls, tt.wantRead)
			}
			if layouts.calls > 0 && (layouts.gotTenant != "tenant-1" || layouts.gotEntity != "courses") {
				t.Fatalf("layout read for (%q, %q); want tenant-pinned entity of the field", layouts.gotTenant, layouts.gotEntity)
			}
			switch {
			case tt.wantErr:
				if err == nil || codeOf(err) != "" {
					t.Fatalf("err = %v; want an infra error", err)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d writes after a failed guard", len(meta.updated))
				}
			case tt.wantCode != "":
				if got := codeOf(err); got != tt.wantCode {
					t.Fatalf("code = %q (%v); want %q", got, err, tt.wantCode)
				}
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Field != tt.wantFields {
					t.Fatalf("ValidationError.Field = %+v; want %q", ve, tt.wantFields)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d writes after a refusal", len(meta.updated))
				}
			default:
				if err != nil {
					t.Fatalf("EditField: %v; want success", err)
				}
				if len(meta.updated) != 1 {
					t.Fatalf("updated = %d; want the edit persisted once", len(meta.updated))
				}
			}
		})
	}
}

// Unwired (unit tests, tools) = no guard, exactly as before.
func TestEditField_RequiredFlip_UnwiredLayoutReaderIsNoGuard(t *testing.T) {
	yes := true
	stored := metadata.FieldDefinition{ID: "fld-a", EntityID: "ent-1", Name: "tax_code", FieldType: "text"}
	meta := &fakeEditMeta{field: &stored}
	svc, _ := editService(meta)
	if _, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-a", IsRequired: &yes}); err != nil {
		t.Fatalf("EditField: %v", err)
	}
}
