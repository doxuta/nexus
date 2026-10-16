// Package workqueue persists role-based work queues (sys_work_queues) — VIEC-HOM-NAY
// (../specs/012-work-queues/spec.md). A queue is PURE METADATA over the existing list/aggregate engine:
// entity + filters (relative tokens allowed) + sort + visibility role + a v1 action verb.
// System table with a fixed schema → GORM per the stack rules; every query is tenant-scoped.
package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// QueueFilter is one stored filter predicate. Value may be a Wave 3.6 relative token — it is
// resolved at QUERY time by the record engine, never materialized here.
type QueueFilter struct {
	Field    string `json:"field"`
	Operator string `json:"op"`
	Value    string `json:"value"`
}

// Queue is one sys_work_queues row.
type Queue struct {
	ID         string          `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID   string          `gorm:"type:char(36);not null" json:"-"`
	Name       string          `gorm:"type:varchar(64);not null" json:"name"`
	Label      string          `gorm:"type:varchar(150);not null" json:"label"`
	EntityName string          `gorm:"type:varchar(64);not null;column:entity_name" json:"entityName"`
	RoleID     *string         `gorm:"type:char(36)" json:"roleId,omitempty"`
	FiltersRaw json.RawMessage `gorm:"type:json;column:filters" json:"-"`
	SortField  *string         `gorm:"type:varchar(64);column:sort_field" json:"sortField,omitempty"`
	SortOrder  string          `gorm:"type:varchar(4);column:sort_order;default:asc" json:"sortOrder"`
	ActionVerb string          `gorm:"type:varchar(16);column:action_verb;default:open" json:"actionVerb"`
	Position   int             `gorm:"not null;default:0" json:"position"`
	IsActive   bool            `gorm:"column:is_active;not null;default:true" json:"isActive"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	DeletedAt  gorm.DeletedAt  `gorm:"index" json:"-"`

	// Filters is the decoded FiltersRaw, populated by the repository on read and serialized on
	// write. Never persisted directly (gorm:"-").
	Filters []QueueFilter `gorm:"-" json:"filters"`
}

// TableName pins the table (avoids GORM pluralization surprises).
func (Queue) TableName() string { return "sys_work_queues" }

// ErrNotFound is returned when a queue id does not exist in the caller's tenant.
var ErrNotFound = errors.New("workqueue: not found")

// ErrDuplicateName is returned when (tenant, name) already exists.
var ErrDuplicateName = errors.New("workqueue: name already exists")

// Repository reads/writes sys_work_queues. Tenant-scoped on every query.
type Repository struct{ db *gorm.DB }

func New(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) encode(q *Queue) error {
	if q.Filters == nil {
		q.Filters = []QueueFilter{}
	}
	b, err := json.Marshal(q.Filters)
	if err != nil {
		return fmt.Errorf("workqueue.encode: %w", err)
	}
	q.FiltersRaw = b
	return nil
}

func decode(q *Queue) {
	q.Filters = []QueueFilter{}
	if len(q.FiltersRaw) > 0 {
		_ = json.Unmarshal(q.FiltersRaw, &q.Filters) // malformed row → empty filters (fail-safe read)
	}
}

// Create inserts one queue. Duplicate (tenant, name) → ErrDuplicateName.
func (r *Repository) Create(ctx context.Context, q *Queue) error {
	if err := r.encode(q); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(q).Error; err != nil {
		if isDuplicate(err) {
			return ErrDuplicateName
		}
		return fmt.Errorf("workqueue.Create: %w", err)
	}
	return nil
}

// Update full-replaces the mutable columns of one queue in the tenant.
func (r *Repository) Update(ctx context.Context, tenantID string, q *Queue) error {
	if err := r.encode(q); err != nil {
		return err
	}
	res := r.db.WithContext(ctx).Model(&Queue{}).
		Where("tenant_id = ? AND id = ?", tenantID, q.ID).
		Updates(map[string]any{
			"name": q.Name, "label": q.Label, "entity_name": q.EntityName,
			"role_id": q.RoleID, "filters": string(q.FiltersRaw),
			"sort_field": q.SortField, "sort_order": q.SortOrder,
			"action_verb": q.ActionVerb, "position": q.Position, "is_active": q.IsActive,
		})
	if res.Error != nil {
		if isDuplicate(res.Error) {
			return ErrDuplicateName
		}
		return fmt.Errorf("workqueue.Update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Distinguish no-op update from missing row (MySQL reports 0 for identical values).
		var n int64
		if err := r.db.WithContext(ctx).Model(&Queue{}).
			Where("tenant_id = ? AND id = ?", tenantID, q.ID).Count(&n).Error; err != nil {
			return fmt.Errorf("workqueue.Update: verify: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// SoftDelete removes one queue (soft) in the tenant.
func (r *Repository) SoftDelete(ctx context.Context, tenantID, id string) error {
	res := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Queue{})
	if res.Error != nil {
		return fmt.Errorf("workqueue.SoftDelete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByID returns one queue in the tenant.
func (r *Repository) GetByID(ctx context.Context, tenantID, id string) (*Queue, error) {
	var q Queue
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&q).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workqueue.GetByID: %w", err)
	}
	decode(&q)
	return &q, nil
}

// ListAll returns every queue of the tenant (admin view), position order.
func (r *Repository) ListAll(ctx context.Context, tenantID string) ([]Queue, error) {
	return r.list(ctx, r.db.WithContext(ctx).Where("tenant_id = ?", tenantID))
}

// ListActive returns active queues of the tenant, position order (end-user view input).
func (r *Repository) ListActive(ctx context.Context, tenantID string) ([]Queue, error) {
	return r.list(ctx, r.db.WithContext(ctx).Where("tenant_id = ? AND is_active = 1", tenantID))
}

func (r *Repository) list(_ context.Context, tx *gorm.DB) ([]Queue, error) {
	var rows []Queue
	if err := tx.Order("position ASC, name ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("workqueue.list: %w", err)
	}
	for i := range rows {
		decode(&rows[i])
	}
	return rows, nil
}

// isDuplicate reports a MySQL 1062 duplicate-key error.
func isDuplicate(err error) bool {
	return err != nil && (errors.Is(err, gorm.ErrDuplicatedKey) ||
		strings.Contains(err.Error(), "Duplicate entry"))
}
