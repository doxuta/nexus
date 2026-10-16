package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
)

// ScheduledLoader is the scheduler's consumer-side view of the workflow store's scheduled scan +
// claim. Satisfied by *SQLRepository. Declared as an interface so the scheduler is unit-testable
// with a fake (no DB).
type ScheduledLoader interface {
	// ListDueScheduled returns the active, non-deleted, scheduled rules across all tenants that
	// are due at `now` (last_run_at NULL or older than the rule's interval).
	ListDueScheduled(ctx context.Context, now time.Time) ([]ScheduledWorkflow, error)
	// ClaimScheduledRun atomically claims a due rule's run (CAS on last_run_at). Returns true
	// when this caller won the claim and should fire the actions.
	ClaimScheduledRun(ctx context.Context, tenantID, id string, now time.Time) (bool, error)
}

// RecordLister is the scheduler's consumer-side view of the dynamic record store: list the records
// of one entity in one tenant so the scheduler can evaluate a scheduled rule's condition against
// each and run its actions on the matches. Satisfied by a thin adapter over dynrepo (declared in
// the services layer so the workflow package keeps no dynrepo dependency). A row count limit
// guards against an unbounded scan on a huge table.
type RecordLister interface {
	// ListRecordsForScheduler returns up to `limit` records (id + committed field map) of the
	// entity in the tenant, under a system (admin) scope. The scheduler evaluates the rule's
	// condition against each record's data.
	ListRecordsForScheduler(ctx context.Context, entity, tenantID string, limit int) ([]ScheduledRecord, error)
}

// ScheduledRecord is one record the scheduler evaluates a rule against: its id + committed field
// map, plus its owner (CreatedBy — obj_*.created_by) so a scheduled rule's in_app_notification
// action can resolve the "$owner" sentinel exactly like the event-triggered path (M7 MED-1 fix).
type ScheduledRecord struct {
	ID   string
	Data map[string]any
	// OwnerID is the record's EFFECTIVE owner (obj_*.owner_id when set, else created_by) — the
	// same value the ACL row filter enforces on. It backs "$owner" for scheduled rules, which
	// used to resolve to the creator and so notified the wrong person after any "Đổi chủ".
	OwnerID string
	// CreatedAt is obj_*.created_at — the start instant a scheduled set_elapsed measures from, and
	// the anchor of a set_deadline with anchor created_at.
	CreatedAt time.Time
}

// defaultMaxRecordsPerScan caps how many records a single scheduled rule evaluates per tick, so a
// rule on a million-row table cannot stall the scheduler goroutine. A rule needing more is a sign
// the condition should be pushed into SQL (a later milestone); for M2 this bound is a safety net.
const defaultMaxRecordsPerScan = 5000

// Scheduler is the EPIC 4 M2 background time-based engine (spec §EPIC 4 "Background scheduler /
// time-based events"; benchmark Salesforce time-based-workflow + SugarCRM scheduler). A single
// goroutine ticks every Interval; each tick scans the due scheduled rules, CLAIMS each one (atomic
// CAS so two ticks / two instances never double-fire), lists the rule's entity records, evaluates
// the condition against each, and runs the actions on every match.
//
// Fail-open + isolation guarantees (same contract as the M1 dispatcher):
//   - A load/claim/list/action error on one rule is logged and the scheduler moves to the next —
//     it NEVER stops the tick or the goroutine.
//   - Every tick recovers from panic so one bad rule cannot kill the scheduler.
//   - It NEVER logs record Data (which holds business values) — only identifiers.
//   - Start is idempotent; Stop is graceful (waits for the in-flight tick to finish) and idempotent.
type Scheduler struct {
	loader ScheduledLoader
	lister RecordLister
	deps   actionDeps
	// runs is the W6 run-history sink, snapshotted from the engine at construction (one run row
	// per CLAIMED scheduled firing — not per record, so a 5000-record scan stays one row).
	// Optional: nil disables recording. Fail-open like every other scheduler error path.
	runs     RunRecorder
	interval time.Duration
	now      func() time.Time // injectable clock (tests); defaults to time.Now
	maxScan  int
	logger   *zap.Logger
	// frozen (specs/068 slice B), when set, reports a write-frozen tenant; its due rules are neither
	// claimed nor fired this pass (a claim would consume the interval for a firing that never ran).
	frozen func(ctx context.Context, tenantID string) bool

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// SchedulerConfig configures a Scheduler. Interval is the tick cadence (env
// WORKFLOW_SCHEDULER_INTERVAL_SEC); it is clamped to a sane minimum so a misconfigured zero never
// produces a busy-loop.
type SchedulerConfig struct {
	Interval time.Duration
}

// NewScheduler builds (does NOT start) a scheduler. lister/loader may be nil — a nil loader makes
// every tick a no-op (engine disabled). The senders that back the actions are taken from the
// supplied AsyncDispatcher's deps so the scheduler and the event engine share one set of action
// adapters (one SMTP sender, one webhook sender, one record mutator, one notification writer).
func NewScheduler(cfg SchedulerConfig, loader ScheduledLoader, lister RecordLister, engine *AsyncDispatcher, logger *zap.Logger) *Scheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	interval := cfg.Interval
	if interval < time.Second {
		interval = 60 * time.Second
	}
	var deps actionDeps
	var runs RunRecorder
	if engine != nil {
		deps = engine.deps()
		runs = engine.runs // W6: share the engine's run recorder (wire WithRunRecorder BEFORE NewScheduler)
	}
	return &Scheduler{
		loader:   loader,
		lister:   lister,
		deps:     deps,
		runs:     runs,
		interval: interval,
		now:      time.Now,
		maxScan:  defaultMaxRecordsPerScan,
		logger:   logger,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the ticker goroutine. It is idempotent (a second call is a no-op). The goroutine
// runs until Stop is called. Start does NOT run a tick immediately — the first tick fires after
// one Interval, so boot does not race the rest of wiring.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		go s.run()
	})
}

// run is the ticker loop. Each tick is wrapped in tickOnce (which recovers panics) so one bad tick
// can never kill the goroutine. It exits when stop is closed, then closes done so Stop can join.
func (s *Scheduler) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.tickOnce()
		}
	}
}

// tickOnce runs one scan, recovering from any panic so the goroutine survives. It scans due rules,
// claims+fires each, all fail-open.
func (s *Scheduler) tickOnce() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("workflow scheduler: recovered panic in tick", zap.Any("panic", r))
		}
	}()
	if s.loader == nil {
		return // engine disabled (no loader wired)
	}
	ctx := context.Background()
	s.runDue(ctx, s.now())
}

// WithTenantGate arms the per-tenant write-freeze skip. Chainable; nil keeps it off.
func (s *Scheduler) WithTenantGate(frozen func(ctx context.Context, tenantID string) bool) *Scheduler {
	s.frozen = frozen
	return s
}

// runDue performs one scheduler pass at time `now`: load due rules, and for each one claim it
// (atomic CAS) then fire it. Exported-internal so a test can drive a deterministic single pass
// without the ticker.
func (s *Scheduler) runDue(ctx context.Context, now time.Time) {
	due, err := s.loader.ListDueScheduled(ctx, now)
	if err != nil {
		s.logger.Warn("workflow scheduler: failed to list due scheduled workflows", zap.Error(err))
		return
	}
	for i := range due {
		if s.frozen != nil && s.frozen(ctx, due[i].TenantID) {
			continue // stays due: it fires on the first pass after the freeze lapses
		}
		s.fireScheduled(ctx, due[i], now)
	}
}

// fireScheduled claims one due rule (CAS) and, if the claim is won, lists the rule's entity records
// and runs the actions on every record matching the condition. Every error path is logged
// fail-open and never aborts the rest of the pass. The claim guarantees the rule fires at most
// once per interval even with concurrent ticks / multiple instances.
func (s *Scheduler) fireScheduled(ctx context.Context, sw ScheduledWorkflow, now time.Time) {
	claimed, err := s.loader.ClaimScheduledRun(ctx, sw.TenantID, sw.ID, now)
	if err != nil {
		s.logger.Warn("workflow scheduler: claim failed (fail-open)",
			zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID), zap.Error(err))
		return
	}
	if !claimed {
		return // another tick already claimed this run — not a double-fire.
	}

	started := time.Now().UTC()
	// specs/059: the firing's run id, minted before any action so a queued email can name the row
	// its delivery outcome belongs to (the row is written once, after the whole scan).
	ctx = WithRunRef(ctx, RunRef{TenantID: sw.TenantID, WorkflowID: sw.ID, RunID: uuid.NewString()})
	// #338: the 'running' row first, with the empty summary a delivery that gives up at once counts into.
	if st, ok := s.runs.(RunStarter); ok {
		ref, _ := RunRefFrom(ctx)
		if err := st.StartRun(ctx, RunInput{
			ID: ref.RunID, TenantID: sw.TenantID, WorkflowID: sw.ID, TriggerType: string(TriggerScheduled),
			ActionsResult: []byte(`{"scanned":0,"matched":0,"failed":0}`), StartedAt: started,
		}); err != nil {
			s.logger.Warn("workflow scheduler: failed to start run row (fail-open; it is written at the end)",
				zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID), zap.Error(err))
		} else {
			ctx = withRunStarted(ctx)
		}
	}
	if s.lister == nil {
		s.logger.Warn("workflow scheduler: no record lister wired; cannot evaluate scheduled rule",
			zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID))
		s.recordScheduledRun(ctx, sw, RunFailed, 0, 0, 0, 0, nil, "no record lister wired", started)
		return
	}
	records, err := s.lister.ListRecordsForScheduler(ctx, sw.EntityName, sw.TenantID, s.maxScan)
	if err != nil {
		s.logger.Warn("workflow scheduler: list records failed (fail-open)",
			zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID),
			zap.String("entity_name", sw.EntityName), zap.Error(err))
		s.recordScheduledRun(ctx, sw, RunFailed, 0, 0, 0, 0, nil, err.Error(), started)
		return
	}

	deps := s.deps
	if deps.sched != nil {
		deps.sched = &firingScheduleCache{inner: deps.sched, got: map[string]*calendar.Schedule{}}
	}
	deps.warnedOnce = map[string]struct{}{}
	var held *heldWebhooks
	if s.webhooksPending(ctx, sw) {
		held = &heldWebhooks{}
		deps.webhookQueue = held
	}
	matched, failed := 0, 0
	var firstErr error
	var warn runWarnings
	for _, rec := range records {
		if !Evaluate(sw.Condition, rec.Data) {
			continue
		}
		matched++
		evt := Event{
			TenantID:    sw.TenantID,
			ActorUserID: SystemActorID,
			EntityName:  sw.EntityName,
			RecordID:    rec.ID,
			Trigger:     TriggerScheduled,
			Data:        rec.Data,
			OwnerUserID: rec.OwnerID, // "$owner" source: EFFECTIVE owner, so a reassigned record notifies its CURRENT owner
			CreatedAt:   rec.CreatedAt,
			// The instant taken BEFORE the records were read: rec.Data is that snapshot, and an
			// edit committed after the read is Emit-stamped later, so a receiver keeping the
			// largest occurredAt keeps the edit, not this stale copy (never the marshal time).
			OccurredAt: started,
		}
		results, aerr := runActions(ctx, sw.Workflow, evt, deps)
		warn.add(results)
		if aerr != nil {
			failed++
			if firstErr == nil {
				firstErr = aerr
			}
			s.logger.Warn("workflow scheduler: action error (fail-open)",
				zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID),
				zap.String("entity_name", sw.EntityName), zap.String("record_id", rec.ID),
				zap.Error(aerr))
		}
	}
	s.logger.Info("workflow scheduler: scheduled rule fired",
		zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID),
		zap.String("entity_name", sw.EntityName), zap.Int("matched", matched), zap.Int("scanned", len(records)),
		zap.Int("webhooks_held", held.count()))

	// W6: record ONE run per claimed firing. skipped = nothing matched; failed = ≥1 record's
	// actions errored; success otherwise. Fail-open like every other path here.
	status := RunSuccess
	errText := ""
	switch {
	case matched == 0:
		status = RunSkipped
	case failed > 0:
		status = RunFailed
		errText = firstErr.Error()
	}
	s.recordScheduledRun(ctx, sw, status, len(records), matched, failed, held.count(), &warn, errText, started)
}

// webhooksPending is the back-pressure of a scheduled rule with an outbound_webhook action (review
// of DAILY-QUEUE #26). Inline, the rule's POSTs throttled it: a firing could not start before the
// previous one's deliveries finished. On the durable queue a firing parks up to maxScan jobs at
// once whether or not the webhook lane drained the last ones, so sys_jobs would grow without bound
// even against a healthy receiver. While deliveries the rule parked earlier are still queued or
// running, the firing's own deliveries are therefore HELD — counted, not parked (heldWebhooks) —
// and its other actions run as usual: a receiver outage must not stop a rule's reminders and field
// updates, which the inline POSTs never did either. The next firing after the backlog drains
// pushes the records' then-current state. A probe error fails open (parks, as before the probe).
func (s *Scheduler) webhooksPending(ctx context.Context, sw ScheduledWorkflow) bool {
	if s.deps.webhookQueue == nil || !slices.ContainsFunc(sw.Actions, func(a Action) bool { return a.Type == ActionWebhook }) {
		return false
	}
	pending, err := s.deps.webhookQueue.HasPendingWebhooks(ctx, sw.TenantID, sw.ID)
	if err != nil {
		s.logger.Warn("workflow scheduler: webhook back-pressure probe failed (fail-open, parking)",
			zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID), zap.Error(err))
		return false
	}
	return pending
}

// runWarnings folds the per-action ActionResult.Warning of a scheduled firing into its one summary
// row (DAILY-QUEUE #105): how many action results warned, and each distinct sentence once in
// first-seen order. The sentences are fixed engine constants (wall-clock fallback, dead pinned
// calendar, unlinked relation), so the list stays a handful long however many records matched.
type runWarnings struct {
	count     int
	sentences []string
}

func (w *runWarnings) add(results []ActionResult) {
	for _, r := range results {
		if r.Warning == "" {
			continue
		}
		w.count++
		if !slices.Contains(w.sentences, r.Warning) {
			w.sentences = append(w.sentences, r.Warning)
		}
	}
}

// firingScheduleCache remembers the working calendar for ONE scheduled firing (DAILY-QUEUE #166).
// Without it set_deadline/set_elapsed in business hours re-read and re-parsed the same calendar
// row for every matched record — up to maxScan per action per tick (≈0.5 ms each on local MySQL,
// so ≈2.5 s of a 5000-record tick). A fresh cache per firing keeps the one property that matters:
// an admin who edits a calendar sees it on the next tick. Errors are NOT remembered, so a
// transient store fault costs one record, not the whole scan. The scheduler fires rules one at a
// time on one goroutine, so the map needs no lock. The event path (one record per event) is not
// wrapped — it has nothing to reuse.
type firingScheduleCache struct {
	inner ScheduleResolver
	got   map[string]*calendar.Schedule // tenantID + "\x00" + calendarID → schedule (nil = no calendar)
}

func (c *firingScheduleCache) ResolveSchedule(ctx context.Context, tenantID, calendarID string) (*calendar.Schedule, error) {
	key := tenantID + "\x00" + calendarID
	if s, ok := c.got[key]; ok {
		return s, nil
	}
	s, err := c.inner.ResolveSchedule(ctx, tenantID, calendarID)
	if err != nil {
		return nil, err
	}
	c.got[key] = s
	return s, nil
}

// heldWebhooks stands in for the webhook queue during a firing whose earlier deliveries are still
// pending: every delivery the firing would park is counted, not parked, and the count lands in the
// run summary (webhooksHeld) so "success, 3 matched" never hides that nothing was sent. The
// scheduler fires rules one at a time on one goroutine, so the counter needs no lock.
type heldWebhooks struct{ n int }

func (h *heldWebhooks) EnqueueWebhook(context.Context, WebhookDelivery) error { h.n++; return nil }

func (h *heldWebhooks) HasPendingWebhooks(context.Context, string, string) (bool, error) {
	return true, nil
}

func (h *heldWebhooks) count() int {
	if h == nil {
		return 0
	}
	return h.n
}

// recordScheduledRun writes the single sys_workflow_runs row of one claimed scheduled firing:
// record_id NULL (a firing spans many records) and actions_result = {scanned, matched, failed}
// summary, plus webhooksHeld when the back-pressure held deliveries (webhooksPending), plus
// warned/warnings when actions ran with a run-history warning (DAILY-QUEUE #105 — a wall-clock
// business_hours deadline). Strictly fail-open: nil recorder is a no-op; an insert failure only logs.
func (s *Scheduler) recordScheduledRun(ctx context.Context, sw ScheduledWorkflow, status RunStatus, scanned, matched, failed, webhooksHeld int, warn *runWarnings, errText string, started time.Time) {
	if s.runs == nil {
		return
	}
	counts := map[string]any{"scanned": scanned, "matched": matched, "failed": failed}
	if webhooksHeld > 0 {
		counts["webhooksHeld"] = webhooksHeld // deliveries not parked: earlier ones still pending
	}
	if warn != nil && warn.count > 0 {
		counts["warned"] = warn.count       // action results that carried a warning
		counts["warnings"] = warn.sentences // each distinct sentence once, verbatim
	}
	summary, err := json.Marshal(counts)
	if err != nil {
		summary = nil // unreachable for ints and strings; keep the row anyway
	}
	ref, _ := RunRefFrom(ctx) // minted by fireScheduled before the scan
	write := s.runs.InsertRun
	if st, ok := s.runs.(RunStarter); ok && runStartedFrom(ctx) {
		write = st.FinishRun // the 'running' row fireScheduled made
	}
	if err := write(ctx, RunInput{
		ID:            ref.RunID,
		TenantID:      sw.TenantID,
		WorkflowID:    sw.ID,
		TriggerType:   string(TriggerScheduled),
		Status:        status,
		Error:         errText,
		ActionsResult: summary,
		StartedAt:     started,
		FinishedAt:    time.Now().UTC(),
	}); err != nil {
		s.logger.Warn("workflow scheduler: failed to record run (fail-open)",
			zap.String("workflow_id", sw.ID), zap.String("tenant_id", sw.TenantID), zap.Error(err))
	}
}

// Stop signals the ticker goroutine to exit and waits (up to the context deadline) for the
// in-flight tick to finish. It is idempotent and safe to call from graceful-shutdown handlers.
// Returns ctx.Err() if the deadline passes before the goroutine exits.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunDueForTest drives one deterministic scheduler pass at `now` without the ticker. It is a test
// seam consumed by the cross-package integration tests (internal/services) which cannot reach the
// unexported runDue. Production code uses Start/Stop; this never runs on the ticker path.
func (s *Scheduler) RunDueForTest(ctx context.Context, now time.Time) {
	if s.loader == nil {
		return
	}
	s.runDue(ctx, now)
}
