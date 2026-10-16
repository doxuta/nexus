package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AsyncDispatcher is the concrete Engine. Emit pushes the record event onto a buffered channel and
// returns immediately; a pool of worker goroutines drains it, and for each event loads
// the active workflows of the mutated entity (through the cached loader), evaluates each rule's
// condition, and runs its actions.
//
// Fail-open guarantees (spec §EPIC 4; same contract as the A7 audit dispatcher):
//   - A full buffer makes Emit DROP the event with a zap warning — it NEVER blocks the calling
//     record mutation (already committed) and NEVER panics.
//   - After Shutdown, further Emits are dropped with a log line (no send on a closed channel).
//   - A load / condition / action error is logged; it never propagates to the caller.
//   - A panic in processing is recovered so one bad event cannot shrink the worker pool.
//
// Ordering (DAILY-QUEUE #444): each worker owns ONE channel and Emit picks the channel by
// hash(tenant, recordID), so the events of one record always run on one worker, in emit order —
// an after_update can never overtake the after_create of the same record. Records on different
// shards still run in parallel.
type AsyncDispatcher struct {
	chs    []chan Event
	loader WorkflowLoader
	mut    RecordMutator
	notif  NotificationWriter
	// email/webhook back the M2 email_alert / outbound_webhook actions. They are optional:
	// a nil sender makes its action error (logged fail-open) without crashing. Injected via
	// WithEmailSender / WithWebhookSender so the M1 constructor signature stays unchanged.
	email   EmailSender
	webhook WebhookSender
	// templates resolves email placeholders the record cannot answer (specs/059). Optional via
	// WithTemplateResolver; nil renders from the record's own data only.
	templates TemplateResolver
	// webhookQueue makes outbound_webhook DURABLE (DAILY-QUEUE #26): set, the action parks a
	// sys_jobs row instead of posting inline. Optional via WithWebhookQueue; nil keeps the inline
	// webhook sender (queue disabled). Shared with the scheduler through deps().
	webhookQueue WebhookQueue
	// creds resolves an outbound_webhook's named-credential key at send time (specs/050 S2b).
	// Optional via WithCredentialResolver; nil makes a vault action fail without sending.
	creds CredentialResolver
	// runs records every (event × workflow) outcome into sys_workflow_runs (W6 run history).
	// Optional: nil disables recording (legacy tests). Recording happens HERE — inside the
	// async worker, after the triggering mutation already committed — so it adds zero sync
	// latency; an insert failure is logged and processing continues (fail-open preserved).
	runs RunRecorder
	// timers parks TIMER-1 date_offset wake-ups (a 'workflow_timer' sys_jobs row at the
	// computed fire instant) for the entity's date_offset rules on after_create/after_update.
	// Optional via WithTimerScheduler: nil = timer scheduling disabled (jobs queue off / legacy
	// tests) — date_offset rules then simply never park a wake-up. Fail-open like every other
	// dispatcher dependency.
	timers TimerScheduler
	// assign resolves the ASSIGN-1 round-robin next-assignee for the assign action. Optional via
	// WithAssignmentResolver: a nil resolver makes the assign action error (logged fail-open)
	// rather than crashing. Shared with the scheduler through deps().
	assign AssignmentResolver
	// recipients expands a notification's $role: / $team: recipient (queue #281).
	recipients RecipientResolver
	// creator backs the create_record action (CREATE-RECORD): insert a NEW record through the full
	// RecordService.Create path. Optional via WithRecordCreator: a nil creator makes the
	// create_record action error (logged fail-open). maxDepth bounds create_record recursion (a
	// created record re-triggering a create_record rule): the action is skipped once the event's
	// Depth reaches it. Both are shared with the scheduler through deps() (wire WithRecordCreator
	// BEFORE NewScheduler, which snapshots deps()).
	creator RecordCreator
	// sched resolves the tenant working calendar a business_hours set_deadline offset needs
	// (DAILY-QUEUE #10). Optional via WithScheduleResolver — but UNLIKE the other optional deps,
	// a nil here makes that action ERROR instead of degrading: wall-clock under a "giờ làm việc"
	// label is indistinguishable from the real answer, so silence is the one unacceptable outcome.
	// Shared with the scheduler through deps() (wire it BEFORE NewScheduler).
	sched ScheduleResolver
	// approvals backs start_approval (specs/043 S9b); optional via WithApprovalStarter and shared
	// with the scheduler through deps() (wire it BEFORE NewScheduler).
	approvals ApprovalStarter
	maxDepth  int
	logger    *zap.Logger

	wg sync.WaitGroup

	// closeOnce guards the channel close so Shutdown is idempotent; closed is read by Emit under
	// mu to avoid sending on a closed channel.
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// DispatcherConfig configures an AsyncDispatcher. Buffer/Workers are clamped to a sane minimum so
// a misconfigured zero can never produce a deadlocked or unbuffered dispatcher.
type DispatcherConfig struct {
	Buffer  int
	Workers int
}

// NewAsyncDispatcher builds and starts a dispatcher: it allocates the buffered channel and
// launches Workers goroutines. The caller must call Shutdown for a graceful drain. loader/mut/
// notif may be nil — a nil loader makes every event a no-op (engine disabled), a nil mut/notif
// makes the corresponding action error (logged, fail-open) without crashing. logger may be nil.
func NewAsyncDispatcher(cfg DispatcherConfig, loader WorkflowLoader, mut RecordMutator, notif NotificationWriter, logger *zap.Logger) *AsyncDispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	buffer := cfg.Buffer
	if buffer < 1 {
		buffer = 1
	}
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}

	// The buffer is split across the shards (rounded up), so the total capacity stays ~Buffer.
	perShard := (buffer + workers - 1) / workers
	chs := make([]chan Event, workers)
	for i := range chs {
		chs[i] = make(chan Event, perShard)
	}
	d := &AsyncDispatcher{
		chs:    chs,
		loader: loader,
		mut:    mut,
		notif:  notif,
		logger: logger,
	}

	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker(chs[i])
	}
	return d
}

// WithEmailSender injects the M2 email_alert transport. Chainable; a nil sender leaves the
// email_alert action failing-open (logged) rather than crashing. Call before Emit traffic.
func (d *AsyncDispatcher) WithEmailSender(s EmailSender) *AsyncDispatcher {
	d.email = s
	return d
}

// WithTemplateResolver injects the specs/059 resolver an email_alert asks for placeholders the
// triggering record cannot answer ({{relation.field}}, lookup fields). Chainable; nil keeps the
// record-only render. Shared with the scheduler and the timer handler through deps() — wire it
// BEFORE NewScheduler, which snapshots deps().
func (d *AsyncDispatcher) WithTemplateResolver(r TemplateResolver) *AsyncDispatcher {
	d.templates = r
	return d
}

// WithWebhookSender injects the M2 outbound_webhook transport. Chainable; a nil sender leaves the
// outbound_webhook action failing-open (logged) rather than crashing. Call before Emit traffic.
func (d *AsyncDispatcher) WithWebhookSender(s WebhookSender) *AsyncDispatcher {
	d.webhook = s
	return d
}

// WithWebhookQueue makes the outbound_webhook action durable: each delivery is parked on the job
// queue and delivered by a queue worker that owns retry/backoff, so it survives a receiver outage
// and a Nexus restart. Chainable; nil keeps the inline WebhookSender. Pass a nil INTERFACE, not a
// typed-nil pointer, to disable it. Call before Emit traffic AND before NewScheduler (the
// scheduler snapshots deps()).
func (d *AsyncDispatcher) WithWebhookQueue(q WebhookQueue) *AsyncDispatcher {
	d.webhookQueue = q
	return d
}

// WithCredentialResolver injects the named-credential vault an outbound_webhook action with a
// credentialId reads its key from at send time (specs/050 S2b). Chainable. Call before Emit
// traffic AND before NewScheduler (the scheduler snapshots deps()).
func (d *AsyncDispatcher) WithCredentialResolver(r CredentialResolver) *AsyncDispatcher {
	d.creds = r
	return d
}

// WithRunRecorder injects the W6 run-history sink (sys_workflow_runs). Chainable; nil disables
// recording. Call before Emit traffic AND before NewScheduler (the scheduler snapshots it).
func (d *AsyncDispatcher) WithRunRecorder(r RunRecorder) *AsyncDispatcher {
	d.runs = r
	return d
}

// TimerScheduler is the dispatcher's consumer-side view of the TIMER-1 scheduling seam: given a
// date_offset rule and the after-commit event of its entity, compute the fire instant from the
// record's date field and park a durable wake-up (a 'workflow_timer' sys_jobs row). Implemented
// by services.WorkflowTimerScheduler (which owns the calendar resolution + jobs enqueue I/O);
// declared here so the workflow package stays free of jobs/calmeta dependencies.
type TimerScheduler interface {
	// ScheduleTimer parks the wake-up for (wf, evt). A record whose date field is unset is a
	// silent no-op (nil error); a real fault (DB, marshal) is returned for fail-open logging.
	ScheduleTimer(ctx context.Context, wf Workflow, evt Event) error
}

// WithTimerScheduler injects the TIMER-1 scheduling seam. Chainable; nil leaves date_offset
// scheduling disabled (documented: with the jobs queue OFF no scheduler is wired, so a
// date_offset rule never parks a wake-up nobody would run). Call before Emit traffic.
func (d *AsyncDispatcher) WithTimerScheduler(t TimerScheduler) *AsyncDispatcher {
	d.timers = t
	return d
}

// WithAssignmentResolver injects the ASSIGN-1 round-robin resolver backing the assign action.
// Chainable; nil leaves the assign action failing-open (logged) rather than crashing. Shared with
// the scheduler via deps() (a scheduled rule with an assign action advances the cursor per matched
// record). Call before Emit traffic AND before NewScheduler (the scheduler snapshots deps()).
func (d *AsyncDispatcher) WithAssignmentResolver(r AssignmentResolver) *AsyncDispatcher {
	d.assign = r
	return d
}

// WithRecipientResolver injects the role / team expansion behind a notification's $role: / $team:
// recipient (queue #281). Chainable; nil makes such a notification fail the run with a clear error.
// Call before Emit traffic AND before NewScheduler.
func (d *AsyncDispatcher) WithRecipientResolver(r RecipientResolver) *AsyncDispatcher {
	d.recipients = r
	return d
}

// WithRecordCreator injects the CREATE-RECORD full-create seam backing the create_record action,
// plus the max automation chain depth that bounds create_record recursion. Chainable; a nil creator
// leaves the create_record action failing-open (logged). maxDepth < 1 is clamped to
// DefaultMaxChainDepth so a mis-set 0 never disables create_record entirely (the depth guard would
// otherwise skip at depth 0). Wired AFTER RecordService is built (it wraps RecordService.Create),
// breaking the construction cycle RecordService→engine→creator→RecordService. Call before Emit
// traffic AND before NewScheduler (the scheduler snapshots deps()).
func (d *AsyncDispatcher) WithRecordCreator(c RecordCreator, maxDepth int) *AsyncDispatcher {
	d.creator = c
	if maxDepth < 1 {
		maxDepth = DefaultMaxChainDepth
	}
	d.maxDepth = maxDepth
	return d
}

// WithScheduleResolver injects the CALENDAR-1 lookup backing a set_deadline action whose offset is
// in business_hours (DAILY-QUEUE #10). Without it such an action ERRORS rather than quietly
// computing wall-clock, so a missing wire is a visible FAILED run instead of a plausible-looking
// wrong deadline. Call before Emit traffic AND before NewScheduler (the scheduler snapshots
// deps()). Chainable.
func (d *AsyncDispatcher) WithScheduleResolver(r ScheduleResolver) *AsyncDispatcher {
	d.sched = r
	return d
}

// WithApprovalStarter injects the approval engine backing the start_approval action (specs/043
// S9b). Without it such an action FAILS its run with a clear error. Call before Emit traffic AND
// before NewScheduler (the scheduler snapshots deps()). Chainable.
func (d *AsyncDispatcher) WithApprovalStarter(s ApprovalStarter) *AsyncDispatcher {
	d.approvals = s
	return d
}

// deps assembles the action dependency bundle from the dispatcher's wired senders. Built per
// process() call (cheap struct copy) so a sender injected after construction is always honoured.
func (d *AsyncDispatcher) deps() actionDeps {
	return actionDeps{
		mut: d.mut, notif: d.notif, email: d.email, templates: d.templates, webhook: d.webhook, webhookQueue: d.webhookQueue, creds: d.creds, assign: d.assign, recipients: d.recipients,
		creator: d.creator, sched: d.sched, approvals: d.approvals, maxDepth: d.maxDepth, logger: d.logger,
	}
}

// Emit schedules a record event for asynchronous workflow processing. It is non-blocking: the
// event is pushed onto the buffered channel via a non-blocking select, and if the buffer is full
// (or the dispatcher is shutting down) the event is dropped with a zap warning rather than
// blocking the record mutation (fail-open). It NEVER carries the record Data into the log (which
// may hold business field values) — only tenant/entity/trigger/recordID identifiers.
func (d *AsyncDispatcher) Emit(_ context.Context, evt Event) {
	// Stamp NOW, not when a worker gets to it: the buffer may hold the event a while, and two
	// updates of one record must keep their commit order in the webhook body's occurredAt.
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		d.logger.Warn("workflow: event dropped, dispatcher shut down",
			zap.String("trigger", string(evt.Trigger)),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
		)
		return
	}

	ch := d.chs[d.shard(evt.TenantID, evt.RecordID)]
	select {
	case ch <- evt:
		// enqueued
	default:
		d.logger.Warn("workflow: event dropped, buffer full",
			zap.String("trigger", string(evt.Trigger)),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Int("buffer", cap(ch)),
		)
	}
}

// EmitWait is Emit for a caller that cannot afford a dropped event (the CSV import job, specs/048
// S7b): a full buffer makes it WAIT until a slot frees or ctx ends, and a shut-down dispatcher
// is an error — never a silent drop. The read lock is held through the send so Shutdown (write
// lock, then close) can never close the channel under it; the wait is bounded by ctx.
func (d *AsyncDispatcher) EmitWait(ctx context.Context, evt Event) error {
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return fmt.Errorf("workflow.EmitWait: dispatcher shut down")
	}
	select {
	case d.chs[d.shard(evt.TenantID, evt.RecordID)] <- evt:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("workflow.EmitWait: %w", ctx.Err())
	}
}

// shard maps a record to the worker channel that owns it. Events without a record id (none today
// on the emit paths) hash on the tenant alone, which still keeps them ordered.
func (d *AsyncDispatcher) shard(tenantID, recordID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(recordID))
	return int(h.Sum32() % uint32(len(d.chs)))
}

// worker drains its own channel until it is closed, processing one event at a time.
func (d *AsyncDispatcher) worker(ch <-chan Event) {
	defer d.wg.Done()
	for evt := range ch {
		d.process(evt)
	}
}

// process handles a single event: load active workflows, evaluate each condition, run matching
// actions. Every failure path is contained — a load/action error is logged but never returned,
// and a panic is recovered so the worker survives one bad event.
func (d *AsyncDispatcher) process(evt Event) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("workflow: recovered panic while processing event",
				zap.String("trigger", string(evt.Trigger)),
				zap.String("tenant_id", evt.TenantID),
				zap.String("entity_name", evt.EntityName),
				zap.Any("panic", r),
			)
		}
	}()

	if d.loader == nil {
		return // engine disabled (no loader wired)
	}

	ctx := context.Background()
	workflows, err := d.loader.ListActive(ctx, evt.TenantID, evt.EntityName, evt.Trigger)
	if err != nil {
		d.logger.Warn("workflow: failed to load active workflows",
			zap.String("trigger", string(evt.Trigger)),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Error(err),
		)
		return
	}

	for _, wf := range workflows {
		// Errors are already logged inside runOne (fail-open) — the loop just moves on.
		_ = d.runOne(ctx, wf, evt)
	}

	// TIMER-1: the SAME post-mutation dispatch also PARKS date_offset wake-ups. A create/update
	// whose record carries the rule's date field schedules a 'workflow_timer' job at dateField ±
	// offset; the job handler re-evaluates everything at fire time (fire-time re-evaluation — the
	// parked row is just a wake-up). after_delete schedules nothing: a deleted record has no future.
	// This runs AFTER the action loop on purpose: a rule above may have just written the date field
	// (a machine-set due_date), and the scheduler reads the record back so the wake-up anchors on
	// that value, not on the pre-action event snapshot (TestDispatcherSchedulesTimersAfterActions).
	d.scheduleTimers(ctx, evt)
}

// runOne evaluates ONE workflow's condition against the event and runs its actions, recording the
// W6 run row (skipped / failed / success). It is the shared per-rule body of the async worker
// (process) and the synchronous TIMER-1 fire path (FireWorkflow). The returned error is the first
// action error — already logged here fail-open, returned only so callers can surface it in their
// own context (the async loop ignores it; the job handler logs the job id alongside).
func (d *AsyncDispatcher) runOne(ctx context.Context, wf Workflow, evt Event) error {
	started := time.Now().UTC()
	// specs/059: mint the run id BEFORE the actions, so a queued email knows the row its delivery
	// outcome belongs to (RunRef) — the row itself is only written once every action returned.
	runID := uuid.NewString()
	ctx = WithRunRef(ctx, RunRef{TenantID: evt.TenantID, WorkflowID: wf.ID, RunID: runID})
	// EvaluateWithBefore, not Evaluate: the edge operators (changed / changedTo) need the record's
	// prior image, which the update emit now carries. A create or delete has none, so an edge rule
	// there evaluates false — nothing became anything.
	if !EvaluateWithBefore(wf.Condition, evt.Data, evt.Before) {
		// Condition not met → skip this rule, but RECORD the skip (W6: the run history
		// must show "evaluated, didn't match" — that is the trust signal admins need).
		d.recordRun(ctx, wf, evt, RunSkipped, nil, nil, started)
		return nil
	}
	// #338: the row goes in BEFORE the actions, so a delivery that gives up at once finds it.
	// ponytail: ~5 round trips per matched run (INSERT + Start/Finish tx); only email_alert needs the early row — gate on it if run volume makes this hot.
	ctx = d.startRun(ctx, wf, evt, started)
	results, err := runActions(ctx, wf, evt, d.deps())
	if err != nil {
		// An action failed: log it (fail-open) and move on. The triggering mutation (or the
		// parked wake-up) already committed and is unaffected.
		d.logger.Warn("workflow: action error (fail-open)",
			zap.String("workflow_id", wf.ID),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID),
			zap.Error(err),
		)
		d.recordRun(ctx, wf, evt, RunFailed, results, err, started)
		return err
	}
	d.recordRun(ctx, wf, evt, RunSuccess, results, nil, started)
	return nil
}

// FireWorkflow synchronously evaluates wf against evt and runs its actions with the dispatcher's
// wired deps, recording the W6 run row — the TIMER-1 fire path (the 'workflow_timer' job handler
// calls this after its own re-evaluation checks). It shares the EXACT per-rule body the async
// after_update path runs (runOne), so a time-triggered firing can never drift from an
// event-triggered one. Safe on a shut-down dispatcher (it uses no channel).
func (d *AsyncDispatcher) FireWorkflow(ctx context.Context, wf Workflow, evt Event) error {
	if d == nil {
		return nil
	}
	return d.runOne(ctx, wf, evt)
}

// scheduleTimers parks a wake-up for every active date_offset rule of the mutated entity. Only
// after_create/after_update mutations anchor timers; every failure path is logged fail-open (the
// mutation already committed — a lost wake-up is a warning, never an error to the caller). The
// per-rule date-field/config work happens in the injected TimerScheduler.
func (d *AsyncDispatcher) scheduleTimers(ctx context.Context, evt Event) {
	if d.timers == nil {
		return // timer scheduling disabled (no jobs queue wired)
	}
	if evt.Trigger != TriggerAfterCreate && evt.Trigger != TriggerAfterUpdate {
		return
	}
	rules, err := d.loader.ListActive(ctx, evt.TenantID, evt.EntityName, TriggerDateOffset)
	if err != nil {
		d.logger.Warn("workflow: failed to load date_offset workflows (fail-open)",
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Error(err),
		)
		return
	}
	for _, wf := range rules {
		if err := d.timers.ScheduleTimer(ctx, wf, evt); err != nil {
			d.logger.Warn("workflow: failed to schedule date_offset wake-up (fail-open)",
				zap.String("workflow_id", wf.ID),
				zap.String("tenant_id", evt.TenantID),
				zap.String("entity_name", evt.EntityName),
				zap.String("record_id", evt.RecordID),
				zap.Error(err),
			)
		}
	}
}

// startRun writes the 'running' row of a matched rule before its actions run (#338). It only does
// so when the recorder can finish rows (RunStarter); the returned ctx remembers that, so recordRun
// finishes the row instead of inserting a second one. Fail-open: a lost start leaves recordRun to
// insert the whole row at the end, exactly as before.
func (d *AsyncDispatcher) startRun(ctx context.Context, wf Workflow, evt Event, started time.Time) context.Context {
	st, ok := d.runs.(RunStarter)
	ref, hasRef := RunRefFrom(ctx)
	if !ok || !hasRef {
		return ctx
	}
	if err := st.StartRun(ctx, RunInput{
		ID: ref.RunID, TenantID: evt.TenantID, WorkflowID: wf.ID, TriggerType: string(evt.Trigger),
		RecordID: evt.RecordID, ActionsResult: placeholderRunActions(wf.Actions), StartedAt: started,
	}); err != nil {
		d.logger.Warn("workflow: failed to start run row (fail-open; it is written at the end)",
			zap.String("workflow_id", wf.ID), zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName), zap.Error(err))
		return ctx
	}
	return withRunStarted(ctx)
}

// recordRun writes one sys_workflow_runs row for a processed (event × workflow) pair. It is
// STRICTLY fail-open: a nil recorder is a no-op, a marshal/insert failure logs a warning and
// returns — it can never crash the worker or affect the (already-committed) mutation.
func (d *AsyncDispatcher) recordRun(ctx context.Context, wf Workflow, evt Event, status RunStatus, results []ActionResult, runErr error, started time.Time) {
	if d.runs == nil {
		return
	}
	var actionsJSON []byte
	if len(results) > 0 {
		b, err := json.Marshal(results)
		if err != nil {
			d.logger.Warn("workflow: marshal actions_result failed (run recorded without it)",
				zap.String("workflow_id", wf.ID), zap.String("tenant_id", evt.TenantID), zap.Error(err))
		} else {
			actionsJSON = b
		}
	}
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	ref, _ := RunRefFrom(ctx) // the id runOne minted before the actions ("" → InsertRun mints one)
	write := d.runs.InsertRun
	if st, ok := d.runs.(RunStarter); ok && runStartedFrom(ctx) {
		write = st.FinishRun // the 'running' row startRun made
	}
	if err := write(ctx, RunInput{
		ID:            ref.RunID,
		TenantID:      evt.TenantID,
		WorkflowID:    wf.ID,
		TriggerType:   string(evt.Trigger),
		RecordID:      evt.RecordID,
		Status:        status,
		Error:         errText,
		ActionsResult: actionsJSON,
		StartedAt:     started,
		FinishedAt:    time.Now().UTC(),
	}); err != nil {
		d.logger.Warn("workflow: failed to record run (fail-open)",
			zap.String("workflow_id", wf.ID),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Error(err),
		)
	}
}

// Shutdown closes the channel and waits for all workers to finish draining the buffered events
// (graceful drain). It is idempotent and safe to call from graceful-shutdown handlers. After
// Shutdown, Emit drops events with a log line instead of panicking on a closed channel.
func (d *AsyncDispatcher) Shutdown(ctx context.Context) error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		for _, ch := range d.chs {
			close(ch)
		}
	})

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
