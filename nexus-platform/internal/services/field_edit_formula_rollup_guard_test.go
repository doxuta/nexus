package services

// queue #234 (formula-resulttype-edit-rollup-guard): a formula that a rollup uses as its aggField
// is read by SUM/AVG through a path fixed by its resultType. Changing the resultType afterwards
// (number<->currency, ->text) left the stored rows in the OLD shape, so the rollup silently read
// NULL/0. EditField must refuse the change while a rollup depends on it, in EVERY direction.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// rollupEditMeta layers the rollup-dependant lookup on top of fakeEditMeta.
type rollupEditMeta struct {
	*fakeEditMeta
	users []string
	err   error
	calls []string
}

func (m *rollupEditMeta) ListRollupsAggregating(_ context.Context, tenantID, entity, field string) ([]string, error) {
	m.calls = append(m.calls, tenantID+"/"+entity+"."+field)
	return m.users, m.err
}

func TestEditField_FormulaResultType_RollupGuard(t *testing.T) {
	num := `{"formula":"qty * price"}`
	numExplicit := `{"formula":"qty * price","resultType":"number"}`
	cur := `{"formula":"qty * price","resultType":"currency","currencyCode":"VND"}`
	sibs := []metadata.FieldDefinition{
		{ID: "a", Name: "qty", FieldType: "number"},
		{ID: "b", Name: "price", FieldType: "number"},
	}
	opts := func(rt string) map[string]any {
		m := map[string]any{"formula": "qty * price"}
		if rt != "" {
			m["resultType"] = rt
		}
		if rt == "currency" {
			m["currencyCode"] = "VND"
		}
		return m
	}
	tests := []struct {
		name     string
		stored   string
		newOpts  map[string]any
		users    []string
		findErr  error
		wantCode string
		wantFind bool
	}{
		{"number→currency while a rollup sums it", num, opts("currency"), []string{"orders.total"}, nil, "FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE", true},
		{"currency→number while a rollup sums it", cur, opts(""), []string{"orders.total"}, nil, "FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE", true},
		{"number→text while a rollup sums it", numExplicit, map[string]any{"formula": `IF(qty > 1, "A", "B")`, "resultType": "text"}, []string{"orders.total"}, nil, "FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE", true},
		{"number→currency with no rollup using it", num, opts("currency"), nil, nil, "", true},
		{"same resultType (explicit number vs default number) is not judged", num, opts("number"), []string{"orders.total"}, nil, "", false},
		{"lookup read fails → fail closed", num, opts("currency"), nil, errors.New("db down"), "ERR", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stored := tc.stored
			f := &metadata.FieldDefinition{ID: "fld-1", EntityID: "ent-1", Name: "line_total", Label: "Thành tiền", FieldType: "formula", Options: &stored}
			meta := &rollupEditMeta{fakeEditMeta: &fakeEditMeta{field: f, siblings: sibs}, users: tc.users, err: tc.findErr}
			svc, _ := editService(meta)
			_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", Options: tc.newOpts})

			if got := len(meta.calls) > 0; got != tc.wantFind {
				t.Fatalf("rollup lookup called = %v (%v); want %v", got, meta.calls, tc.wantFind)
			}
			if tc.wantFind && meta.calls[0] != "tenant-1/courses.line_total" {
				t.Errorf("lookup scoped as %q; want tenant-1/courses.line_total", meta.calls[0])
			}
			var ve *ValidationError
			switch tc.wantCode {
			case "":
				if err != nil {
					t.Fatalf("EditField: %v", err)
				}
			case "ERR":
				if err == nil || errors.As(err, &ve) {
					t.Fatalf("err = %v; want infra error", err)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted on failed guard read")
				}
			default:
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v; want %s", err, tc.wantCode)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d update(s) on a refused edit; options must stay as stored", len(meta.updated))
				}
				if !strings.Contains(ve.Msg, "orders.total") || !strings.Contains(ve.Msg, "Thành tiền") {
					t.Errorf("message %q must name the field and the rollup", ve.Msg)
				}
			}
		})
	}
}

// queue #284: a stored formula a rollup sums must not become a formula that reads a lookup/rollup
// (computed at view, no longer written). Direct tests of the shared guard body (EditField and the
// AI propose_edit_field both call it).
func TestGuardFormulaBecomesDerived(t *testing.T) {
	sibs := []metadata.FieldDefinition{
		{Name: "qty", FieldType: "number"},
		{Name: "gia", FieldType: "lookup"},
		{Name: "tong", FieldType: "rollup"},
	}
	stored := metadata.FieldDefinition{Name: "line_total", Label: "Thành tiền", FieldType: "formula"}
	plain := map[string]any{"formula": "qty * 2"}
	tests := []struct {
		name       string
		storedOpts map[string]any
		newOpts    map[string]any
		users      []string
		findErr    error
		wantCode   string
		wantFind   bool
	}{
		{"becomes lookup-reading while a rollup sums it", plain, map[string]any{"formula": "qty * gia"}, []string{"orders.total"}, nil, "FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED", true},
		{"becomes rollup-reading while a rollup sums it", plain, map[string]any{"formula": "qty + tong"}, []string{"orders.total"}, nil, "FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED", true},
		{"no rollup uses it", plain, map[string]any{"formula": "qty * gia"}, nil, nil, "", true},
		{"stays plain", plain, map[string]any{"formula": "qty * 3"}, []string{"orders.total"}, nil, "", false},
		{"was already derived", map[string]any{"formula": "qty * gia"}, map[string]any{"formula": "qty * gia * 2"}, []string{"orders.total"}, nil, "", false},
		{"options not supplied", plain, nil, []string{"orders.total"}, nil, "", false},
		{"lookup read fails → fail closed", plain, map[string]any{"formula": "qty * gia"}, nil, errors.New("db down"), "ERR", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta := &rollupEditMeta{fakeEditMeta: &fakeEditMeta{}, users: tc.users, err: tc.findErr}
			err := GuardFormulaBecomesDerived(context.Background(), meta, "tenant-1", "courses", stored, tc.storedOpts, tc.newOpts, sibs)
			if got := len(meta.calls) > 0; got != tc.wantFind {
				t.Fatalf("rollup lookup called = %v; want %v", got, tc.wantFind)
			}
			var ve *ValidationError
			switch tc.wantCode {
			case "":
				if err != nil {
					t.Fatalf("err = %v; want nil", err)
				}
			case "ERR":
				if err == nil || errors.As(err, &ve) {
					t.Fatalf("err = %v; want infra error", err)
				}
			default:
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v; want %s", err, tc.wantCode)
				}
				if !strings.Contains(ve.Msg, "orders.total") || !strings.Contains(ve.Msg, "Thành tiền") {
					t.Errorf("message %q must name the field and the rollup", ve.Msg)
				}
			}
		})
	}
}

// End to end through EditField: the edit is refused before anything is persisted.
func TestEditField_FormulaBecomesDerived_RollupGuard(t *testing.T) {
	stored := `{"formula":"qty * price"}`
	f := &metadata.FieldDefinition{ID: "fld-1", EntityID: "ent-1", Name: "line_total", Label: "Thành tiền", FieldType: "formula", Options: &stored}
	sibs := []metadata.FieldDefinition{
		{ID: "a", Name: "qty", FieldType: "number"},
		{ID: "b", Name: "price", FieldType: "number"},
		{ID: "c", Name: "tong", FieldType: "rollup"},
	}
	meta := &rollupEditMeta{fakeEditMeta: &fakeEditMeta{field: f, siblings: sibs}, users: []string{"orders.total"}}
	svc, _ := editService(meta)
	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", Options: map[string]any{"formula": "qty * tong"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED" {
		t.Fatalf("err = %v; want FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED", err)
	}
	if len(meta.updated) != 0 {
		t.Fatalf("persisted %d update(s) on a refused edit", len(meta.updated))
	}
}
