package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/dashboard"
)

// ACL by construction: GET /dashboards must drop a dashboard whose every widget entity the caller
// cannot view (the rep saw "Tổng quan Dự án" built on pm_tasks and got FORBIDDEN on all widgets).
func TestDashboardService_ListVisibleDashboards(t *testing.T) {
	repo := newDraftFakeDashRepo()
	mk := func(name string, entities ...string) string {
		ws := make([]WidgetConfig, 0, len(entities))
		for _, e := range entities {
			w := kpiWidget("w-" + e)
			w.Entity = e
			ws = append(ws, w)
		}
		raw, _ := json.Marshal(ws)
		id, _ := repo.Create(context.Background(), dashboard.CreateInput{TenantID: "t1", Name: name, Widgets: raw})
		return id
	}
	hidden := mk("only-hidden", "tickets_hidden")
	mixed := mk("mixed", "tickets_hidden", "tickets")
	shown := mk("shown", "tickets")
	empty := mk("empty")
	otherTenant, _ := repo.Create(context.Background(), dashboard.CreateInput{TenantID: "t2", Name: "x"})

	authz := &rjAuthz{
		allow: map[string]bool{"tickets": true, "tickets_hidden": false},
		level: map[string]string{"tickets": "owner"},
	}
	svc := NewDashboardService(repo, buildRJService(&rjStore{}, authz, nil), nil)

	ids := func(rs []dashboard.Record) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.ID] = true
		}
		return m
	}

	rep, err := svc.ListVisibleDashboards(context.Background(), RecordScope{UserID: "u1", RoleID: "rep"}, "t1")
	if err != nil {
		t.Fatalf("rep: %v", err)
	}
	got := ids(rep)
	if got[hidden] || !got[mixed] || !got[shown] || !got[empty] || got[otherTenant] || len(got) != 3 {
		t.Fatalf("rep sees %v; want mixed+shown+empty only (hidden=%s)", got, hidden)
	}

	adm, err := svc.ListVisibleDashboards(context.Background(), RecordScope{UserID: "a", IsAdmin: true}, "t1")
	if err != nil || len(adm) != 4 {
		t.Fatalf("admin = %d (%v); want all 4 tenant dashboards", len(adm), err)
	}
}
