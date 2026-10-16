// Wave 3.6 FILTER-RELATIVE-DATES (TOTAL-UPGRADE-PLAN; điều kiện cứng của "Việc Hôm Nay" —
// RESEARCH-DIFFERENTIATION §3.1 #10): server-resolved relative date tokens on date/datetime
// filters, and an END to the silent-empty-result trap: an unparseable date operand used to fall
// through to SQL CAST → NULL → 0 rows with no error; it is now 422 LIST_INVALID_DATE_FILTER.
//
// Tokens resolve against the APP timezone (env APP_TIMEZONE, default Asia/Ho_Chi_Minh — the
// market this engine serves; degrade-to-default on a bad value, never fail boot). A token
// becomes a [start, end] day range in local time:
//
//	eq  → between [start, end]         (the whole range)
//	gte → gte start                     lte → lte end
//	gt  → gt  end  ("after the range")  lt  → lt  start ("before the range")
//
// For date fields operands are calendar dates ("2026-07-21"); for datetime fields they are
// RFC3339 locals ("2026-07-21T00:00:00+07:00" / "...T23:59:59+07:00") which the existing
// canonicalizeFilterValues → canonicalDatetimeUTC pipeline converts to the stored UTC literal —
// one resolver, zero new storage semantics.
package services

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"nexus/platform/internal/dynrepo"
)

// relativeDateTokens is the fixed token set (FE preset picker mirrors this list; a fixed set
// beats a parameterized last_n_days:N — simpler to validate, localize and document).
var relativeDateTokens = map[string]struct{}{
	"today": {}, "yesterday": {}, "tomorrow": {},
	"this_week": {}, "last_week": {}, "next_week": {},
	"this_month": {}, "last_month": {},
	"this_quarter": {}, "this_year": {},
	"last_7_days": {}, "last_30_days": {}, "last_90_days": {},
	"next_7_days": {}, "next_30_days": {},
}

var (
	appLocOnce sync.Once
	appLoc     *time.Location
)

// appLocation resolves the application timezone once. Bad/unknown APP_TIMEZONE degrades to the
// default (consistent with every other env knob in this codebase — never fail boot over a typo).
func appLocation() *time.Location {
	appLocOnce.Do(func() {
		name := strings.TrimSpace(os.Getenv("APP_TIMEZONE"))
		if name == "" {
			name = "Asia/Ho_Chi_Minh"
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			loc, err = time.LoadLocation("Asia/Ho_Chi_Minh")
			if err != nil {
				loc = time.FixedZone("ICT", 7*3600) // tzdata missing entirely — VN offset, no DST
			}
		}
		appLoc = loc
	})
	return appLoc
}

// tokenRange resolves one token to an inclusive [start, end] LOCAL day pair. end is the LAST day
// of the range (inclusive) — the operand builders below expand it to 23:59:59 for datetimes.
// Weeks are Monday-first (T2 — VN convention, mirrors the calendar widget).
func tokenRange(token string, now time.Time) (start, end time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch token {
	case "today":
		return day, day
	case "yesterday":
		d := day.AddDate(0, 0, -1)
		return d, d
	case "tomorrow":
		d := day.AddDate(0, 0, 1)
		return d, d
	case "this_week":
		monday := day.AddDate(0, 0, -mondayOffset(day))
		return monday, monday.AddDate(0, 0, 6)
	case "last_week":
		monday := day.AddDate(0, 0, -mondayOffset(day)-7)
		return monday, monday.AddDate(0, 0, 6)
	case "next_week":
		monday := day.AddDate(0, 0, -mondayOffset(day)+7)
		return monday, monday.AddDate(0, 0, 6)
	case "this_month":
		first := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
		return first, first.AddDate(0, 1, -1)
	case "last_month":
		first := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location()).AddDate(0, -1, 0)
		return first, first.AddDate(0, 1, -1)
	case "this_quarter":
		qm := time.Month((int(day.Month())-1)/3*3 + 1)
		first := time.Date(day.Year(), qm, 1, 0, 0, 0, 0, day.Location())
		return first, first.AddDate(0, 3, -1)
	case "this_year":
		first := time.Date(day.Year(), 1, 1, 0, 0, 0, 0, day.Location())
		return first, time.Date(day.Year(), 12, 31, 0, 0, 0, 0, day.Location())
	case "last_7_days":
		return day.AddDate(0, 0, -6), day
	case "last_30_days":
		return day.AddDate(0, 0, -29), day
	case "last_90_days":
		return day.AddDate(0, 0, -89), day
	case "next_7_days":
		return day, day.AddDate(0, 0, 6)
	case "next_30_days":
		return day, day.AddDate(0, 0, 29)
	}
	return day, day // unreachable — callers gate on relativeDateTokens membership
}

// mondayOffset returns how many days back Monday is from d (Mon=0 … Sun=6).
func mondayOffset(d time.Time) int { return (int(d.Weekday()) + 6) % 7 }

// dateOperand renders a range boundary as a filter operand for the field type. Date fields use
// the calendar date; datetime fields use an RFC3339 local (endOfDay=23:59:59) that the existing
// canonicalize pipeline converts to the stored UTC literal.
func dateOperand(fieldType string, t time.Time, endOfDay bool) string {
	if fieldType == "date" {
		return t.Format("2006-01-02")
	}
	if endOfDay {
		t = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	}
	return t.Format(time.RFC3339)
}

// validDateLiteral reports whether a NON-token operand is an acceptable literal for the field
// type: date → YYYY-MM-DD; datetime → anything canonicalDatetimeUTC accepts, plus a bare
// calendar date (long-standing behavior: CAST treats it as midnight — kept for compat).
func validDateLiteral(fieldType, v string) bool {
	v = strings.TrimSpace(v)
	if _, err := time.Parse("2006-01-02", v); err == nil {
		return true
	}
	if fieldType == "datetime" {
		if _, ok := canonicalDatetimeUTC(v); ok {
			return true
		}
	}
	return false
}

// resolveDateFilters rewrites a date/datetime filter whose operands may contain relative tokens,
// and rejects garbage operands loudly. Returns the (possibly rewritten) operator + operands.
// Call BEFORE validateFilterOperator: a token rewrite may change the operator (eq → between),
// and arity/type checks must see the final shape.
func resolveDateFilters(fieldType string, op dynrepo.FilterOperator, values []string, now time.Time) (dynrepo.FilterOperator, []string, error) {
	if fieldType != "date" && fieldType != "datetime" {
		return op, values, nil
	}
	if op == dynrepo.FilterIsNull || op == dynrepo.FilterIsNotNull {
		return op, values, nil
	}
	now = now.In(appLocation())

	// Single-operand token rewrite (eq/gt/gte/lt/lte).
	if len(values) == 1 {
		token := strings.ToLower(strings.TrimSpace(values[0]))
		if _, isToken := relativeDateTokens[token]; isToken {
			start, end := tokenRange(token, now)
			switch op {
			case dynrepo.FilterEq, "":
				return dynrepo.FilterBetween, []string{
					dateOperand(fieldType, start, false),
					dateOperand(fieldType, end, true),
				}, nil
			case dynrepo.FilterGte:
				return op, []string{dateOperand(fieldType, start, false)}, nil
			case dynrepo.FilterLte:
				return op, []string{dateOperand(fieldType, end, true)}, nil
			case dynrepo.FilterGt:
				return op, []string{dateOperand(fieldType, end, true)}, nil
			case dynrepo.FilterLt:
				return op, []string{dateOperand(fieldType, start, false)}, nil
			default:
				return op, values, invalid("LIST_INVALID_DATE_FILTER",
					"token ngày tương đối %q không dùng được với operator %q — dùng eq/gt/gte/lt/lte", token, op)
			}
		}
	}

	// No token: every operand must be a REAL date/datetime literal. The old behavior let
	// garbage fall through to SQL CAST → NULL → silently zero rows (the trust-killer this
	// slice exists to fix).
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue // empty operands fail arity checks downstream with their own code
		}
		if t := strings.ToLower(strings.TrimSpace(v)); func() bool { _, ok := relativeDateTokens[t]; return ok }() {
			// A token inside a multi-value op (in/between) — unsupported, say so.
			return op, values, invalid("LIST_INVALID_DATE_FILTER",
				"token ngày tương đối %q chỉ dùng được một mình với eq/gt/gte/lt/lte", t)
		}
		if !validDateLiteral(fieldType, v) {
			return op, values, invalid("LIST_INVALID_DATE_FILTER",
				"giá trị lọc ngày %q không hợp lệ — dùng YYYY-MM-DD%s hoặc token (%s)",
				v,
				map[bool]string{true: "", false: "/RFC3339"}[fieldType == "date"],
				strings.Join(sortedTokenList(), ", "))
		}
	}
	return op, values, nil
}

// sortedTokenList renders the token set for error messages/docs (stable order).
func sortedTokenList() []string {
	out := []string{
		"today", "yesterday", "tomorrow",
		"this_week", "last_week", "next_week",
		"this_month", "last_month", "this_quarter", "this_year",
		"last_7_days", "last_30_days", "last_90_days", "next_7_days", "next_30_days",
	}
	if len(out) != len(relativeDateTokens) {
		panic(fmt.Sprintf("relative token list drift: %d vs %d", len(out), len(relativeDateTokens)))
	}
	return out
}
