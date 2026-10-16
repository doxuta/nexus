// usage_roller.go — specs/066 L4: the janitor-pattern ticker that rolls each tenant's per-day usage
// (rule runs, mail, webhooks out and in) into sys_usage_daily, so a day's figure outlives the
// retention janitor that deletes the source rows.
//
// Per tick and tenant: today and yesterday are OVERWRITTEN (the source is still complete); the first
// time the roller sees a tenant it also fills the 28 days before them with fillMissing (never lowers
// a figure that is already stored). Tenant iteration is the same deliberate tenant-agnostic exception
// the janitor and DoctorScheduler make: enumerating tenant ids is a cluster-level concern, every
// statement inside the loop is pinned to one tenant.
package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/usage"
)

const (
	usageRollerWindow    = 2000 // tenants per sweep (as doctorSchedulerWindow)
	usageBackfillDays    = 28   // days before yesterday filled once per tenant
	usageRollerFirstWait = 60 * time.Second
)

// usageStore is the roller's and the service's consumer-side view of *usage.Repository.
type usageStore interface {
	CountersForDay(ctx context.Context, tenantID string, fromUTC, toUTC time.Time) (map[usage.Key]int64, error)
	Upsert(ctx context.Context, rows []usage.Row, mode usage.Mode) error
}

// usageTenantLister lists the sweep universe. Satisfied by *doctor.SQLStore.
type usageTenantLister interface {
	ListActiveTenants(ctx context.Context, limit int) ([]string, error)
}

// usageDayBounds returns the calendar day (YYYY-MM-DD) that contains t in APP_TIMEZONE and its
// [from, to) edges in UTC. Built with time.Date so a DST zone still gets a 23/25-hour day right.
func usageDayBounds(t time.Time) (day string, fromUTC, toUTC time.Time) {
	loc := appLocation()
	y, m, d := t.In(loc).Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, loc)
	to := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	return from.Format("2006-01-02"), from.UTC(), to.UTC()
}

// UsageRoller is the background roll-up ticker (idempotent Start/Stop, panic-recovering ticks).
type UsageRoller struct {
	interval  time.Duration
	firstWait time.Duration
	store     usageStore
	tenants   usageTenantLister
	now       func() time.Time
	logger    *zap.Logger
	gauges    *UsageGaugeSource

	mu         sync.Mutex
	backfilled map[string]struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// NewUsageRoller builds (does NOT start) a roller. interval 0 is the caller's business — main.go
// only Starts when the config is Enabled().
func NewUsageRoller(interval time.Duration, store usageStore, tenants usageTenantLister, logger *zap.Logger) *UsageRoller {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &UsageRoller{
		interval: interval, firstWait: usageRollerFirstWait, store: store, tenants: tenants,
		now: time.Now, logger: logger, backfilled: map[string]struct{}{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// WithGauges turns on the instant snapshot (specs/066 L5) taken after the counters of each tenant.
func (r *UsageRoller) WithGauges(g *UsageGaugeSource) *UsageRoller {
	r.gauges = g
	return r
}

// Start launches the ticker goroutine (idempotent). The first sweep runs after a short wait so boot
// never races wiring; later ones every interval.
func (r *UsageRoller) Start() { r.startOnce.Do(func() { go r.run() }) }

func (r *UsageRoller) run() {
	defer close(r.done)
	interval := r.interval
	if interval < time.Second {
		interval = time.Hour // defensive: a sub-second interval would be a busy loop
	}
	wait := r.firstWait
	if wait > interval {
		wait = interval
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-timer.C:
			r.sweepOnce(context.Background())
			timer.Reset(interval)
		}
	}
}

func (r *UsageRoller) sweepOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("usage roller: recovered panic during sweep", zap.Any("panic", rec))
		}
	}()
	r.Sweep(ctx)
}

// Sweep performs one roll-up pass over every active tenant. Exported so a test can drive a pass
// without the ticker. A failing tenant is logged and the sweep continues with the others.
func (r *UsageRoller) Sweep(ctx context.Context) {
	tenants, err := r.tenants.ListActiveTenants(ctx, usageRollerWindow)
	if err != nil {
		r.logger.Warn("usage roller: list tenants failed", zap.Error(err))
		return
	}
	started := time.Now()
	now := r.now().In(appLocation())
	failed := 0
	for _, tenantID := range tenants {
		if err := r.rollTenant(ctx, tenantID, now); err != nil {
			failed++
			r.logger.Warn("usage roller: tenant failed", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}
	r.logger.Info("usage roller: sweep done", zap.Int("tenants", len(tenants)), zap.Int("failed", failed),
		zap.Duration("elapsed", time.Since(started)))
}

func (r *UsageRoller) rollTenant(ctx context.Context, tenantID string, now time.Time) error {
	// ponytail: UPSERT is idempotent, so replicas need no lock (as the janitor).
	for back := 0; back <= 1; back++ {
		if err := r.rollDay(ctx, tenantID, now.AddDate(0, 0, -back), usage.Overwrite); err != nil {
			return err
		}
	}
	if err := r.rollGauge(ctx, tenantID, now); err != nil {
		return err
	}
	r.mu.Lock()
	_, done := r.backfilled[tenantID]
	r.mu.Unlock()
	if done {
		return nil
	}
	for back := 2; back < 2+usageBackfillDays; back++ {
		if err := r.rollDay(ctx, tenantID, now.AddDate(0, 0, -back), usage.FillMissing); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.backfilled[tenantID] = struct{}{}
	r.mu.Unlock()
	return nil
}

// rollGauge overwrites today's snapshot. ponytail: each tick runs one COUNT per entity table of every
// tenant (idx_created_at prefix); past ~2000 tenants x 50 tables per hour, snapshot less often.
func (r *UsageRoller) rollGauge(ctx context.Context, tenantID string, now time.Time) error {
	if r.gauges == nil {
		return nil
	}
	g, err := r.gauges.take(ctx, tenantID, now)
	if err != nil {
		return fmt.Errorf("services.UsageRoller.rollGauge: %w", err)
	}
	day, _, _ := usageDayBounds(now)
	if err := r.store.Upsert(ctx, g.Rows(tenantID, day), usage.Overwrite); err != nil {
		return fmt.Errorf("services.UsageRoller.rollGauge: %w", err)
	}
	return nil
}

func (r *UsageRoller) rollDay(ctx context.Context, tenantID string, at time.Time, mode usage.Mode) error {
	day, from, to := usageDayBounds(at)
	counters, err := r.store.CountersForDay(ctx, tenantID, from, to)
	if err != nil {
		return fmt.Errorf("services.UsageRoller.rollDay: %w", err)
	}
	rows := make([]usage.Row, 0, len(counters))
	for k, v := range counters {
		rows = append(rows, usage.Row{TenantID: tenantID, Day: day, Metric: k.Metric, Dim: k.Dim, Value: v})
	}
	if err := r.store.Upsert(ctx, rows, mode); err != nil {
		return fmt.Errorf("services.UsageRoller.rollDay: %w", err)
	}
	return nil
}

// Stop signals the goroutine to exit and waits (up to the context deadline). Idempotent; only call
// after Start.
func (r *UsageRoller) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
