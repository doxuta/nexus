// Package inboundhook persists the CONG-TIN-HIEU inbound webhook registry + delivery log
// (../specs/015-inbound-hooks/spec.md). Fixed system tables → GORM (rules/database.md). Every method is
// tenant-scoped; the hot receive path looks a hook up by (tenant slug is resolved by the
// service — this repo only ever sees tenant IDs).
package inboundhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SystemActorID is the synthetic user id a hook runs under when its creator is not a user of the hook's
// own tenant (a Nexus builder working inside a customer's workspace, a pack install into it):
// sys_inbound_hooks.created_by carries it, so every record the hook later writes is attributed to
// "Webhook vào" and not to a builder's name (specs/062-customer-workspace D-22, FR-023a). Same family
// as workflow.SystemActorID (f1…) and tenantbackup.SystemActorID (f3…); the FE labels it in
// lib/auditLabels.ts ENGINE_ACTORS.
const SystemActorID = "f2000000-0000-0000-0000-000000000000"

// ErrNotFound signals an unknown/deleted hook within the tenant.
var ErrNotFound = errors.New("inboundhook: not found")

// ErrDuplicateName signals a (tenant, name) uniqueness violation.
var ErrDuplicateName = errors.New("inboundhook: name already exists")

// Mapping is one payload→field extraction rule.
type Mapping struct {
	Source string `json:"source"` // dot-path into the JSON payload ("data.amount", "items.0.sku")
	Field  string `json:"field"`  // target entity field
}

// PaymentMatch is the optional bank-credit matching step (DONG-TIEN p2, ../specs/016-payment-flow/spec.md §8):
// after the normal upsert, pull the transfer memo from MemoSource, find the reference via
// RefPattern, look the target record up by its unique RefField and flip StatusField to
// StatusValue. AmountSource/AmountField (both or neither) additionally require the paid
// amount to equal the target's amount — a mismatch goes to the hộp chờ, never a blind match.
type PaymentMatch struct {
	TargetEntity string `json:"targetEntity"`
	RefField     string `json:"refField"`   // unique field on the target entity
	RefPattern   string `json:"refPattern"` // regex that finds the reference inside the memo
	MemoSource   string `json:"memoSource"` // dot-path to the transfer memo in the payload
	StatusField  string `json:"statusField"`
	StatusValue  string `json:"statusValue"`
	AmountSource string `json:"amountSource,omitempty"` // dot-path to the paid amount
	AmountField  string `json:"amountField,omitempty"`  // number/currency field on the target
}

// Hook is one sys_inbound_hooks row.
type Hook struct {
	ID              string          `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID        string          `gorm:"type:char(36);not null" json:"-"`
	Name            string          `gorm:"type:varchar(64);not null" json:"name"`
	EntityName      string          `gorm:"type:varchar(64);not null;column:entity_name" json:"entityName"`
	Secret          string          `gorm:"type:varchar(128);not null" json:"secret"`
	MappingRaw      json.RawMessage `gorm:"type:json;column:mapping" json:"-"`
	MatchField      *string         `gorm:"type:varchar(64);column:match_field" json:"matchField,omitempty"`
	PaymentMatchRaw json.RawMessage `gorm:"type:json;column:payment_match" json:"-"`
	IsActive        bool            `gorm:"column:is_active;not null;default:true" json:"isActive"`
	CreatedBy       string          `gorm:"type:char(36);not null;column:created_by" json:"-"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	DeletedAt       gorm.DeletedAt  `gorm:"index" json:"-"`

	// Mapping is the decoded MappingRaw (populated on read, serialized on write).
	Mapping []Mapping `gorm:"-" json:"mapping"`
	// PaymentMatch is the decoded PaymentMatchRaw; nil = no matching step.
	PaymentMatch *PaymentMatch `gorm:"-" json:"paymentMatch,omitempty"`
}

func (Hook) TableName() string { return "sys_inbound_hooks" }

// Delivery is one sys_hook_deliveries row (observability of the receive path).
type Delivery struct {
	ID          string  `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID    string  `gorm:"type:char(36);not null" json:"-"`
	HookID      string  `gorm:"type:char(36);not null;column:hook_id" json:"hookId"`
	Status      string  `gorm:"type:varchar(16);not null" json:"status"` // created|updated|error
	RecordID    *string `gorm:"type:char(36);column:record_id" json:"recordId,omitempty"`
	Error       *string `gorm:"type:varchar(500)" json:"error,omitempty"`
	PayloadHead *string `gorm:"type:varchar(2048);column:payload_head" json:"payloadHead,omitempty"`
	// MatchStatus/MatchRecordID (DONG-TIEN p2): matched | unmatched (+ the target record when
	// matched). NULL on hooks without a payment_match step. Unmatched rows ARE the hộp chờ.
	MatchStatus   *string   `gorm:"type:varchar(16);column:match_status" json:"matchStatus,omitempty"`
	MatchRecordID *string   `gorm:"type:char(36);column:match_record_id" json:"matchRecordId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (Delivery) TableName() string { return "sys_hook_deliveries" }

// Repository is the GORM-backed store.
type Repository struct{ db *gorm.DB }

func New(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) encode(h *Hook) error {
	if h.Mapping == nil {
		h.Mapping = []Mapping{}
	}
	b, err := json.Marshal(h.Mapping)
	if err != nil {
		return fmt.Errorf("inboundhook.encode: %w", err)
	}
	h.MappingRaw = b
	h.PaymentMatchRaw = nil
	if h.PaymentMatch != nil {
		pm, err := json.Marshal(h.PaymentMatch)
		if err != nil {
			return fmt.Errorf("inboundhook.encode: payment_match: %w", err)
		}
		h.PaymentMatchRaw = pm
	}
	return nil
}

func decode(h *Hook) {
	h.Mapping = []Mapping{}
	if len(h.MappingRaw) > 0 {
		_ = json.Unmarshal(h.MappingRaw, &h.Mapping) // malformed row → empty mapping (fail-safe)
	}
	h.PaymentMatch = nil
	if len(h.PaymentMatchRaw) > 0 && string(h.PaymentMatchRaw) != "null" {
		var pm PaymentMatch
		if json.Unmarshal(h.PaymentMatchRaw, &pm) == nil && pm.TargetEntity != "" {
			h.PaymentMatch = &pm
		}
	}
}

// Create inserts one hook.
func (r *Repository) Create(ctx context.Context, h *Hook) error {
	if err := r.encode(h); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(h).Error; err != nil {
		if isDuplicate(err) {
			return ErrDuplicateName
		}
		return fmt.Errorf("inboundhook.Create: %w", err)
	}
	return nil
}

// Update full-replaces a hook's config (never the secret — see UpdateSecret).
func (r *Repository) Update(ctx context.Context, tenantID string, h *Hook) error {
	if err := r.encode(h); err != nil {
		return err
	}
	res := r.db.WithContext(ctx).Model(&Hook{}).
		Where("id = ? AND tenant_id = ?", h.ID, tenantID).
		Updates(map[string]any{
			"name":          h.Name,
			"entity_name":   h.EntityName,
			"mapping":       string(h.MappingRaw),
			"match_field":   h.MatchField,
			"payment_match": rawOrNil(h.PaymentMatchRaw),
			"is_active":     h.IsActive,
		})
	if res.Error != nil {
		if isDuplicate(res.Error) {
			return ErrDuplicateName
		}
		return fmt.Errorf("inboundhook.Update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Same-value update also reports 0 — disambiguate with an existence count.
		var n int64
		if err := r.db.WithContext(ctx).Model(&Hook{}).
			Where("id = ? AND tenant_id = ?", h.ID, tenantID).Count(&n).Error; err != nil {
			return fmt.Errorf("inboundhook.Update: recheck: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// UpdateSecret rotates the shared secret.
func (r *Repository) UpdateSecret(ctx context.Context, tenantID, id, secret string) error {
	res := r.db.WithContext(ctx).Model(&Hook{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Update("secret", secret)
	if res.Error != nil {
		return fmt.Errorf("inboundhook.UpdateSecret: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDelete removes a hook from service. The name is tombstoned to "<name>~<id8>" in the
// same statement (review W3): uq_hook(tenant_id, name) ignores deleted_at, so a plain soft
// delete would ghost-block re-creating a hook under the same slug forever.
func (r *Repository) SoftDelete(ctx context.Context, tenantID, id string) error {
	suffix := id
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	res := r.db.WithContext(ctx).Model(&Hook{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Updates(map[string]any{
			"name":       gorm.Expr("CONCAT(LEFT(name, 55), '~', ?)", suffix),
			"deleted_at": gorm.Expr("NOW()"),
		})
	if res.Error != nil {
		return fmt.Errorf("inboundhook.SoftDelete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByID returns one hook of the tenant.
func (r *Repository) GetByID(ctx context.Context, tenantID, id string) (*Hook, error) {
	var h Hook
	err := r.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, tenantID).First(&h).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inboundhook.GetByID: %w", err)
	}
	decode(&h)
	return &h, nil
}

// GetByName returns one hook by its URL slug (the hot receive path).
func (r *Repository) GetByName(ctx context.Context, tenantID, name string) (*Hook, error) {
	var h Hook
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenantID, name).First(&h).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inboundhook.GetByName: %w", err)
	}
	decode(&h)
	return &h, nil
}

// ListAll returns every hook of the tenant (admin screen), stable order.
func (r *Repository) ListAll(ctx context.Context, tenantID string) ([]Hook, error) {
	var rows []Hook
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("inboundhook.ListAll: %w", err)
	}
	for i := range rows {
		decode(&rows[i])
	}
	return rows, nil
}

// RecordDelivery appends one delivery-log row (fail-open at the call site).
func (r *Repository) RecordDelivery(ctx context.Context, d *Delivery) error {
	if err := r.db.WithContext(ctx).Create(d).Error; err != nil {
		return fmt.Errorf("inboundhook.RecordDelivery: %w", err)
	}
	return nil
}

// ListDeliveries returns the most recent deliveries of one hook.
func (r *Repository) ListDeliveries(ctx context.Context, tenantID, hookID string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []Delivery
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND hook_id = ?", tenantID, hookID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("inboundhook.ListDeliveries: %w", err)
	}
	return rows, nil
}

// rawOrNil stores empty raw JSON as SQL NULL (a "" string would be invalid JSON for MySQL).
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// isDuplicate reports a MySQL unique-key violation (mirrors workqueue's check).
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}
