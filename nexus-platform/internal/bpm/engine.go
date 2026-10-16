package bpm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// ----------------------------------------------------------------------------------------------
// Consumer-side interfaces (so the engine is unit-testable with fakes — no DB).
// ----------------------------------------------------------------------------------------------

// ProcessLoader loads the active approval processes of a (tenant, entity, trigger). Satisfied by
// *SQLRepository (optionally behind a TTL cache).
type ProcessLoader interface {
	ListActiveProcesses(ctx context.Context, tenantID, entityName string, trigger Trigger) ([]Process, error)
}

// TaskGuard is what every state write of an instance proves under the instance's row lock
// (specs/043 D-4, S3b): ExpectSeq is the seq the caller READ — the write wins only on it, so a
// write that left current_step and escalated as they were (a delegation, an ABA) still makes a
// stale decision lose; Actor, when set, must hold an open seat of the step (or be an admin).
type TaskGuard struct {
	ExpectSeq int64
	Actor     *Actor
}

// InstanceStore is the engine's view of the instance table: the idempotency check, the arm insert,
// the single-instance read, the open seats, and the one guarded transaction every state write goes
// through. Satisfied by *SQLRepository.
type InstanceStore interface {
	// HasOpenExclusive reports an open APPROVAL (lane 'exclusive') on the record — a tracking-lane
	// instance (S12b) never counts. The database's uq_ai_open is the real guard; this is the cheap
	// early answer that spares a process load and gives "already sent" its own message.
	HasOpenExclusive(ctx context.Context, tenantID, entityName, recordID string) (bool, error)
	// CreateInstance returns ErrAlreadyPending when uq_ai_open says the record is already waiting.
	CreateInstance(ctx context.Context, in CreateInstanceInput) (string, error)
	GetInstance(ctx context.Context, tenantID, id string) (*Instance, error)
	// FindByIdempotencyKey returns the instance a Submit with this key created and the hash of the
	// request that created it, or ErrInstanceNotFound (specs/043 S9b, uq_ai_idem).
	FindByIdempotencyKey(ctx context.Context, tenantID, key string) (*Instance, string, error)
	// ListOpenSeats is who holds the step the instance stands on — what authority is judged on
	// (AwaitsActor), the same rows the inbox reads.
	ListOpenSeats(ctx context.Context, tenantID, instanceID string) ([]Seat, error)
	// ListVisitSeats returns every seat, in any state, of the latest visit of node (S4).
	ListVisitSeats(ctx context.Context, tenantID, instanceID, nodeID string) ([]VisitSeat, error)
	// ResolveSeatUsers turns seats into the users that hold them — a user seat is its user, a role /
	// team / queue seat the people behind it — at most limit, tenant-pinned (S6a: who gets pinged).
	ResolveSeatUsers(ctx context.Context, tenantID string, seats []Seat, limit int) ([]string, error)
	// VoteTaskTx records one vote on a parallel step and, in the SAME transaction, counts the
	// visit: a vote that does not finish the step only closes the voter's seat and appends the
	// history; one that does applies onComplete and closes the sibling seats (S4).
	VoteTaskTx(ctx context.Context, tenantID, id string, g VoteGuard, onComplete InstanceTransition) (*VoteResult, error)
	// DecideTaskTx applies next + historyJSON in ONE transaction: lock the instance row, check
	// g.ExpectSeq (ErrConcurrent), that it is still pending (ErrNotPending) and that g.Actor holds
	// a seat (ErrNotAuthorized), then write the instance and its seats. Used by decide, recall,
	// delegate and every history append — one guard, so no verb can write around it.
	DecideTaskTx(ctx context.Context, tenantID, id string, g TaskGuard, next InstanceTransition, historyJSON []byte) error
}

// ProcessGetter reads one process definition (its steps) by id — the decision path needs the steps
// array to know what the NEXT step's approver is. Satisfied by *SQLRepository.
type ProcessGetter interface {
	GetProcess(ctx context.Context, tenantID, id string) (*ProcessRecord, error)
}

// InboxWriter writes an in-app notification (a step's approver gets pinged that something awaits
// their decision). Satisfied by *SQLRepository. Optional: a nil writer disables the notice.
type InboxWriter interface {
	InsertNotification(ctx context.Context, n Notification) (string, error)
}

// RecordWriter applies the terminal write-back to the record an approval decided about. Satisfied
// by an adapter over *services.RecordService, so the write goes through the SAME validated path a
// human edit takes — picklist membership, computed-field refusal and field-ACL all still apply.
// Optional: nil disables write-back (the historical behaviour).
type RecordWriter interface {
	// actorUserID is the APPROVER. It is threaded rather than left blank because it becomes the
	// record's updated_by and the audit event's actor: without it the write-back shows up in the
	// record's history as an anonymous system change, on the one action where "who decided this"
	// is the entire point.
	SetField(ctx context.Context, tenantID, entityName, recordID, field string, value any, actorUserID string) error
}

// Recorder is the engine's view of the audit trail (who approved/rejected what, when, why). It
// mirrors internal/audit.Auditor's contract but is declared here so bpm keeps no hard audit
// dependency; the wiring passes an adapter over the real Auditor. Optional: nil disables auditing.
type Recorder interface {
	RecordApprovalDecision(ctx context.Context, ev ApprovalAuditEvent)
}

// ApprovalAuditEvent is the audit payload emitted on every approval decision (append-only trail).
type ApprovalAuditEvent struct {
	TenantID    string
	ActorUserID string
	EntityName  string
	RecordID    string
	InstanceID  string
	// ProcessID + Definition say WHICH chain Step indexes into (DAILY-QUEUE #70). A bare step
	// index is not self-describing: "approved step 1" means nothing once the instance's pinned
	// snapshot is gone (row purged, or migration 000065 rolled back), and before the pin it could
	// even mean a different step than the one shown today. Definition is the steps + outcome the
	// engine actually walked for THIS decision — the pinned copy, or, for an instance armed before
	// 000065, the live process as read at decision time. nil only on the SLA path for a pre-000065
	// instance (the scheduler does not load the live process).
	ProcessID  string
	Definition *Definition
	Step       int
	Action     string // HistoryApproved / HistoryRejected / HistoryEscalated
	Comment    string
	At         time.Time
}

// ----------------------------------------------------------------------------------------------
// Engine — the consumer-side arming interface RecordService depends on.
// ----------------------------------------------------------------------------------------------

// Event is the payload RecordService emits AFTER a record mutation commits, so the BPM engine can
// arm an approval if the record matches a process's criteria. Mirrors workflow.Event.
type Event struct {
	TenantID    string
	ActorUserID string
	EntityName  string
	RecordID    string
	Trigger     Trigger
	Data        map[string]any
	// Before is the record's committed field map as it stood BEFORE an update (nil on create).
	// The edge operators (changed / changedTo) read it: without it a process could only ask "is
	// the record in this state", so "start when the status BECOMES Chờ duyệt" re-armed on every
	// later save of a record already in that status (specs/043 S2b, FR-025). Same meaning as
	// workflow.Event.Before, filled by the same emit site.
	Before map[string]any
	// Depth is the automation chain depth of this event — the SAME counter workflow.Event.Depth
	// carries (workflow.ChainDepthFromContext): 0 for a person's mutation, parent+1 for a record
	// an automation wrote. The engine refuses to arm at or past the shared cap (FR-042), so a
	// chain rule → process → write-back → rule cannot loop forever.
	Depth int
}

// Engine is the consumer-side interface the service layer depends on for ARMING approvals. Emit is
// non-blocking by contract: it enqueues the event and returns immediately, so a slow/down database
// can never stall a record mutation. Implementations must be safe for concurrent use.
type Engine interface {
	Emit(ctx context.Context, evt Event)
}

// noopEngine is the default Engine wired into services so flows run unchanged when BPM is disabled.
type noopEngine struct{}

// NewNoopEngine returns an Engine that drops every event. Safe default for services/tests.
func NewNoopEngine() Engine { return noopEngine{} }

// Emit discards the event. It deliberately does nothing.
func (noopEngine) Emit(context.Context, Event) {}

// ----------------------------------------------------------------------------------------------
// AsyncEngine — the concrete arming dispatcher (async + fail-open, like the workflow dispatcher).
// ----------------------------------------------------------------------------------------------

// AsyncEngine is the concrete Engine. Emit pushes the record event onto a buffered channel and
// returns immediately; a pool of workers drains it, and for each event loads the active processes
// of the mutated entity, evaluates each criteria, and arms an instance on a match.
//
// Fail-open guarantees (same contract as the EPIC 4 workflow dispatcher):
//   - A full buffer makes Emit park the event as a durable bpm_arm job (WithArmJobs, S9a) — or, with
//     no queue / a failed park, DROP it with a zap warning. It NEVER blocks the calling record
//     mutation (already committed) and NEVER panics.
//   - After Shutdown, further Emits are dropped with a log line.
//   - A load / arm error is logged; it never propagates to the caller.
//   - A panic in processing is recovered so one bad event cannot shrink the worker pool.
//   - It NEVER logs record Data (business values) — only identifiers.
type AsyncEngine struct {
	ch        chan Event
	loader    ProcessLoader
	store     InstanceStore
	inbox     InboxWriter // optional: step-1 approver notice
	deadlines DeadlineCalculator
	router    Router      // optional: resolves field / submitter assignments (S6a)
	runner    Continuer   // optional: walks the machine nodes an armed instance starts on (specs/043 S7)
	armJobs   ArmJobQueue // optional: where a start event goes when the buffer is full (specs/043 S9a)
	now       func() time.Time
	logger    *zap.Logger
	// maxDepth is the automation chain cap shared with the workflow dispatcher: an event at
	// Depth >= maxDepth never arms (EngineConfig.MaxChainDepth).
	maxDepth int

	wg sync.WaitGroup

	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// EngineConfig configures an AsyncEngine. Buffer/Workers are clamped to a sane minimum.
type EngineConfig struct {
	Buffer  int
	Workers int
	// MaxChainDepth is the automation chain cap shared with the workflow dispatcher
	// (WORKFLOW_MAX_CHAIN_DEPTH): an event at Depth >= it never arms. < 1 → workflow.DefaultMaxChainDepth.
	MaxChainDepth int
}

// NewAsyncEngine builds and starts an arming engine: it allocates the buffered channel and launches
// Workers goroutines. The caller must call Shutdown for a graceful drain. loader/store may be nil —
// a nil loader/store makes every event a no-op (engine disabled). inbox may be nil. logger may be nil.
func NewAsyncEngine(cfg EngineConfig, loader ProcessLoader, store InstanceStore, inbox InboxWriter, logger *zap.Logger) *AsyncEngine {
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
	// Same clamp as workflow.AsyncDispatcher.WithRecordCreator: a mis-set 0 must neither disable
	// the guard nor stop every event from arming.
	maxDepth := cfg.MaxChainDepth
	if maxDepth < 1 {
		maxDepth = workflow.DefaultMaxChainDepth
	}
	e := &AsyncEngine{
		ch:       make(chan Event, buffer),
		loader:   loader,
		store:    store,
		inbox:    inbox,
		now:      time.Now,
		logger:   logger,
		maxDepth: maxDepth,
	}
	e.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go e.worker()
	}
	return e
}

// WithRunner arms the walk through the machine nodes a new instance starts on (a gateway first).
// Chainable.
func (e *AsyncEngine) WithRunner(c Continuer) *AsyncEngine {
	e.runner = c
	return e
}

// Emit schedules a record event for asynchronous approval-arming and returns at once. Non-blocking:
// a full buffer (or a shut-down engine) drops the event with a zap warning rather than blocking the
// record mutation (fail-open). It never carries record Data into the log — only identifiers.
// Exception: on a full buffer with a job queue wired, Emit parks the event as a bpm_arm job with ONE
// synchronous INSERT bounded by armParkTimeout (500ms), while holding e.mu.RLock — so during a burst
// on a slow database a request can wait up to that long, and a pending Shutdown write-lock queues
// later Emits behind it. Bounded and fail-open (timeout ⇒ dropped with a warning).
func (e *AsyncEngine) Emit(ctx context.Context, evt Event) {
	// The read lock is held THROUGH the send: Shutdown takes the write lock before it closes the
	// channel, so a send can never meet a closed channel. It used to be released first, and since an
	// auto node (specs/043 S7) writes the record from inside a worker — which emits an event from
	// the very goroutine Shutdown is waiting on — that window stopped being theoretical. The send
	// is non-blocking, so the lock is held for nanoseconds.
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		e.logger.Warn("bpm: event dropped, engine shut down",
			zap.String("trigger", string(evt.Trigger)),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName))
		return
	}
	select {
	case e.ch <- evt:
	default:
		// Busy system: park the event as a durable bpm_arm job instead of losing it (S9a, D-11).
		// Without a queue (JOBS_ENABLED off) — or when parking fails — it is dropped, loudly.
		if e.armJobs != nil {
			if err := e.parkArm(ctx, evt); err == nil {
				return
			} else {
				e.logger.Warn("bpm: buffer full and the bpm_arm job could not be parked",
					zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName), zap.Error(err))
			}
		}
		e.logger.Warn("bpm: event dropped, buffer full",
			zap.String("trigger", string(evt.Trigger)),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Int("buffer", cap(e.ch)))
	}
}

// EmitWait is Emit for a caller that cannot afford a dropped event (the CSV import job, specs/048
// S7b): a full buffer makes it wait until a slot frees or ctx ends; a shut-down engine is an error.
// The read lock is held through the send (see Emit) and the wait is bounded by ctx.
func (e *AsyncEngine) EmitWait(ctx context.Context, evt Event) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return fmt.Errorf("bpm.EmitWait: engine shut down")
	}
	select {
	case e.ch <- evt:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("bpm.EmitWait: %w", ctx.Err())
	}
}

// worker drains the channel until it is closed, arming one event at a time.
func (e *AsyncEngine) worker() {
	defer e.wg.Done()
	for evt := range e.ch {
		e.arm(evt)
	}
}

// arm handles a single event: load active processes of the entity+trigger, evaluate each criteria,
// and create an instance for the FIRST matching process (one approval per record). Every failure
// path is contained — a load/arm error is logged but never returned, and a panic is recovered.
func (e *AsyncEngine) arm(evt Event) {
	if err := e.tryArm(evt); err != nil {
		e.logger.Warn("bpm: arming failed (fail-open)",
			zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID), zap.Error(err))
	}
}

// tryArm is arm with its outcome: nil when the event was handled (armed, skipped, or the record
// was already waiting) and an error for a store failure worth retrying — the bpm_arm job returns it
// so the queue tries again, the in-memory worker only logs it (fail-open).
func (e *AsyncEngine) tryArm(evt Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("bpm: recovered panic while arming approval",
				zap.String("trigger", string(evt.Trigger)),
				zap.String("tenant_id", evt.TenantID),
				zap.String("entity_name", evt.EntityName),
				zap.Any("panic", r))
			err = fmt.Errorf("bpm.tryArm: panic: %v", r)
		}
	}()
	if e.loader == nil || e.store == nil {
		return nil // engine disabled
	}
	// FR-042: rule → record → process → write-back → rule is a chain, and the workflow dispatcher
	// already bounds it for create_record at the same depth. Arming here is one more hop, so the
	// same cap applies — past it the event is dropped loudly instead of looping.
	if evt.Depth >= e.maxDepth {
		e.logger.Warn("bpm: event skipped, automation chain depth cap reached",
			zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID), zap.Int("depth", evt.Depth), zap.Int("max_depth", e.maxDepth))
		return nil
	}
	ctx := context.Background()
	if evt.Trigger == TriggerAfterUpdate {
		e.signalWaits(ctx, evt)  // an update may be the answer a resting wait node is waiting for (S12a)
		e.signalPauses(ctx, evt) // …or the reason to stop (or restart) an SLA clock (S13)
	}
	procs, err := e.loader.ListActiveProcesses(ctx, evt.TenantID, evt.EntityName, evt.Trigger)
	if err != nil {
		return fmt.Errorf("bpm.tryArm: load active processes: %w", err)
	}
	approvalArmed := false
	for _, p := range procs {
		if !matchesCriteria(p, evt.Data, evt.Before) {
			continue
		}
		// A process with no steps cannot be armed (nothing to approve) — skip it. Nor can a graph
		// this binary does not run (a node kind or key of a later slice): fail closed (D-2/D-7).
		if len(p.Chain()) == 0 {
			e.logger.Warn("bpm: matched process has no steps this engine runs; skipping",
				zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID))
			continue
		}
		// The tracking lane (S12b) runs BESIDE an approval: it never looks at the exclusive guard and
		// never consumes the "one approval per event" slot. uq_ai_open keeps it to one per process.
		if p.Options.IsIndependent() {
			if f, why := CheckLane(p.Options, p.Trigger, GraphKinds(p.Graph)); f != "" {
				e.logger.Warn("bpm: tracking-lane process cannot run; skipping",
					zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID), zap.String("reason", why))
				continue
			}
			if err := e.armProcess(ctx, p, evt); err != nil {
				return err
			}
			continue
		}
		if approvalArmed {
			continue // arm at most one approval per record (first match wins)
		}
		// Idempotency: do not double-arm a record that is already pending approval.
		pending, err := e.store.HasOpenExclusive(ctx, evt.TenantID, evt.EntityName, evt.RecordID)
		if err != nil {
			return fmt.Errorf("bpm.tryArm: open-approval check: %w", err)
		}
		approvalArmed = true
		if pending {
			continue
		}
		if err := e.armProcess(ctx, p, evt); err != nil {
			return err
		}
	}
	return nil
}

// armProcess creates the instance on the first step of a matched process and pings the first
// approver's inbox. A record that is already waiting (uq_ai_open) is a success, not an error: the
// event was handled by whoever armed it first. Any other insert failure is returned (the caller
// decides: the worker logs, the bpm_arm job retries); an inbox error is only logged.
func (e *AsyncEngine) armProcess(ctx context.Context, p Process, evt Event) error {
	def := pinDefinition(p) // pinned WITH ids and explicit clocks — see Definition, pinDefinition
	chain := def.Chain()
	start := def.StartIndex() // the graph's start node, else the first step (S7)
	if start < 0 {
		e.logger.Warn("bpm: matched process starts on a node it does not have; skipping",
			zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID))
		return nil
	}
	first := chain[start]
	now := e.now().UTC()
	var (
		due   *time.Time
		basis SLABasis
		seats []Seat // a machine start (gateway first) has none: the walk opens the first person's step
		stuck string // why nobody holds the first step ("" = someone does), S6a FR-018
	)
	if !first.IsMachine() {
		var err error
		due, basis, err = stepDue(ctx, e.deadlines, evt.TenantID, def.CalendarID(), chain, start, true, now)
		if err != nil {
			// Nobody is waiting on this path to show an error to, and a record event must never be
			// lost to a calendar hiccup (fail-open) — so stamp the wall clock, under its OWN basis:
			// "could not read the calendar" is not "this organisation has no calendar" (D-8).
			e.logger.Warn("bpm: business calendar unreadable — first-step deadline stamped on the wall clock",
				zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID),
				zap.String("record_id", evt.RecordID), zap.Error(err))
			t := now.Add(time.Duration(first.SLASeconds) * time.Second)
			due, basis = &t, SLABasisWallCalendarError
		}
		def.FirstDueBasis = basis
		// Who holds the first step: the named holder, or the one a rule finds (S6a). Nobody to be
		// found is not a reason to lose the approval — the instance is armed STUCK, visible to the
		// Nexus team.
		var rerr error
		seats, stuck, rerr = routeSeats(ctx, e.router,
			RouteInput{TenantID: evt.TenantID, ProcessID: p.ID, EntityName: evt.EntityName, RecordID: evt.RecordID, Submitter: evt.ActorUserID},
			def.Options, first, due, basis)
		if rerr != nil {
			e.logger.Warn("bpm: routing the first step failed — the approval is armed stuck",
				zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID),
				zap.String("record_id", evt.RecordID), zap.Error(rerr))
			seats, stuck = nil, routeErrMessage
		}
		if stuck != "" {
			e.logger.Warn("bpm: approval armed with nobody holding its first step",
				zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID), zap.String("record_id", evt.RecordID))
		}
	}
	approverType, approverID := projection(seats) // D-5: never a team/queue into the ENUM
	instID, err := e.store.CreateInstance(ctx, CreateInstanceInput{
		TenantID:            evt.TenantID,
		ProcessID:           p.ID,
		EntityName:          evt.EntityName,
		RecordID:            evt.RecordID,
		CurrentApproverType: approverType,
		CurrentApproverID:   approverID,
		SLADueAt:            due,
		Definition:          def,
		Seats:               seats,
		LastError:           stuck,
		CreatedBy:           evt.ActorUserID,
		StartStep:           start,
		Lane:                def.Lane(),
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyPending) {
			e.logger.Info("bpm: record already waiting on an approval — start event absorbed",
				zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID), zap.String("record_id", evt.RecordID))
			return nil
		}
		return fmt.Errorf("bpm.armProcess: create instance: %w", err)
	}
	e.logger.Info("bpm: approval instance armed",
		zap.String("tenant_id", evt.TenantID), zap.String("process_id", p.ID),
		zap.String("instance_id", instID), zap.String("record_id", evt.RecordID))
	if first.IsMachine() {
		// Starts on a gateway / auto / end node: nobody to ping, the walk finds the first person.
		if e.runner != nil {
			if err := e.runner.Continue(ctx, evt.TenantID, instID); err != nil {
				e.logger.Warn("bpm: walk after arming failed — the continue job will pick it up",
					zap.String("tenant_id", evt.TenantID), zap.String("instance_id", instID), zap.Error(err))
			}
		}
		return nil
	}
	// Ping everyone who holds the first step: the named user, and the people behind a role / team /
	// queue seat (S6a, FR-019).
	e.notifyApprover(ctx, evt.TenantID, evt.EntityName, evt.RecordID, first, seats...)
	return nil
}

// notifyApprover pings the holders of the first step: the seats it was routed to, or — with none
// given — the seats the step names outright.
func (e *AsyncEngine) notifyApprover(ctx context.Context, tenantID, entity, recordID string, step Step, seats ...Seat) {
	if len(seats) == 0 {
		seats = stepSeats(step, nil, "")
	}
	notifyHolders(ctx, e.store, e.inbox, e.logger, tenantID, entity, recordID, step, "đang chờ bạn phê duyệt", seats)
}

// stepNotice is the in-app notice a step's holder gets. A task is work to do, not a decision to
// make, so it says so ("Việc mới") — the customer face never tells someone to approve a checklist.
// approvalTail finishes the approval sentence ("đang chờ bạn phê duyệt").
func stepNotice(step Step, entity, approvalTail string) (title, message string) {
	if step.IsTask() {
		return "Việc mới", fmt.Sprintf("Một bản ghi %s có việc đang chờ bạn xử lý.", entity)
	}
	return "Cần phê duyệt", fmt.Sprintf("Một bản ghi %s %s.", entity, approvalTail)
}

// Shutdown closes the channel and waits for all workers to finish draining the buffered events
// (graceful drain). It is idempotent. After Shutdown, Emit drops events with a log line.
func (e *AsyncEngine) Shutdown(ctx context.Context) error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		close(e.ch)
	})
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ----------------------------------------------------------------------------------------------
// Decision engine — the SYNCHRONOUS approve/reject path (a user action returning a status).
// ----------------------------------------------------------------------------------------------

// DecisionKind is approve or reject.
type DecisionKind string

const (
	DecisionApprove DecisionKind = "approve"
	DecisionReject  DecisionKind = "reject"
	// DecisionComplete finishes a TASK step (specs/043 S5). It advances like an approve, but it is
	// the only decision a task accepts — and a task refuses approve / reject.
	DecisionComplete DecisionKind = "complete"
)

// Decider applies a user's approve/reject decision to a pending instance. It is the synchronous
// counterpart to the async arming engine: errors ARE surfaced (a user is waiting on the result).
type Decider struct {
	store      InstanceStore
	procs      ProcessGetter
	inbox      InboxWriter
	auditor    Recorder
	records    RecordWriter      // optional: nil → a decision changes no field (pre-APPROVAL-OUTCOME)
	submitters SubmitterResolver // optional: who a rejection returned to the submitter goes back to (S8)
	deadlines  DeadlineCalculator
	router     Router    // optional: resolves field / submitter assignments of the step a decision opens (S6a)
	runner     Continuer // optional: walks the machine nodes a decision lands on (specs/043 S7)
	now        func() time.Time
	logger     *zap.Logger
}

// WithRunner arms the walk through gateway / auto / end nodes after a decision. Without it an
// instance a decision lands on a machine node stays there until the bpm_continue job picks it up.
func (d *Decider) WithRunner(c Continuer) *Decider {
	d.runner = c
	return d
}

// WithRecordWriter arms the terminal write-back.
//
// Before this, an approval decision produced a terminal instance state that NOTHING in the system
// read: StatusApproved had one producer and zero consumers outside this package. The quote everyone
// signed off kept whatever status it already had, so approving was a note in an inbox that no
// screen, rule or report could act on. Chainable.
func (d *Decider) WithRecordWriter(w RecordWriter) *Decider {
	d.records = w
	return d
}

// NewDecider wires the decision engine. inbox/auditor may be nil (notices/audit disabled).
func NewDecider(store InstanceStore, procs ProcessGetter, inbox InboxWriter, auditor Recorder, logger *zap.Logger) *Decider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Decider{store: store, procs: procs, inbox: inbox, auditor: auditor, now: time.Now, logger: logger}
}

// Decision errors. ErrNotPending: the instance is already terminal. ErrNotAuthorized: the actor is
// not the current step's approver. ErrInstanceNotFound is re-exported from the repo for the handler.
var (
	ErrNotPending    = fmt.Errorf("approval instance is not pending")
	ErrNotAuthorized = fmt.Errorf("actor is not the current approver")
	ErrConcurrent    = fmt.Errorf("approval step changed concurrently")
	// errRouteNeeded: a vote prepared WITHOUT routing the next step (VoteGuard.Unrouted) turned out to
	// finish the step after all. Nothing was written; the caller prepares the transition again.
	errRouteNeeded = fmt.Errorf("vote finished the step but the next step was not routed")
	// ErrSeatClaimed: the actor is a candidate of the current step's pool seat, but someone else has
	// claimed it (specs/043 S5). Distinct from ErrNotAuthorized so a candidate hears "taken", while
	// a stranger still hears nothing.
	ErrSeatClaimed = fmt.Errorf("pool seat is claimed by another user")
	// ErrWrongStepKind: approve / reject on a task step, or complete on an approval step.
	ErrWrongStepKind = fmt.Errorf("decision does not fit the kind of the current step")
	// ErrAlreadyDecidedByActor: the actor has already voted in this visit of a parallel step —
	// one person never decides two seats of the same step (FR-022).
	ErrAlreadyDecidedByActor = fmt.Errorf("actor already decided this step")
	// ErrAdminOverrideRequired: an admin who holds no seat of a step with several seats cannot
	// "approve" it like a seat holder — it would silently settle a council's vote. They use the
	// explicit override, which records why (FR-023, D-13).
	ErrAdminOverrideRequired = fmt.Errorf("admin must override a multi-seat step explicitly")
)

// Actor is the verified caller making a decision: their user id plus the role ids they hold (used
// to authorize a role-targeted step).
type Actor struct {
	UserID  string
	RoleIDs []string
	// TeamIDs are the teams the actor is a member of (S6a): a team seat is a pool its members hold.
	// Filled by the service that builds the actor (it knows the team directory); nil = none known.
	TeamIDs []string
	IsAdmin bool
	// QueueView is the actor's entity-view ACL for queue seats (see EntityView); nil = not judged.
	QueueView EntityView
}

// HolderIDs are the ids a seat can name the actor by: their roles and their teams. A seat's type
// (role / team) says which it is; the ids are distinct uuids, so one list serves both.
func (a Actor) HolderIDs() []string {
	if len(a.TeamIDs) == 0 {
		return a.RoleIDs
	}
	return append(append(make([]string, 0, len(a.RoleIDs)+len(a.TeamIDs)), a.RoleIDs...), a.TeamIDs...)
}

// Decide applies kind (approve/reject) with an optional comment to instance id in tenant. On
// approve of the LAST step the instance becomes 'approved'; on approve of a non-last step it
// advances to the next step (new approver + SLA). On reject the instance becomes 'rejected'
// (terminal — no loop-back in M1). The transition is written in one transaction under the
// instance's row lock and a seq guard (DecideTaskTx), so two concurrent decisions cannot both
// apply (ErrConcurrent).
// It appends a history entry and emits an audit event. Returns the updated instance.
func (d *Decider) Decide(ctx context.Context, tenantID, id string, kind DecisionKind, comment string, actor Actor) (*Instance, error) {
	return d.DecideAt(ctx, tenantID, id, kind, comment, actor, nil)
}

// DecideAt is Decide for a caller that tells which step it SAW (expectedStep, the index the
// screen rendered). Decide re-reads the instance inside the request, so a second click that
// arrives AFTER the first committed reads the NEW step, passes authority (an admin, or one person
// seated on two consecutive steps) and would approve a step the clicker never saw: only the step
// the client saw tells the click is stale (ErrConcurrent). nil = an older client that sends none:
// it decides on what the server reads, as before.
func (d *Decider) DecideAt(ctx context.Context, tenantID, id string, kind DecisionKind, comment string, actor Actor, expectedStep *int) (*Instance, error) {
	inst, err := d.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err // ErrInstanceNotFound or a wrapped DB error
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	// Authority first, before any calendar read or next-step work: the actor must hold a seat of
	// this step (an admin always may). DecideTaskTx judges it again under the row lock.
	seats, err := d.store.ListOpenSeats(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.Decide: read seats: %w", err)
	}
	if err := authorize(inst, seats, actor); err != nil {
		if errors.Is(err, ErrNotAuthorized) {
			// A person who already cast their vote on this step has no seat left on it: say so
			// (FR-022) rather than "you are not the approver", which is untrue and unhelpful.
			visit, verr := d.store.ListVisitSeats(ctx, tenantID, id, seatNodeID(inst))
			if verr != nil {
				return nil, fmt.Errorf("bpm.Decider.Decide: read visit: %w", verr)
			}
			if DecidedIn(visit, actor.UserID) {
				return nil, ErrAlreadyDecidedByActor
			}
			// A click from a screen that showed a step the instance has since LEFT (a rejection that
			// returned it, S8): someone who held a seat on the step they saw knows the instance
			// exists, so they hear "it changed" (409), not "you are nobody here". A stranger still
			// hears the refusal.
			if expectedStep != nil && *expectedStep != inst.CurrentStep && d.hadSeatOnStep(ctx, inst, *expectedStep, actor) {
				return nil, ErrConcurrent
			}
		}
		return nil, err
	}
	// Judged after authority so a non-seat-holder learns nothing about which step it is on.
	if expectedStep != nil && *expectedStep != inst.CurrentStep {
		return nil, ErrConcurrent
	}

	// The steps this instance walks + the outcome it writes: the definition pinned at arm time.
	def, err := d.definitionOf(ctx, inst)
	if err != nil {
		return nil, err
	}
	chain := def.Chain()
	if reworkSeatOf(inst, seats) { // with the submitter (S8): the only verb is "Gửi lại" = complete
		if kind != DecisionComplete {
			return nil, ErrWrongStepKind
		}
		return d.resubmit(ctx, inst, def, comment, actor)
	}
	if inst.CurrentStep >= 0 && inst.CurrentStep < len(chain) && chain[inst.CurrentStep].Parallel() {
		if kind == DecisionComplete { // a council step is always an approval
			return nil, ErrWrongStepKind
		}
		return d.vote(ctx, inst, def, chain, kind, comment, actor)
	}
	return d.decideStep(ctx, inst, def, kind, comment, actor, closeSeatsAs(kind), "")
}

// hadSeatOnStep reports whether the actor held (or decided) a seat on step i of the instance's
// pinned chain, in any visit. Errors read as "no": the caller then answers the plain refusal.
func (d *Decider) hadSeatOnStep(ctx context.Context, inst *Instance, i int, actor Actor) bool {
	def, err := d.definitionOf(ctx, inst)
	if err != nil {
		return false
	}
	node, _ := def.StepRef(i)
	if node == "" {
		return false
	}
	visit, err := d.store.ListVisitSeats(ctx, inst.TenantID, inst.ID, node)
	if err != nil {
		return false
	}
	for _, v := range visit {
		if v.DecidedBy == actor.UserID ||
			(v.AssigneeType == ApproverUser && v.AssigneeID == actor.UserID) ||
			(v.AssigneeType == ApproverRole && holdsRole(actor.RoleIDs, v.AssigneeID)) {
			return true
		}
	}
	return false
}

// closeSeatsAs is the outcome the acting seat ends with for a plain decision.
func closeSeatsAs(kind DecisionKind) SeatOutcome {
	switch kind {
	case DecisionReject:
		return SeatRejected
	case DecisionComplete:
		return SeatCompleted
	}
	return SeatApproved
}

// decideStep settles the CURRENT step of inst in one go — the step ends with `kind`, every open
// seat of it closes as closeAs, and the instance moves on (or becomes terminal): the path of a
// single-seat step, and of an admin's override of a many-seat one. via marks an override on the
// history entry. The caller has already judged authority; DecideTaskTx judges it again.
func (d *Decider) decideStep(ctx context.Context, inst *Instance, def *Definition, kind DecisionKind, comment string, actor Actor, closeAs SeatOutcome, via string) (*Instance, error) {
	tenantID, id := inst.TenantID, inst.ID
	now := d.now().UTC()
	chain := def.Chain()
	// A task is completed, an approval is approved or rejected: the verb must fit the step it lands
	// on (specs/043 S5). Judged after authority, like expectedStep, so a stranger learns no kind.
	// A machine node (S7) takes no decision at all: an admin cannot "approve" a gateway.
	if inst.CurrentStep < len(chain) && (chain[inst.CurrentStep].IsMachine() || chain[inst.CurrentStep].IsTask() != (kind == DecisionComplete)) {
		return nil, ErrWrongStepKind
	}
	action := HistoryApproved
	switch kind {
	case DecisionReject:
		action = HistoryRejected
	case DecisionComplete:
		action = HistoryCompleted
	}
	next, err := d.transition(inst, def, kind)
	if err != nil {
		return nil, err
	}
	// A rejection that does not end the approval (onReject, S8) opens another step or the
	// submitter's rework seat instead of a terminal state.
	ret, err := d.returnOf(ctx, inst, def, kind, comment, now, via)
	if err != nil {
		return nil, err
	}
	// The step this decision opens gets its deadline here — on the calendar when its pinned clock
	// says business (D-9: an unpinned, pre-000065 instance stays on the wall clock). A calendar the
	// store cannot read FAILS the decision (D-8): the approver is waiting on the answer, and a
	// deadline silently computed on the wrong clock is indistinguishable from a right one after.
	var basis SLABasis
	// The actor's seat closes with their decision; the step it opens gets its seats and the
	// projection of them (specs/043 S3a dual write, D-5) — in the same transaction as the CAS.
	next.Seats = SeatWrite{Close: closeAs, Node: seatNodeID(inst), By: actor.UserID, Comment: comment}
	if ret != nil {
		next, basis = ret.next, ret.basis
		next.Seats.Close, next.Seats.Node, next.Seats.By, next.Seats.Comment = closeAs, seatNodeID(inst), actor.UserID, comment
	} else if next.Status == StatusPending && chain[next.CurrentStep].IsMachine() {
		next.Continue = true // lands on a gateway / auto / end node: nobody holds it, the walk goes on (S7)
	} else if next.Status == StatusPending {
		next.SLADueAt, basis, err = stepDue(ctx, d.deadlines, tenantID, inst.Definition.CalendarID(), chain, next.CurrentStep, inst.Definition != nil, now)
		if err != nil {
			return nil, fmt.Errorf("bpm.Decider.Decide: next-step deadline: %w", err)
		}
		// The approver is waiting on this answer: a store that cannot say who holds the next step
		// fails the decision (like the calendar above), while "nobody found" is an answer — the
		// decision stands and the step is stuck, visible to the Nexus team (FR-018).
		if next.Seats.Open, next.LastError, err = d.seatsFor(ctx, inst, def, chain[next.CurrentStep], next.SLADueAt, basis); err != nil {
			return nil, fmt.Errorf("bpm.Decider.Decide: route next step: %w", err)
		}
		next.CurrentApproverType, next.CurrentApproverID = projection(next.Seats.Open)
	}
	node, stepName := def.StepRef(inst.CurrentStep)
	newHistory := append(inst.History, HistoryEntry{
		Step:     inst.CurrentStep,
		Action:   action,
		Actor:    actor.UserID,
		Comment:  comment,
		Node:     node,
		StepName: stepName,
		SLABasis: basis, // the basis of the deadline THIS transition set (Instance.CurrentSLABasis)
		Via:      via,
		Version:  def.version(),
		At:       now,
	})
	if ret != nil {
		newHistory[len(newHistory)-1].ReturnedTo, newHistory[len(newHistory)-1].Target = ret.returnedTo, ret.submitter
	}
	historyJSON, err := json.Marshal(newHistory)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.Decide: marshal history: %w", err)
	}

	// ErrConcurrent: another write (a decision in another tab, a delegation, an escalation) moved
	// the instance between GetInstance and this transaction — surfaced, never silently lost.
	if err := d.store.DecideTaskTx(ctx, tenantID, id, TaskGuard{ExpectSeq: inst.Seq, Actor: &actor}, next, historyJSON); err != nil {
		return nil, txError("bpm.Decider.Decide", err)
	}

	// Audit + (on a non-terminal advance) ping the next approvers. Best-effort — a notice/audit
	// failure does not undo the committed decision.
	d.audit(ctx, inst, def, action, actor.UserID, comment, now)
	switch {
	case ret != nil:
		d.afterReturn(ctx, inst, def, ret, comment, actor.UserID)
	case next.Status == StatusPending:
		d.notifySeats(ctx, inst, chain[next.CurrentStep], next.Seats.Open)
	default:
		d.applyOutcome(ctx, inst, def.Outcome, next.Status, actor.UserID)
	}

	// Reflect the new state on the returned instance.
	inst.History = newHistory
	inst.CurrentStep = next.CurrentStep
	inst.Status = next.Status
	inst.CurrentApproverType = next.CurrentApproverType
	inst.CurrentApproverID = next.CurrentApproverID
	inst.SLADueAt = next.SLADueAt
	inst.Escalated = next.Escalated
	inst.LastError = next.LastError
	inst.UpdatedAt = now
	return d.walk(ctx, inst, next), nil
}

// walk goes on from a machine node the decision landed the instance on, and returns the instance
// as the walk left it (the next person's step, or a terminal state). A walk that fails is not the
// decision's failure — the decision is committed; the instance stands on the machine node with a
// bpm_continue job parked to pick it up. The decided instance is returned when there is nothing to
// walk or the fresh read fails.
func (d *Decider) walk(ctx context.Context, inst *Instance, next InstanceTransition) *Instance {
	if !next.Continue || d.runner == nil {
		return inst
	}
	if err := d.runner.Continue(ctx, inst.TenantID, inst.ID); err != nil {
		d.logger.Warn("bpm: walk after a decision failed — the continue job will pick it up",
			zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.Error(err))
	}
	if fresh, err := d.store.GetInstance(ctx, inst.TenantID, inst.ID); err == nil {
		return fresh
	}
	return inst
}

// seatsFor is routeSeats for the step an instance is about to enter: the submitter is whoever
// created the instance, the fallback is the one the instance's pinned definition carries.
func (d *Decider) seatsFor(ctx context.Context, inst *Instance, def *Definition, step Step, due *time.Time, basis SLABasis) ([]Seat, string, error) {
	var opts *Options
	if def != nil {
		opts = def.Options
	}
	return routeSeats(ctx, d.router,
		RouteInput{TenantID: inst.TenantID, ProcessID: inst.ProcessID, EntityName: inst.EntityName, RecordID: inst.RecordID, Submitter: inst.CreatedBy},
		opts, step, due, basis)
}

// vote is the decision on a PARALLEL step (specs/043 S4): the actor decides their own seat, and
// the step moves only when the votes add up (castVote, counted in VoteTaskTx under the row lock).
//
// The transition for "this vote finishes the step" is prepared HERE, before the transaction,
// because it needs the calendar for the next step's deadline — and it is prepared for every vote,
// since which vote is the last one is only known under the lock (two last approvers pressing
// together each read "one seat to go"). The transaction applies it or discards it.
func (d *Decider) vote(ctx context.Context, inst *Instance, def *Definition, chain []Step, kind DecisionKind, comment string, actor Actor) (*Instance, error) {
	tenantID, id := inst.TenantID, inst.ID
	step := chain[inst.CurrentStep]
	node := seatNodeID(inst)
	visit, err := d.store.ListVisitSeats(ctx, tenantID, id, node)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.vote: read visit: %w", err)
	}
	seat := ActorSeat(visit, actor)
	switch {
	case seat == nil && actor.IsAdmin:
		// Not a council member: approving would silently settle everyone else's vote (D-13).
		return nil, ErrAdminOverrideRequired
	case seat == nil && DecidedIn(visit, actor.UserID):
		return nil, ErrAlreadyDecidedByActor
	case seat == nil:
		return nil, ErrNotAuthorized
	case DecidedIn(visit, actor.UserID):
		return nil, ErrAlreadyDecidedByActor // the transaction judges this again under the lock
	}

	now := d.now().UTC()
	action := HistoryApproved
	if kind == DecisionReject {
		action = HistoryRejected
	}
	stepNode, stepName := def.StepRef(inst.CurrentStep)
	// A step after this one may deal its holder out with a cursor (round_robin / load_based): asking
	// turns the cursor, so only the vote that really opens that step may ask. The transition below is
	// prepared for every vote (which vote is the last is only known under the lock), so when this
	// vote visibly leaves the step open it is prepared WITHOUT routing, and the transaction refuses
	// (errRouteNeeded) in the rare case a sibling's vote lands first and this one finishes the step.
	unrouted := false
	if def.hasStatefulRouting() {
		_, verdict, perr := castVote(visit, seat.ID, actor.UserID, closeSeatsAs(kind), step.CompletionPolicy(), comment, now)
		unrouted = perr == nil && verdict == VerdictPending
	}
	var (
		next  InstanceTransition
		ret   *returnPlan
		basis SLABasis
		res   *VoteResult
	)
	for {
		var perr error
		if next, ret, basis, perr = d.prepareVote(ctx, inst, def, chain, kind, comment, now, unrouted); perr != nil {
			return nil, perr
		}
		entry := HistoryEntry{
			Step: inst.CurrentStep, Action: action, Actor: actor.UserID, Comment: comment,
			Node: stepNode, StepName: stepName, Version: def.version(), At: now,
		}
		if ret != nil { // VoteTaskTx drops these again when the vote does not finish the step
			entry.ReturnedTo, entry.Target = ret.returnedTo, ret.submitter
		}
		res, err = d.store.VoteTaskTx(ctx, tenantID, id, VoteGuard{
			Actor: actor, ExpectStep: inst.CurrentStep, Node: node, SeatID: seat.ID,
			Kind: closeSeatsAs(kind), Mode: step.CompletionMode(), Percent: step.CompletionPolicy().Percent, Comment: comment,
			Entry:         entry,
			CompleteBasis: basis,
			Unrouted:      unrouted,
		}, next)
		if errors.Is(err, errRouteNeeded) && unrouted {
			unrouted = false // the prediction was wrong: route the next step and go again
			continue
		}
		break
	}
	if err != nil {
		return nil, txError("bpm.Decider.vote", err)
	}

	d.audit(ctx, inst, def, action, actor.UserID, comment, now)
	inst.History = res.History
	inst.UpdatedAt = now
	if !res.Completed {
		return inst, nil // one vote more; the step is still open
	}
	switch {
	case ret != nil:
		d.afterReturn(ctx, inst, def, ret, comment, actor.UserID)
	case next.Status == StatusPending:
		d.notifySeats(ctx, inst, chain[next.CurrentStep], next.Seats.Open)
	default:
		d.applyOutcome(ctx, inst, def.Outcome, next.Status, actor.UserID)
	}
	inst.CurrentStep = next.CurrentStep
	inst.Status = next.Status
	inst.CurrentApproverType = next.CurrentApproverType
	inst.CurrentApproverID = next.CurrentApproverID
	inst.SLADueAt = next.SLADueAt
	inst.Escalated = next.Escalated
	inst.LastError = next.LastError
	return d.walk(ctx, inst, next), nil
}

// prepareVote builds the transition "this vote finishes the step": where it leads (the next step with
// its deadline and its routed seats, a return to an earlier node or the submitter, or the end) —
// the half of a vote that needs the calendar and the router. unrouted skips all of it (the vote is
// known to leave the step open): the zero transition is then never applied.
func (d *Decider) prepareVote(ctx context.Context, inst *Instance, def *Definition, chain []Step, kind DecisionKind, comment string, now time.Time, unrouted bool) (InstanceTransition, *returnPlan, SLABasis, error) {
	if unrouted {
		return InstanceTransition{}, nil, "", nil
	}
	next, err := d.transition(inst, def, kind)
	if err != nil {
		return next, nil, "", err
	}
	// A rejection that finishes this step may return instead of ending (onReject, S8).
	ret, err := d.returnOf(ctx, inst, def, kind, comment, now, "")
	if err != nil {
		return next, nil, "", err
	}
	var basis SLABasis
	if ret != nil {
		next, basis = ret.next, ret.basis
	} else if next.Status == StatusPending && chain[next.CurrentStep].IsMachine() {
		next.Continue = true
	} else if next.Status == StatusPending {
		next.SLADueAt, basis, err = stepDue(ctx, d.deadlines, inst.TenantID, inst.Definition.CalendarID(), chain, next.CurrentStep, inst.Definition != nil, now)
		if err != nil {
			return next, nil, "", fmt.Errorf("bpm.Decider.vote: next-step deadline: %w", err)
		}
		if next.Seats.Open, next.LastError, err = d.seatsFor(ctx, inst, def, chain[next.CurrentStep], next.SLADueAt, basis); err != nil {
			return next, nil, "", fmt.Errorf("bpm.Decider.vote: route next step: %w", err)
		}
		next.CurrentApproverType, next.CurrentApproverID = projection(next.Seats.Open)
	}
	return next, ret, basis, nil
}

// Override lets an ADMIN decide the current step in place of its seats (specs/043 FR-023, D-13):
// the explicit command for a many-seat step, where a plain approve by someone who holds no seat
// is refused. The note is the reason and goes on the history entry (marked via=override); every
// open seat of the step closes as superseded — nobody on them decided anything.
//
// expectedStep is the step the admin's screen showed (as in DecideAt): the most powerful verb must
// not settle a step the clicker never saw, so a stale one is ErrConcurrent. nil = no guard.
func (d *Decider) Override(ctx context.Context, tenantID, id string, kind DecisionKind, note string, actor Actor, expectedStep *int) (*Instance, error) {
	if !actor.IsAdmin {
		return nil, ErrNotAuthorized
	}
	inst, err := d.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	if expectedStep != nil && *expectedStep != inst.CurrentStep {
		return nil, ErrConcurrent
	}
	def, err := d.definitionOf(ctx, inst)
	if err != nil {
		return nil, err
	}
	seats, err := d.store.ListOpenSeats(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.Override: read seats: %w", err)
	}
	if reworkSeatOf(inst, seats) { // the submitter is fixing the record: nothing to approve or reject yet
		return nil, ErrWrongStepKind
	}
	return d.decideStep(ctx, inst, def, kind, note, actor, SeatSuperseded, "override")
}

// definitionOf returns the steps + outcome this instance runs under. Pinned at arm time on the
// instance (migration 000065) — so an admin editing the live process cannot re-wire an approval
// already in flight (the silent step-skip DAILY-QUEUE #8 closed). An instance armed BEFORE the
// snapshot column existed carries nil and reads the live process, exactly as before; that
// fallback ends with the last pre-000065 instance.
func (d *Decider) definitionOf(ctx context.Context, inst *Instance) (*Definition, error) {
	if inst.Definition != nil {
		// CreateInstance refuses a 0-step definition, so this is unreachable through our own
		// writes — but the check belongs on the READ side too, because a 0-step snapshot makes
		// the next approve satisfy `next >= len(steps)` and terminal-approve the instance: the
		// exact bug 000065 closes. Refuse the decision rather than fall back to the live process.
		//
		// The same guard covers a pinned GRAPH this binary cannot run (a node of a later slice —
		// a rollback, a hand edit): Chain() is nil, and the decision is refused, never walked as
		// a shorter chain (specs/043 S3a2: "0 bước" became "0 bước người").
		if len(inst.Definition.Chain()) == 0 {
			return nil, fmt.Errorf("bpm.Decider.definitionOf: instance %s has a pinned definition with no approval step this engine runs", inst.ID)
		}
		return inst.Definition, nil
	}
	proc, err := d.procs.GetProcess(ctx, inst.TenantID, inst.ProcessID)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.Decide: load process: %w", err)
	}
	def := &Definition{Version: proc.Version} // an unpinned instance runs the live version
	// The live process in either form, graph first (D-2).
	if len(proc.Graph) > 0 && string(proc.Graph) != "null" {
		var g Graph
		if err := json.Unmarshal(proc.Graph, &g); err != nil {
			return nil, fmt.Errorf("bpm.Decider.Decide: decode graph: %w", err)
		}
		def.Steps, def.Graph = []Step{}, g.normalized()
	} else if len(proc.Steps) > 0 && string(proc.Steps) != "null" {
		if err := json.Unmarshal(proc.Steps, &def.Steps); err != nil {
			return nil, fmt.Errorf("bpm.Decider.Decide: decode steps: %w", err)
		}
		def.Steps = NormalizeSteps(def.Steps)
	}
	if len(def.Chain()) == 0 {
		return nil, fmt.Errorf("bpm.Decider.definitionOf: process %s has no approval step this engine runs", proc.ID)
	}
	// An unreadable outcome block skips the write-back, never the decision (fail-open, as before).
	if len(proc.Outcome) > 0 && string(proc.Outcome) != "null" {
		var o Outcome
		if err := json.Unmarshal(proc.Outcome, &o); err != nil {
			d.logger.Warn("bpm: approval outcome config unreadable",
				zap.String("process_id", proc.ID), zap.Error(err))
		} else {
			def.Outcome = &o
		}
	}
	return def, nil
}

// applyOutcome writes the (pinned) outcome's field back onto the decided record.
//
// BEST-EFFORT, deliberately. The decision is already committed by the time this runs, and it is
// the decision that people rely on; a failed write-back must not roll it back or turn a completed
// approval into an error the approver has to re-do. So a failure is logged loudly and the decision
// stands — the same fail-open posture the arming engine takes, for the same reason. That is also
// what makes a pinned outcome safe when its statusField was renamed after arming: the write is
// refused, logged, and the approval still stands.
//
// The write goes through the normal record path, so an invalid value is refused exactly as a human
// edit would be refused: configuring approvedValue as a picklist option that does not exist fails
// here rather than corrupting the record. That failure IS the signal the configuration is wrong.
func (d *Decider) applyOutcome(ctx context.Context, inst *Instance, o *Outcome, st InstanceStatus, actorUserID string) {
	if err := writeOutcome(ctx, d.records, d.logger, inst, o, st, actorUserID); err != nil {
		// FR-010: the line goes to the history too — the Nexus team reads the timeline, not the log.
		node, name := inst.Definition.StepRef(inst.CurrentStep)
		recordEffectFailed(ctx, d.store, d.logger, inst, node, name, "ghi kết quả duyệt lên hồ sơ không được: "+reason(err), d.now().UTC())
	}
}

// writeOutcome writes the outcome's field onto the decided record and returns the writer's error
// (nil when there was nothing to write). The log lines live here so every path that settles an
// instance — a person's decision and an end node alike — reports a refused write-back the same way.
func writeOutcome(ctx context.Context, w RecordWriter, logger *zap.Logger, inst *Instance, o *Outcome, st InstanceStatus, actorUserID string) error {
	if w == nil || o == nil {
		return nil
	}
	value, ok := o.ValueFor(st)
	if !ok {
		return nil
	}
	if err := w.SetField(ctx, inst.TenantID, inst.EntityName, inst.RecordID, o.StatusField, value, actorUserID); err != nil {
		logger.Error("bpm: approval decided but the record write-back FAILED",
			zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID),
			zap.String("entity", inst.EntityName), zap.String("record_id", inst.RecordID),
			zap.String("field", o.StatusField), zap.String("value", value), zap.Error(err))
		return err
	}
	logger.Info("bpm: approval outcome applied",
		zap.String("tenant_id", inst.TenantID), zap.String("record_id", inst.RecordID),
		zap.String("field", o.StatusField), zap.String("value", value))
	return nil
}

// canAct reports whether the actor may act on the instance's CURRENT step: an admin always may;
// otherwise they must hold one of its open seats — the inbox's own rule (AwaitsActor), so a row
// someone sees in their inbox is a row they can decide. seats is the instance's open seats; nil
// (an instance no seat mirrors, or a unit test) answers from the current_approver_* projection,
// the D-6 fallback AwaitsActor already owns.
//
// Package-level and shared because TWO paths ask the same question — approve/reject (Decider) and
// delegate (Requester). Delegation is the authority to decide, exercised differently: anyone who
// may approve may hand it on, and anyone who may not, may not. One body so the two can never
// drift into "you can give away a decision you were not allowed to make".
func canAct(inst *Instance, seats []Seat, actor Actor) bool {
	return actor.IsAdmin || AwaitsActor(inst, seats, actor.UserID, actor.HolderIDs(), actor.QueueView)
}

// authorize is canAct with the reason for a refusal: nil when the actor may act, ErrSeatClaimed
// when they are a candidate of a pool seat someone else has claimed (specs/043 S5), else
// ErrNotAuthorized. One body for decide, delegate and the transaction that re-judges both.
func authorize(inst *Instance, seats []Seat, actor Actor) error {
	switch {
	case canAct(inst, seats, actor):
		return nil
	case claimedAgainst(inst, seats, actor.UserID, actor.HolderIDs(), actor.QueueView):
		return ErrSeatClaimed
	default:
		return ErrNotAuthorized
	}
}

// CanDecide is canAct judged on the projection alone (no seats read) — what an instance's
// current_approver_* columns say about the actor.
func CanDecide(inst *Instance, actor Actor) bool { return canAct(inst, nil, actor) }

// txError returns the lifecycle sentinels DecideTaskTx answers with untouched (the service maps
// them to coded errors) and wraps anything else as a real failure.
func txError(op string, err error) error {
	switch {
	case errors.Is(err, ErrConcurrent), errors.Is(err, ErrNotPending), errors.Is(err, ErrNotAuthorized), errors.Is(err, ErrInstanceNotFound),
		errors.Is(err, ErrSeatClaimed), errors.Is(err, ErrWrongStepKind),
		errors.Is(err, ErrAlreadyDecidedByActor), errors.Is(err, ErrAdminOverrideRequired):
		return err
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// transition computes the new instance state for a decision. Reject → terminal 'rejected'. Approve
// of the last step → terminal 'approved'. Approve of a non-last step → advance to the next step,
// escalated reset for the new step. The new step's deadline, seats and approver projection are
// NOT set here: the deadline needs the calendar (stepDue) and the seats need the deadline, which
// Decide sets before anything is written.
func (d *Decider) transition(inst *Instance, def *Definition, kind DecisionKind) (InstanceTransition, error) {
	if kind == DecisionReject {
		return InstanceTransition{
			CurrentStep: inst.CurrentStep,
			Status:      StatusRejected,
			Escalated:   inst.Escalated, // unchanged; terminal
		}, nil
	}
	// Where an approved step leads: the next in the array, or the node it names (S7).
	nextStep, end, ok := def.Successor(inst.CurrentStep)
	if !ok {
		return InstanceTransition{}, fmt.Errorf("bpm.Decider.transition: step %d names a next step that is not in the process", inst.CurrentStep)
	}
	if end {
		// Approved the final step → fully approved.
		return InstanceTransition{
			CurrentStep: inst.CurrentStep,
			Status:      StatusApproved,
			Escalated:   inst.Escalated,
		}, nil
	}
	return InstanceTransition{
		CurrentStep: nextStep,
		Status:      StatusPending,
		Escalated:   false, // fresh step, not yet escalated
	}, nil
}

// returnOf is the answer to "does this decision return the approval instead of ending it": nil for
// everything but a rejection of a node whose onReject points somewhere other than @end. An override
// that cannot find the submitter ends the approval instead (the admin's explicit command must not
// be blocked by a missing person); an ordinary rejection is refused (ErrNoSubmitter).
func (d *Decider) returnOf(ctx context.Context, inst *Instance, def *Definition, kind DecisionKind, note string, now time.Time, via string) (*returnPlan, error) {
	if kind != DecisionReject {
		return nil, nil
	}
	or := def.OnRejectAt(inst.CurrentStep)
	if or.Terminal() {
		return nil, nil
	}
	plan, err := d.planReturn(ctx, inst, def, or, note, now)
	if errors.Is(err, ErrNoSubmitter) && via == "override" {
		return nil, nil
	}
	return plan, err
}

// audit emits an approval-decision audit event (best-effort). def is the definition this decision
// executed (definitionOf), so the audit row can say which chain inst.CurrentStep indexes into.
func (d *Decider) audit(ctx context.Context, inst *Instance, def *Definition, action, actorID, comment string, at time.Time) {
	if d.auditor == nil {
		return
	}
	d.auditor.RecordApprovalDecision(ctx, ApprovalAuditEvent{
		TenantID:    inst.TenantID,
		ActorUserID: actorID,
		EntityName:  inst.EntityName,
		RecordID:    inst.RecordID,
		InstanceID:  inst.ID,
		ProcessID:   inst.ProcessID,
		Definition:  def,
		Step:        inst.CurrentStep,
		Action:      action,
		Comment:     comment,
		At:          at,
	})
}

// notifySeats pings everyone who holds a seat of the step the instance just entered — the named
// user and the people behind a role / team / queue seat (routing.go notifyHolders); a task says
// "Việc mới" via stepNotice.
func (d *Decider) notifySeats(ctx context.Context, inst *Instance, step Step, seats []Seat) {
	notifyHolders(ctx, d.store, d.inbox, d.logger, inst.TenantID, inst.EntityName, inst.RecordID, step, "đang chờ bạn phê duyệt", seats)
}

// ----------------------------------------------------------------------------------------------
// Request engine — submit / recall / delegate. The verbs the SUBMITTER and the APPROVER need.
//
// Everything above this line serves the automatic path: a record is saved, the system decides
// whether an approval is owed, and failure is deliberately silent (fail-open — a broken approval
// rule must never block a save). Those are the wrong semantics for a person pressing a button.
// Here, a request that matched no process is an ERROR the caller must see, not a shrug.
// ----------------------------------------------------------------------------------------------

// Request-path errors, all surfaced to the caller.
var (
	// ErrNoApprovalProcess: nothing is configured to approve this record. Distinct from a
	// generic failure on purpose — the fix is an admin action (configure a manual process), and
	// a silent no-op here is exactly how someone ends up believing they sent something.
	ErrNoApprovalProcess = fmt.Errorf("no manual approval process matches this record")
	// ErrAlreadyPending: this record is already awaiting a decision. Re-submitting would either
	// double-arm it or silently do nothing; both are worse than saying so.
	ErrAlreadyPending = fmt.Errorf("record already has a pending approval")
	// ErrProcessNotApplicable: the sender named a process that is not an active manual process of
	// this entity, or whose entry criteria the record does not meet (S9b).
	ErrProcessNotApplicable = fmt.Errorf("the chosen approval process does not apply to this record")
	// ErrIdempotencyKeyReused: the Idempotency-Key was already used for a different request (S9b).
	ErrIdempotencyKeyReused = fmt.Errorf("idempotency key already used for a different request")
	// ErrIdempotencyKeyConflict is CreateInstance's answer when uq_ai_idem says another call already
	// holds the key (a race between two sends with one key); the sender then replays (S9b).
	ErrIdempotencyKeyConflict = fmt.Errorf("idempotency key already holds an instance")
	// ErrNotSubmitter: only the person who asked may withdraw the request.
	ErrNotSubmitter = fmt.Errorf("only the submitter may recall this request")
	// ErrDelegateToSelf: handing a step to yourself is a no-op that would still write history.
	ErrDelegateToSelf = fmt.Errorf("cannot delegate to yourself")
	// ErrDelegateTargetSeated: on a step with several seats, the target already holds a seat of
	// this visit or already voted in it. One person casts one vote per visit (FR-022), so the
	// delegated seat could never be used and an `all` step would wait on it forever.
	ErrDelegateTargetSeated = fmt.Errorf("delegate target already sits on this step")
)

// Requester serves the human verbs of an approval: ask for one, take it back, hand it on.
// It reuses the SAME store the automatic engine and the Decider use, so a manually-armed instance
// is indistinguishable downstream — the pending queue, the SLA scheduler and the decision path all
// treat it identically. Only the way it came into existence differs.
type Requester struct {
	loader    ProcessLoader
	store     InstanceStore
	procs     ProcessGetter
	inbox     InboxWriter
	deadlines DeadlineCalculator
	router    Router    // optional: resolves field / submitter assignments (S6a)
	runner    Continuer // optional: walks the machine nodes a submitted instance starts on (specs/043 S7)
	now       func() time.Time
	logger    *zap.Logger
}

// WithRunner arms the walk through the machine nodes a submitted instance starts on. Chainable.
func (r *Requester) WithRunner(c Continuer) *Requester {
	r.runner = c
	return r
}

// NewRequester wires the request engine. inbox may be nil (notices disabled).
func NewRequester(loader ProcessLoader, store InstanceStore, procs ProcessGetter, inbox InboxWriter, logger *zap.Logger) *Requester {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Requester{loader: loader, store: store, procs: procs, inbox: inbox, now: time.Now, logger: logger}
}

// SubmitOptions are the optional knobs of a send (specs/043 S9b).
type SubmitOptions struct {
	// ProcessID picks the process; "" = the first manual process whose criteria the record meets.
	ProcessID string
	// IdempotencyKey makes a retried call return the hồ sơ the first call made instead of a second
	// one (or APPROVAL_ALREADY_PENDING). "" = no such protection.
	IdempotencyKey string
}

// StartableProcess is one manual process a record may be sent through right now.
type StartableProcess struct {
	ProcessID string `json:"processId"`
	Name      string `json:"name"`
}

// Startable lists the active manual processes of entityName whose entry criteria data meets — what
// the sender may choose between when pressing "Gửi duyệt".
func (r *Requester) Startable(ctx context.Context, tenantID, entityName string, data map[string]any) ([]StartableProcess, error) {
	if r.loader == nil {
		return []StartableProcess{}, nil
	}
	procs, err := r.loader.ListActiveProcesses(ctx, tenantID, entityName, TriggerManual)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Startable: load processes: %w", err)
	}
	out := make([]StartableProcess, 0, len(procs))
	for _, p := range procs {
		if len(p.Chain()) == 0 || pinDefinition(p).StartIndex() < 0 || !matchesCriteria(p, data, nil) {
			continue
		}
		out = append(out, StartableProcess{ProcessID: p.ID, Name: p.Name})
	}
	return out, nil
}

// requestHash binds an Idempotency-Key to WHAT was asked and by WHOM: another record, another
// chosen process or another sender under the same key is a different request.
func requestHash(entityName, recordID, processID, actorID string) string {
	sum := sha256.Sum256([]byte(entityName + "\x1f" + recordID + "\x1f" + processID + "\x1f" + actorID))
	return hex.EncodeToString(sum[:])
}

// Submit arms a manual approval with the defaults (first matching process, no idempotency key).
func (r *Requester) Submit(ctx context.Context, tenantID, entityName, recordID string, data map[string]any, actor Actor) (*Instance, error) {
	return r.SubmitWith(ctx, tenantID, entityName, recordID, data, actor, SubmitOptions{})
}

// SubmitWith arms a manual approval on (entity, record) and returns the created instance.
//
// data is the record's current field values, used to evaluate each candidate process's entry
// criteria — the SAME evaluator the automatic path uses, so "over 50 million needs the director"
// means the same thing however the approval was started.
//
// Errors are the point of this method: ErrNoApprovalProcess when nothing matches, ErrAlreadyPending
// when one is already open, ErrProcessNotApplicable when the named process cannot take the record.
// The caller is a person waiting to find out whether they sent it.
func (r *Requester) SubmitWith(ctx context.Context, tenantID, entityName, recordID string, data map[string]any, actor Actor, opt SubmitOptions) (*Instance, error) {
	if r.loader == nil || r.store == nil {
		return nil, ErrNoApprovalProcess
	}
	var hash string
	if opt.IdempotencyKey != "" {
		// BEFORE every other check: a replay is answered from what the first call did, even though
		// that hồ sơ is by now pending (the very condition that would otherwise say "already sent").
		hash = requestHash(entityName, recordID, opt.ProcessID, actor.UserID)
		if prev, found, err := r.replayed(ctx, tenantID, opt.IdempotencyKey, hash); err != nil || found {
			return prev, err
		}
	}
	procs, err := r.loader.ListActiveProcesses(ctx, tenantID, entityName, TriggerManual)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Submit: load processes: %w", err)
	}

	// Pending-check BEFORE matching: "already sent" is the more useful answer than "no process
	// matches", and it is true regardless of which process armed the open instance.
	pending, err := r.store.HasOpenExclusive(ctx, tenantID, entityName, recordID)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Submit: pending check: %w", err)
	}
	if pending {
		// A retry that lost a race: the first call's hồ sơ is what is pending, and it is the answer.
		if prev, found, err := r.replayed(ctx, tenantID, opt.IdempotencyKey, hash); err != nil || found {
			return prev, err
		}
		return nil, ErrAlreadyPending
	}

	for _, p := range procs {
		if opt.ProcessID != "" && p.ID != opt.ProcessID {
			continue
		}
		if p.Options.IsIndependent() { // a tracking lane has no "send": it follows record events (S12b)
			continue
		}
		if len(p.Chain()) == 0 || !matchesCriteria(p, data, nil) { // a send has no before-image
			continue
		}
		now := r.now().UTC()
		def := pinDefinition(p) // same pin as the engine path
		chain := def.Chain()
		start := def.StartIndex() // the graph's start node, else the first step (S7)
		if start < 0 {
			continue // starts on a node it does not have: not a process this binary can run
		}
		first := chain[start]
		var (
			due   *time.Time
			basis SLABasis
			seats []Seat // a machine start has none: the walk opens the first person's step
			stuck string // why nobody holds the first step ("" = someone does), S6a FR-018
		)
		if !first.IsMachine() {
			// A person is waiting: a calendar the store cannot read fails the request BEFORE anything
			// is created (D-8), rather than arming an instance on a deadline nobody can trust.
			var derr error
			due, basis, derr = stepDue(ctx, r.deadlines, tenantID, def.CalendarID(), chain, start, true, now)
			if derr != nil {
				return nil, fmt.Errorf("bpm.Requester.Submit: first-step deadline: %w", derr)
			}
			def.FirstDueBasis = basis
			// A store that cannot say who holds the step fails the request (as the calendar above):
			// the person asking hears about it rather than getting an approval nobody can act on.
			// "Nobody found" is different — the approval is created stuck and the Nexus team sees it
			// (FR-018).
			var rerr error
			seats, stuck, rerr = routeSeats(ctx, r.router,
				RouteInput{TenantID: tenantID, ProcessID: p.ID, EntityName: entityName, RecordID: recordID, Submitter: actor.UserID},
				def.Options, first, due, basis)
			if rerr != nil {
				return nil, fmt.Errorf("bpm.Requester.Submit: route first step: %w", rerr)
			}
		}
		approverType, approverID := projection(seats) // D-5, as on the engine path
		instID, err := r.store.CreateInstance(ctx, CreateInstanceInput{
			TenantID:            tenantID,
			ProcessID:           p.ID,
			EntityName:          entityName,
			RecordID:            recordID,
			CurrentApproverType: approverType,
			CurrentApproverID:   approverID,
			SLADueAt:            due,
			Definition:          def,
			Seats:               seats,
			LastError:           stuck,
			CreatedBy:           actor.UserID,
			StartStep:           start,
			IdempotencyKey:      opt.IdempotencyKey,
			RequestHash:         hash,
		})
		if err != nil {
			if errors.Is(err, ErrIdempotencyKeyConflict) || errors.Is(err, ErrAlreadyPending) {
				// Two calls with one key raced and the other won — which unique key (uq_ai_idem or
				// uq_ai_open) trips first is the database's choice, so both mean "look the key up".
				if prev, found, rerr := r.replayed(ctx, tenantID, opt.IdempotencyKey, hash); rerr != nil || found {
					return prev, rerr
				}
			}
			return nil, fmt.Errorf("bpm.Requester.Submit: create instance: %w", err)
		}

		// Open the timeline with "who sent it". The automatic path has no equivalent entry
		// because nobody sent anything — that asymmetry is the honest one.
		inst, err := r.store.GetInstance(ctx, tenantID, instID)
		if err != nil {
			return nil, fmt.Errorf("bpm.Requester.Submit: reload instance: %w", err)
		}
		node, stepName := inst.Definition.StepRef(start)
		r.appendHistory(ctx, inst, HistoryEntry{Step: start, Action: HistorySubmitted, Actor: actor.UserID, Node: node, StepName: stepName, Version: inst.Definition.version(), At: now})
		notifyHolders(ctx, r.store, r.inbox, r.logger, tenantID, entityName, recordID, first, "vừa được gửi và đang chờ bạn phê duyệt", seats)
		r.logger.Info("bpm: approval submitted",
			zap.String("tenant_id", tenantID), zap.String("instance_id", instID),
			zap.String("record_id", recordID), zap.String("by", actor.UserID))
		if first.IsMachine() && r.runner != nil {
			// The person pressed Submit and is waiting: walk now, so the answer already reflects a
			// record auto-approved by its rules. A failed walk is not a failed submit — the instance
			// exists and the continue job will pick it up.
			if werr := r.runner.Continue(ctx, tenantID, instID); werr != nil {
				r.logger.Warn("bpm: walk after submit failed — the continue job will pick it up",
					zap.String("tenant_id", tenantID), zap.String("instance_id", instID), zap.Error(werr))
			}
			if fresh, gerr := r.store.GetInstance(ctx, tenantID, instID); gerr == nil {
				return fresh, nil
			}
		}
		return inst, nil
	}
	if opt.ProcessID != "" {
		return nil, ErrProcessNotApplicable
	}
	return nil, ErrNoApprovalProcess
}

// replayed answers a send whose Idempotency-Key already created a hồ sơ: found=true with that hồ sơ
// when the earlier request was this same one, ErrIdempotencyKeyReused when it was another. No key,
// or no hồ sơ under it yet, is found=false.
func (r *Requester) replayed(ctx context.Context, tenantID, key, hash string) (*Instance, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	prev, prevHash, err := r.store.FindByIdempotencyKey(ctx, tenantID, key)
	switch {
	case errors.Is(err, ErrInstanceNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("bpm.Requester.Submit: idempotency lookup: %w", err)
	case prevHash != hash:
		return nil, true, ErrIdempotencyKeyReused
	}
	return prev, true, nil
}

// Recall withdraws a pending request. Only its submitter may — not the approver (who has reject),
// and not an arbitrary editor. An admin may, because an admin can already do everything and
// pretending otherwise here would only produce a workaround.
//
// The instance becomes 'cancelled', which is terminal: recalling then re-submitting produces a
// NEW instance with its own timeline, rather than resurrecting the old one with a hole in it.
func (r *Requester) Recall(ctx context.Context, tenantID, id, comment string, actor Actor) (*Instance, error) {
	inst, err := r.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	if inst.Definition.Lane() == LaneIndependent {
		return nil, ErrTrackingLane // not even an admin: it is the record's clock, not a request (S12b)
	}
	if !actor.IsAdmin && inst.CreatedBy != actor.UserID {
		return nil, ErrNotSubmitter
	}

	now := r.now().UTC()
	node, stepName := inst.Definition.StepRef(inst.CurrentStep)
	history := append(inst.History, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryRecalled, Actor: actor.UserID, Comment: comment,
		Node: node, StepName: stepName, Version: inst.Definition.version(), At: now,
	})
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Recall: marshal history: %w", err)
	}
	// Same guard the Decider uses (the seq read, under the row lock): a recall racing an approval
	// must lose cleanly rather than cancelling something already decided. The submitter test above
	// is on created_by, which never changes, so it needs no seat.
	if err := r.store.DecideTaskTx(ctx, tenantID, id, TaskGuard{ExpectSeq: inst.Seq}, InstanceTransition{
		CurrentStep:         inst.CurrentStep,
		Status:              StatusCancelled,
		CurrentApproverType: inst.CurrentApproverType,
		CurrentApproverID:   inst.CurrentApproverID,
		Escalated:           inst.Escalated,
		// Every open seat is withdrawn — cancelled, not decided by anyone (FR-029).
		Seats: SeatWrite{Close: SeatRecalled, By: actor.UserID},
	}, historyJSON); err != nil {
		return nil, txError("bpm.Requester.Recall", err)
	}
	inst.History, inst.Status, inst.UpdatedAt = history, StatusCancelled, now
	return inst, nil
}

// Delegate hands the CURRENT step to another user. The step does not advance and no decision is
// recorded: the same approval is still owed, by someone else. Authorization is identical to
// deciding — if you may approve it, you may pass it on; if you may not, you may not.
//
// The target becomes a USER-targeted approver even when the step was role-targeted. That is
// deliberate: delegation names a person, and leaving it role-targeted would mean handing it to
// everyone with that role, which is what it already was.
func (r *Requester) Delegate(ctx context.Context, tenantID, id, toUserID, comment string, actor Actor) (*Instance, error) {
	toUserID = strings.TrimSpace(toUserID)
	if toUserID == "" {
		return nil, ErrNotAuthorized
	}
	inst, err := r.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	seats, err := r.store.ListOpenSeats(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Delegate: read seats: %w", err)
	}
	if err := authorize(inst, seats, actor); err != nil {
		return nil, err
	}
	if reworkSeatOf(inst, seats) { // "fix and resend" belongs to the submitter; it is not an approval to hand on
		return nil, ErrWrongStepKind
	}
	if inst.OnMachineNode() {
		return nil, ErrWrongStepKind // nobody holds a gateway / auto / end node: there is nothing to hand on (S7)
	}
	if toUserID == actor.UserID {
		return nil, ErrDelegateToSelf
	}
	// On a step with several seats a delegation hands on the delegator's OWN seat — never the
	// others' — and leaves the (empty) single-holder projection alone (S4). "Several seats" is a
	// property of the STEP (as in DecideAt), not of how many are still open: after all but one
	// voted, the last seat is still a council's. An admin who holds none cannot say whose seat to
	// move: that is the override's job (D-13).
	var ownSeat string
	if chain := inst.Definition.Chain(); inst.CurrentStep >= 0 && inst.CurrentStep < len(chain) && chain[inst.CurrentStep].Parallel() {
		visit, verr := r.store.ListVisitSeats(ctx, tenantID, id, seatNodeID(inst))
		if verr != nil {
			return nil, fmt.Errorf("bpm.Requester.Delegate: read visit: %w", verr)
		}
		mine := ActorSeat(visit, actor)
		if mine == nil {
			if actor.IsAdmin {
				return nil, ErrAdminOverrideRequired
			}
			return nil, ErrNotAuthorized
		}
		// The target must be someone who can still cast a vote here: one vote per visit.
		if delegateTargetSeated(visit, toUserID) {
			return nil, ErrDelegateTargetSeated
		}
		ownSeat = mine.ID
	}

	now := r.now().UTC()
	node, stepName := inst.Definition.StepRef(inst.CurrentStep)
	history := append(inst.History, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryDelegated, Actor: actor.UserID,
		Comment: strings.TrimSpace(comment), Target: toUserID, Node: node, StepName: stepName, Version: inst.Definition.version(), At: now,
	})
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("bpm.Requester.Delegate: marshal history: %w", err)
	}
	// The seat moves: the delegator's closes as handed on, the target's opens on the SAME visit
	// with the same deadline and basis — and, on an escalated step, already breached.
	nodeID := seatNodeID(inst)
	seat := Seat{
		NodeID: nodeID, AssigneeType: ApproverUser, AssigneeID: toUserID,
		DueAt: inst.SLADueAt, DueBasis: inst.CurrentSLABasis(), DelegatedFrom: actor.UserID,
	}
	for _, s := range seats { // a handed-on task stays a task
		if s.NodeID == nodeID && s.Kind != "" {
			seat.Kind = s.Kind
			break
		}
	}
	if inst.Escalated {
		seat.BreachNotifiedAt = escalatedAt(inst, now)
	}
	// SLA deliberately UNCHANGED: passing work along does not buy more time, otherwise a chain of
	// delegations is an unbounded way to defeat the deadline.
	projType, projID := ApproverUser, toUserID
	if ownSeat != "" {
		projType, projID = inst.CurrentApproverType, inst.CurrentApproverID
	}
	if err := r.store.DecideTaskTx(ctx, tenantID, id, TaskGuard{ExpectSeq: inst.Seq, Actor: &actor}, InstanceTransition{
		CurrentStep:         inst.CurrentStep,
		Status:              StatusPending,
		CurrentApproverType: projType,
		CurrentApproverID:   projID,
		SLADueAt:            inst.SLADueAt,
		Escalated:           inst.Escalated,
		Seats: SeatWrite{
			Close: SeatReassigned, CloseSeat: ownSeat, Node: nodeID, By: actor.UserID, Comment: strings.TrimSpace(comment),
			Open: []Seat{seat}, SameVisit: true,
		},
	}, historyJSON); err != nil {
		return nil, txError("bpm.Requester.Delegate", err)
	}
	r.notify(ctx, tenantID, inst.EntityName, inst.RecordID, ApproverUser, toUserID,
		"Được uỷ quyền phê duyệt", fmt.Sprintf("Một bản ghi %s được chuyển cho bạn phê duyệt.", inst.EntityName))
	inst.History = history
	inst.CurrentApproverType, inst.CurrentApproverID, inst.UpdatedAt = projType, projID, now
	return inst, nil
}

// delegateTargetSeated says whether user already has a say in this visit: an open seat of their
// own name, or a vote cast. (A seat they hold only through a ROLE is not seen here — their roles
// are not known to the engine; the vote guard still keeps them to one vote.)
func delegateTargetSeated(visit []VisitSeat, userID string) bool {
	if DecidedIn(visit, userID) {
		return true
	}
	for _, v := range visit {
		if v.Status == "open" && v.AssigneeType == ApproverUser && v.AssigneeID == userID {
			return true
		}
	}
	return false
}

// appendHistory writes the history array without changing any state field (best-effort: the
// instance exists and is usable even if this write fails, so a failure is logged, not returned).
func (r *Requester) appendHistory(ctx context.Context, inst *Instance, e HistoryEntry) {
	history := append(inst.History, e)
	historyJSON, err := json.Marshal(history)
	if err != nil {
		r.logger.Warn("bpm: marshal submit history failed", zap.Error(err))
		return
	}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, InstanceTransition{
		CurrentStep:         inst.CurrentStep,
		Status:              inst.Status,
		CurrentApproverType: inst.CurrentApproverType,
		CurrentApproverID:   inst.CurrentApproverID,
		SLADueAt:            inst.SLADueAt,
		Escalated:           inst.Escalated,
		LastError:           inst.LastError, // a history append does not un-stick a step
		// seq moves with this write, so the bpm_continue job parked at arm (for the old seq) would
		// be stale: park a fresh one when the instance stands on a machine node.
		Continue: inst.OnMachineNode(),
	}, historyJSON); err != nil {
		r.logger.Warn("bpm: write submit history failed", zap.Error(err))
		return
	}
	inst.History = history
}

// notify pings a user-targeted approver (best-effort, mirroring the async engine's notifyApprover).
func (r *Requester) notify(ctx context.Context, tenantID, entity, recordID string, at ApproverType, approverID, title, msg string) {
	if r.inbox == nil || at != ApproverUser || approverID == "" {
		return
	}
	if _, err := r.inbox.InsertNotification(ctx, Notification{
		TenantID: tenantID, UserID: approverID, Title: title, Type: "approval",
		Message: msg, EntityName: entity, RecordID: recordID,
	}); err != nil {
		r.logger.Warn("bpm: approval notice failed (fail-open)",
			zap.String("tenant_id", tenantID), zap.String("record_id", recordID), zap.Error(err))
	}
}
