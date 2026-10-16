package tenantbackup

// scheduler.go — the nightly / after-deploy backup of every customer and the retention that keeps
// the disk bounded (specs/068 slice E, FR-054..FR-058). The pure rules (which slot is due, which
// backups expire) live here as functions of their inputs so they are table-tested; Service.sweepTenant
// is the only place they meet the database; Scheduler is the DoctorScheduler-shaped ticker.
//
// Duplicate control is the database's job, not an in-process lock: two replicas ticking at once both
// try to INSERT the same (tenant_id, schedule_slot) and uq_tbk_slot lets exactly one win (FR-056).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
)

// ictZone is Vietnam time. Vietnam has no DST, so a fixed zone is exact and needs no tzdata in the image.
var ictZone = time.FixedZone("ICT", 7*3600)

const (
	defaultKeepScheduled = 7
	maxKeepScheduled     = 30
)

// Schedule is one row of sys_tenant_backup_schedules.
type Schedule struct {
	ID            string     `gorm:"column:id;primaryKey"`
	TenantID      string     `gorm:"column:tenant_id"`
	Enabled       bool       `gorm:"column:enabled"`
	KeepScheduled int        `gorm:"column:keep_scheduled"`
	CreatedBy     *string    `gorm:"column:created_by"`
	UpdatedBy     *string    `gorm:"column:updated_by"`
	DeletedAt     *time.Time `gorm:"column:deleted_at"`
}

// TableName pins the fixed system table.
func (Schedule) TableName() string { return "sys_tenant_backup_schedules" }

// ScheduleConfig is the part of the TENANT_BACKUP_* settings the schedule needs.
type ScheduleConfig struct {
	HourLocal           int // first hour (Asia/Ho_Chi_Minh) at which the daily backup is due
	KeepPreRestore      int
	FailedRetentionDays int
}

// ScheduleStore is the persistence port of the schedule and the retention (fakeable; implemented by GormStore).
type ScheduleStore interface {
	// ActiveTenants lists the ids the sweep walks — ids only, no customer data (same precedent as DoctorScheduler).
	ActiveTenants(ctx context.Context) ([]string, error)
	// Schedule returns the tenant's row, or nil when none exists (the default applies).
	Schedule(ctx context.Context, tenantID string) (*Schedule, error)
	SaveSchedule(ctx context.Context, tenantID string, enabled bool, keep int, userID string) error
	// SlotExists is true when the slot is taken, by a soft-deleted row too (a pruned backup must not be retaken the same day).
	SlotExists(ctx context.Context, tenantID, slot string) (bool, error)
	// Live lists the tenant's non-deleted backups.
	Live(ctx context.Context, tenantID string) ([]Backup, error)
	// InUse returns the ids that must not be removed: a restore in flight and the backup it restores from.
	InUse(ctx context.Context, tenantID string) (map[string]bool, error)
	// SoftDelete hides a backup; 0 rows = already gone.
	SoftDelete(ctx context.Context, tenantID, id string) (int64, error)
}

// WithSchedule wires the schedule store + settings. Chainable. Without it the schedule / delete
// routes fail with an error rather than pretend.
func (s *Service) WithSchedule(st ScheduleStore, cfg ScheduleConfig) *Service {
	s.sched, s.schedCfg = st, cfg
	return s
}

// ---- pure rules -------------------------------------------------------------------------------

// dailySlot is the schedule_slot of the local day containing now.
func dailySlot(now time.Time) string { return "d:" + now.In(ictZone).Format("2006-01-02") }

// dueSlots lists, in priority order, the slots that may need a backup now: the daily one once the
// local hour is reached, and the catch-up one when the newest ready backup predates the current
// schema (a deploy changed the structure). The caller takes the first that is not already taken —
// a daily backup taken after the deploy already carries the new version, so it wins.
func dueSlots(now time.Time, hour int, curVer int64, latestReadyVer *int64) []string {
	var out []string
	if now.In(ictZone).Hour() >= hour {
		out = append(out, dailySlot(now))
	}
	if latestReadyVer != nil && *latestReadyVer < curVer {
		out = append(out, fmt.Sprintf("v:%d", curVer))
	}
	return out
}

// nextDue is when the next scheduled backup will start: today's hour if it is still ahead, now if
// the day's backup has not been taken yet, otherwise tomorrow's hour.
func nextDue(now time.Time, hour int, dayTaken bool) time.Time {
	l := now.In(ictZone)
	todayAt := time.Date(l.Year(), l.Month(), l.Day(), hour, 0, 0, 0, ictZone)
	switch {
	case l.Before(todayAt):
		return todayAt
	case !dayTaken:
		return now
	default:
		return todayAt.AddDate(0, 0, 1)
	}
}

type retentionPolicy struct{ KeepScheduled, KeepPreRestore, FailedDays int }

// expired picks the backups retention removes (FR-057): scheduled-ready beyond the newest N,
// pre_restore-ready beyond the newest M, failed older than FailedDays. Manual ready backups and
// running ones are never picked, and neither is anything in protected.
func expired(rows []Backup, now time.Time, p retentionPolicy, protected map[string]bool) []Backup {
	byNewest := func(kind string) []Backup {
		var out []Backup
		for _, b := range rows {
			if b.Kind == kind && b.Status == StatusReady {
				out = append(out, b)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.After(out[j].CreatedAt)
			}
			return out[i].ID > out[j].ID
		})
		return out
	}
	var out []Backup
	add := func(b Backup) {
		if !protected[b.ID] {
			out = append(out, b)
		}
	}
	if s := byNewest(KindScheduled); len(s) > p.KeepScheduled {
		for _, b := range s[p.KeepScheduled:] {
			add(b)
		}
	}
	if s := byNewest(KindPreRestore); len(s) > p.KeepPreRestore {
		for _, b := range s[p.KeepPreRestore:] {
			add(b)
		}
	}
	cutoff := now.Add(-time.Duration(p.FailedDays) * 24 * time.Hour)
	for _, b := range rows {
		if b.Status != StatusFailed {
			continue
		}
		at := b.CreatedAt
		if b.FinishedAt != nil {
			at = *b.FinishedAt
		}
		if at.Before(cutoff) {
			add(b)
		}
	}
	return out
}

// ---- service: sweep ---------------------------------------------------------------------------

// SweepSchedules runs one pass: fail abandoned running backups, then for every active customer take
// the backup that is due and prune. Customers are walked one after another — a backup streams a
// whole customer through the disk, and two at once would only slow both.
func (s *Service) SweepSchedules(ctx context.Context) error {
	if s.sched == nil {
		return errors.New("tenantbackup.Service.SweepSchedules: schedule store is not wired")
	}
	if _, err := s.ReapStale(ctx); err != nil {
		s.logger.Error("tenantbackup.Service.SweepSchedules: reap failed", zap.Error(err))
	}
	ids, err := s.sched.ActiveTenants(ctx)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.SweepSchedules: tenants: %w", err)
	}
	now := s.now()
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		s.sweepTenant(ctx, id, now)
	}
	return nil
}

// settings returns the effective (enabled, keep) of a tenant: no row = the default.
func (s *Service) settings(ctx context.Context, tenantID string) (bool, int, error) {
	sc, err := s.sched.Schedule(ctx, tenantID)
	if err != nil {
		return false, 0, err
	}
	if sc == nil {
		return true, defaultKeepScheduled, nil
	}
	return sc.Enabled, sc.KeepScheduled, nil
}

// sweepTenant never returns an error: one broken customer must not stop the rest, so every failure is logged.
func (s *Service) sweepTenant(ctx context.Context, tenantID string, now time.Time) {
	log := s.logger.With(zap.String("tenant_id", tenantID))
	enabled, keep, err := s.settings(ctx, tenantID)
	if err != nil {
		log.Error("tenantbackup.Service.sweepTenant: schedule read failed", zap.Error(err))
		return
	}
	if enabled {
		s.takeDue(ctx, tenantID, now, log)
	}
	s.prune(ctx, tenantID, now, keep, log)
}

func (s *Service) takeDue(ctx context.Context, tenantID string, now time.Time, log *zap.Logger) {
	rows, err := s.sched.Live(ctx, tenantID)
	if err != nil {
		log.Error("tenantbackup.Service.takeDue: list failed", zap.Error(err))
		return
	}
	ver, _, err := s.store.CurrentSchema(ctx)
	if err != nil {
		log.Error("tenantbackup.Service.takeDue: schema version failed", zap.Error(err))
		return
	}
	var newest *Backup
	for i := range rows {
		b := &rows[i]
		if b.Status == StatusReady && (newest == nil || b.CreatedAt.After(newest.CreatedAt)) {
			newest = b
		}
	}
	var latest *int64
	if newest != nil {
		latest = &newest.SchemaVersion
	}
	for _, slot := range dueSlots(now, s.schedCfg.HourLocal, ver, latest) {
		taken, err := s.sched.SlotExists(ctx, tenantID, slot)
		if err != nil {
			log.Error("tenantbackup.Service.takeDue: slot check failed", zap.String("slot", slot), zap.Error(err))
			return
		}
		if taken {
			continue
		}
		_, done, err := s.start(ctx, CreateInput{TenantID: tenantID}, KindScheduled, slot)
		var be *Error
		switch {
		case err == nil:
			select {
			case <-done:
			case <-ctx.Done():
			}
		case errors.Is(err, ErrSlotTaken), errors.Is(err, ErrInProgress):
			// Another replica / another operation got there first: this pass has nothing to do.
		case errors.As(err, &be):
			// BACKUP_DISK_LOW and BACKUP_IN_PROGRESS skip the customer this pass: no failed row is
			// written for a backup that never started (FR-070).
			log.Warn("tenantbackup.Service.takeDue: backup skipped", zap.String("slot", slot), zap.String("code", be.Code))
		default:
			log.Error("tenantbackup.Service.takeDue: backup not started", zap.String("slot", slot), zap.Error(err))
		}
		return // one backup per pass: a daily backup already carries the catch-up version
	}
}

func (s *Service) prune(ctx context.Context, tenantID string, now time.Time, keep int, log *zap.Logger) {
	rows, err := s.sched.Live(ctx, tenantID)
	if err != nil {
		log.Error("tenantbackup.Service.prune: list failed", zap.Error(err))
		return
	}
	protected, err := s.sched.InUse(ctx, tenantID)
	if err != nil {
		log.Error("tenantbackup.Service.prune: in-use read failed", zap.Error(err))
		return
	}
	pol := retentionPolicy{KeepScheduled: keep, KeepPreRestore: s.schedCfg.KeepPreRestore, FailedDays: s.schedCfg.FailedRetentionDays}
	victims := expired(rows, now, pol, protected)
	if len(victims) == 0 {
		return
	}
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		log.Error("tenantbackup.Service.prune: tenant read failed", zap.Error(err))
		return
	}
	for i := range victims {
		b := &victims[i]
		// File first, row second: a crash between the two leaves a row with no file (shown as
		// FILE_MISSING, retried next pass), never a file nobody lists.
		if b.StorageKey != nil {
			if err := s.backups.Delete(ctx, *b.StorageKey); err != nil {
				log.Error("tenantbackup.Service.prune: file removal failed", zap.String("backup_id", b.ID), zap.Error(err))
				continue
			}
		}
		if _, err := s.sched.SoftDelete(ctx, tenantID, b.ID); err != nil {
			log.Error("tenantbackup.Service.prune: row removal failed", zap.String("backup_id", b.ID), zap.Error(err))
			continue
		}
		s.auditBoth(ctx, Actor{}, t, b, audit.ActionDelete, SystemActorLabel)
		log.Info("tenantbackup.Service.prune: expired backup removed", zap.String("backup_id", b.ID), zap.String("kind", b.Kind))
	}
}

// ---- Scheduler --------------------------------------------------------------------------------

// Scheduler ticks SweepSchedules (the DoctorScheduler lifecycle: idempotent Start/Stop, panic-recovering
// ticks, first tick after one full interval so boot never races wiring).
type Scheduler struct {
	svc      *Service
	interval time.Duration
	logger   *zap.Logger

	mu        sync.Mutex
	started   bool
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewScheduler builds (does not start) the ticker. logger may be nil.
func NewScheduler(svc *Service, interval time.Duration, logger *zap.Logger) *Scheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval < time.Second {
		interval = 10 * time.Minute // defensive: a sub-second interval would be a busy loop
	}
	return &Scheduler{svc: svc, interval: interval, logger: logger, done: make(chan struct{})}
}

// Start launches the ticker goroutine (idempotent).
func (sc *Scheduler) Start() {
	sc.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		sc.mu.Lock()
		sc.cancel, sc.started = cancel, true
		sc.mu.Unlock()
		go sc.run(ctx)
	})
}

func (sc *Scheduler) run(ctx context.Context) {
	defer close(sc.done)
	t := time.NewTicker(sc.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.tick(ctx)
		}
	}
}

func (sc *Scheduler) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			sc.logger.Error("tenantbackup.Scheduler: recovered panic during sweep", zap.Any("panic", r))
		}
	}()
	if err := sc.svc.SweepSchedules(ctx); err != nil {
		sc.logger.Error("tenantbackup.Scheduler: sweep failed", zap.Error(err))
	}
}

// Stop cancels the ticker (and a backup the sweep is waiting on is cancelled by Runner.Stop) and
// waits for the goroutine, up to ctx. Idempotent; a no-op when never started.
func (sc *Scheduler) Stop(ctx context.Context) error {
	var err error
	sc.stopOnce.Do(func() {
		sc.mu.Lock()
		started, cancel := sc.started, sc.cancel
		sc.mu.Unlock()
		if !started {
			return
		}
		cancel()
		select {
		case <-sc.done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}
