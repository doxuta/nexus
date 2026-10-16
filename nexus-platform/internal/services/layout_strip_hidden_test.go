package services

import (
	"encoding/json"
	"testing"
)

// TestStripHiddenLayoutFields pins the serve-time strip GET /metadata/layouts applies for a
// non-admin: every cell shape the layout engine emits (string cell, object cell, list column) that
// names a hidden field disappears, a section left with no real field goes with it, spacers and
// visible fields stay, single-field config keys are untouched, and an empty hidden set is a no-op.
func TestStripHiddenLayoutFields(t *testing.T) {
	hidden := map[string]struct{}{"revenue": {}}
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		return m
	}
	cases := []struct {
		name, view, in, want string
	}{
		{
			name: "edit: string cell + object cell dropped, emptied section dropped, spacer kept",
			view: LayoutViewEdit,
			in:   `{"version":1,"sections":[{"title":"A","columns":2,"fields":["name","revenue",{"spacer":true},{"field":"notes","fullWidth":true}]},{"title":"B","columns":1,"fields":[{"field":"revenue"}]}]}`,
			want: `{"version":1,"sections":[{"title":"A","columns":2,"fields":["name",{"spacer":true},{"field":"notes","fullWidth":true}]}]}`,
		},
		{
			name: "detail: same walk",
			view: LayoutViewDetail,
			in:   `{"version":1,"sections":[{"title":"A","columns":1,"fields":["revenue","name"]}]}`,
			want: `{"version":1,"sections":[{"title":"A","columns":1,"fields":["name"]}]}`,
		},
		{
			name: "list: column dropped, defaultSort (single-field config key) untouched",
			view: LayoutViewList,
			in:   `{"version":1,"columns":[{"field":"name","width":200},{"field":"revenue","width":120}],"defaultSort":{"field":"created_at","order":"desc"}}`,
			want: `{"version":1,"columns":[{"field":"name","width":200}],"defaultSort":{"field":"created_at","order":"desc"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := json.Marshal(StripHiddenLayoutFields(decode(tc.in), tc.view, hidden))
			want, _ := json.Marshal(decode(tc.want))
			if string(got) != string(want) {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
	// No hidden set → the layout comes back untouched.
	in := decode(`{"version":1,"sections":[{"title":"A","columns":1,"fields":["revenue"]}]}`)
	if out := StripHiddenLayoutFields(in, LayoutViewEdit, nil); len(out["sections"].([]any)) != 1 {
		t.Fatalf("empty hidden set must be a no-op, got %v", out)
	}
}
