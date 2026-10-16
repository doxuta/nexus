package metadiff

import (
	"encoding/json"
	"reflect"
	"testing"
)

func j(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMerge3(t *testing.T) {
	tests := []struct {
		name, typ       string
		base, ours, thr string
		want            string // "" ⇒ not mergeable
		taken           []string
	}{
		{"disjoint keys", "workflow",
			`{"a":1,"cond":{"x":1},"act":"old"}`, `{"a":1,"cond":{"x":2},"act":"old"}`, `{"a":1,"cond":{"x":1},"act":"new"}`,
			`{"a":1,"cond":{"x":2},"act":"new"}`, []string{"act"}},
		{"same path both sides", "workflow",
			`{"act":"old"}`, `{"act":"mine"}`, `{"act":"new"}`, "", nil},
		{"both made the same edit", "workflow",
			`{"act":"old"}`, `{"act":"new"}`, `{"act":"new"}`, `{"act":"new"}`, nil},
		{"array is a block: one side edits", "workflow",
			`{"l":[1,2]}`, `{"l":[1,2,3]}`, `{"l":[1,2]}`, `{"l":[1,2,3]}`, nil},
		{"array is a block: both edit", "workflow",
			`{"l":[1,2]}`, `{"l":[1,2,3]}`, `{"l":[1,2,4]}`, "", nil},
		{"picklist adds both sides keeps builder order", "field",
			`{"options":{"values":["A","B"]}}`, `{"options":{"values":["A","VIP","B"]}}`, `{"options":{"values":["A","B","Đối tác"]}}`,
			`{"options":{"values":["A","VIP","B","Đối tác"]}}`, []string{"options.values"}},
		{"picklist never drops a builder value", "field",
			`{"options":{"values":["A","B"]}}`, `{"options":{"values":["A","B","VIP"]}}`, `{"options":{"values":["A"]}}`,
			`{"options":{"values":["A","B","VIP"]}}`, nil},
		{"role actions by (entity,action)", "role",
			`{"actions":[{"entityName":"a","action":"read"}]}`,
			`{"actions":[{"entityName":"a","action":"read"},{"entityName":"b","action":"read"}]}`,
			`{"actions":[{"entityName":"a","action":"read"},{"entityName":"a","action":"create"}]}`,
			`{"actions":[{"entityName":"a","action":"read"},{"entityName":"b","action":"read"},{"entityName":"a","action":"create"}]}`,
			[]string{"actions"}},
		{"dashboard widget by id: pack edits one, builder adds one", "dashboard",
			`{"widgets":[{"id":"w1","title":"x"}]}`,
			`{"widgets":[{"id":"w1","title":"x"},{"id":"w9","title":"mine"}]}`,
			`{"widgets":[{"id":"w1","title":"y"}]}`,
			`{"widgets":[{"id":"w1","title":"y"},{"id":"w9","title":"mine"}]}`, []string{"widgets[0].title"}},
		{"approval step edited both sides", "approval",
			`{"steps":[{"id":"s1","n":1}]}`, `{"steps":[{"id":"s1","n":2}]}`, `{"steps":[{"id":"s1","n":3}]}`, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, taken, ok := Merge3(tc.typ, j(t, tc.base), j(t, tc.ours), j(t, tc.thr))
			if tc.want == "" {
				if ok {
					t.Fatalf("expected conflict, merged %v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected merge, got conflict")
			}
			if !reflect.DeepEqual(got, j(t, tc.want)) {
				t.Fatalf("merged = %v, want %s", got, tc.want)
			}
			if len(tc.taken) > 0 && !reflect.DeepEqual(taken, tc.taken) {
				t.Fatalf("taken = %v, want %v", taken, tc.taken)
			}
		})
	}
}
