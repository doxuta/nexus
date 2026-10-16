package handlers

// specs/043 S6b (T115) at the HTTP seam: `completion {mode:"quorum", percent}` is stored on a
// council step and a graph node; a percent the engine cannot run is 422 APPROVAL_QUORUM_INVALID at
// the exact key; a percent beside any other mode is refused, not ignored (parity B6).

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/bpm"
)

func TestApprovalProcessAPI_QuorumIsStoredWithItsPercent(t *testing.T) {
	r := graphAdminRouter(&graphMemRepo{})
	const body = `{"name":"Ban duyệt","entityName":"crm_quotes","trigger":"manual","steps":[
		{"name":"Ban duyệt","approvers":[{"type":"user","id":"u-1"},{"type":"user","id":"u-2"},{"type":"user","id":"u-3"}],
		 "completion":{"mode":"quorum","percent":60}}]}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("quorum create = %d %s, want 201", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Process struct {
				Graph *bpm.Graph `json:"graph"`
			} `json:"process"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g := env.Data.Process.Graph
	if g == nil || len(g.Nodes) != 1 || g.Nodes[0].Completion == nil ||
		g.Nodes[0].Completion.Mode != bpm.CompletionQuorum || g.Nodes[0].Completion.Percent != 60 {
		t.Fatalf("stored graph = %+v; want one node with completion quorum 60", g)
	}
}

func TestApprovalProcessAPI_QuorumMistakes(t *testing.T) {
	two := `"approvers":[{"type":"user","id":"a"},{"type":"user","id":"b"}]`
	for name, tc := range map[string]struct{ body, field string }{
		"quorum without percent":        {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{` + two + `,"completion":{"mode":"quorum"}}]}`, "steps[0].completion.percent"},
		"quorum 0":                      {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{` + two + `,"completion":{"mode":"quorum","percent":0}}]}`, "steps[0].completion.percent"},
		"quorum 101":                    {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{` + two + `,"completion":{"mode":"quorum","percent":101}}]}`, "steps[0].completion.percent"},
		"quorum negative":               {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{` + two + `,"completion":{"mode":"quorum","percent":-1}}]}`, "steps[0].completion.percent"},
		"percent beside any":            {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{` + two + `,"completion":{"mode":"any","percent":60}}]}`, "steps[0].completion.percent"},
		"node form points at the graph": {`{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"kind":"approval",` + two + `,"completion":{"mode":"quorum","percent":150}}]}}`, "graph.nodes[0].completion.percent"},
	} {
		w := sendJSON(graphAdminRouter(&graphMemRepo{}), http.MethodPost, "/admin/approval-processes", tc.body)
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusUnprocessableEntity || e.Code != "APPROVAL_QUORUM_INVALID" {
			t.Errorf("%s = %d %s; want 422 APPROVAL_QUORUM_INVALID (the key %s is pinned in the services test)", name, w.Code, w.Body.String(), tc.field)
		}
	}
}
