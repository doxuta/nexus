package handlers

import (
	"encoding/json"
	"testing"
)

// Queue #294: a widget's joins survive the JSON → service mapping (a handler that swallowed them
// would save a "<rel>.<field>" card with no join and it would die on every run).
func TestToServiceWidgets_CarriesJoins(t *testing.T) {
	var req dashboardRequest
	body := `{"name":"b","widgets":[{"id":"w","entity":"tickets","metric":"count","groupBy":"dept.id",
	  "joins":[{"relationship":"assignee"},{"relationship":"dept","optional":true}]}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := toServiceWidgets(req.Widgets)[0].Joins
	if len(got) != 2 || got[0].Relationship != "assignee" || got[0].Optional || got[1].Relationship != "dept" || !got[1].Optional {
		t.Fatalf("joins = %+v, want assignee, dept(optional)", got)
	}
}
