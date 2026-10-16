package handlers

import (
	"encoding/json"
	"testing"

	"nexus/platform/internal/services"
)

// TestToServiceConfig_ColumnGroup: the admin request body carries the matrix column axis through
// to the service (specs/049 S2b). The handler's config struct is the second place a new key is
// silently dropped (plan §2) — the response echoes the stored JSON, so a dropped key only shows
// as a report that never becomes a cross-tab.
func TestToServiceConfig_ColumnGroup(t *testing.T) {
	var req reportConfigRequest
	if err := json.Unmarshal([]byte(`{"groupBy":"stage","metric":"count","columnGroup":{"field":"close_date","bucket":"month"}}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := toServiceConfig(&req).ColumnGroup
	if got == nil || *got != (services.ReportGroupInput{Field: "close_date", Bucket: "month"}) {
		t.Fatalf("columnGroup = %+v, want close_date/month", got)
	}
	if toServiceConfig(&reportConfigRequest{GroupBy: "stage"}).ColumnGroup != nil {
		t.Fatalf("a body without columnGroup must map to no column axis")
	}
}

// TestToServiceConfig_SubGroups: the admin request body carries a summary's sub-levels through to
// the service, in order (specs/049 S3) — the handler struct would otherwise drop the key silently.
func TestToServiceConfig_SubGroups(t *testing.T) {
	var req reportConfigRequest
	if err := json.Unmarshal([]byte(`{"groupBy":"stage","metric":"count","subGroups":[{"field":"close_date","bucket":"month"},{"field":"ownerId"}]}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := toServiceConfig(&req).SubGroups
	want := []services.ReportGroupInput{{Field: "close_date", Bucket: "month"}, {Field: "ownerId"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("subGroups = %+v, want %+v", got, want)
	}
	if toServiceConfig(&reportConfigRequest{GroupBy: "stage"}).SubGroups != nil {
		t.Fatalf("a body without subGroups must map to no sub-level")
	}
}

// TestToServiceConfig_Having: the admin request body carries a report's threshold through to the
// service (specs/049 S5) — the handler struct would otherwise drop the key silently, and a report
// saved "with a threshold" would total every group.
func TestToServiceConfig_Having(t *testing.T) {
	var req reportConfigRequest
	if err := json.Unmarshal([]byte(`{"groupBy":"stage","metric":"count","having":{"on":"count","op":"between","values":["2","5"]}}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := toServiceConfig(&req).Having
	if got == nil || got.On != "count" || got.Op != "between" || len(got.Values) != 2 || got.Values[0] != "2" || got.Values[1] != "5" {
		t.Fatalf("having = %+v, want count between 2 and 5", got)
	}
	if toServiceConfig(&reportConfigRequest{GroupBy: "stage"}).Having != nil {
		t.Fatalf("a body without having must map to no threshold")
	}
}
