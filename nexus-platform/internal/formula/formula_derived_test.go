package formula

// queue #284 (formula-reads-lookup-rollup): a formula may read a `lookup` (when the value it
// mirrors is a number/currency) or a `rollup`. Those are resolved at READ time, so Validate only
// vouches for the type of what they yield; whether the formula is then computed at read is
// Derived's call.

import (
	"strings"
	"testing"
)

var derivedFields = []FieldInfo{
	{Name: "so_luong", FieldType: "number"},
	{Name: "don_gia", FieldType: "lookup", ValueType: "currency"},
	{Name: "ty_le", FieldType: "lookup", ValueType: "number"},
	{Name: "ten_dv", FieldType: "lookup", ValueType: "text"},
	{Name: "chua_ro", FieldType: "lookup"}, // target unresolved
	{Name: "tong_tien", FieldType: "rollup"},
	{Name: "giam_gia", FieldType: "number"},
	{Name: "ghi_chu", FieldType: "text"},
}

func TestValidate_LookupAndRollupRefs(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		wantCode string // "" = valid
	}{
		{"qty × lookup(currency)", "so_luong * don_gia", ""},
		{"rollup minus number", "tong_tien - giam_gia", ""},
		{"lookup(number) in a condition", "IF(ty_le > 1, so_luong, 0)", ""},
		{"lookup of text is not a number", "so_luong * ten_dv", "FIELD_FORMULA_NON_NUMERIC_REF"},
		{"lookup with unresolved target fails closed", "so_luong * chua_ro", "FIELD_FORMULA_NON_NUMERIC_REF"},
		{"plain text field stays refused", "so_luong * ghi_chu", "FIELD_FORMULA_NON_NUMERIC_REF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(tc.expr, "thanh_tien", derivedFields)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate(%q): %v", tc.expr, err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantCode+":") {
				t.Fatalf("Validate(%q) = %v; want %s", tc.expr, err, tc.wantCode)
			}
		})
	}
}

func TestDerived(t *testing.T) {
	fields := []FieldInfo{
		{Name: "a", FieldType: "number"},
		{Name: "l", FieldType: "lookup", ValueType: "number"},
		{Name: "r", FieldType: "rollup"},
	}
	tests := []struct {
		expr string
		want []string
	}{
		{"a * 2", nil},
		{"a * l", []string{"l"}},
		{"r - a", []string{"r"}},
		{"IF(l > 1, r, a) + l", []string{"l", "r"}},
	}
	for _, tc := range tests {
		got := Derived(tc.expr, fields)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Derived(%q) = %v; want %v", tc.expr, got, tc.want)
		}
	}
}
