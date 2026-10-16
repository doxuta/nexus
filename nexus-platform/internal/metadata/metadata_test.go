package metadata

import "testing"

// TestTableNames guards the metadata registry table mapping. If a TableName()
// drifts, dynamic reads would silently hit the wrong table — so we pin them.
func TestTableNames(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"entity", EntityDefinition{}.TableName(), "sys_entities"},
		{"field", FieldDefinition{}.TableName(), "sys_fields"},
		{"relationship", RelationshipDefinition{}.TableName(), "sys_relationships"},
		{"layout", LayoutDefinition{}.TableName(), "sys_layouts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("TableName() = %q, want %q", tc.got, tc.want)
			}
		})
	}
}
