package services

import "testing"

// #365: email fields trim surrounding whitespace before the shape check and before storing,
// on create (ValidateRecord, also used by CSV import) and update (ValidatePartialRecord).
func TestEmailField_TrimBeforeValidate(t *testing.T) {
	specs := []FieldSpec{{Name: "mail", FieldType: "email"}}
	cases := []struct {
		name, in, want, code string
	}{
		{"leading+trailing", "  a@b.vn \t", "a@b.vn", ""},
		{"clean", "a@b.vn", "a@b.vn", ""},
		{"inner space still bad", "a @b.vn", "", "RECORD_INVALID_EMAIL"},
	}
	for _, c := range cases {
		for _, mode := range []string{"create", "update"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				var clean map[string]any
				var err error
				p := map[string]any{"mail": c.in}
				if mode == "create" {
					clean, err = ValidateRecord(specs, p)
				} else {
					clean, err = ValidatePartialRecord(specs, p)
				}
				if codeOf(err) != c.code {
					t.Fatalf("code=%q want %q (err=%v)", codeOf(err), c.code, err)
				}
				if c.code == "" && clean["mail"] != c.want {
					t.Fatalf("stored %q want %q", clean["mail"], c.want)
				}
			})
		}
	}
}
