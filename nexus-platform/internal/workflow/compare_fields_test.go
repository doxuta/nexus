package workflow

import "testing"

// queue #286: Rule.Value may name another field of the same record: {"field": "<name>"}.
func TestEvaluate_CompareFields(t *testing.T) {
	ref := map[string]any{"field": "si_so_toi_da"}
	cases := []struct {
		name string
		op   string
		data map[string]any
		want bool
	}{
		{"full class gte", OpGte, map[string]any{"da_ghi_danh": 3.0, "si_so_toi_da": 3.0}, true},
		{"room left", OpGte, map[string]any{"da_ghi_danh": 2.0, "si_so_toi_da": 3.0}, false},
		{"eq", OpEq, map[string]any{"da_ghi_danh": 3.0, "si_so_toi_da": 3.0}, true},
		{"currency amount", OpGt, map[string]any{"da_ghi_danh": map[string]any{"amount": 5.0}, "si_so_toi_da": 3.0}, true},
		{"other missing is false", OpGte, map[string]any{"da_ghi_danh": 3.0}, false},
	}
	for _, c := range cases {
		cond := &Condition{Rules: []Rule{{Field: "da_ghi_danh", Op: c.op, Value: ref}}}
		if got := Evaluate(cond, c.data); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRuleFieldRef(t *testing.T) {
	for _, c := range []struct {
		in   any
		name string
		ok   bool
	}{
		{map[string]any{"field": "a"}, "a", true},
		{map[string]any{"field": " a "}, "a", true},
		{map[string]any{"field": ""}, "", false},
		{map[string]any{"field": "a", "x": 1}, "", false},
		{map[string]any{"field": 3.0}, "", false},
		{"a", "", false},
		{nil, "", false},
	} {
		name, ok := RuleFieldRef(c.in)
		if name != c.name || ok != c.ok {
			t.Errorf("RuleFieldRef(%v)=%q,%v want %q,%v", c.in, name, ok, c.name, c.ok)
		}
	}
}
