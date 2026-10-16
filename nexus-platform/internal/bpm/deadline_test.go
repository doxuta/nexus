package bpm

// specs/043 S1 (#39) — approval deadlines on the working calendar.
//
// The exact clock case (sent 16:00 Friday, 4-hour step ⇒ due 11:00 Monday, not 20:00 Friday) is
// pinned HERE with a fixed clock and the real calendar arithmetic: no server has a backdoor to set
// the time, so this is the only place the Friday case can be proven exactly.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
)

// vnWeek is the Q9 default calendar: Mon–Fri 08:00–17:00 Asia/Ho_Chi_Minh.
func vnWeek(t *testing.T) *calendar.Schedule {
	t.Helper()
	day := []calendar.Interval{{Start: "08:00", End: "17:00"}}
	s, err := calendar.New(calendar.Config{
		Timezone: "Asia/Ho_Chi_Minh",
		Hours:    map[string][]calendar.Interval{"mon": day, "tue": day, "wed": day, "thu": day, "fri": day},
	})
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	return s
}

// fakeCalc is a DeadlineCalculator over an in-memory schedule: nil schedule = the tenant has no
// calendar; err = the calendar store could not be read.
type fakeCalc struct {
	sched *calendar.Schedule
	err   error
	calls int
	cals  []string // the calendarID of every DueAt call
}

// BusinessBetween makes fakeCalc a BusinessMeter (S13).
func (f *fakeCalc) BusinessBetween(_ context.Context, _, _ string, from, to time.Time) (time.Duration, bool, error) {
	if f.err != nil || f.sched == nil {
		return 0, false, f.err
	}
	return f.sched.BusinessDurationBetween(from, to), true, nil
}

func (f *fakeCalc) DueAt(_ context.Context, _, calID string, from time.Time, d time.Duration) (time.Time, SLABasis, error) {
	f.calls++
	f.cals = append(f.cals, calID)
	if f.err != nil {
		return time.Time{}, "", f.err
	}
	if f.sched == nil {
		return from.Add(d), SLABasisWallNoCalendar, nil
	}
	due, err := f.sched.AddBusinessDuration(from, d)
	if err != nil {
		return from.Add(d), SLABasisWallCalendarError, nil
	}
	return due, SLABasisBusiness, nil
}

var (
	ict = func() *time.Location {
		l, err := time.LoadLocation("Asia/Ho_Chi_Minh")
		if err != nil {
			panic(err)
		}
		return l
	}()
	fri1600 = time.Date(2026, 10, 2, 16, 0, 0, 0, ict) // a Friday
	fri2000 = time.Date(2026, 10, 2, 20, 0, 0, 0, ict)
	mon1100 = time.Date(2026, 10, 5, 11, 0, 0, 0, ict)
)

const fourHours = 4 * 3600

func wantDue(t *testing.T, what string, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: due = nil, want %s", what, want.In(ict).Format("Mon 15:04"))
	}
	if !got.Equal(want) {
		t.Fatalf("%s: due = %s, want %s", what, got.In(ict).Format("Mon 02/01 15:04"), want.In(ict).Format("Mon 02/01 15:04"))
	}
}

// armOne arms a single after_create event on a one-step process and returns what was created.
func armOne(t *testing.T, calc DeadlineCalculator, step Step) (CreateInstanceInput, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterCreate,
		Steps: []Step{step}, IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, loader, store, store, nil).WithDeadlines(calc)
	e.now = func() time.Time { return fri1600 }
	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate})
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 instance armed, got %d", len(store.created))
	}
	return store.created[0], store
}

// T017 — the headline case of #39.
func TestArm_BusinessClock_Fri1600_4h_DueMon1100(t *testing.T) {
	in, store := armOne(t, &fakeCalc{sched: vnWeek(t)}, Step{ApproverType: ApproverUser, ApproverID: "u1", SLASeconds: fourHours})
	wantDue(t, "arm", in.SLADueAt, mon1100)
	// A live step without slaClock means "business" for a NEW instance — written EXPLICITLY into the
	// pin, so the instance keeps that answer whatever the live process says later.
	if got := in.Definition.Steps[0].SLAClock; got != SLAClockBusiness {
		t.Errorf("pinned slaClock = %q, want %q written explicitly at arm", got, SLAClockBusiness)
	}
	if got := store.instances["inst-q1"].CurrentSLABasis(); got != SLABasisBusiness {
		t.Errorf("basis = %q, want %q", got, SLABasisBusiness)
	}
}

// T017 — a tenant with no live calendar (#82, northwind today) still gets the wall-clock number, and the
// basis says WHY, so the screen never presents it as working hours.
func TestArm_NoCalendar_WallClock2000_BasisNoCalendar(t *testing.T) {
	in, store := armOne(t, &fakeCalc{}, Step{ApproverType: ApproverUser, ApproverID: "u1", SLASeconds: fourHours})
	wantDue(t, "arm (no calendar)", in.SLADueAt, fri2000)
	if got := store.instances["inst-q1"].CurrentSLABasis(); got != SLABasisWallNoCalendar {
		t.Errorf("basis = %q, want %q", got, SLABasisWallNoCalendar)
	}
}

// The escape hatch: a step configured slaClock "wall" keeps wall-clock on purpose and never asks
// the calendar.
func TestArm_ExplicitWallStepKeepsWall(t *testing.T) {
	calc := &fakeCalc{sched: vnWeek(t)}
	in, store := armOne(t, calc, Step{ApproverType: ApproverUser, ApproverID: "u1", SLASeconds: fourHours, SLAClock: SLAClockWall})
	wantDue(t, "arm (explicit wall)", in.SLADueAt, fri2000)
	if calc.calls != 0 {
		t.Errorf("calendar consulted %d times for a wall-clock step", calc.calls)
	}
	if got := store.instances["inst-q1"].CurrentSLABasis(); got != SLABasisWall {
		t.Errorf("basis = %q, want %q", got, SLABasisWall)
	}
}

// T020 — the async arming path cannot ask anyone, so a calendar-store failure degrades to wall
// clock, but under its OWN label: "we could not read the calendar" is a different fact from "this
// organisation has none", and the two must never share a word on screen.
func TestArm_StoreErrorStampsCalendarErrorBasis(t *testing.T) {
	in, store := armOne(t, &fakeCalc{err: errors.New("db down")}, Step{ApproverType: ApproverUser, ApproverID: "u1", SLASeconds: fourHours})
	wantDue(t, "arm (store error)", in.SLADueAt, fri2000)
	if got := store.instances["inst-q1"].CurrentSLABasis(); got != SLABasisWallCalendarError {
		t.Errorf("basis = %q, want %q (not %q)", got, SLABasisWallCalendarError, SLABasisWallNoCalendar)
	}
}

// T018 — Submit is the second way an instance starts; same calendar, same pin.
func TestSubmit_DueOnCalendar(t *testing.T) {
	p := manualProc(nil)
	p.Steps[0].SLASeconds = fourHours
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {p}})
	r.WithDeadlines(&fakeCalc{sched: vnWeek(t)})
	r.now = func() time.Time { return fri1600 }

	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	wantDue(t, "submit", store.created[0].SLADueAt, mon1100)
	if got := store.created[0].Definition.Steps[0].SLAClock; got != SLAClockBusiness {
		t.Errorf("pinned slaClock = %q, want %q", got, SLAClockBusiness)
	}
	if got := inst.CurrentSLABasis(); got != SLABasisBusiness {
		t.Errorf("basis = %q, want %q", got, SLABasisBusiness)
	}
}

// twoStepPinned seeds an instance at step 0 whose step 1 has a 4-hour SLA on the given clock.
func twoStepPinned(store *fakeStore, clock string) {
	pinnedInstance(store, "i1", "t1", "p1", &Definition{Steps: NormalizeSteps([]Step{
		{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr", SLAClock: clock},
		{Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir", SLASeconds: fourHours, SLAClock: clock},
	})})
}

// T018 — approving step 1 opens step 2 on the calendar; the decision entry records the basis of
// the deadline it set.
func TestDecide_NextStepDueOnCalendar(t *testing.T) {
	store := newFakeStore()
	twoStepPinned(store, SLAClockBusiness)
	d := NewDecider(store, store, store, nil, nil).WithDeadlines(&fakeCalc{sched: vnWeek(t)})
	d.now = func() time.Time { return fri1600 }

	got, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	wantDue(t, "next step", got.SLADueAt, mon1100)
	if e := got.History[len(got.History)-1]; e.SLABasis != SLABasisBusiness {
		t.Errorf("decision entry slaBasis = %q, want %q", e.SLABasis, SLABasisBusiness)
	}
	if b := got.CurrentSLABasis(); b != SLABasisBusiness {
		t.Errorf("basis = %q, want %q", b, SLABasisBusiness)
	}
}

// T018 — handing a step on buys no time and does not re-ask the calendar.
func TestDelegate_KeepsDue(t *testing.T) {
	p := manualProc(nil)
	p.Steps[0].SLASeconds = fourHours
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {p}})
	calc := &fakeCalc{sched: vnWeek(t)}
	r.WithDeadlines(calc)
	r.now = func() time.Time { return fri1600 }
	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	r.now = func() time.Time { return mon1100.Add(-time.Hour) }
	got, err := r.Delegate(context.Background(), "t1", inst.ID, "deputy", "", Actor{UserID: "boss"})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	wantDue(t, "after delegate", store.instances[inst.ID].SLADueAt, mon1100)
	if calc.calls != 1 {
		t.Errorf("calendar consulted %d times; want 1 (submit only — delegation keeps the deadline)", calc.calls)
	}
	if b := got.CurrentSLABasis(); b != SLABasisBusiness {
		t.Errorf("basis after delegate = %q, want %q (the deadline did not change)", b, SLABasisBusiness)
	}
}

// T019 — an instance pinned BEFORE this slice has no slaClock on its steps: its next step keeps
// wall clock. Changing how an in-flight instance is timed half-way through is the retroactive
// change #39 forbids.
func TestDecide_PinnedPreSliceSnapshotKeepsWall(t *testing.T) {
	store := newFakeStore()
	twoStepPinned(store, "") // pinned pre-S1: no key at all
	calc := &fakeCalc{sched: vnWeek(t)}
	d := NewDecider(store, store, store, nil, nil).WithDeadlines(calc)
	d.now = func() time.Time { return fri1600 }

	got, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	wantDue(t, "pre-slice pin", got.SLADueAt, fri2000)
	if calc.calls != 0 {
		t.Errorf("calendar consulted %d times for a pre-slice pin", calc.calls)
	}
	if b := got.CurrentSLABasis(); b != SLABasisWall {
		t.Errorf("basis = %q, want %q", b, SLABasisWall)
	}
}

// T019 — an instance armed before migration 000065 has NO pin and decides against the live
// process. The live steps lacking slaClock (every process saved before S1) must not flip it to
// business mid-flight — and neither does a live step re-saved as explicit business: the instance
// started on wall clock and finishes on it (D-9; Migration A later pins it "wall" explicitly).
func TestDecide_NullSnapshotLegacyKeepsWall(t *testing.T) {
	for _, clock := range []string{"", SLAClockBusiness} {
		t.Run("live slaClock="+clock, func(t *testing.T) {
			store := newFakeStore()
			procWithSteps(t, store, "p-live", "t1", []Step{
				{ApproverType: ApproverUser, ApproverID: "u-mgr", SLAClock: clock},
				{ApproverType: ApproverUser, ApproverID: "u-dir", SLASeconds: fourHours, SLAClock: clock},
			})
			store.instances["i-old"] = &Instance{ID: "i-old", TenantID: "t1", ProcessID: "p-live", EntityName: "deals",
				RecordID: "r9", Status: StatusPending, CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr"}
			calc := &fakeCalc{sched: vnWeek(t)}
			d := NewDecider(store, store, store, nil, nil).WithDeadlines(calc)
			d.now = func() time.Time { return fri1600 }

			got, err := d.Decide(context.Background(), "t1", "i-old", DecisionApprove, "", Actor{UserID: "u-mgr"})
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			wantDue(t, "pre-000065 instance", got.SLADueAt, fri2000)
			if calc.calls != 0 {
				t.Errorf("calendar consulted %d times for an unpinned instance", calc.calls)
			}
		})
	}
}

// T020 — the person pressing the button gets the error: a deadline silently computed on the wrong
// clock is indistinguishable from a right one afterwards. Nothing is written.
func TestDeadline_StoreErrorSyncPathErrors(t *testing.T) {
	storeDown := errors.New("calendar store down")

	t.Run("submit", func(t *testing.T) {
		p := manualProc(nil)
		p.Steps[0].SLASeconds = fourHours
		r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {p}})
		r.WithDeadlines(&fakeCalc{err: storeDown})
		_, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
		if !errors.Is(err, storeDown) {
			t.Fatalf("err = %v, want the store error", err)
		}
		if len(store.created) != 0 {
			t.Errorf("an instance was created although its deadline could not be computed")
		}
	})

	t.Run("decide", func(t *testing.T) {
		store := newFakeStore()
		twoStepPinned(store, SLAClockBusiness)
		d := NewDecider(store, store, store, nil, nil).WithDeadlines(&fakeCalc{err: storeDown})
		_, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"})
		if !errors.Is(err, storeDown) {
			t.Fatalf("err = %v, want the store error", err)
		}
		if store.advanceCnt != 0 || store.instances["i1"].CurrentStep != 0 {
			t.Errorf("the decision was applied although the next deadline could not be computed")
		}
	})
}

// T021 — a server whose engine was never handed the calendar must refuse to start: unwired, every
// deadline would quietly go back to wall clock under a "working hours" configuration.
func TestServer_BPMDeadlineUnwiredFailsFast(t *testing.T) {
	calc := &fakeCalc{}
	wired := func() (*AsyncEngine, *Decider, *Requester) {
		return NewAsyncEngine(EngineConfig{}, nil, nil, nil, nil).WithDeadlines(calc),
			NewDecider(nil, nil, nil, nil, nil).WithDeadlines(calc),
			NewRequester(nil, nil, nil, nil, nil).WithDeadlines(calc)
	}
	e, d, r := wired()
	if err := RequireDeadlines(e, d, r); err != nil {
		t.Fatalf("fully wired: err = %v, want nil", err)
	}
	for _, tc := range []struct {
		name  string
		unset func(*AsyncEngine, *Decider, *Requester)
	}{
		{"engine", func(e *AsyncEngine, _ *Decider, _ *Requester) { e.deadlines = nil }},
		{"decider", func(_ *AsyncEngine, d *Decider, _ *Requester) { d.deadlines = nil }},
		{"requester", func(_ *AsyncEngine, _ *Decider, r *Requester) { r.deadlines = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, d, r := wired()
			tc.unset(e, d, r)
			err := RequireDeadlines(e, d, r)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("err = %v, want a refusal naming the %s", err, tc.name)
			}
		})
	}
	if err := RequireDeadlines(nil, d, r); err == nil {
		t.Error("a nil engine must be refused too")
	}
}

// The first deadline's basis rides the pin, later ones ride the decision entry that set them —
// and the JSON of both stays additive (an old reader sees the keys it always saw).
func TestCurrentSLABasis_ReadOrder(t *testing.T) {
	due := fri2000
	cases := []struct {
		name string
		inst Instance
		want SLABasis
	}{
		{"no SLA", Instance{}, ""},
		{"pre-slice: nothing recorded", Instance{SLADueAt: &due, History: []HistoryEntry{{Action: HistoryApproved}}}, SLABasisWall},
		{"first deadline from the pin", Instance{SLADueAt: &due, Definition: &Definition{FirstDueBasis: SLABasisWallNoCalendar}}, SLABasisWallNoCalendar},
		{"later deadline from its decision", Instance{SLADueAt: &due, Definition: &Definition{FirstDueBasis: SLABasisWallNoCalendar},
			History: []HistoryEntry{{Action: HistoryApproved, SLABasis: SLABasisBusiness}, {Action: HistoryDelegated}}}, SLABasisBusiness},
	}
	for _, tc := range cases {
		if got := tc.inst.CurrentSLABasis(); got != tc.want {
			t.Errorf("%s: basis = %q, want %q", tc.name, got, tc.want)
		}
	}
	b, err := json.Marshal(HistoryEntry{Step: 1, Action: HistoryApproved, Actor: "u1", At: fri1600.UTC()})
	if err != nil || strings.Contains(string(b), "slaBasis") {
		t.Errorf("entry without a basis must not serialize the key: %s (err %v)", b, err)
	}
}
