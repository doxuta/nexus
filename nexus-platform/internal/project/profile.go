// Package project holds the project registry's storage (specs/062-customer-workspace L5): one
// profile row per customer workspace (industry, origin, trial mark, last health snapshot) and the
// fixed-count signal queries behind GET /admin/projects.
//
// The registry is operator-side: it is read and written by a Nexus builder from the workshop's home
// tenant ABOUT other tenants. Every query is therefore keyed by the customer's tenant_id passed in
// explicitly (never a tenant from request context) and uses bound parameters only.
package project

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// Origin values of Profile.Origin: how the project was created.
const (
	OriginPack  = "pack"
	OriginBlank = "blank"
	OriginAI    = "ai"
)

// Health statuses of Profile.HealthStatus. Absent (NULL) reads as "unknown" in the service.
const (
	HealthOK       = "ok"
	HealthWarn     = "warn"
	HealthCritical = "critical"
)

// Profile is one sys_project_profiles row. There is deliberately NO delivered_at: "delivered" is
// the first numbered publish of the draft/publish tier (metaversion.DeliveryRepo), read at list
// time, so it cannot drift from the thing it describes.
type Profile struct {
	ID              string          `gorm:"column:id;type:char(36);primaryKey"`
	TenantID        string          `gorm:"column:tenant_id;type:char(36);not null"`
	Industry        *string         `gorm:"column:industry"`
	Origin          *string         `gorm:"column:origin"`
	OriginPack      *string         `gorm:"column:origin_pack"`
	TrialSince      *time.Time      `gorm:"column:trial_since"`
	HealthStatus    *string         `gorm:"column:health_status"`
	HealthJSON      json.RawMessage `gorm:"column:health_json;type:json"`
	HealthCheckedAt *time.Time      `gorm:"column:health_checked_at"`
	CreatedBy       string          `gorm:"column:created_by;type:char(36)"`
	UpdatedBy       string          `gorm:"column:updated_by;type:char(36)"`
	CreatedAt       time.Time       `gorm:"column:created_at"`
	UpdatedAt       time.Time       `gorm:"column:updated_at"`
	DeletedAt       gorm.DeletedAt  `gorm:"column:deleted_at"`
}

// TableName pins the fixed system table.
func (Profile) TableName() string { return "sys_project_profiles" }

// Counts are the per-tenant signals the S5 build steps read (FR-027a). All counts exclude
// soft-deleted rows of tables that have deleted_at.
type Counts struct {
	Apps          int `json:"apps"`
	Entities      int `json:"entities"`
	CustomLayouts int `json:"customLayouts"`
	Workflows     int `json:"workflows"`
	Approvals     int `json:"approvals"`
	Roles         int `json:"roles"`
	Users         int `json:"users"`      // active, not deleted
	StaffUsers    int `json:"staffUsers"` // active, not deleted, NOT admin
	Reports       int `json:"reports"`
	Dashboards    int `json:"dashboards"`
}

// Signals is everything the list needs per tenant beyond the profile row.
type Signals struct {
	Backup       BackupSignal
	Counts       Counts
	Branded      bool
	LastActiveAt *time.Time
}

// BackupSignal is the registry's view of a customer's backups (specs/068 T100): when the last backup became ready,
// and whether the most recent finished attempt failed.
type BackupSignal struct {
	LastReadyAt *time.Time
	LastFailed  bool
}
