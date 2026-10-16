package metadata

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// labelFieldTypes are the field types that can name a record: stored as a plain string.
var labelFieldTypes = map[string]bool{"text": true, "email": true, "auto_number": true}

// ResolveLabelField is the ONE source of "which field names a record of this entity" (#272):
// expand labels, record titles, Kanban/Calendar cards, related lists and import-by-name all read
// it. Order: the builder's choice (sys_entities.options.labelField, when it still names a usable
// field) -> a field called "name" -> first required text -> first text -> first email/auto_number.
// explicit reports whether the builder's choice won. Returns "" when no field can name a record.
func ResolveLabelField(entityOptions *string, fields []FieldDefinition) (name string, explicit bool) {
	byName := make(map[string]FieldDefinition, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}
	if entityOptions != nil && *entityOptions != "" {
		var o map[string]any
		if json.Unmarshal([]byte(*entityOptions), &o) == nil {
			if lf, _ := o["labelField"].(string); strings.TrimSpace(lf) != "" {
				lf = strings.TrimSpace(lf)
				if f, ok := byName[lf]; ok && labelFieldTypes[f.FieldType] {
					return lf, true
				}
			}
		}
	}
	// The same names the FE's display-name heuristic prefers (lib/recordLabel.ts tier 1), so a
	// table that was never configured keeps the title it already had.
	byLower := make(map[string]FieldDefinition, len(fields))
	for _, f := range fields {
		byLower[strings.ToLower(f.Name)] = f
	}
	for _, p := range []string{"name", "title", "label", "full_name", "display_name", "subject"} {
		if f, ok := byLower[p]; ok && labelFieldTypes[f.FieldType] {
			return f.Name, false
		}
	}
	ordered := append([]FieldDefinition(nil), fields...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].FieldOrder != ordered[j].FieldOrder {
			return ordered[i].FieldOrder < ordered[j].FieldOrder
		}
		return ordered[i].Name < ordered[j].Name
	})
	for _, pass := range []struct {
		types    []string
		required bool
	}{{[]string{"text"}, true}, {[]string{"text"}, false}, {[]string{"auto_number", "email"}, false}} {
		for _, f := range ordered {
			if (!pass.required || f.IsRequired) && contains(pass.types, f.FieldType) {
				return f.Name, false
			}
		}
	}
	return "", false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ValidateLabelFieldOption checks an entity options.labelField value at config time: a string
// naming an existing text/email/auto_number field of the entity ("" clears the choice).
func ValidateLabelFieldOption(v any, fields []FieldDefinition) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("options.labelField phải là tên một trường")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, f := range fields {
		if f.Name == s {
			if !labelFieldTypes[f.FieldType] {
				return fmt.Errorf("trường %q không dùng làm tên được (cần kiểu chữ, email hoặc số tự tăng)", s)
			}
			return nil
		}
	}
	return fmt.Errorf("trường %q không tồn tại trong bảng", s)
}
