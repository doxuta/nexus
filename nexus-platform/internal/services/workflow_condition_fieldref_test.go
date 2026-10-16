package services

import "testing"

// queue #286: a condition value may reference another field; a malformed object is refused.
func TestBuildConditionJSON_FieldRef(t *testing.T) {
	ok := []WorkflowRuleInput{{Field: "da_ghi_danh", Op: "gte", Value: map[string]any{"field": "si_so_toi_da"}}}
	if _, err := buildConditionJSON("and", ok, nil, ""); err != nil {
		t.Fatalf("valid field ref: %v", err)
	}
	for name, v := range map[string]any{
		"empty name":  map[string]any{"field": ""},
		"extra key":   map[string]any{"field": "a", "x": 1},
		"unknown key": map[string]any{"foo": "a"},
	} {
		bad := []WorkflowRuleInput{{Field: "a", Op: "gte", Value: v}}
		if _, err := buildConditionJSON("and", bad, nil, ""); err == nil {
			t.Errorf("%s: want rejection", name)
		}
	}
}
