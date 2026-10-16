package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAIProposeApproval_QuorumParity (specs/043 S6b T117): propose_approval takes a quorum council
// through the SAME validator POST /admin/approval-processes runs, buffers the percent for Apply, and
// refuses a quorum the engine cannot run with the same code (APPROVAL_QUORUM_INVALID).
func TestAIProposeApproval_QuorumParity(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{"name": "Ban duyệt", "entity": "tickets", "trigger": "manual", "steps": [
		{"id": "ban-duyet", "approvers": [{"type": "role", "ref": "manager"}, {"type": "role", "ref": "ketoan"}, {"type": "role", "ref": "phaply"}],
		 "completion": "quorum", "completionPercent": 60}]}`)
	st := e.draft[0].Approval.Steps[0]
	if st.Completion != "quorum" || st.CompletionPercent != 60 || len(st.Approvers) != 3 {
		t.Fatalf("drafted step = %+v; want quorum 60 over 3 approvers", st)
	}
	b, err := json.Marshal(e.draft[0])
	if err != nil || !strings.Contains(string(b), `"completion":"quorum"`) || !strings.Contains(string(b), `"completionPercent":60`) {
		t.Fatalf("draft JSON must carry the percent for Apply: %s (err %v)", b, err)
	}

	base := `{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [{"approvers": [{"type": "role", "ref": "a"}, {"type": "role", "ref": "b"}], `
	for name, args := range map[string]string{
		"quorum without a percent": base + `"completion": "quorum"}]}`,
		"quorum over 100":          base + `"completion": "quorum", "completionPercent": 150}]}`,
		"quorum 0":                 base + `"completion": "quorum", "completionPercent": 0}]}`,
		"a percent beside any":     base + `"completion": "any", "completionPercent": 60}]}`,
		"a percent without a mode": base + `"completionPercent": 60}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			code := "APPROVAL_QUORUM_INVALID"
			if name == "a percent without a mode" {
				code = "APPROVAL_STEP_INVALID" // no mode to carry it: judged as a missing completion, not a bad quorum
			}
			mustReject(t, &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}, "propose_approval", args, code)
		})
	}
}
