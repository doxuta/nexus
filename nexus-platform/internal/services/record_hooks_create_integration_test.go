//go:build integration

// specs/048 S2 (T044, T046) — the before_save veto on every create path, on real MySQL: the real
// workflow repository + CachedLoader + WorkflowService, the real dynrepo (atomic bulk insert,
// sys_sequences) and ImportService, a throwaway database per test (o3aSetup, TEST_DATABASE_URL).
// Proves a refused create — single, all-or-nothing batch, best-effort batch, CSV row — writes no row
// and claims no auto_number: the codes of everything that WAS created run without a gap.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'TestCreate_BeforeSaveVeto_NoSequenceBurn|TestCreateBulk_Atomic_BeforeSaveVetoAbortsWholeBatch|TestBeforeHooks_WritePathMatrix_Integration' -race -count=1 -v
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/workflow"
)

// createOpps registers hk_opps: the two fields the US3 rule reads and an OPP- auto_number.
func (e hkEnv) createOpps(t *testing.T, tenant string) {
	t.Helper()
	if _, _, err := e.ent.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name: "hk_opps", Label: "Opps",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "stage", Label: "Stage", FieldType: "picklist", Options: map[string]any{"values": []any{"Proposal", "Closed Won"}}},
			{Name: "close_date", Label: "Close", FieldType: "date"},
			{Name: "opp_code", Label: "Code", FieldType: "auto_number", Options: map[string]any{"prefix": "OPP-", "padding": float64(5)}},
		},
	}); err != nil {
		t.Fatalf("create hk_opps in %s: %v", tenant, err)
	}
}

// hkVeto switches on a before_save veto rule on entity (saveOn "" = create and update).
func (e hkEnv) hkVeto(t *testing.T, tenant, entity, saveOn string, rules []WorkflowRuleInput, field, msg string) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: "veto " + msg + " " + saveOn, EntityName: entity, Trigger: string(workflow.TriggerBeforeSave), SaveOn: saveOn,
		Rules: rules, Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Field: field, Message: msg}}, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create veto rule: %v", err)
	}
	return wf.ID
}

var wonNeedsDateRules = []WorkflowRuleInput{
	{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"},
	{Field: "close_date", Op: workflow.OpIsNull},
}

func (e hkEnv) importer() *ImportService {
	return NewImportService(e.meta, e.dyn, zap.NewNop()).WithSequenceClaimer(e.dyn).WithRelationChecker(e.dyn).WithBeforeHooks(e.cache, e.runs)
}

func (e hkEnv) importCSV(t *testing.T, tenant, entity, csv string, scope RecordScope) *ImportResult {
	t.Helper()
	header := strings.SplitN(csv, "\n", 2)[0]
	mapping := map[string]string{}
	for _, col := range strings.Split(header, ",") {
		mapping[col] = col
	}
	res, err := e.importer().Import(context.Background(), ImportInput{
		TenantID: tenant, UserID: scope.UserID, RoleID: scope.RoleID, IsAdmin: scope.IsAdmin, EntityName: entity,
		Mapping: mapping, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

// storedCodes returns the opp_code of every live hk_opps row of tenant, sorted, and their names.
func (e hkEnv) storedOpps(t *testing.T, tenant string) (codes, names []string) {
	t.Helper()
	rows := []struct{ Code, Name string }{}
	if err := e.sdb.Select(&rows, `SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.opp_code')) AS code,
		JSON_UNQUOTE(JSON_EXTRACT(data, '$.name')) AS name FROM obj_hk_opps WHERE tenant_id = ? AND deleted_at IS NULL`, tenant); err != nil {
		t.Fatalf("read hk_opps: %v", err)
	}
	for _, r := range rows {
		codes = append(codes, r.Code)
		names = append(names, r.Name)
	}
	sort.Strings(codes)
	sort.Strings(names)
	return codes, names
}

func TestCreate_BeforeSaveVeto_NoSequenceBurn(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createOpps(t, engTenantA)
	env.hkVeto(t, engTenantA, "hk_opps", "", wonNeedsDateRules, "close_date", hkMsg)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}
	create := func(name, stage string) error {
		_, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_opps", map[string]any{"name": name, "stage": stage})
		return err
	}

	if err := create("O1", "Proposal"); err != nil {
		t.Fatalf("O1: %v", err)
	}
	hkWantVeto(t, create("O2 refused", "Closed Won"), hkMsg)

	atomic, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_opps", []map[string]any{
		{"name": "A1 aborted", "stage": "Proposal"}, {"name": "A2 refused", "stage": "Closed Won"}, {"name": "A3 aborted", "stage": "Proposal"},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("atomic bulk: %v", err)
	}
	if atomic.Created != 0 || atomic.Results[1].Code != "RECORD_HOOK_VETO" || atomic.Results[1].Error != hkMsg {
		t.Fatalf("atomic bulk = %+v, want nothing created and row 1 refused", atomic)
	}

	partial, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_opps", []map[string]any{
		{"name": "P1", "stage": "Proposal"}, {"name": "P2 refused", "stage": "Closed Won"}, {"name": "P3", "stage": "Closed Won", "close_date": "2026-10-01"},
	}, BulkModePartial)
	if err != nil {
		t.Fatalf("partial bulk: %v", err)
	}
	if partial.Created != 2 || partial.Results[1].Code != "RECORD_HOOK_VETO" {
		t.Fatalf("partial bulk = %+v, want rows 0 and 2 created, row 1 refused", partial)
	}

	res := env.importCSV(t, engTenantA, "hk_opps", "name,stage,close_date\nI1,Proposal,\nI2 refused,Closed Won,\nI3,Closed Won,2026-10-02\n", rep)
	want := ImportRowError{Row: 2, Field: "close_date", Code: "RECORD_HOOK_VETO", Message: hkMsg}
	if res.Inserted != 2 || len(res.Errors) != 1 || res.Errors[0] != want {
		t.Fatalf("import = %+v, want 2 inserted and %+v", res, want)
	}

	if err := create("O3", "Proposal"); err != nil {
		t.Fatalf("O3: %v", err)
	}

	codes, names := env.storedOpps(t, engTenantA)
	wantCodes := []string{"OPP-00001", "OPP-00002", "OPP-00003", "OPP-00004", "OPP-00005", "OPP-00006"}
	if fmt.Sprint(codes) != fmt.Sprint(wantCodes) {
		t.Fatalf("stored codes = %v, want %v (a refused create burned a number)", codes, wantCodes)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"I1", "I3", "O1", "O3", "P1", "P3"}) {
		t.Fatalf("stored rows = %v, want only the six allowed creates", names)
	}

	// Tenant B has no rule: the same refused create saves there.
	env.createOpps(t, engTenantB)
	if _, err := env.rec.Create(ctx, rep, engTenantB, hkUser, "hk_opps", map[string]any{"name": "B1", "stage": "Closed Won"}); err != nil {
		t.Fatalf("tenant B create judged by tenant A's rule: %v", err)
	}
}

// T046: one refused row aborts the all-or-nothing batch — no row, no number — and a violation
// anchored on a field the caller's role cannot see (real sys_field_acl, can_view=0) loses its anchor.
func TestCreateBulk_Atomic_BeforeSaveVetoAbortsWholeBatch(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	flsSeedTenant(t, env.sdb, engTenantA, "hk-bulk")
	const roleID = "00000000-0000-0000-0000-0000000000b1"
	flsSeedRole(t, env.sdb, roleID, engTenantA, "hk_rep")
	env.createOpps(t, engTenantA)
	env.hkVeto(t, engTenantA, "hk_opps", workflow.SaveOnCreate, wonNeedsDateRules, "close_date", hkMsg)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: roleID}
	batch := func() []map[string]any {
		return []map[string]any{{"name": "A1", "stage": "Proposal"}, {"name": "A2", "stage": "Closed Won"}, {"name": "A3", "stage": "Closed Won"}}
	}

	res, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_opps", batch(), BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Created != 0 || res.Failed != 2 || res.Skipped != 1 {
		t.Fatalf("created/failed/skipped = %d/%d/%d, want 0/2/1", res.Created, res.Failed, res.Skipped)
	}
	for _, i := range []int{1, 2} {
		if r := res.Results[i]; r.Index != i || r.Code != "RECORD_HOOK_VETO" || r.Error != hkMsg || r.Details["field"] != "close_date" {
			t.Fatalf("row %d = %+v, want RECORD_HOOK_VETO anchored on close_date", i, r)
		}
	}
	if codes, _ := env.storedOpps(t, engTenantA); len(codes) != 0 {
		t.Fatalf("refused batch wrote %v", codes)
	}

	t.Run("hidden anchor dropped for a role that cannot see the field", func(t *testing.T) {
		flsSeedFieldGrant(t, env.sdb, engTenantA, roleID, "hk_opps", "close_date", false, false)
		env.rec.WithFieldACL(NewFieldACLService(auth.NewFieldACLRepository(env.gdb)))
		res, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_opps", batch(), BulkModeAtomic)
		if err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		for _, i := range []int{1, 2} {
			r := res.Results[i]
			vs, _ := r.Details["violations"].([]workflow.Violation)
			if _, has := r.Details["field"]; has || len(vs) != 1 || vs[0].Field != "" || r.Error != hkMsg {
				t.Fatalf("row %d = %+v, want the rule's words without the hidden anchor", i, r)
			}
		}
	})

	// Nothing refused, nothing burned: the first batch that goes through starts at OPP-00001.
	ok, err := env.rec.CreateBulk(ctx, engAdmin(hkUser), engTenantA, hkUser, "hk_opps", []map[string]any{
		{"name": "B1", "stage": "Proposal"}, {"name": "B2", "stage": "Closed Won", "close_date": "2026-10-01"},
	}, BulkModeAtomic)
	if err != nil || ok.Created != 2 {
		t.Fatalf("compliant batch = %+v, %v", ok, err)
	}
	if codes, _ := env.storedOpps(t, engTenantA); fmt.Sprint(codes) != fmt.Sprint([]string{"OPP-00001", "OPP-00002"}) {
		t.Fatalf("codes = %v, want OPP-00001 and OPP-00002 (refused batches claim nothing)", codes)
	}
}

// createPathRow is one create-side row of TestBeforeHooks_WritePathMatrix_Integration.
type createPathRow struct {
	path  string
	name  string // the record the path tries to create; none may exist afterwards
	write func() error
}

// createPathRows lists every user-reachable create path (plan §"Ma trận đường ghi"), each writing a
// record named LOCKED-* that the matrix's create-only rule refuses with the matrix's own message.
func createPathRows(t *testing.T, env hkEnv, hooks *InboundHookService, secret string, rep RecordScope) []createPathRow {
	ctx := context.Background()
	rowErr := func(code, msg string) error { return &ValidationError{Code: code, Msg: msg} }
	bulk := func(name string, mode BulkMode) error {
		res, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_deals", []map[string]any{{"name": name, "code": "HK-" + name}}, mode)
		if err != nil {
			return err
		}
		if r := res.Results[0]; r.Status != "created" {
			return rowErr(r.Code, r.Error)
		}
		return nil
	}
	return []createPathRow{
		{"POST /:entity (Create)", "LOCKED-C", func() error {
			_, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_deals", map[string]any{"name": "LOCKED-C", "code": "HK-LC"})
			return err
		}},
		{"POST /:entity/bulk mode=partial (via Create)", "LOCKED-P", func() error { return bulk("LOCKED-P", BulkModePartial) }},
		{"POST /:entity/bulk mode=atomic (createBulkAtomic)", "LOCKED-A", func() error { return bulk("LOCKED-A", BulkModeAtomic) }},
		{"POST /:entity/import (Import)", "LOCKED-I", func() error {
			res := env.importCSV(t, engTenantA, "hk_deals", "name,code\nLOCKED-I,HK-LI\n", rep)
			if len(res.Errors) > 0 {
				return rowErr(res.Errors[0].Code, res.Errors[0].Message)
			}
			return nil
		}},
		{"inbound hook create (Receive, no match)", "LOCKED-N", func() error {
			body := []byte(`{"code":"HK-LN","name":"LOCKED-N"}`)
			_, err := hooks.Receive(ctx, hkSlug, "hk-in", ihSign(secret, body), body)
			return err
		}},
		{"create_record action of an after rule (WorkflowRecordCreator)", "LOCKED-W", func() error {
			_, err := NewWorkflowRecordCreator(env.rec).CreateRecord(ctx, engTenantA, "hk_deals", hkUser, map[string]any{"name": "LOCKED-W", "code": "HK-LW"})
			return err
		}},
	}
}

// countNamed counts the live hk_deals rows of tenant A named name (-1 when the read fails, which
// every caller treats as "written", so a broken read can only fail the test).
func (e hkEnv) countNamed(name string) int {
	var n int
	if err := e.sdb.Get(&n, `SELECT COUNT(*) FROM obj_hk_deals WHERE tenant_id = ? AND deleted_at IS NULL
		AND JSON_UNQUOTE(JSON_EXTRACT(data, '$.name')) = ?`, engTenantA, name); err != nil {
		return -1
	}
	return n
}
