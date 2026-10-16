package services

// specs/043 S9b: the start_approval action — config-time validation, the dry-run, and the adapter
// that turns the engine's "start" into the approval engine's manual send.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/workflow"
)

// fakeProcessReader answers GetProcess from a map; a missing id is bpm.ErrProcessNotFound.
type fakeProcessReader map[string]*bpm.ProcessRecord

func (f fakeProcessReader) GetProcess(_ context.Context, tenantID, id string) (*bpm.ProcessRecord, error) {
	if p, ok := f[id]; ok && p.TenantID == tenantID {
		return p, nil
	}
	return nil, bpm.ErrProcessNotFound
}

func saProc(id, entity, trigger string, active bool) *bpm.ProcessRecord {
	return &bpm.ProcessRecord{ID: id, TenantID: "t1", Name: id, EntityName: entity, Trigger: trigger, IsActive: active}
}

func saInput(trigger, processID string) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name: "start-" + trigger + processID, EntityName: "deals", Trigger: trigger,
		Actions: []WorkflowActionInput{{Type: "start_approval", ProcessID: processID}},
		ActorID: "u1",
	}
}

func TestStartApprovalAction_EntityMismatch422(t *testing.T) {
	procs := fakeProcessReader{
		"ok":       saProc("ok", "deals", "manual", true),
		"other":    saProc("other", "quotes", "manual", true),
		"auto":     saProc("auto", "deals", "after_create", true),
		"inactive": saProc("inactive", "deals", "manual", false),
	}
	tests := []struct {
		name     string
		trigger  string
		process  string
		wantCode string // "" = accepted
	}{
		{"manual process of the rule's entity", "after_update", "ok", ""},
		{"process of another entity", "after_update", "other", "WF_START_APPROVAL_INVALID"},
		{"unknown process", "after_update", "ghost", "WF_START_APPROVAL_INVALID"},
		{"process that starts itself (not manual)", "after_update", "auto", "WF_START_APPROVAL_INVALID"},
		{"process switched off", "after_update", "inactive", "WF_START_APPROVAL_INVALID"},
		{"empty process id", "after_update", "", "WF_START_APPROVAL_INVALID"},
		{"after_delete has no record left", "after_delete", "ok", "WF_START_APPROVAL_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithApprovalProcessReader(procs)
			_, err := svc.CreateWorkflow(context.Background(), "t1", saInput(tt.trigger, tt.process))
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("CreateWorkflow = %v, want accepted", err)
				}
				return
			}
			if codeOf(err) != tt.wantCode {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rejected rule must not be persisted")
			}
		})
	}
}

func TestStartApprovalAction_DryRunNoArm(t *testing.T) {
	act := workflow.Action{Type: workflow.ActionStartApproval, ProcessID: "ok"}
	detail, would := previewAction(context.Background(), "wf-1", act, workflow.Event{Data: map[string]any{"status": "x"}}, true, nil, nil, nil)
	if !would || detail["wouldStart"] != true || detail["processId"] != "ok" {
		t.Fatalf("detail = %v (would=%v), want a plan that names the process", detail, would)
	}
	detail, would = previewAction(context.Background(), "wf-1", act, workflow.Event{}, false, nil, nil, nil)
	if would || detail["wouldStart"] != false {
		t.Fatalf("condition not met must read as would-not-start: %v", detail)
	}
}

// recordingSender stands for bpm.Requester.
type recordingSender struct {
	got bpm.SubmitOptions
	who string
	err error
}

func (r *recordingSender) SubmitWith(_ context.Context, _, _, _ string, _ map[string]any, actor bpm.Actor, opt bpm.SubmitOptions) (*bpm.Instance, error) {
	r.got, r.who = opt, actor.UserID
	return &bpm.Instance{ID: "ai-1"}, r.err
}

func TestWorkflowApprovalStarter(t *testing.T) {
	t.Run("sends through the named process as the record's owner", func(t *testing.T) {
		rs := &recordingSender{}
		st := NewWorkflowApprovalStarter()
		st.Use(rs)
		started, err := st.StartApproval(context.Background(), "t1", "deals", "r1", "p-1", "owner-1", map[string]any{})
		if err != nil || !started {
			t.Fatalf("started=%v err=%v", started, err)
		}
		if rs.got.ProcessID != "p-1" || rs.who != "owner-1" {
			t.Errorf("engine saw %+v as %q", rs.got, rs.who)
		}
	})
	t.Run("already waiting is not an error", func(t *testing.T) {
		st := NewWorkflowApprovalStarter()
		st.Use(&recordingSender{err: bpm.ErrAlreadyPending})
		started, err := st.StartApproval(context.Background(), "t1", "deals", "r1", "p-1", "o", nil)
		if err != nil || started {
			t.Fatalf("started=%v err=%v, want false,nil", started, err)
		}
	})
	t.Run("a process that does not apply surfaces", func(t *testing.T) {
		st := NewWorkflowApprovalStarter()
		st.Use(&recordingSender{err: bpm.ErrProcessNotApplicable})
		_, err := st.StartApproval(context.Background(), "t1", "deals", "r1", "p-1", "o", nil)
		if !errors.Is(err, bpm.ErrProcessNotApplicable) {
			t.Fatalf("err = %v, want ErrProcessNotApplicable", err)
		}
	})
	t.Run("not bound yet is an error, never a silent skip", func(t *testing.T) {
		if _, err := NewWorkflowApprovalStarter().StartApproval(context.Background(), "t1", "deals", "r1", "p", "o", nil); err == nil {
			t.Fatal("an unbound starter must say so")
		}
	})
}
