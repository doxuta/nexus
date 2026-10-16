// doctor_scheduler.go — DOCTOR-OPS scheduled scan (../specs/008-nexus-doctor/spec.md §4 lát 3):
// the janitor-pattern background ticker that turns the Doctor from "bấm tay khi nghi" into
// "tự khám định kỳ, người chỉ duyệt". Every DOCTOR_SCAN_INTERVAL_SEC (default 0 = OFF —
// opt-in) it runs a READ-ONLY Scan for every active tenant and, when findings exist, writes
// one in-app notification to each of the tenant's admins pointing at the Bảo trì → Khám hệ thống screen.
//
// HARD RULE (spec §3): the scheduler NEVER fixes anything. By construction, not by
// discipline — its scanner dependency exposes ONLY Scan (doctorScanner below has no Fix
// method), so no future edit of the sweep loop can reach a mutation path.
//
// Tenant iteration is the same deliberate tenant-agnostic exception the janitor and the
// workflow scheduler make: enumerating tenant IDs is a global operational concern, while
// every scan/notification inside the loop is pinned to its own tenant.
package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// doctorScanner is the scheduler's consumer-side view of the DoctorService: READ-ONLY scan,
// nothing else (see the package comment — no-fix is enforced by this interface's shape).
type doctorScanner interface {
	Scan(ctx context.Context, tenantID string) ([]doctor.CheckResult, error)
}

// doctorScheduleStore lists the sweep universe (active tenants) and each tenant's admin
// recipients. Satisfied by *doctor.SQLStore.
type doctorScheduleStore interface {
	ListActiveTenants(ctx context.Context, limit int) ([]string, error)
	ListTenantAdmins(ctx context.Context, tenantID string, limit int) ([]string, error)
}

// doctorNotificationSink writes the in-app notification rows. Satisfied by
// *workflow.SQLRepository.InsertNotification — the SAME sys_notifications write path the
// workflow in_app_notification action uses (one insert body, never a mirror).
type doctorNotificationSink interface {
	InsertNotification(ctx context.Context, n workflow.Notification) (string, error)
}

// doctorSchedulerWindow bounds the tenant and admin list queries per sweep — same rationale
// as the other scan windows (an installation beyond it gets an honest partial sweep; the
// next tick resumes nothing because scans are stateless, but 2000 tenants/admins is far past
// any real deployment today).
const doctorSchedulerWindow = 2000

// DoctorSchedulerConfig configures a DoctorScheduler. Interval 0 is the caller's business —
// a disabled scheduler should not be constructed/started (main.go guards with Enabled()).
type DoctorSchedulerConfig struct {
	Interval time.Duration
}

// DoctorScheduler is the background scan ticker. It shares the janitor's lifecycle contract:
// idempotent Start/Stop, panic-recovering ticks, injectable clock-free design (the sweep
// takes ctx only), first tick after one full interval so boot never races wiring.
type DoctorScheduler struct {
	interval time.Duration
	scanner  doctorScanner
	store    doctorScheduleStore
	notifier doctorNotificationSink
	logger   *zap.Logger
	// frozen (specs/068 slice B), when set, reports a write-frozen tenant: it is not scanned this
	// sweep (mid-restore data would read as drift).
	frozen func(ctx context.Context, tenantID string) bool

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// NewDoctorScheduler builds (does NOT start) a scheduler. A nil logger is replaced with a
// no-op.
func NewDoctorScheduler(cfg DoctorSchedulerConfig, scanner doctorScanner, store doctorScheduleStore, notifier doctorNotificationSink, logger *zap.Logger) *DoctorScheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DoctorScheduler{
		interval: cfg.Interval,
		scanner:  scanner,
		store:    store,
		notifier: notifier,
		logger:   logger,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the ticker goroutine (idempotent).
func (s *DoctorScheduler) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// run is the ticker loop — one sweep per interval until Stop.
func (s *DoctorScheduler) run() {
	defer close(s.done)
	interval := s.interval
	if interval < time.Second {
		interval = time.Hour // defensive: a sub-second interval would be a busy loop
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sweepOnce(context.Background())
		}
	}
}

// sweepOnce runs one full sweep, recovering from any panic so the goroutine survives.
func (s *DoctorScheduler) sweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("doctor scheduler: recovered panic during sweep", zap.Any("panic", r))
		}
	}()
	s.Sweep(ctx)
}

// WithTenantGate arms the per-tenant write-freeze skip. Chainable; nil keeps it off.
func (s *DoctorScheduler) WithTenantGate(frozen func(ctx context.Context, tenantID string) bool) *DoctorScheduler {
	s.frozen = frozen
	return s
}

// Sweep performs one scan pass over every active tenant. Exported so a test (and a future
// ops "run now" hook) can drive a single pass without the ticker. Per-tenant errors are
// logged and the sweep CONTINUES (one broken tenant must not blind the doctor to the rest).
// NO fix is ever issued — Scan is read-only absolute (see package comment).
func (s *DoctorScheduler) Sweep(ctx context.Context) {
	tenants, err := s.store.ListActiveTenants(ctx, doctorSchedulerWindow)
	if err != nil {
		s.logger.Warn("doctor scheduler: list tenants failed", zap.Error(err))
		return
	}
	for _, tenantID := range tenants {
		if s.frozen != nil && s.frozen(ctx, tenantID) {
			continue
		}
		findings, err := s.scanTenant(ctx, tenantID)
		if err != nil {
			s.logger.Warn("doctor scheduler: tenant scan failed",
				zap.String("tenant_id", tenantID), zap.Error(err))
			continue
		}
		if findings == 0 {
			continue
		}
		s.notifyAdmins(ctx, tenantID, findings)
	}
}

// scanTenant runs one read-only scan and returns the total finding count across all checks.
func (s *DoctorScheduler) scanTenant(ctx context.Context, tenantID string) (int, error) {
	results, err := s.scanner.Scan(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("services.DoctorScheduler.scanTenant: %w", err)
	}
	total := 0
	for _, r := range results {
		total += r.Count
	}
	return total, nil
}

// notifyAdmins writes one in-app notification per admin of the tenant. An insert error is
// logged and the fan-out continues (fail-open, like every notification sink in the engine).
func (s *DoctorScheduler) notifyAdmins(ctx context.Context, tenantID string, findings int) {
	admins, err := s.store.ListTenantAdmins(ctx, tenantID, doctorSchedulerWindow)
	if err != nil {
		s.logger.Warn("doctor scheduler: list admins failed",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	title := fmt.Sprintf("Bác sĩ hệ thống: %d khoản lệch", findings)
	message := "Quét định kỳ phát hiện lệch giữa metadata và dữ liệu phái sinh — mở Xưởng Nexus → Bảo trì → Khám hệ thống để xem báo cáo và duyệt sửa. Quét định kỳ KHÔNG tự sửa gì."
	notified := 0
	for _, adminID := range admins {
		if _, err := s.notifier.InsertNotification(ctx, workflow.Notification{
			TenantID: tenantID,
			UserID:   adminID,
			Title:    title,
			Message:  message,
		}); err != nil {
			s.logger.Warn("doctor scheduler: notification insert failed",
				zap.String("tenant_id", tenantID), zap.String("user_id", adminID), zap.Error(err))
			continue
		}
		notified++
	}
	s.logger.Info("doctor scheduler: findings reported",
		zap.String("tenant_id", tenantID),
		zap.Int("findings", findings),
		zap.Int("admins_notified", notified))
}

// Stop signals the ticker goroutine to exit and waits (up to the context deadline) for the
// in-flight sweep to finish. Idempotent; only call after Start (main.go guards on Enabled()).
func (s *DoctorScheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
