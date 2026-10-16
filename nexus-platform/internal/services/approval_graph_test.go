package services

// specs/043 S3a2 (T054, T062, D-2, D-7, FR-004): a process body may carry its chain in GRAPH form
// ({nodes:[…]}). The service stores it in `graph` with steps '[]', validates every node with the
// same rules as a step, opens only the node kind the engine runs (approval), and refuses a
// linear-only save that would erase a stored graph (409 APPROVAL_PROCESS_V2_OVERWRITE) unless the
// caller forces it.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/bpm"
)

// graphProcRepo captures what the service persists and replays it on read.
type graphProcRepo struct {
	fakeApprovalProcessRepo // the S10 draft/publish/history half (specs/043) — unused here
	created                 *bpm.CreateProcessInput
	updated                 *bpm.UpdateProcessInput
	updateErr               error
}

func (r *graphProcRepo) CreateProcess(_ context.Context, in bpm.CreateProcessInput) (string, error) {
	r.created = &in
	return "p-1", nil
}
func (r *graphProcRepo) GetProcess(_ context.Context, tenantID, id string) (*bpm.ProcessRecord, error) {
	rec := &bpm.ProcessRecord{ID: id, TenantID: tenantID}
	if r.created != nil {
		rec.Steps, rec.Graph = r.created.Steps, r.created.Graph
	}
	return rec, nil
}
func (r *graphProcRepo) ListProcesses(context.Context, string) ([]bpm.ProcessRecord, error) {
	return nil, nil
}
func (r *graphProcRepo) UpdateProcess(_ context.Context, _, _ string, in bpm.UpdateProcessInput) error {
	r.updated = &in
	return r.updateErr
}
func (r *graphProcRepo) DeleteProcess(context.Context, string, string) error { return nil }

func approvalNode(id, kind, approverType, approverID string) ApprovalNodeInput {
	return ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{ID: id, ApproverType: approverType, ApproverID: approverID}, Kind: kind}
}

func graphInput(nodes ...ApprovalNodeInput) *ApprovalGraphInput {
	return &ApprovalGraphInput{Nodes: nodes}
}

func TestCreateProcess_GraphStoredBesideEmptySteps(t *testing.T) {
	repo := &graphProcRepo{}
	svc := NewApprovalService(repo, nil, nil)
	_, err := svc.CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "Duyệt đồ thị", EntityName: "crm_quotes", Trigger: "manual",
		Graph: graphInput(
			approvalNode("truong-phong", "approval", "role", "r-mgr"),
			approvalNode("", "approval", "user", "u-dir"),
		),
	})
	if err != nil {
		t.Fatalf("CreateProcess with a graph: %v", err)
	}
	if string(repo.created.Steps) != "[]" {
		t.Fatalf("steps stored %s; want [] beside the graph (D-2: an older binary skips it)", repo.created.Steps)
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.created.Graph, &g); err != nil {
		t.Fatalf("graph stored %s: %v", repo.created.Graph, err)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].ID != "truong-phong" || g.Nodes[1].ID != "s2" || g.Nodes[1].Kind != bpm.NodeApproval {
		t.Fatalf("graph stored %s; want two approval nodes, the missing id assigned s2", repo.created.Graph)
	}
}

func TestCreateProcess_GraphValidatedLikeSteps(t *testing.T) {
	cases := []struct {
		name      string
		steps     []ApprovalStepInput
		graph     *ApprovalGraphInput
		wantCode  string
		wantField string
	}{
		{"kind missing", nil, graphInput(approvalNode("a", "", "role", "r")), "APPROVAL_STEP_INVALID", "graph.nodes[0].kind"},
		// A later slice's node in the shape a caller really sends it: a gateway has no approver, and
		// the answer is "that kind is not open yet", never "add an approver to your gateway".
		{"kind a later slice runs", nil, graphInput(approvalNode("a", "approval", "role", "r"), approvalNode("b", "subprocess", "", "")),
			"APPROVAL_NODE_KIND_UNSUPPORTED", "graph.nodes[1].kind"},
		{"kind a later slice runs, before an approver error", nil, graphInput(approvalNode("a", "approval", "team", "t"), approvalNode("b", "subprocess", "", "")),
			"APPROVAL_NODE_KIND_UNSUPPORTED", "graph.nodes[1].kind"},
		{"node key a later slice runs", nil, &ApprovalGraphInput{Nodes: []ApprovalNodeInput{{
			ApprovalStepInput: ApprovalStepInput{ApproverType: "role", ApproverID: "r", Unsupported: []string{"approvers"}}, Kind: "approval"}}},
			"APPROVAL_NODE_KIND_UNSUPPORTED", "graph.nodes[0].approvers"},
		{"graph key a later slice runs", nil, &ApprovalGraphInput{Nodes: []ApprovalNodeInput{approvalNode("a", "approval", "role", "r")}, Unsupported: []string{"lanes"}},
			"APPROVAL_NODE_KIND_UNSUPPORTED", "graph.lanes"},
		{"duplicate node id", nil, graphInput(approvalNode("a", "approval", "role", "r"), approvalNode("a", "approval", "user", "u")),
			"APPROVAL_NODE_ID_DUPLICATE", "graph.nodes[1].id"},
		{"bad approver type", nil, graphInput(approvalNode("a", "approval", "team", "t")), "APPROVAL_STEP_INVALID", ""},
		{"no nodes", nil, graphInput(), "APPROVAL_STEPS_REQUIRED", ""},
		{"steps and graph together", []ApprovalStepInput{roleStep}, graphInput(approvalNode("a", "approval", "role", "r")),
			"APPROVAL_STEP_INVALID", "graph"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &graphProcRepo{}
			_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
				Name: "P", EntityName: "crm_quotes", Trigger: "manual", Steps: c.steps, Graph: c.graph,
			})
			ve := wantApprovalCode(t, err, c.wantCode)
			if c.wantField != "" && ve.Field != c.wantField {
				t.Fatalf("field %q; want %q (%s)", ve.Field, c.wantField, ve.Msg)
			}
			if repo.created != nil {
				t.Fatalf("an invalid graph reached the repository")
			}
		})
	}
}

// TestUpdateProcess_RefusesGraphOverwrite409 (T054, service seam): the repository's guard answer
// becomes APPROVAL_PROCESS_V2_OVERWRITE (409), and force/graph travel down to it.
func TestUpdateProcess_RefusesGraphOverwrite409(t *testing.T) {
	repo := &graphProcRepo{updateErr: bpm.ErrProcessGraphOverwrite}
	svc := NewApprovalService(repo, nil, nil)
	_, err := svc.UpdateProcess(context.Background(), "t1", "p-1", UpdateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Steps: []ApprovalStepInput{roleStep},
	})
	wantApprovalCode(t, err, "APPROVAL_PROCESS_V2_OVERWRITE")
	if repo.updated.Graph != nil || repo.updated.Force {
		t.Fatalf("linear save sent graph=%s force=%v; want neither", repo.updated.Graph, repo.updated.Force)
	}

	repo.updateErr = nil
	if _, err := svc.UpdateProcess(context.Background(), "t1", "p-1", UpdateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Steps: []ApprovalStepInput{roleStep}, Force: true,
	}); err != nil {
		t.Fatalf("forced linear save: %v", err)
	}
	if !repo.updated.Force || repo.updated.Graph != nil || string(repo.updated.Steps) == "[]" {
		t.Fatalf("forced save sent force=%v graph=%s steps=%s", repo.updated.Force, repo.updated.Graph, repo.updated.Steps)
	}

	if _, err := svc.UpdateProcess(context.Background(), "t1", "p-1", UpdateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Graph: graphInput(approvalNode("a", "approval", "role", "r")),
	}); err != nil {
		t.Fatalf("graph save: %v", err)
	}
	if repo.updated.Graph == nil || string(repo.updated.Steps) != "[]" {
		t.Fatalf("graph save sent graph=%s steps=%s; want the graph beside []", repo.updated.Graph, repo.updated.Steps)
	}
}
