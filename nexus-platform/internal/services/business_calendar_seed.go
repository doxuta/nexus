package services

// Default working calendar for a newly provisioned tenant (DAILY-QUEUE #10, Founder decision Q9
// 2026-09-05). Without a calendar every business_hours deadline silently degrades to wall-clock
// (the documented CALENDAR-1 fallback), so a ticket raised at 16:00 Friday with a 3-business-hour
// SLA would be due at 19:00 Friday — a commitment nobody is at work to keep. Seeding one makes the
// out-of-the-box answer Monday 10:00 instead, and the tenant can edit or replace it afterwards.
//
// The seed goes in at PROVISION time, never in a migration: migration 000047 states DDL-ONLY, and
// a seeding migration could not know which tenants are new.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nexus/platform/internal/calendar"
)

const (
	// DefaultCalendarName is deliberately NOT "Default": uq_sys_business_calendars_tenant_name is
	// (tenant_id, name) and existing fixtures already create a calendar called "Default" on their
	// tenants (internal/services/workflow_timer_integration_test.go), which would collide.
	DefaultCalendarName = "Giờ hành chính"
	// DefaultCalendarTimezone is where the business is, not where the server is.
	DefaultCalendarTimezone = "Asia/Ho_Chi_Minh"
	// DefaultCalendarHoursJSON is Mon-Fri 08:00-17:00, nine CONTINUOUS hours — v1 does not carve
	// out a lunch break (Q9), because a tenant that wants one edits the calendar, while a tenant
	// that does not would otherwise silently lose an hour a day off every SLA.
	//
	// The literal is the wire shape the CALENDAR-1 validator accepts and calendar.FromJSON parses;
	// TestDefaultCalendarHoursJSON_ParsesAndComputes runs the real parser over these exact bytes,
	// so the seed cannot drift from the shape the engine reads (validator parity by construction).
	DefaultCalendarHoursJSON = `{"mon":[{"start":"08:00","end":"17:00"}],` +
		`"tue":[{"start":"08:00","end":"17:00"}],` +
		`"wed":[{"start":"08:00","end":"17:00"}],` +
		`"thu":[{"start":"08:00","end":"17:00"}],` +
		`"fri":[{"start":"08:00","end":"17:00"}],` +
		`"sat":[],"sun":[]}`
)

// DefaultCalendarSchedule parses the seed literal into the schedule the engine would compute with.
// Exported so a test (and any caller that wants to show the default) reads the SAME bytes the
// INSERT below writes, instead of a hand-copied duplicate that can drift.
func DefaultCalendarSchedule() (*calendar.Schedule, error) {
	return calendar.FromJSON(DefaultCalendarTimezone, []byte(DefaultCalendarHoursJSON), nil)
}

// SeedDefaultBusinessCalendar inserts the tenant's default working calendar. It runs inside the
// caller's transaction (db may be a *gorm.DB tx) so a provision that fails later leaves no orphan
// calendar — which is why this is a parameterized tx.Exec rather than a call into
// calmeta.SQLRepository: that repository is sqlx and opens its OWN transaction, and mixing the two
// would break the zero-orphan guarantee TestProvision_DuplicateSlug pins.
//
// IDEMPOTENT, and deliberately "has this tenant EVER had a calendar", not "has one right now":
// the existence probe does NOT filter deleted_at. A tenant that deliberately deleted its only
// calendar chose the wall-clock fallback; re-seeding it on the next bootstrap run would silently
// move every SLA instant in that tenant — a change to customer commitments, with nothing on the
// tenant's side saying why. Only a tenant with no calendar row at all is seeded. That still makes
// bootstrap-admin the backfill path for tenants created before this seed existed (demo, northwind).
//
// ONE STATEMENT: probe and insert are a single INSERT … SELECT … WHERE NOT EXISTS. Strictly this
// is not atomic under REPEATABLE READ — what actually stops a double insert is the unique index
// uq_sys_business_calendars_tenant_name plus InnoDB's gap lock — but it closes the window that
// matters. Two separate statements race on the bootstrap path, which unlike Provision runs against
// a LIVE tenant with no surrounding transaction: an admin creating their own default calendar between the probe and the
// insert would leave two rows with is_default=1, and GetDefault has no ORDER BY, so which calendar
// applies to a deadline becomes non-deterministic. Migration 000047 states the invariant as
// "≤1 per tenant"; this is the one writer that does not go through the repository's clear-old
// UPDATE, so it has to hold the invariant itself.
//
// Going through Exec skips validateCalendarInput, so the bytes written here MUST be the shape that
// validator would have produced — see DefaultCalendarHoursJSON.
//
// Returns true when a calendar was actually inserted, so a caller can tell the operator (a
// bootstrap re-run that seeds a calendar is NOT "nothing changed").
func SeedDefaultBusinessCalendar(ctx context.Context, db *gorm.DB, tenantID string) (bool, error) {
	now := time.Now().UTC()
	const q = `INSERT INTO sys_business_calendars
		(id, tenant_id, name, timezone, hours_json, holidays_json, is_default,
		 created_by, updated_by, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, NULL, 1, NULL, NULL, ?, ?
		FROM DUAL
		WHERE NOT EXISTS (SELECT 1 FROM sys_business_calendars WHERE tenant_id = ?)`
	res := db.WithContext(ctx).Exec(q,
		uuid.NewString(), tenantID, DefaultCalendarName, DefaultCalendarTimezone,
		DefaultCalendarHoursJSON, now, now,
		tenantID,
	)
	if res.Error != nil {
		return false, fmt.Errorf("services.SeedDefaultBusinessCalendar: insert: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}
