package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// freezeCacheTTL bounds how long a replica may keep serving a stale "not frozen" answer; the restore
// runner waits 2×TTL+3 s after setting the freeze so every replica has dropped it (plan D-16).
const freezeCacheTTL = 2 * time.Second

// FreezeRepo is the slice of the auth repository the checker needs (consumer-side interface).
type FreezeRepo interface {
	GetMaintenance(ctx context.Context, tenantID string) (*time.Time, string, error)
	SetMaintenance(ctx context.Context, tenantID string, until time.Time, reason string) error
	ClearMaintenance(ctx context.Context, tenantID string) error
}

// ErrTenantMaintenance: the tenant is write-frozen right now. Callers map it to 503 + Retry-After.
var ErrTenantMaintenance = errors.New("services: tenant under maintenance")

// MaintenanceError carries the deadline; errors.Is(err, ErrTenantMaintenance) holds.
type MaintenanceError struct{ Until time.Time }

func (e *MaintenanceError) Error() string        { return ErrTenantMaintenance.Error() }
func (e *MaintenanceError) Is(target error) bool { return target == ErrTenantMaintenance }

// FreezeReader is the consumer-side view of TenantFreeze used by services.
type FreezeReader interface {
	FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error)
}

type freezeEntry struct {
	until     time.Time // zero = not frozen
	fetchedAt time.Time
}

// TenantFreeze is the ONE answer to "is this tenant write-frozen right now?" (specs/068 slice B,
// plan D-15). The write middleware, inbound webhooks, the cross-tenant operator target, the job
// runner and the three schedulers all call FrozenUntil — no gate interprets a read error itself.
type TenantFreeze struct {
	repo   FreezeRepo
	logger *zap.Logger
	now    func() time.Time
	cache  sync.Map // tenantID -> freezeEntry
}

// NewTenantFreeze wires the checker. logger may be nil.
func NewTenantFreeze(repo FreezeRepo, logger *zap.Logger) *TenantFreeze {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &TenantFreeze{repo: repo, logger: logger, now: time.Now}
}

// FrozenUntil reports whether tenantID is frozen and until when. On a read error: if the latest
// cached entry (even past its TTL) says the freeze outlives now, it STAYS frozen (until, true, err);
// a tenant never known to be frozen passes (0, false, err) and the caller logs (FR-030a).
func (f *TenantFreeze) FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error) {
	now := f.now()
	prev, havePrev := f.load(tenantID)
	if havePrev && now.Sub(prev.fetchedAt) < freezeCacheTTL {
		return frozenAnswer(prev.until, now)
	}
	until, _, err := f.repo.GetMaintenance(ctx, tenantID)
	if err != nil {
		err = fmt.Errorf("services.TenantFreeze.FrozenUntil: %w", err)
		f.logger.Error("tenant freeze read failed", zap.String("tenant_id", tenantID), zap.Error(err))
		if havePrev && prev.until.After(now) {
			return prev.until, true, err
		}
		return time.Time{}, false, err
	}
	e := freezeEntry{fetchedAt: now}
	if until != nil {
		e.until = *until
	}
	f.cache.Store(tenantID, e)
	return frozenAnswer(e.until, now)
}

func frozenAnswer(until, now time.Time) (time.Time, bool, error) {
	if until.After(now) {
		return until, true, nil
	}
	return time.Time{}, false, nil
}

func (f *TenantFreeze) load(tenantID string) (freezeEntry, bool) {
	v, ok := f.cache.Load(tenantID)
	if !ok {
		return freezeEntry{}, false
	}
	return v.(freezeEntry), true
}

// Invalidate drops this replica's cached answer for the tenant.
func (f *TenantFreeze) Invalidate(tenantID string) { f.cache.Delete(tenantID) }

// Set starts a freeze and returns the manual freeze that was still live before it (nil when none),
// so a restore can hand it back through Clear(prev).
func (f *TenantFreeze) Set(ctx context.Context, tenantID string, until time.Time, reason string) (*time.Time, error) {
	cur, _, err := f.repo.GetMaintenance(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.TenantFreeze.Set: read previous: %w", err)
	}
	var prev *time.Time
	if cur != nil && cur.After(f.now()) {
		prev = cur
	}
	if err := f.repo.SetMaintenance(ctx, tenantID, until, reason); err != nil {
		return nil, fmt.Errorf("services.TenantFreeze.Set: %w", err)
	}
	f.Invalidate(tenantID)
	return prev, nil
}

// Extend moves the deadline (a restore heartbeat) without touching the reason.
func (f *TenantFreeze) Extend(ctx context.Context, tenantID string, until time.Time) error {
	_, reason, err := f.repo.GetMaintenance(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.TenantFreeze.Extend: read: %w", err)
	}
	if err := f.repo.SetMaintenance(ctx, tenantID, until, reason); err != nil {
		return fmt.Errorf("services.TenantFreeze.Extend: %w", err)
	}
	f.Invalidate(tenantID)
	return nil
}

// Clear lifts the freeze. A prev that is still in the future (the manual freeze a restore found) is
// put back as a manual freeze instead of unlocking early.
func (f *TenantFreeze) Clear(ctx context.Context, tenantID string, prev *time.Time) error {
	var err error
	if prev != nil && prev.After(f.now()) {
		err = f.repo.SetMaintenance(ctx, tenantID, *prev, "manual")
	} else {
		err = f.repo.ClearMaintenance(ctx, tenantID)
	}
	f.Invalidate(tenantID)
	if err != nil {
		return fmt.Errorf("services.TenantFreeze.Clear: %w", err)
	}
	return nil
}
