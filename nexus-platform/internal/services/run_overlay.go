package services

import (
	"strings"
	"time"

	"nexus/platform/internal/dynrepo"
)

// RunOverlay is the request-time, NON-PERSISTED filter overlay for dashboard runs, report runs
// and report CSV exports (W4 §3.10/§3.11 — the fix for the silent no-op filter drawer):
//
//   - DateFrom / DateTo narrow by the record's creation instant via the MANDATORY system column
//     `created_at` (present on every obj_* table per the database design rules, so the overlay
//     composes with EVERY entity — no widget/report can lack it).
//   - OwnerID narrows by creator via the mandatory system column `created_by` (exact match).
//
// The overlay is ANDed on top of each widget's / the report's saved filters; it never mutates
// the stored config. Absent params (nil overlay) leave behavior byte-identical to a plain run.
//
// Date semantics: calendar dates in the DB session time zone. DateFrom is inclusive from
// 00:00:00; DateTo is inclusive of the whole day (created_at < DateTo+1d 00:00:00 — avoids the
// 23:59:59-to-midnight sub-second gap).
type RunOverlay struct {
	DateFrom string // "YYYY-MM-DD" — created_at >= 'DATE 00:00:00'; empty = no lower bound
	DateTo   string // "YYYY-MM-DD" — created_at <  DATE+1d 00:00:00; empty = no upper bound
	OwnerID  string // created_by = ?; empty = no owner filter
}

// runOverlayDateLayout is the strict ISO calendar-date layout the overlay accepts. time.Parse
// with this layout rejects non-padded forms ("2026-1-1"), datetime strings and impossible
// dates ("2026-02-30").
const runOverlayDateLayout = "2006-01-02"

// ParseRunOverlay validates the raw ?dateFrom / ?dateTo / ?ownerId query values into a
// RunOverlay. All params are optional; all empty → (nil, nil) so callers keep the exact
// pre-W4 behavior. Errors (both 422 via statusForCode default):
//   - RUN_DATE_INVALID       — a date is not a valid YYYY-MM-DD calendar date.
//   - RUN_DATE_RANGE_INVALID — dateFrom > dateTo (an empty range is a client bug, not "0 rows").
//
// OwnerID is a free-form id matched exactly against created_by (parameterized — a value that
// matches no creator simply yields 0 rows; no format constraint is imposed).
func ParseRunOverlay(dateFrom, dateTo, ownerID string) (*RunOverlay, error) {
	dateFrom = strings.TrimSpace(dateFrom)
	dateTo = strings.TrimSpace(dateTo)
	ownerID = strings.TrimSpace(ownerID)
	if dateFrom == "" && dateTo == "" && ownerID == "" {
		return nil, nil
	}
	var from, to time.Time
	var err error
	if dateFrom != "" {
		if from, err = time.Parse(runOverlayDateLayout, dateFrom); err != nil {
			return nil, invalid("RUN_DATE_INVALID",
				"dateFrom %q không hợp lệ (định dạng YYYY-MM-DD)", dateFrom)
		}
	}
	if dateTo != "" {
		if to, err = time.Parse(runOverlayDateLayout, dateTo); err != nil {
			return nil, invalid("RUN_DATE_INVALID",
				"dateTo %q không hợp lệ (định dạng YYYY-MM-DD)", dateTo)
		}
	}
	if dateFrom != "" && dateTo != "" && from.After(to) {
		return nil, invalid("RUN_DATE_RANGE_INVALID",
			"dateFrom %q phải nhỏ hơn hoặc bằng dateTo %q", dateFrom, dateTo)
	}
	return &RunOverlay{DateFrom: dateFrom, DateTo: dateTo, OwnerID: ownerID}, nil
}

// overlayFilters translates the overlay into dynrepo predicates on the mandatory system
// columns created_at / created_by. These are PHYSICAL, always-indexed columns (idx_created_by
// / idx_created_at per the obj_* table contract), so the predicates are index-friendly and
// need no CAST. They are appended AFTER the saved/query filters — AND semantics, order
// irrelevant. System columns are not sys_fields entries, so they carry no field-ACL surface;
// the regular field-ACL checks on the saved filters/metric/groupBy still run unchanged.
//
// Construct the overlay via ParseRunOverlay (dates pre-validated). A hand-built overlay with
// an unparseable DateTo falls back to an inclusive `<= 'DATE 23:59:59'` bound rather than
// silently dropping the predicate.
func overlayFilters(o *RunOverlay) []dynrepo.Filter {
	if o == nil {
		return nil
	}
	var out []dynrepo.Filter
	if o.DateFrom != "" {
		out = append(out, dynrepo.Filter{
			Column:         "created_at",
			PhysicalColumn: "created_at",
			Operator:       dynrepo.FilterGte,
			Values:         []string{o.DateFrom + " 00:00:00"},
			Value:          o.DateFrom + " 00:00:00",
		})
	}
	if o.DateTo != "" {
		if next, err := time.Parse(runOverlayDateLayout, o.DateTo); err == nil {
			// Inclusive whole-day upper bound: created_at < dateTo+1d 00:00:00. AddDate
			// handles month/year boundaries (2026-01-31 → 2026-02-01).
			bound := next.AddDate(0, 0, 1).Format(runOverlayDateLayout) + " 00:00:00"
			out = append(out, dynrepo.Filter{
				Column:         "created_at",
				PhysicalColumn: "created_at",
				Operator:       dynrepo.FilterLt,
				Values:         []string{bound},
				Value:          bound,
			})
		} else {
			// Defensive fallback (never hit via ParseRunOverlay): second-granular inclusive bound.
			bound := o.DateTo + " 23:59:59"
			out = append(out, dynrepo.Filter{
				Column:         "created_at",
				PhysicalColumn: "created_at",
				Operator:       dynrepo.FilterLte,
				Values:         []string{bound},
				Value:          bound,
			})
		}
	}
	if o.OwnerID != "" {
		out = append(out, dynrepo.Filter{
			Column:         "created_by",
			PhysicalColumn: "created_by",
			Operator:       dynrepo.FilterEq,
			Values:         []string{o.OwnerID},
			Value:          o.OwnerID,
		})
	}
	return out
}
