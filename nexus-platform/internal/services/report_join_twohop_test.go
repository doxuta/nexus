package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// Queue #293 (report-two-hop-join): a report may chain two relationships — tickets → employees
// (assignee) → departments (dept). Every hop is authorized on its OWN far entity, and a joined
// token resolves against the hop its "<rel>." prefix names.

var twoHop = []AggregateJoin{{Relationship: "assignee"}, {Relationship: "dept"}}

func TestResolveReportJoin_TwoHopChain(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	allow := &rjAuthz{allow: map[string]bool{"employees": true, "departments": true}}

	t.Run("hop 2 chains off hop 1's alias", func(t *testing.T) {
		s := buildRJService(&rjStore{}, allow, nil)
		j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", twoHop)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(j.hops) != 2 || len(j.joinSpecs) != 2 {
			t.Fatalf("hops=%d specs=%d, want 2/2", len(j.hops), len(j.joinSpecs))
		}
		h0, h1 := j.joinSpecs[0], j.joinSpecs[1]
		if h0.Alias != "j0" || h0.FromAlias != "" || h0.RelatedTable != "obj_employees" {
			t.Fatalf("hop0 = %+v", h0)
		}
		if h1.Alias != "j1" || h1.FromAlias != "j0" || h1.RelatedTable != "obj_departments" ||
			h1.LeftCol != "dept_id" || h1.RightCol != "id" || h1.Entity != "departments" {
			t.Fatalf("hop1 = %+v, want j1 chained off j0 over obj_departments dept_id=id", h1)
		}
		if j.hops[1].alias != "j1" || j.hops[1].relatedEntity != "departments" {
			t.Fatalf("hops[1] = %+v", j.hops[1])
		}
	})

	t.Run("a denied MIDDLE entity refuses the whole report", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": false, "departments": true}}, nil)
		_, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", twoHop)
		if codeOf(err) != "REPORT_JOIN_FORBIDDEN" {
			t.Fatalf("code = %q, want REPORT_JOIN_FORBIDDEN (err=%v)", codeOf(err), err)
		}
	})

	t.Run("a denied LAST entity refuses the whole report", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true, "departments": false}}, nil)
		_, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", twoHop)
		if codeOf(err) != "REPORT_JOIN_FORBIDDEN" {
			t.Fatalf("code = %q, want REPORT_JOIN_FORBIDDEN (err=%v)", codeOf(err), err)
		}
	})

	t.Run("each hop carries its own row-level scope", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{
			allow: map[string]bool{"employees": true, "departments": true},
			level: map[string]string{"employees": "owner", "departments": "all"},
		}, nil)
		j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", twoHop)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if j.joinSpecs[0].Scope.Level != dynrepo.AccessOwner || j.joinSpecs[1].Scope.Level != dynrepo.AccessAll {
			t.Fatalf("scopes = %q/%q, want owner/all", j.joinSpecs[0].Scope.Level, j.joinSpecs[1].Scope.Level)
		}
	})

	t.Run("hop 2 must start from the entity hop 1 reached", func(t *testing.T) {
		s := buildRJService(&rjStore{}, allow, nil)
		// "dept" is employees↔departments; tickets is neither, so listing it FIRST is a mismatch.
		_, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", []AggregateJoin{{Relationship: "dept"}, {Relationship: "assignee"}})
		if codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
			t.Fatalf("code = %q, want REPORT_JOIN_ENTITY_MISMATCH (err=%v)", codeOf(err), err)
		}
	})

	t.Run("a third hop is refused before any lookup", func(t *testing.T) {
		s := buildRJService(&rjStore{}, allow, nil)
		_, err := s.resolveReportJoin(ctx, scope, "t1", "tickets",
			[]AggregateJoin{{Relationship: "assignee"}, {Relationship: "dept"}, {Relationship: "tagged"}})
		if codeOf(err) != "REPORT_JOIN_TOO_MANY" {
			t.Fatalf("code = %q, want REPORT_JOIN_TOO_MANY", codeOf(err))
		}
	})

	t.Run("N-N as the second hop chains the link table off hop 1", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true, "tags": true}}, nil)
		j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets",
			[]AggregateJoin{{Relationship: "assignee"}, {Relationship: "emp_tags"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// hop 0 = employees (1 spec); hop 1 = link table + tags (2 specs).
		if len(j.joinSpecs) != 3 {
			t.Fatalf("specs = %d, want 3 (employees, link, tags)", len(j.joinSpecs))
		}
		link, far := j.joinSpecs[1], j.joinSpecs[2]
		if link.Alias != "lt1" || link.FromAlias != "j0" || link.RelatedTable != "rel_employees_tags" || link.LeftCol != "id" || link.RightCol != "employees_id" {
			t.Fatalf("link = %+v, want lt1 chained off j0 (NOT the base) over rel_employees_tags id=employees_id", link)
		}
		if far.Alias != "j1" || far.FromAlias != "lt1" || far.RelatedTable != "obj_tags" || far.LeftCol != "tags_id" || far.RightCol != "id" {
			t.Fatalf("far = %+v, want j1 chained off lt1 over obj_tags tags_id=id", far)
		}
	})
}

func TestValidateReportJoinConfig_TwoHop(t *testing.T) {
	ctx := context.Background()
	s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true, "departments": true}}, nil)
	if err := s.ValidateReportJoinConfig(ctx, "t1", "tickets", twoHop, "count"); err != nil {
		t.Fatalf("valid two-hop chain rejected at save: %v", err)
	}
	err := s.ValidateReportJoinConfig(ctx, "t1", "tickets", []AggregateJoin{{Relationship: "dept"}, {Relationship: "assignee"}}, "count")
	if codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
		t.Fatalf("misordered chain: code = %q, want REPORT_JOIN_ENTITY_MISMATCH (err=%v) — would save then die at run", codeOf(err), err)
	}
}

func TestAggregate_TwoHop(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	allow := &rjAuthz{allow: map[string]bool{"employees": true, "departments": true}}

	t.Run("groupBy on the far hop rides alias j1 over both specs", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, allow, &rjFieldACL{})
		q := AggregateQuery{Metric: "count", GroupBy: "dept.id", Joins: twoHop}
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("err: %v", err)
		}
		p := store.groupedParams
		if p.GroupByAlias != "j1" || len(p.Joins) != 2 {
			t.Fatalf("params alias=%q joins=%d, want j1 / 2", p.GroupByAlias, len(p.Joins))
		}
	})

	t.Run("metric on the MIDDLE hop rides alias j0", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, allow, &rjFieldACL{})
		q := AggregateQuery{Metric: "sum", Field: "assignee.salary", GroupBy: "dept.id", Joins: twoHop}
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("err: %v", err)
		}
		if store.groupedParams.FieldAlias != "j0" || store.groupedParams.GroupByAlias != "j1" {
			t.Fatalf("aliases field=%q group=%q, want j0/j1", store.groupedParams.FieldAlias, store.groupedParams.GroupByAlias)
		}
	})

	t.Run("a field hidden on the MIDDLE entity stays blocked, store never reached", func(t *testing.T) {
		store := &rjStore{}
		fACL := &rjFieldACL{hidden: map[string]bool{"employees.salary": true}}
		s := buildRJService(store, allow, fACL)
		q := AggregateQuery{Metric: "sum", Field: "assignee.salary", GroupBy: "dept.id", Joins: twoHop}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "FIELD_FORBIDDEN" {
			t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if store.scalarCalled || store.groupedCalled {
			t.Fatalf("store must NOT be reached")
		}
	})

	t.Run("a field hidden on the LAST entity stays blocked", func(t *testing.T) {
		store := &rjStore{}
		fACL := &rjFieldACL{hidden: map[string]bool{"departments.budget": true}}
		s := buildRJService(store, allow, fACL)
		q := AggregateQuery{Metric: "sum", Field: "dept.budget", Joins: twoHop}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "FIELD_FORBIDDEN" {
			t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
		}
	})

	t.Run("a token naming no configured hop is refused", func(t *testing.T) {
		s := buildRJService(&rjStore{}, allow, &rjFieldACL{})
		q := AggregateQuery{Metric: "count", GroupBy: "tagged.id", Joins: twoHop}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
			t.Fatalf("code = %q, want REPORT_JOIN_ENTITY_MISMATCH (err=%v)", codeOf(err), err)
		}
	})
}
