// Package calmeta is the storage layer of the business-hours calendar primitive
// (CALENDAR-1, migration 000047). It persists sys_business_calendars rows: a per-tenant
// named calendar = IANA timezone + per-weekday working intervals (hours_json) + holiday
// dates (holidays_json). The MATH over these rows lives in internal/calendar (pure, no DB);
// this package only stores and retrieves the definition.
//
// Multi-tenant invariant: every repository query filters tenant_id.
package calmeta

import (
	"encoding/json"
	"fmt"
	"time"
)

// Usage names one live workflow that pins a calendar by id — through its date_offset timer
// (condition.timer.calendarId) or a set_deadline action (actions[].calendarId).
type Usage struct {
	WorkflowID   string `db:"workflow_id" json:"workflowId"`
	WorkflowName string `db:"workflow_name" json:"workflowName"`
}

// ErrCalendarNotFound is returned by Get/Update/SoftDelete for an unknown or cross-tenant id.
var ErrCalendarNotFound = fmt.Errorf("business calendar not found")

// ErrCalendarDuplicateName is returned by Create/Update when the (tenant_id, name) slot is
// taken (MySQL 1062 on uq_sys_business_calendars_tenant_name) — the service maps it to
// 409 CALENDAR_ALREADY_EXISTS.
var ErrCalendarDuplicateName = fmt.Errorf("business calendar name already exists")

// BusinessCalendar is a persisted sys_business_calendars row. Hours/Holidays are forwarded
// as raw JSON (validated by the service through calendar.New before every write); json tags
// give the camelCase wire shape.
type BusinessCalendar struct {
	ID        string          `db:"id" json:"id"`
	TenantID  string          `db:"tenant_id" json:"tenantId"`
	Name      string          `db:"name" json:"name"`
	Timezone  string          `db:"timezone" json:"timezone"`
	Hours     json.RawMessage `db:"hours_json" json:"hours"`
	Holidays  json.RawMessage `db:"holidays_json" json:"holidays"`
	IsDefault bool            `db:"is_default" json:"isDefault"`
	CreatedBy *string         `db:"created_by" json:"createdBy"`
	UpdatedBy *string         `db:"updated_by" json:"updatedBy"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}
