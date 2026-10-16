package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/bpm"
)

// TestApprovalProcessAPI_TaskNodeKeepsRequiredFields (specs/043 S5): the admin API takes a graph
// with a TASK node, its requiredFields and a work-queue assignee, and stores them. The first cut of
// this slice passed every service-level test while the HTTP decoder dropped `requiredFields` on the
// floor (found by the non-admin acceptance run): a task that saved as 201 and could then be
// completed with nothing filled. This pins the decoder.
func TestApprovalProcessAPI_TaskNodeKeepsRequiredFields(t *testing.T) {
	repo := &graphMemRepo{}
	r := graphAdminRouter(repo)
	const body = `{"name":"Kiểm tra chứng từ","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[
		{"id":"kiem-tra","name":"Kiểm tra chứng từ","kind":"task","approverType":"queue","approverId":"q-1","requiredFields":["doc_date","owner"]},
		{"id":"duyet","kind":"approval","approverType":"role","approverId":"r-1"}]}}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s; want 201", w.Code, w.Body.String())
	}
	var stored bpm.Graph
	if err := json.Unmarshal(repo.graph, &stored); err != nil || len(stored.Nodes) != 2 {
		t.Fatalf("stored graph %s (%v); want two nodes", repo.graph, err)
	}
	n := stored.Nodes[0]
	if n.Kind != bpm.NodeTask || n.ApproverType != bpm.ApproverQueue || len(n.RequiredFields) != 2 || n.RequiredFields[0] != "doc_date" {
		t.Fatalf("stored task node = %+v; want a task on a queue that kept both required fields", n)
	}
	if len(stored.Nodes[1].RequiredFields) != 0 {
		t.Fatalf("the approval node grew required fields: %+v", stored.Nodes[1])
	}

	// requiredFields on an approval node is refused, not dropped.
	const onApproval = `{"name":"X","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[
		{"id":"a","kind":"approval","approverType":"role","approverId":"r","requiredFields":["doc_date"]}]}}`
	if w := sendJSON(r, http.MethodPost, "/admin/approval-processes", onApproval); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("requiredFields on an approval node = %d %s; want 422", w.Code, w.Body.String())
	}
	// A linear step still cannot carry the new keys.
	const linearReq = `{"name":"X","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"queue","approverId":"q","requiredFields":["a"]}]}`
	if w := sendJSON(r, http.MethodPost, "/admin/approval-processes", linearReq); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("linear step with requiredFields/queue = %d %s; want 422", w.Code, w.Body.String())
	}
}
