package metadiff

import (
	"encoding/json"
	"fmt"
	"testing"
)

func dec(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Round trip (FR-056): the stored side as a service serves it vs the equivalent proposal.
func TestCanonicalize_SavedAsIsRoundTripEmpty(t *testing.T) {
	tests := []struct{ name, typ, stored, proposed string }{
		{"layout list: served sortable flag", "layout",
			`{"entityName":"p","viewType":"list","layout":{"version":1,"columns":[{"field":"a","sortable":true},{"field":"b","sortable":false}]}}`,
			`{"entityName":"p","viewType":"list","layout":{"version":1,"columns":[{"field":"a"},{"field":"b"}]}}`},
		{"workflow: interval, match, null condition", "workflow",
			`{"name":"r","entityName":"p","trigger":"after_create","condition":null,"actions":[{"type":"x"}],"isActive":true,"intervalSeconds":0}`,
			`{"name":"r","entityName":"p","trigger":"after_create","actions":[{"type":"x"}]}`},
		{"workflow: match and", "workflow",
			`{"condition":{"match":"and","rules":[{"field":"a"}]},"intervalSeconds":0,"isActive":true}`,
			`{"condition":{"rules":[{"field":"a"}]}}`},
		{"report: empty config keys", "report",
			`{"name":"r","type":"summary","config":{"groupBy":"s","filters":[]},"isActive":true}`,
			`{"name":"r","type":"summary","config":{"groupBy":"s"}}`},
		{"field: empty options", "field",
			`{"label":"L","description":"","options":{"values":["a","b"]}}`,
			`{"label":"L","options":{"values":["a","b"]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := dec(t, tc.stored), dec(t, tc.proposed)
			Canonicalize(tc.typ, a)
			Canonicalize(tc.typ, b)
			if d := Diff(a, b); len(d) != 0 {
				t.Fatalf("want empty diff, got %v", d)
			}
		})
	}
}

func TestDiff_OrderAndLength(t *testing.T) {
	a, b := dec(t, `{"actions":[{"m":"1"},{"m":"2"}],"nav":["x","y"]}`), dec(t, `{"actions":[{"m":"1"}],"nav":["y","x"]}`)
	if got := Diff(a, b); fmt.Sprint(got) != "[actions nav[0] nav[1]]" {
		t.Fatalf("got %v", got)
	}
	c, d := dec(t, `{"actions":[{"m":"1"},{"m":"2"}]}`), dec(t, `{"actions":[{"m":"1"},{"m":"3"}]}`)
	if got := Diff(c, d); fmt.Sprint(got) != "[actions[1].m]" {
		t.Fatalf("got %v", got)
	}
}

func TestLimit(t *testing.T) {
	p := []string{"a", "b", "c"}
	if kept, more := Limit(p, 2); len(kept) != 2 || more != 1 {
		t.Fatalf("got %v %d", kept, more)
	}
	if kept, more := Limit(p, 5); len(kept) != 3 || more != 0 {
		t.Fatalf("got %v %d", kept, more)
	}
}
