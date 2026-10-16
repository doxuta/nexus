package services

import (
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// TestResolveGroupAxis_SameErrorsAsBefore pins the group-axis resolution (specs/049 S1, T021/T024):
// resolveGroupAxis is extracted VERBATIM from resolveAggregatePlan / resolveAggregatePlanJoined so
// the grid engine (one axis per report level) and the single-axis path share ONE body. Every case
// asserts the literal code + message the single-axis path returned BEFORE the extraction, through
// BOTH entry points — the new helper and the old plan resolvers — so a drift in either shows up
// here, not as a changed 422 in a customer's report.
func TestResolveGroupAxis_SameErrorsAsBefore(t *testing.T) {
	base := []FieldSpec{
		{Name: "stage", FieldType: "picklist", IsIndexed: true},
		{Name: "close_date", FieldType: "date", IsIndexed: true},
		{Name: "notes", FieldType: "text"},
	}
	baseByName := map[string]FieldSpec{}
	for _, s := range base {
		baseByName[s.Name] = s
	}
	// "assignee" hop: region (indexed text), salary (number), budget (currency) + an indexed date,
	// so a bucket on a JOINED date axis proves the bucket check reads the related entity's specs.
	join := relatedJoin()
	hired := FieldSpec{Name: "hired_on", FieldType: "date", IsIndexed: true}
	join.hops[0].specs = append(join.hops[0].specs, hired)
	join.hops[0].byName[hired.Name] = hired

	const notIndexed = "không thể group theo field %q (cần là field có index: unique/relation/indexed, hoặc cột hệ thống id/created_at/updated_at)"
	tests := []struct {
		name        string
		token       string
		bucket      string
		joined      bool
		wantCode    string
		wantMsg     string
		wantPhys    string
		wantField   string
		wantAlias   string
		wantBucket  dynrepo.AggregateBucket
		wantOnJoin  bool // axis spec map must be the joined entity's
		wantNoError bool
	}{
		{name: "base indexed", token: "stage", wantNoError: true, wantPhys: "stage", wantField: "stage"},
		{name: "base trims token", token: "  stage ", wantNoError: true, wantPhys: "stage", wantField: "stage"},
		{name: "base date + bucket", token: "close_date", bucket: " Month ", wantNoError: true,
			wantPhys: "close_date", wantField: "close_date", wantBucket: dynrepo.BucketMonth},
		{name: "base system column bucket", token: "created_at", bucket: "year", wantNoError: true,
			wantPhys: "created_at", wantField: "created_at", wantBucket: dynrepo.BucketYear},
		{name: "base not indexed", token: "notes", wantCode: "AGGREGATE_REQUIRES_INDEXED",
			wantMsg: sprintfQ(notIndexed, "notes")},
		{name: "base dotted token is NOT split without a join", token: "assignee.region",
			wantCode: "AGGREGATE_REQUIRES_INDEXED", wantMsg: sprintfQ(notIndexed, "assignee.region")},
		{name: "bucket invalid (raw token echoed)", token: "close_date", bucket: "Fortnight",
			wantCode: "AGGREGATE_BUCKET_INVALID",
			wantMsg:  `bucket "Fortnight" không hợp lệ (chấp nhận: day,week,month,quarter,year)`},
		{name: "bucket without groupBy", token: "", bucket: "month",
			wantCode: "AGGREGATE_BUCKET_REQUIRES_GROUPBY",
			wantMsg:  "bucket cần groupBy trỏ vào field ngày (date/datetime) hoặc cột hệ thống created_at/updated_at"},
		{name: "bucket on non-date", token: "stage", bucket: "month",
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE",
			wantMsg:  `không thể bucket theo field "stage" — chỉ field date/datetime hoặc cột hệ thống created_at/updated_at`},
		{name: "no token no bucket", token: "", wantNoError: true},

		{name: "joined namespaced axis", token: "assignee.region", joined: true, wantNoError: true,
			wantPhys: "region", wantField: "region", wantAlias: reportJoinAlias, wantOnJoin: true},
		{name: "joined date + bucket rides the related entity", token: "assignee.hired_on", bucket: "quarter",
			joined: true, wantNoError: true, wantPhys: "hired_on", wantField: "hired_on",
			wantAlias: reportJoinAlias, wantBucket: dynrepo.BucketQuarter, wantOnJoin: true},
		{name: "joined bare axis stays base", token: "stage", joined: true, wantNoError: true,
			wantPhys: "stage", wantField: "stage"},
		{name: "joined prefix mismatch", token: "wrongrel.region", joined: true,
			wantCode: "REPORT_JOIN_ENTITY_MISMATCH",
			wantMsg:  `groupBy "wrongrel.region" tham chiếu "wrongrel" — không khớp join "assignee"`},
		{name: "joined not indexed on related", token: "assignee.salary", joined: true,
			wantCode: "AGGREGATE_REQUIRES_INDEXED", wantMsg: sprintfQ(notIndexed, "salary")},
		{name: "joined bucket on related non-date", token: "assignee.region", bucket: "month", joined: true,
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE",
			wantMsg:  `không thể bucket theo field "region" — chỉ field date/datetime hoặc cột hệ thống created_at/updated_at`},
		{name: "joined bucket without groupBy", token: "", bucket: "day", joined: true,
			wantCode: "AGGREGATE_BUCKET_REQUIRES_GROUPBY",
			wantMsg:  "bucket cần groupBy trỏ vào field ngày (date/datetime) hoặc cột hệ thống created_at/updated_at"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var j *resolvedReportJoin
			if tt.joined {
				j = join
			}
			ax, err := resolveGroupAxis(tt.token, tt.bucket, base, baseByName, j)

			// The single-axis plan resolvers (count → no metric-field noise) must agree verbatim.
			var plan aggregatePlan
			var perr error
			if tt.joined {
				plan, perr = resolveAggregatePlanJoined("count", "", tt.token, tt.bucket, base, baseByName, join)
			} else {
				plan, perr = resolveAggregatePlan("count", "", tt.token, tt.bucket, base, baseByName)
			}

			for label, e := range map[string]error{"resolveGroupAxis": err, "plan": perr} {
				if tt.wantNoError {
					if e != nil {
						t.Fatalf("%s: unexpected error %v", label, e)
					}
					continue
				}
				var ve *ValidationError
				if !errors.As(e, &ve) {
					t.Fatalf("%s: err = %v; want *ValidationError %s", label, e, tt.wantCode)
				}
				if ve.Code != tt.wantCode || ve.Msg != tt.wantMsg {
					t.Fatalf("%s: got (%s, %q); want (%s, %q)", label, ve.Code, ve.Msg, tt.wantCode, tt.wantMsg)
				}
			}
			if !tt.wantNoError {
				return
			}
			if ax.physical != tt.wantPhys || ax.field != tt.wantField || ax.alias != tt.wantAlias || ax.bucket != tt.wantBucket {
				t.Fatalf("axis = %+v; want physical=%q field=%q alias=%q bucket=%q",
					ax, tt.wantPhys, tt.wantField, tt.wantAlias, tt.wantBucket)
			}
			if plan.groupByPhysical != ax.physical || plan.groupByField != ax.field ||
				plan.groupByAlias != ax.alias || plan.bucket != ax.bucket {
				t.Fatalf("plan groupBy (%q,%q,%q,%q) != axis %+v", plan.groupByPhysical, plan.groupByField,
					plan.groupByAlias, plan.bucket, ax)
			}
			// The axis carries the spec map of the entity it lives on — the field-ACL polymorphic
			// guard of the grid engine (S2a) asserts against it.
			wantMap := baseByName
			if tt.wantOnJoin {
				wantMap = join.hops[0].byName
			}
			if tt.wantField != "" {
				if _, declared := wantMap[tt.wantField]; declared {
					if _, ok := ax.byName[tt.wantField]; !ok {
						t.Fatalf("axis spec map does not know %q", tt.wantField)
					}
				}
				if len(ax.byName) != len(wantMap) {
					t.Fatalf("axis spec map is the wrong entity's (len %d, want %d)", len(ax.byName), len(wantMap))
				}
			}
		})
	}
}

// sprintfQ formats a one-%q message template (keeps the literal templates above readable).
func sprintfQ(format, arg string) string { return fmt.Sprintf(format, arg) }
