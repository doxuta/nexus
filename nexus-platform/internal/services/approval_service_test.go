package services

// specs/043 S0 bpm-node-ids (T005/T008/T010): the ONE config validator every write path shares
// (Studio/API, AI propose_approval, pack install) now owns step ids and refuses keys the engine
// does not run yet (D-7) — while a legacy request (no ids, v1 keys only) is accepted exactly as
// before and simply gets its positional ids written on this first save.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/workflow"
)

func wantApprovalCode(t *testing.T, err error, code string) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != code {
		t.Fatalf("err = %v, want ValidationError %s", err, code)
	}
	return ve
}

func twoSteps(a, b ApprovalStepInput) []ApprovalStepInput { return []ApprovalStepInput{a, b} }

var (
	roleStep = ApprovalStepInput{ApproverType: "role", ApproverID: "r-mgr"}
	userStep = ApprovalStepInput{ApproverType: "user", ApproverID: "u-dir", SLASeconds: 3600}
)

func withID(s ApprovalStepInput, id string) ApprovalStepInput { s.ID = id; return s }

func TestValidateApprovalProcess_NodeIDDuplicate422(t *testing.T) {
	cases := [][]ApprovalStepInput{
		twoSteps(withID(roleStep, "duyet"), withID(userStep, "duyet")),
		twoSteps(withID(roleStep, "s1"), withID(userStep, " s1 ")), // trimmed before comparing
	}
	for _, steps := range cases {
		_, _, _, err := ValidateApprovalProcess("P", "after_create", "", nil, steps)
		ve := wantApprovalCode(t, err, "APPROVAL_NODE_ID_DUPLICATE")
		if !strings.Contains(ve.Msg, "bước 2") || ve.Field != "steps[1].id" {
			t.Errorf("error must point at the offending step: msg=%q field=%q", ve.Msg, ve.Field)
		}
	}
}

func TestValidateApprovalProcess_NodeIDPattern422(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 61), "Kế toán", "S1", "a b", "bpm:x"} {
		_, _, _, err := ValidateApprovalProcess("P", "after_create", "", nil,
			twoSteps(roleStep, withID(userStep, id)))
		ve := wantApprovalCode(t, err, "APPROVAL_NODE_ID_INVALID")
		if ve.Field != "steps[1].id" {
			t.Errorf("id %q: field = %q, want steps[1].id", id, ve.Field)
		}
	}
	// 60 characters is the ceiling, inclusive: "bpm:"+id is exactly the cursor column's 64.
	if _, _, _, err := ValidateApprovalProcess("P", "after_create", "", nil,
		twoSteps(roleStep, withID(userStep, strings.Repeat("a", 60)))); err != nil {
		t.Fatalf("60-char id must pass: %v", err)
	}
}

// TestValidateApprovalProcess_LegacyShapeUnchanged: the request every existing client sends (no
// ids, v1 keys only) still validates, keeps every value, and is persisted WITH positional ids —
// the "first save writes the real id" half of FR-002. Mixed input keeps the explicit id and
// fills the rest without a collision.
func TestValidateApprovalProcess_LegacyShapeUnchanged(t *testing.T) {
	legacy := twoSteps(
		ApprovalStepInput{Name: " Trưởng phòng ", ApproverType: "role", ApproverID: " r-mgr "},
		userStep,
	)
	trig, _, stepsJSON, err := ValidateApprovalProcess("P", "after_update", "", nil, legacy)
	if err != nil || trig != bpm.TriggerAfterUpdate {
		t.Fatalf("legacy request refused: trig=%q err=%v", trig, err)
	}
	var got []bpm.Step
	if err := json.Unmarshal(stepsJSON, &got); err != nil {
		t.Fatalf("stored steps: %v", err)
	}
	want := []bpm.Step{
		{ID: "s1", Name: "Trưởng phòng", ApproverType: bpm.ApproverRole, ApproverID: "r-mgr"},
		{ID: "s2", ApproverType: bpm.ApproverUser, ApproverID: "u-dir", SLASeconds: 3600},
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], want[0]) || !reflect.DeepEqual(got[1], want[1]) { // Step carries slices since S4: not ==-comparable
		t.Fatalf("stored steps = %+v, want %+v", got, want)
	}

	_, _, mixed, err := ValidateApprovalProcess("P", "after_update", "", nil, twoSteps(roleStep, withID(userStep, "s1")))
	if err != nil {
		t.Fatalf("mixed ids refused: %v", err)
	}
	if err := json.Unmarshal(mixed, &got); err != nil || got[0].ID != "s2" || got[1].ID != "s1" {
		t.Fatalf("mixed ids = %s, want [s2 s1] (explicit id kept, no collision)", mixed)
	}

	// The pre-existing step rules are untouched.
	_, _, _, err = ValidateApprovalProcess("P", "after_create", "", nil, []ApprovalStepInput{{ApproverType: "team", ApproverID: "x"}})
	wantApprovalCode(t, err, "APPROVAL_STEP_INVALID")
}

// TestValidateApprovalProcess_UnsupportedKey422 (D-7): a step key the linear engine does not run
// (parallel approvers, routing, onReject, an sla object, slaClock before S1 …) is refused at
// config time instead of being dropped on the floor — accepting config the engine then ignores
// is the software lying about what it will do.
func TestValidateApprovalProcess_UnsupportedKey422(t *testing.T) {
	s := userStep
	s.Unsupported = []string{"approvers", "completion"}
	_, _, _, err := ValidateApprovalProcess("P", "after_create", "", nil, twoSteps(roleStep, s))
	ve := wantApprovalCode(t, err, "APPROVAL_NODE_KIND_UNSUPPORTED")
	if !strings.Contains(ve.Msg, "bước 2") || !strings.Contains(ve.Msg, "approvers") || ve.Field != "steps[1].approvers" {
		t.Errorf("error must name the step and the key: msg=%q field=%q", ve.Msg, ve.Field)
	}

	// Process level: graph/options arrive in later slices (S3a2) — refused until then.
	wantApprovalCode(t, UnsupportedApprovalProcessKeys([]string{"graph"}), "APPROVAL_NODE_KIND_UNSUPPORTED")
	if err := UnsupportedApprovalProcessKeys(nil); err != nil {
		t.Fatalf("no extra keys must pass: %v", err)
	}
}

func TestUnsupportedApprovalKeys(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"id":"s1","name":"x","approverType":"user","approverId":"u","slaSeconds":1}`, ""},
		// encoding/json binds keys case-insensitively — so does this check: a key the decoder
		// understood is never reported as unsupported.
		{`{"ApproverType":"user","APPROVERID":"u"}`, ""},
		{`{"approverType":"user","approverId":"u","onReject":{},"approvers":[]}`, "approvers,onReject"},
		// slaClock joined the v1 step shape in S1 (the calendar-backed deadline honours it).
		{`{"approverType":"user","approverId":"u","slaClock":"business"}`, ""},
		{`"not an object"`, ""},
	}
	for _, tc := range cases {
		got := strings.Join(UnsupportedApprovalKeys([]byte(tc.raw), ApprovalStepV1Keys...), ",")
		if got != tc.want {
			t.Errorf("UnsupportedApprovalKeys(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestValidateApprovalTrigger_MessageListsManual (T008): "manual" has been a valid trigger since
// the submit verb landed, but the refusal still told the admin only after_create/after_update
// exist.
func TestValidateApprovalTrigger_MessageListsManual(t *testing.T) {
	_, err := validateApprovalTrigger("after_delete")
	ve := wantApprovalCode(t, err, "APPROVAL_TRIGGER_INVALID")
	if !strings.Contains(ve.Msg, "manual") {
		t.Fatalf("trigger refusal must list manual: %q", ve.Msg)
	}
	if _, err := validateApprovalTrigger("manual"); err != nil {
		t.Fatalf("manual must validate: %v", err)
	}
}

// --- specs/043 S2b: "vừa đổi thành" + HOẶC on approval entry criteria ---------------------------

func rule(field, op string, value any) ApprovalCriteriaRuleInput {
	return ApprovalCriteriaRuleInput{Field: field, Op: op, Value: value}
}

// TestValidate_EdgeOpOnlyAfterUpdate (T041): changed / changedTo compare the record against its
// before-image, which only an EDIT has. They are accepted on after_update and refused on a create
// or a manual send (where they could never be true — a rule that never fires, saved without a
// word). Value rules mirror the workflow validator: changed takes none, changedTo needs one.
func TestValidate_EdgeOpOnlyAfterUpdate(t *testing.T) {
	cases := []struct {
		name, trigger string
		r             ApprovalCriteriaRuleInput
		wantCode      string
	}{
		{"changedTo on edit", "after_update", rule("status", "changedTo", "Chờ duyệt"), ""},
		{"changed on edit", "after_update", rule("status", "changed", nil), ""},
		{"changed carries no value", "after_update", rule("status", "changed", "x"), "APPROVAL_CRITERIA_INVALID"},
		{"changedTo needs a value", "after_update", rule("status", "changedTo", nil), "APPROVAL_CRITERIA_INVALID"},
		{"changedTo on create", "after_create", rule("status", "changedTo", "Chờ duyệt"), "APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED"},
		{"changed on manual send", "manual", rule("status", "changed", nil), "APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, crit, _, err := ValidateApprovalProcess("P", tc.trigger, "", []ApprovalCriteriaRuleInput{tc.r}, twoSteps(roleStep, userStep))
			if tc.wantCode != "" {
				wantApprovalCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			var c workflow.Condition
			if err := json.Unmarshal(crit, &c); err != nil || len(c.Rules) != 1 || c.Rules[0].Op != tc.r.Op {
				t.Fatalf("persisted criteria = %s (%v); want the %s rule kept", crit, err, tc.r.Op)
			}
		})
	}
}

// TestValidate_CriteriaOrAccepted (T041): match "or" is persisted as "or" (the evaluator already
// honours it) — so a quote over 50 million OR with a discount over 20% needs sign-off, which AND
// could not say. A typo is still refused rather than coerced.
func TestValidate_CriteriaOrAccepted(t *testing.T) {
	rules := []ApprovalCriteriaRuleInput{rule("total", "gt", float64(50000000)), rule("discount", "gt", float64(20))}
	onlyDiscount := map[string]any{"total": float64(1000), "discount": float64(30)}
	for _, match := range []string{"or", " OR "} {
		_, crit, _, err := ValidateApprovalProcess("P", "after_create", match, rules, twoSteps(roleStep, userStep))
		if err != nil {
			t.Fatalf("match %q: %v", match, err)
		}
		var c workflow.Condition
		if err := json.Unmarshal(crit, &c); err != nil || c.Match != "or" {
			t.Fatalf("match %q persisted as %s (%v); want \"or\"", match, crit, err)
		}
		if !workflow.Evaluate(&c, onlyDiscount) {
			t.Fatalf("match %q: one satisfied rule must pass an OR criteria", match)
		}
	}
	_, crit, _, err := ValidateApprovalProcess("P", "after_create", "", rules, twoSteps(roleStep, userStep))
	var c workflow.Condition
	if err != nil || json.Unmarshal(crit, &c) != nil || c.Match != "and" || workflow.Evaluate(&c, onlyDiscount) {
		t.Fatalf("empty match must stay AND: %s %v", crit, err)
	}
	_, _, _, err = ValidateApprovalProcess("P", "after_create", "xor", rules, twoSteps(roleStep, userStep))
	wantApprovalCode(t, err, "APPROVAL_CRITERIA_INVALID")
}

// TestValidate_OutcomeSelfTrigger422 (T041, US7 #5): a process whose own write-back lands on the
// value it starts on would restart itself every time it ends. Refused on create AND update, naming
// the colliding outcome value; the look-alikes that cannot loop stay accepted.
func TestValidate_OutcomeSelfTrigger422(t *testing.T) {
	svc := NewApprovalService(&fakeApprovalProcessRepo{}, nil, nil)
	loop := []ApprovalCriteriaRuleInput{rule("status", "changedTo", "Chấp nhận")}
	approvedLoop := &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"}

	_, err := svc.CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{Name: "P", EntityName: "crm_quotes",
		Trigger: "after_update", Rules: loop, Steps: twoSteps(roleStep, userStep), Outcome: approvedLoop})
	if ve := wantApprovalCode(t, err, "APPROVAL_OUTCOME_SELF_TRIGGER"); ve.Field != "outcome.approvedValue" {
		t.Errorf("create: field = %q, want outcome.approvedValue", ve.Field)
	}
	_, err = svc.UpdateProcess(context.Background(), "t1", "p1", UpdateApprovalProcessInput{Name: "P", EntityName: "crm_quotes",
		Trigger: "after_update", Match: "or", Rules: []ApprovalCriteriaRuleInput{rule("total", "gt", float64(1)), rule("status", "changedTo", "Từ chối")},
		Steps: twoSteps(roleStep, userStep), Outcome: approvedLoop})
	if ve := wantApprovalCode(t, err, "APPROVAL_OUTCOME_SELF_TRIGGER"); ve.Field != "outcome.rejectedValue" {
		t.Errorf("update: field = %q, want outcome.rejectedValue", ve.Field)
	}

	cases := []struct {
		name    string
		trigger string
		rules   []ApprovalCriteriaRuleInput
		out     *ApprovalOutcomeInput
		wantErr bool
	}{
		{"changed on the outcome field", "after_update", []ApprovalCriteriaRuleInput{rule(" status ", "changed", nil)}, approvedLoop, true},
		// State rules on the outcome field loop too: the write-back is an edit, the pending
		// instance has just closed, and the record now satisfies the rule — the engine re-arms
		// (TestArm_ChangedTo_NotOnLaterEdits' eq control shows exactly that).
		{"eq the approved value", "after_update", []ApprovalCriteriaRuleInput{rule("status", "eq", "Chấp nhận")}, approvedLoop, true},
		{"neq a draft value", "after_update", []ApprovalCriteriaRuleInput{rule("status", "neq", "Nháp")}, approvedLoop, true},
		{"notnull on the outcome field", "after_update", []ApprovalCriteriaRuleInput{rule("status", "notnull", nil)}, approvedLoop, true},
		{"contains the rejected value", "after_update", []ApprovalCriteriaRuleInput{rule("status", "contains", "chối")}, approvedLoop, true},
		{"eq a value neither outcome writes", "after_update", []ApprovalCriteriaRuleInput{rule("status", "eq", "Chờ duyệt")}, approvedLoop, false},
		{"isnull on the outcome field", "after_update", []ApprovalCriteriaRuleInput{rule("status", "isnull", nil)}, approvedLoop, false},
		{"state rule on another field", "after_update", []ApprovalCriteriaRuleInput{rule("stage", "eq", "Chấp nhận")}, approvedLoop, false},
		{"starts on another value", "after_update", []ApprovalCriteriaRuleInput{rule("status", "changedTo", "Chờ duyệt")}, approvedLoop, false},
		{"edge rule on another field", "after_update", []ApprovalCriteriaRuleInput{rule("stage", "changedTo", "Chấp nhận")}, approvedLoop, false},
		{"no write-back", "after_update", loop, nil, false},
		{"created, not edited", "after_create", []ApprovalCriteriaRuleInput{rule("status", "eq", "Chấp nhận")}, approvedLoop, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ApprovalOutcomeSelfTrigger(tc.trigger, tc.rules, tc.out)
			if tc.wantErr {
				wantApprovalCode(t, err, "APPROVAL_OUTCOME_SELF_TRIGGER")
			} else if err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

// ---- specs/043 S2 (FR-027): a rejection states its reason, on the server ----------------------

// countingDecider records whether the engine was reached.
type countingDecider struct{ calls int }

func (d *countingDecider) DecideAt(_ context.Context, _, id string, _ bpm.DecisionKind, comment string, _ bpm.Actor, _ *int) (*bpm.Instance, error) {
	d.calls++
	return &bpm.Instance{ID: id, History: []bpm.HistoryEntry{{Comment: comment}}}, nil
}

// rejectWith runs a reject through the service with the given note and returns the error plus
// whether the engine was reached.
func rejectWith(t *testing.T, note string) (error, int) {
	t.Helper()
	d := &countingDecider{}
	_, err := NewApprovalService(nil, nil, d).Decide(context.Background(), "t1", "ai-1", bpm.DecisionReject, note, bpm.Actor{UserID: "duyet2"})
	return err, d.calls
}

// TestDecide_RejectEmptyReason422: the reason was required only by the screens; any caller holding
// a token could reject with nothing, and the submitter was left with a "no" and no why.
func TestDecide_RejectEmptyReason422(t *testing.T) {
	err, calls := rejectWith(t, "")
	if ve := wantApprovalCode(t, err, "APPROVAL_REJECT_REASON_REQUIRED"); ve.Field != "note" {
		t.Errorf("field = %q, want note", ve.Field)
	}
	if calls != 0 {
		t.Fatalf("engine reached %d time(s) on a reasonless reject; want 0 — the instance must stay pending", calls)
	}
	// A reason given goes through; approving still needs none.
	if err, calls := rejectWith(t, "Sai đơn giá"); err != nil || calls != 1 {
		t.Fatalf("reject with a reason: err = %v, engine calls = %d; want nil, 1", err, calls)
	}
	d := &countingDecider{}
	if _, err := NewApprovalService(nil, nil, d).Decide(context.Background(), "t1", "ai-1", bpm.DecisionApprove, "", bpm.Actor{UserID: "duyet2"}); err != nil || d.calls != 1 {
		t.Fatalf("approve without a note: err = %v, engine calls = %d; want nil, 1", err, d.calls)
	}
}

// TestDecide_RejectWhitespaceReason422: blank is not a reason.
func TestDecide_RejectWhitespaceReason422(t *testing.T) {
	for _, note := range []string{" ", "\t\n  ", " 　"} {
		err, calls := rejectWith(t, note)
		wantApprovalCode(t, err, "APPROVAL_REJECT_REASON_REQUIRED")
		if calls != 0 {
			t.Fatalf("note %q reached the engine", note)
		}
	}
}
