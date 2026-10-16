package services

// Service-layer unit tests for REPORT-JOIN v1 (../specs/025-report-join/spec.md) — the branchy guards
// both review passes flagged as untested: direction/N-N/edge resolution, prefix-namespacing,
// per-hop entity-ACL (deny → 403, fail-closed when unwired), config-time relationship validation,
// and — the security-auditor's concrete leak scenario — field-ACL routed to the RELATED entity.
// No DB: the aggregate SQL isolation is proven separately by the dynrepo integration tests.

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// ---- pure helpers -------------------------------------------------------------------------------

func rel(kind, parent, child, fk string, joinTable *string) *metadata.RelationshipDefinition {
	return &metadata.RelationshipDefinition{
		Name: "r", RelationshipType: kind, ParentEntityName: parent, ChildEntityName: child,
		ChildFKColumn: fk, JoinTableName: joinTable,
	}
}

func TestResolveJoinDirection(t *testing.T) {
	jt := "rel_a_b"
	tests := []struct {
		name                             string
		rel                              *metadata.RelationshipDefinition
		base                             string
		wantRelated, wantLeft, wantRight string
		wantCode                         string // "" = success
	}{
		{"base is child → hop to parent", rel("1-N", "accounts", "contacts", "account_id", nil),
			"contacts", "accounts", "account_id", "id", ""},
		{"base is parent → fan-out to child", rel("1-N", "accounts", "contacts", "account_id", nil),
			"accounts", "contacts", "id", "account_id", ""},
		{"1-1 also FK-on-child", rel("1-1", "profiles", "users", "profile_id", nil),
			"users", "profiles", "profile_id", "id", ""},
		{"N-N rejected by type", rel("N-N", "a", "b", "", &jt), "a", "", "", "", "REPORT_JOIN_NN_UNSUPPORTED"},
		{"N-N rejected by join table even if type looks 1-N", rel("1-N", "a", "b", "x_id", &jt),
			"a", "", "", "", "REPORT_JOIN_NN_UNSUPPORTED"},
		{"empty FK column fails closed", rel("1-N", "accounts", "contacts", "", nil),
			"contacts", "", "", "", "REPORT_JOIN_RELATION_UNKNOWN"},
		{"base not an edge", rel("1-N", "accounts", "contacts", "account_id", nil),
			"tickets", "", "", "", "REPORT_JOIN_ENTITY_MISMATCH"},
		{"self relation resolves child edge", rel("1-N", "tasks", "tasks", "parent_task_id", nil),
			"tasks", "tasks", "parent_task_id", "id", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			related, left, right, err := resolveJoinDirection(tt.rel, tt.base, "r")
			if tt.wantCode != "" {
				if codeOf(err) != tt.wantCode {
					t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if related != tt.wantRelated || left != tt.wantLeft || right != tt.wantRight {
				t.Fatalf("got (%s,%s,%s), want (%s,%s,%s)", related, left, right, tt.wantRelated, tt.wantLeft, tt.wantRight)
			}
		})
	}
}

func TestSplitJoinedToken(t *testing.T) {
	tests := []struct {
		in                string
		wantPre, wantBare string
		wantNS            bool
	}{
		{"industry", "", "industry", false},
		{"account.industry", "account", "industry", true},
		{"a.b.c", "a", "b.c", true}, // split at FIRST dot only
	}
	for _, tt := range tests {
		pre, bare, ns := splitJoinedToken(tt.in)
		if pre != tt.wantPre || bare != tt.wantBare || ns != tt.wantNS {
			t.Fatalf("%q → (%q,%q,%v), want (%q,%q,%v)", tt.in, pre, bare, ns, tt.wantPre, tt.wantBare, tt.wantNS)
		}
	}
}

// relatedJoin builds a resolvedReportJoin over an "assignee" hop whose related entity carries an
// indexed "region" (groupable) + a "salary" number + a "budget" currency field.
func relatedJoin() *resolvedReportJoin {
	specs := []FieldSpec{
		{Name: "region", FieldType: "text", IsIndexed: true},
		{Name: "salary", FieldType: "number"},
		{Name: "budget", FieldType: "currency", Options: map[string]any{"currencyCode": "USD"}},
	}
	byName := map[string]FieldSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	return &resolvedReportJoin{hops: []resolvedHop{{name: "assignee", relatedEntity: "employees", alias: reportJoinAlias, specs: specs, byName: byName}}}
}

func TestResolveAggregatePlanJoined(t *testing.T) {
	base := []FieldSpec{{Name: "stage", FieldType: "picklist", IsIndexed: true}}
	baseByName := map[string]FieldSpec{"stage": base[0]}
	join := relatedJoin()

	t.Run("namespaced groupBy resolves on related + alias j0", func(t *testing.T) {
		plan, err := resolveAggregatePlanJoined("count", "", "assignee.region", "", base, baseByName, join)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if plan.groupByAlias != reportJoinAlias || plan.groupByPhysical != "region" || plan.groupByField != "region" {
			t.Fatalf("groupBy plan = %+v, want alias j0 / physical region", plan)
		}
	})

	t.Run("bare groupBy stays on base (no alias)", func(t *testing.T) {
		plan, err := resolveAggregatePlanJoined("count", "", "stage", "", base, baseByName, join)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if plan.groupByAlias != "" || plan.groupByPhysical != "stage" {
			t.Fatalf("bare groupBy must stay base-unqualified: %+v", plan)
		}
	})

	t.Run("namespaced currency metric carries subPath + currencyCode + alias", func(t *testing.T) {
		plan, err := resolveAggregatePlanJoined("sum", "assignee.budget", "", "", base, baseByName, join)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if plan.fieldAlias != reportJoinAlias || plan.fieldColumn != "budget" || plan.fieldSubPath != "amount" || plan.currencyCode != "USD" {
			t.Fatalf("currency metric plan = %+v, want alias j0 / budget / amount / USD", plan)
		}
	})

	t.Run("prefix mismatch on groupBy → ENTITY_MISMATCH", func(t *testing.T) {
		_, err := resolveAggregatePlanJoined("count", "", "wrongrel.region", "", base, baseByName, join)
		if codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
			t.Fatalf("code = %q, want REPORT_JOIN_ENTITY_MISMATCH", codeOf(err))
		}
	})

	t.Run("prefix mismatch on metric field → ENTITY_MISMATCH", func(t *testing.T) {
		_, err := resolveAggregatePlanJoined("sum", "wrongrel.salary", "", "", base, baseByName, join)
		if codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
			t.Fatalf("code = %q, want REPORT_JOIN_ENTITY_MISMATCH", codeOf(err))
		}
	})

	t.Run("joined non-indexed groupBy → REQUIRES_INDEXED", func(t *testing.T) {
		_, err := resolveAggregatePlanJoined("count", "", "assignee.salary", "", base, baseByName, join)
		if codeOf(err) != "AGGREGATE_REQUIRES_INDEXED" {
			t.Fatalf("code = %q, want AGGREGATE_REQUIRES_INDEXED", codeOf(err))
		}
	})
}

// ---- fakes for receiver methods -----------------------------------------------------------------

type rjCatalog struct {
	entities map[string]*metadata.EntityDefinition
	fields   map[string][]metadata.FieldDefinition // keyed by entity ID
}

func (c *rjCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	e, ok := c.entities[name]
	if !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return e, nil
}
func (c *rjCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return c.fields[entityID], nil
}

type rjRollup struct {
	rels map[string]*metadata.RelationshipDefinition
}

func (r *rjRollup) GetEntityByName(context.Context, string, string) (*metadata.EntityDefinition, error) {
	panic("rjRollup.GetEntityByName should not be called (resolve uses catalog)")
}
func (r *rjRollup) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	panic("rjRollup.ListFields should not be called")
}
func (r *rjRollup) GetRelationshipByName(_ context.Context, _, name string) (*metadata.RelationshipDefinition, error) {
	rel, ok := r.rels[name]
	if !ok {
		return nil, metadata.ErrRelationshipNotFound
	}
	return rel, nil
}

type rjAuthz struct {
	allow map[string]bool
	level map[string]string
}

func (a *rjAuthz) Authorize(_ context.Context, _, _, entity, _ string) (bool, string, error) {
	lvl := a.level[entity]
	if lvl == "" {
		lvl = "all"
	}
	return a.allow[entity], lvl, nil
}

type rjFieldACL struct {
	hidden map[string]bool // "entity.field" hidden
	asked  []string        // records "entity.field" queried
}

func (f *rjFieldACL) key(entity, field string) string { return entity + "." + field }
func (f *rjFieldACL) AssertFieldVisible(_ context.Context, _, _, entity string, isAdmin bool, field string) error {
	if field == "" || isAdmin {
		return nil
	}
	f.asked = append(f.asked, f.key(entity, field))
	if f.hidden[f.key(entity, field)] {
		return invalid("FIELD_FORBIDDEN", "field %q ẩn với role", field)
	}
	return nil
}
func (f *rjFieldACL) AssertFieldsVisible(ctx context.Context, tid, rid, entity string, isAdmin bool, fields []string) error {
	for _, fld := range fields {
		if err := f.AssertFieldVisible(ctx, tid, rid, entity, isAdmin, fld); err != nil {
			return err
		}
	}
	return nil
}
func (f *rjFieldACL) FilterReadFields(context.Context, string, string, string, bool, map[string]any) error {
	return nil
}
func (f *rjFieldACL) FilterReadFieldsBatch(context.Context, string, string, string, bool, []map[string]any) error {
	return nil
}
func (f *rjFieldACL) CheckWriteFields(context.Context, string, string, string, bool, map[string]any) error {
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: this fake has no notion of a record's
// current values. The update-path rule it enables — an unchanged locked field is a no-op, not an
// edit — is exercised where the current values ARE modelled.
func (f *rjFieldACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}
func (f *rjFieldACL) HiddenFields(context.Context, string, string, string, bool) (map[string]struct{}, error) {
	return nil, nil
}

// rjStore implements RecordStore; only the aggregate methods are expected to be reached, and only
// AFTER the ACL gates pass. scalarCalled/groupedCalled/gridCalled record that; the grid call also
// keeps the params it received and answers with the canned gridRows/gridCols/gridErr.
type rjStore struct {
	scalarCalled, groupedCalled, gridCalled bool
	groupedParams                           dynrepo.AggregateParams
	gridParams                              dynrepo.GridParams
	gridScope                               dynrepo.RowScope
	gridRows, gridCols                      []dynrepo.GridRow
	gridErr                                 error
}

func (s *rjStore) InsertRecord(context.Context, string, string, string, map[string]any, []dynrepo.UniqueFieldSpec) (string, error) {
	panic("unexpected InsertRecord")
}
func (s *rjStore) GetRecord(context.Context, string, string, string, dynrepo.RowScope) (*dynrepo.Record, error) {
	panic("unexpected GetRecord")
}
func (s *rjStore) ListRecords(context.Context, string, string, dynrepo.RowScope, dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	panic("unexpected ListRecords")
}
func (s *rjStore) ListRecordsCursor(context.Context, string, string, dynrepo.RowScope, dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	panic("unexpected ListRecordsCursor")
}
func (s *rjStore) UpdateRecord(context.Context, string, string, string, string, dynrepo.RowScope, map[string]any, *int, []dynrepo.UniqueFieldSpec) error {
	panic("unexpected UpdateRecord")
}
func (s *rjStore) SoftDeleteRecord(context.Context, string, string, string, dynrepo.RowScope) error {
	panic("unexpected SoftDeleteRecord")
}
func (s *rjStore) RestoreRecord(context.Context, string, string, string, string, dynrepo.RowScope, []dynrepo.UniqueFieldSpec) error {
	panic("unexpected RestoreRecord")
}
func (s *rjStore) RecordExists(context.Context, string, string, string) (bool, error) {
	panic("unexpected RecordExists")
}
func (s *rjStore) BatchRecordExists(context.Context, string, string, []string) (map[string]bool, error) {
	panic("unexpected BatchRecordExists")
}
func (s *rjStore) AggregateScalar(context.Context, string, string, dynrepo.RowScope, dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	s.scalarCalled = true
	return dynrepo.AggregateScalar{Count: 0}, nil
}
func (s *rjStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, p dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	s.groupedCalled, s.groupedParams = true, p
	return nil, nil
}
func (s *rjStore) AggregateGrid(_ context.Context, _, _ string, scope dynrepo.RowScope, p dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	s.gridCalled, s.gridParams, s.gridScope = true, p, scope
	return s.gridRows, s.gridCols, s.gridErr
}
func (s *rjStore) SetOwner(context.Context, string, string, string, string, dynrepo.RowScope, string) error {
	panic("unexpected SetOwner")
}

func ent(name string) *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: name, Name: name}
}

// rjNNJoinTable is the fixture N-N relationship's physical link table name (ddl.JoinParts sorts
// "tags" < "tickets" alphabetically — rel_tags_tickets).
var rjNNJoinTable = "rel_tags_tickets"
var rjEmpTagsTable = "rel_employees_tags"

// buildRJService wires a RecordService with the given fakes for the join-path tests. Includes
// BOTH an FK-on-child fixture ("assignee": tickets→employees, 1-N) and an N-N fixture ("tagged":
// tickets↔tags) so REPORT-JOIN and REPORT-JOIN-NN tests share one builder.
func buildRJService(store RecordStore, authz *rjAuthz, fACL *rjFieldACL) *RecordService {
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"tickets": ent("tickets"), "employees": ent("employees"), "tags": ent("tags"), "departments": ent("departments"),
		},
		fields: map[string][]metadata.FieldDefinition{
			"tickets":     {{Name: "stage", FieldType: "picklist"}, {Name: "priority", FieldType: "number"}},
			"employees":   {{Name: "salary", FieldType: "number"}},
			"tags":        {{Name: "weight", FieldType: "number"}},
			"departments": {{Name: "budget", FieldType: "number"}},
		},
	}
	rollup := &rjRollup{rels: map[string]*metadata.RelationshipDefinition{
		"assignee": rel("1-N", "employees", "tickets", "assignee_id", nil), // tickets(child).assignee_id → employees(parent)
		"tagged":   rel("N-N", "tickets", "tags", "", &rjNNJoinTable),      // tickets↔tags, N-N
		"dept":     rel("1-N", "departments", "employees", "dept_id", nil), // employees(child).dept_id → departments(parent)
		"emp_tags": rel("N-N", "employees", "tags", "", &rjEmpTagsTable),   // employees↔tags, N-N (second-hop fixture)
	}}
	s := NewRecordService(cat, store).WithRollupMeta(rollup)
	if authz != nil {
		s = s.WithACLAuthorizer(authz)
	}
	if fACL != nil {
		s = s.WithFieldACL(fACL)
	}
	return s
}

func TestResolveReportJoin_Authz(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}

	t.Run("deny on related entity → REPORT_JOIN_FORBIDDEN", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": false}}, nil)
		_, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", []AggregateJoin{{Relationship: "assignee"}})
		if codeOf(err) != "REPORT_JOIN_FORBIDDEN" {
			t.Fatalf("code = %q, want REPORT_JOIN_FORBIDDEN (err=%v)", codeOf(err), err)
		}
	})

	t.Run("allow → related scope carries the resolved access level", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true}, level: map[string]string{"employees": "owner"}}, nil)
		j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", []AggregateJoin{{Relationship: "assignee"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(j.joinSpecs) != 1 {
			t.Fatalf("FK-on-child hop must have exactly 1 JoinSpec, got %d", len(j.joinSpecs))
		}
		hop := j.joinSpecs[0]
		if hop.Scope.Level != dynrepo.AccessOwner || hop.Scope.UserID != "u1" {
			t.Fatalf("hop scope = %+v, want owner/u1", hop.Scope)
		}
		if hop.LeftCol != "assignee_id" || hop.RightCol != "id" || hop.RelatedTable != "obj_employees" {
			t.Fatalf("hop spec cols = %+v, want assignee_id/id/obj_employees", hop)
		}
		// DAILY-QUEUE #225: the hop names its entity so manual shares can be pinned to it.
		if hop.Entity != "employees" {
			t.Fatalf("hop.Entity = %q, want employees", hop.Entity)
		}
	})

	t.Run("shares follow the request scope: opted-in caller → hop opts in, otherwise not", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true}, level: map[string]string{"employees": "owner"}}, nil)
		for _, optIn := range []bool{true, false} {
			sc := scope
			sc.IncludeShares = optIn
			j, err := s.resolveReportJoin(ctx, sc, "t1", "tickets", []AggregateJoin{{Relationship: "assignee"}})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got := j.joinSpecs[0].Scope.IncludeShares; got != optIn {
				t.Fatalf("hop IncludeShares = %v for a request scope with %v", got, optIn)
			}
		}
	})

	t.Run("no authorizer wired → hop fails closed to AccessNone", func(t *testing.T) {
		s := buildRJService(&rjStore{}, nil, nil) // authz nil
		j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", []AggregateJoin{{Relationship: "assignee"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if j.joinSpecs[0].Scope.Level != dynrepo.AccessNone {
			t.Fatalf("nil-authz hop scope = %q, want AccessNone (fail-closed)", j.joinSpecs[0].Scope.Level)
		}
	})

	t.Run("admin → related scope is AccessAll", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": false}}, nil) // deny ignored for admin
		adminScope := RecordScope{UserID: "root", RoleID: "r", IsAdmin: true, AccessLevel: "all"}
		j, err := s.resolveReportJoin(ctx, adminScope, "t1", "tickets", []AggregateJoin{{Relationship: "assignee"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if j.joinSpecs[0].Scope.Level != dynrepo.AccessAll || !j.joinSpecs[0].Scope.IsAdmin {
			t.Fatalf("admin hop scope = %+v, want AccessAll/IsAdmin", j.joinSpecs[0].Scope)
		}
	})
}

func TestValidateReportJoinConfig(t *testing.T) {
	ctx := context.Background()
	s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"employees": true}}, nil)

	cases := []struct {
		name     string
		base     string
		joins    []AggregateJoin
		metric   AggregateMetric
		wantCode string
	}{
		{"unknown relationship", "tickets", []AggregateJoin{{Relationship: "nope"}}, "count", "REPORT_JOIN_RELATION_UNKNOWN"},
		{"entity not an edge", "invoices", []AggregateJoin{{Relationship: "assignee"}}, "count", "REPORT_JOIN_ENTITY_MISMATCH"},
		{"too many", "tickets", []AggregateJoin{{Relationship: "assignee"}, {Relationship: "tagged"}, {Relationship: "assignee"}}, "count", "REPORT_JOIN_TOO_MANY"},
		{"same relationship twice", "tickets", []AggregateJoin{{Relationship: "assignee"}, {Relationship: "assignee"}}, "count", "REPORT_JOIN_ENTITY_MISMATCH"},
		{"valid child edge", "tickets", []AggregateJoin{{Relationship: "assignee"}}, "count", ""},
		{"empty relationship name", "tickets", []AggregateJoin{{Relationship: "  "}}, "count", "REPORT_JOIN_RELATION_UNKNOWN"},
		{"no joins is a no-op", "tickets", nil, "count", ""},
		// REPORT-JOIN-NN config-time fan-out gate — N-N is SYMMETRIC (either side can fan out),
		// so sum/avg is rejected UNCONDITIONALLY regardless of which field the report would
		// aggregate (that choice isn't even visible here — only the metric matters).
		{"N-N: count allowed", "tickets", []AggregateJoin{{Relationship: "tagged"}}, "count", ""},
		{"N-N: sum rejected (symmetric fan-out)", "tickets", []AggregateJoin{{Relationship: "tagged"}}, "sum", "AGGREGATE_NN_FANOUT_UNSAFE"},
		{"N-N: avg rejected (symmetric fan-out)", "tickets", []AggregateJoin{{Relationship: "tagged"}}, "avg", "AGGREGATE_NN_FANOUT_UNSAFE"},
		{"N-N: min allowed (non-additive)", "tickets", []AggregateJoin{{Relationship: "tagged"}}, "min", ""},
		{"N-N: max allowed (non-additive)", "tickets", []AggregateJoin{{Relationship: "tagged"}}, "max", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := s.ValidateReportJoinConfig(ctx, "t1", tt.base, tt.joins, tt.metric)
			if codeOf(err) != tt.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
			}
		})
	}

	t.Run("nil resolver → no-op (run-time re-validates)", func(t *testing.T) {
		bare := NewRecordService(&rjCatalog{}, &rjStore{}) // no WithRollupMeta
		if err := bare.ValidateReportJoinConfig(ctx, "t1", "tickets", []AggregateJoin{{Relationship: "x"}}, "count"); err != nil {
			t.Fatalf("nil-resolver must be a no-op, got %v", err)
		}
	})
}

// TestResolveJoinEdge_Dispatch proves resolveJoinEdge routes to the FK-on-child resolver for
// 1-1/1-N rows and to resolveNNJoinEdge for N-N rows (by type OR non-nil JoinTableName), and that
// the N-N branch resolves the far entity from EITHER side of the registered edge.
func TestResolveJoinEdge_Dispatch(t *testing.T) {
	jt := "rel_a_b"
	tests := []struct {
		name        string
		rel         *metadata.RelationshipDefinition
		base        string
		wantRelated string
		wantIsNN    bool
		wantCode    string
	}{
		{"FK-on-child dispatches to resolveJoinDirection", rel("1-N", "accounts", "contacts", "account_id", nil),
			"contacts", "accounts", false, ""},
		{"N-N by type dispatches to resolveNNJoinEdge", rel("N-N", "a", "b", "", &jt), "a", "b", true, ""},
		{"N-N by type, far resolved from the other edge", rel("N-N", "a", "b", "", &jt), "b", "a", true, ""},
		{"N-N by non-nil JoinTableName even if type says 1-N (defense-in-depth)",
			rel("1-N", "a", "b", "x_id", &jt), "a", "b", true, ""},
		{"N-N base not an edge", rel("N-N", "a", "b", "", &jt), "c", "", true, "REPORT_JOIN_ENTITY_MISMATCH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			related, _, _, isNN, err := resolveJoinEdge(tt.rel, tt.base, "r")
			if tt.wantCode != "" {
				if codeOf(err) != tt.wantCode {
					t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if related != tt.wantRelated || isNN != tt.wantIsNN {
				t.Fatalf("got (related=%s isNN=%v), want (related=%s isNN=%v)", related, isNN, tt.wantRelated, tt.wantIsNN)
			}
		})
	}
}

// TestValidateNNFanoutSafety is the allow/reject matrix table test (see validateNNFanoutSafety's
// doc for the full rationale per row). N-N is SYMMETRIC — sum/avg is rejected UNCONDITIONALLY
// (the function doesn't even take a field argument any more: which side the aggregated field
// lives on cannot make sum/avg safe, since EITHER side of an N-N hop can fan out).
func TestValidateNNFanoutSafety(t *testing.T) {
	tests := []struct {
		name     string
		isNN     bool
		metric   AggregateMetric
		wantCode string
	}{
		{"not N-N → always allowed regardless of metric", false, "sum", ""},
		{"not N-N avg → always allowed", false, "avg", ""},
		{"N-N count → allowed (pair count)", true, "count", ""},
		{"N-N min → allowed (non-additive)", true, "min", ""},
		{"N-N max → allowed (non-additive)", true, "max", ""},
		{"N-N sum → rejected unconditionally (symmetric fan-out)", true, "sum", "AGGREGATE_NN_FANOUT_UNSAFE"},
		{"N-N avg → rejected unconditionally (symmetric fan-out)", true, "avg", "AGGREGATE_NN_FANOUT_UNSAFE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNNFanoutSafety(tt.isNN, tt.metric)
			if codeOf(err) != tt.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
			}
		})
	}
}

// TestResolveReportJoin_NN proves the N-N hop builds TWO chained JoinSpecs (base→link,
// link→far) with the correct aliases/columns/flags — the structural half of REPORT-JOIN-NN (the
// per-hop SQL's tenant + ACL correctness is proven by the dynrepo integration tests).
func TestResolveReportJoin_NN(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"tags": true}, level: map[string]string{"tags": "owner"}}, nil)

	j, err := s.resolveReportJoin(ctx, scope, "t1", "tickets", []AggregateJoin{{Relationship: "tagged"}})
	if err != nil {
		t.Fatalf("resolveReportJoin: %v", err)
	}
	if !j.isNN {
		t.Fatalf("isNN = false, want true for an N-N relationship")
	}
	if j.hops[0].relatedEntity != "tags" {
		t.Fatalf("relatedEntity = %q, want tags", j.hops[0].relatedEntity)
	}
	if len(j.joinSpecs) != 2 {
		t.Fatalf("N-N hop must build exactly 2 chained JoinSpecs, got %d: %+v", len(j.joinSpecs), j.joinSpecs)
	}
	hop1, hop2 := j.joinSpecs[0], j.joinSpecs[1]

	// Hop 1: base → link table. Pure tenant isolation — no soft-delete, no A5 row-scope (the
	// link table carries no deleted_at/team_set_id; see dynrepo.JoinSpec's doc).
	if hop1.Alias != reportJoinLinkAlias || hop1.RelatedTable != rjNNJoinTable {
		t.Fatalf("hop1 = %+v, want alias %q / table %q", hop1, reportJoinLinkAlias, rjNNJoinTable)
	}
	if hop1.LeftCol != "id" || hop1.RightCol != "tickets_id" {
		t.Fatalf("hop1 cols = %+v, want LeftCol=id RightCol=tickets_id (rel_tags_tickets link column for tickets)", hop1)
	}
	if !hop1.NoSoftDelete || !hop1.NoRowScope {
		t.Fatalf("hop1 = %+v, want NoSoftDelete=true NoRowScope=true (link table has neither column)", hop1)
	}
	if hop1.FromAlias != "" {
		t.Fatalf("hop1.FromAlias = %q, want empty (defaults to overall base alias)", hop1.FromAlias)
	}

	// Hop 2: link → far entity ("tags"). Chains off hop1's alias; carries the SAME tenant + A5
	// row-ACL predicate the FK-on-child hop emits (Scope resolved from authz "owner").
	if hop2.Alias != reportJoinAlias || hop2.RelatedTable != "obj_tags" {
		t.Fatalf("hop2 = %+v, want alias %q / table obj_tags", hop2, reportJoinAlias)
	}
	if hop2.FromAlias != reportJoinLinkAlias {
		t.Fatalf("hop2.FromAlias = %q, want %q (chains off hop1)", hop2.FromAlias, reportJoinLinkAlias)
	}
	// #225: only the far entity's hop names its entity (the link table has no rows to share).
	if hop2.Entity != "tags" || hop1.Entity != "" {
		t.Fatalf("hop entities = (%q, %q), want (\"\", \"tags\")", hop1.Entity, hop2.Entity)
	}
	if hop2.LeftCol != "tags_id" || hop2.RightCol != "id" {
		t.Fatalf("hop2 cols = %+v, want LeftCol=tags_id RightCol=id", hop2)
	}
	if hop2.NoSoftDelete || hop2.NoRowScope {
		t.Fatalf("hop2 = %+v, want normal soft-delete + row-scope predicates (the far entity)", hop2)
	}
	if hop2.Scope.Level != dynrepo.AccessOwner || hop2.Scope.UserID != "u1" {
		t.Fatalf("hop2 scope = %+v, want owner/u1", hop2.Scope)
	}
}

// TestAggregate_JoinedFieldACLRoutesToRelatedEntity is the security-auditor's concrete leak
// scenario: a caller who cannot view employees.salary must not SUM it through a joined report. The
// field-ACL check must be asked about the RELATED entity ("employees"), not the base ("tickets").
func TestAggregate_JoinedFieldACLRoutesToRelatedEntity(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	q := AggregateQuery{Metric: "sum", Field: "assignee.salary", Joins: []AggregateJoin{{Relationship: "assignee"}}}

	t.Run("hidden related field → FIELD_FORBIDDEN, store never reached", func(t *testing.T) {
		store := &rjStore{}
		fACL := &rjFieldACL{hidden: map[string]bool{"employees.salary": true}}
		s := buildRJService(store, &rjAuthz{allow: map[string]bool{"employees": true}}, fACL)
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "FIELD_FORBIDDEN" {
			t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if store.scalarCalled || store.groupedCalled {
			t.Fatalf("store must NOT be reached once field-ACL rejects")
		}
		// Proves routing: the ACL was asked about the RELATED entity, not the base.
		var askedRelated bool
		for _, k := range fACL.asked {
			if k == "employees.salary" {
				askedRelated = true
			}
			if k == "tickets.salary" {
				t.Fatalf("field-ACL asked about BASE entity 'tickets.salary' — routing bug")
			}
		}
		if !askedRelated {
			t.Fatalf("field-ACL was never asked about employees.salary; asked=%v", fACL.asked)
		}
	})

	t.Run("visible related field → passes ACL, store reached", func(t *testing.T) {
		store := &rjStore{}
		fACL := &rjFieldACL{hidden: map[string]bool{}} // nothing hidden
		s := buildRJService(store, &rjAuthz{allow: map[string]bool{"employees": true}}, fACL)
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.scalarCalled {
			t.Fatalf("store.AggregateScalar should be reached once ACL passes")
		}
	})

	t.Run("namespaced filter rejected before SQL", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, &rjAuthz{allow: map[string]bool{"employees": true}}, &rjFieldACL{})
		bad := AggregateQuery{Metric: "count", Joins: []AggregateJoin{{Relationship: "assignee"}},
			Filters: []FilterInput{{Field: "assignee.region"}}}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", bad)
		if codeOf(err) != "REPORT_JOIN_FILTER_UNSUPPORTED" {
			t.Fatalf("code = %q, want REPORT_JOIN_FILTER_UNSUPPORTED", codeOf(err))
		}
		if store.scalarCalled || store.groupedCalled {
			t.Fatalf("store must not be reached when a namespaced filter is rejected")
		}
	})
}

// TestAggregate_NNFanoutGate is the end-to-end (mocked store) proof of the REPORT-JOIN-NN
// allow/reject matrix at the run-time Aggregate() entrypoint — the SQL-level correctness of the
// ALLOWED cases is proven separately by the dynrepo integration tests (the fan-out fixture). N-N
// is SYMMETRIC: sum/avg is rejected REGARDLESS of whether the aggregated field is a base field or
// a "<rel>.<field>" far field (an earlier version of this test wrongly allowed the far-field case
// — see docs/api-conventions.md §19a for the corrected matrix and why).
func TestAggregate_NNFanoutGate(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	authz := &rjAuthz{allow: map[string]bool{"tags": true}}

	t.Run("count over N-N → allowed, store reached", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "count", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.scalarCalled {
			t.Fatalf("store.AggregateScalar should be reached for count over N-N")
		}
	})

	t.Run("sum of FAR field over N-N → 422 AGGREGATE_NN_FANOUT_UNSAFE (symmetric fan-out), store never reached", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "sum", Field: "tagged.weight", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "AGGREGATE_NN_FANOUT_UNSAFE" {
			t.Fatalf("code = %q, want AGGREGATE_NN_FANOUT_UNSAFE (err=%v) — a far row can link to MANY base rows too, so sum-of-far is NOT safe under N-N", codeOf(err), err)
		}
		if store.scalarCalled || store.groupedCalled {
			t.Fatalf("store must NOT be reached once the fan-out gate rejects")
		}
	})

	t.Run("avg of FAR field over N-N → 422 AGGREGATE_NN_FANOUT_UNSAFE", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "avg", Field: "tagged.weight", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "AGGREGATE_NN_FANOUT_UNSAFE" {
			t.Fatalf("code = %q, want AGGREGATE_NN_FANOUT_UNSAFE (err=%v)", codeOf(err), err)
		}
	})

	t.Run("sum of BASE field over N-N → 422 AGGREGATE_NN_FANOUT_UNSAFE, store never reached", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "sum", Field: "priority", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "AGGREGATE_NN_FANOUT_UNSAFE" {
			t.Fatalf("code = %q, want AGGREGATE_NN_FANOUT_UNSAFE (err=%v)", codeOf(err), err)
		}
		if store.scalarCalled || store.groupedCalled {
			t.Fatalf("store must NOT be reached once the fan-out gate rejects")
		}
	})

	t.Run("avg of BASE field over N-N → 422 AGGREGATE_NN_FANOUT_UNSAFE", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "avg", Field: "priority", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		_, err := s.Aggregate(ctx, scope, "t1", "tickets", q)
		if codeOf(err) != "AGGREGATE_NN_FANOUT_UNSAFE" {
			t.Fatalf("code = %q, want AGGREGATE_NN_FANOUT_UNSAFE (err=%v)", codeOf(err), err)
		}
	})

	t.Run("min of BASE field over N-N → allowed (non-additive)", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "min", Field: "priority", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.scalarCalled {
			t.Fatalf("store.AggregateScalar should be reached for min of a base field over N-N")
		}
	})

	t.Run("max of FAR field over N-N → allowed (non-additive)", func(t *testing.T) {
		store := &rjStore{}
		s := buildRJService(store, authz, &rjFieldACL{})
		q := AggregateQuery{Metric: "max", Field: "tagged.weight", Joins: []AggregateJoin{{Relationship: "tagged"}}}
		if _, err := s.Aggregate(ctx, scope, "t1", "tickets", q); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.scalarCalled {
			t.Fatalf("store.AggregateScalar should be reached for max of a far field over N-N")
		}
	})
}
