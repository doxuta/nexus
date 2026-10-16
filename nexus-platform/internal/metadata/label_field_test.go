package metadata

import "testing"

func fd(name, typ string, required bool, order int) FieldDefinition {
	return FieldDefinition{Name: name, FieldType: typ, IsRequired: required, FieldOrder: order}
}

func TestResolveLabelField(t *testing.T) {
	opt := func(s string) *string { return &s }
	fields := []FieldDefinition{
		fd("note", "text", false, 10),
		fd("ho_ten", "text", true, 20),
		fd("so_gd", "auto_number", false, 30),
		fd("ngay", "date", true, 40),
	}
	tests := []struct {
		name     string
		opts     *string
		fields   []FieldDefinition
		want     string
		explicit bool
	}{
		{"explicit entity choice wins", opt(`{"labelField":"so_gd"}`), fields, "so_gd", true},
		{"explicit but unknown field falls to default", opt(`{"labelField":"ghost"}`), fields, "ho_ten", false},
		{"explicit but non-label type (date) falls to default", opt(`{"labelField":"ngay"}`), fields, "ho_ten", false},
		{"default prefers field called name", nil, append([]FieldDefinition{fd("name", "text", false, 99)}, fields...), "name", false},
		{"default first required text by order", nil, fields, "ho_ten", false},
		{"default keeps the FE-preferred names (full_name beats an earlier required code)", nil,
			[]FieldDefinition{fd("code", "text", true, 1), fd("Full_Name", "text", false, 2)}, "Full_Name", false},
		{"no required text -> first text", nil, []FieldDefinition{fd("b", "text", false, 20), fd("a", "text", false, 10)}, "a", false},
		{"only auto_number", nil, []FieldDefinition{fd("so_gd", "auto_number", false, 1)}, "so_gd", false},
		{"nothing usable", nil, []FieldDefinition{fd("ngay", "date", false, 1)}, "", false},
		{"corrupt options ignored", opt(`not json`), fields, "ho_ten", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, explicit := ResolveLabelField(tc.opts, tc.fields)
			if got != tc.want || explicit != tc.explicit {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, explicit, tc.want, tc.explicit)
			}
		})
	}
}

func TestValidateLabelFieldOption(t *testing.T) {
	fields := []FieldDefinition{fd("ho_ten", "text", true, 1), fd("ngay", "date", false, 2)}
	for _, tc := range []struct {
		name string
		v    any
		ok   bool
	}{
		{"valid", "ho_ten", true}, {"empty clears", "", true}, {"unknown", "ghost", false},
		{"wrong type field", "ngay", false}, {"not a string", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateLabelFieldOption(tc.v, fields); (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
		})
	}
}
