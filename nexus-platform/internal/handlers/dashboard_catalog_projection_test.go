package handlers

// GET /dashboards is the OPEN discovery list: authenticated but ungranted by design, so any
// employee can find a dashboard by name. It used to return the repository row WHOLE, and that row
// carries `widgets` — each widget's entity, metric, field, groupBy and filters. A caller with no
// grant on those tables could therefore read their names and what is measured on them, from an
// endpoint whose own doc comment promised "id, name, description".
//
// The fix is a PROJECTION, not a removal, and this test pins both halves: the schema is gone, and
// the customer-facing /dashboards page still gets the id+type it draws its count and preview from.
// Cutting the array outright would have shown every dashboard as empty — a leak fixed by breaking
// the feature is not a fix.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogTiles_KeepsOnlyIDAndType(t *testing.T) {
	raw := json.RawMessage(`[
		{"id":"w1","title":"Doanh thu theo tháng","type":"bar","entity":"crm_opportunities",
		 "metric":"sum","field":"amount","groupBy":"stage",
		 "filters":[{"field":"status","op":"eq","value":"won"}]},
		{"id":"w2","title":"Số hợp đồng","type":"kpi","entity":"crm_contracts","metric":"count"}
	]`)

	tiles := catalogTiles(raw)
	if len(tiles) != 2 {
		t.Fatalf("got %d tiles; want 2 — the page counts these", len(tiles))
	}
	if tiles[0].ID != "w1" || tiles[0].Type != "bar" || tiles[1].Type != "kpi" {
		t.Errorf("id/type lost: %+v", tiles)
	}

	// The actual security assertion: nothing that names the data survives serialization.
	out, err := json.Marshal(tiles)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{
		"crm_opportunities", "crm_contracts", // entity names
		"amount", "stage", "status", "won", // field / groupBy / filter
		"metric", "sum", "count", // what is measured
		"Doanh thu", // even the author's title is not needed to count or preview by type
	} {
		if strings.Contains(string(out), leaked) {
			t.Errorf("catalog still leaks %q: %s", leaked, out)
		}
	}
}

func TestCatalogTiles_BadBlobDoesNotBreakDiscovery(t *testing.T) {
	// One dashboard with odd config must not fail the whole list — discovery is not the place to
	// surface a config error.
	if got := catalogTiles(json.RawMessage(`{"not":"an array"}`)); len(got) != 0 {
		t.Errorf("malformed widgets yielded %v; want an empty list", got)
	}
	if got := catalogTiles(nil); got == nil {
		t.Error("nil widgets yielded a nil slice; want an empty one so the JSON is [] not null")
	}
}
