// Package calendar is the PURE business-hours math engine (CALENDAR-1, engine-depth
// primitive #1a — ../specs/023-workflow-timer-calendar/spec.md §1). No DB, no HTTP: it parses a
// calendar definition (per-weekday working intervals + holidays + IANA timezone) into an
// in-memory Schedule and answers the three questions an SLA/timer engine needs:
//
//   - AddBusinessDuration:     start + N business-hours → deadline instant
//   - BusinessDurationBetween: how much business time elapsed in [from, to)
//   - IsWorkingTime:           is this instant inside a working interval
//
// Semantics (deliberate, documented — review anchor):
//
//   - Intervals are defined in LOCAL wall-clock of the calendar's timezone ("HH:MM", end
//     exclusive, "24:00" allowed as end-of-day so a 24/7 calendar has no midnight gap).
//   - The duration consumed/summed is PHYSICAL elapsed time inside those windows. All
//     arithmetic happens on time.Time values in the loaded *time.Location — never fixed
//     offsets — so DST is handled by construction: on a spring-forward day a window
//     spanning the gap contributes one hour less physical time, on fall-back one more
//     (a window 08:00–17:00 never spans the 02:00–03:00 transition, so ordinary
//     calendars see plain local wall-clock behaviour).
//   - Nonexistent local times (inside a spring-forward gap) are snapped to the DST transition
//     instant (resolveWall) — NOT left to Go's time.Date normalization, whose direction is
//     unspecified (BACKWARD for NY/Santiago, FORWARD for Beirut). This matters for windows
//     touching a gap AND, critically, for the day-step itself in zones that spring forward at
//     midnight (Chile/Cuba/Azores), where a naive next-midnight would normalize BACKWARD and stall
//     the walk. Ambiguous fall-back times take the first occurrence.
//   - Holidays are whole LOCAL dates (YYYY-MM-DD in the calendar's timezone).
//   - A schedule with zero working minutes per week — or a forward walk that finds no
//     working time within maxWalkDays (holiday wall) — returns ErrNoWorkingTime instead
//     of hanging.
package calendar

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Sentinel errors. The service layer maps them to CALENDAR_* API codes
// (ErrTimezoneInvalid → CALENDAR_TIMEZONE_INVALID, ErrHoursInvalid → CALENDAR_HOURS_INVALID,
// ErrHolidaysInvalid → CALENDAR_HOLIDAYS_INVALID).
var (
	ErrTimezoneInvalid = errors.New("calendar: invalid timezone")
	ErrHoursInvalid    = errors.New("calendar: invalid hours")
	ErrHolidaysInvalid = errors.New("calendar: invalid holidays")
	// ErrNoWorkingTime is returned by AddBusinessDuration when the schedule has no working
	// minutes at all, or the forward walk exhausted maxWalkDays without consuming the
	// duration (e.g. a wall of holidays) — the anti-hang guard.
	ErrNoWorkingTime = errors.New("calendar: no working time reachable")
	// ErrInvalidDuration is returned by AddBusinessDuration for a negative duration.
	ErrInvalidDuration = errors.New("calendar: invalid duration")
)

// maxWalkDays caps AddBusinessDuration's forward day-walk (~10 years). A duration that
// needs more calendar days than this returns ErrNoWorkingTime rather than looping —
// SLA/reminder deadlines live in hours-to-months, never decades.
const maxWalkDays = 3660

// Interval is one working window inside a day, LOCAL "HH:MM" 24h. Start is inclusive,
// End exclusive; End may be "24:00" (end of day). This is the exact hours_json wire/storage
// shape: {"start":"08:00","end":"12:00"}.
type Interval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Config is the parsed calendar definition. Hours keys are lowercase weekday names
// (mon..sun); a key absent or mapped to an empty slice is a non-working day. Holidays are
// "YYYY-MM-DD" local dates.
type Config struct {
	Timezone string
	Hours    map[string][]Interval
	Holidays []string
}

// weekdayIndex maps the hours_json weekday keys to time.Weekday (Sunday = 0).
var weekdayIndex = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// minuteSpan is a validated interval in minutes since local midnight (end may be 1440).
type minuteSpan struct{ start, end int }

// Schedule is the immutable, validated in-memory form of a calendar. Safe for concurrent
// reads after New returns.
type Schedule struct {
	loc           *time.Location
	week          [7][]minuteSpan // indexed by time.Weekday, spans sorted by start
	holidays      map[string]struct{}
	weeklyMinutes int
}

// New validates cfg and builds a Schedule. Validation IS the config-time validator the
// admin API uses (one body for validate-at-save and parse-at-run — validator parity by
// construction). Errors wrap the package sentinels.
func New(cfg Config) (*Schedule, error) {
	if cfg.Timezone == "" || cfg.Timezone == "Local" {
		// "Local" would make deadlines depend on the server machine's tz — reject.
		return nil, fmt.Errorf("calendar.New: timezone %q: %w", cfg.Timezone, ErrTimezoneInvalid)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("calendar.New: timezone %q: %w", cfg.Timezone, ErrTimezoneInvalid)
	}

	s := &Schedule{loc: loc, holidays: make(map[string]struct{}, len(cfg.Holidays))}
	for key, intervals := range cfg.Hours {
		wd, ok := weekdayIndex[key]
		if !ok {
			return nil, fmt.Errorf("calendar.New: unknown weekday key %q (want mon..sun): %w", key, ErrHoursInvalid)
		}
		spans := make([]minuteSpan, 0, len(intervals))
		for i, iv := range intervals {
			start, err := parseHHMM(iv.Start, false)
			if err != nil {
				return nil, fmt.Errorf("calendar.New: %s[%d].start: %w", key, i, err)
			}
			end, err := parseHHMM(iv.End, true)
			if err != nil {
				return nil, fmt.Errorf("calendar.New: %s[%d].end: %w", key, i, err)
			}
			if start >= end {
				return nil, fmt.Errorf("calendar.New: %s[%d]: start %q must be before end %q: %w", key, i, iv.Start, iv.End, ErrHoursInvalid)
			}
			spans = append(spans, minuteSpan{start: start, end: end})
		}
		sort.Slice(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
		for i := 1; i < len(spans); i++ {
			if spans[i].start < spans[i-1].end {
				return nil, fmt.Errorf("calendar.New: %s: intervals overlap: %w", key, ErrHoursInvalid)
			}
		}
		s.week[wd] = spans
		for _, sp := range spans {
			s.weeklyMinutes += sp.end - sp.start
		}
	}
	for _, h := range cfg.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return nil, fmt.Errorf("calendar.New: holiday %q (want YYYY-MM-DD): %w", h, ErrHolidaysInvalid)
		}
		s.holidays[h] = struct{}{}
	}
	return s, nil
}

// FromJSON builds a Schedule straight from the stored sys_business_calendars columns
// (hours_json, holidays_json). This is the ONE parse body shared by the admin validator
// (which marshals the request into these bytes) and the run-time consumers (SLA engine
// reading rows) — a stored calendar can therefore never fail to parse at run time.
// holidaysJSON may be nil/empty/"null" (no holidays).
func FromJSON(timezone string, hoursJSON, holidaysJSON []byte) (*Schedule, error) {
	var hours map[string][]Interval
	if err := json.Unmarshal(hoursJSON, &hours); err != nil {
		return nil, fmt.Errorf("calendar.FromJSON: hours: %w", ErrHoursInvalid)
	}
	var holidays []string
	if len(holidaysJSON) > 0 {
		if err := json.Unmarshal(holidaysJSON, &holidays); err != nil {
			return nil, fmt.Errorf("calendar.FromJSON: holidays: %w", ErrHolidaysInvalid)
		}
	}
	return New(Config{Timezone: timezone, Hours: hours, Holidays: holidays})
}

// parseHHMM parses strict zero-padded "HH:MM" into minutes since midnight.
// allowEndOfDay permits "24:00" (= 1440) for interval ends.
func parseHHMM(s string, allowEndOfDay bool) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("time %q (want HH:MM): %w", s, ErrHoursInvalid)
	}
	hh, mm := 0, 0
	for _, c := range []int{0, 1, 3, 4} {
		if s[c] < '0' || s[c] > '9' {
			return 0, fmt.Errorf("time %q (want HH:MM): %w", s, ErrHoursInvalid)
		}
	}
	hh = int(s[0]-'0')*10 + int(s[1]-'0')
	mm = int(s[3]-'0')*10 + int(s[4]-'0')
	if mm > 59 {
		return 0, fmt.Errorf("time %q: minutes out of range: %w", s, ErrHoursInvalid)
	}
	switch {
	case hh < 24:
		return hh*60 + mm, nil
	case hh == 24 && mm == 0 && allowEndOfDay:
		return 24 * 60, nil
	default:
		return 0, fmt.Errorf("time %q: hours out of range: %w", s, ErrHoursInvalid)
	}
}

// HasWorkingHours reports whether the schedule has any working minutes in its week.
// The admin service rejects an all-empty calendar at config time (CALENDAR_HOURS_EMPTY);
// this is also the fast path of the AddBusinessDuration anti-hang guard.
func (s *Schedule) HasWorkingHours() bool { return s.weeklyMinutes > 0 }

// WeeklyBusinessDuration is the working time one full week of the schedule holds (holidays not
// subtracted). The workflow config validator quotes it when it refuses a business_hours offset
// this calendar cannot reach within maxWalkDays (DAILY-QUEUE #80): "45 giờ mỗi tuần" is what lets
// an admin fix the rule without knowing the walk exists.
func (s *Schedule) WeeklyBusinessDuration() time.Duration {
	return time.Duration(s.weeklyMinutes) * time.Minute
}

// Location returns the calendar's loaded timezone (for callers that render local times).
func (s *Schedule) Location() *time.Location { return s.loc }

// isHoliday reports whether the local date (y, m, d) is listed as a holiday.
func (s *Schedule) isHoliday(y int, m time.Month, d int) bool {
	if len(s.holidays) == 0 {
		return false
	}
	_, ok := s.holidays[fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)]
	return ok
}

// resolveWall materializes `minutes`-from-local-midnight on date (y, m, d) as an absolute instant,
// gap-safe. minutes may be 0..1440 (1440 = "24:00" = the start of the next local day). A minute
// inside a spring-forward DST gap has NO local wall-clock representation; time.Date then normalizes
// it to one of the surrounding offsets — and Go does NOT guarantee the direction (BACKWARD for
// NY/Santiago/Havana/Azores, FORWARD for Beirut per the tz rules). Either way the intended wall
// time did not exist, so the correct instant is the DST TRANSITION itself (the first real instant
// at/after the requested wall time), obtained from ZoneBounds. Detection: if the materialized
// instant's own wall-clock reading differs from what we asked for, it was normalized off the gap.
// This keeps every day-step and span-bound strictly monotonic across midnight-transition zones, so
// the walks can never stall (the BLOCKER a naive time.Date(...) caused).
func (s *Schedule) resolveWall(y int, m time.Month, d, minutes int) time.Time {
	dayOffset := minutes / 1440
	mm := minutes % 1440
	t := time.Date(y, m, d+dayOffset, 0, mm, 0, 0, s.loc)
	if t.Hour()*60+t.Minute() == mm {
		return t // the wall time exists as requested (incl. fall-back: the first occurrence)
	}
	_, transition := t.ZoneBounds() // t sits in a DST gap → snap to the transition instant
	return transition
}

// spanBounds materializes a minute-span on the local date (y, m, d) as two absolute, gap-safe
// instants (see resolveWall). A span that lies entirely inside a spring-forward gap collapses to
// zero width (start == end) and is skipped by the callers.
func (s *Schedule) spanBounds(y int, m time.Month, d int, sp minuteSpan) (time.Time, time.Time) {
	return s.resolveWall(y, m, d, sp.start), s.resolveWall(y, m, d, sp.end)
}

// AddBusinessDuration walks forward from start through the schedule's working intervals,
// skipping non-working time (nights, non-working weekdays, holidays), consuming d of
// business time, and returns the instant at which d is exhausted.
//
//   - start mid-interval: that interval's remainder is consumed first.
//   - start outside working time: the walk advances to the next interval start.
//   - d == 0: start is returned unchanged (zero business hours from now = now).
//   - d < 0: ErrInvalidDuration.
//   - No working time (empty schedule, or a holiday wall longer than maxWalkDays):
//     ErrNoWorkingTime — never a hang.
//
// The result preserves the caller's perspective as an absolute instant (computed in the
// calendar's timezone; convert with .In() as needed).
func (s *Schedule) AddBusinessDuration(start time.Time, d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, fmt.Errorf("calendar.AddBusinessDuration: negative duration %v: %w", d, ErrInvalidDuration)
	}
	if d == 0 {
		return start, nil
	}
	if s.weeklyMinutes == 0 {
		return time.Time{}, fmt.Errorf("calendar.AddBusinessDuration: schedule has no working hours: %w", ErrNoWorkingTime)
	}
	remaining := d
	cursor := start.In(s.loc)
	for day := 0; day < maxWalkDays; day++ {
		y, m, dd := cursor.Date()
		if !s.isHoliday(y, m, dd) {
			for _, sp := range s.week[cursor.Weekday()] {
				spanStart, spanEnd := s.spanBounds(y, m, dd, sp)
				if !spanEnd.After(spanStart) {
					continue // DST-degenerate span (entirely inside a spring-forward gap)
				}
				from := cursor
				if from.Before(spanStart) {
					from = spanStart
				}
				if !from.Before(spanEnd) {
					continue // interval already passed
				}
				avail := spanEnd.Sub(from)
				if avail >= remaining {
					return from.Add(remaining), nil
				}
				remaining -= avail
				cursor = spanEnd
			}
		}
		cursor = s.resolveWall(y, m, dd+1, 0) // start of the next local day (gap-safe)
	}
	return time.Time{}, fmt.Errorf("calendar.AddBusinessDuration: no working time within %d days: %w", maxWalkDays, ErrNoWorkingTime)
}

// BusinessDurationBetween sums the working time inside [from, to). Returns 0 when to <= from.
// The day-step is gap-safe (resolveWall) so the cursor is strictly monotonic; a defensive
// non-advance guard + a hard iteration cap make the loop terminate unconditionally regardless of
// tzdata surprises (this path has no maxWalkDays and would otherwise be a config-triggered hang
// once a caller wires it into an HTTP/SLA path).
func (s *Schedule) BusinessDurationBetween(from, to time.Time) time.Duration {
	fromLoc, toLoc := from.In(s.loc), to.In(s.loc)
	if !toLoc.After(fromLoc) {
		return 0
	}
	var total time.Duration
	cursor := fromLoc
	for day := 0; cursor.Before(toLoc) && day < maxWalkDays; day++ {
		y, m, dd := cursor.Date()
		if !s.isHoliday(y, m, dd) {
			for _, sp := range s.week[cursor.Weekday()] {
				spanStart, spanEnd := s.spanBounds(y, m, dd, sp)
				lo := spanStart
				if cursor.After(lo) {
					lo = cursor
				}
				hi := spanEnd
				if toLoc.Before(hi) {
					hi = toLoc
				}
				if hi.After(lo) {
					total += hi.Sub(lo)
				}
			}
		}
		next := s.resolveWall(y, m, dd+1, 0) // start of the next local day (gap-safe)
		if !next.After(cursor) {
			break // defensive: never allow a non-advancing cursor to spin
		}
		cursor = next
	}
	return total
}

// IsWorkingTime reports whether t falls inside a working interval on a non-holiday day
// (interval start inclusive, end exclusive), evaluated in the calendar's timezone
// regardless of t's own location.
func (s *Schedule) IsWorkingTime(t time.Time) bool {
	lt := t.In(s.loc)
	y, m, dd := lt.Date()
	if s.isHoliday(y, m, dd) {
		return false
	}
	for _, sp := range s.week[lt.Weekday()] {
		spanStart, spanEnd := s.spanBounds(y, m, dd, sp)
		if !lt.Before(spanStart) && lt.Before(spanEnd) {
			return true
		}
	}
	return false
}
