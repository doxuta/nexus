package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PreviewSessionRepository persists sys_preview_sessions over GORM. Every query is parameterized
// and pinned to tenant_id: a session id from tenant A never resolves under tenant B.
type PreviewSessionRepository struct {
	db *gorm.DB
}

// NewPreviewSessionRepository wires the repository over an existing GORM connection.
func NewPreviewSessionRepository(db *gorm.DB) *PreviewSessionRepository {
	return &PreviewSessionRepository{db: db}
}

// Create inserts a new session row (synchronously, before the token is minted).
func (r *PreviewSessionRepository) Create(ctx context.Context, s *PreviewSession) error {
	if err := r.db.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("auth.PreviewSessionRepository.Create: %w", err)
	}
	return nil
}

// GetByID returns the (tenant, id) row, ended or not — the caller decides what "live" means.
// ErrPreviewSessionNotFound when there is no such row in that tenant.
func (r *PreviewSessionRepository) GetByID(ctx context.Context, tenantID, id string) (*PreviewSession, error) {
	var s PreviewSession
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrPreviewSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.PreviewSessionRepository.GetByID: %w", err)
	}
	return &s, nil
}

// End stamps ended_at / end_reason on a session that has not ended yet and reports whether this
// call ended it. Idempotent: ending an ended (or unknown) session affects zero rows, no error.
func (r *PreviewSessionRepository) End(ctx context.Context, tenantID, id, reason string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&PreviewSession{}).
		Where("tenant_id = ? AND id = ? AND ended_at IS NULL", tenantID, id).
		Updates(map[string]any{"ended_at": now, "end_reason": reason})
	if res.Error != nil {
		return false, fmt.Errorf("auth.PreviewSessionRepository.End: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}
