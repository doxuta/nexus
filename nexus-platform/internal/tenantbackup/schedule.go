package tenantbackup

// schedule.go — the operator-facing half of slice E: read / change one customer's schedule and
// delete one backup by hand (FR-058, FR-059). The sweep itself is in scheduler.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
)

// ScheduleView is the API shape of a customer's schedule.
type ScheduleView struct {
	Enabled         bool       `json:"enabled"`
	KeepScheduled   int        `json:"keepScheduled"`
	HourLocal       int        `json:"hourLocal"`
	LastScheduledAt *time.Time `json:"lastScheduledAt"`
	NextDueAt       *time.Time `json:"nextDueAt"`
}

var errScheduleNotWired = errors.New("tenantbackup.Service: schedule store is not wired")

func (s *Service) scheduleView(ctx context.Context, tenantID string) (*ScheduleView, error) {
	enabled, keep, err := s.settings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := s.sched.Live(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	v := &ScheduleView{Enabled: enabled, KeepScheduled: keep, HourLocal: s.schedCfg.HourLocal}
	for _, b := range rows {
		if b.Kind == KindScheduled && (v.LastScheduledAt == nil || b.CreatedAt.After(*v.LastScheduledAt)) {
			at := b.CreatedAt
			v.LastScheduledAt = &at
		}
	}
	if enabled {
		now := s.now()
		taken, err := s.sched.SlotExists(ctx, tenantID, dailySlot(now))
		if err != nil {
			return nil, err
		}
		next := nextDue(now, s.schedCfg.HourLocal, taken)
		v.NextDueAt = &next
	}
	return v, nil
}

// GetSchedule returns the customer's schedule (the default when none was saved).
func (s *Service) GetSchedule(ctx context.Context, tenantID string) (*ScheduleView, error) {
	if s.sched == nil {
		return nil, errScheduleNotWired
	}
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.GetSchedule: tenant: %w", err)
	}
	v, err := s.scheduleView(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.GetSchedule: %w", err)
	}
	return v, nil
}

// PutSchedule saves enabled + keepScheduled (1..30). Both are required: a PUT replaces the setting.
func (s *Service) PutSchedule(ctx context.Context, tenantID string, actor Actor, enabled *bool, keep *int) (*ScheduleView, error) {
	if s.sched == nil {
		return nil, errScheduleNotWired
	}
	if enabled == nil || keep == nil || *keep < 1 || *keep > maxKeepScheduled {
		return nil, &Error{Code: "BACKUP_SCHEDULE_INVALID", Msg: "Cần enabled (true/false) và keepScheduled là số nguyên từ 1 đến 30"}
	}
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.PutSchedule: tenant: %w", err)
	}
	if err := s.sched.SaveSchedule(ctx, t.ID, *enabled, *keep, actor.UserID); err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.PutSchedule: %w", err)
	}
	label := s.store.OperatorLabel(ctx, actor.TenantID, actor.UserID)
	after := map[string]any{"tenantId": t.ID, "enabled": *enabled, "keepScheduled": *keep}
	at := s.now().UTC()
	if actor.TenantID != "" && actor.TenantID != t.ID {
		s.audit.Record(ctx, audit.Event{TenantID: actor.TenantID, ActorUserID: actor.UserID, EntityName: audit.TenantBackupMeta,
			RecordID: t.ID, Action: audit.ActionUpdate, Diff: &audit.Diff{After: after}, At: at})
	}
	s.audit.Record(ctx, audit.Event{TenantID: t.ID, ActorUserID: actorOrSystem(actor.UserID), EntityName: audit.TenantBackupMeta,
		RecordID: t.ID, Action: audit.ActionUpdate,
		Diff: &audit.Diff{After: map[string]any{"enabled": *enabled, "keepScheduled": *keep, "by": label}}, At: at})
	s.logger.Info("tenantbackup.Service.PutSchedule: schedule saved", zap.String("tenant_id", t.ID),
		zap.Bool("enabled", *enabled), zap.Int("keep", *keep), zap.String("operator_user_id", actor.UserID))
	v, err := s.scheduleView(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.PutSchedule: view: %w", err)
	}
	return v, nil
}

// Delete removes one backup by hand (FR-058): its file first, then the row is hidden. A backup that
// is still being written, or that a restore is running on / from, is 409 BACKUP_DELETE_IN_USE.
func (s *Service) Delete(ctx context.Context, tenantID string, actor Actor, id string) error {
	if s.sched == nil {
		return errScheduleNotWired
	}
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return tenantNotFound()
		}
		return fmt.Errorf("tenantbackup.Service.Delete: tenant: %w", err)
	}
	b, err := s.getBackup(ctx, t.ID, id)
	if err != nil {
		return err
	}
	inUse, err := s.sched.InUse(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.Delete: %w", err)
	}
	if b.Status == StatusRunning || inUse[b.ID] {
		return &Error{Code: "BACKUP_DELETE_IN_USE", Msg: "Bản sao lưu đang được dùng (đang ghi hoặc đang khôi phục) — chờ xong rồi xoá"}
	}
	if b.StorageKey != nil {
		if err := s.backups.Delete(ctx, *b.StorageKey); err != nil {
			return fmt.Errorf("tenantbackup.Service.Delete: file: %w", err)
		}
	}
	n, err := s.sched.SoftDelete(ctx, t.ID, b.ID)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.Delete: %w", err)
	}
	if n == 0 {
		// Lost a race with another remover or a restore that started: the file is already gone, so tell the caller it is in use.
		return &Error{Code: "BACKUP_DELETE_IN_USE", Msg: "Bản sao lưu vừa được dùng — tải lại danh sách rồi thử lại"}
	}
	label := s.store.OperatorLabel(ctx, actor.TenantID, actor.UserID)
	s.auditBoth(ctx, actor, t, b, audit.ActionDelete, label)
	s.logger.Warn("tenantbackup.Service.Delete: backup deleted by hand", zap.String("tenant_id", t.ID), zap.String("backup_id", b.ID),
		zap.String("operator_tenant_id", actor.TenantID), zap.String("operator_user_id", actor.UserID))
	return nil
}
