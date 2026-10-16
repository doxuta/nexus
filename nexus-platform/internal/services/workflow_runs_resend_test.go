package services

// specs/066 L2 (T050b, T051): RenderEmailForRecord — the email a "Gửi lại" would send is built from the
// LIVE rule (never the builder's draft layer) and the record's CURRENT data, through the SAME event
// helper and workflow.RenderEmail the dry-run uses.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

const rnrTenant = "t1"

// rnrSvc wires a WorkflowService whose admin repo (s.repo — the draft-wrapped one in production) answers
// the NHÁP rule and whose live repo answers the SỐNG one, over one record.
func rnrSvc(t *testing.T, liveActions, draftActions string) (*WorkflowService, *fakeWFRepo, *fakeWFRepo, *fakeRecordReader) {
	t.Helper()
	draft, live := newFakeWFRepo(), newFakeWFRepo()
	draft.rows["wf-1"] = &workflow.WorkflowRecord{ID: "wf-1", TenantID: rnrTenant, Name: "r", EntityName: "deals",
		Trigger: "after_create", Actions: []byte(draftActions), IsActive: true}
	live.rows["wf-1"] = &workflow.WorkflowRecord{ID: "wf-1", TenantID: rnrTenant, Name: "r", EntityName: "deals",
		Trigger: "after_create", Actions: []byte(liveActions), IsActive: true}
	rec := &dynrepo.Record{ID: "r1", TenantID: rnrTenant, Data: map[string]any{"ho_ten": "Lan", "owner": "lan@x.vn"}}
	reader := &fakeRecordReader{rec: rec}
	svc := NewWorkflowService(draft).WithEntityChecker(w6Catalog()).WithRecordReader(reader).WithLiveRepo(liveFake{live})
	return svc, draft, live, reader
}

const (
	rnrLive  = `[{"type":"in_app_notification","message":"x"},{"type":"email_alert","to":"{{owner}}","subject":"SỐNG {{ho_ten}}","body":"b"}]`
	rnrDraft = `[{"type":"in_app_notification","message":"x"},{"type":"email_alert","to":"{{owner}}","subject":"NHÁP {{ho_ten}}","body":"b"}]`
)

// T050b: the live rule wins; the draft-layer repo is never read (FR-032, US2 AS10).
func TestRenderEmailForRecord_ReadsTheLiveRuleNotTheDraft(t *testing.T) {
	svc, draft, _, _ := rnrSvc(t, rnrLive, rnrDraft)
	spy := &getSpyRepo{fakeWFRepo: draft}
	svc.repo = spy

	mail, act, err := svc.RenderEmailForRecord(context.Background(), rnrTenant, "wf-1", 1, "r1")
	if err != nil {
		t.Fatalf("RenderEmailForRecord: %v", err)
	}
	if mail.Subject != "SỐNG Lan" || mail.To != "lan@x.vn" {
		t.Fatalf("mail = %+v; want the LIVE subject and the record's current owner address", mail)
	}
	if workflow.EmailActionHash(act) == "" || act.Subject != "SỐNG {{ho_ten}}" {
		t.Fatalf("action = %+v; want the live action back (the caller hashes it for ruleChangedSinceSend)", act)
	}
	if spy.gets != 0 {
		t.Fatalf("the admin (draft-aware) repo was read %d times; resend must go through liveRepo only", spy.gets)
	}
}

type getSpyRepo struct {
	*fakeWFRepo
	gets int
}

func (g *getSpyRepo) Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	g.gets++
	return g.fakeWFRepo.Get(ctx, tenantID, id)
}

// A rule switched off (the operator's "Tắt ngay cho khách", specs/063 FR-017) must not mail anyone
// through the resend either: the live rule is read WITH its is_active and a switched-off one is refused.
func TestRenderEmailForRecord_RefusesASwitchedOffRule(t *testing.T) {
	svc, _, live, _ := rnrSvc(t, rnrLive, rnrDraft)
	live.rows["wf-1"].IsActive = false
	_, _, err := svc.RenderEmailForRecord(context.Background(), rnrTenant, "wf-1", 1, "r1")
	if !errors.Is(err, ErrEmailRuleDisabled) {
		t.Fatalf("err = %v; want ErrEmailRuleDisabled", err)
	}
	// A deleted/shifted slot is still RULE_GONE even on a switched-off rule: the gone verdict wins.
	_, _, err = svc.RenderEmailForRecord(context.Background(), rnrTenant, "wf-1", 9, "r1")
	if !errors.Is(err, ErrEmailRuleGone) {
		t.Fatalf("err = %v; want ErrEmailRuleGone for a slot past the end", err)
	}
}

func TestRenderEmailForRecord_Errors(t *testing.T) {
	cases := []struct {
		name    string
		wfID    string
		tenant  string
		idx     int
		recID   string
		unwired bool
		want    error
	}{
		{name: "unknown rule", wfID: "ghost", tenant: rnrTenant, idx: 1, recID: "r1", want: ErrEmailRuleGone},
		{name: "other tenant's rule", wfID: "wf-1", tenant: "t2", idx: 1, recID: "r1", want: ErrEmailRuleGone},
		{name: "index past the end", wfID: "wf-1", tenant: rnrTenant, idx: 9, recID: "r1", want: ErrEmailRuleGone},
		{name: "negative index", wfID: "wf-1", tenant: rnrTenant, idx: -1, recID: "r1", want: ErrEmailRuleGone},
		{name: "slot is no longer an email", wfID: "wf-1", tenant: rnrTenant, idx: 0, recID: "r1", want: ErrEmailRuleGone},
		{name: "record gone or in the bin", wfID: "wf-1", tenant: rnrTenant, idx: 1, recID: "nope", want: ErrEmailRecordGone},
		{name: "no live repo wired", wfID: "wf-1", tenant: rnrTenant, idx: 1, recID: "r1", unwired: true, want: ErrEmailResendUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _ := rnrSvc(t, rnrLive, rnrDraft)
			if tc.unwired {
				svc.liveRepo = nil
			}
			_, _, err := svc.RenderEmailForRecord(context.Background(), tc.tenant, tc.wfID, tc.idx, tc.recID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

// T051: the dry-run's email preview and the resend render the same mail for the same record — one
// render path, so "Chạy thử" can never show an address the resend would not use.
func TestRenderEmailForRecord_MatchesTheDryRunPreview(t *testing.T) {
	svc, _, live, _ := rnrSvc(t, rnrLive, rnrLive)
	_ = live
	// DryRun reads the rule through s.repo (draft == live here, same document).
	res, err := svc.TestWorkflow(context.Background(), rnrTenant, "wf-1", "r1")
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	var preview map[string]any
	for _, st := range res.Trace {
		if st.Kind == "action" && st.Detail["type"] == workflow.ActionEmailAlert {
			preview = st.Detail
		}
	}
	if preview == nil {
		t.Fatalf("no email step in %+v", res.Trace)
	}
	mail, _, err := svc.RenderEmailForRecord(context.Background(), rnrTenant, "wf-1", 1, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if preview["to"] != mail.To || preview["subject"] != mail.Subject || preview["body"] != mail.Body {
		t.Fatalf("dry-run preview %v != resend render %+v", preview, mail)
	}
}
