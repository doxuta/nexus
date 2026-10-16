//go:build integration

// specs/048 S6 (T108, T109) — the two writes exempt from vetoes (FR-019), on real MySQL through the
// production composition: the approval chain (bpm Requester → ApprovalService.Decide → Decider →
// ApprovalOutcomeWriter → RecordService.Update) and the bank-payment match (InboundHookService →
// RecordService.Update). A rule "only an approval may accept a quote" refuses the hand edit, yet
// the approver's decision lands; the fill rule still applies to it (US8 #4); the veto's history says
// why it did not refuse (skipped, reason = the origin) — and never a value.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'Bypasses.*_Integration' -race -count=1 -v
package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/workflow"
)

const (
	exApprover  = "00000000-0000-0000-0000-0000000000b1"
	exAcceptMsg = "Báo giá chỉ được Chấp nhận qua quy trình duyệt"
	exPaidMsg   = "Chỉ đối soát ngân hàng được ghi Đã thu"
)

func (e hkEnv) createQuotes(t *testing.T, tenant string) {
	t.Helper()
	if _, _, err := e.ent.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name: "hk_quotes", Label: "Quotes",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "code", Label: "Code", FieldType: "text"},
			{Name: "status", Label: "Status", FieldType: "picklist", Options: map[string]any{
				"values": []any{"Nháp", "Chờ duyệt", "Chấp nhận", "Từ chối"}}},
			{Name: "payment", Label: "Payment", FieldType: "picklist", Options: map[string]any{
				"values": []any{"Chưa thu", "Đã thu"}}},
			{Name: "stamp", Label: "Stamp", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("create hk_quotes in %s: %v", tenant, err)
	}
}

// quoteFill switches on a before_save fill on hk_quotes: when field becomes value, stamp = text.
func (e hkEnv) quoteFill(t *testing.T, tenant, field, value, text string) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: "fill " + text, EntityName: "hk_quotes", Trigger: string(workflow.TriggerBeforeSave), SaveOn: workflow.SaveOnUpdate,
		Rules:   []WorkflowRuleInput{{Field: field, Op: workflow.OpChangedTo, Value: value}},
		Actions: []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "stamp", Value: text}}, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create fill rule: %v", err)
	}
	return wf.ID
}

// wantExemptRuns: the veto left one blocked run (the hand edit) and one skipped run naming origin;
// the fill left one success run on the record; not one of them carries a value.
func (e hkEnv) wantExemptRuns(t *testing.T, tenant, vetoID, fillID, recordID, origin string, values ...string) {
	t.Helper()
	byStatus := map[string]workflow.RunRecord{}
	for _, r := range e.drainRuns(t, tenant, vetoID) {
		byStatus[r.Status] = r
	}
	skipped, ok := byStatus[string(workflow.RunSkipped)]
	if len(byStatus) != 2 || !ok || byStatus[string(workflow.RunBlocked)].ID == "" {
		t.Fatalf("veto runs by status = %v, want one blocked (hand edit) + one skipped (%s)", byStatus, origin)
	}
	if skipped.RecordID == nil || *skipped.RecordID != recordID || skipped.TriggerType != string(workflow.TriggerBeforeSave) {
		t.Errorf("skipped run = %+v, want trigger before_save on %s", skipped, recordID)
	}
	var acts []workflow.ActionResult
	if skipped.ActionsResult == nil || json.Unmarshal(*skipped.ActionsResult, &acts) != nil || len(acts) != 1 ||
		acts[0].Type != workflow.ActionVeto || acts[0].Skipped != origin {
		t.Errorf("skipped run actionsResult = %s, want one veto skipped %q", rawOf(skipped), origin)
	}
	fills := e.drainRuns(t, tenant, fillID)
	if len(fills) != 1 || fills[0].Status != string(workflow.RunSuccess) || fills[0].RecordID == nil || *fills[0].RecordID != recordID {
		t.Errorf("fill runs = %+v, want one success on %s", fills, recordID)
	}
	all := rawOf(skipped) + rawOf(byStatus[string(workflow.RunBlocked)])
	for _, r := range fills {
		all += rawOf(r)
	}
	for _, v := range values {
		if strings.Contains(all, v) {
			t.Errorf("a run carries the value %q: %s", v, all)
		}
	}
}

func rawOf(r workflow.RunRecord) string {
	if r.ActionsResult == nil {
		return ""
	}
	return string(*r.ActionsResult)
}

// T108 (US8 #1–#4): the hand edit is refused; the approver's decision is written, filled, and traced.
func TestApprovalOutcome_BypassesVetoAppliesMutations_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createQuotes(t, engTenantA)
	vetoID := env.hkVeto(t, engTenantA, "hk_quotes", workflow.SaveOnUpdate,
		[]WorkflowRuleInput{{Field: "status", Op: workflow.OpChangedTo, Value: "Chấp nhận"}}, "status", exAcceptMsg)
	fillID := env.quoteFill(t, engTenantA, "status", "Chấp nhận", "Đã duyệt")
	id, err := env.rec.Create(ctx, engAdmin(hkUser), engTenantA, hkUser, "hk_quotes", map[string]any{"name": "Báo giá A", "code": "Q-1", "status": "Nháp"})
	if err != nil {
		t.Fatalf("seed quote: %v", err)
	}

	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}
	hkWantVeto(t, env.rec.Update(ctx, rep, engTenantA, "hk_quotes", id, map[string]any{"status": "Chấp nhận"}, nil), exAcceptMsg)
	if got := env.storedIn(t, engTenantA, "hk_quotes", id).Data["status"]; got != "Nháp" {
		t.Fatalf("status after the refused hand edit = %v, want Nháp", got)
	}

	// The approval chain wired as cmd/server wires it; the outcome write-back goes through env.rec.
	repo := bpm.NewSQLRepository(env.sdb, nil)
	decider := bpm.NewDecider(repo, repo, repo, nil, nil).WithRecordWriter(NewApprovalOutcomeWriter(env.rec))
	approval := NewApprovalService(repo, repo, decider).WithEntityChecker(env.meta)
	on := true
	if _, err := approval.CreateProcess(ctx, engTenantA, CreateApprovalProcessInput{
		Name: "Duyệt báo giá", EntityName: "hk_quotes", Trigger: "manual",
		Steps:    []ApprovalStepInput{{Name: "Trưởng nhóm", ApproverType: "user", ApproverID: exApprover}},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"},
		IsActive: &on, ActorID: hkUser,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	inst, err := bpm.NewRequester(repo, repo, repo, repo, nil).Submit(ctx, engTenantA, "hk_quotes", id,
		env.storedIn(t, engTenantA, "hk_quotes", id).Data, bpm.Actor{UserID: hkUser})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	decided, err := approval.Decide(ctx, engTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: exApprover})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != bpm.StatusApproved {
		t.Fatalf("instance = %s, want approved", decided.Status)
	}
	stored := env.storedIn(t, engTenantA, "hk_quotes", id).Data
	if stored["status"] != "Chấp nhận" || stored["stamp"] != "Đã duyệt" {
		t.Fatalf("after approval: status %v stamp %v, want Chấp nhận / Đã duyệt (veto exempt, fill applied)", stored["status"], stored["stamp"])
	}
	env.wantExemptRuns(t, engTenantA, vetoID, fillID, id, originApprovalOutcome, "Chấp nhận", "Đã duyệt", "Báo giá A")
}

// T109: the matched bank payment marks the document paid although a veto guards that edge.
func TestPaymentMatch_BypassesVeto_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createQuotes(t, engTenantA)
	vetoID := env.hkVeto(t, engTenantA, "hk_quotes", workflow.SaveOnUpdate,
		[]WorkflowRuleInput{{Field: "payment", Op: workflow.OpChangedTo, Value: "Đã thu"}}, "payment", exPaidMsg)
	fillID := env.quoteFill(t, engTenantA, "payment", "Đã thu", "Đã đối soát")
	id, err := env.rec.Create(ctx, engAdmin(hkUser), engTenantA, hkUser, "hk_quotes", map[string]any{"name": "Báo giá B", "code": "Q-7", "payment": "Chưa thu"})
	if err != nil {
		t.Fatalf("seed quote: %v", err)
	}

	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}
	hkWantVeto(t, env.rec.Update(ctx, rep, engTenantA, "hk_quotes", id, map[string]any{"payment": "Đã thu"}, nil), exPaidMsg)

	hooks := NewInboundHookService(inboundhook.New(env.gdb), env.rec, ihResolver{}, zap.NewNop())
	status, target, reason := hooks.runPaymentMatch(ctx, &inboundhook.Hook{TenantID: engTenantA, ID: "h-pay", CreatedBy: hkUser,
		PaymentMatch: &inboundhook.PaymentMatch{TargetEntity: "hk_quotes", RefField: "code", RefPattern: `Q-\d+`,
			MemoSource: "desc", StatusField: "payment", StatusValue: "Đã thu"}},
		map[string]any{"desc": "CK Q-7 thanh toan"})
	if status != "matched" || target != id {
		t.Fatalf("payment match = %s on %q (%s), want matched on %s", status, target, reason, id)
	}
	stored := env.storedIn(t, engTenantA, "hk_quotes", id).Data
	if stored["payment"] != "Đã thu" || stored["stamp"] != "Đã đối soát" {
		t.Fatalf("after the match: payment %v stamp %v, want Đã thu / Đã đối soát", stored["payment"], stored["stamp"])
	}
	env.wantExemptRuns(t, engTenantA, vetoID, fillID, id, originPaymentMatch, "Đã thu", "Đã đối soát", "Báo giá B")
}
