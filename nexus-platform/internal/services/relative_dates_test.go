// Wave 3.6 unit tests: relative-date token resolution (op rewrite + range boundaries in the app
// timezone, Monday-first weeks) and the loud-garbage contract (LIST_INVALID_DATE_FILTER instead
// of the old silent CAST-NULL empty result).
package services

import (
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
)

// fixedNow: Wednesday 2026-07-22 15:04:05 UTC = 22:04:05 +07 (same calendar day in VN).
var fixedNow = time.Date(2026, 7, 22, 15, 4, 5, 0, time.UTC)

func TestResolveDateFilters_TokensOnDateFields(t *testing.T) {
	tests := []struct {
		name    string
		op      dynrepo.FilterOperator
		token   string
		wantOp  dynrepo.FilterOperator
		wantVal []string
	}{
		{"eq today → between the day", dynrepo.FilterEq, "today", dynrepo.FilterBetween, []string{"2026-07-22", "2026-07-22"}},
		{"eq yesterday", dynrepo.FilterEq, "yesterday", dynrepo.FilterBetween, []string{"2026-07-21", "2026-07-21"}},
		{"eq this_week → Mon..Sun", dynrepo.FilterEq, "this_week", dynrepo.FilterBetween, []string{"2026-07-20", "2026-07-26"}},
		{"eq last_week", dynrepo.FilterEq, "last_week", dynrepo.FilterBetween, []string{"2026-07-13", "2026-07-19"}},
		{"eq this_month", dynrepo.FilterEq, "this_month", dynrepo.FilterBetween, []string{"2026-07-01", "2026-07-31"}},
		{"eq last_month", dynrepo.FilterEq, "last_month", dynrepo.FilterBetween, []string{"2026-06-01", "2026-06-30"}},
		{"eq this_quarter (Q3)", dynrepo.FilterEq, "this_quarter", dynrepo.FilterBetween, []string{"2026-07-01", "2026-09-30"}},
		{"eq this_year", dynrepo.FilterEq, "this_year", dynrepo.FilterBetween, []string{"2026-01-01", "2026-12-31"}},
		{"eq last_7_days (inclusive window)", dynrepo.FilterEq, "last_7_days", dynrepo.FilterBetween, []string{"2026-07-16", "2026-07-22"}},
		{"gte today → floor", dynrepo.FilterGte, "today", dynrepo.FilterGte, []string{"2026-07-22"}},
		{"lte this_week → last day", dynrepo.FilterLte, "this_week", dynrepo.FilterLte, []string{"2026-07-26"}},
		{"gt today → after the day", dynrepo.FilterGt, "today", dynrepo.FilterGt, []string{"2026-07-22"}},
		{"lt this_month → before the 1st", dynrepo.FilterLt, "this_month", dynrepo.FilterLt, []string{"2026-07-01"}},
		{"empty op treated as eq", "", "tomorrow", dynrepo.FilterBetween, []string{"2026-07-23", "2026-07-23"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, vals, err := resolveDateFilters("date", tt.op, []string{tt.token}, fixedNow)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if op != tt.wantOp {
				t.Fatalf("op = %q, want %q", op, tt.wantOp)
			}
			if len(vals) != len(tt.wantVal) {
				t.Fatalf("vals = %v, want %v", vals, tt.wantVal)
			}
			for i := range vals {
				if vals[i] != tt.wantVal[i] {
					t.Fatalf("vals[%d] = %q, want %q (all %v)", i, vals[i], tt.wantVal[i], vals)
				}
			}
		})
	}
}

func TestResolveDateFilters_TokensOnDatetimeFields(t *testing.T) {
	// Datetime operands are RFC3339 LOCALS (+07) that the existing canonicalize pipeline
	// converts to stored-UTC — assert the local rendering + end-of-day expansion.
	op, vals, err := resolveDateFilters("datetime", dynrepo.FilterEq, []string{"today"}, fixedNow)
	if err != nil || op != dynrepo.FilterBetween {
		t.Fatalf("op %q err %v", op, err)
	}
	if vals[0] != "2026-07-22T00:00:00+07:00" || vals[1] != "2026-07-22T23:59:59+07:00" {
		t.Fatalf("datetime bounds = %v", vals)
	}
	// And the canonicalize handoff produces the stored UTC literal shape.
	canon := canonicalizeFilterValues("datetime", vals)
	if canon[0] != "2026-07-21 17:00:00" || canon[1] != "2026-07-22 16:59:59" {
		t.Fatalf("canonicalized = %v", canon)
	}
}

func TestResolveDateFilters_GarbageIsLoud(t *testing.T) {
	for _, v := range []string{"hom-nay", "2026-13-45", "next_century", "07/22/2026"} {
		_, _, err := resolveDateFilters("date", dynrepo.FilterGte, []string{v}, fixedNow)
		if err == nil {
			t.Fatalf("garbage %q accepted", v)
		}
		if !strings.Contains(err.Error(), "không hợp lệ") {
			t.Fatalf("unexpected error for %q: %v", v, err)
		}
	}
}

func TestResolveDateFilters_TokenWithUnsupportedOps(t *testing.T) {
	if _, _, err := resolveDateFilters("date", dynrepo.FilterNeq, []string{"today"}, fixedNow); err == nil {
		t.Fatal("neq+token must be rejected")
	}
	if _, _, err := resolveDateFilters("date", dynrepo.FilterIn, []string{"today", "yesterday"}, fixedNow); err == nil {
		t.Fatal("in+tokens must be rejected")
	}
}

func TestResolveDateFilters_LiteralsAndOtherTypesPassThrough(t *testing.T) {
	// Valid literals untouched.
	op, vals, err := resolveDateFilters("date", dynrepo.FilterGte, []string{"2026-07-01"}, fixedNow)
	if err != nil || op != dynrepo.FilterGte || vals[0] != "2026-07-01" {
		t.Fatalf("literal mangled: %q %v %v", op, vals, err)
	}
	// Datetime accepts RFC3339 + naive + bare date (compat).
	for _, v := range []string{"2026-07-01T09:30:00Z", "2026-07-01T09:30:00", "2026-07-01"} {
		if _, _, err := resolveDateFilters("datetime", dynrepo.FilterLte, []string{v}, fixedNow); err != nil {
			t.Fatalf("datetime literal %q rejected: %v", v, err)
		}
	}
	// Non-date field types never touched.
	op, vals, err = resolveDateFilters("text", dynrepo.FilterEq, []string{"today"}, fixedNow)
	if err != nil || op != dynrepo.FilterEq || vals[0] != "today" {
		t.Fatalf("text field must pass through: %q %v %v", op, vals, err)
	}
	// isnull skips entirely.
	if _, _, err := resolveDateFilters("date", dynrepo.FilterIsNull, nil, fixedNow); err != nil {
		t.Fatalf("isnull: %v", err)
	}
}

func TestSortedTokenListMatchesSet(t *testing.T) {
	if len(sortedTokenList()) != len(relativeDateTokens) {
		t.Fatal("token list drift")
	}
}

func TestResolveDateFilters_TimezoneBoundary(t *testing.T) {
	// 2026-07-22 18:30 UTC = 2026-07-23 01:30 +07 — "today" must be the VN calendar day (23rd),
	// not the UTC one. This is the exact off-by-one a UTC-naive implementation would make.
	lateUTC := time.Date(2026, 7, 22, 18, 30, 0, 0, time.UTC)
	op, vals, err := resolveDateFilters("date", dynrepo.FilterEq, []string{"today"}, lateUTC)
	if err != nil || op != dynrepo.FilterBetween {
		t.Fatalf("op %q err %v", op, err)
	}
	if vals[0] != "2026-07-23" || vals[1] != "2026-07-23" {
		t.Fatalf("VN-day boundary broken: %v (want 2026-07-23)", vals)
	}
}
