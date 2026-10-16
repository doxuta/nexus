package services

// specs/043 S4 `bpm-parallel-approval` at the service seam: a step in the many-approver form is
// validated by the engine's own rule (bpm.CheckApprovers), lifted with its whole chain into the
// graph form (the linear column stays what a pre-S4 binary can read), and the explicit override
// asks for its reason before anything else.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
)

func parallelStep(name string, mode string, approvers ...ApprovalApproverInput) ApprovalStepInput {
	s := ApprovalStepInput{Name: name, Approvers: approvers}
	if mode != "" {
		s.Completion = &ApprovalCompletionInput{Mode: mode}
	}
	return s
}

func userA(id string) ApprovalApproverInput { return ApprovalApproverInput{Type: "user", ID: id} }

// A request whose `steps` carry approvers is stored as a graph beside steps '[]', ALL steps lifted
// in order (the plain ones too), the default completion written down, ids assigned.
func TestCreateProcess_ParallelStepsLiftTheChainToAGraph(t *testing.T) {
	repo := &graphProcRepo{}
	_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "Hội đồng", EntityName: "crm_quotes", Trigger: "manual",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "role", ApproverID: "r-mgr"},
			parallelStep("Hội đồng", "", userA("u1"), userA("u2"), userA("u3")),
			parallelStep("Ban kiểm soát", "any", userA("u4"), ApprovalApproverInput{Type: "role", ID: "r-aud"}),
		},
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	if string(repo.created.Steps) != "[]" || repo.created.Graph == nil {
		t.Fatalf("stored steps=%s graph=%s; want [] beside a graph", repo.created.Steps, repo.created.Graph)
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.created.Graph, &g); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	chain := bpm.Process{Graph: &g}.Chain()
	if len(chain) != 3 || chain[0].ID != "s1" || chain[1].ID != "s2" || chain[2].ID != "s3" {
		t.Fatalf("graph chain = %+v; want 3 runnable nodes s1..s3 (this binary runs it)", chain)
	}
	if chain[0].Parallel() || !chain[1].Parallel() || len(chain[1].Approvers) != 3 {
		t.Fatalf("chain shapes = %+v", chain)
	}
	if chain[1].CompletionMode() != bpm.CompletionAll || chain[1].Completion == nil {
		t.Fatalf("default completion not written down: %+v", chain[1].Completion)
	}
	if chain[2].CompletionMode() != bpm.CompletionAny || chain[2].Approvers[1].Type != bpm.ApproverRole {
		t.Fatalf("second council step = %+v", chain[2])
	}
}

// The same mistakes the engine refuses are refused at config time, located at steps[i].<key>.
func TestCreateProcess_ParallelStepValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		step      ApprovalStepInput
		wantField string
	}{
		{"one approver", parallelStep("x", "all", userA("u1")), "steps[0].approvers"},
		{"duplicate approver", parallelStep("x", "all", userA("u1"), userA("u1")), "steps[0].approvers"},
		{"unknown approver type", parallelStep("x", "", userA("u1"), ApprovalApproverInput{Type: "team", ID: "t"}), "steps[0].approvers"},
		{"blank approver id", parallelStep("x", "", userA("u1"), userA("  ")), "steps[0].approvers"},
		{"unknown mode", parallelStep("x", "majority", userA("u1"), userA("u2")), "steps[0].completion.mode"}, // "quorum" was this row until S6b
		{"approvers beside approverType", ApprovalStepInput{ApproverType: "user", ApproverID: "u9", Approvers: []ApprovalApproverInput{userA("u1"), userA("u2")}}, "steps[0].approverType"},
		{"completion without approvers", ApprovalStepInput{ApproverType: "user", ApproverID: "u9", Completion: &ApprovalCompletionInput{Mode: "all"}}, "steps[0].completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &graphProcRepo{}
			_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
				Name: "P", EntityName: "crm_quotes", Trigger: "manual", Steps: []ApprovalStepInput{tc.step},
			})
			ve := wantApprovalCode(t, err, "APPROVAL_STEP_INVALID")
			if ve.Field != tc.wantField {
				t.Fatalf("field %q; want %q (%s)", ve.Field, tc.wantField, ve.Msg)
			}
			if repo.created != nil {
				t.Fatalf("an invalid parallel step reached the repository")
			}
		})
	}
}

// A graph node carries the same keys; an error there points at graph.nodes[i].
func TestCreateProcess_ParallelGraphNode(t *testing.T) {
	repo := &graphProcRepo{}
	svc := NewApprovalService(repo, nil, nil)
	node := ApprovalNodeInput{Kind: "approval", ApprovalStepInput: parallelStep("Hội đồng", "all", userA("u1"), userA("u2"))}
	if _, err := svc.CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Graph: graphInput(node),
	}); err != nil {
		t.Fatalf("graph with a parallel node: %v", err)
	}
	node.Approvers = node.Approvers[:1]
	_, err := svc.CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Graph: graphInput(node),
	})
	if ve := wantApprovalCode(t, err, "APPROVAL_STEP_INVALID"); ve.Field != "graph.nodes[0].approvers" {
		t.Fatalf("field %q; want graph.nodes[0].approvers", ve.Field)
	}
}

// The linear validator used by the AI tool and the pack never stores a parallel step in `steps`.
func TestValidateApprovalProcess_NeverStoresParallelLinear(t *testing.T) {
	_, _, _, err := ValidateApprovalProcess("P", "manual", "", nil, []ApprovalStepInput{parallelStep("x", "all", userA("u1"), userA("u2"))})
	wantApprovalCode(t, err, "APPROVAL_NODE_KIND_UNSUPPORTED")
}

type overrideSpy struct {
	countingDecider
	gotNote     string
	gotExpected *int
	err         error
}

func (o *overrideSpy) Override(_ context.Context, _, id string, _ bpm.DecisionKind, note string, _ bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	o.gotNote, o.gotExpected = note, expectedStep
	return &bpm.Instance{ID: id}, o.err
}

// The override asks for its reason first (both directions), then maps the engine's refusals.
func TestOverrideApproval_NoteRequiredAndErrorsMapped(t *testing.T) {
	spy := &overrideSpy{}
	svc := NewApprovalService(nil, nil, spy)
	admin := bpm.Actor{UserID: "admin", IsAdmin: true}

	for _, note := range []string{"", "   "} {
		_, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", bpm.DecisionApprove, note, admin, nil)
		if ve := wantApprovalCode(t, err, "APPROVAL_OVERRIDE_NOTE_REQUIRED"); ve.Field != "note" {
			t.Fatalf("field %q; want note", ve.Field)
		}
	}
	if _, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", "maybe", "x", admin, nil); err == nil {
		t.Fatalf("an unknown decision was accepted")
	}
	if _, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", bpm.DecisionReject, "  vì hồ sơ giả  ", admin, nil); err != nil || spy.gotNote != "vì hồ sơ giả" {
		t.Fatalf("override: err=%v note=%q; want it passed trimmed", err, spy.gotNote)
	}
	for sentinel, code := range map[error]string{
		bpm.ErrNotAuthorized:         "APPROVAL_NOT_AUTHORIZED",
		bpm.ErrNotPending:            "APPROVAL_NOT_PENDING",
		bpm.ErrAlreadyDecidedByActor: "APPROVAL_ALREADY_DECIDED_BY_ACTOR",
		bpm.ErrAdminOverrideRequired: "APPROVAL_ADMIN_OVERRIDE_REQUIRED",
		bpm.ErrConcurrent:            "APPROVAL_CONFLICT",
	} {
		spy.err = sentinel
		_, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", bpm.DecisionApprove, "x", admin, nil)
		wantApprovalCode(t, err, code)
	}
	spy.err = errors.New("boom")
	if _, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", bpm.DecisionApprove, "x", admin, nil); err == nil {
		t.Fatalf("a real failure vanished")
	}
}

// The step the admin's screen saw reaches the engine; the engine's stale answer is the 409 code.
func TestOverrideApproval_ExpectedStepPassedDown(t *testing.T) {
	spy := &overrideSpy{}
	svc := NewApprovalService(nil, nil, spy)
	step := 3
	if _, err := svc.OverrideApproval(context.Background(), "t1", "ai-1", bpm.DecisionApprove, "x", bpm.Actor{UserID: "admin", IsAdmin: true}, &step); err != nil {
		t.Fatalf("override: %v", err)
	}
	if spy.gotExpected == nil || *spy.gotExpected != 3 {
		t.Fatalf("engine got expectedStep %v; want 3", spy.gotExpected)
	}
}

// A council seat handed to someone already on the council is a coded refusal, not a 500.
func TestMapRequestErr_DelegateTargetSeated(t *testing.T) {
	wantApprovalCode(t, mapRequestErr(bpm.ErrDelegateTargetSeated), "APPROVAL_DELEGATE_TARGET_SEATED")
}
