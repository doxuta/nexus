package tenantbackup

// maintenance.go — the operator's MANUAL write freeze of one customer (specs/068 slice B, FR-024).
// The freeze itself lives in services.TenantFreeze (the one checker every gate reads); this file is
// the rules around switching it by hand: minutes 1..120, never the operator's own workspace, never
// while a restore holds the freeze, and both trails get a row.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
)

const (
	minMaintenanceMinutes = 1
	maxMaintenanceMinutes = 120
	// manualFreezeReason marks a freeze set by hand (a restore uses "restore").
	manualFreezeReason = "manual"
)

// Freezer is the write-freeze control the service needs (services.TenantFreeze; defined here because
// this package imports neither services nor net/http).
type Freezer interface {
	Set(ctx context.Context, tenantID string, until time.Time, reason string) (prev *time.Time, err error)
	Clear(ctx context.Context, tenantID string, prev *time.Time) error
	FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error)
}

// WithFreezer wires the freeze control. Chainable. Without it SetMaintenance/ClearMaintenance fail
// with an error rather than pretend to have frozen anything.
func (s *Service) WithFreezer(f Freezer) *Service {
	s.freezer = f
	return s
}

// MaintenanceInput is one "Tạm khoá ghi" / "Mở khoá" request. Minutes is ignored by Clear.
type MaintenanceInput struct {
	TenantID string
	Minutes  int
	Actor    Actor
}

// MaintenanceView is the answer to a successful freeze.
type MaintenanceView struct {
	MaintenanceUntil time.Time `json:"maintenanceUntil"`
}

// checkMaintenanceTarget runs the refusals shared by Set and Clear, in the order the operator should
// learn about them: own workspace, unknown customer, a restore that owns the freeze.
func (s *Service) checkMaintenanceTarget(ctx context.Context, in MaintenanceInput) (*TenantInfo, error) {
	if s.freezer == nil {
		return nil, errors.New("tenantbackup.Service: write freeze is not wired")
	}
	if in.TenantID == in.Actor.TenantID {
		return nil, &Error{Code: "MAINTENANCE_SELF", Msg: "Không thể khoá ghi chính không gian bạn đang đứng"}
	}
	t, err := s.store.TenantInfo(ctx, in.TenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.checkMaintenanceTarget: tenant: %w", err)
	}
	restoring, err := s.store.RestoreRunning(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.checkMaintenanceTarget: restore state: %w", err)
	}
	if restoring {
		return nil, &Error{Code: "BACKUP_IN_PROGRESS", Msg: "Khách này đang khôi phục dữ liệu — khoá ghi do việc khôi phục giữ, chờ xong rồi thử lại"}
	}
	return t, nil
}

// SetMaintenance freezes writes of one customer for Minutes (1..120).
func (s *Service) SetMaintenance(ctx context.Context, in MaintenanceInput) (*MaintenanceView, error) {
	if in.Minutes < minMaintenanceMinutes || in.Minutes > maxMaintenanceMinutes {
		return nil, &Error{Code: "MAINTENANCE_MINUTES_INVALID", Msg: "Số phút khoá ghi phải từ 1 đến 120",
			Details: map[string]any{"min": minMaintenanceMinutes, "max": maxMaintenanceMinutes}}
	}
	t, err := s.checkMaintenanceTarget(ctx, in)
	if err != nil {
		return nil, err
	}
	until := s.now().UTC().Add(time.Duration(in.Minutes) * time.Minute)
	if _, err := s.freezer.Set(ctx, t.ID, until, manualFreezeReason); err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.SetMaintenance: %w", err)
	}
	s.auditMaintenance(ctx, in.Actor, t, map[string]any{"maintenanceUntil": until.Format(time.RFC3339), "minutes": in.Minutes})
	s.logger.Info("tenantbackup.Service.SetMaintenance: write freeze set",
		zap.String("tenant_id", t.ID), zap.String("operator_tenant_id", in.Actor.TenantID),
		zap.String("operator_user_id", in.Actor.UserID), zap.Time("until", until))
	return &MaintenanceView{MaintenanceUntil: until}, nil
}

// ClearMaintenance lifts the freeze of one customer (idempotent).
func (s *Service) ClearMaintenance(ctx context.Context, in MaintenanceInput) error {
	t, err := s.checkMaintenanceTarget(ctx, in)
	if err != nil {
		return err
	}
	if err := s.freezer.Clear(ctx, t.ID, nil); err != nil {
		return fmt.Errorf("tenantbackup.Service.ClearMaintenance: %w", err)
	}
	s.auditMaintenance(ctx, in.Actor, t, map[string]any{"maintenanceUntil": nil})
	s.logger.Info("tenantbackup.Service.ClearMaintenance: write freeze lifted",
		zap.String("tenant_id", t.ID), zap.String("operator_tenant_id", in.Actor.TenantID),
		zap.String("operator_user_id", in.Actor.UserID))
	return nil
}

// auditMaintenance writes the row in the workshop's trail (with the customer's slug) and in the
// customer's own trail (with the operator's label, never an id of the workshop).
func (s *Service) auditMaintenance(ctx context.Context, actor Actor, t *TenantInfo, after map[string]any) {
	at := s.now().UTC()
	label := s.store.OperatorLabel(ctx, actor.TenantID, actor.UserID)
	ours := map[string]any{"tenantId": t.ID, "slug": t.Slug, "kind": "maintenance"}
	theirs := map[string]any{"kind": "maintenance", "by": label}
	for k, v := range after {
		ours[k], theirs[k] = v, v
	}
	s.audit.Record(ctx, audit.Event{
		TenantID: actor.TenantID, ActorUserID: actor.UserID, EntityName: audit.TenantBackupMeta, RecordID: t.ID,
		Action: audit.ActionUpdate, Diff: &audit.Diff{After: ours}, At: at,
	})
	s.audit.Record(ctx, audit.Event{
		TenantID: t.ID, ActorUserID: actorOrSystem(actor.UserID), EntityName: audit.TenantBackupMeta, RecordID: t.ID,
		Action: audit.ActionUpdate, Diff: &audit.Diff{After: theirs}, At: at,
	})
}
