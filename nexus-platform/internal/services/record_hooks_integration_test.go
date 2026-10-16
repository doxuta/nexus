//go:build integration

// specs/048 S1b (T034) — the before_save veto on real MySQL: the real workflow repository +
// CachedLoader + WorkflowService (the same wiring cmd/server uses), the real dynrepo, a throwaway
// database per test (o3aSetup, TEST_DATABASE_URL). Proves a vetoed PUT leaves the row, its version
// and the audit trail untouched; a rule never crosses tenants; switching a rule off takes effect on
// the very next save; and — the write-path matrix — which user-facing write paths are judged
// (every later BE slice adds its rows; a path that is knowingly NOT judged is pinned as such so
// the named debt fails loudly the day somebody changes it).
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'TestRecordUpdate_BeforeSaveVeto_Integration|TestBeforeHooks_WritePathMatrix_Integration' -race -count=1 -v
package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

const (
	hkUser  = "00000000-0000-0000-0000-0000000000a1"
	hkOwner = "00000000-0000-0000-0000-0000000000a2"
	hkSlug  = "hk-lab"
	hkMsg   = "Chốt Thắng phải có Ngày chốt"
)

type hkEnv struct {
	gdb   *gorm.DB
	sdb   *sqlx.DB
	dyn   *dynrepo.Repository
	meta  *metadata.Repository
	cache *workflow.CachedLoader
	runs  *workflow.RunSink
	rec   *RecordService
	wf    *WorkflowService
	ent   *EntityService
	audit *spyAuditor
}

func hkSetup(t *testing.T) hkEnv {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sdb)
	wfRepo := workflow.NewSQLRepository(sdb)
	// An hour-long TTL: only WorkflowService's Invalidate can make a rule edit visible in this test,
	// so "switched off ⇒ the next save passes" proves the invalidation wiring, not the TTL.
	cache := workflow.NewCachedLoader(wfRepo, time.Hour)
	spy := &spyAuditor{}
	// S3: the run history goes through the real RunSink into sys_workflow_runs, as in cmd/server.
	runs := workflow.NewRunSink(wfRepo, zap.NewNop())
	t.Cleanup(func() {
		if err := runs.Close(context.Background()); err != nil {
			t.Errorf("drain run sink: %v", err)
		}
	})
	return hkEnv{
		gdb:   gdb,
		sdb:   sdb,
		dyn:   dyn,
		meta:  metaRepo,
		cache: cache,
		runs:  runs,
		// S2: the create paths need the auto_number claimer and the atomic bulk inserter, wired
		// exactly as cmd/server does.
		rec:   NewRecordService(metaRepo, dyn).WithBeforeHooks(cache, runs).WithAuditor(spy).WithSequenceClaimer(dyn).WithBulkInserter(dyn),
		wf:    NewWorkflowService(wfRepo).WithEntityChecker(metaRepo).WithCache(cache),
		ent:   NewEntityService(dyn, metaRepo),
		audit: spy,
	}
}

func (e hkEnv) createDeals(t *testing.T, tenant string) {
	t.Helper()
	if _, _, err := e.ent.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name: "hk_deals", Label: "Deals",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
			{Name: "stage", Label: "Stage", FieldType: "picklist", Options: map[string]any{
				"values": []any{"Proposal", "Closed Won", "Locked"}}},
			{Name: "close_date", Label: "Close", FieldType: "date"},
			{Name: "docs", Label: "Docs", FieldType: "file"},
		},
	}); err != nil {
		t.Fatalf("create hk_deals in %s: %v", tenant, err)
	}
}

func (e hkEnv) seed(t *testing.T, tenant string, data map[string]any) string {
	t.Helper()
	id, err := e.rec.Create(context.Background(), engAdmin(hkUser), tenant, hkUser, "hk_deals", data)
	if err != nil {
		t.Fatalf("seed %v: %v", data, err)
	}
	return id
}

func (e hkEnv) stored(t *testing.T, tenant, id string) *dynrepo.Record {
	t.Helper()
	rec, err := e.dyn.GetRecord(context.Background(), "hk_deals", tenant, id, dynrepo.RowScope{IsAdmin: true})
	if err != nil {
		t.Fatalf("read back %s: %v", id, err)
	}
	return rec
}

func (e hkEnv) vetoRule(t *testing.T, tenant string, rules []WorkflowRuleInput, field, msg string) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: "veto " + msg, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeSave), SaveOn: workflow.SaveOnUpdate,
		Rules: rules, Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Field: field, Message: msg}}, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create veto rule: %v", err)
	}
	return wf.ID
}

func hkWantVeto(t *testing.T, err error, msg string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_HOOK_VETO" || ve.Msg != msg {
		t.Fatalf("err = %v, want RECORD_HOOK_VETO %q", err, msg)
	}
}

func (e hkEnv) updateAudits() int {
	n := 0
	for _, ev := range e.audit.events {
		if ev.Action == audit.ActionUpdate {
			n++
		}
	}
	return n
}

func TestRecordUpdate_BeforeSaveVeto_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createDeals(t, engTenantA)
	env.createDeals(t, engTenantB)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	a1 := env.seed(t, engTenantA, map[string]any{"name": "A1", "code": "A-1", "stage": "Proposal"})
	a2 := env.seed(t, engTenantA, map[string]any{"name": "A2", "code": "A-2", "stage": "Proposal", "close_date": "2026-10-01"})
	b1 := env.seed(t, engTenantB, map[string]any{"name": "B1", "code": "B-1", "stage": "Proposal"})
	ruleID := env.vetoRule(t, engTenantA, []WorkflowRuleInput{
		{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"},
		{Field: "close_date", Op: workflow.OpIsNull},
	}, "close_date", hkMsg)

	before := env.stored(t, engTenantA, a1)
	auditsBefore := env.updateAudits()
	hkWantVeto(t, env.rec.Update(ctx, rep, engTenantA, "hk_deals", a1, map[string]any{"stage": "Closed Won"}, nil), hkMsg)
	after := env.stored(t, engTenantA, a1)
	if after.Data["stage"] != "Proposal" || after.Version != before.Version {
		t.Fatalf("vetoed PUT changed the row: stage %v version %d → %d", after.Data["stage"], before.Version, after.Version)
	}
	if env.updateAudits() != auditsBefore {
		t.Fatal("vetoed PUT wrote an audit row")
	}

	// Tenant B has no rule: the same change on its own record saves.
	if err := env.rec.Update(ctx, rep, engTenantB, "hk_deals", b1, map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("tenant B save judged by tenant A's rule: %v", err)
	}

	// bulk-update runs Update per row: the dated deal saves, the undated one is refused with the rule's words.
	res, err := env.rec.UpdateBulk(ctx, rep, engTenantA, "hk_deals", []string{a1, a2}, map[string]any{"stage": "Closed Won"})
	if err != nil {
		t.Fatalf("UpdateBulk: %v", err)
	}
	outcome := map[string]BulkOpRowResult{}
	for _, r := range res.Results {
		outcome[r.ID] = r
	}
	if !outcome[a2].OK || outcome[a1].OK || outcome[a1].Code != "RECORD_HOOK_VETO" || outcome[a1].Error != hkMsg {
		t.Fatalf("bulk-update outcomes = %+v, want a2 ok and a1 RECORD_HOOK_VETO %q", res.Results, hkMsg)
	}
	if got := env.stored(t, engTenantA, a2).Data["stage"]; got != "Closed Won" {
		t.Fatalf("a2 stage = %v, want Closed Won", got)
	}

	// Switching the rule off takes effect on the very next save (Invalidate, not the 1 h TTL).
	off := false
	if _, err := env.wf.UpdateWorkflow(ctx, engTenantA, ruleID, UpdateWorkflowInput{
		Name: "veto " + hkMsg, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeSave), SaveOn: workflow.SaveOnUpdate,
		Rules: []WorkflowRuleInput{
			{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"},
			{Field: "close_date", Op: workflow.OpIsNull},
		},
		Actions:  []WorkflowActionInput{{Type: workflow.ActionVeto, Field: "close_date", Message: hkMsg}},
		IsActive: &off,
	}); err != nil {
		t.Fatalf("switch rule off: %v", err)
	}
	if err := env.rec.Update(ctx, rep, engTenantA, "hk_deals", a1, map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("save after the rule was switched off: %v", err)
	}
}

// TestBeforeHooks_WritePathMatrix_Integration pins, per user-reachable write path, whether a
// before_save veto judges it (plan §"Ma trận đường ghi"). The record is Locked BEFORE the rule is
// created, so any judged save of it — whatever the payload — is refused. S2/S3 add their rows.
func TestBeforeHooks_WritePathMatrix_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createDeals(t, engTenantA)
	env.sdb.MustExec("INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)", engTenantA, hkSlug, "HK")
	env.sdb.MustExec("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) VALUES (?, ?, ?, 'x', 'Owner', 'active', 0)",
		hkOwner, engTenantA, "owner@hk.vn")
	id := env.seed(t, engTenantA, map[string]any{"name": "Hồ sơ", "code": "HK-1", "stage": "Locked"})
	const locked = "Hồ sơ đã khoá, không sửa được"
	env.vetoRule(t, engTenantA, []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Locked"}}, "", locked)

	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}
	hooks := NewInboundHookService(inboundhook.New(env.gdb), env.rec, ihResolver{tenants: map[string]string{hkSlug: engTenantA}}, zap.NewNop())
	inbound, err := hooks.Create(ctx, engTenantA, hkUser, InboundHookInput{
		Name: "hk-in", EntityName: "hk_deals", MatchField: "code",
		Mapping: []inboundhook.Mapping{{Source: "code", Field: "code"}, {Source: "name", Field: "name"}},
	})
	if err != nil {
		t.Fatalf("create inbound hook: %v", err)
	}

	tests := []struct {
		path    string
		judged  bool // false = knowingly NOT judged in v1 (spec "Nợ còn mở" 1/2) or exempt (FR-019)
		write   func() error
		changed func(*dynrepo.Record) bool
	}{
		{"PUT /:entity/:id (Update)", true,
			func() error {
				return env.rec.Update(ctx, rep, engTenantA, "hk_deals", id, map[string]any{"name": "PUT"}, nil)
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "PUT" }},
		{"POST /:entity/bulk-update (UpdateBulk)", true,
			func() error {
				res, err := env.rec.UpdateBulk(ctx, rep, engTenantA, "hk_deals", []string{id}, map[string]any{"name": "BULK"})
				if err != nil {
					return err
				}
				if r := res.Results[0]; !r.OK {
					return &ValidationError{Code: r.Code, Msg: r.Error}
				}
				return nil
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "BULK" }},
		{"file upload (SetFileRefs)", true,
			func() error {
				return env.rec.SetFileRefs(ctx, rep, engTenantA, "hk_deals", id, "docs",
					[]map[string]any{{"id": "f-1", "name": "a.pdf"}})
			},
			func(r *dynrepo.Record) bool { return r.Data["docs"] != nil }},
		{"inbound hook upsert (Receive)", true,
			func() error {
				body := []byte(`{"code":"HK-1","name":"INBOUND"}`)
				_, err := hooks.Receive(ctx, hkSlug, "hk-in", ihSign(inbound.Secret, body), body)
				return err
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "INBOUND" }},
		{"approval outcome write-back (exempt, FR-019)", false,
			func() error {
				return NewApprovalOutcomeWriter(env.rec).SetField(ctx, engTenantA, "hk_deals", id, "name", "APPROVED", hkUser)
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "APPROVED" }},
		{"payment match status write (exempt, FR-019)", false,
			func() error {
				status, _, reason := hooks.runPaymentMatch(ctx, &inboundhook.Hook{TenantID: engTenantA, ID: "h-pay", CreatedBy: hkUser,
					PaymentMatch: &inboundhook.PaymentMatch{TargetEntity: "hk_deals", RefField: "code", RefPattern: `HK-\d+`,
						MemoSource: "desc", StatusField: "name", StatusValue: "PAID"}},
					map[string]any{"desc": "CK HK-1"})
				if status != "matched" {
					return errors.New(reason)
				}
				return nil
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "PAID" }},
		{"after-rule field write (WorkflowRecordMutator — debt, spec Nợ 1)", false,
			func() error {
				return NewWorkflowRecordMutator(env.dyn).UpdateField(ctx, "hk_deals", engTenantA, id, hkUser, "name", "MACHINE")
			},
			func(r *dynrepo.Record) bool { return r.Data["name"] == "MACHINE" }},
		{"owner change (SetOwner — debt, spec Nợ 1)", false,
			func() error { return env.rec.SetOwner(ctx, engAdmin(hkUser), engTenantA, "hk_deals", id, hkOwner) },
			func(r *dynrepo.Record) bool { return r.OwnerID == hkOwner }},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := tt.write()
			changed := tt.changed(env.stored(t, engTenantA, id))
			if tt.judged {
				hkWantVeto(t, err, locked)
				if changed {
					t.Fatal("a vetoed write reached the row")
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("unjudged path: err %v, changed %v — if this path is now judged, move it to judged and close its debt", err, changed)
			}
		})
	}

	// S2: every create path. A create-only rule refuses any new record named LOCKED-*; each path
	// writes one and must leave no row behind.
	env.hkVeto(t, engTenantA, "hk_deals", workflow.SaveOnCreate,
		[]WorkflowRuleInput{{Field: "name", Op: workflow.OpContains, Value: "LOCKED"}}, "", locked)
	for _, tt := range createPathRows(t, env, hooks, inbound.Secret, rep) {
		t.Run(tt.path, func(t *testing.T) {
			hkWantVeto(t, tt.write(), locked)
			if n := env.countNamed(tt.name); n != 0 {
				t.Fatalf("a vetoed create left %d row(s) named %s (-1 = unreadable)", n, tt.name)
			}
		})
	}

	// S3: every delete path. A before_delete rule refuses deleting the Locked record; each path must
	// leave it live (env.stored fails the test on a deleted row).
	env.deleteVeto(t, engTenantA, []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Locked"}}, locked)
	for _, tt := range []struct {
		path  string
		write func() error
	}{
		{"DELETE /:entity/:id (Delete)", func() error { return env.rec.Delete(ctx, rep, engTenantA, "hk_deals", id) }},
		{"POST /:entity/bulk-delete (DeleteBulk)", func() error {
			res, err := env.rec.DeleteBulk(ctx, rep, engTenantA, "hk_deals", []string{id})
			if err != nil {
				return err
			}
			if r := res.Results[0]; !r.OK {
				return &ValidationError{Code: r.Code, Msg: r.Error}
			}
			return nil
		}},
	} {
		t.Run(tt.path, func(t *testing.T) {
			hkWantVeto(t, tt.write(), locked)
			env.stored(t, engTenantA, id)
		})
	}
}
