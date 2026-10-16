//go:build integration

// specs/048 S3 (T057, T063) — the before_delete veto and the before-rule run history on real MySQL:
// the real workflow repository + CachedLoader + WorkflowService + RunSink (sys_workflow_runs), the
// real dynrepo, a throwaway database per test (o3aSetup, TEST_DATABASE_URL). Proves a refused delete
// leaves the row live with no audit and no after_delete event, bulk delete refuses per row, a rule
// never crosses tenants, switching it off takes effect at once — and that every refusal lands in
// the run history as `blocked` (record id NULL on a create, one summary per batch), never a value.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'TestDelete_BeforeDeleteVeto_Integration|TestInsertRun_BlockedStatus|TestBeforeHooks_WritePathMatrix_Integration' -race -count=1 -v
package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// deleteVeto switches on a before_delete veto rule on hk_deals.
func (e hkEnv) deleteVeto(t *testing.T, tenant string, rules []WorkflowRuleInput, msg string) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: "no delete " + msg, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeDelete),
		Rules: rules, Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Message: msg}}, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create before_delete rule: %v", err)
	}
	return wf.ID
}

// drainRuns closes the env's RunSink — every queued run written — and returns wfID's runs through
// the same repository read the runs API serves (newest first).
func (e hkEnv) drainRuns(t *testing.T, tenant, wfID string) []workflow.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.runs.Close(ctx); err != nil {
		t.Fatalf("drain run sink: %v", err)
	}
	runs, _, err := workflow.NewSQLRepository(e.sdb).ListRuns(ctx, tenant, wfID, 100, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return runs
}

// actionsOf decodes a run's actions_result (MySQL re-serialises JSON, so compare decoded).
func actionsOf(t *testing.T, r workflow.RunRecord) any {
	t.Helper()
	if r.ActionsResult == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(*r.ActionsResult, &v); err != nil {
		t.Fatalf("run %s actions_result %s: %v", r.ID, *r.ActionsResult, err)
	}
	return v
}

func (e hkEnv) deleteAudits() int {
	n := 0
	for _, ev := range e.audit.events {
		if ev.Action == audit.ActionDelete {
			n++
		}
	}
	return n
}

func afterDeletes(eng *captureEngine) int {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	n := 0
	for _, ev := range eng.evts {
		if ev.Trigger == workflow.TriggerAfterDelete {
			n++
		}
	}
	return n
}

func TestDelete_BeforeDeleteVeto_Integration(t *testing.T) {
	env := hkSetup(t)
	eng := &captureEngine{}
	env.rec.WithEngine(eng)
	ctx := context.Background()
	env.createDeals(t, engTenantA)
	env.createDeals(t, engTenantB)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	won := env.seed(t, engTenantA, map[string]any{"name": "Won", "code": "A-W", "stage": "Closed Won"})
	open := env.seed(t, engTenantA, map[string]any{"name": "Open", "code": "A-O", "stage": "Proposal"})
	bWon := env.seed(t, engTenantB, map[string]any{"name": "B Won", "code": "B-W", "stage": "Closed Won"})
	ruleID := env.deleteVeto(t, engTenantA, []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}}, delVetoMsg)

	hkWantVeto(t, env.rec.Delete(ctx, rep, engTenantA, "hk_deals", won), delVetoMsg)
	if got := env.stored(t, engTenantA, won).Data["stage"]; got != "Closed Won" {
		t.Fatalf("refused delete changed the row: stage %v", got)
	}
	if env.deleteAudits() != 0 || afterDeletes(eng) != 0 {
		t.Fatalf("refused delete wrote %d delete audit(s), fired %d after_delete event(s)", env.deleteAudits(), afterDeletes(eng))
	}

	// bulk-delete runs Delete per row: the open deal goes, the won one is refused with the rule's words.
	res, err := env.rec.DeleteBulk(ctx, rep, engTenantA, "hk_deals", []string{won, open})
	if err != nil {
		t.Fatalf("DeleteBulk: %v", err)
	}
	if res.Deleted != 1 || res.Results[0].OK || res.Results[0].Code != "RECORD_HOOK_VETO" || res.Results[0].Error != delVetoMsg || !res.Results[1].OK {
		t.Fatalf("bulk-delete = %+v, want the won deal refused with %q and the open one deleted", res, delVetoMsg)
	}
	if _, err := env.dyn.GetRecord(ctx, "hk_deals", engTenantA, open, dynrepo.RowScope{IsAdmin: true}); !errors.Is(err, dynrepo.ErrRecordNotFound) {
		t.Fatalf("open deal after bulk-delete: %v, want deleted", err)
	}
	env.stored(t, engTenantA, won)

	// Tenant B has no rule: its own won deal deletes.
	if err := env.rec.Delete(ctx, rep, engTenantB, "hk_deals", bWon); err != nil {
		t.Fatalf("tenant B delete judged by tenant A's rule: %v", err)
	}

	// Switching the rule off takes effect on the very next delete (Invalidate, not the 1 h TTL).
	off := false
	if _, err := env.wf.UpdateWorkflow(ctx, engTenantA, ruleID, UpdateWorkflowInput{
		Name: "no delete " + delVetoMsg, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeDelete),
		Rules:    []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}},
		Actions:  []WorkflowActionInput{{Type: workflow.ActionVeto, Message: delVetoMsg}},
		IsActive: &off,
	}); err != nil {
		t.Fatalf("switch rule off: %v", err)
	}
	if err := env.rec.Delete(ctx, rep, engTenantA, "hk_deals", won); err != nil {
		t.Fatalf("delete after the rule was switched off: %v", err)
	}

	// US4 scenario 4: the rule's history shows a blocked run per refusal, pointing at the record.
	runs := env.drainRuns(t, engTenantA, ruleID)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (single delete + bulk row)", len(runs))
	}
	for _, r := range runs {
		want := []any{map[string]any{"type": "veto", "ok": true}}
		if r.Status != "blocked" || r.TriggerType != "before_delete" || r.RecordID == nil || *r.RecordID != won ||
			!jsonEqual(actionsOf(t, r), want) || r.Error != nil {
			t.Fatalf("run = %+v actions %v", r, actionsOf(t, r))
		}
	}
}

// T063: the blocked status round-trips through sys_workflow_runs (ENUM widened by 000071) — a create
// refusal with a NULL record id, an update refusal pointing at its record, an all-or-nothing batch
// as ONE summary run — and none of them carries a value of the record.
func TestInsertRun_BlockedStatus(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createOpps(t, engTenantA)
	ruleID := env.hkVeto(t, engTenantA, "hk_opps", "", wonNeedsDateRules, "close_date", hkMsg)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	_, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_opps", map[string]any{"name": "SECRET-OPP", "stage": "Closed Won"})
	hkWantVeto(t, err, hkMsg)
	id, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_opps", map[string]any{"name": "O1", "stage": "Proposal"})
	if err != nil {
		t.Fatalf("create O1: %v", err)
	}
	hkWantVeto(t, env.rec.Update(ctx, rep, engTenantA, "hk_opps", id, map[string]any{"stage": "Closed Won"}, nil), hkMsg)
	if _, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_opps", []map[string]any{
		{"name": "B1", "stage": "Closed Won"}, {"name": "B2", "stage": "Closed Won"}, {"name": "B3", "stage": "Proposal"},
	}, BulkModeAtomic); err != nil {
		t.Fatalf("atomic bulk: %v", err)
	}

	runs := env.drainRuns(t, engTenantA, ruleID)
	veto := []any{map[string]any{"type": "veto", "field": "close_date", "ok": true}}
	summary := map[string]any{"blocked": float64(2), "applied": float64(0), "failed": float64(0)}
	var createRuns, updateRuns, batchRuns int
	for _, r := range runs {
		raw := ""
		if r.ActionsResult != nil {
			raw = string(*r.ActionsResult)
		}
		if strings.Contains(raw, "SECRET") || strings.Contains(raw, "Closed Won") || r.Status != "blocked" || r.TriggerType != "before_save" {
			t.Fatalf("run = %+v actions %s", r, raw)
		}
		switch got := actionsOf(t, r); {
		case r.RecordID == nil && jsonEqual(got, veto):
			createRuns++
		case r.RecordID != nil && *r.RecordID == id && jsonEqual(got, veto):
			updateRuns++
		case r.RecordID == nil && jsonEqual(got, summary):
			batchRuns++
		default:
			t.Fatalf("unexpected run %+v actions %s", r, raw)
		}
	}
	if createRuns != 1 || updateRuns != 1 || batchRuns != 1 {
		t.Fatalf("create/update/batch runs = %d/%d/%d, want 1/1/1", createRuns, updateRuns, batchRuns)
	}
}

func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
