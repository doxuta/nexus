package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAIProposeApproval_OnRejectParity (specs/043 S8 T140): propose_approval takes a step's
// onReject through the SAME validator POST /admin/approval-processes runs, buffers it with the
// default resume point written down, and refuses what the engine would not run.
func TestAIProposeApproval_OnRejectParity(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{"name": "Duyệt báo giá", "entity": "tickets", "trigger": "manual", "steps": [
		{"id": "truong-phong", "name": "Trưởng phòng", "approverType": "role", "approverRef": "manager"},
		{"id": "ke-toan", "name": "Kế toán", "approverType": "role", "approverRef": "ketoan", "onReject": {"to": "@submitter"}},
		{"name": "Giám đốc", "approverType": "role", "approverRef": "boss", "onReject": {"to": "truong-phong"}}]}`)
	ap := e.draft[0].Approval
	if ap.Steps[0].OnReject != nil {
		t.Fatalf("step 1 grew an onReject: %+v", ap.Steps[0].OnReject)
	}
	if o := ap.Steps[1].OnReject; o == nil || o.To != "@submitter" || o.ResumeAt != "rejector" {
		t.Fatalf("step 2 onReject = %+v; want @submitter with resumeAt rejector written down", o)
	}
	if o := ap.Steps[2].OnReject; o == nil || o.To != "truong-phong" || o.ResumeAt != "" {
		t.Fatalf("step 3 onReject = %+v; want a return to truong-phong", o)
	}
	b, err := json.Marshal(e.draft[0])
	if err != nil || !strings.Contains(string(b), `"onReject":{"to":"@submitter","resumeAt":"rejector"}`) {
		t.Fatalf("draft JSON must carry onReject for Apply: %s (err %v)", b, err)
	}

	base := `{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [{"id": "a", "approverType": "role", "approverRef": "m"},`
	for name, args := range map[string]string{
		"unknown target":      base + `{"approverType": "role", "approverRef": "m", "onReject": {"to": "zzz"}}]}`,
		"later step":          `{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [{"id": "a", "approverType": "role", "approverRef": "m", "onReject": {"to": "b"}}, {"id": "b", "approverType": "role", "approverRef": "m"}]}`,
		"itself":              `{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [{"id": "a", "approverType": "role", "approverRef": "m", "onReject": {"to": "a"}}]}`,
		"empty target":        base + `{"approverType": "role", "approverRef": "m", "onReject": {}}]}`,
		"bad resume":          base + `{"approverType": "role", "approverRef": "m", "onReject": {"to": "@submitter", "resumeAt": "middle"}}]}`,
		"resume on node jump": base + `{"approverType": "role", "approverRef": "m", "onReject": {"to": "a", "resumeAt": "start"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			mustReject(t, &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}, "propose_approval", args, "APPROVAL_STEP_INVALID")
		})
	}
}
