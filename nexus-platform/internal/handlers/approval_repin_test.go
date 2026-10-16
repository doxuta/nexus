package handlers

// specs/043 S11 at the HTTP seam: the impact / instances / repin routes are mounted on the admin
// group, repin is a DRY RUN unless the body says "dryRun": false, and a save answers with `impact`
// beside `process` without moving the key the client already reads.

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/bpm"
)

func TestApprovalRepin_DryRunUnlessExplicitlyFalse(t *testing.T) {
	cases := []struct {
		name, body string
		wantDry    bool
		wantIDs    int
		wantTo     int
	}{
		{"empty body is a dry run", ``, true, 0, 0},
		{"omitted dryRun is a dry run", `{"instanceIds":["i1","i2"],"toVersion":3}`, true, 2, 3},
		{"explicit false applies", `{"dryRun":false}`, false, 0, 0},
		{"explicit true", `{"dryRun":true}`, true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memProcessRepo{}
			w := sendJSON(newApprovalAdminRouter(repo), http.MethodPost, "/admin/approval-processes/p-1/repin", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("repin = %d %s", w.Code, w.Body.String())
			}
			got := repo.lastRepin
			if got.DryRun != tc.wantDry || len(got.InstanceIDs) != tc.wantIDs || got.ToVersion != tc.wantTo || got.By != "admin-1" {
				t.Fatalf("repo saw %+v; want dry=%v ids=%d to=%d by admin-1", got, tc.wantDry, tc.wantIDs, tc.wantTo)
			}
		})
	}
}

func TestApprovalImpact_AndInstancesRoutes(t *testing.T) {
	repo := &memProcessRepo{impact: &bpm.Impact{Version: 2, PendingTotal: 3, PendingOnOldDefinition: 2, ByNode: []bpm.NodeCount{{NodeID: "s1", Name: "A", Count: 2}}}}
	r := newApprovalAdminRouter(repo)
	w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/impact", "")
	var env struct {
		Data struct {
			PendingOnOldDefinition int `json:"pendingOnOldDefinition"`
			PendingTotal           int `json:"pendingTotal"`
			ByNode                 []struct {
				NodeID string `json:"nodeId"`
				Count  int    `json:"count"`
			} `json:"byNode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || w.Code != http.StatusOK {
		t.Fatalf("impact = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if env.Data.PendingOnOldDefinition != 2 || env.Data.PendingTotal != 3 || len(env.Data.ByNode) != 1 || env.Data.ByNode[0].NodeID != "s1" {
		t.Fatalf("impact body = %s", w.Body.String())
	}
	if w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/instances?version=1&status=pending", ""); w.Code != http.StatusOK {
		t.Fatalf("instances = %d %s", w.Code, w.Body.String())
	}
	for _, q := range []string{"?version=abc", "?version=-1"} {
		if w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/instances"+q, ""); w.Code != http.StatusBadRequest {
			t.Errorf("instances%s = %d; want 400", q, w.Code)
		}
	}
	if w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/instances?status=bogus", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=bogus = %d; want 422", w.Code)
	}
}

func TestApprovalUpdate_ReturnsImpactBesideProcess(t *testing.T) {
	repo := &memProcessRepo{impact: &bpm.Impact{PendingOnOldDefinition: 4}}
	r := newApprovalAdminRouter(repo)
	const body = `{"name":"Duyệt","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"user","approverId":"u-1"}]}`
	if w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	w := sendJSON(r, http.MethodPut, "/admin/approval-processes/p-1", body)
	var env struct {
		Data struct {
			Process *struct {
				ID string `json:"id"`
			} `json:"process"`
			Impact *struct {
				Old int `json:"pendingOnOldDefinition"`
			} `json:"impact"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || w.Code != http.StatusOK {
		t.Fatalf("update = %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if env.Data.Process == nil || env.Data.Process.ID != "p-1" || env.Data.Impact == nil || env.Data.Impact.Old != 4 {
		t.Fatalf("update body = %s; want process + impact{pendingOnOldDefinition:4}", w.Body.String())
	}
}
