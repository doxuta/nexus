package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
)

// fakeScheduledLoader is an in-memory ScheduledLoader. claimed tracks which (tenant,id) have been
// claimed so a second claim returns false (mirroring the real CAS that prevents double-fire).
type fakeScheduledLoader struct {
	mu       sync.Mutex
	due      []ScheduledWorkflow
	listErr  error
	claimErr error
	claimed  map[string]int // key "tenant|id" → claim count
}

func newFakeScheduledLoader(due ...ScheduledWorkflow) *fakeScheduledLoader {
	return &fakeScheduledLoader{due: due, claimed: map[string]int{}}
}

func (l *fakeScheduledLoader) ListDueScheduled(_ context.Context, _ time.Time) ([]ScheduledWorkflow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listErr != nil {
		return nil, l.listErr
	}
	return l.due, nil
}

func (l *fakeScheduledLoader) ClaimScheduledRun(_ context.Context, tenantID, id string, _ time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimErr != nil {
		return false, l.claimErr
	}
	key := tenantID + "|" + id
	l.claimed[key]++
	// First claim wins; subsequent claims (same window) lose — the CAS contract.
	return l.claimed[key] == 1, nil
}

// fakeRecordLister returns a fixed record set per (entity).
type fakeRecordLister struct {
	mu      sync.Mutex
	records map[string][]ScheduledRecord
	err     error
	calls   int
}

func (r *fakeRecordLister) ListRecordsForScheduler(_ context.Context, entity, _ string, _ int) ([]ScheduledRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.records[entity], nil
}

func newScheduler(loader ScheduledLoader, lister RecordLister, deps actionDeps) *Scheduler {
	s := NewScheduler(SchedulerConfig{Interval: time.Minute}, loader, lister, nil, zap.NewNop())
	s.deps = deps
	return s
}

func TestScheduler_FiresMatchingRecordsOnce(t *testing.T) {
	mut := &fakeMutator{}
	loader := newFakeScheduledLoader(ScheduledWorkflow{
		Workflow: Workflow{
			ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
			Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "overdue"}}},
			Actions:   []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}},
		},
	})
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
		"deals": {
			{ID: "r1", Data: map[string]any{"status": "overdue"}}, // matches
			{ID: "r2", Data: map[string]any{"status": "ok"}},      // no match
			{ID: "r3", Data: map[string]any{"status": "overdue"}}, // matches
		},
	}}
	s := newScheduler(loader, lister, actionDeps{mut: mut})

	s.runDue(context.Background(), time.Now())

	// Two matching records → two field_update calls.
	if len(mut.calls) != 2 {
		t.Fatalf("expected 2 field_update calls (2 matching records), got %d", len(mut.calls))
	}
}

func TestScheduler_ClaimLostSkipsFire(t *testing.T) {
	mut := &fakeMutator{}
	sw := ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}},
	}}
	loader := newFakeScheduledLoader(sw)
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
		"deals": {{ID: "r1", Data: map[string]any{}}},
	}}
	s := newScheduler(loader, lister, actionDeps{mut: mut})

	// First pass claims + fires.
	s.runDue(context.Background(), time.Now())
	// Second pass: the same rule is still "due" in the fake, but the claim now LOSES (count==2),
	// so no fire happens — proving idempotency (no double-fire within a window).
	s.runDue(context.Background(), time.Now())

	if len(mut.calls) != 1 {
		t.Fatalf("expected exactly 1 fire across two passes (claim CAS), got %d", len(mut.calls))
	}
	if lister.calls != 1 {
		t.Errorf("record list should only run on the won claim, got %d calls", lister.calls)
	}
}

func TestScheduler_FailOpenOnListError(t *testing.T) {
	loader := newFakeScheduledLoader()
	loader.listErr = errors.New("db down")
	s := newScheduler(loader, &fakeRecordLister{}, actionDeps{})
	// Must not panic; just logs + returns.
	s.runDue(context.Background(), time.Now())
}

func TestScheduler_FailOpenOnRecordListError(t *testing.T) {
	mut := &fakeMutator{}
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}},
	}})
	lister := &fakeRecordLister{err: errors.New("list boom")}
	s := newScheduler(loader, lister, actionDeps{mut: mut})

	s.runDue(context.Background(), time.Now()) // must not panic
	if len(mut.calls) != 0 {
		t.Errorf("a list error must run no actions, got %d", len(mut.calls))
	}
}

func TestScheduler_FailOpenOnActionError(t *testing.T) {
	mut := &fakeMutator{err: errors.New("update failed")}
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}},
	}})
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
		"deals": {{ID: "r1", Data: map[string]any{}}},
	}}
	s := newScheduler(loader, lister, actionDeps{mut: mut})

	s.runDue(context.Background(), time.Now()) // action errors → swallowed, no panic
	if len(mut.calls) != 1 {
		t.Errorf("action should have been attempted once, got %d", len(mut.calls))
	}
}

// TestScheduler_OwnerSentinel_ResolvesFromScheduledRecordCreatedBy proves the scheduled-trigger
// plumbing populates Event.OwnerUserID from ScheduledRecord.CreatedBy (M7 MED-1 fix, review
// 2026-07-08) — the blind spot a plain runInAppNotification unit test cannot see: a scheduled
// rule's Event is built entirely inside fireScheduled (scheduler.go), a DIFFERENT construction
// site than the record-mutation paths. Without this wiring a scheduled "$owner" rule would stay
// silently dead exactly like the pre-fix bug, even after actions.go was fixed.
func TestScheduler_OwnerSentinel_ResolvesFromScheduledRecordCreatedBy(t *testing.T) {
	notif := &fakeNotif{}
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionInAppNotification, UserID: OwnerSentinel, Message: "Deal is due"}},
	}})
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
		"deals": {{ID: "r1", Data: map[string]any{}, OwnerID: "owner-x"}},
	}}
	s := newScheduler(loader, lister, actionDeps{notif: notif})

	s.runDue(context.Background(), time.Now())

	if len(notif.rows) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.rows))
	}
	if got := notif.rows[0].UserID; got != "owner-x" {
		t.Fatalf("UserID = %q; want the scheduled record's CreatedBy %q resolved (not the literal sentinel)", got, "owner-x")
	}
}

// TestScheduler_SetElapsed_MeasuresFromScheduledRecordCreatedAt pins the scheduled emit site's half
// of the Event.CreatedAt wiring (spec 046 §2.3). fireScheduled builds its Event itself, so a dropped
// `CreatedAt: rec.CreatedAt` leaves every scheduled set_elapsed FAILED ("creation instant is
// unknown") while every other test stays green — the runner's own tests hand it a filled Event.
// No calendar is wired (wall-clock fallback), so the stamp must be the minutes since the RECORD's
// creation, not since some other instant.
func TestScheduler_SetElapsed_MeasuresFromScheduledRecordCreatedAt(t *testing.T) {
	mut := &fakeMutator{}
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionSetElapsed, Field: "age_minutes"}},
	}})
	created := time.Now().UTC().Add(-90 * time.Minute)
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
		"tk_tickets": {{ID: "r1", Data: map[string]any{}, CreatedAt: created}},
	}}
	s := newScheduler(loader, lister, actionDeps{mut: mut, sched: &fakeScheduleResolver{}})

	before := time.Now().UTC()
	s.runDue(context.Background(), before)
	after := time.Now().UTC()

	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes, want 1 — the scheduled Event lost the record's CreatedAt", len(mut.calls))
	}
	got := writtenMinutes(t, mut.calls[0].value)
	if lo, hi := math.Floor(before.Sub(created).Minutes()), math.Floor(after.Sub(created).Minutes()); got < lo || got > hi {
		t.Fatalf("stamped %v minutes, want the minutes since the record's creation within [%v, %v]", got, lo, hi)
	}
}

// DAILY-QUEUE #166: under the `scheduled` trigger every matched record used to resolve the working
// calendar again — one sys_business_calendars read + parse per record per business-hours action,
// up to maxScan (5000) per tick. Measured on local MySQL 8 (2026-09-30): ≈0.5 ms per lookup, so
// ≈2.5 s of a 5000-record tick went to reading the same row. A calendar cannot change meaning
// between two records of ONE firing, so each (tenant, calendar) is resolved once per firing; the
// next firing reads again, so an edited calendar is picked up on the next tick.
func TestScheduler_ResolvesEachCalendarOncePerFiring(t *testing.T) {
	const records = 50
	var recs []ScheduledRecord
	for i := 0; i < records; i++ {
		recs = append(recs, ScheduledRecord{ID: "r" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
			Data: map[string]any{}, CreatedAt: time.Now().UTC().Add(-time.Hour)})
	}
	rule := func(id string) ScheduledWorkflow {
		return ScheduledWorkflow{Workflow: Workflow{
			ID: id, TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerScheduled, IsActive: true,
			Actions: []Action{
				{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}},
				{Type: ActionSetDeadline, Field: "due_24", CalendarID: "cal-24", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}},
				{Type: ActionSetElapsed, Field: "age_minutes"},
			},
		}}
	}

	t.Run("one lookup per calendar per firing, every record still written", func(t *testing.T) {
		mut := &fakeMutator{}
		res := &byIDResolver{byID: map[string]*calendar.Schedule{"": roundTheClock(t), "cal-24": roundTheClock(t)}}
		loader := newFakeScheduledLoader(rule("wf1"), rule("wf2"))
		s := newScheduler(loader, &fakeRecordLister{records: map[string][]ScheduledRecord{"tk_tickets": recs}},
			actionDeps{mut: mut, sched: res})

		s.runDue(context.Background(), time.Now())

		if len(mut.calls) != 2*records*3 {
			t.Fatalf("got %d writes, want %d (2 firings × %d records × 3 actions)", len(mut.calls), 2*records*3, records)
		}
		// Two firings × two calendars. Per record it would be 2 × 50 × 3 = 300.
		if len(res.ids) != 4 {
			t.Fatalf("calendar resolved %d times, want 4 (each firing reads the default and cal-24 once)", len(res.ids))
		}
	})

	t.Run("a store error is not remembered: the next record asks again", func(t *testing.T) {
		mut := &fakeMutator{}
		res := &failFirstResolver{sched: roundTheClock(t)}
		loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
			ID: "wf1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerScheduled, IsActive: true,
			Actions: []Action{{Type: ActionSetElapsed, Field: "age_minutes"}},
		}})
		s := newScheduler(loader, &fakeRecordLister{records: map[string][]ScheduledRecord{"tk_tickets": recs[:3]}},
			actionDeps{mut: mut, sched: res})

		s.runDue(context.Background(), time.Now())

		if res.calls != 2 || len(mut.calls) != 2 {
			t.Fatalf("resolver calls = %d, writes = %d; want 2 and 2 (the failed lookup is retried once, then cached)", res.calls, len(mut.calls))
		}
	})
}

// DAILY-QUEUE #105 (a): a scheduled firing writes ONE summary row, not per-action results, so the
// run-history warning #82 put on a wall-clock business_hours deadline was dropped on this path —
// only a log line said it. The summary now counts the warned actions and carries each distinct
// sentence verbatim; a firing with nothing to say keeps the old shape (no keys added).
func TestScheduler_SummaryCarriesWallClockWarnings(t *testing.T) {
	recs := []ScheduledRecord{
		{ID: "r1", Data: map[string]any{}, CreatedAt: time.Now().UTC().Add(-time.Hour)},
		{ID: "r2", Data: map[string]any{}, CreatedAt: time.Now().UTC().Add(-time.Hour)},
		{ID: "r3", Data: map[string]any{}, CreatedAt: time.Now().UTC().Add(-time.Hour)},
	}
	fire := func(t *testing.T, sched ScheduleResolver, actions ...Action) map[string]any {
		t.Helper()
		loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
			ID: "wf1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerScheduled, IsActive: true,
			Actions: actions,
		}})
		runs := &fakeRunRecorder{}
		s := newScheduler(loader, &fakeRecordLister{records: map[string][]ScheduledRecord{"tk_tickets": recs}},
			actionDeps{mut: &fakeMutator{}, sched: sched})
		s.runs = runs
		s.runDue(context.Background(), time.Now())
		all := runs.all()
		if len(all) != 1 || all[0].Status != RunSuccess {
			t.Fatalf("runs = %+v, want one success (a fallback deadline is still stamped)", all)
		}
		var summary map[string]any
		if err := json.Unmarshal(all[0].ActionsResult, &summary); err != nil {
			t.Fatalf("summary: %v", err)
		}
		return summary
	}
	deadline := Action{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}}
	pinned := Action{Type: ActionSetDeadline, Field: "due_24", CalendarID: "cal-gone", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}}

	t.Run("no usable calendar: count and both sentences reach the row", func(t *testing.T) {
		summary := fire(t, &fakeScheduleResolver{}, deadline, pinned)
		if summary["warned"] != float64(6) {
			t.Fatalf("summary = %v; want warned 6 (3 records × 2 wall-clock deadlines)", summary)
		}
		got, _ := summary["warnings"].([]any)
		if len(got) != 2 || got[0] != wallClockFallbackWarning || got[1] != pinnedCalendarFallbackWarning {
			t.Fatalf("warnings = %v; want the two distinct sentences, verbatim, in first-seen order", summary["warnings"])
		}
	})

	t.Run("calendar present: the summary keeps its old shape", func(t *testing.T) {
		summary := fire(t, &byIDResolver{byID: map[string]*calendar.Schedule{"": roundTheClock(t), "cal-gone": roundTheClock(t)}}, deadline, pinned)
		if _, ok := summary["warned"]; ok {
			t.Fatalf("summary = %v; no warning ⇒ no warned key", summary)
		}
		if _, ok := summary["warnings"]; ok {
			t.Fatalf("summary = %v; no warning ⇒ no warnings key", summary)
		}
	})
}

// failFirstResolver fails its first lookup (a transient store fault) and answers sched afterwards.
type failFirstResolver struct {
	sched *calendar.Schedule
	calls int
}

func (f *failFirstResolver) ResolveSchedule(context.Context, string, string) (*calendar.Schedule, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("dial tcp: connection refused")
	}
	return f.sched, nil
}

func TestScheduler_NilLoaderTickIsNoOp(t *testing.T) {
	s := NewScheduler(SchedulerConfig{Interval: time.Minute}, nil, nil, nil, zap.NewNop())
	s.tickOnce() // must not panic
}

func TestScheduler_StartStopGraceful(t *testing.T) {
	loader := newFakeScheduledLoader()
	// A tiny interval so the ticker fires during the test, but tickOnce is a quick no-op (no due).
	s := NewScheduler(SchedulerConfig{Interval: time.Minute}, loader, &fakeRecordLister{}, nil, zap.NewNop())
	s.Start()
	s.Start() // idempotent

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop should drain cleanly: %v", err)
	}
	if err := s.Stop(ctx); err != nil { // idempotent
		t.Fatalf("second Stop should be a no-op: %v", err)
	}
}

// TestScheduler_HoldsWebhooksWhileEarlierDeliveriesArePending (review of #26): a scheduled rule
// used to POST its webhooks inline, so it could not fire again before its deliveries finished.
// Parked on the queue they are enqueued at once — up to maxScan jobs a firing, drained or not — so
// sys_jobs grew faster than the lane drained, with no bound, even against a healthy receiver. While
// deliveries the rule parked earlier are pending, a firing's own deliveries are HELD (counted in the
// run summary, not parked) and its other actions still run — a receiver outage must not stop the
// rule's reminders and field updates.
func TestScheduler_HoldsWebhooksWhileEarlierDeliveriesArePending(t *testing.T) {
	webhookRule := []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}, {Type: ActionWebhook, URL: "https://erp.test/in"}}
	cases := []struct {
		name       string
		actions    []Action
		pending    bool
		probeErr   error
		wantProbes int
		wantParked int
		wantHeld   int
	}{
		{"pending deliveries hold this firing's deliveries", webhookRule, true, nil, 1, 0, 2},
		{"a drained queue parks them", webhookRule, false, nil, 1, 2, 0},
		{"a probe error fails open (parks, as before the probe)", webhookRule, false, errors.New("db down"), 1, 2, 0},
		{"a rule without a webhook is never probed", webhookRule[:1], true, nil, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
				ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
				Actions: tc.actions,
			}})
			lister := &fakeRecordLister{records: map[string][]ScheduledRecord{
				"deals": {{ID: "r1", Data: map[string]any{}}, {ID: "r2", Data: map[string]any{}}},
			}}
			q := &fakeWebhookQueue{pending: tc.pending, probeErr: tc.probeErr}
			mut := &fakeMutator{}
			runs := &fakeRunRecorder{}
			s := newScheduler(loader, lister, actionDeps{mut: mut, webhookQueue: q})
			s.runs = runs

			s.runDue(context.Background(), time.Now())

			if len(q.probes) != tc.wantProbes || (tc.wantProbes > 0 && q.probes[0] != "t1|wf1") {
				t.Fatalf("probes = %q, want %d probe(s) of t1|wf1", q.probes, tc.wantProbes)
			}
			if len(q.parked) != tc.wantParked {
				t.Fatalf("parked %d deliveries, want %d", len(q.parked), tc.wantParked)
			}
			if len(mut.calls) != 2 {
				t.Fatalf("field_update ran %d times, want 2 — held webhooks must not stop the rule's other actions", len(mut.calls))
			}
			all := runs.all()
			if len(all) != 1 || all[0].Status != RunSuccess {
				t.Fatalf("runs = %+v, want one success", all)
			}
			var summary map[string]int
			if err := json.Unmarshal(all[0].ActionsResult, &summary); err != nil {
				t.Fatalf("summary: %v", err)
			}
			if summary["webhooksHeld"] != tc.wantHeld || summary["matched"] != 2 {
				t.Fatalf("summary = %v, want matched 2 and webhooksHeld %d", summary, tc.wantHeld)
			}
		})
	}
}

// snapshotClockLister records the instant it reads the records, then holds for a few ms — the
// time a real list query plus the actions of earlier records take before a later record's body
// is built.
type snapshotClockLister struct {
	fakeRecordLister
	readAt time.Time
}

func (r *snapshotClockLister) ListRecordsForScheduler(ctx context.Context, entity, tenantID string, max int) ([]ScheduledRecord, error) {
	r.readAt = time.Now()
	time.Sleep(3 * time.Millisecond)
	return r.fakeRecordLister.ListRecordsForScheduler(ctx, entity, tenantID, max)
}

// TestScheduler_WebhookOccurredAtPrecedesSnapshotRead (review of #26): a receiver keeps, per
// recordId, the body with the LARGEST occurredAt. A scheduled firing's body carries the data read
// by ListRecordsForScheduler, so its occurredAt must not be later than that read: an edit
// committed after the read is Emit-stamped after it, and must win over the firing's stale copy.
// Stamping at marshal time (after the read, after earlier records' actions) inverted that.
func TestScheduler_WebhookOccurredAtPrecedesSnapshotRead(t *testing.T) {
	loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
		Actions: []Action{{Type: ActionWebhook, URL: "https://erp.test/in"}},
	}})
	lister := &snapshotClockLister{fakeRecordLister: fakeRecordLister{records: map[string][]ScheduledRecord{
		"deals": {{ID: "r1", Data: map[string]any{}}, {ID: "r2", Data: map[string]any{}}},
	}}}
	q := &fakeWebhookQueue{}
	s := newScheduler(loader, lister, actionDeps{webhookQueue: q})

	s.runDue(context.Background(), time.Now())

	if len(q.parked) != 2 {
		t.Fatalf("parked %d deliveries, want 2", len(q.parked))
	}
	for i, d := range q.parked {
		at, err := time.Parse(time.RFC3339Nano, webhookOccurredAt(t, d.Body))
		if err != nil || at.After(lister.readAt) {
			t.Fatalf("delivery %d occurredAt = %s (err %v), want <= the snapshot read at %s", i, at, err, lister.readAt.UTC())
		}
	}
}
