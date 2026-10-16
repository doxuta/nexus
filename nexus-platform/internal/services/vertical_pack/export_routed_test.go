package vertical_pack

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
)

// A routed graph (specs/043 S7: a gateway, an auto node, an end node) has no flat `steps` form: it is
// left out by name instead of being exported as a chain that would silently drop its branches.
func TestExport_RoutedGraphProcessSkippedWithItsOwnWarning(t *testing.T) {
	graph := mustJSON(t, bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "route"}, Kind: bpm.NodeGateway, Default: "end",
			Branches: []bpm.Branch{{Name: "x", To: "end"}}},
		{Step: bpm.Step{ID: "end"}, Kind: bpm.NodeEnd, Result: bpm.ResultApproved},
	}})
	svc, _ := graphExportService(t, graph)
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 0 {
		t.Fatalf("a routed graph was exported as %+v", pack.Approvals)
	}
	for _, w := range warnings {
		if strings.Contains(w, "Duyệt đồ thị") && strings.Contains(w, "rẽ nhánh") {
			return
		}
	}
	t.Fatalf("warnings %v; want one naming the routed process", warnings)
}
