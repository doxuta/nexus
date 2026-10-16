// TIMER-1 (engine-depth primitive #1b — ../specs/023-workflow-timer-calendar/spec.md §2 "parked row +
// wake-up"): the date_offset trigger's config types and PURE fire-time math. A date_offset rule
// fires at a computed future instant — "X minutes/hours/days/business-hours BEFORE/AFTER the
// record's date field F" — instead of at the mutation itself. Scheduling (enqueue a
// 'workflow_timer' sys_jobs wake-up row) and firing (the job handler) live in the services layer;
// this file owns only the shared vocabulary and the deterministic ComputeFireAt so the
// schedule-time producer and any future consumer can never drift on what an offset means.
package workflow

import (
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/calendar"
)

// Timer offset units. minutes/hours/days are WALL-CLOCK ("days" = amount × 24h, deliberately
// calendar-blind — a "3 days before expiry" reminder means 72 physical hours, DST shifts included);
// business_hours consumes working time through a CALENDAR-1 schedule (AddBusinessDuration), falling
// back to wall-clock hours when the tenant has no calendar (the documented CALENDAR-1 contract).
const (
	TimerUnitMinutes       = "minutes"
	TimerUnitHours         = "hours"
	TimerUnitDays          = "days"
	TimerUnitBusinessHours = "business_hours"
)

// Timer directions relative to the anchor date. V1 restriction: business_hours supports "after"
// only — CALENDAR-1 has no backward business-time walk (AddBusinessDuration is forward-only), and
// the flagship uses split cleanly: SLA deadlines = business-hours AFTER, reminders = wall-clock
// days BEFORE. Config validation (services.buildWorkflowRule) rejects the combination.
const (
	TimerDirectionBefore = "before"
	TimerDirectionAfter  = "after"
)

// maxTimerAmount bounds the offset amount so a typo can never produce a fire time decades out
// (calendar.AddBusinessDuration already caps its walk at ~10 years; this keeps wall-clock units
// in the same ballpark: 100000 hours ≈ 11.4 years).
const maxTimerAmount = 100000

// TimerOffset is the {amount, unit} pair of a date_offset rule.
type TimerOffset struct {
	Amount int    `json:"amount"`
	Unit   string `json:"unit"`
}

// TimerConfig is the date_offset trigger's configuration, persisted INSIDE the existing
// sys_workflows.condition JSON under the "timer" key (next to "match"/"rules" — no new column):
//
//	{"match":"and","rules":[…],"timer":{"dateField":"expiry_date",
//	  "offset":{"amount":3,"unit":"days"},"direction":"before"}}
//
// DateField names a date/datetime field of the rule's entity, or a system stamp (created_at, or
// updated_at for "no change for N days" — see IsSystemTimerAnchor).
// CalendarID (optional, business_hours only) pins a specific sys_business_calendars row; absent →
// the tenant's default calendar; no default → wall-clock fallback.
type TimerConfig struct {
	DateField  string      `json:"dateField"`
	Offset     TimerOffset `json:"offset"`
	Direction  string      `json:"direction"`
	CalendarID string      `json:"calendarId,omitempty"`
	// Repeat turns a one-shot reminder into a recurring one (ACTIVITY-CORE #3b). Absent = fire
	// once, which is every rule written before this existed.
	Repeat *TimerRepeat `json:"repeat,omitempty"`
}

// System stamps accepted as a timer anchor besides date/datetime fields. updated_at moves on every
// write, so a wake-up parked for one stamp goes stale on the next edit (the fire-time "did the date
// move?" check skips it) while the edit parks a replacement N units after the NEW stamp — that is
// the inactivity timer: it fires only when nothing touched the record for the whole offset (#287).
const (
	TimerAnchorCreatedAt = "created_at"
	TimerAnchorUpdatedAt = "updated_at"
)

// IsSystemTimerAnchor reports whether f is a record stamp (read off the row, not obj data).
func IsSystemTimerAnchor(f string) bool {
	return f == TimerAnchorCreatedAt || f == TimerAnchorUpdatedAt
}

// TimerRepeat is a BOUNDED recurrence: fire again every Every×Unit, Count times in total
// (including the first). There is deliberately no "forever" and no `until` date in v1:
//
//   - unbounded recurrence on a durable queue is a runaway generator — one bad rule parks jobs
//     until someone notices, and "someone notices" is not a design;
//   - an `until` DATE needs timezone reasoning to be unambiguous, while a COUNT means the same
//     thing in every zone. The scope cut is stated rather than hidden; add `until` when a real
//     rule needs it, with the timezone question answered.
//
// The interval is plain CALENDAR time even when the initial offset used business_hours: "remind
// 2 business hours before the deadline, then every day for 3 days" is the sentence people write,
// and stretching business-hours math across repeats would make the fire instants unpredictable.
type TimerRepeat struct {
	Every int    `json:"every"`
	Unit  string `json:"unit"`  // days | weeks
	Count int    `json:"count"` // total fires, including the first
}

// Repeat units. Minutes/hours are deliberately absent: a recurring reminder measured in minutes is
// a polling loop wearing a workflow's clothes, and the queue is not the place for one.
const (
	RepeatUnitDays  = "days"
	RepeatUnitWeeks = "weeks"
)

// maxRepeatCount bounds a recurrence so a typo cannot park hundreds of jobs.
const maxRepeatCount = 60

// ValidTimerRepeat reports whether r is a usable recurrence, and why not when it is not.
func ValidTimerRepeat(r *TimerRepeat) error {
	if r == nil {
		return nil // one-shot: the pre-existing behaviour
	}
	if r.Every <= 0 || r.Every > 365 {
		return fmt.Errorf("repeat.every phải trong khoảng 1..365")
	}
	if r.Unit != RepeatUnitDays && r.Unit != RepeatUnitWeeks {
		return fmt.Errorf("repeat.unit phải là %q hoặc %q", RepeatUnitDays, RepeatUnitWeeks)
	}
	if r.Count < 2 || r.Count > maxRepeatCount {
		return fmt.Errorf("repeat.count phải trong khoảng 2..%d (1 lần thì bỏ hẳn repeat)", maxRepeatCount)
	}
	return nil
}

// RepeatOffsetFor returns how far occurrence n (0-based) sits AFTER the first fire instant.
// Occurrence 0 is the original fire, so it adds nothing — which is what keeps a recurring rule's
// first fire identical to the one-shot it upgraded from.
func RepeatOffsetFor(r *TimerRepeat, n int) time.Duration {
	if r == nil || n <= 0 {
		return 0
	}
	days := r.Every * n
	if r.Unit == RepeatUnitWeeks {
		days *= 7
	}
	return time.Duration(days) * 24 * time.Hour
}

// ValidTimerUnit reports whether u is a supported offset unit.
func ValidTimerUnit(u string) bool {
	switch u {
	case TimerUnitMinutes, TimerUnitHours, TimerUnitDays, TimerUnitBusinessHours:
		return true
	default:
		return false
	}
}

// ValidTimerDirection reports whether d is a supported direction.
func ValidTimerDirection(d string) bool {
	return d == TimerDirectionBefore || d == TimerDirectionAfter
}

// ValidTimerAmount reports whether n is a usable offset amount (positive, bounded).
func ValidTimerAmount(n int) bool { return n > 0 && n <= maxTimerAmount }

// timerDateCanonicalLayout renders a time.Time anchor (created_at, TIMESTAMP(3)) to the canonical
// dateValue string carried in the workflow_timer job payload. Fixed-width milliseconds (NOT
// RFC3339Nano, which trims trailing zeros) so the same DB value always canonicalizes identically —
// the fire-time "did the date move?" comparison is a plain string equality.
const timerDateCanonicalLayout = "2006-01-02T15:04:05.000Z07:00"

// mysqlSpaceDatetimeLayout is the persisted datetime shape: the MySQL DATETIME literal in UTC that
// record_service.normalizeForStore (canonicalDatetimeUTC → mysqlDatetimeLayout) writes for EVERY
// datetime field — space separator, no 'T'/'Z'/offset. ParseTimerDate must accept it or no
// datetime-anchored date_offset timer ever schedules or fires: both the producer (record read-back
// after the action loop; evt.Data only on the nil-reader fallback) and the fire-time handler (record
// re-read) get this exact persisted form, and the parse would fail on it.
const mysqlSpaceDatetimeLayout = "2006-01-02 15:04:05"

// ParseTimerDate resolves a record's date-field value to (anchor instant, canonical string).
// Accepted shapes mirror what the record validator persists: a date field stores "YYYY-MM-DD"
// (anchored at 00:00 UTC — documented v1 semantics; a calendar-local midnight would need the
// schedule resolved before parsing), a datetime field stores the MySQL space-form
// "2006-01-02 15:04:05" in UTC (normalizeForStore/canonicalDatetimeUTC; RFC3339 is also accepted
// for any caller that still passes it), and the created_at system column arrives as a time.Time.
// ok=false means "no usable date" (absent, null, empty, wrong type, malformed) — the caller skips
// scheduling/firing rather than erroring, because an unset optional date is a normal record state,
// not a fault.
func ParseTimerDate(v any) (anchor time.Time, canonical string, ok bool) {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return time.Time{}, "", false
		}
		return t, t.UTC().Format(timerDateCanonicalLayout), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}, "", false
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, s, true
		}
		// MySQL space-form ("2006-01-02 15:04:05") is the PERSISTED datetime shape. It has no
		// 'T'/'Z', so RFC3339 above rejects it; the layout carries no zone, so time.Parse anchors
		// it at UTC — matching the UTC instant the engine stored. Canonical is the raw string:
		// schedule-time (record read-back after the action loop) and fire-time (record re-read)
		// read this identical persisted string, so the fire-time "did the date move?" equality
		// (a string compare) holds.
		if ts, err := time.Parse(mysqlSpaceDatetimeLayout, s); err == nil {
			return ts, s, true
		}
		if d, err := time.Parse("2006-01-02", s); err == nil {
			return d, s, true // date-only: 00:00 UTC anchor
		}
		return time.Time{}, "", false
	default:
		return time.Time{}, "", false
	}
}

// ComputeFireAt turns (anchor date, offset, direction) into the wake-up instant, truncated to the
// second (sys_jobs.run_at is TIMESTAMP — a deterministic sub-second-free value keeps the enqueue
// dedup and the fire-time comparison exact). sched is the CALENDAR-1 schedule for business_hours;
// nil sched = wall-clock fallback (no calendar for the tenant — the documented contract). A fire
// time in the past is returned as-is: the queue runs it immediately (a late reminder is still a
// reminder — Salesforce time-based workflow semantics).
func ComputeFireAt(cfg TimerConfig, anchor time.Time, sched *calendar.Schedule) (time.Time, error) {
	if !ValidTimerAmount(cfg.Offset.Amount) {
		return time.Time{}, fmt.Errorf("workflow.ComputeFireAt: invalid offset amount %d", cfg.Offset.Amount)
	}
	if !ValidTimerDirection(cfg.Direction) {
		return time.Time{}, fmt.Errorf("workflow.ComputeFireAt: invalid direction %q", cfg.Direction)
	}
	amount := time.Duration(cfg.Offset.Amount)

	var fireAt time.Time
	switch cfg.Offset.Unit {
	case TimerUnitMinutes:
		fireAt = shift(anchor, amount*time.Minute, cfg.Direction)
	case TimerUnitHours:
		fireAt = shift(anchor, amount*time.Hour, cfg.Direction)
	case TimerUnitDays:
		fireAt = shift(anchor, amount*24*time.Hour, cfg.Direction)
	case TimerUnitBusinessHours:
		if cfg.Direction != TimerDirectionAfter {
			// Config validation rejects business_hours+before; this is the run-time backstop.
			return time.Time{}, fmt.Errorf("workflow.ComputeFireAt: business_hours supports direction=after only")
		}
		if sched == nil {
			fireAt = anchor.Add(amount * time.Hour) // wall-clock fallback (no calendar)
		} else {
			t, err := sched.AddBusinessDuration(anchor, amount*time.Hour)
			if err != nil {
				return time.Time{}, fmt.Errorf("workflow.ComputeFireAt: %w", err)
			}
			fireAt = t
		}
	default:
		return time.Time{}, fmt.Errorf("workflow.ComputeFireAt: invalid unit %q", cfg.Offset.Unit)
	}
	return fireAt.UTC().Truncate(time.Second), nil
}

// shift moves the anchor by d in the given direction (before = into the past).
func shift(anchor time.Time, d time.Duration, direction string) time.Time {
	if direction == TimerDirectionBefore {
		return anchor.Add(-d)
	}
	return anchor.Add(d)
}
