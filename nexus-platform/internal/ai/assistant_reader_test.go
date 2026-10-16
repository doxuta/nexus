package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// fakeQuerier records the scope RecordService.List/Aggregate/Get would receive from the adapter,
// so the tests can prove the identity fields stay VERBATIM and only AccessLevel is the
// ACL-resolved per-entity level (the missing RequireACL stage, run per tool call).
type fakeQuerier struct {
	gotScope  services.RecordScope
	gotEntity string
	gotGetID  string
	listRes   services.ListResult
	aggRes    services.AggregateResult
	getRec    *dynrepo.Record
	getErr    error
	hidden    map[string]struct{} // field names the caller's role may not view
	hiddenErr error
}

func (f *fakeQuerier) List(_ context.Context, scope services.RecordScope, _, entityName string, _ services.ListQuery) (services.ListResult, error) {
	f.gotScope, f.gotEntity = scope, entityName
	return f.listRes, nil
}

func (f *fakeQuerier) Aggregate(_ context.Context, scope services.RecordScope, _, entityName string, _ services.AggregateQuery) (services.AggregateResult, error) {
	f.gotScope, f.gotEntity = scope, entityName
	return f.aggRes, nil
}

func (f *fakeQuerier) Get(_ context.Context, scope services.RecordScope, _, entityName, id string, _ []string, _ []string) (*dynrepo.Record, error) {
	f.gotScope, f.gotEntity, f.gotGetID = scope, entityName, id
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRec, nil
}

// HiddenFields mirrors RecordService.HiddenFields: admin bypasses (nil set), everyone else gets the
// scripted hidden set or load error.
func (f *fakeQuerier) HiddenFields(_ context.Context, scope services.RecordScope, _, _ string) (map[string]struct{}, error) {
	if scope.IsAdmin {
		return nil, nil
	}
	return f.hidden, f.hiddenErr
}

// fakeACL scripts per-entity view decisions: viewable maps entity → access level; anything absent
// is denied. calls counts Authorize invocations so the admin-bypass test can assert zero.
type fakeACL struct {
	viewable map[string]string
	calls    int
	err      error
}

func (f *fakeACL) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	f.calls++
	if f.err != nil {
		return false, "", f.err
	}
	if action != "view" {
		return false, "", nil
	}
	level, ok := f.viewable[entity]
	return ok, level, nil
}

func (f *fakeACL) CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error) {
	ok, _, err := f.Authorize(ctx, tenantID, roleID, entityName, "view")
	return ok, err
}

// TestAdapter_QueryRecords_ResolvesLevel_IdentityVerbatim — the trap-#1 core: for a non-admin the
// adapter runs the per-entity ACL stage (Authorize "view") and forwards the caller's scope with
// ONLY AccessLevel replaced by the resolved grant; UserID/RoleID/IsAdmin are untouched.
func TestAdapter_QueryRecords_ResolvesLevel_IdentityVerbatim(t *testing.T) {
	q := &fakeQuerier{}
	acl := &fakeACL{viewable: map[string]string{"opportunities": "owner"}}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	// scopeOf on the /assistant/ask route fails closed to "none" — the adapter must upgrade it
	// to the entity's resolved level, not pass "none" through (which would blank every read).
	in := services.RecordScope{UserID: "u-77", AccessLevel: "none", IsAdmin: false, RoleID: "r-9"}
	if _, err := a.QueryRecords(context.Background(), in, "t1", "opportunities", services.ListQuery{}); err != nil {
		t.Fatalf("QueryRecords: %v", err)
	}
	want := services.RecordScope{UserID: "u-77", AccessLevel: "owner", IsAdmin: false, RoleID: "r-9", IncludeShares: true} // #225: ⌘K reads include records shared with the caller
	if q.gotScope != want {
		t.Fatalf("scope handed to RecordService = %+v; want identity verbatim + resolved level %+v", q.gotScope, want)
	}
}

// TestAdapter_QueryRecords_Denied_NoExistenceLeak: no "view" grant → services.ErrEntityNotFound,
// indistinguishable from a nonexistent entity, and the record service is never touched.
func TestAdapter_QueryRecords_Denied_NoExistenceLeak(t *testing.T) {
	q := &fakeQuerier{}
	acl := &fakeACL{viewable: map[string]string{}}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	in := services.RecordScope{UserID: "u-77", AccessLevel: "none", RoleID: "r-9"}
	_, err := a.QueryRecords(context.Background(), in, "t1", "salaries", services.ListQuery{})
	if !errors.Is(err, services.ErrEntityNotFound) {
		t.Fatalf("err = %v; want services.ErrEntityNotFound (no existence leak)", err)
	}
	if q.gotEntity != "" {
		t.Fatal("record service must NOT be called for a denied entity")
	}
}

// TestAdapter_QueryRecords_ACLFault_FailsClosed: an Authorize infrastructure error refuses the
// read (fail closed) instead of widening or defaulting.
func TestAdapter_QueryRecords_ACLFault_FailsClosed(t *testing.T) {
	q := &fakeQuerier{}
	acl := &fakeACL{err: errors.New("db down")}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	_, err := a.QueryRecords(context.Background(), services.RecordScope{UserID: "u", RoleID: "r"}, "t1", "accounts", services.ListQuery{})
	if err == nil {
		t.Fatal("an ACL fault must fail the read closed")
	}
	if q.gotEntity != "" {
		t.Fatal("record service must NOT be called on an ACL fault")
	}
}

// TestAdapter_Admin_BypassesACL_ScopeVerbatim: an admin's scope passes through untouched and the
// ACL decider is never consulted (mirrors the middleware's D3 admin bypass).
func TestAdapter_Admin_BypassesACL_ScopeVerbatim(t *testing.T) {
	q := &fakeQuerier{}
	acl := &fakeACL{viewable: map[string]string{}}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	in := services.RecordScope{UserID: "root", AccessLevel: "none", IsAdmin: true, RoleID: ""}
	if _, err := a.AggregateRecords(context.Background(), in, "t1", "anything", services.AggregateQuery{Metric: services.AggMetricCount}); err != nil {
		t.Fatalf("AggregateRecords: %v", err)
	}
	if q.gotScope != in {
		t.Fatalf("admin scope = %+v; want VERBATIM %+v (IsAdmin short-circuits row scope downstream)", q.gotScope, in)
	}
	if acl.calls != 0 {
		t.Fatalf("Authorize called %d times for an admin; want 0", acl.calls)
	}
}

// TestAdapter_AggregateRecords_ResolvesLevel mirrors the query pin on the aggregate path.
func TestAdapter_AggregateRecords_ResolvesLevel(t *testing.T) {
	q := &fakeQuerier{}
	acl := &fakeACL{viewable: map[string]string{"tickets": "team"}}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	in := services.RecordScope{UserID: "u-1", AccessLevel: "none", RoleID: "r-1"}
	if _, err := a.AggregateRecords(context.Background(), in, "t1", "tickets", services.AggregateQuery{Metric: services.AggMetricCount}); err != nil {
		t.Fatalf("AggregateRecords: %v", err)
	}
	if q.gotScope.AccessLevel != "team" || q.gotScope.UserID != "u-1" || q.gotScope.RoleID != "r-1" || q.gotScope.IsAdmin || !q.gotScope.IncludeShares {
		t.Fatalf("scope = %+v; want resolved level team + identity verbatim", q.gotScope)
	}
}

// TestAdapter_GetRecord_ResolvesLevel_SentinelsPassThrough — the propose_record_update probe path:
// a non-admin's Get runs the same per-entity ACL stage (level resolved, identity verbatim); the
// two not-found sentinels cross the adapter UNWRAPPED so the tool layer can map them
// (ENTITY_NOT_FOUND vs RECORD_NOT_FOUND, both no-leak); a denied entity never touches the record
// service.
func TestAdapter_GetRecord_ResolvesLevel_SentinelsPassThrough(t *testing.T) {
	q := &fakeQuerier{getRec: &dynrepo.Record{ID: "r-1", Data: map[string]any{"status": "open"}}}
	acl := &fakeACL{viewable: map[string]string{"orders": "owner"}}
	a := NewRecordReadAdapter(q, acl, twoTenantReader(), nil)

	in := services.RecordScope{UserID: "u-77", AccessLevel: "none", RoleID: "r-9"}
	rec, err := a.GetRecord(context.Background(), in, "t1", "orders", "r-1")
	if err != nil || rec == nil || rec.ID != "r-1" {
		t.Fatalf("GetRecord = (%+v, %v); want the record", rec, err)
	}
	want := services.RecordScope{UserID: "u-77", AccessLevel: "owner", RoleID: "r-9", IncludeShares: true}
	if q.gotScope != want || q.gotGetID != "r-1" {
		t.Fatalf("record service got scope=%+v id=%q; want resolved level + verbatim identity, id r-1", q.gotScope, q.gotGetID)
	}

	// Absent / row-scope-hidden record: services.ErrRecordNotFound passes through unwrapped.
	q.getErr = services.ErrRecordNotFound
	if _, err := a.GetRecord(context.Background(), in, "t1", "orders", "ghost"); !errors.Is(err, services.ErrRecordNotFound) {
		t.Fatalf("err = %v; want services.ErrRecordNotFound unwrapped", err)
	}

	// Denied entity: ENTITY_NOT_FOUND sentinel, record service never consulted.
	qDeny := &fakeQuerier{}
	aDeny := NewRecordReadAdapter(qDeny, &fakeACL{viewable: map[string]string{}}, twoTenantReader(), nil)
	if _, err := aDeny.GetRecord(context.Background(), in, "t1", "salaries", "r-1"); !errors.Is(err, services.ErrEntityNotFound) {
		t.Fatalf("err = %v; want services.ErrEntityNotFound (no existence leak)", err)
	}
	if qDeny.gotEntity != "" {
		t.Fatal("record service must NOT be called for a denied entity")
	}
}

// TestAdapter_ViewableEntities_ACLFiltered is the trap-#2 pin: an entity whose CanViewEntity says
// false does NOT appear in the assistant's grounding/list — even though the tenant metadata has
// it. Admins see everything.
func TestAdapter_ViewableEntities_ACLFiltered(t *testing.T) {
	meta := &fakeMetaReader{entities: map[string][]metadata.EntityDefinition{
		"t1": {
			{ID: "e1", TenantID: "t1", Name: "accounts", Label: "Khách hàng"},
			{ID: "e2", TenantID: "t1", Name: "salaries", Label: "Lương"},
		},
	}}
	acl := &fakeACL{viewable: map[string]string{"accounts": "all"}} // salaries: NO view grant
	a := NewRecordReadAdapter(&fakeQuerier{}, acl, meta, nil)

	got, err := a.ViewableEntities(context.Background(), services.RecordScope{UserID: "u", RoleID: "r"}, "t1")
	if err != nil {
		t.Fatalf("ViewableEntities: %v", err)
	}
	if len(got) != 1 || got[0].Name != "accounts" {
		t.Fatalf("entities = %+v; want ONLY accounts (salaries is ACL-hidden)", got)
	}

	// Admin: unfiltered, no ACL calls.
	acl2 := &fakeACL{}
	a2 := NewRecordReadAdapter(&fakeQuerier{}, acl2, meta, nil)
	gotAdmin, err := a2.ViewableEntities(context.Background(), services.RecordScope{UserID: "root", IsAdmin: true}, "t1")
	if err != nil || len(gotAdmin) != 2 {
		t.Fatalf("admin entities = %+v (err=%v); want both", gotAdmin, err)
	}
	if acl2.calls != 0 {
		t.Fatalf("admin listing consulted ACL %d times; want 0", acl2.calls)
	}
}

// TestAdapter_Fields_ACLGated: list_fields for a non-viewable entity resolves to the SAME
// not-found sentinel (no metadata leak); a viewable one returns its fields.
func TestAdapter_Fields_ACLGated(t *testing.T) {
	meta := twoTenantReader() // tenant-A has "patients" (ID eA) with one field
	acl := &fakeACL{viewable: map[string]string{"patients": "all"}}
	a := NewRecordReadAdapter(&fakeQuerier{}, acl, meta, nil)

	scope := services.RecordScope{UserID: "u", RoleID: "r"}
	fields, err := a.Fields(context.Background(), scope, "tenant-A", "patients")
	if err != nil || len(fields) != 1 || fields[0].Name != "full_name" {
		t.Fatalf("fields = %+v (err=%v); want patients' full_name", fields, err)
	}

	// Same entity, role WITHOUT the view grant → ENTITY_NOT_FOUND, fields never exposed.
	aclDeny := &fakeACL{viewable: map[string]string{}}
	aDeny := NewRecordReadAdapter(&fakeQuerier{}, aclDeny, meta, nil)
	if _, err := aDeny.Fields(context.Background(), scope, "tenant-A", "patients"); !errors.Is(err, services.ErrEntityNotFound) {
		t.Fatalf("err = %v; want services.ErrEntityNotFound for an ACL-hidden entity", err)
	}

	// Nonexistent entity (wildcard-viewable role) normalizes metadata.ErrEntityNotFound → the
	// one services sentinel the executor maps.
	aclWild := &fakeACL{viewable: map[string]string{"ghosts": "all"}}
	aWild := NewRecordReadAdapter(&fakeQuerier{}, aclWild, meta, nil)
	if _, err := aWild.Fields(context.Background(), scope, "tenant-A", "ghosts"); !errors.Is(err, services.ErrEntityNotFound) {
		t.Fatalf("err = %v; want services.ErrEntityNotFound for a nonexistent entity", err)
	}
}

// TestAssistant_HiddenField_NeverReachesTheModel — queue #143 follow-up (adversarial review
// 2026-09-30): ⌘K read field definitions straight from metadata, gated only per ENTITY. So a role
// with can_view=0 on a field got that field's name/label/type/required from list_fields, and
// propose_record_create answered `field "amount" là bắt buộc` for it — the leak #143 closed on the
// form, "Tạo nhiều dòng" and CSV import, still open on a fourth create surface. Fields() must serve
// the caller exactly the fields a normal read serves them: hidden ones are not described, not
// validated as required, and a hidden name in a draft reads as unknown — the same answer a field
// that does not exist gets.
func TestAssistant_HiddenField_NeverReachesTheModel(t *testing.T) {
	meta := &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{"t1": {{ID: "e1", TenantID: "t1", Name: "opportunities", Label: "Cơ hội"}}},
		fields: map[string]map[string][]metadata.FieldDefinition{"t1": {"e1": {
			{ID: "f1", Name: "name", Label: "Tên", FieldType: "text", IsRequired: true},
			{ID: "f2", Name: "amount", Label: "Giá trị", FieldType: "currency", IsRequired: true},
		}}},
	}
	rep := callerScope()
	admin := services.RecordScope{UserID: "root", IsAdmin: true}
	tests := []struct {
		name      string
		scope     services.RecordScope
		hiddenErr error
		tool      string
		args      string
		wantOK    bool
		wantHas   []string
		wantNot   []string
	}{
		{name: "list_fields omits the hidden field", scope: rep, tool: "list_fields", args: `{"entity":"opportunities"}`,
			wantOK: true, wantHas: []string{`"name"`}, wantNot: []string{"amount", "Giá trị", "currency"}},
		{name: "list_fields for an admin keeps it", scope: admin, tool: "list_fields", args: `{"entity":"opportunities"}`,
			wantOK: true, wantHas: []string{`"amount"`, "Giá trị"}},
		{name: "grant load failure refuses the listing, never serves it unfiltered", scope: rep, hiddenErr: errors.New("db down"),
			tool: "list_fields", args: `{"entity":"opportunities"}`, wantOK: false, wantHas: []string{"LIST_FIELDS_FAILED"}, wantNot: []string{"amount", "db down"}},
		{name: "propose_record_create does not demand the hidden required field", scope: rep, tool: "propose_record_create",
			args: `{"entity":"opportunities","data":{"name":"Hợp đồng A"}}`, wantOK: true, wantNot: []string{"amount"}},
		{name: "a hidden name in a create draft is unknown, like a field that does not exist", scope: rep, tool: "propose_record_create",
			args: `{"entity":"opportunities","data":{"name":"A","amount":5}}`, wantOK: false, wantHas: []string{"RECORD_UNKNOWN_FIELD"}},
		{name: "control: a field that does not exist", scope: rep, tool: "propose_record_create",
			args: `{"entity":"opportunities","data":{"name":"A","ghost":5}}`, wantOK: false, wantHas: []string{"RECORD_UNKNOWN_FIELD"}},
		{name: "a hidden name in an update draft is unknown too", scope: rep, tool: "propose_record_update",
			args: `{"entity":"opportunities","recordId":"r1","data":{"amount":5}}`, wantOK: false, wantHas: []string{"RECORD_UNKNOWN_FIELD"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQuerier{
				hidden:    map[string]struct{}{"amount": {}},
				hiddenErr: tt.hiddenErr,
				getRec:    &dynrepo.Record{ID: "r1", Data: map[string]any{"name": "A"}},
			}
			acl := &fakeACL{viewable: map[string]string{"opportunities": "all"}}
			e := &assistantExecutor{reader: NewRecordReadAdapter(q, acl, meta, nil), tenantID: "t1", scope: tt.scope}
			res := e.execute(context.Background(), tt.tool, tt.args)
			if ok, _ := parseToolResult(t, res); ok != tt.wantOK {
				t.Fatalf("ok = %v; want %v (%s)", ok, tt.wantOK, res)
			}
			for _, s := range tt.wantHas {
				if !strings.Contains(res, s) {
					t.Fatalf("result %s lacks %q", res, s)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(res, s) {
					t.Fatalf("result %s discloses %q", res, s)
				}
			}
		})
	}
}
