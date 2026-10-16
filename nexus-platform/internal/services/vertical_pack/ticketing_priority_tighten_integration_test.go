//go:build integration

// Queue #81 (`sla-deadline-on-priority-change`, specs/043 S14a) end to end against real MySQL: the
// built-in Ticketing pack, installed fresh, recomputes a ticket's deadline when its priority
// changes — shortening only, never overwriting a hand-typed deadline, never touching a ticket whose
// deadline the machine cannot prove it wrote.
//
// Every expectation is derived from the ticket's REAL created_at through the tenant's working
// calendar (DefaultCalendarSchedule), so a recomputation anchored on the wrong instant (the moment
// of the priority change) or in wall-clock hours is caught at any hour of the day: 40 business
// hours always span a weekend, 4 business hours from creation differ from 4 from "now" whenever the
// two instants differ, which the restart between create and update guarantees by at least the
// dispatcher's drain.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -run 'TicketingPack_' ./internal/services/vertical_pack/
package vertical_pack

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// tightenFixture installs the Ticketing pack into a fresh tenant with the default working calendar
// and returns the stack plus the admin scope tickets are raised and edited with.
func tightenFixture(t *testing.T) (*slaStack, services.RecordScope) {
	return tightenFixtureWorkers(t, 1)
}

func tightenFixtureWorkers(t *testing.T, workers int) (*slaStack, services.RecordScope) {
	t.Helper()
	gdb, sdb := vpSetup(t)
	st := vpBuildSLAWorkers(gdb, sdb, workers)
	slaSeedInstaller(t, gdb)
	if _, err := services.SeedDefaultBusinessCalendar(context.Background(), gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	if _, err := st.pack.Install(context.Background(), vpTenantA, vpSeedUserID, pack); err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}
	return st, services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
}

// ticketDeadline reads a ticket's due_date, its machine stamp (zero when absent) and created_at.
func (s *slaStack) ticketDeadline(t *testing.T, scope services.RecordScope, id string) (due, stamp, created time.Time) {
	t.Helper()
	rec, err := s.records.Get(context.Background(), scope, vpTenantA, "tk_tickets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get ticket: %v", err)
	}
	parse := func(k string) time.Time {
		raw, _ := rec.Data[k].(string)
		if raw == "" {
			return time.Time{}
		}
		ts, err := time.Parse("2006-01-02 15:04:05", raw)
		if err != nil {
			t.Fatalf("%s persisted as %q: %v", k, raw, err)
		}
		return ts.UTC()
	}
	return parse("due_date"), parse("due_date_auto"), rec.CreatedAt.UTC()
}

// raise creates a ticket with no deadline and lets the creation tier stamp it.
func (s *slaStack) raise(t *testing.T, scope services.RecordScope, priority string) string {
	t.Helper()
	id, err := s.records.Create(context.Background(), scope, vpTenantA, vpSeedUserID, "tk_tickets",
		map[string]any{"subject": "Phiếu " + priority, "priority": priority, "status": "Open"})
	if err != nil {
		t.Fatalf("create %s ticket: %v", priority, err)
	}
	s.restart(t)
	due, stamp, _ := s.ticketDeadline(t, scope, id)
	if due.IsZero() || !due.Equal(stamp) {
		t.Fatalf("creation tier left due_date=%s stamp=%s — want both set and equal (the machine's signature)", due, stamp)
	}
	return id
}

func (s *slaStack) edit(t *testing.T, scope services.RecordScope, id string, patch map[string]any) {
	t.Helper()
	if err := s.records.Update(context.Background(), scope, vpTenantA, "tk_tickets", id, patch, nil); err != nil {
		t.Fatalf("update ticket %v: %v", patch, err)
	}
	s.restart(t)
}

// US11 AC1: Low → Urgent recomputes FROM CREATION through the calendar, and since that is earlier
// it applies — to the deadline and to the stamp, so the next change still knows it is the machine's.
func TestTicketingPack_PriorityRaiseTightensDue(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	sched, err := services.DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("parse default calendar: %v", err)
	}
	id := st.raise(t, admin, "Low")
	// US11's Independent Test verbatim: the ticket was raised 09:00 Monday (ICT). Pinning created_at
	// makes the answer independent of the hour this runs AND tells a created_at anchor from a "now"
	// one — both land on the same second when create and update run back to back.
	monday9 := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	if _, err := st.sdb.Exec("UPDATE obj_tk_tickets SET created_at = ? WHERE tenant_id = ? AND id = ?", monday9, vpTenantA, id); err != nil {
		t.Fatalf("pin created_at: %v", err)
	}
	st.edit(t, admin, id, map[string]any{"priority": "Urgent"})

	due, stamp, created := st.ticketDeadline(t, admin, id)
	if !created.Equal(monday9) {
		t.Fatalf("created_at reads back %s, want %s", created, monday9)
	}
	want, err := sched.AddBusinessDuration(created, time.Duration(slaTierAmounts(t)["Urgent"])*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	if !want.Equal(time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("calendar says 09:00 Mon + 4 business hours = %s, spec says 13:00 Mon ICT", want)
	}
	if !due.Equal(want.UTC()) {
		t.Fatalf("after Low→Urgent due_date = %s, want %s (created_at %s + %d business hours)",
			due.Format(time.RFC3339), want.UTC().Format(time.RFC3339), created.Format(time.RFC3339), slaTierAmounts(t)["Urgent"])
	}
	if !stamp.Equal(due) {
		t.Fatalf("stamp %s not moved with the deadline %s — the next priority change would see a hand-typed deadline", stamp, due)
	}
}

// US11 AC2: Urgent → Low would be later, so the promised deadline stays.
func TestTicketingPack_PriorityLowerKeepsDue(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	id := st.raise(t, admin, "Urgent")
	promised, _, _ := st.ticketDeadline(t, admin, id)
	st.edit(t, admin, id, map[string]any{"priority": "Low"})
	if due, _, _ := st.ticketDeadline(t, admin, id); !due.Equal(promised) {
		t.Fatalf("Urgent→Low moved due_date %s → %s; a promised deadline must never be lengthened", promised, due)
	}
}

// US11 AC3: an agent typed the deadline after the machine stamped it — raising the priority leaves
// the typed value alone.
func TestTicketingPack_ManualDueUntouched(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	id := st.raise(t, admin, "Low")
	st.edit(t, admin, id, map[string]any{"due_date": "2027-03-01T09:00:00Z"})
	st.edit(t, admin, id, map[string]any{"priority": "Urgent"})
	want := time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)
	if due, _, _ := st.ticketDeadline(t, admin, id); !due.Equal(want) {
		t.Fatalf("a hand-typed deadline was overwritten: due_date = %s, want %s", due, want)
	}
}

// DAILY-QUEUE #172: the priority-change event is handled late (the async dispatcher's queue delay —
// seconds under a mass edit), and an agent typed a deadline in between. The event's snapshot still
// shows the machine's deadline; the record shows the typed one. The recompute must keep the typed
// deadline — it decides from the row, and writes only while the row is still the one it read.
func TestTicketingPack_DeadlineTypedWhileTheRecomputeWasQueuedIsKept(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	ctx := context.Background()
	id := st.raise(t, admin, "Low")
	snap, err := st.records.Get(ctx, admin, vpTenantA, "tk_tickets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get ticket: %v", err)
	}
	// What the queued priority-change event carries: priority Urgent over a machine deadline.
	evtData := map[string]any{}
	for k, v := range snap.Data {
		evtData[k] = v
	}
	evtData["priority"] = "Urgent"

	st.edit(t, admin, id, map[string]any{"due_date": "2027-03-01T09:00:00Z"}) // the agent's save

	var rule *workflow.Workflow
	recs, err := st.pack.workflowWr.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	for i := range recs {
		if recs[i].Name == "Tính lại hạn SLA — đổi sang Khẩn" {
			wf, err := recs[i].ToWorkflow()
			if err != nil {
				t.Fatalf("decode rule: %v", err)
			}
			rule = &wf
		}
	}
	if rule == nil {
		t.Fatal("the Urgent recompute rule is not installed")
	}
	if err := st.dispatcher.FireWorkflow(ctx, *rule, workflow.Event{
		TenantID: vpTenantA, ActorUserID: vpSeedUserID, EntityName: "tk_tickets", RecordID: id,
		Trigger: workflow.TriggerAfterUpdate, Data: evtData, Before: map[string]any{"priority": "Low"},
		CreatedAt: snap.CreatedAt,
	}); err != nil {
		t.Fatalf("fire the late recompute: %v", err)
	}
	want := time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)
	if due, _, _ := st.ticketDeadline(t, admin, id); !due.Equal(want) {
		t.Fatalf("the late recompute overwrote the typed deadline: due_date = %s, want %s", due, want)
	}
}

// DAILY-QUEUE #172, the store half: the conditional write is a real RM-24 version predicate — a
// write that landed after the read makes it match no row, write nothing, and say so.
func TestWorkflowRecordMutator_UpdateFieldsIfVersionLosesToALaterWrite(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	ctx := context.Background()
	id := st.raise(t, admin, "Low")
	mut := services.NewWorkflowRecordMutator(dynrepo.New(st.sdb))
	_, version, err := mut.ReadForUpdate(ctx, "tk_tickets", vpTenantA, id)
	if err != nil {
		t.Fatalf("ReadForUpdate: %v", err)
	}
	st.edit(t, admin, id, map[string]any{"due_date": "2027-03-01T09:00:00Z"}) // bumps the version
	err = mut.UpdateFieldsIfVersion(ctx, "tk_tickets", vpTenantA, id, workflow.SystemActorID,
		map[string]any{"due_date": "2026-01-01T00:00:00Z", "due_date_auto": "2026-01-01T00:00:00Z"}, version)
	if !errors.Is(err, workflow.ErrRecordChanged) {
		t.Fatalf("stale conditional write err = %v; want workflow.ErrRecordChanged", err)
	}
	due, stamp, _ := st.ticketDeadline(t, admin, id)
	if !due.Equal(time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)) || stamp.Year() == 2026 && stamp.Month() == 1 && stamp.Day() == 1 {
		t.Fatalf("stale write changed the row: due_date %s, stamp %s", due, stamp)
	}
	// At the current version the same call writes both fields in one update.
	_, version, _ = mut.ReadForUpdate(ctx, "tk_tickets", vpTenantA, id)
	if err := mut.UpdateFieldsIfVersion(ctx, "tk_tickets", vpTenantA, id, workflow.SystemActorID,
		map[string]any{"due_date": "2026-11-02T03:00:00Z", "due_date_auto": "2026-11-02T03:00:00Z"}, version); err != nil {
		t.Fatalf("current-version conditional write: %v", err)
	}
	if _, after, _ := mut.ReadForUpdate(ctx, "tk_tickets", vpTenantA, id); after != version+1 {
		t.Fatalf("version after one conditional write = %d, want %d (one update for both fields)", after, version+1)
	}
}

// US11 AC5: a ticket stamped by pack 1.7.0 carries a machine deadline but no stamp. The software
// cannot prove the deadline is its own, so it is treated like a hand-typed one and never shortened.
func TestTicketingPack_PreSliceTicketWithoutStampUntouched(t *testing.T) {
	st, admin := tightenFixture(t)
	defer st.drain(t)
	id := st.raise(t, admin, "Low")
	// The 1.7.0 shape: remove the stamp without an event (no automation reacts to a raw store edit).
	if _, err := st.sdb.Exec("UPDATE obj_tk_tickets SET data = JSON_REMOVE(data, '$.due_date_auto') WHERE tenant_id = ? AND id = ?",
		vpTenantA, id); err != nil {
		t.Fatalf("strip stamp: %v", err)
	}
	before, stamp, _ := st.ticketDeadline(t, admin, id)
	if !stamp.IsZero() {
		t.Fatalf("stamp still present after strip: %s", stamp)
	}
	st.edit(t, admin, id, map[string]any{"priority": "Urgent"})
	if due, _, _ := st.ticketDeadline(t, admin, id); !due.Equal(before) {
		t.Fatalf("a deadline with no machine stamp was shortened %s → %s", before, due)
	}
}

// The export half of the pack path: a fresh install diffs UNCHANGED on every deadline rule. If
// export dropped mode/stampField/anchor, the recompute tiers would read as modified here — and a
// pack exported from this tenant would install them as OVERWRITE rules on the next customer.
func TestTicketingPack_DeadlineRulesRoundTripThroughExport(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := newFakeItemStore()
	st.pack.WithItemStore(store)
	pack, _ := BuiltinPack("ticketing")
	ctx := context.Background()
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, pack); err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}
	report, err := st.pack.DiffInstalled(ctx, vpTenantA, "ticketing")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	deadlineRules := map[string]bool{}
	for _, pw := range pack.Workflows {
		for _, a := range pw.Actions {
			if a.Type == "set_deadline" {
				deadlineRules[pw.Name] = true
			}
		}
	}
	seen := 0
	for _, it := range report.Items {
		if it.ItemType != ItemWorkflow || !deadlineRules[it.ItemKey] {
			continue
		}
		seen++
		if it.Status != "unchanged" {
			t.Errorf("%q after a fresh install: status=%s changed=%v", it.ItemKey, it.Status, it.ChangedPaths)
		}
	}
	if seen != len(deadlineRules) {
		t.Fatalf("diff covered %d of %d deadline rules", seen, len(deadlineRules))
	}
}

// #444: a ticket created Low and changed to Urgent straight away (both events in the queue at once,
// no restart between) must still end on the Urgent deadline when several workers drain the queue.
// Before the dispatcher keyed its channels by record, a free worker could run the recompute before
// the creation tier had stamped the row, and the ticket kept the 40-hour Low deadline.
func TestTicketingPack_LowThenUrgentBackToBackKeepsUrgentDeadline(t *testing.T) {
	st, admin := tightenFixtureWorkers(t, 4)
	sched, err := services.DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("parse default calendar: %v", err)
	}
	urgent := time.Duration(slaTierAmounts(t)["Urgent"]) * time.Hour
	const tickets = 12
	ids := make([]string, 0, tickets)
	for i := 0; i < tickets; i++ {
		id, err := st.records.Create(context.Background(), admin, vpTenantA, vpSeedUserID, "tk_tickets",
			map[string]any{"subject": "Phiếu dồn", "priority": "Low", "status": "Open"})
		if err != nil {
			t.Fatalf("create ticket %d: %v", i, err)
		}
		if err := st.records.Update(context.Background(), admin, vpTenantA, "tk_tickets", id, map[string]any{"priority": "Urgent"}, nil); err != nil {
			t.Fatalf("update ticket %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	st.restart(t) // drains: every event of every ticket has been processed
	defer st.drain(t)
	for i, id := range ids {
		due, _, created := st.ticketDeadline(t, admin, id)
		want, err := sched.AddBusinessDuration(created, urgent)
		if err != nil {
			t.Fatalf("AddBusinessDuration: %v", err)
		}
		if !due.Equal(want) {
			t.Errorf("ticket %d: due_date = %s, want the Urgent deadline %s (created %s)", i, due, want, created)
		}
	}
}
