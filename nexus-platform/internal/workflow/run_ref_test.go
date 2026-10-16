package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

// specs/059 FR-006: a queued email is delivered AFTER its run was recorded, so a delivery that
// gives up can only reach the builder's run history if it knows WHICH row it belongs to. The run
// id is therefore minted BEFORE the actions run and travels to the sender through ctx.

// refCapturingSender records the RunRef each send saw.
type refCapturingSender struct {
	mu   sync.Mutex
	refs []RunRef
	oks  []bool
}

func (s *refCapturingSender) SendEmail(ctx context.Context, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := RunRefFrom(ctx)
	s.refs = append(s.refs, ref)
	s.oks = append(s.oks, ok)
	return nil
}

func TestDispatcher_EmailSenderKnowsTheRunRowItBelongsTo(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerAfterCreate, IsActive: true,
		Actions: []Action{
			{Type: ActionEmailAlert, To: "le.tan@nhakhoa.vn", Subject: "s", Body: "b"},
			{Type: ActionEmailAlert, To: "bac.si@nhakhoa.vn", Subject: "s", Body: "b"},
		},
	}}}
	sender := &refCapturingSender{}
	rec := &fakeRunRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, &fakeMutator{}, nil, nil).
		WithEmailSender(sender).
		WithRunRecorder(rec)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "lich_hen", RecordID: "a-1", Trigger: TriggerAfterCreate, Data: map[string]any{}})
	drainDispatcher(t, d)

	runs := rec.all()
	if len(runs) != 1 || runs[0].ID == "" {
		t.Fatalf("runs = %+v; want one run carrying the id minted before its actions", runs)
	}
	if len(sender.refs) != 2 {
		t.Fatalf("sends = %d; want 2", len(sender.refs))
	}
	for i, ref := range sender.refs {
		want := RunRef{TenantID: "t1", WorkflowID: "wf1", RunID: runs[0].ID, ActionIndex: i}
		if !sender.oks[i] || ref != want {
			t.Errorf("send %d saw %+v (present=%v); want %+v — the row its outcome must land on", i, ref, sender.oks[i], want)
		}
	}
}

func TestScheduler_EmailSenderKnowsTheScheduledRunRow(t *testing.T) {
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf-s", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionEmailAlert, To: "le.tan@nhakhoa.vn", Subject: "s", Body: "b"}},
	}})
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"lich_hen": {{ID: "a-1", Data: map[string]any{}}, {ID: "a-2", Data: map[string]any{}}}}}
	sender := &refCapturingSender{}
	rec := &fakeRunRecorder{}
	s := newScheduler(loader, lister, actionDeps{email: sender})
	s.runs = rec

	s.runDue(context.Background(), time.Now())

	runs := rec.all()
	if len(runs) != 1 || runs[0].ID == "" {
		t.Fatalf("runs = %+v; want ONE scheduled run carrying a pre-minted id", runs)
	}
	for i, ref := range sender.refs {
		want := RunRef{TenantID: "t1", WorkflowID: "wf-s", RunID: runs[0].ID, ActionIndex: 0}
		if !sender.oks[i] || ref != want {
			t.Errorf("send %d saw %+v; want %+v", i, ref, want)
		}
	}
}

// No run context (a direct call, a dry-run, legacy wiring) = no ref — never a zero-valued one that
// a consumer could mistake for a real row.
func TestRunRefFrom_AbsentWithoutARun(t *testing.T) {
	if _, ok := RunRefFrom(context.Background()); ok {
		t.Fatal("RunRefFrom(background) reported a run")
	}
}
