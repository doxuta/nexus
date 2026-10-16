package ai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The same JSON is read by nexus-web/lib/appNav.test.ts: one merge rule, two implementations.
func TestMergeAppNav_SharedCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/app_nav_merge_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		ItemType string `json:"itemType"`
		Key      string `json:"key"`
		Label    string `json:"label,omitempty"`
	}
	var cases []struct {
		Name    string   `json:"name"`
		Current []item   `json:"current"`
		Add     []string `json:"add"`
		Want    []item   `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 5 {
		t.Fatalf("bad cases file: %v (%d)", err, len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var before []ContextNavItem
			for _, i := range c.Current {
				before = append(before, ContextNavItem{Type: i.ItemType, Name: i.Key, Label: i.Label})
			}
			var got []item
			for _, n := range mergeAppNav(before, c.Add) {
				got = append(got, item{n.Type, n.Name, n.Label})
			}
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %+v want %+v", got, c.Want)
			}
		})
	}
}
