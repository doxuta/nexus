//go:build integration

// DAILY-QUEUE #11 (`ticketing-sla-pack`) — the headline claim, end to end against real MySQL:
// install the built-in Ticketing pack into a fresh tenant, raise a ticket the way an agent does
// (subject + priority, NO deadline), and the ticket comes back carrying a due_date the machine
// computed through the tenant's WORKING CALENDAR.
//
// What this proves that ticketing_sla_test.go (pack literals only) cannot:
//
//   - the pack's set_deadline actions survive install — `offset` is a param the pack→install
//     mapping has to carry by hand (install.go), and an unmapped one fails Pass 8 with no rollback;
//
//   - the computed instant survives the REAL record write path (datetime validator →
//     canonicalDatetimeUTC → obj_*.data), the gap specs/037 §4 calls out;
//
//   - the number is BUSINESS-hours, not wall-clock. Every assertion below compares against
//     DefaultCalendarSchedule().AddBusinessDuration — and the no-calendar fallback is silent by
//     design (the documented CALENDAR-1 contract), so a test that only asserted "due_date is not
//     empty" would pass just as happily on wall-clock arithmetic.
//
//     Know the LIMIT of that, though: business-hours and wall-clock arithmetic COINCIDE for a
//     small offset taken during the working day. Run at 10:30 on a Wednesday, `+4 business hours`
//     and `+4 wall-clock hours` are the same instant, so the Urgent assertion alone cannot tell
//     them apart — measured on 2026-09-16, where flipping the Urgent tier to `hours` left this
//     file green. The 40-business-hour fall-through tier is what discriminates in EVERY run (it
//     spans a weekend, so wall-clock lands days early), and ticketing_sla_test.go pins the unit
//     on all four tiers without consulting a clock at all. Do not accept this slice on the
//     Urgent assertion, and do not read a mutation-prove here without noting the hour it ran.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -run TicketingSLA ./internal/services/vertical_pack/
package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"
)

// slaStack is the production-shaped composition a fired pack rule needs: a RecordService with the
// workflow engine attached, and that engine carrying the CALENDAR-1 schedule resolver. vpBuildWF
// deliberately stops short of this (its tests only assert rules were CREATED), and the missing
// piece is load-bearing: without WithScheduleResolver a business_hours set_deadline REFUSES to
// stamp rather than quietly producing a wall-clock number (internal/workflow/actions.go).
//
// It also carries the TIMER-1 producer (WithTimerScheduler over the real jobs store), wired the way
// cmd/server/main.go wires it when the queue is on (DAILY-QUEUE #84). Without it this stack proved
// "due_date is stamped" and stopped exactly there, so the pack's two due_date timers — the reason
// the stamp exists — were only ARGUED to anchor on it, never shown.
type slaStack struct {
	pack       *Service
	records    *services.RecordService
	dispatcher *workflow.AsyncDispatcher
	sdb        *sqlx.DB
	// newDispatcher builds another engine over the SAME deps. drain() shuts the dispatcher down for
	// good, so a test that needs a second round of events (create, THEN change priority) swaps a
	// fresh one in with restart().
	newDispatcher func() *workflow.AsyncDispatcher
}

func vpBuildSLA(gdb *gorm.DB, sdb *sqlx.DB) *slaStack { return vpBuildSLAWorkers(gdb, sdb, 1) }

// vpBuildSLAWorkers is vpBuildSLA with a worker pool of the given size (the server runs
// ALERTS_WORKERS=2); #444 needs more than one to show that events of one record keep their order.
func vpBuildSLAWorkers(gdb *gorm.DB, sdb *sqlx.DB, workers int) *slaStack {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)
	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL)

	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc) // as cmd/server: an added field is visible to the workflow validator at once
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	wfMutator := services.NewWorkflowRecordMutator(dyn).WithCatalog(mc)
	calRepo := calmeta.NewSQLRepository(sdb)
	resolver := services.NewWorkflowScheduleResolver(calRepo, zap.NewNop())
	newDispatcher := func() *workflow.AsyncDispatcher {
		return workflow.NewAsyncDispatcher(
			workflow.DispatcherConfig{Buffer: 64, Workers: workers},
			wfCache, wfMutator, wfRepo, zap.NewNop(),
		).WithScheduleResolver(resolver).WithRunRecorder(wfRepo).
			WithTimerScheduler(services.NewWorkflowTimerScheduler(jobs.NewSQLStore(sdb, 5), calRepo, dyn))
	}
	dispatcher := newDispatcher()
	workflowSvc := services.NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithScheduleResolver(resolver)

	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn).
		WithEngine(dispatcher)

	dashboardSvc := services.NewDashboardService(dashboard.NewSQLRepository(sdb), recordSvc, zap.NewNop())

	svc := NewService(Deps{
		EntityReader:       metaRepo,
		LayoutReader:       layoutRepo,
		RoleReader:         roleRepo,
		EntityWriter:       entitySvc,
		EntityExists:       metaRepo,
		FieldWriter:        fieldSvc,
		LayoutWriter:       layoutSvc,
		RoleWriter:         roleSvc,
		RelationWriter:     relationSvc,
		RecordWriter:       recordSvc,
		Linker:             relationSvc,
		RecordExists:       dyn,
		RecordFetcher:      dyn,
		DashboardWriter:    dashboardSvc,
		WorkflowWriter:     workflowSvc,
		WorkQueueWriter:    services.NewWorkQueueService(workqueue.New(gdb), recordSvc, nil, roleRepo, nil),
		TeamWriter:         auth.NewTeamRepository(gdb),
		RelationshipReader: metaRepo,
		RecordLister:       dyn,
		ObjProber:          dyn,
		ScheduleResolver:   resolver, // DAILY-QUEUE #82: install warns when no default calendar resolves
	})
	return &slaStack{pack: svc, records: recordSvc, dispatcher: dispatcher, sdb: sdb, newDispatcher: newDispatcher}
}

// restart drains the current dispatcher (every emitted event processed) and attaches a fresh one,
// so the next mutation's events are processed too.
func (s *slaStack) restart(t *testing.T) {
	t.Helper()
	s.drain(t)
	s.dispatcher = s.newDispatcher()
	s.records.WithEngine(s.dispatcher)
}

// drain blocks until every emitted event has been processed, then closes the dispatcher.
func (s *slaStack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain: %v", err)
	}
}

// TestTicketingSLAPack_FreshInstallStampsBusinessHoursDeadline is the slice's acceptance in code.
func TestTicketingSLAPack_FreshInstallStampsBusinessHoursDeadline(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	ctx := context.Background()
	const user = vpSeedUserID

	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		user, vpTenantA, "installer@tk-sla-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}
	// Every tenant provisioned since DAILY-QUEUE #10 carries this calendar
	// (tenant_provision_service.go). vpSetup inserts its tenants with a raw INSERT, so seed it
	// here through the same exported seeder rather than a hand-written copy.
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	sched, err := services.DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("parse default calendar: %v", err)
	}

	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	installed, err := st.pack.Install(ctx, vpTenantA, user, pack)
	if err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}
	// DAILY-QUEUE #82, the quiet half: a tenant that HAS a default calendar is not told it has none.
	if got := calendarWarnings(installed); len(got) != 0 {
		t.Errorf("install warned %q in a tenant whose default calendar resolves", got)
	}

	admin := services.RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	// An agent raises three tickets the way the create form does: no deadline typed.
	before := time.Now().UTC()
	urgentID, err := st.records.Create(ctx, admin, vpTenantA, user, "tk_tickets",
		map[string]any{"subject": "Máy chủ không phản hồi", "priority": "Urgent", "status": "Open"})
	if err != nil {
		t.Fatalf("create Urgent ticket: %v", err)
	}
	noPriorityID, err := st.records.Create(ctx, admin, vpTenantA, user, "tk_tickets",
		map[string]any{"subject": "Chưa phân loại", "status": "Open"})
	if err != nil {
		t.Fatalf("create ticket with no priority: %v", err)
	}
	// ...and one where the agent DID type a deadline. The machine must leave it alone.
	const typedDeadline = "2026-12-24T09:00:00Z"
	typedID, err := st.records.Create(ctx, admin, vpTenantA, user, "tk_tickets",
		map[string]any{"subject": "Hẹn trước với khách", "priority": "Urgent", "status": "Open", "due_date": typedDeadline})
	if err != nil {
		t.Fatalf("create ticket with a typed deadline: %v", err)
	}
	after := time.Now().UTC()

	st.drain(t)

	readDue := func(id string) time.Time {
		t.Helper()
		rec, err := st.records.Get(ctx, admin, vpTenantA, "tk_tickets", id, nil, nil)
		if err != nil {
			t.Fatalf("Get ticket: %v", err)
		}
		raw, _ := rec.Data["due_date"].(string)
		if raw == "" {
			t.Fatalf("due_date is empty after install+create — the pack's SLA timers have nothing to anchor on (data=%+v)", rec.Data)
		}
		// The PERSISTED shape is the MySQL literal in UTC, not the RFC3339 the action wrote:
		// parsing it here is itself the assertion that canonicalization ran.
		ts, err := time.Parse("2006-01-02 15:04:05", raw)
		if err != nil {
			t.Fatalf("due_date persisted as %q, not the canonical MySQL datetime literal: %v", raw, err)
		}
		return ts.UTC()
	}

	// The window: the action stamps at fire time, somewhere between `before` and `after`. Walking
	// the calendar from both ends brackets the only legitimate answers.
	assertBusinessHours := func(what string, got time.Time, hours int) {
		t.Helper()
		lo, err := sched.AddBusinessDuration(before, time.Duration(hours)*time.Hour)
		if err != nil {
			t.Fatalf("%s: AddBusinessDuration(before): %v", what, err)
		}
		hi, err := sched.AddBusinessDuration(after, time.Duration(hours)*time.Hour)
		if err != nil {
			t.Fatalf("%s: AddBusinessDuration(after): %v", what, err)
		}
		// `after` is sampled before the dispatcher worker runs the action, so the stamp is
		// legitimately LATER than AddBusinessDuration(after, …) by however long the worker took.
		// The slack absorbs that (CI runs ~4-5x slower than local) and costs no discrimination:
		// the wall-clock answer this test exists to catch is HOURS away, not seconds.
		const workerSlack = 2 * time.Minute
		if got.Before(lo.UTC().Truncate(time.Second)) || got.After(hi.UTC().Add(workerSlack)) {
			wall := before.Add(time.Duration(hours) * time.Hour)
			t.Errorf("%s: due_date = %s, want %s..%s (%d business hours). Wall-clock would give %s — if that is what you see, the calendar was not consulted.",
				what, got.Format(time.RFC3339), lo.UTC().Format(time.RFC3339), hi.UTC().Format(time.RFC3339), hours, wall.Format(time.RFC3339))
		}
	}

	// Urgent = the fastest tier. noPriority = the fall-through tier. Both read their expected
	// amount off the pack so the test does not hard-code a promise the pack can change.
	amounts := slaTierAmounts(t)
	assertBusinessHours("Urgent ticket", readDue(urgentID), amounts["Urgent"])
	assertBusinessHours("ticket with no priority", readDue(noPriorityID), amounts[slaFallThroughTier])

	// The typed deadline is untouched — `due_date isnull` is what keeps the machine off it.
	gotTyped := readDue(typedID)
	wantTyped, _ := time.Parse(time.RFC3339, typedDeadline)
	if !gotTyped.Equal(wantTyped.UTC()) {
		t.Errorf("a deadline the agent typed was overwritten: due_date = %s, want %s",
			gotTyped.Format(time.RFC3339), wantTyped.UTC().Format(time.RFC3339))
	}
}

// slaSeedInstaller inserts the install user every SLA test creates tickets as.
func slaSeedInstaller(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@tk-sla-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}
}

// slaWorkflowIDs maps the tenant's workflow NAME → id, so a parked job (which names its rule by id)
// can be matched to the pack rule that parked it.
func (s *slaStack) slaWorkflowIDs(t *testing.T) map[string]string {
	t.Helper()
	var rows []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	if err := s.sdb.Select(&rows, "SELECT id, name FROM sys_workflows WHERE tenant_id = ? AND deleted_at IS NULL", vpTenantA); err != nil {
		t.Fatalf("select sys_workflows: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Name] = r.ID
	}
	return out
}

// TestTicketingSLAPack_TimersParkOnTheStampedDeadline (DAILY-QUEUE #84) walks the WHOLE chain the
// pack exists for: install → an agent raises a ticket with no deadline → set_deadline stamps
// due_date → BOTH due_date timers park a wake-up anchored on that stamped value.
//
// Before this, each link had a test of its own (the stamp here, the read-back ordering in
// TestDispatcherSchedulesTimersAfterActions) and the JOIN between them was a paragraph: if
// scheduleTimers / resolveAnchor regressed to the pre-action event snapshot — where due_date is
// still empty — the pack's reminder and escalation would silently never park while every gate
// stayed green. Mutation-proved: pointing resolveAnchor at evt.Data turns this test red.
func TestTicketingSLAPack_TimersParkOnTheStampedDeadline(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	ctx := context.Background()
	slaSeedInstaller(t, gdb)
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, pack); err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}

	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	id, err := st.records.Create(ctx, admin, vpTenantA, vpSeedUserID, "tk_tickets",
		map[string]any{"subject": "Máy chủ không phản hồi", "priority": "Urgent", "status": "Open"})
	if err != nil {
		t.Fatalf("create Urgent ticket: %v", err)
	}
	st.drain(t)

	rec, err := st.records.Get(ctx, admin, vpTenantA, "tk_tickets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get ticket: %v", err)
	}
	stamped, _ := rec.Data["due_date"].(string)
	due, err := time.Parse("2006-01-02 15:04:05", stamped)
	if err != nil {
		t.Fatalf("due_date = %q after install+create — nothing stamped, so nothing can anchor: %v", stamped, err)
	}

	var jobsRows []struct {
		Payload []byte    `db:"payload"`
		RunAt   time.Time `db:"run_at"`
	}
	if err := st.sdb.Select(&jobsRows, "SELECT payload, run_at FROM sys_jobs WHERE tenant_id = ? AND job_type = ?",
		vpTenantA, services.WorkflowTimerJobType); err != nil {
		t.Fatalf("select sys_jobs: %v", err)
	}
	type parked struct {
		dateValue string
		runAt     time.Time
	}
	byRule := map[string]parked{}
	for _, j := range jobsRows {
		var p struct {
			WorkflowID string `json:"workflowId"`
			RecordID   string `json:"recordId"`
			DateValue  string `json:"dateValue"`
		}
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			t.Fatalf("job payload: %v", err)
		}
		if p.RecordID == id {
			byRule[p.WorkflowID] = parked{dateValue: p.DateValue, runAt: j.RunAt.UTC()}
		}
	}

	// Read the timers' offsets off the pack, so the test follows the pack instead of restating it.
	ids := st.slaWorkflowIDs(t)
	var timers int
	for _, pw := range pack.Workflows {
		if pw.Trigger != "date_offset" || pw.Condition == nil || pw.Condition.Timer == nil || pw.Condition.Timer.DateField != "due_date" {
			continue
		}
		timers++
		off := time.Duration(pw.Condition.Timer.Offset.Amount) * time.Hour
		if pw.Condition.Timer.Offset.Unit != "hours" {
			t.Fatalf("%q: offset unit %q — this test only knows how to compute `hours`", pw.Name, pw.Condition.Timer.Offset.Unit)
		}
		want := due.Add(off)
		if pw.Condition.Timer.Direction == "before" {
			want = due.Add(-off)
		}
		got, ok := byRule[ids[pw.Name]]
		if !ok {
			t.Errorf("%q parked NO wake-up for a ticket whose due_date the pack stamped — the SLA timer is decorative again", pw.Name)
			continue
		}
		if got.dateValue != stamped {
			t.Errorf("%q anchored on %q, want the stamped due_date %q", pw.Name, got.dateValue, stamped)
		}
		if !got.runAt.Equal(want) {
			t.Errorf("%q parks at %s, want %s (due_date %s %s %s)", pw.Name, got.runAt.Format(time.RFC3339),
				want.Format(time.RFC3339), stamped, pw.Condition.Timer.Direction, off)
		}
	}
	if timers != 2 {
		t.Fatalf("the pack has %d due_date timers, want 2 (the reminder and the escalation)", timers)
	}
}

// TestTicketingSLAPack_NoCalendarIsSaidAtInstallAndInRunHistory (DAILY-QUEUE #82) is the join the
// unit tests cannot see: the SAME resolver the engine computes deadlines with answers "no default
// calendar", the install result says so, and the run row written to sys_workflow_runs — the JSON
// the run-history screen renders — carries the wall-clock warning on a run that is still `success`.
// Before #82 the install result was silent and actions_result read a bare {ok:true}.
func TestTicketingSLAPack_NoCalendarIsSaidAtInstallAndInRunHistory(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	ctx := context.Background()
	slaSeedInstaller(t, gdb)
	// Deliberately NO SeedDefaultBusinessCalendar: this is the tenant #82 measured (every calendar
	// tombstoned) and the one an admin reaches by un-defaulting or deleting the default calendar.

	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	installed, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}
	warned := calendarWarnings(installed)
	if len(warned) != 1 {
		t.Fatalf("install warnings = %q; want exactly one saying the SLA deadlines will be wall-clock", installed.Warnings)
	}
	for _, wf := range ticketingDeadlineRules(t, "") { // creation AND recompute tiers walk the calendar
		if !strings.Contains(warned[0], wf.Name) {
			t.Errorf("install warning %q does not name the business_hours rule %q", warned[0], wf.Name)
		}
	}

	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	id, err := st.records.Create(ctx, admin, vpTenantA, vpSeedUserID, "tk_tickets",
		map[string]any{"subject": "Máy chủ không phản hồi", "priority": "Urgent", "status": "Open"})
	if err != nil {
		t.Fatalf("create Urgent ticket: %v", err)
	}
	st.drain(t)

	var runs []struct {
		Status        string  `db:"status"`
		ActionsResult *[]byte `db:"actions_result"`
	}
	if err := st.sdb.Select(&runs,
		"SELECT status, actions_result FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ? AND trigger_type = ?",
		vpTenantA, id, string(workflow.TriggerAfterCreate)); err != nil {
		t.Fatalf("select sys_workflow_runs: %v", err)
	}
	var stamped int
	for _, r := range runs {
		if r.Status != string(workflow.RunSuccess) || r.ActionsResult == nil {
			continue
		}
		var results []workflow.ActionResult
		if err := json.Unmarshal(*r.ActionsResult, &results); err != nil {
			t.Fatalf("actions_result %s: %v", *r.ActionsResult, err)
		}
		for _, a := range results {
			if a.Type != workflow.ActionSetDeadline {
				continue
			}
			stamped++
			if !strings.Contains(a.Warning, "giờ treo tường") {
				t.Errorf("run history for the Urgent tier records %s — a wall-clock deadline reported as a plain success", *r.ActionsResult)
			}
		}
	}
	if stamped != 1 {
		t.Fatalf("found %d successful set_deadline runs for the ticket (runs=%d), want 1 — the fallback still stamps", stamped, len(runs))
	}
}
