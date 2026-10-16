package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAIProposeApproval_SLAClock (specs/043 S1): the model can say a step's deadline is wall
// clock, the draft keeps it for Apply, and a mistyped clock is refused by the SAME validator the
// admin API runs — never dropped (the draft would then silently time the step in working hours).
func TestAIProposeApproval_SLAClock(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{"name": "Duyệt chi", "entity": "tickets", "trigger": "manual", "steps": [
		{"approverType": "role", "approverRef": "manager", "slaSeconds": 14400, "slaClock": "wall"},
		{"approverType": "role", "approverRef": "manager", "slaSeconds": 14400}]}`)
	ap := e.draft[0].Approval
	if len(ap.Steps) != 2 || ap.Steps[0].SLAClock != "wall" || ap.Steps[1].SLAClock != "" {
		t.Fatalf("draft clocks = %+v, want [wall \"\"]", ap.Steps)
	}
	b, err := json.Marshal(e.draft[0])
	if err != nil || !strings.Contains(string(b), `"slaClock":"wall"`) {
		t.Fatalf("draft JSON must carry slaClock for Apply: %s (err %v)", b, err)
	}
	mustReject(t, &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}, "propose_approval",
		`{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [{"approverType": "role", "approverRef": "m", "slaClock": "gio"}]}`,
		"APPROVAL_STEP_INVALID")
}
