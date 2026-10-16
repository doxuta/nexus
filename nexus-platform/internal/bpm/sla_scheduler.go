package bpm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// BreachStore is the SLA scheduler's view of the approval tables: the tenants to sweep, one
// tenant-pinned scan of its breached instances, and the atomic claim that marks a step escalated
// exactly once. Satisfied by *SQLRepository. Nothing here reads approval rows across tenants.
type BreachStore interface {
	ListBreachTenants(ctx context.Context) ([]string, error)
	ListBreachedForTenant(ctx context.Context, tenantID string, now time.Time) ([]Instance, error)
	// expectSeq is the seq the scan read: a step decided, delegated or escalated since loses.
	ClaimBreach(ctx context.Context, tenantID, id string, expectSeq int64, historyJSON []byte) (bool, error)
	// ListOpenSeats + ResolveSeatUsers name who a breach pings: the open seats of the step, each
	// resolved to its people (a role seat to the active users carrying the role), capped.
	ListOpenSeats(ctx context.Context, tenantID, instanceID string) ([]Seat, error)
	ResolveSeatUsers(ctx context.Context, tenantID string, seats []Seat, limit int) ([]string, error)
}

// SLAScheduler is the EPIC 5 M1 SLA breach engine (spec §EPIC 5 "SLA Breach detection + violation
// actions"; benchmark ServiceNow SLA engine). A single goroutine ticks every Interval; each tick
// scans, tenant by tenant, the PENDING instances whose current step is past its SLA deadline and not yet escalated,
// CLAIMS each one (atomic CAS so two ticks / instances never double-escalate the same step), and
// runs the breach action: append an 'escalated' history entry + notify the current approver.
//
// Fail-open + isolation guarantees (same contract as the workflow scheduler):
//   - A scan/claim/notify error on one instance is logged and the scheduler moves to the next — it
//     NEVER stops the tick or the goroutine.
//   - Every tick recovers from panic so one bad instance cannot kill the scheduler.
//   - Start is idempotent; Stop is graceful (waits for the in-flight tick) and idempotent.
type SLAScheduler struct {
	store    BreachStore
	inbox    InboxWriter
	auditor  Recorder
	router   Router // optional: resolves the field / submitter a wait node's sla.notify names (S12a)
	interval time.Duration
	now      func() time.Time
	logger   *zap.Logger
	// frozen (specs/068 slice B), when set, reports a write-frozen tenant: its breaches are not
	// scanned or claimed this sweep; the next sweep after the freeze lapses escalates them.
	frozen func(ctx context.Context, tenantID string) bool

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// SLASchedulerConfig configures the SLA scheduler. Interval is the breach-scan cadence; it is
// clamped to a sane minimum so a misconfigured zero never produces a busy-loop.
type SLASchedulerConfig struct {
	Interval time.Duration
}

// NewSLAScheduler builds (does NOT start) an SLA scheduler. store may be nil — a nil store makes
// every tick a no-op (engine disabled). inbox/auditor may be nil. logger may be nil.
func NewSLAScheduler(cfg SLASchedulerConfig, store BreachStore, inbox InboxWriter, auditor Recorder, logger *zap.Logger) *SLAScheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	interval := cfg.Interval
	if interval < time.Second {
		interval = 60 * time.Second
	}
	return &SLAScheduler{
		store:    store,
		inbox:    inbox,
		auditor:  auditor,
		interval: interval,
		now:      time.Now,
		logger:   logger,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the ticker goroutine. Idempotent. The goroutine runs until Stop is called. The
// first tick fires after one Interval (boot does not race the rest of wiring).
func (s *SLAScheduler) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// run is the ticker loop. Each tick is wrapped in tickOnce (recovers panics).
func (s *SLAScheduler) run() {
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

// tickOnce runs one breach scan, recovering from any panic so the goroutine survives.
func (s *SLAScheduler) tickOnce() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("bpm SLA scheduler: recovered panic in tick", zap.Any("panic", r))
		}
	}()
	if s.store == nil {
		return // engine disabled
	}
	s.runBreaches(context.Background(), s.now())
}

// WithTenantGate arms the per-tenant write-freeze skip. Chainable; nil keeps it off.
func (s *SLAScheduler) WithTenantGate(frozen func(ctx context.Context, tenantID string) bool) *SLAScheduler {
	s.frozen = frozen
	return s
}

// runBreaches performs one SLA pass at `now`: for each active tenant, scan ITS breached instances
// (one pinned query), and for each claim it (atomic CAS) then fire the breach action. A tenant
// whose scan fails is logged and skipped — the others still run. Exported-internal so a test can
// drive a deterministic single pass without the ticker.
func (s *SLAScheduler) runBreaches(ctx context.Context, now time.Time) {
	tenants, err := s.store.ListBreachTenants(ctx)
	if err != nil {
		s.logger.Warn("bpm SLA scheduler: failed to list tenants", zap.Error(err))
		return
	}
	for _, tenantID := range tenants {
		if s.frozen != nil && s.frozen(ctx, tenantID) {
			continue
		}
		breached, err := s.store.ListBreachedForTenant(ctx, tenantID, now)
		if err != nil {
			s.logger.Warn("bpm SLA scheduler: failed to list breached instances",
				zap.String("tenant_id", tenantID), zap.Error(err))
			continue
		}
		for i := range breached {
			_ = s.fireBreach(ctx, breached[i], now) // logged inside; the next sweep retries
		}
	}
}

// fireBreach claims one breached instance (CAS) and, if won, notifies the current approver of the
// SLA breach. The claim's appended 'escalated' history entry + the escalated=1 flag guarantee the
// breach action fires AT MOST ONCE per step even with concurrent ticks / multiple instances.
//
// It returns an error only for a failure that left the step UNclaimed (marshal, claim) — the job
// handler retries on it; the sweep ignores it (the next tick is the retry).
func (s *SLAScheduler) fireBreach(ctx context.Context, inst Instance, now time.Time) error {
	node, stepName := inst.Definition.StepRef(inst.CurrentStep) // "" "" for a pre-000065 instance
	waitNode := inst.Definition.NodeAt(inst.CurrentStep)
	if waitNode != nil && waitNode.Kind != NodeWait {
		waitNode = nil
	}
	comment := "SLA breach: quá hạn phê duyệt"
	if waitNode != nil {
		comment = "SLA breach: quá hạn xử lý" // nobody holds a wait node: nothing was left unapproved
	}
	entry := HistoryEntry{
		Step:     inst.CurrentStep,
		Action:   HistoryEscalated,
		Actor:    "", // system escalation
		Comment:  comment,
		Node:     node,
		StepName: stepName,
		Version:  inst.Definition.version(),
		At:       now.UTC(),
	}
	newHistory := append(inst.History, entry)
	historyJSON, err := json.Marshal(newHistory)
	if err != nil {
		s.logger.Warn("bpm SLA scheduler: marshal history failed (fail-open)",
			zap.String("instance_id", inst.ID), zap.Error(err))
		return fmt.Errorf("bpm.SLAScheduler.fireBreach: marshal history: %w", err)
	}
	won, err := s.store.ClaimBreach(ctx, inst.TenantID, inst.ID, inst.Seq, historyJSON)
	if err != nil {
		s.logger.Warn("bpm SLA scheduler: claim breach failed (fail-open)",
			zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
		return fmt.Errorf("bpm.SLAScheduler.fireBreach: claim: %w", err)
	}
	if !won {
		return nil // another tick / the due job already escalated this step — not a double-fire.
	}

	// Audit the escalation (best-effort).
	if s.auditor != nil {
		s.auditor.RecordApprovalDecision(ctx, ApprovalAuditEvent{
			TenantID:   inst.TenantID,
			EntityName: inst.EntityName,
			RecordID:   inst.RecordID,
			InstanceID: inst.ID,
			ProcessID:  inst.ProcessID,
			Definition: inst.Definition, // nil for a pre-000065 instance — the SLA path reads no live process
			Step:       inst.CurrentStep,
			Action:     HistoryEscalated,
			Comment:    "SLA breach",
			At:         now,
		})
	}

	// ponytail: claim-then-notify. Once the claim is won the step is escalated=1, so neither the sweep
	// nor a re-delivered job can retry a lost notification (recipient lookup / insert failure here is
	// logged, not recovered). Window is narrow; upgrade path = record an unnotified flag on the step.
	// Notify everyone who holds the step: each open seat's user, or the users of a role seat,
	// deduplicated and capped. A step no seat mirrors (armed by a pre-S3a binary) falls back to
	// the instance's own projection. A failure here never un-claims the breach (fail-open).
	title, message := "Quá hạn phê duyệt (SLA)", fmt.Sprintf("Bản ghi %s đã quá hạn phê duyệt — vui lòng xử lý ngay.", inst.EntityName)
	var recipients []string
	if waitNode != nil {
		title, message = "Quá hạn", fmt.Sprintf("Bản ghi %s đã quá hạn xử lý — vui lòng xử lý ngay.", inst.EntityName)
		recipients = s.waitRecipients(ctx, inst, waitNode)
		if len(recipients) == 0 {
			s.recordNoRecipient(ctx, inst, node, stepName, now)
		}
	} else {
		recipients = s.breachRecipients(ctx, inst)
	}
	if s.inbox != nil {
		for _, uid := range recipients {
			if _, err := s.inbox.InsertNotification(ctx, Notification{
				TenantID:   inst.TenantID,
				UserID:     uid,
				Title:      title,
				Type:       "due",
				Message:    message,
				EntityName: inst.EntityName,
				RecordID:   inst.RecordID,
			}); err != nil {
				s.logger.Warn("bpm SLA scheduler: breach notice failed (fail-open)",
					zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
			}
		}
	}
	s.logger.Info("bpm SLA scheduler: approval SLA breached",
		zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID),
		zap.Int("step", inst.CurrentStep))
	return nil
}

// breachRecipients lists the user ids a breach of inst pings (see fireBreach). Errors are logged
// and yield whoever could be resolved — the claim already won, so a notice failure must not undo it.
func (s *SLAScheduler) breachRecipients(ctx context.Context, inst Instance) []string {
	seats, err := s.store.ListOpenSeats(ctx, inst.TenantID, inst.ID)
	if err != nil {
		s.logger.Warn("bpm SLA scheduler: read seats failed (fail-open)",
			zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
		seats = nil
	}
	if len(seats) == 0 && inst.CurrentApproverID != "" {
		seats = []Seat{{AssigneeType: inst.CurrentApproverType, AssigneeID: inst.CurrentApproverID}}
	}
	users, err := s.store.ResolveSeatUsers(ctx, inst.TenantID, seats, breachNotifyCap)
	if err != nil {
		s.logger.Warn("bpm SLA scheduler: resolve seat holders failed (fail-open)",
			zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
	}
	return users
}

// Stop signals the ticker goroutine to exit and waits (up to ctx deadline) for the in-flight tick.
// Idempotent. Returns ctx.Err() if the deadline passes before the goroutine exits.
func (s *SLAScheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunBreachesForTest drives one deterministic SLA pass at `now` without the ticker (test seam for
// the cross-package integration tests which cannot reach the unexported runBreaches).
func (s *SLAScheduler) RunBreachesForTest(ctx context.Context, now time.Time) {
	if s.store == nil {
		return
	}
	s.runBreaches(ctx, now)
}
