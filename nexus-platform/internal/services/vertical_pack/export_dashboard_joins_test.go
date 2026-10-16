package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/dashboard"
)

// Queue #294/#295: the pack widget format has no joins, so a widget that carries them is dropped
// WITH a warning — never shipped without its joins (its "<rel>.<field>" token would die on every run).
func TestExportDashboards_DropsJoinedWidgetWithWarning(t *testing.T) {
	widgets := `[
	 {"id":"w1","title":"Plain","type":"kpi","entity":"tickets","metric":"count"},
	 {"id":"w2","title":"Theo phòng","type":"bar","entity":"tickets","metric":"count","groupBy":"dept.id",
	  "joins":[{"relationship":"assignee"},{"relationship":"dept"}]}]`
	dash := &fakeMintDash{recs: map[string][]dashboard.Record{"t1": {{ID: "d1", TenantID: "t1", Name: "Board", Widgets: json.RawMessage(widgets)}}}}
	s := &Service{dashboardWr: dash}
	pack := &Pack{}
	var warns []string
	warn := func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }
	if err := s.exportDashboards(context.Background(), "t1", map[string]bool{"tickets": true}, pack, warn, func(string) {}); err != nil {
		t.Fatalf("exportDashboards: %v", err)
	}
	if len(pack.Dashboards) != 1 || len(pack.Dashboards[0].Widgets) != 1 || pack.Dashboards[0].Widgets[0].ID != "w1" {
		t.Fatalf("dashboards = %+v, want only the un-joined widget w1", pack.Dashboards)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "widget ghép quan hệ") {
		t.Fatalf("warns = %v, want one joined-widget warning", warns)
	}
}
