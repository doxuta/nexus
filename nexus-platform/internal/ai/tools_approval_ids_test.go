package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAIProposeApproval_SharesValidator_IDs (specs/043 S0 T007): propose_approval runs the SAME
// step-id rules and the SAME D-7 refusal as POST /admin/approval-processes — a model-supplied id is
// validated and kept, a missing one is filled exactly as the apply path persists it, and a key the
// engine does not run is refused instead of vanishing from the draft (which would let the model
// tell the user it configured something that is not there).
func TestAIProposeApproval_SharesValidator_IDs(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{"name": "Duyệt chi", "entity": "tickets", "trigger": "manual", "steps": [
		{"id": "truong-phong", "name": "Trưởng phòng", "approverType": "role", "approverRef": "manager"},
		{"approverType": "user", "approverRef": "boss@example.com"}]}`)
	ap := e.draft[0].Approval
	if len(ap.Steps) != 2 || ap.Steps[0].ID != "truong-phong" || ap.Steps[1].ID != "s2" {
		t.Fatalf("draft step ids = %+v, want [truong-phong s2] (explicit kept, missing filled like the apply path)", ap.Steps)
	}
	b, err := json.Marshal(e.draft[0])
	if err != nil || !strings.Contains(string(b), `"id":"truong-phong"`) {
		t.Fatalf("draft JSON must carry the step id for Apply: %s (err %v)", b, err)
	}

	base := `{"name": "x", "entity": "tickets", "trigger": "after_create", `
	cases := []struct{ name, args, code string }{
		{"duplicate id", base + `"steps": [{"id": "a", "approverType": "role", "approverRef": "m"}, {"id": "a", "approverType": "role", "approverRef": "n"}]}`, "APPROVAL_NODE_ID_DUPLICATE"},
		{"bad id", base + `"steps": [{"id": "Kế toán", "approverType": "role", "approverRef": "m"}]}`, "APPROVAL_NODE_ID_INVALID"},
		{"step key the engine does not run", base + `"steps": [{"approverType": "role", "approverRef": "m", "assignment": {"type": "field"}}]}`, "APPROVAL_NODE_KIND_UNSUPPORTED"}, // approvers opened in S4; assignment is S6a
		{"process key the engine does not run", base + `"graph": {"start": "a"}, "steps": [{"approverType": "role", "approverRef": "m"}]}`, "APPROVAL_NODE_KIND_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustReject(t, &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}, "propose_approval", tc.args, tc.code)
		})
	}
}
