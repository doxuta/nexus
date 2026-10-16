package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
)

// DAILY-QUEUE #443: every approval mutation answers data.approval in camelCase — never the
// PascalCase `ID`/`TenantID` of a bare bpm.Instance.
func TestApprovalMutation_ResponseIsCamelCase(t *testing.T) {
	due := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	spy := &spyDecider{ret: &bpm.Instance{
		ID: "i1", TenantID: "t-1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "r1",
		CurrentStep: 1, Status: bpm.StatusPending, CurrentApproverType: "user", CurrentApproverID: "u9",
		SLADueAt: &due, CreatedAt: due, UpdatedAt: due,
	}}
	w := postDecision(newApprovalDecideRouter(spy), "/approvals/i1/approve", map[string]any{"note": "ok"})
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Approval map[string]any `json:"approval"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := env.Data.Approval
	for _, k := range []string{"id", "tenantId", "processId", "entityName", "recordId", "currentStep", "status",
		"currentApproverType", "currentApproverId", "slaDueAt", "escalated", "history", "createdAt", "updatedAt"} {
		if _, ok := a[k]; !ok {
			t.Errorf("data.approval lacks camelCase key %q: %s", k, w.Body.String())
		}
	}
	for _, k := range []string{"ID", "TenantID", "EntityName", "CurrentStep", "Status", "History", "SLADueAt"} {
		if _, ok := a[k]; ok {
			t.Errorf("data.approval still carries PascalCase key %q", k)
		}
	}
	if a["id"] != "i1" || a["status"] != "pending" {
		t.Errorf("id/status = %v/%v; want i1/pending", a["id"], a["status"])
	}
}

func TestToApprovalInstanceView_NilAndEmptyHistory(t *testing.T) {
	if toApprovalInstanceView(nil) != nil {
		t.Fatal("nil instance must stay nil")
	}
	raw, err := json.Marshal(toApprovalInstanceView(&bpm.Instance{ID: "x"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if h, ok := m["history"].([]any); !ok || len(h) != 0 {
		t.Fatalf("history = %v; want [] (never null)", m["history"])
	}
}
