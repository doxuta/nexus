package services

import (
	"strings"
)

// Relation picker dependent filter (queue #304): a `relation` field can narrow its picker by the
// value of ANOTHER relation field on the same form — Ticket.device lists only the devices of the
// customer already chosen on the ticket. Config lives on the dependent field's options:
//
//	"filterBy": {"field": "customer", "parentField": "customer"}
//
// field       = a relation field on the TARGET entity (device.customer);
// parentField = a sibling relation field on THIS entity (ticket.customer).
// The picker lists target records where field = <value of parentField>. It is a UX filter read by
// the form (GET /metadata/fields serves options verbatim); the write path is unchanged.
const (
	optFilterBy   = "filterBy"
	codeRelFilter = "FIELD_RELATION_FILTER_INVALID"
)

// validateRelationFilterConfig is the single-field structural check (every create/add/edit path):
// filterBy is a non-empty-string pair with no stray keys, and only a `relation` field may carry it
// (elsewhere it would be silently ignored).
func validateRelationFilterConfig(name, fieldType string, options map[string]any) error {
	raw, present := options[optFilterBy]
	if !present {
		return nil
	}
	if fieldType != "relation" {
		return invalid(codeRelFilter, "field %s %q: options.filterBy chỉ dùng cho field relation", fieldType, name)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return invalid(codeRelFilter, "field relation %q: options.filterBy phải là object {field, parentField}", name)
	}
	for k := range m {
		if k != "field" && k != "parentField" {
			return invalid(codeRelFilter, "field relation %q: options.filterBy có khoá lạ %q", name, k)
		}
	}
	for _, k := range []string{"field", "parentField"} {
		if s, ok := m[k].(string); !ok || strings.TrimSpace(s) == "" {
			return invalid(codeRelFilter, "field relation %q: options.filterBy.%s phải là chuỗi không rỗng", name, k)
		}
	}
	if m["parentField"] == name {
		return invalid(codeRelFilter, "field relation %q không thể lọc theo chính nó", name)
	}
	return nil
}

func relationFilterParent(options map[string]any) string {
	m, _ := options[optFilterBy].(map[string]any)
	s, _ := m["parentField"].(string)
	return s
}

// validateRelationFilterRefs checks f's filterBy.parentField against its LIVE siblings: it names a
// relation field on the same entity and the chain never loops back to f. stored is f's persisted
// options (nil on create/add): an unchanged parent that has since gone is tolerated, so an
// unrelated edit is never frozen by a parent deleted later (same rule as validateDependentRefs).
func validateRelationFilterRefs(f FieldInput, siblings []FieldInput, stored map[string]any) error {
	parent := relationFilterParent(f.Options)
	if parent == "" {
		return nil
	}
	byName := make(map[string]FieldInput, len(siblings))
	for _, s := range siblings {
		if s.Name != f.Name {
			byName[s.Name] = s
		}
	}
	p, ok := byName[parent]
	if !ok || p.FieldType != "relation" {
		if parent == relationFilterParent(stored) {
			return nil
		}
		return invalid(codeRelFilter,
			"field relation %q: options.filterBy.parentField %q phải là một field relation đang có trên cùng entity", f.Name, parent)
	}
	for cur, hops := parent, 0; cur != "" && hops <= len(byName); hops++ {
		next := relationFilterParent(byName[cur].Options)
		if next == f.Name {
			return invalid(codeRelFilter, "field relation %q: chuỗi filterBy tạo vòng (%q lọc ngược lại %q)", f.Name, cur, f.Name)
		}
		cur = next
	}
	return nil
}

func hasRelationFilter(options map[string]any) bool {
	_, ok := options[optFilterBy]
	return ok
}
