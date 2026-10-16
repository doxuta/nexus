package bpm

// specs/043 S13 — the SLA clock stops while the record waits for the customer, and the deadline can
// run on a calendar the process pinned. Fixed clocks and the real calendar arithmetic (Mon–Fri
// 08:00–17:00 ICT) — no server has a backdoor to set the time.

import (
	"context"
	"testing"
	"time"

	"nexus/platform/internal/workflow"
)

func pendingCond() *workflow.Condition {
	return cond("", workflow.Rule{Field: "status", Op: "eq", Value: "Pending"})
}

// pauseRig: a wait node with a 4 working-hour SLA, armed Monday 10:00 ICT ⇒ due Monday 14:00.
func pauseRig(t *testing.T, calID string) (*waitRig, *fakeCalc, *Instance) {
	t.Helper()
	calc := &fakeCalc{sched: vnWeek(t)}
	sla := &WaitSLA{Amount: 4, Unit: workflow.TimerUnitBusinessHours, Notify: &Assignment{Type: AssignField, Field: "assigned_to"}}
	data := map[string]any{"status": "Open"}
	mon1000 := time.Date(2026, 10, 5, 10, 0, 0, 0, ict)
	w := newWaitRig(waitGraph(sla), data, mon1000)
	w.runner.deadlines = calc
	inst := w.arm(data)
	if inst == nil || inst.SLADueAt == nil {
		t.Fatalf("instance %+v; want it resting with a deadline", inst)
	}
	inst.Definition.Options = &Options{PauseWhen: pendingCond(), CalendarID: calID}
	return w, calc, inst
}

func (w *waitRig) sync(t *testing.T, at time.Time, status string) *Instance {
	t.Helper()
	w.runner.now = func() time.Time { return at }
	w.rec.data["status"] = status
	inst := w.store.instances["inst-q1"]
	if err := w.runner.syncClock(context.Background(), inst, map[string]any{"status": status}); err != nil {
		t.Fatalf("syncClock: %v", err)
	}
	return w.store.instances["inst-q1"]
}

// TestPause_ResumeShiftsDueByPausedBusinessTime (T192): paused one working hour in (3 left), resumed
// the next morning at 09:00 ⇒ due 12:00 — the three REMAINING working hours, not the wall gap.
func TestPause_ResumeShiftsDueByPausedBusinessTime(t *testing.T) {
	w, _, _ := pauseRig(t, "")
	paused := w.sync(t, time.Date(2026, 10, 5, 11, 0, 0, 0, ict), "Pending")
	if paused.SLADueAt != nil || paused.SLAPausedAt == nil || paused.SLARemainingSeconds == nil || *paused.SLARemainingSeconds != 3*3600 {
		t.Fatalf("paused: due %v pausedAt %v remaining %v; want no due, a pause mark and 10800s", paused.SLADueAt, paused.SLAPausedAt, paused.SLARemainingSeconds)
	}
	if last := paused.History[len(paused.History)-1]; last.Action != HistoryPaused || last.SLABasis != SLABasisBusiness {
		t.Fatalf("last history %+v; want paused under business", last)
	}
	// a second update that still holds changes nothing (no rewrite, no second line)
	seq := paused.Seq
	if again := w.sync(t, time.Date(2026, 10, 5, 12, 0, 0, 0, ict), "Pending"); again.Seq != seq {
		t.Fatalf("seq %d→%d: an update that still holds must not rewrite the pause", seq, again.Seq)
	}
	resumed := w.sync(t, time.Date(2026, 10, 6, 9, 0, 0, 0, ict), "Open")
	wantDue(t, "resumed", resumed.SLADueAt, time.Date(2026, 10, 6, 12, 0, 0, 0, ict))
	if resumed.SLAPausedAt != nil || resumed.SLARemainingSeconds != nil {
		t.Fatalf("resumed still carries pause state %v / %v", resumed.SLAPausedAt, resumed.SLARemainingSeconds)
	}
	if last := resumed.History[len(resumed.History)-1]; last.Action != HistoryResumed {
		t.Fatalf("last history %+v; want resumed", last)
	}
	if !resting(resumed) {
		t.Fatal("pause/resume lines must not break the resting mark (the wait would re-stamp its deadline)")
	}
}

// TestPause_ResumeReenqueuesSLADueJob (T192): resume leaves the instance with a deadline and a NEW
// seq — the repository parks bpm_sla_due for (instance, seq) whenever a write sets sla_due_at, so a
// job of the old seq is stale and the new one is the only live one. (The enqueue itself is the T290
// integration invariant; here: the transition carries the deadline and bumps seq.)
func TestPause_ResumeReenqueuesSLADueJob(t *testing.T) {
	w, _, inst := pauseRig(t, "")
	before := inst.Seq
	w.sync(t, time.Date(2026, 10, 5, 11, 0, 0, 0, ict), "Pending")
	resumed := w.sync(t, time.Date(2026, 10, 6, 9, 0, 0, 0, ict), "Open")
	if resumed.Seq != before+2 {
		t.Fatalf("seq %d→%d; want +2 (pause, resume) so every older job is stale", before, resumed.Seq)
	}
	if last := w.store.lastNext; last.SLADueAt == nil || last.Clock == nil {
		t.Fatalf("resume transition %+v; want a deadline and a clock write", last)
	}
}

// TestPause_SeatsStillActionable (T192): a frozen clock does not freeze the work — the holder can
// still decide an approval step while its SLA is paused.
func TestPause_SeatsStillActionable(t *testing.T) {
	calc := &fakeCalc{sched: vnWeek(t)}
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "mgr", Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr", SLASeconds: 4 * 3600, SLAClock: SLAClockBusiness}, Kind: NodeApproval},
		{Step: Step{ID: "xong", Name: "Xong"}, Kind: NodeEnd, Result: ResultApproved},
	}}
	data := map[string]any{"status": "Open"}
	mon1000 := time.Date(2026, 10, 5, 10, 0, 0, 0, ict)
	w := newWaitRig(g, data, mon1000)
	w.runner.deadlines, w.eng.deadlines = calc, calc
	inst := w.arm(data)
	if inst == nil || inst.SLADueAt == nil {
		t.Fatalf("instance %+v; want an approval with a deadline", inst)
	}
	inst.Definition.Options = &Options{PauseWhen: pendingCond()}
	paused := w.sync(t, time.Date(2026, 10, 5, 11, 0, 0, 0, ict), "Pending")
	if paused.SLAPausedAt == nil || paused.SLADueAt != nil {
		t.Fatalf("approval not paused: %+v", paused)
	}
	got, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil || got.Status != StatusApproved {
		t.Fatalf("decide while paused: %v / %+v; want approved", err, got)
	}
}

// TestPinnedCalendar_OverridesDefault (T193): a process that pins a calendar asks for THAT one at
// arm time; one that does not asks for "" (the tenant default).
func TestPinnedCalendar_OverridesDefault(t *testing.T) {
	for _, tc := range []struct{ name, pin string }{{"pinned", "cal-support"}, {"default", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			calc := &fakeCalc{sched: vnWeek(t)}
			g := &Graph{Nodes: []Node{
				{Step: Step{ID: "mgr", Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr", SLASeconds: 3600, SLAClock: SLAClockBusiness}, Kind: NodeApproval},
				{Step: Step{ID: "xong", Name: "Xong"}, Kind: NodeEnd, Result: ResultApproved},
			}}
			data := map[string]any{"status": "Open"}
			w := newWaitRig(g, data, time.Date(2026, 10, 5, 10, 0, 0, 0, ict))
			w.eng.deadlines = calc
			w.eng.loader.(fakeLoader).procs[0].Options = &Options{CalendarID: tc.pin}
			if inst := w.arm(data); inst == nil || inst.SLADueAt == nil {
				t.Fatalf("instance %+v; want a deadline", inst)
			}
			if len(calc.cals) != 1 || calc.cals[0] != tc.pin {
				t.Fatalf("calendar asked = %q; want [%q]", calc.cals, tc.pin)
			}
		})
	}
}

// TestBreach_SkipsPaused (T276): a breach job that wakes while the clock is frozen is a no-op — no
// claim, no bell — whether the row still shows a stale due date or none.
func TestBreach_SkipsPaused(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	store := newFakeStore()
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u", SLADueAt: &past,
	})
	paused, secs := time.Now().UTC().Add(-time.Hour), int64(600)
	store.instances[id].SLAPausedAt, store.instances[id].SLARemainingSeconds = &paused, &secs
	s := NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, nil, nil)
	if err := s.NewSLADueJobHandler(store)(context.Background(), dueJob("t1", id, 1)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if store.instances[id].Escalated || len(store.notes) != 0 {
		t.Fatalf("a paused clock breached: escalated=%v notes=%+v", store.instances[id].Escalated, store.notes)
	}
}
