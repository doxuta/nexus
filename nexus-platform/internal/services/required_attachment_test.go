package services

import "testing"

// #350: a file/image field can only be uploaded AFTER the record exists (files API), so "required"
// cannot be judged at create — it used to make every create 422 RECORD_REQUIRED_MISSING.
func TestValidateRecord_RequiredAttachmentDoesNotBlockCreate(t *testing.T) {
	cases := []struct {
		name, fieldType string
	}{
		{"image", "image"},
		{"file", "file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			specs := []FieldSpec{
				{Name: "tieu_de", FieldType: "text", IsRequired: true},
				{Name: "anh", FieldType: c.fieldType, IsRequired: true},
			}
			clean, err := ValidateRecord(specs, map[string]any{"tieu_de": "Phiếu 1"})
			if err != nil {
				t.Fatalf("create without attachment: %v", err)
			}
			if _, has := clean["anh"]; has {
				t.Fatalf("attachment key must not be materialized: %v", clean)
			}
		})
	}
	t.Run("other required types still enforced", func(t *testing.T) {
		specs := []FieldSpec{{Name: "tieu_de", FieldType: "text", IsRequired: true}}
		if _, err := ValidateRecord(specs, map[string]any{}); codeOf(err) != "RECORD_REQUIRED_MISSING" {
			t.Fatalf("code=%q want RECORD_REQUIRED_MISSING", codeOf(err))
		}
	})
}
