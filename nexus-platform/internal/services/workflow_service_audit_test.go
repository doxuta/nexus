package services

// specs/062-customer-workspace L4 T063 — create / update / pause / delete of an automation rule leaves
// a __workflow_rule__ audit row (actor from ctx, diff before/after), a write that only reached the
// builder's draft leaves none (FR-022), and NO signing key / auth header ever reaches the diff.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/workflow"
)

const (
	sentinelSecret = "SENTINEL-SECRET"
	sentinelHeader = "SENTINEL-H"
	sentinelQuery  = "SENTINEL-QUERY"
)

func wfAuditCtx() context.Context { return audit.WithActor(context.Background(), "builder-1") }

func wfAuditSvc() (*WorkflowService, *fakeWFRepo, *captureAuditor) {
	repo := newFakeWFRepo()
	aud := &captureAuditor{}
	return NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithAuditor(aud, nil), repo, aud
}

// assertNoSentinel fails when the serialized events carry any of the planted secrets.
func assertNoSentinel(t *testing.T, events []audit.Event, secrets ...string) string {
	t.Helper()
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			t.Fatalf("audit events leak %q: %s", s, b)
		}
	}
	return string(b)
}

func TestWorkflowService_WritesEmitRuleAudit(t *testing.T) {
	svc, _, aud := wfAuditSvc()
	ctx := wfAuditCtx()

	created, err := svc.CreateWorkflow(ctx, "t1", w6CreateInput("stage"))
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	upd := UpdateWorkflowInput{Name: "rule", EntityName: "deals", Trigger: "after_update",
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "y"}}}
	if _, err := svc.UpdateWorkflow(ctx, "t1", created.ID, upd); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	if _, err := svc.SetWorkflowActive(ctx, "t1", created.ID, false, "builder-1"); err != nil {
		t.Fatalf("SetWorkflowActive: %v", err)
	}
	if err := svc.DeleteWorkflow(ctx, "t1", created.ID); err != nil {
		t.Fatalf("DeleteWorkflow: %v", err)
	}

	wantActions := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(wantActions) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(wantActions), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.WorkflowRuleMeta || e.Action != wantActions[i] {
			t.Errorf("event %d = %s/%s; want %s/%s", i, e.EntityName, e.Action, audit.WorkflowRuleMeta, wantActions[i])
		}
		if e.TenantID != "t1" || e.ActorUserID != "builder-1" || e.RecordID != created.ID {
			t.Errorf("event %d tenant/actor/record = %q/%q/%q", i, e.TenantID, e.ActorUserID, e.RecordID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	if aud.events[0].Diff.Before != nil || aud.events[0].Diff.After["name"] != "rule" {
		t.Errorf("create diff = %+v; want after-only carrying the name", aud.events[0].Diff)
	}
	if aud.events[1].Diff.Before["actions"] == nil || aud.events[1].Diff.After["actions"] == nil {
		t.Errorf("update diff = %+v; want both sides of the changed actions", aud.events[1].Diff)
	}
	if aud.events[2].Diff.After["isActive"] != false || aud.events[2].Diff.Before["isActive"] != true {
		t.Errorf("pause diff = %+v; want isActive true -> false", aud.events[2].Diff)
	}
	if aud.events[3].Diff.After != nil || aud.events[3].Diff.Before["name"] != "rule" {
		t.Errorf("delete diff = %+v; want before-only", aud.events[3].Diff)
	}
}

// A failed write leaves no row.
func TestWorkflowService_RejectedWriteEmitsNothing(t *testing.T) {
	svc, _, aud := wfAuditSvc()
	if _, err := svc.CreateWorkflow(wfAuditCtx(), "t1", w6CreateInput("total")); err == nil { // formula target: WF-01
		t.Fatal("want a validation error")
	}
	if err := svc.DeleteWorkflow(wfAuditCtx(), "t1", "ghost"); err == nil {
		t.Fatal("want WORKFLOW_NOT_FOUND")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after rejected writes = %d; want 0: %+v", len(aud.events), aud.events)
	}
}

// legacyKeyedRule seeds a rule saved before S3a: its webhook action still holds an inline signing key,
// auth headers and a token in the URL query — the only way such a row can exist today.
func legacyKeyedRule(repo *fakeWFRepo, id string) string {
	repo.rows[id] = &workflow.WorkflowRecord{
		ID: id, TenantID: "t1", Name: "notify partner", EntityName: "deals", Trigger: "after_update", IsActive: true,
		Actions: json.RawMessage(`[{"type":"outbound_webhook","url":"https://partner.example/hook?token=` + sentinelQuery +
			`","secret":"` + sentinelSecret + `","headers":{"Authorization":"` + sentinelHeader + `"}}]`),
	}
	return id
}

// SENTINEL (L4 mutation-prove: remove the redaction and this goes red): the diff of every write that
// touches a legacy keyed webhook rule carries "[redacted]" and none of the planted values.
func TestWorkflowService_AuditDiffNeverCarriesWebhookSecrets(t *testing.T) {
	svc, repo, aud := wfAuditSvc()
	id := legacyKeyedRule(repo, "legacy-1")
	ctx := wfAuditCtx()

	// Edit that moves the webhook to a new URL (the S2a guard lets a changed URL through).
	upd := UpdateWorkflowInput{Name: "notify partner", EntityName: "deals", Trigger: "after_update", IsActive: boolPtr(true),
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: "https://other.example/hook"}}}
	if _, err := svc.UpdateWorkflow(ctx, "t1", id, upd); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("events = %d; want 1", len(aud.events))
	}
	if got := assertNoSentinel(t, aud.events, sentinelSecret, sentinelHeader, sentinelQuery); !strings.Contains(got, auditRedacted) {
		t.Fatalf("update diff has no %q marker: %s", auditRedacted, got)
	}

	// Pause, then delete a second keyed rule: the delete diff is the whole row on the before side.
	id2 := legacyKeyedRule(repo, "legacy-2")
	if _, err := svc.SetWorkflowActive(ctx, "t1", id2, false, "builder-1"); err != nil {
		t.Fatalf("SetWorkflowActive: %v", err)
	}
	if err := svc.DeleteWorkflow(ctx, "t1", id2); err != nil {
		t.Fatalf("DeleteWorkflow: %v", err)
	}
	got := assertNoSentinel(t, aud.events, sentinelSecret, sentinelHeader, sentinelQuery)
	if strings.Count(got, auditRedacted) < 2 {
		t.Fatalf("delete diff lost the redaction marker: %s", got)
	}
}

func TestRedactWorkflowActionsForAudit(t *testing.T) {
	tests := []struct {
		name, in string
		keep     []string // substrings that must survive
		drop     []string // substrings that must be gone
	}{
		{"secret + headers + url token", `[{"type":"outbound_webhook","url":"https://u:` + sentinelSecret + `@h.example/p?k=` + sentinelQuery + `","secret":"` + sentinelSecret + `","headers":{"X-A":"` + sentinelHeader + `"}}]`,
			[]string{`"type":"outbound_webhook"`, auditRedacted, `"X-A"`, "h.example"}, []string{sentinelSecret, sentinelHeader, sentinelQuery}},
		{"case-insensitive keys", `[{"type":"outbound_webhook","SECRET":"` + sentinelSecret + `","Headers":{"A":"` + sentinelHeader + `"}}]`,
			[]string{auditRedacted}, []string{sentinelSecret, sentinelHeader}},
		{"headers that is not an object is masked whole", `[{"type":"outbound_webhook","headers":"` + sentinelHeader + `"}]`,
			[]string{auditRedacted}, []string{sentinelHeader}},
		{"vault path query value", `[{"type":"outbound_webhook","credentialId":"c1","path":"/v1/x?sig=` + sentinelQuery + `"}]`,
			[]string{`"credentialId":"c1"`, "/v1/x?sig="}, []string{sentinelQuery}},
		{"empty secret stays empty", `[{"type":"outbound_webhook","url":"https://h.example","secret":""}]`,
			[]string{`"secret":""`}, nil},
		{"not a list is replaced whole", `{"secret":"` + sentinelSecret + `"}`, []string{auditRedacted}, []string{sentinelSecret}},
		{"big integer survives", `[{"type":"field_update","field":"n","value":12345678901234567}]`, []string{"12345678901234567"}, nil},
		{"null stays null", `null`, []string{"null"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(redactWorkflowActionsForAudit(json.RawMessage(tc.in)))
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Errorf("output %s lost %q", got, k)
				}
			}
			for _, d := range tc.drop {
				if strings.Contains(got, d) {
					t.Errorf("output %s still has %q", got, d)
				}
			}
		})
	}
}

// FR-022: on a delivered workspace the builder's save only reaches the draft, so no audit row; the
// legacy customer admin's save is live, so it does leave one.
func TestWorkflowService_DraftOnlyWriteLeavesNoRow(t *testing.T) {
	e := newDWEnv(true)
	aud := &captureAuditor{}
	svc := NewWorkflowService(e.repo).WithEntityChecker(w6Catalog()).WithAuditor(aud, NewDraftGate(e.deliv))

	created, err := svc.CreateWorkflow(builderCtx(), "t1", w6CreateInput("stage"))
	if err != nil {
		t.Fatalf("builder CreateWorkflow: %v", err)
	}
	if created.DraftState != "new" {
		t.Fatalf("DraftState = %q; the builder's write should be a draft", created.DraftState)
	}
	if len(aud.events) != 0 {
		t.Fatalf("a draft-only create wrote %d audit row(s); want 0", len(aud.events))
	}

	liveID := e.seedLive(t, "live rule")
	in := UpdateWorkflowInput{Name: "live rule", EntityName: "deals", Trigger: "after_update",
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "z"}}}
	if _, err := svc.UpdateWorkflow(legacyAdminCtx(), "t1", liveID, in); err != nil {
		t.Fatalf("legacy admin UpdateWorkflow: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].Action != audit.ActionUpdate || aud.events[0].RecordID != liveID {
		t.Fatalf("a live (customer admin) write must be audited; events = %+v", aud.events)
	}
}

// "Tắt ngay cho khách" switches the LIVE rule off at once even though the caller is a builder: the
// change is live, so it is audited (not subject to the draft skip), and a repeat press writes nothing.
func TestDisableNow_EmitsAuditOnlyWhenItChangesTheRule(t *testing.T) {
	e := newDWEnv(true)
	aud := &captureAuditor{}
	svc := NewWorkflowService(e.repo).WithEntityChecker(w6Catalog()).WithLiveRepo(liveFake{e.live}).WithAuditor(aud, NewDraftGate(e.deliv))
	id := e.seedLive(t, "running")

	if _, err := svc.DisableNow(builderCtx(), dwTenant, id, "builder"); err != nil {
		t.Fatal(err)
	}
	if len(aud.events) != 1 || aud.events[0].EntityName != audit.WorkflowRuleMeta || aud.events[0].RecordID != id {
		t.Fatalf("events = %+v; want one __workflow_rule__ row", aud.events)
	}
	if d := aud.events[0].Diff; d == nil || d.Before["isActive"] != true || d.After["isActive"] != false {
		t.Fatalf("diff = %+v; want isActive true -> false", d)
	}
	if _, err := svc.DisableNow(builderCtx(), dwTenant, id, "builder"); err != nil {
		t.Fatal(err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("a repeat DisableNow added a row: %d events", len(aud.events))
	}
}
