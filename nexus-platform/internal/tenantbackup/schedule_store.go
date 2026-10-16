package tenantbackup

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ ScheduleStore = (*GormStore)(nil)

// ActiveTenants lists the active, non-retired tenant ids. It is the one cross-tenant read of the
// schedule and reads ids only (precedent: doctor.SQLStore.ListActiveTenants, jobs.ReapStuck) —
// every backup row it leads to is pinned to its own tenant_id.
func (r *GormStore) ActiveTenants(ctx context.Context) ([]string, error) {
	var ids []string
	if err := r.sdb.SelectContext(ctx, &ids, "SELECT id FROM tenants WHERE status = 'active' AND deleted_at IS NULL ORDER BY id"); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ActiveTenants: %w", err)
	}
	return ids, nil
}

// Schedule reads the tenant's schedule row; nil = none (the default applies).
func (r *GormStore) Schedule(ctx context.Context, tenantID string) (*Schedule, error) {
	var sc Schedule
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID).Take(&sc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.Schedule: %w", err)
	}
	return &sc, nil
}

// SaveSchedule upserts on uq_tbs_tenant.
func (r *GormStore) SaveSchedule(ctx context.Context, tenantID string, enabled bool, keep int, userID string) error {
	row := Schedule{ID: uuid.NewString(), TenantID: tenantID, Enabled: enabled, KeepScheduled: keep}
	if userID != "" {
		row.CreatedBy, row.UpdatedBy = &userID, &userID
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "keep_scheduled", "updated_by"}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("tenantbackup.GormStore.SaveSchedule: %w", err)
	}
	return nil
}

// SlotExists counts the slot including soft-deleted rows: uq_tbk_slot still holds them.
func (r *GormStore) SlotExists(ctx context.Context, tenantID, slot string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Table("sys_tenant_backups").
		Where("tenant_id = ? AND schedule_slot = ?", tenantID, slot).Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("tenantbackup.GormStore.SlotExists: %w", err)
	}
	return n > 0, nil
}

// Live lists every non-deleted backup of the tenant.
func (r *GormStore) Live(ctx context.Context, tenantID string) ([]Backup, error) {
	var out []Backup
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.Live: %w", err)
	}
	return out, nil
}

// InUse returns the rows a restore is running on and the backups they restore from.
func (r *GormStore) InUse(ctx context.Context, tenantID string) (map[string]bool, error) {
	var rows []struct {
		ID       string  `gorm:"column:id"`
		SourceID *string `gorm:"column:restore_source_id"`
	}
	err := r.db.WithContext(ctx).Table("sys_tenant_backups").Select("id, restore_source_id").
		Where("tenant_id = ? AND restore_status = ? AND deleted_at IS NULL", tenantID, "running").Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.InUse: %w", err)
	}
	out := map[string]bool{}
	for _, x := range rows {
		out[x.ID] = true
		if x.SourceID != nil {
			out[*x.SourceID] = true
		}
	}
	return out, nil
}

// SoftDelete hides one backup. The WHERE excludes the state it writes and a running row, so a
// second remover (or a backup that started meanwhile) reports 0 rows.
func (r *GormStore) SoftDelete(ctx context.Context, tenantID, id string) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_tenant_backups SET deleted_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL AND status <> ? AND (restore_status IS NULL OR restore_status <> ?)",
		id, tenantID, StatusRunning, "running")
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.SoftDelete: %w", res.Error)
	}
	return res.RowsAffected, nil
}
