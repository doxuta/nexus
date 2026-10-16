package services

// Queue #316 (formula-derives-hidden-field): a stored formula value is DERIVED from its operands,
// so hiding `amount` while serving `weighted_value = amount * probability / 100` hands the caller
// the hidden number back (divide by probability). The hidden set must therefore include every
// formula that reads a hidden field — one predicate (loadRestrictions) that every surface (record
// read strip, metadata, filter/sort guard, report, export, ⌘K) already consults.

import (
	"context"
	"errors"
	"sort"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

// formulaCatalog is a one-entity EntityCatalog; calls counts catalog reads so a test can prove the
// no-restriction fast path never touches it.
type formulaCatalog struct {
	fields   []metadata.FieldDefinition
	entErr   error
	fieldErr error
	calls    int
}

func (c *formulaCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	c.calls++
	if c.entErr != nil {
		return nil, c.entErr
	}
	return &metadata.EntityDefinition{ID: "ent-1", Name: name}, nil
}

func (c *formulaCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	c.calls++
	return c.fields, c.fieldErr
}

func formulaField(name, expr string) metadata.FieldDefinition {
	o := `{"formula":"` + expr + `","resultType":"number"}`
	return metadata.FieldDefinition{Name: name, FieldType: "formula", Options: &o}
}

func plainField(name, typ string) metadata.FieldDefinition {
	return metadata.FieldDefinition{Name: name, FieldType: typ}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFLS_HiddenFields_DerivesFormulaFromHiddenOperand(t *testing.T) {
	fields := []metadata.FieldDefinition{
		plainField("amount", "currency"),
		plainField("probability", "number"),
		plainField("qty", "number"),
		formulaField("weighted_value", "amount * probability / 100"),
		formulaField("units_x2", "qty * 2"),
		formulaField("tiered", "IF(probability > 50, weighted_value, 0)"), // formula over a derived-hidden formula
	}
	tests := []struct {
		name   string
		rows   []auth.FieldACL
		admin  bool
		want   []string
		noCall bool
		extra  metadata.FieldDefinition
	}{
		{name: "operand hidden hides the formula, and transitively the formula over it",
			rows: []auth.FieldACL{hide("amount")},
			want: []string{"amount", "tiered", "weighted_value"}},
		{name: "an unparsable formula is hidden once anything is hidden (cannot prove what it reads)",
			rows: []auth.FieldACL{hide("probability")}, extra: formulaField("broken", "amount *"),
			want: []string{"broken", "probability", "tiered", "weighted_value"}},
		{name: "formula over visible operands stays visible",
			rows: []auth.FieldACL{hide("qty")},
			want: []string{"qty", "units_x2"}},
		{name: "a hidden formula alone hides nothing else",
			rows: []auth.FieldACL{hide("units_x2")},
			want: []string{"units_x2"}},
		{name: "admin sees everything", rows: []auth.FieldACL{hide("amount")}, admin: true, want: []string{}},
		{name: "no restriction never reads the catalog", rows: nil, want: []string{}, noCall: true},
		{name: "read-only is not hidden", rows: []auth.FieldACL{lock("amount")}, want: []string{}, noCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat := &formulaCatalog{fields: append([]metadata.FieldDefinition{}, fields...)}
			if tt.extra.Name != "" {
				cat.fields = append(cat.fields, tt.extra)
			}
			svc := NewFieldACLService(&fakeFieldACLLoader{rows: tt.rows}).WithCatalog(cat)
			got, err := svc.HiddenFields(context.Background(), flsTenant, flsRole, flsEntity, tt.admin)
			if err != nil {
				t.Fatalf("HiddenFields: %v", err)
			}
			if g := sortedKeys(got); len(g) != len(tt.want) || (len(g) > 0 && !equalStrings(g, tt.want)) {
				t.Fatalf("hidden = %v, want %v", g, tt.want)
			}
			if tt.noCall && cat.calls != 0 {
				t.Fatalf("catalog read %d times with nothing hidden; must be 0", cat.calls)
			}
		})
	}
}

// The record read strip and the write guard ride the same set: the derived value is removed from
// the response data, and the formula cannot be written either (hidden ⊂ locked).
func TestFLS_FormulaDerivedHidden_StripsStoredValueAndLocksWrite(t *testing.T) {
	cat := &formulaCatalog{fields: []metadata.FieldDefinition{
		plainField("amount", "currency"), plainField("probability", "number"),
		formulaField("weighted_value", "amount * probability / 100"),
	}}
	svc := NewFieldACLService(&fakeFieldACLLoader{rows: []auth.FieldACL{hide("amount")}}).WithCatalog(cat)
	ctx := context.Background()

	data := map[string]any{"name": "Deal", "probability": float64(10), "weighted_value": float64(14000000)}
	if err := svc.FilterReadFields(ctx, flsTenant, flsRole, flsEntity, false, data); err != nil {
		t.Fatalf("FilterReadFields: %v", err)
	}
	if _, leaked := data["weighted_value"]; leaked {
		t.Fatalf("weighted_value (derived from hidden amount) leaked in read: %v", data)
	}
	if data["probability"] != float64(10) || data["name"] != "Deal" {
		t.Fatalf("visible fields must survive the strip: %v", data)
	}

	batch := []map[string]any{{"weighted_value": 1.0, "probability": 2.0}, {"weighted_value": 3.0}}
	if err := svc.FilterReadFieldsBatch(ctx, flsTenant, flsRole, flsEntity, false, batch); err != nil {
		t.Fatalf("FilterReadFieldsBatch: %v", err)
	}
	for i, row := range batch {
		if _, leaked := row["weighted_value"]; leaked {
			t.Fatalf("row %d leaks weighted_value: %v", i, row)
		}
	}

	if err := svc.AssertFieldsVisible(ctx, flsTenant, flsRole, flsEntity, false, []string{"weighted_value"}); err == nil {
		t.Fatalf("filtering/sorting by a derived-hidden formula must be refused (binary-search oracle)")
	}
}

func TestFLS_FormulaDerivedHidden_FailsClosed(t *testing.T) {
	rows := []auth.FieldACL{hide("amount")}
	boom := errors.New("catalog down")
	tests := []struct {
		name    string
		cat     *formulaCatalog
		wantErr bool
		want    []string
	}{
		{name: "entity read fails", cat: &formulaCatalog{entErr: boom}, wantErr: true},
		{name: "field read fails", cat: &formulaCatalog{fieldErr: boom}, wantErr: true},
		{name: "entity gone: nothing to derive", cat: &formulaCatalog{entErr: metadata.ErrEntityNotFound}, want: []string{"amount"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewFieldACLService(&fakeFieldACLLoader{rows: rows}).WithCatalog(tt.cat)
			got, err := svc.HiddenFields(context.Background(), flsTenant, flsRole, flsEntity, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !equalStrings(sortedKeys(got), tt.want) {
				t.Fatalf("hidden = %v, want %v", sortedKeys(got), tt.want)
			}
		})
	}
	// A strip over a failed derivation must drop every business field, never serve unverified data.
	svc := NewFieldACLService(&fakeFieldACLLoader{rows: rows}).WithCatalog(&formulaCatalog{fieldErr: boom})
	data := map[string]any{"weighted_value": 1.0, "name": "x"}
	if err := svc.FilterReadFields(context.Background(), flsTenant, flsRole, flsEntity, false, data); err == nil || len(data) != 0 {
		t.Fatalf("a failed derivation must fail closed (err + empty data); err=%v data=%v", err, data)
	}
}
