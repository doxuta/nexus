package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAIProposeApproval_ParallelParity (specs/043 S4 T083): propose_approval takes a many-approver
// step through the SAME validator POST /admin/approval-processes runs (services.ValidateApprovalChain),
// buffers it with its approvers + completion (default written down) and the step ids the apply path
// would store, and refuses the same mistakes with the same codes.
func TestAIProposeApproval_ParallelParity(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{"name": "Hội đồng duyệt", "entity": "tickets", "trigger": "manual", "steps": [
		{"name": "Trưởng phòng", "approverType": "role", "approverRef": "manager"},
		{"id": "hoi-dong", "name": "Hội đồng", "approvers": [{"type": "role", "ref": "manager"}, {"type": "user", "ref": "boss@example.com"}], "slaSeconds": 3600},
		{"approvers": [{"type": "role", "ref": "ketoan"}, {"type": "role", "ref": "phaply"}], "completion": "any"}]}`)
	ap := e.draft[0].Approval
	if len(ap.Steps) != 3 || ap.Steps[0].ID != "s1" || ap.Steps[1].ID != "hoi-dong" || ap.Steps[2].ID != "s3" {
		t.Fatalf("draft step ids = %+v, want [s1 hoi-dong s3] (the graph path's ids)", ap.Steps)
	}
	council, any := ap.Steps[1], ap.Steps[2]
	if len(council.Approvers) != 2 || council.Approvers[1].Ref != "boss@example.com" || council.Completion != "all" || council.ApproverType != "" {
		t.Fatalf("council step = %+v; want 2 approvers, completion all written down, no single approver", council)
	}
	if any.Completion != "any" {
		t.Fatalf("second council completion = %q; want any", any.Completion)
	}
	b, err := json.Marshal(e.draft[0])
	if err != nil || !strings.Contains(string(b), `"approvers":[{"type":"role","ref":"manager"}`) || !strings.Contains(string(b), `"completion":"any"`) {
		t.Fatalf("draft JSON must carry approvers + completion for Apply: %s (err %v)", b, err)
	}

	base := `{"name": "x", "entity": "tickets", "trigger": "manual", "steps": [`
	for name, args := range map[string]string{
		"one approver":            base + `{"approvers": [{"type": "role", "ref": "m"}]}]}`,
		"duplicate approver":      base + `{"approvers": [{"type": "role", "ref": "m"}, {"type": "role", "ref": "m"}]}]}`,
		"both approver sources":   base + `{"approverType": "role", "approverRef": "m", "approvers": [{"type": "role", "ref": "a"}, {"type": "role", "ref": "b"}]}]}`,
		"unknown mode (majority)": base + `{"approvers": [{"type": "role", "ref": "a"}, {"type": "role", "ref": "b"}], "completion": "majority"}]}`, // "quorum" was this row until S6b
		"completion alone":        base + `{"approverType": "role", "approverRef": "m", "completion": "all"}]}`,
		"unknown approver type":   base + `{"approvers": [{"type": "team", "ref": "a"}, {"type": "role", "ref": "b"}]}]}`,
		"blank approver ref":      base + `{"approvers": [{"type": "role", "ref": " "}, {"type": "role", "ref": "b"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			mustReject(t, &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}, "propose_approval", args, "APPROVAL_STEP_INVALID")
		})
	}
}
