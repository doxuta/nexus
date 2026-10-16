package vertical_pack

import "testing"

// DAILY-QUEUE #4 (sortable-columns-from-grid) — writer invariant, pack half.
//
// A layout's `sortable` flag is decided at serve time from the field's real sort capability
// (GetLayout → buildSortAllowlist); no writer of layout JSON may carry an opinion. The built-in
// packs used to ship ten `"sortable": true` literals — every one on a plain text field the engine
// cannot sort — so the header promised a sort the list endpoint answered with 422. The builder
// half of the same invariant lives in nexus-web/components/studio/LayoutBuilder.tsx (buildListJson
// emits no `sortable`); this test pins the pack half so a future pack author cannot reintroduce it.
func TestBuiltinPacks_NoLayoutColumnWritesSortable(t *testing.T) {
	for _, slug := range BuiltinSlugs() {
		pack, ok := BuiltinPack(slug)
		if !ok {
			t.Fatalf("pack %q listed but not buildable", slug)
		}
		for _, l := range pack.Layouts {
			if l.ViewType != "list" {
				continue
			}
			cols, _ := l.LayoutJSON["columns"].([]any)
			for _, colRaw := range cols {
				col, _ := colRaw.(map[string]any)
				if v, has := col["sortable"]; has {
					t.Errorf("pack %q layout %s:list column %q writes sortable=%v — serve time is the only decider; drop the key",
						slug, l.EntityName, col["field"], v)
				}
			}
		}
	}
}
