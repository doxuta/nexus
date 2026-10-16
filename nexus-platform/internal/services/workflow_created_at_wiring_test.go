package services

// Event.CreatedAt wiring (spec 046 §2.3): set_elapsed measures from the record's creation, and
// created_at lives on the row, not in Data — so EVERY site that builds a workflow.Event must copy it
// from the Record it already holds. One assert per emit site. RecordService.Update is pinned by the
// MySQL integration test (TestSetElapsed_ResolvedTicketCarriesBusinessMinutes); the scheduler's own
// Event is pinned in internal/workflow (TestScheduler_SetElapsed_MeasuresFromScheduledRecordCreatedAt).
// A dropped copy is silent everywhere else: the runner's tests hand it a filled Event, and a zero
// CreatedAt only surfaces as a FAILED run in production.

import (
	"context"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// wiringCreatedAt is a fixed, non-zero creation instant a copy can be checked against exactly.
var wiringCreatedAt = time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)

// captureFirer records the Event the timer handler fires with (implements timerFirer).
type captureFirer struct{ evts []workflow.Event }

func (f *captureFirer) FireWorkflow(_ context.Context, _ workflow.Workflow, evt workflow.Event) error {
	f.evts = append(f.evts, evt)
	return nil
}

// captureEngine records every Event RecordService emits (implements workflow.Engine).
type captureEngine struct {
	mu   sync.Mutex
	evts []workflow.Event
}

func (e *captureEngine) Emit(_ context.Context, evt workflow.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evts = append(e.evts, evt)
}

// (a) the scheduler's lister copies created_at off the store row into ScheduledRecord.
func TestWorkflowRecordLister_CarriesCreatedAt(t *testing.T) {
	store := &fakeRecordStore{listRecords: []dynrepo.Record{{ID: "r1", TenantID: "t1", CreatedAt: wiringCreatedAt}}}
	out, err := NewWorkflowRecordLister(store).ListRecordsForScheduler(context.Background(), "tk_tickets", "t1", 10)
	if err != nil {
		t.Fatalf("ListRecordsForScheduler: %v", err)
	}
	if len(out) != 1 || !out[0].CreatedAt.Equal(wiringCreatedAt) {
		t.Fatalf("got %+v; want one record with CreatedAt %v", out, wiringCreatedAt)
	}
}

// (c) the date_offset wake-up fires with the re-read row's created_at.
func TestWorkflowTimerJobHandler_EventCarriesCreatedAt(t *testing.T) {
	row := &dynrepo.Record{
		ID: "r1", TenantID: "t1", CreatedAt: wiringCreatedAt,
		Data: map[string]any{"close_date": "2026-08-01", "stage": "new"},
	}
	firer := &captureFirer{}
	h := NewWorkflowTimerJobHandler(
		&fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}},
		&fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": row}},
		firer, nil, nil)
	if err := h(context.Background(), timerJob(t, "2026-08-01")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(firer.evts) != 1 || !firer.evts[0].CreatedAt.Equal(wiringCreatedAt) {
		t.Fatalf("fired %d events (%+v); want one carrying CreatedAt %v", len(firer.evts), firer.evts, wiringCreatedAt)
	}
}

// (d) the dry-run rehearses set_elapsed from the record's created_at — without it "Chạy thử" claims
// the creation instant is unreadable while the live run would have measured fine (or vice versa).
func TestDryRun_SetElapsedFromRecordCreatedAt(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", CreatedAt: wiringCreatedAt, Data: map[string]any{"amount": nil}}
	svc, wfID, _ := w6DryRunSvc(t, nil, []WorkflowActionInput{{Type: "set_elapsed", Field: "amount"}}, rec)
	svc.WithScheduleResolver(fixedScheduleResolver{})

	got, err := svc.TestWorkflow(context.Background(), "t1", wfID, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	act := got.Trace[len(got.Trace)-1]
	if act.Kind != "action" || act.Detail["from"] != wiringCreatedAt.Format(time.RFC3339) {
		t.Fatalf("set_elapsed preview = %+v; want from = the record's created_at %s", act.Detail, wiringCreatedAt.Format(time.RFC3339))
	}
}

// (d2) a SIMULATED create — op create, the default for an after_create rule — rehearses the record
// the create would make, and emitCreateEvents stamps that record's creation instant as "now" (e).
// The dry-run took the sample record's created_at instead (or none, on a record-less create), so
// "Chạy thử" printed minutes the live run never writes, or told the admin the run would fail. The
// same Event.CreatedAt feeds set_deadline's anchor created_at, so that preview drifted with it.
func TestDryRun_SimulatedCreateMeasuresFromNow(t *testing.T) {
	ctx := context.Background()
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", CreatedAt: wiringCreatedAt, Data: map[string]any{"amount": nil}}
	catalog := &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "f1", EntityID: "ent-1", Name: "amount", FieldType: "number"},
			{ID: "f2", EntityID: "ent-1", Name: "due_at", FieldType: "datetime"},
		},
	}
	svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(catalog).
		WithRecordReader(&fakeRecordReader{rec: rec}).WithScheduleResolver(fixedScheduleResolver{})
	wf, err := svc.CreateWorkflow(ctx, "t1", CreateWorkflowInput{
		Name: "rule", EntityName: "deals", Trigger: "after_create", ActorID: "u1",
		Actions: []WorkflowActionInput{
			{Type: "set_elapsed", Field: "amount"},
			{Type: "set_deadline", Field: "due_at", Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: workflow.TimerUnitHours}, Anchor: workflow.DeadlineAnchorCreatedAt},
		},
	})
	if err != nil {
		t.Fatalf("seed CreateWorkflow: %v", err)
	}
	for _, tc := range []struct {
		name string
		in   WorkflowTestInput
	}{
		{"sample record", WorkflowTestInput{RecordID: "r1"}},
		{"no record", WorkflowTestInput{Op: "create"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now().UTC().Truncate(time.Second)
			got, err := svc.DryRun(ctx, "t1", wf.ID, tc.in)
			after := time.Now().UTC()
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			elapsed, deadline := got.Trace[len(got.Trace)-2].Detail, got.Trace[len(got.Trace)-1].Detail
			if elapsed["wouldApply"] != true || elapsed["after"] != float64(0) {
				t.Fatalf("set_elapsed preview = %+v; want wouldApply and after = 0 — a record created now has taken no time", elapsed)
			}
			from, perr := time.Parse(time.RFC3339, fmtString(elapsed["from"]))
			if perr != nil || from.Before(before) || from.After(after) {
				t.Fatalf("set_elapsed from = %v; want the simulated creation instant within [%v, %v]", elapsed["from"], before, after)
			}
			at, perr := time.Parse(time.RFC3339, fmtString(deadline["after"]))
			if perr != nil || at.Before(before.Add(4*time.Hour)) || at.After(after.Add(4*time.Hour)) {
				t.Fatalf("set_deadline preview = %+v; want created_at (now) + 4h within [%v, %v]", deadline, before.Add(4*time.Hour), after.Add(4*time.Hour))
			}
		})
	}
}

// fmtString reads a trace detail value that must be a string ("" otherwise, which fails the parse).
func fmtString(v any) string {
	s, _ := v.(string)
	return s
}

// (e) create (and CreateBulk, which reuses emitCreateEvents) emits after_create with a creation
// instant — "now", the same second the DB stamps CURRENT_TIMESTAMP.
func TestRecordCreate_AfterCreateEventCarriesCreatedAt(t *testing.T) {
	catalog := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-t", TenantID: "t1", Name: "tk_tickets", DataTable: "obj_tk_tickets"},
		fields: []metadata.FieldDefinition{{Name: "subject", FieldType: "text"}},
	}
	eng := &captureEngine{}
	svc := NewRecordService(catalog, &fakeRecordStore{}).WithEngine(eng)

	before := time.Now().UTC()
	if _, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "t1", "u1", "tk_tickets", map[string]any{"subject": "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now().UTC()

	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.evts) != 1 || eng.evts[0].Trigger != workflow.TriggerAfterCreate {
		t.Fatalf("emitted %+v; want one after_create event", eng.evts)
	}
	if c := eng.evts[0].CreatedAt; c.Before(before) || c.After(after) {
		t.Fatalf("after_create CreatedAt = %v; want within [%v, %v]", c, before, after)
	}
}
