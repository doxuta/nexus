package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

// -----------------------------------------------------------------------------------------------
// TIMER-1 — date_offset workflow trigger (engine-depth primitive #1b; ../specs/023-workflow-timer-calendar/spec.md §2
// "parked row + wake-up"). Two halves:
//
//   - WorkflowTimerScheduler (the PRODUCER, workflow.TimerScheduler seam): on after_create /
//     after_update, for each active date_offset rule of the entity, compute fire_at =
//     dateField ± offset (business_hours through CALENDAR-1, wall-clock otherwise) and park a
//     durable 'workflow_timer' sys_jobs row (JOBS-1) at run_at = fire_at.
//
//   - NewWorkflowTimerJobHandler (the CONSUMER): at wake-up, RE-EVALUATE everything against the
//     CURRENT state — rule still live/active/date_offset on the same entity, record still live,
//     date field unchanged, condition still true — then run the actions through the SAME
//     dispatcher body the event triggers use (FireWorkflow). FIRE-TIME RE-EVALUATION is the
//     design: the handler is the source of truth; the parked job is just a wake-up.
//
// Double-fire risk + mitigation (documented decision): every create/update with the date field
// set parks a wake-up, so a record edited N times can hold N parked jobs. Two guards keep that
// from double-firing: (1) ENQUEUE DEDUP — the producer skips when a queued 'workflow_timer' job
// with an IDENTICAL payload (workflowId+recordId+dateValue) already exists; (2) FIRE-TIME SKIP —
// the handler compares the record's CURRENT canonical date value to the payload's captured one
// and no-ops on mismatch (the mutation that moved the date already parked a replacement at the
// new instant). Residual risk: JOBS-1 is at-least-once, and two concurrent producers can race
// past the dedup probe — a rare duplicate firing remains possible (same contract as the queue's
// own re-execution semantics; actions are engine-idempotent-ish: field_update converges,
// notification may repeat). Accepted for v1, noted in docs/api-conventions.md §18.
// -----------------------------------------------------------------------------------------------

// WorkflowTimerJobType is the sys_jobs job_type of one parked date_offset wake-up.
const WorkflowTimerJobType = "workflow_timer"

// workflowTimerJobPayload is the sys_jobs.payload of a WorkflowTimerJobType job. DateValue is the
// CANONICAL string of the record's date field captured at schedule time (workflow.ParseTimerDate)
// — the fire-time "did the date move?" comparison and the enqueue dedup both ride it. Tenant
// pinning lives on the sys_jobs row itself (tenant_id column).
type workflowTimerJobPayload struct {
	WorkflowID string `json:"workflowId"`
	EntityName string `json:"entityName"`
	RecordID   string `json:"recordId"`
	DateValue  string `json:"dateValue"`
	// Occurrence is the 0-based repeat index (ACTIVITY-CORE #3b). Absent/0 = the first fire, which
	// is exactly what every pre-recurrence payload decodes to — so old parked jobs keep working.
	//
	// It is also what makes the re-park possible at all: the enqueue dedup probe matches on payload
	// EQUALITY including done jobs, so a second wake-up carrying the same payload would be silently
	// dropped as a duplicate. Occurrence makes each fire's payload distinct by construction.
	Occurrence int `json:"occurrence,omitempty"`
	// WallClock marks a business_hours wake-up whose instant was computed WITHOUT a working
	// calendar — the documented fallback (no usable default, or a pinned calendar that is gone).
	// It is the only place that fact survives until fire time, where the run it becomes carries
	// workflow.TimerWallClockWarning (DAILY-QUEUE #105). Absent on every other wake-up, so a
	// calendar-computed payload keeps its old bytes; ScheduleTimer's dedup probe checks both
	// variants so the flag never splits one wake-up into two.
	WallClock bool `json:"wallClock,omitempty"`
	// RepeatWallClock marks occurrence 2..N of a business_hours rule: rearmTimerRepeat has no
	// calendar read, so the instant is wall-clock by design (#447). Kept apart from WallClock
	// because the WallClock notes blame a missing/dead calendar, which is false here.
	RepeatWallClock bool `json:"repeatWallClock,omitempty"`
}

// timerJobQueue is the producer-side slice of the jobs store: enqueue + the dedup probe.
// Satisfied by *jobs.SQLStore.
type timerJobQueue interface {
	Enqueue(ctx context.Context, j jobs.Job) (string, error)
	HasJobWithPayload(ctx context.Context, jobType, tenantID string, payload []byte) (bool, error)
}

// timerCalendarReader resolves the CALENDAR-1 rows business_hours math needs. Satisfied by
// *calmeta.SQLRepository (tenant-scoped).
type timerCalendarReader interface {
	GetByID(ctx context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error)
	GetDefault(ctx context.Context, tenantID string) (*calmeta.BusinessCalendar, error)
}

// timerRecordReader loads one live record under an admin scope (the timer acts as a system
// principal, like every other automation seam). Satisfied by *dynrepo.Repository.
type timerRecordReader interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// WorkflowTimerScheduler implements workflow.TimerScheduler: it turns "a record of a
// date_offset rule's entity just committed" into a parked sys_jobs wake-up at the computed fire
// instant. All I/O lives here (calendar rows, the record read-back the anchor comes from, jobs
// enqueue); the offset math is workflow.ComputeFireAt — one deterministic body.
type WorkflowTimerScheduler struct {
	queue     timerJobQueue
	calendars timerCalendarReader
	records   timerRecordReader
}

// NewWorkflowTimerScheduler wires the producer over the jobs store, the calendar store and the
// dynamic record store.
func NewWorkflowTimerScheduler(queue timerJobQueue, calendars timerCalendarReader, records timerRecordReader) *WorkflowTimerScheduler {
	return &WorkflowTimerScheduler{queue: queue, calendars: calendars, records: records}
}

// Ensure the producer satisfies the dispatcher's seam at compile time.
var _ workflow.TimerScheduler = (*WorkflowTimerScheduler)(nil)

// ScheduleTimer parks the wake-up for (wf, evt): resolve the anchor date, compute fire_at,
// dedup-probe, enqueue. A record whose date field is unset/unparseable is a silent no-op — an
// optional empty date is a normal record state (the config validator already guarantees the
// field's TYPE). Real faults (DB, marshal) are returned for the dispatcher's fail-open log.
func (s *WorkflowTimerScheduler) ScheduleTimer(ctx context.Context, wf workflow.Workflow, evt workflow.Event) error {
	if s == nil || s.queue == nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: no queue wired")
	}
	if wf.Condition == nil || wf.Condition.Timer == nil {
		// Defensive: a date_offset rule without a timer cannot exist past config validation.
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: workflow %s has no timer config", wf.ID)
	}
	cfg := *wf.Condition.Timer

	anchor, canonical, ok, err := s.resolveAnchor(ctx, cfg.DateField, evt)
	if err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: %w", err)
	}
	if !ok {
		return nil // date field unset → nothing to park (not a fault)
	}

	sched, err := s.resolveSchedule(ctx, evt.TenantID, cfg)
	if err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: %w", err)
	}
	fireAt, err := workflow.ComputeFireAt(cfg, anchor, sched)
	if err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: %w", err)
	}

	p := workflowTimerJobPayload{
		WorkflowID: wf.ID,
		EntityName: evt.EntityName,
		RecordID:   evt.RecordID,
		DateValue:  canonical,
		WallClock:  cfg.Offset.Unit == workflow.TimerUnitBusinessHours && sched == nil,
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: marshal payload: %w", err)
	}

	// Enqueue dedup: an unrelated update (date unchanged) would otherwise park a second wake-up
	// with the SAME payload → double-fire. The probe matches queued+running+DONE, so a job that
	// already FIRED (done) blocks a re-park too (the HIGH re-fire bug: every post-deadline edit
	// re-ran the action). The probe seeks a hash of MySQL's canonical JSON text, so key order and
	// whitespace don't matter but one value must always render the same way (json.Marshal of this
	// fixed struct does; see jobs.dedupProbeSQL). See the double-fire note in the
	// package header + api-conventions §18.2 for the bounded post-retention residual.
	exists, err := s.queue.HasJobWithPayload(ctx, WorkflowTimerJobType, evt.TenantID, payload)
	if err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: dedup probe: %w", err)
	}
	if !exists && cfg.Offset.Unit == workflow.TimerUnitBusinessHours {
		// The WallClock flag (#105) is part of the payload, but not part of the wake-up's identity:
		// a calendar added or removed between two edits that leave the date alone must not park a
		// second wake-up for the same (rule, record, date) — the reminder would fire twice. So a
		// business_hours park also probes the other variant. The first wake-up keeps its instant,
		// exactly as before the flag existed.
		alt := p
		alt.WallClock = !p.WallClock
		altPayload, err := json.Marshal(alt)
		if err != nil {
			return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: marshal payload: %w", err)
		}
		if exists, err = s.queue.HasJobWithPayload(ctx, WorkflowTimerJobType, evt.TenantID, altPayload); err != nil {
			return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: dedup probe: %w", err)
		}
	}
	if exists {
		return nil
	}
	if _, err := s.queue.Enqueue(ctx, jobs.Job{
		Type:     WorkflowTimerJobType,
		TenantID: evt.TenantID,
		Payload:  payload,
		RunAt:    fireAt,
	}); err != nil {
		return fmt.Errorf("services.WorkflowTimerScheduler.ScheduleTimer: %w", err)
	}
	return nil
}

// resolveAnchor resolves the timer's anchor instant + canonical date string. It reads the record
// BACK from the store (tenant-pinned, admin scope) instead of trusting evt.Data: the after-commit
// event is a snapshot taken BEFORE the same dispatch's action loop, and a field_update in that
// loop may have just written the very date field this rule anchors on (a machine-set due_date —
// DAILY-QUEUE #6). field_update is deliberately event-less (no cascade), so nothing else would ever
// re-park; the dispatcher runs actions first (pinned by TestDispatcherSchedulesTimersAfterActions),
// so the row on disk already holds the new value here. The DB read also makes the canonical string
// identical to what the fire-time premise check reads (same column) — the moved-check depends on
// that, and created_at (never in Data) always needed the read.
//
// A raced delete parks nothing. Without a reader (legacy fakes) a data field falls back to the
// event snapshot — stated, not hidden: on that path a date written by an action in the same
// dispatch stays invisible — and created_at, which has no snapshot, is an error.
// ponytail: one GetRecord per (date_offset rule × mutation); hoist to one read per event in the
// dispatcher if an entity ever carries more than a handful of timer rules.
func (s *WorkflowTimerScheduler) resolveAnchor(ctx context.Context, dateField string, evt workflow.Event) (time.Time, string, bool, error) {
	if s.records == nil {
		if workflow.IsSystemTimerAnchor(dateField) {
			return time.Time{}, "", false, fmt.Errorf("resolveAnchor: no record reader wired for %s", dateField)
		}
		anchor, canonical, ok := workflow.ParseTimerDate(evt.Data[dateField])
		return anchor, canonical, ok, nil
	}
	rec, err := s.records.GetRecord(ctx, evt.EntityName, evt.TenantID, evt.RecordID, dynrepo.AdminScope())
	if err != nil {
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return time.Time{}, "", false, nil // raced delete → nothing to park
		}
		return time.Time{}, "", false, fmt.Errorf("resolveAnchor: %w", err)
	}
	anchor, canonical, ok := workflow.ParseTimerDate(timerAnchorValue(rec, dateField))
	return anchor, canonical, ok, nil
}

// timerAnchorValue is the raw anchor of a timer on row: the record stamp for a system anchor, the
// obj data value otherwise. The ONE place producer, fire-time premise check and re-arm agree on it.
func timerAnchorValue(row *dynrepo.Record, dateField string) any {
	switch dateField {
	case workflow.TimerAnchorCreatedAt:
		return row.CreatedAt
	case workflow.TimerAnchorUpdatedAt:
		return row.UpdatedAt
	}
	return row.Data[dateField]
}

// WorkflowScheduleResolver is the ONE place "which working calendar applies to this tenant" is
// answered: the pinned calendarId when the caller names one, else the tenant's default. It backs
// both business_hours consumers — the date_offset timer producer below and the set_deadline ACTION
// (workflow.ScheduleResolver, DAILY-QUEUE #10) — so a deadline and the reminder about it can never
// disagree about when the office is open.
//
// Not-found is the documented WALL-CLOCK FALLBACK (nil schedule, nil error) in BOTH cases: a
// calendar deleted after the rule was saved degrades gracefully instead of killing the timer
// (CALENDAR-1 contract, specs/023 §1). A store failure is returned as an error — its caller decides
// whether to degrade, and the set_deadline action deliberately does not.
type WorkflowScheduleResolver struct {
	calendars timerCalendarReader
	logger    *zap.Logger
}

// NewWorkflowScheduleResolver wires the resolver over the CALENDAR-1 store. A nil store makes
// ResolveSchedule ERROR (see there): "nobody wired the store" must not be reported as "this tenant
// has no calendar". logger may be nil.
func NewWorkflowScheduleResolver(calendars timerCalendarReader, logger *zap.Logger) *WorkflowScheduleResolver {
	return &WorkflowScheduleResolver{calendars: calendars, logger: logger}
}

// Ensure the resolver satisfies the engine's seam at compile time.
var _ workflow.ScheduleResolver = (*WorkflowScheduleResolver)(nil)

// ResolveSchedule loads the schedule for (tenantID, calendarID); calendarID "" means the tenant's
// default calendar.
func (r *WorkflowScheduleResolver) ResolveSchedule(ctx context.Context, tenantID, calendarID string) (*calendar.Schedule, error) {
	// An unwired store is a WIRING BUG, not "this tenant has no calendar" — and it has to error,
	// because a typed-nil *WorkflowScheduleResolver handed to WithScheduleResolver makes
	// actionDeps.sched non-nil, so runSetDeadline's "no resolver wired" guard never fires and a
	// (nil, nil) here would land in the silent wall-clock arm instead. That is the exact ambiguity
	// this slice exists to remove. The TIMER-1 producer's own nil-store case is handled at its
	// call site (resolveSchedule below), where degrading is the documented contract.
	if r == nil || r.calendars == nil {
		return nil, fmt.Errorf("services.ResolveSchedule: no business-calendar store wired")
	}
	var (
		cal *calmeta.BusinessCalendar
		err error
	)
	if strings.TrimSpace(calendarID) != "" {
		cal, err = r.calendars.GetByID(ctx, tenantID, calendarID)
	} else {
		cal, err = r.calendars.GetDefault(ctx, tenantID)
	}
	if err != nil {
		if errors.Is(err, calmeta.ErrCalendarNotFound) {
			return nil, nil // wall-clock fallback (documented CALENDAR-1 contract)
		}
		return nil, fmt.Errorf("services.ResolveSchedule: %w", err)
	}
	sched, err := calendar.FromJSON(cal.Timezone, cal.Hours, cal.Holidays)
	if err != nil {
		// "Impossible" by validator parity — and it was NOT impossible: a binary built without
		// the embedded tzdata cannot load the calendar's timezone, so on the runtime image EVERY
		// calendar failed here and the caller reported "this tenant has no calendar" while the
		// admin was looking at one in the UI (fixed by `_ "time/tzdata"` in the shipped mains).
		// Still degrade rather than kill the timer — but never silently again: a calendar that
		// exists and cannot be read is a different fact from a tenant that has none.
		logger := r.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		logger.Warn("workflow: stored business calendar could not be parsed — treating the tenant as having no calendar",
			zap.String("tenant_id", tenantID),
			zap.String("calendar_id", cal.ID),
			zap.String("timezone", cal.Timezone),
			zap.Error(err),
		)
		return nil, nil
	}
	return sched, nil
}

// resolveSchedule loads the CALENDAR-1 schedule for a business_hours timer through the shared
// resolver above. Non-business units never touch the store.
func (s *WorkflowTimerScheduler) resolveSchedule(ctx context.Context, tenantID string, cfg workflow.TimerConfig) (*calendar.Schedule, error) {
	if cfg.Offset.Unit != workflow.TimerUnitBusinessHours || s.calendars == nil {
		// A nil store degrades HERE, at the timer's own call site: the legacy fakes construct this
		// producer without a calendar store and the TIMER-1 contract says degrade. The resolver
		// itself errors on a nil store because its other consumer (set_deadline) must not.
		return nil, nil
	}
	return NewWorkflowScheduleResolver(s.calendars, nil).ResolveSchedule(ctx, tenantID, cfg.CalendarID)
}

// -----------------------------------------------------------------------------------------------
// Consumer: the workflow_timer job handler.
// -----------------------------------------------------------------------------------------------

// timerRuleReader loads one tenant-scoped workflow row (inactive/soft-deleted-aware). Satisfied
// by *workflow.SQLRepository.
type timerRuleReader interface {
	Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
}

// timerFirer runs one workflow synchronously through the dispatcher's shared per-rule body
// (evaluate condition → run actions → record W6 run). Satisfied by *workflow.AsyncDispatcher.
type timerFirer interface {
	FireWorkflow(ctx context.Context, wf workflow.Workflow, evt workflow.Event) error
}

// NewWorkflowTimerJobHandler returns the queue-side consumer of WorkflowTimerJobType. At wake-up
// it re-establishes every premise against CURRENT state (fire-time re-evaluation), then fires:
//
//	rule gone / inactive / repointed (trigger≠date_offset or entity changed) → no-op done
//	record gone (deleted / raced)                                            → no-op done
//	date field now unset OR canonical value ≠ payload.dateValue (date moved) → no-op done
//	    (the mutation that moved the date already parked a replacement wake-up)
//	otherwise → FireWorkflow: condition re-evaluated on the CURRENT record data; actions run
//	    through the SAME engine deps the event triggers use; W6 run row trigger_type=date_offset
//
// Error contract: infra faults BEFORE the decision point (DB read errors) return an error so the
// queue retries with backoff; once FireWorkflow runs, an action failure is already recorded
// fail-open in the run history (exactly like after_update), so the handler returns nil — a retry
// would double-run the earlier successful actions.
// NewWorkflowTimerJobHandler builds the fire-time handler. queue may be nil — a recurrence then
// fires ONCE and logs that it could not re-park, rather than silently pretending it repeated.
func NewWorkflowTimerJobHandler(rules timerRuleReader, records timerRecordReader, engine timerFirer, queue timerJobQueue, logger *zap.Logger) jobs.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(ctx context.Context, job jobs.Job) error {
		if rules == nil || records == nil || engine == nil {
			return fmt.Errorf("services.WorkflowTimerJobHandler: not fully wired")
		}
		var p workflowTimerJobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("services.WorkflowTimerJobHandler: unmarshal payload: %w", err)
		}
		if job.TenantID == "" || p.WorkflowID == "" || p.EntityName == "" || p.RecordID == "" {
			return fmt.Errorf("services.WorkflowTimerJobHandler: incomplete payload (tenant/workflow/entity/record required)")
		}

		// Rule premise: still live, active, date_offset, same entity, still carrying a timer.
		rec, err := rules.Get(ctx, job.TenantID, p.WorkflowID)
		if err != nil {
			if errors.Is(err, workflow.ErrWorkflowNotFound) {
				return nil // rule deleted → wake-up is moot
			}
			return fmt.Errorf("services.WorkflowTimerJobHandler: load rule: %w", err)
		}
		wf, err := rec.ToWorkflow()
		if err != nil {
			return fmt.Errorf("services.WorkflowTimerJobHandler: decode rule: %w", err)
		}
		if !wf.IsActive || wf.Trigger != workflow.TriggerDateOffset || wf.EntityName != p.EntityName ||
			wf.Condition == nil || wf.Condition.Timer == nil {
			return nil // rule deactivated/repointed since the wake-up was parked
		}

		// Record premise: still live in the SAME tenant (GetRecord is tenant-pinned). The event's
		// occurredAt is taken BEFORE the read: row.Data is that read, and an edit committed after
		// it is Emit-stamped later, so a webhook receiver keeping the largest occurredAt keeps the
		// edit rather than this copy (unstamped, the body would take the later marshal time).
		readAt := time.Now().UTC()
		row, err := records.GetRecord(ctx, p.EntityName, job.TenantID, p.RecordID, dynrepo.AdminScope())
		if err != nil {
			if errors.Is(err, dynrepo.ErrRecordNotFound) {
				return nil // record deleted → nothing to fire on
			}
			return fmt.Errorf("services.WorkflowTimerJobHandler: load record: %w", err)
		}

		// Date premise: the anchor value must still be what this wake-up was computed from.
		_, current, ok := workflow.ParseTimerDate(timerAnchorValue(row, wf.Condition.Timer.DateField))
		if !ok || current != p.DateValue {
			// Date cleared or moved — skip. The mutation that changed it already parked a
			// replacement wake-up at the new instant (fire-time re-evaluation contract).
			logger.Info("workflow timer: wake-up skipped, date field moved/cleared",
				zap.String("workflow_id", p.WorkflowID),
				zap.String("tenant_id", job.TenantID),
				zap.String("entity_name", p.EntityName),
				zap.String("record_id", p.RecordID))
			return nil
		}

		// Fire: condition re-evaluated + actions through the SAME engine body after_update uses.
		evt := workflow.Event{
			TenantID:    job.TenantID,
			ActorUserID: workflow.SystemActorID,
			EntityName:  p.EntityName,
			RecordID:    p.RecordID,
			Trigger:     workflow.TriggerDateOffset,
			Data:        row.Data,
			OwnerUserID: row.OwnerID, // "$owner" source: EFFECTIVE owner (owner_id ?? created_by), not the creator
			CreatedAt:   row.CreatedAt,
			OccurredAt:  readAt,
		}
		// Re-park BEFORE firing. The action can fail (it is fail-open and recorded as a failed run),
		// and a recurrence must not stop because one occurrence's email bounced — the next reminder
		// is exactly what the rule was written for. Parking first also means a crash between the
		// two leaves an extra wake-up rather than a lost chain, and an extra one is idempotent-ish
		// (the date/rule premises are re-checked at its own fire time) while a lost one is silent.
		rearmTimerRepeat(ctx, queue, job, p, wf, row, logger)

		if p.WallClock {
			// #105: this wake-up's instant was wall-clock; the run it becomes says so.
			note := workflow.TimerWallClockWarning
			if strings.TrimSpace(wf.Condition.Timer.CalendarID) != "" {
				note = workflow.TimerPinnedCalendarWarning
			}
			ctx = workflow.WithRunWarning(ctx, note)
		} else if p.RepeatWallClock {
			ctx = workflow.WithRunWarning(ctx, workflow.TimerRepeatWallClockWarning)
		}
		if err := engine.FireWorkflow(ctx, wf, evt); err != nil {
			// Already logged + recorded as a FAILED run inside the engine (fail-open, same as the
			// async path). Do NOT return it: a queue retry would double-run earlier actions.
			logger.Warn("workflow timer: action error at fire (fail-open, recorded in run history)",
				zap.String("workflow_id", p.WorkflowID),
				zap.String("tenant_id", job.TenantID),
				zap.String("record_id", p.RecordID),
				zap.Error(err))
		}
		return nil
	}
}

// rearmTimerRepeat parks the NEXT occurrence of a recurring date_offset rule, if there is one.
//
// The next instant is recomputed from the ANCHOR (this occurrence's own fire time plus one
// interval would drift with every retry and every late worker pickup), so occurrence n always
// lands at the same absolute instant no matter when n-1 actually ran.
func rearmTimerRepeat(
	ctx context.Context,
	queue timerJobQueue,
	job jobs.Job,
	p workflowTimerJobPayload,
	wf workflow.Workflow,
	row *dynrepo.Record,
	logger *zap.Logger,
) {
	rep := wf.Condition.Timer.Repeat
	if rep == nil {
		return
	}
	next := p.Occurrence + 1
	if next >= rep.Count {
		return // the chain is complete — a bounded recurrence ends on its own
	}
	if queue == nil {
		logger.Warn("workflow timer: recurrence cannot re-park (no queue wired) — fired once only",
			zap.String("workflow_id", p.WorkflowID), zap.String("tenant_id", job.TenantID))
		return
	}

	// The anchor is the value this occurrence already verified as unmoved, so the base fire instant
	// is recoverable without a second read.
	anchor, _, ok := workflow.ParseTimerDate(timerAnchorValue(row, wf.Condition.Timer.DateField))
	if !ok {
		return // cannot recompute → stop the chain rather than guess an instant
	}
	// Reuse ComputeFireAt for the BASE instant — it is the one place that knows business-hours
	// math, direction and the calendar. sched=nil is the wall-clock path: a business_hours offset
	// resolved its calendar when the FIRST wake-up was parked, and re-resolving it here would need
	// a calendar read this handler does not have. The repeat interval is plain calendar time by
	// design (see TimerRepeat), so the only thing lost is business-hours shaping of the base on a
	// re-park — stated rather than silently approximated.
	base, err := workflow.ComputeFireAt(*wf.Condition.Timer, anchor, nil)
	if err != nil {
		logger.Warn("workflow timer: recurrence cannot recompute the base instant — chain stops",
			zap.String("workflow_id", p.WorkflowID), zap.Error(err))
		return
	}
	fireAt := base.Add(workflow.RepeatOffsetFor(rep, next))

	nextPayload := p
	nextPayload.Occurrence = next
	// #447: sched=nil above means a business_hours base is wall-clock on every re-park, even when
	// the first occurrence resolved a calendar — so the run carries its own (truthful) note.
	nextPayload.RepeatWallClock = p.RepeatWallClock || wf.Condition.Timer.Offset.Unit == workflow.TimerUnitBusinessHours
	raw, err := json.Marshal(nextPayload)
	if err != nil {
		logger.Warn("workflow timer: recurrence marshal failed", zap.Error(err))
		return
	}
	if _, err := queue.Enqueue(ctx, jobs.Job{
		Type:     WorkflowTimerJobType,
		TenantID: job.TenantID,
		Payload:  raw,
		RunAt:    fireAt,
	}); err != nil {
		// Fail-open, and LOUD: the chain stops here, and the operator needs to know that rather
		// than wonder why reminder 3 of 5 never arrived.
		logger.Warn("workflow timer: could not park the next occurrence — recurrence stops here",
			zap.String("workflow_id", p.WorkflowID), zap.String("tenant_id", job.TenantID),
			zap.Int("occurrence", next), zap.Error(err))
	}
}
