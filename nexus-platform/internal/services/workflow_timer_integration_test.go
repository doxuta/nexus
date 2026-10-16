//go:build integration

// TIMER-1 — date_offset workflow trigger, exercised END-TO-END over a freshly-migrated throwaway
// MySQL DB (o3aSetup runs ALL migrations incl. 000048 trigger_event ENUM append via the REAL
// golang-migrate CLI). What this proves beyond the unit tests (all fakes):
//
//	RecordService.Create                     (after-commit Emit)
//	  → workflow.AsyncDispatcher.process     (async worker)
//	    → CachedLoader.ListActive(date_offset)  (real sys_workflows query — ENUM value live)
//	    → services.WorkflowTimerScheduler    (real calmeta default-calendar lookup +
//	                                          calendar.AddBusinessDuration + jobs.SQLStore.Enqueue)
//	      → sys_jobs row: job_type=workflow_timer, tenant-pinned, run_at = created_at + 1
//	        BUSINESS hour (holiday-aware — NOT wall-clock)
//	then the wake-up:
//	  services.NewWorkflowTimerJobHandler    (fire-time re-evaluation)
//	    → workflow.AsyncDispatcher.FireWorkflow → field_update applied on the record
//	    → sys_workflow_runs row (trigger_type=date_offset, status=success)
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run TimerE2E
package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// timerStack is the fully-wired timer composition (mirrors cmd/server/main.go).
type timerStack struct {
	*wfStack
	jobsStore *jobs.SQLStore
	calRepo   *calmeta.SQLRepository
	wfRepo    *workflow.SQLRepository
	dynRepo   *dynrepo.Repository
}

// timerWire stands up wfWire's stack PLUS the TIMER-1 pieces: the jobs store, the calendar repo,
// the timer scheduler attached to the dispatcher, and the run recorder (the handler's fire must
// leave a run row).
func timerWire(t *testing.T) *timerStack {
	t.Helper()
	gdb, sdb := o3aSetup(t)

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	wfMutator := NewWorkflowRecordMutator(dynRepo).WithCatalog(mc)
	jobsStore := jobs.NewSQLStore(sdb, 5)
	calRepo := calmeta.NewSQLRepository(sdb)

	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 64, Workers: 1},
		wfCache, wfMutator, wfRepo, nil,
	).WithRunRecorder(wfRepo).
		WithTimerScheduler(NewWorkflowTimerScheduler(jobsStore, calRepo, dynRepo))

	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithCalendarChecker(calRepo)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(dispatcher)

	return &timerStack{
		wfStack: &wfStack{
			sdb: sdb, entitySvc: entitySvc, recordSvc: recordSvc,
			workflowSvc: workflowSvc, dispatcher: dispatcher, cache: wfCache,
		},
		jobsStore: jobsStore, calRepo: calRepo, wfRepo: wfRepo, dynRepo: dynRepo,
	}
}

// timerJobRow is the sys_jobs projection the assertions read.
type timerJobRow struct {
	ID       string    `db:"id"`
	TenantID string    `db:"tenant_id"`
	Payload  []byte    `db:"payload"`
	RunAt    time.Time `db:"run_at"`
	Status   string    `db:"status"`
}

func (s *timerStack) timerJobs(t *testing.T, tenant string) []timerJobRow {
	t.Helper()
	var rows []timerJobRow
	if err := s.sdb.Select(&rows, `
		SELECT id, tenant_id, payload, run_at, status FROM sys_jobs
		WHERE job_type = ? AND tenant_id = ?`, WorkflowTimerJobType, tenant); err != nil {
		t.Fatalf("select sys_jobs: %v", err)
	}
	return rows
}

func TestWorkflowTimerE2E_BusinessHourAfterCreatedAt(t *testing.T) {
	st := timerWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000101"

	// Default calendar: 24/7 UTC but with a 4-day HOLIDAY WALL around now — a wall-clock +1h
	// would land within the hour; the business-hours walk must skip at least a full day. That
	// asymmetry is the proof the schedule (not the fallback) computed run_at. The wall spans
	// yesterday..now+2d so the assertion is immune to the test DSN's loc=Local round-trip skew
	// on created_at (the calendar sees the same skewed instant the production path does).
	hours := `{"mon":[{"start":"00:00","end":"24:00"}],"tue":[{"start":"00:00","end":"24:00"}],
		"wed":[{"start":"00:00","end":"24:00"}],"thu":[{"start":"00:00","end":"24:00"}],
		"fri":[{"start":"00:00","end":"24:00"}],"sat":[{"start":"00:00","end":"24:00"}],
		"sun":[{"start":"00:00","end":"24:00"}]}`
	wall := make([]string, 0, 4)
	for d := -1; d <= 2; d++ {
		wall = append(wall, time.Now().UTC().Add(time.Duration(d)*24*time.Hour).Format("2006-01-02"))
	}
	holidays, _ := json.Marshal(wall)
	if _, err := st.calRepo.Create(ctx, calmeta.CreateInput{
		TenantID: tenant, Name: "Default", Timezone: "UTC",
		Hours: []byte(hours), Holidays: holidays, IsDefault: true, CreatedBy: user,
	}); err != nil {
		t.Fatalf("create calendar: %v", err)
	}

	st.createEntity(t, ctx, tenant, "contracts", []FieldInput{
		{Name: "stage", Label: "Stage", FieldType: "text"},
		{Name: "expiry", Label: "Expiry", FieldType: "date"},
	})

	// Rule: 1 BUSINESS hour after created_at → set stage = "due" (registered through the
	// production admin path: validation + ENUM write + cache invalidation).
	wfRec, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "sla-1bh",
		EntityName: "contracts",
		Trigger:    "date_offset",
		Timer: &WorkflowTimerInput{
			DateField: "created_at",
			Offset:    WorkflowTimerOffsetInput{Amount: 1, Unit: "business_hours"},
			Direction: "after",
		},
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "due"}},
		ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow date_offset: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "contracts",
		map[string]any{"stage": "new"})
	if err != nil {
		t.Fatalf("Create contract: %v", err)
	}
	st.drain(t) // async engine parks the wake-up

	// --- the parked row ---
	rows := st.timerJobs(t, tenant)
	if len(rows) != 1 {
		t.Fatalf("sys_jobs workflow_timer rows = %d, want 1", len(rows))
	}
	job := rows[0]
	if job.Status != jobs.StatusQueued {
		t.Fatalf("job status = %q, want queued", job.Status)
	}
	var p workflowTimerJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.WorkflowID != wfRec.ID || p.EntityName != "contracts" || p.RecordID != id || p.DateValue == "" {
		t.Fatalf("payload = %+v, want workflow %s / record %s", p, wfRec.ID, id)
	}

	// run_at must equal AddBusinessDuration(created_at, 1h) on the SAME calendar — computed here
	// through the calendar package directly (independent of the production ComputeFireAt path).
	rec, err := st.dynRepo.GetRecord(ctx, "contracts", tenant, id, dynrepo.AdminScope())
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	sched, err := calendar.FromJSON("UTC", []byte(hours), holidays)
	if err != nil {
		t.Fatalf("calendar.FromJSON: %v", err)
	}
	want, err := sched.AddBusinessDuration(rec.CreatedAt, time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	want = want.UTC().Truncate(time.Second)
	if !job.RunAt.UTC().Equal(want) {
		t.Fatalf("run_at = %v, want %v (business-hours math)", job.RunAt.UTC(), want)
	}
	// Holiday proof: the deadline is ≥ 1 day out; a wall-clock fallback would be +1h.
	if job.RunAt.Sub(rec.CreatedAt) < 24*time.Hour {
		t.Fatalf("run_at only %v after created_at — calendar (holidays) was NOT consulted", job.RunAt.Sub(rec.CreatedAt))
	}

	// --- the wake-up fires (handler invoked directly; the queue's run_at gate is JOBS-1-tested) ---
	h := NewWorkflowTimerJobHandler(st.wfRepo, st.dynRepo, st.dispatcher, nil, nil)
	if err := h(ctx, jobs.Job{ID: job.ID, TenantID: job.TenantID, Type: WorkflowTimerJobType, Payload: job.Payload}); err != nil {
		t.Fatalf("workflow_timer handler: %v", err)
	}

	after, err := st.dynRepo.GetRecord(ctx, "contracts", tenant, id, dynrepo.AdminScope())
	if err != nil {
		t.Fatalf("GetRecord after fire: %v", err)
	}
	if got, _ := after.Data["stage"].(string); got != "due" {
		t.Fatalf("stage = %q, want \"due\" (field_update at fire time)", got)
	}

	// Run history: one date_offset run row, success.
	var runs int
	if err := st.sdb.Get(&runs, `
		SELECT COUNT(*) FROM sys_workflow_runs
		WHERE tenant_id = ? AND workflow_id = ? AND trigger_type = 'date_offset' AND status = 'success'`,
		tenant, wfRec.ID); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("date_offset success runs = %d, want 1", runs)
	}
}

func TestWorkflowTimerE2E_UnsetDateSchedulesNothing(t *testing.T) {
	st := timerWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000102"

	st.createEntity(t, ctx, tenant, "renewals", []FieldInput{
		{Name: "stage", Label: "Stage", FieldType: "text"},
		{Name: "renew_on", Label: "Renew on", FieldType: "date"},
	})
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "remind-3d",
		EntityName: "renewals",
		Trigger:    "date_offset",
		Timer: &WorkflowTimerInput{
			DateField: "renew_on",
			Offset:    WorkflowTimerOffsetInput{Amount: 3, Unit: "days"},
			Direction: "before",
		},
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "remind"}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	// Record WITHOUT the date field → no wake-up may be parked.
	if _, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "renewals",
		map[string]any{"stage": "new"}); err != nil {
		t.Fatalf("Create renewal: %v", err)
	}
	st.drain(t)

	if rows := st.timerJobs(t, tenant); len(rows) != 0 {
		t.Fatalf("sys_jobs workflow_timer rows = %d, want 0 (date unset)", len(rows))
	}
}

// TestWorkflowTimerE2E_AnchorFollowsActionWrittenDate — DAILY-QUEUE #6 (timer-anchor-current-record)
// over real MySQL. The deadline is written by an after_update rule (status → Pending sets due_date
// through field_update), NOT by the user, and the pack-style "2 hours BEFORE due_date" reminder
// must park from it. The after-commit event Data is the FULL after-image, but it is a snapshot
// taken BEFORE this dispatch's action loop: the ticket has no due_date yet at that instant, and
// field_update is deliberately event-less, so before the anchor read-back the producer parked
// nothing and the reminder never fired:
//
//	RecordService.Update {status: Pending}          (after-commit Emit, after-image: due_date absent)
//	  → dispatcher.process: after_update rule       → field_update due_date = T (row on disk)
//	  → dispatcher.scheduleTimers: date_offset rule → WorkflowTimerScheduler reads the RECORD
//	    → sys_jobs workflow_timer, run_at = T − 2h, payload.dateValue = persisted due_date
//	then the wake-up: handler premise (current due_date == dateValue) holds → owner notified.
func TestWorkflowTimerE2E_AnchorFollowsActionWrittenDate(t *testing.T) {
	st := timerWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000103"

	st.createEntity(t, ctx, tenant, "tickets", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "due_date", Label: "Due", FieldType: "datetime"},
	})

	// The pack's reminder: 2 hours BEFORE due_date → notify the owner (ticketing "Nhắc hạn SLA").
	remind, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "Nhắc hạn SLA", EntityName: "tickets", Trigger: "date_offset",
		Timer: &WorkflowTimerInput{
			DateField: "due_date",
			Offset:    WorkflowTimerOffsetInput{Amount: 2, Unit: "hours"},
			Direction: "before",
		},
		Actions: []WorkflowActionInput{{Type: "in_app_notification", UserID: "$owner", Title: "Ticket sắp tới hạn SLA", Message: "Ticket của bạn sắp tới hạn xử lý."}},
		ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow reminder: %v", err)
	}
	// The machine-set deadline: status = Pending → field_update due_date = T (RFC3339 on the wire;
	// the mutator's catalog validation persists the UTC space-form).
	const dueRFC = "2026-12-01T10:00:00Z"
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "Tự đặt hạn", EntityName: "tickets", Trigger: "after_update",
		Match:   "and",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "Pending"}},
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "due_date", Value: dueRFC}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow set-deadline: %v", err)
	}

	// A ticket with no deadline; the user only flips status. Neither write carries due_date.
	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "tickets", map[string]any{"status": "Open"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "tickets", id, map[string]any{"status": "Pending"}, nil); err != nil {
		t.Fatalf("Update ticket: %v", err)
	}
	st.drain(t) // the after_update rule writes due_date, THEN the same dispatch parks the wake-up

	rec, err := st.dynRepo.GetRecord(ctx, "tickets", tenant, id, dynrepo.AdminScope())
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	const duePersisted = "2026-12-01 10:00:00"
	if got, _ := rec.Data["due_date"].(string); got != duePersisted {
		t.Fatalf("due_date = %q, want %q (written by the after_update rule)", got, duePersisted)
	}

	rows := st.timerJobs(t, tenant)
	if len(rows) != 1 {
		t.Fatalf("sys_jobs workflow_timer rows = %d, want 1 (reminder parked from the action-written due_date)", len(rows))
	}
	job := rows[0]
	var p workflowTimerJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.WorkflowID != remind.ID || p.RecordID != id || p.DateValue != duePersisted {
		t.Fatalf("payload = %+v, want workflow %s / record %s / dateValue %q", p, remind.ID, id, duePersisted)
	}
	want := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC) // due_date − 2h
	if !job.RunAt.UTC().Equal(want) {
		t.Fatalf("run_at = %v, want %v", job.RunAt.UTC(), want)
	}

	// The wake-up fires: the handler's fire-time premise (current due_date == parked dateValue)
	// holds by construction — both strings come from the same DB column — and the owner gets the bell.
	h := NewWorkflowTimerJobHandler(st.wfRepo, st.dynRepo, st.dispatcher, nil, nil)
	if err := h(ctx, jobs.Job{ID: job.ID, TenantID: job.TenantID, Type: WorkflowTimerJobType, Payload: job.Payload}); err != nil {
		t.Fatalf("workflow_timer handler: %v", err)
	}
	if n := st.notifCount(t, tenant, user); n != 1 {
		t.Fatalf("owner notifications = %d, want 1 (Ticket sắp tới hạn SLA)", n)
	}
}

// TestWorkflowTimerE2E_ResendOfFailedTimerRefused — review of #32 (HIGH): "Gửi lại" must never
// run a workflow_timer again. The enqueue dedup probe (HasJobWithPayload) deliberately ignores
// FAILED rows so a later edit can re-park a wake-up whose budget ran out; that re-parked twin fires
// and ends done. Requeueing the original failed row afterwards would run every action of the rule a
// second time — the re-fire TIMER-1 closed. Built with the real producer and handler over MySQL:
// failed T → an unrelated edit re-parks T' (same payload) → T' fires (one reminder) → the admin
// presses "Gửi lại" on T.
func TestWorkflowTimerE2E_ResendOfFailedTimerRefused(t *testing.T) {
	st := timerWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000104"

	st.createEntity(t, ctx, tenant, "tickets", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "due_date", Label: "Due", FieldType: "datetime"},
	})
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "Nhắc hạn SLA", EntityName: "tickets", Trigger: "date_offset",
		Timer: &WorkflowTimerInput{
			DateField: "due_date",
			Offset:    WorkflowTimerOffsetInput{Amount: 2, Unit: "hours"},
			Direction: "before",
		},
		Actions: []WorkflowActionInput{{Type: "in_app_notification", UserID: "$owner", Title: "Ticket sắp tới hạn SLA", Message: "Ticket của bạn sắp tới hạn xử lý."}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "tickets",
		map[string]any{"status": "Open", "due_date": "2026-12-01T10:00:00Z"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	// Wait for the async engine to park the wake-up WITHOUT draining (drain closes the dispatcher,
	// and the re-park below needs it running).
	var rows []timerJobRow
	for deadline := time.Now().Add(5 * time.Second); len(rows) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		rows = st.timerJobs(t, tenant)
	}
	if len(rows) != 1 {
		t.Fatalf("workflow_timer rows = %d, want 1", len(rows))
	}
	first := rows[0]

	// The wake-up exhausts its budget (a DB outage past the ladder, a reaper-failed lease).
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET status = ?, attempts = max_attempts, last_error = ?,
		finished_at = NOW(3) WHERE id = ?`, jobs.StatusFailed, "dynrepo: connection refused", first.ID); err != nil {
		t.Fatalf("fail the timer: %v", err)
	}

	// An unrelated edit, deadline unchanged: the probe skips the failed row and re-parks the SAME
	// wake-up — by design, that is how a failed timer recovers.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "tickets", id, map[string]any{"status": "Pending"}, nil); err != nil {
		t.Fatalf("Update ticket: %v", err)
	}
	st.drain(t)
	rows = st.timerJobs(t, tenant)
	if len(rows) != 2 {
		t.Fatalf("workflow_timer rows = %d, want 2 (the failed one + its re-parked twin)", len(rows))
	}
	twin := rows[0]
	if twin.ID == first.ID {
		twin = rows[1]
	}
	var a, b workflowTimerJobPayload
	if err := json.Unmarshal(first.Payload, &a); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := json.Unmarshal(twin.Payload, &b); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if twin.Status != jobs.StatusQueued || a != b {
		t.Fatalf("twin = %s %+v; want queued with the failed timer's payload %+v", twin.Status, b, a)
	}

	// The twin fires: the owner gets the one reminder this deadline owes them; the queue marks it done.
	h := NewWorkflowTimerJobHandler(st.wfRepo, st.dynRepo, st.dispatcher, nil, nil)
	if err := h(ctx, jobs.Job{ID: twin.ID, TenantID: twin.TenantID, Type: WorkflowTimerJobType, Payload: twin.Payload}); err != nil {
		t.Fatalf("workflow_timer handler: %v", err)
	}
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET status = ?, finished_at = NOW(3) WHERE id = ?`, jobs.StatusDone, twin.ID); err != nil {
		t.Fatalf("finish the twin: %v", err)
	}
	if n := st.notifCount(t, tenant, user); n != 1 {
		t.Fatalf("owner notifications = %d, want 1", n)
	}

	// "Gửi lại" on the old failed row: refused, and nothing is back in the queue to fire again.
	_, err = NewBackgroundJobService(st.jobsStore).Retry(ctx, tenant, user, first.ID)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "JOB_NOT_RETRYABLE" {
		t.Fatalf("resend of a failed timer whose twin already fired: err = %v; want JOB_NOT_RETRYABLE", err)
	}
	for _, r := range st.timerJobs(t, tenant) {
		if want := map[string]string{first.ID: jobs.StatusFailed, twin.ID: jobs.StatusDone}[r.ID]; r.Status != want {
			t.Fatalf("timer %s is %s after the refused resend; want %s", r.ID, r.Status, want)
		}
	}
	if n := st.notifCount(t, tenant, user); n != 1 {
		t.Fatalf("owner notifications = %d after the refused resend, want 1", n)
	}
}
