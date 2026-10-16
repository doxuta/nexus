package workflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// #338: the run row is written BEFORE the actions, so a queued email that gives up at once can
// always find it. startRecorder logs the order of the writes; probeSender looks at the recorder from
// INSIDE an action — the moment a delivery job could be claimed.

type startRecorder struct {
	mu     sync.Mutex
	events []string // "start:<id>" / "finish:<id>:<status>" / "insert:<id>:<status>"
	runs   map[string]RunInput
}

func (r *startRecorder) log(ev string, in RunInput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[string]RunInput{}
	}
	r.runs[in.ID] = in
	r.events = append(r.events, ev)
}

func (r *startRecorder) InsertRun(_ context.Context, in RunInput) error {
	r.log("insert:"+in.ID+":"+string(in.Status), in)
	return nil
}
func (r *startRecorder) StartRun(_ context.Context, in RunInput) error {
	in.Status = RunRunning
	r.log("start:"+in.ID, in)
	return nil
}
func (r *startRecorder) FinishRun(_ context.Context, in RunInput) error {
	r.log("finish:"+in.ID+":"+string(in.Status), in)
	return nil
}
func (r *startRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// probeSender records, per send, whether the run row had already been started when it ran.
type probeSender struct {
	rec     *startRecorder
	started []bool
}

func (s *probeSender) SendEmail(ctx context.Context, _, _, _ string) error {
	ref, _ := RunRefFrom(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	in, ok := s.rec.runs[ref.RunID]
	s.started = append(s.started, ok && in.Status == RunRunning)
	return nil
}

func TestDispatcher_WritesTheRunRowBeforeTheActions(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerAfterCreate, IsActive: true,
		Actions: []Action{
			{Type: ActionEmailAlert, To: "le.tan@nhakhoa.vn", Subject: "s", Body: "b"},
		},
	}}}
	rec := &startRecorder{}
	sender := &probeSender{rec: rec}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, &fakeMutator{}, nil, nil).
		WithEmailSender(sender).WithRunRecorder(rec)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "lich_hen", RecordID: "a-1", Trigger: TriggerAfterCreate, Data: map[string]any{}})
	drainDispatcher(t, d)

	ev := rec.snapshot()
	if len(ev) != 2 || len(sender.started) != 1 || !sender.started[0] {
		t.Fatalf("events %v, row started before send = %v; want [start finish] and the row already 'running' when the email action ran", ev, sender.started)
	}
	id := rec.runs[rec.snapshotID(t)].ID
	if ev[0] != "start:"+id || ev[1] != "finish:"+id+":success" {
		t.Fatalf("events %v; want start then finish of the SAME run id %s", ev, id)
	}
	var slots []map[string]any
	if err := json.Unmarshal(rec.runs[id].ActionsResult, &slots); err != nil || len(slots) != 1 {
		t.Fatalf("finished actions_result = %s (%v); want the real per-action results", rec.runs[id].ActionsResult, err)
	}
}

func (r *startRecorder) snapshotID(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.runs {
		return id
	}
	t.Fatal("no run recorded")
	return ""
}

func TestDispatcher_SkippedRunIsOneInsert(t *testing.T) {
	wf := matchingRule()
	wf.Condition = &Condition{Rules: []Rule{{Field: "stage", Op: "eq", Value: "won"}}}
	loader := &fakeLoader{workflows: []Workflow{wf}}
	rec := &startRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, &fakeMutator{}, nil, nil).WithRunRecorder(rec)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "d-1", Trigger: TriggerAfterUpdate, Data: map[string]any{"stage": "open"}})
	drainDispatcher(t, d)
	if ev := rec.snapshot(); len(ev) != 1 || ev[0][:7] != "insert:" {
		t.Fatalf("events %v; want ONE insert (a skipped rule has no actions to race)", ev)
	}
}

func TestScheduler_WritesTheScheduledRunRowBeforeTheScan(t *testing.T) {
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf-s", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionEmailAlert, To: "le.tan@nhakhoa.vn", Subject: "s", Body: "b"}},
	}})
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"lich_hen": {{ID: "a-1", Data: map[string]any{}}}}}
	rec := &startRecorder{}
	sender := &probeSender{rec: rec}
	s := newScheduler(loader, lister, actionDeps{email: sender})
	s.runs = rec

	s.runDue(context.Background(), time.Now())

	ev := rec.snapshot()
	if len(ev) != 2 || len(sender.started) != 1 || !sender.started[0] {
		t.Fatalf("events %v, started before send = %v; want [start finish] with the row 'running' during the scan", ev, sender.started)
	}
}

// queuingSender stands in for the enqueuing email sender: it parks the mail and says so.
type queuingSender struct{}

func (queuingSender) SendEmail(ctx context.Context, _, _, _ string) error {
	MarkQueued(ctx)
	return nil
}

// #339: an action that parked its work on the queue is recorded as delivery "queued"; an inline one is not.
func TestDispatcher_QueuedEmailSlotIsRecordedAsQueued(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerAfterCreate, IsActive: true,
		Actions: []Action{
			{Type: ActionEmailAlert, To: "le.tan@nhakhoa.vn", Subject: "s", Body: "b"},
			{Type: ActionInAppNotification, UserID: "u1", Message: "m"},
		},
	}}}
	rec := &startRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, &fakeMutator{}, nil, nil).
		WithEmailSender(queuingSender{}).WithRunRecorder(rec)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "lich_hen", RecordID: "a-1", Trigger: TriggerAfterCreate, Data: map[string]any{}})
	drainDispatcher(t, d)

	var slots []ActionResult
	if err := json.Unmarshal(rec.runs[rec.snapshotID(t)].ActionsResult, &slots); err != nil || len(slots) != 2 {
		t.Fatalf("actions_result: %v (%d slots)", err, len(slots))
	}
	if slots[0].Delivery != DeliveryQueued || slots[1].Delivery != "" {
		t.Fatalf("deliveries = %q / %q; want the email queued and the in-app notification not", slots[0].Delivery, slots[1].Delivery)
	}
}
