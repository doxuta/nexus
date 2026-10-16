package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/bpm"
)

// specs/043 S6b (T115): the quorum percent is judged by the engine's own body (bpm.CheckApprovers),
// located at the exact key, answered with its own code; a valid one is stored in the graph and the
// default `all` is still written down.

func quorumStep(percent int, approvers ...ApprovalApproverInput) ApprovalStepInput {
	return ApprovalStepInput{Name: "Ban duyệt", Approvers: approvers, Completion: &ApprovalCompletionInput{Mode: "quorum", Percent: percent}}
}

func TestCreateProcess_QuorumValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		step      ApprovalStepInput
		wantField string
	}{
		{"no percent", quorumStep(0, userA("u1"), userA("u2")), "steps[0].completion.percent"},
		{"over 100", quorumStep(101, userA("u1"), userA("u2")), "steps[0].completion.percent"},
		{"negative", quorumStep(-3, userA("u1"), userA("u2")), "steps[0].completion.percent"},
		{"percent beside all", ApprovalStepInput{Approvers: []ApprovalApproverInput{userA("u1"), userA("u2")}, Completion: &ApprovalCompletionInput{Mode: "all", Percent: 50}}, "steps[0].completion.percent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &graphProcRepo{}
			_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
				Name: "P", EntityName: "crm_quotes", Trigger: "manual", Steps: []ApprovalStepInput{tc.step},
			})
			ve := wantApprovalCode(t, err, "APPROVAL_QUORUM_INVALID")
			if ve.Field != tc.wantField {
				t.Fatalf("field %q; want %q (%s)", ve.Field, tc.wantField, ve.Msg)
			}
			if repo.created != nil {
				t.Fatal("an invalid quorum reached the repository")
			}
		})
	}
}

func TestCreateProcess_QuorumIsStoredInTheGraph(t *testing.T) {
	repo := &graphProcRepo{}
	if _, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual",
		Steps: []ApprovalStepInput{quorumStep(60, userA("u1"), userA("u2"), userA("u3"))},
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.created.Graph, &g); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	chain := bpm.Process{Graph: &g}.Chain()
	if len(chain) != 1 || chain[0].CompletionMode() != bpm.CompletionQuorum || chain[0].CompletionPolicy().Percent != 60 {
		t.Fatalf("chain = %+v; want one runnable quorum-60 step", chain)
	}
}
