package services

// specs/048 S1a — config-time rules for "before" workflows (before_save / before_delete). A before
// rule runs synchronously inside the save (from S1b), so everything that could make it slow, leaky
// or silently dead is refused HERE, at save time of the rule: side-effect actions, a veto without a
// usable message, a condition that can never be true, a timer, too many actions, too many active
// rules, and — until the runtime path ships — switching the rule on at all (R13). All fakes, no DB.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// beforeCatalog: a deals entity with the fields the CRM "Chốt Thắng phải có Ngày chốt" rule reads.
func beforeCatalog() *fakeWFCatalog {
	return &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-b1", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "f1", EntityID: "ent-b1", Name: "stage", FieldType: "picklist"},
			{ID: "f2", EntityID: "ent-b1", Name: "close_date", FieldType: "date"},
			{ID: "f3", EntityID: "ent-b1", Name: "probability", FieldType: "number"},
			{ID: "f4", EntityID: "ent-b1", Name: "email", FieldType: "email"},
		},
	}
}

func beforeFieldTypes() map[string]string {
	return map[string]string{"stage": "picklist", "close_date": "date", "probability": "number", "email": "email"}
}

func vetoAction(field, msg string) WorkflowActionInput {
	return WorkflowActionInput{Type: workflow.ActionVeto, Field: field, Message: msg}
}

func wantBeforeCode(t *testing.T, err error, want string) *ValidationError {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("want accepted, got %v", err)
		}
		return nil
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError %s, got %v", want, err)
	}
	if ve.Code != want {
		t.Fatalf("code = %s (%q), want %s", ve.Code, ve.Msg, want)
	}
	return ve
}

// T007: a before rule runs INSIDE the save, so an action with an outside effect (mail, HTTP, a
// notification row, another record, an owner change, a deadline stamp) would fire for a save that
// is then vetoed or rolled back — and would slow every save down. Refused at config time, naming
// both the action and the trigger so the Studio can say which pair is wrong.
func TestBuildWorkflowRule_Before_RejectsSideEffectActions(t *testing.T) {
	sideEffects := []WorkflowActionInput{
		{Type: workflow.ActionEmailAlert, To: "a@b.vn", Subject: "s", Body: "b"},
		{Type: workflow.ActionWebhook, URL: "https://example.com/hook"},
		{Type: workflow.ActionInAppNotification, UserID: "$owner", Message: "m"},
		{Type: workflow.ActionAssign, Team: "team-1", Field: "stage"},
		{Type: workflow.ActionCreateRecord, CreateEntity: "tasks", Values: map[string]any{"title": "x"}},
		{Type: workflow.ActionSetDeadline, Field: "close_date", Offset: &WorkflowTimerOffsetInput{Amount: 1, Unit: "days"}},
		{Type: workflow.ActionSetElapsed, Field: "probability"},
	}
	for _, trig := range []string{"before_save", "before_delete"} {
		for _, a := range sideEffects {
			t.Run(trig+"/"+a.Type, func(t *testing.T) {
				err := ValidateWorkflowConfig("deals", trig, nil, []WorkflowActionInput{a}, beforeFieldTypes())
				ve := wantBeforeCode(t, err, "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER")
				if ve.Details["actionType"] != a.Type || ve.Details["trigger"] != trig {
					t.Fatalf("details = %v, want actionType=%s trigger=%s", ve.Details, a.Type, trig)
				}
			})
		}
	}
}

// T008: the shape of a before rule, one row per way to get it wrong — plus the after_* rules that
// must keep passing exactly as before.
func TestBuildWorkflowRule_BeforeVetoShape(t *testing.T) {
	okVeto := []WorkflowActionInput{vetoAction("close_date", "Chốt Thắng phải có Ngày chốt")}
	wonNoDate := []WorkflowRuleInput{
		{Field: "stage", Op: "changedTo", Value: "Closed Won"},
		{Field: "close_date", Op: "isnull"},
	}
	eleven := make([]WorkflowActionInput, 11)
	for i := range eleven {
		eleven[i] = WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "probability", Value: float64(i)}
	}

	tests := []struct {
		name      string
		trigger   string
		cond      *WorkflowConditionInput
		actions   []WorkflowActionInput
		wantCode  string
		wantField string // details.field when the error anchors on one
	}{
		{name: "before_save veto with changedTo on update", trigger: "before_save",
			cond: &WorkflowConditionInput{Rules: wonNoDate, SaveOn: "update"}, actions: okVeto},
		{name: "before_save veto on both create and update (saveOn absent)", trigger: "before_save",
			cond: &WorkflowConditionInput{Rules: []WorkflowRuleInput{{Field: "stage", Op: "eq", Value: "Closed Won"}}}, actions: okVeto},
		{name: "before_save veto without an anchor field", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("", "Không được lưu")}},
		{name: "before_delete veto", trigger: "before_delete",
			cond:    &WorkflowConditionInput{Rules: []WorkflowRuleInput{{Field: "stage", Op: "eq", Value: "Closed Won"}}},
			actions: []WorkflowActionInput{vetoAction("", "Không xoá cơ hội đã Thắng")}},
		{name: "veto message of exactly 500 characters", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("", strings.Repeat("ắ", 500))}},

		{name: "empty veto message", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("close_date", "")}, wantCode: "WORKFLOW_VETO_MESSAGE_INVALID"},
		{name: "whitespace veto message", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("close_date", "   \n ")}, wantCode: "WORKFLOW_VETO_MESSAGE_INVALID"},
		{name: "veto message of 501 characters", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("", strings.Repeat("ắ", 501))}, wantCode: "WORKFLOW_VETO_MESSAGE_INVALID"},
		{name: "veto anchored on an unknown field", trigger: "before_save",
			actions: []WorkflowActionInput{vetoAction("closing_day", "x")}, wantCode: "RECORD_UNKNOWN_FIELD", wantField: "closing_day"},
		{name: "two vetoes in one rule", trigger: "before_save",
			actions:  []WorkflowActionInput{vetoAction("", "a"), vetoAction("", "b")},
			wantCode: "WORKFLOW_BEFORE_ACTIONS_MIXED"},
		{name: "veto mixed with a value change", trigger: "before_save",
			actions:  []WorkflowActionInput{vetoAction("", "a"), {Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0}},
			wantCode: "WORKFLOW_BEFORE_ACTIONS_MIXED"},
		{name: "saveOn outside create/update", trigger: "before_save",
			cond: &WorkflowConditionInput{SaveOn: "both"}, actions: okVeto, wantCode: "WORKFLOW_SAVE_ON_INVALID"},
		{name: "saveOn on an after trigger", trigger: "after_update",
			cond:     &WorkflowConditionInput{SaveOn: "update"},
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 1.0}},
			wantCode: "WORKFLOW_SAVE_ON_INVALID"},
		{name: "saveOn on before_delete", trigger: "before_delete",
			cond: &WorkflowConditionInput{SaveOn: "update"}, actions: okVeto, wantCode: "WORKFLOW_SAVE_ON_INVALID"},
		{name: "timer on before_save", trigger: "before_save",
			cond: &WorkflowConditionInput{Timer: &WorkflowTimerInput{DateField: "close_date",
				Offset: WorkflowTimerOffsetInput{Amount: 1, Unit: "days"}, Direction: "before"}},
			actions: okVeto, wantCode: "WORKFLOW_BEFORE_TIMER_INVALID"},
		{name: "changed on before_delete can never be true", trigger: "before_delete",
			cond:    &WorkflowConditionInput{Rules: []WorkflowRuleInput{{Field: "stage", Op: "changed"}}},
			actions: okVeto, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE", wantField: "stage"},
		{name: "changedTo on a create-only rule can never be true", trigger: "before_save",
			cond:    &WorkflowConditionInput{Rules: wonNoDate, SaveOn: "create"},
			actions: okVeto, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE", wantField: "stage"},
		{name: "before_delete with a value change", trigger: "before_delete",
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 0.0}},
			wantCode: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER"},
		{name: "eleven actions", trigger: "before_save", actions: eleven, wantCode: "WORKFLOW_TOO_MANY_ACTIONS"},
		// S4 runs field_update inside the save, S5 normalize: a before_save rule may carry either.
		{name: "field_update on before_save", trigger: "before_save",
			actions: []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0}}},
		{name: "normalize on before_save", trigger: "before_save",
			actions: []WorkflowActionInput{{Type: workflow.ActionNormalize, Field: "email", Ops: []string{"trim"}}}},
		{name: "veto on an after trigger", trigger: "after_update",
			actions: okVeto, wantCode: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER"},

		// after_* rules: unchanged — including more than ten actions (the cap is a before-rule cap).
		{name: "after_update field_update unchanged", trigger: "after_update",
			cond:    &WorkflowConditionInput{Rules: []WorkflowRuleInput{{Field: "stage", Op: "changedTo", Value: "Closed Won"}}},
			actions: []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0}}},
		{name: "after_create with eleven actions unchanged", trigger: "after_create", actions: eleven},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("deals", tt.trigger, tt.cond, tt.actions, beforeFieldTypes())
			ve := wantBeforeCode(t, err, tt.wantCode)
			if tt.wantField != "" && (ve.Field != tt.wantField || ve.Details["field"] != tt.wantField) {
				t.Fatalf("field = %q details=%v, want %q", ve.Field, ve.Details, tt.wantField)
			}
		})
	}
}

// "changed"/"changedTo" read the record as it stood before the write. Only an update carries that
// image (after_update, and a before_save rule that runs on updates); every other trigger evaluates
// with none, so the rule would be stored active and skipped forever. The refusal is trigger-wide,
// not before-only — the AI Builder's propose_workflow now invites these operators and lands on
// after_create/after_delete too.
func TestBuildWorkflowRule_EdgeOpsNeedABeforeImage(t *testing.T) {
	update := []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 1.0}}
	timer := &WorkflowTimerInput{DateField: "close_date", Offset: WorkflowTimerOffsetInput{Amount: 1, Unit: "days"}, Direction: "before"}
	tests := []struct {
		trigger, saveOn, op string
		timer               *WorkflowTimerInput
		actions             []WorkflowActionInput
		wantCode            string
	}{
		{trigger: "after_update", op: "changedTo", actions: update},
		{trigger: "after_update", op: "changed", actions: update},
		{trigger: "before_save", saveOn: "update", op: "changedTo", actions: []WorkflowActionInput{vetoAction("", "x")}},
		{trigger: "before_save", op: "changed", actions: []WorkflowActionInput{vetoAction("", "x")}},

		{trigger: "after_create", op: "changedTo", actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "after_create", op: "changed", actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "after_delete", op: "changedTo", actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "after_delete", op: "changed", actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "scheduled", op: "changedTo", actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "date_offset", op: "changed", timer: timer, actions: update, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "before_save", saveOn: "create", op: "changed", actions: []WorkflowActionInput{vetoAction("", "x")}, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
		{trigger: "before_delete", op: "changedTo", actions: []WorkflowActionInput{vetoAction("", "x")}, wantCode: "WORKFLOW_CONDITION_NEVER_TRUE"},
	}
	for _, tt := range tests {
		t.Run(tt.trigger+"/"+tt.saveOn+"/"+tt.op, func(t *testing.T) {
			rule := WorkflowRuleInput{Field: "stage", Op: tt.op}
			if tt.op == workflow.OpChangedTo {
				rule.Value = "Closed Won"
			}
			cond := &WorkflowConditionInput{Rules: []WorkflowRuleInput{rule}, SaveOn: tt.saveOn, Timer: tt.timer}
			ve := wantBeforeCode(t, ValidateWorkflowConfig("deals", tt.trigger, cond, tt.actions, beforeFieldTypes()), tt.wantCode)
			if ve != nil && (ve.Field != "stage" || ve.Details["op"] != tt.op || ve.Details["trigger"] != tt.trigger) {
				t.Fatalf("field=%q details=%v, want stage/%s/%s", ve.Field, ve.Details, tt.op, tt.trigger)
			}
		})
	}
}

// The persisted form: saveOn rides inside the condition JSON (like the TIMER-1 timer), veto reuses
// the action's field/message keys. A saveOn-only condition is still persisted — dropping it would
// turn an update-only rule into one that also blocks creates.
func TestBuildWorkflowRule_BeforePersistedShape(t *testing.T) {
	_, condJSON, actionsJSON, err := buildWorkflowRule("deals", "before_save",
		&WorkflowConditionInput{SaveOn: "update"},
		[]WorkflowActionInput{vetoAction(" close_date ", " Chốt Thắng phải có Ngày chốt ")}, beforeFieldTypes())
	if err != nil {
		t.Fatal(err)
	}
	var cond workflow.Condition
	if err := json.Unmarshal(condJSON, &cond); err != nil {
		t.Fatalf("condition JSON %s: %v", condJSON, err)
	}
	if cond.SaveOn != workflow.SaveOnUpdate {
		t.Fatalf("condition = %s, want saveOn=update", condJSON)
	}
	var acts []map[string]any
	if err := json.Unmarshal(actionsJSON, &acts); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "veto", "field": "close_date", "message": "Chốt Thắng phải có Ngày chốt"}
	if len(acts) != 1 || fmt.Sprint(acts[0]) != fmt.Sprint(want) {
		t.Fatalf("actions = %s, want [%v]", actionsJSON, want)
	}
	// saveOn absent + no rules → match-all NULL column, as before.
	_, condJSON, _, err = buildWorkflowRule("deals", "before_save", nil,
		[]WorkflowActionInput{vetoAction("", "x")}, beforeFieldTypes())
	if err != nil || condJSON != nil {
		t.Fatalf("no rules, no saveOn: cond=%s err=%v, want NULL", condJSON, err)
	}
}

func boolp(b bool) *bool { return &b }

// T009: at most 50 ACTIVE before rules per (tenant, entity, trigger) — each one runs inside every
// save of that entity. Checked on create and on switching a rule back on; the rule being edited
// never counts against itself.
func TestCreateWorkflow_BeforeRuleLimit50(t *testing.T) {
	veto := []WorkflowActionInput{vetoAction("", "Không được")}
	seed := func(repo *fakeWFRepo, n int, tenant, entity, trigger string, active bool) {
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("seed-%s-%s-%s-%d", tenant, entity, trigger, i)
			repo.rows[id] = &workflow.WorkflowRecord{ID: id, TenantID: tenant, Name: id, EntityName: entity,
				Trigger: trigger, IsActive: active, Actions: json.RawMessage(`[]`)}
		}
	}

	t.Run("create over the limit", func(t *testing.T) {
		repo := newFakeWFRepo()
		seed(repo, 50, "t1", "deals", "before_save", true)
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r51", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(true),
		})
		wantBeforeCode(t, err, "WORKFLOW_BEFORE_RULE_LIMIT")
	})

	t.Run("switching a rule back on over the limit", func(t *testing.T) {
		repo := newFakeWFRepo()
		seed(repo, 50, "t1", "deals", "before_save", true)
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		ctx := context.Background()
		rec, err := svc.CreateWorkflow(ctx, "t1", CreateWorkflowInput{
			Name: "r51", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(false),
		})
		if err != nil {
			t.Fatalf("saving it switched off is always allowed: %v", err)
		}
		_, err = svc.UpdateWorkflow(ctx, "t1", rec.ID, UpdateWorkflowInput{
			Name: "r51", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(true),
		})
		wantBeforeCode(t, err, "WORKFLOW_BEFORE_RULE_LIMIT")
	})

	t.Run("re-saving the 50th active rule does not count itself", func(t *testing.T) {
		repo := newFakeWFRepo()
		seed(repo, 50, "t1", "deals", "before_save", true)
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		_, err := svc.UpdateWorkflow(context.Background(), "t1", "seed-t1-deals-before_save-0", UpdateWorkflowInput{
			Name: "renamed", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(true),
		})
		// Past the limit check, and every before rule switches on.
		wantBeforeCode(t, err, "")
	})

	// S3: before_delete runs, so it switches on like before_save (the R13 activation gate is gone),
	// and it is capped on its own count.
	t.Run("before_delete switches on and has its own cap", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		ctx := context.Background()
		rec, err := svc.CreateWorkflow(ctx, "t1", CreateWorkflowInput{
			Name: "del", EntityName: "deals", Trigger: "before_delete", Actions: veto,
		})
		wantBeforeCode(t, err, "")
		if rec == nil || !repo.rows[rec.ID].IsActive {
			t.Fatal("a before_delete rule with isActive omitted must be stored switched on")
		}
		seed(repo, 49, "t1", "deals", "before_delete", true)
		_, err = svc.CreateWorkflow(ctx, "t1", CreateWorkflowInput{
			Name: "del51", EntityName: "deals", Trigger: "before_delete", Actions: veto, IsActive: boolp(true),
		})
		wantBeforeCode(t, err, "WORKFLOW_BEFORE_RULE_LIMIT")
	})

	t.Run("other tenants, other triggers, inactive rules do not count", func(t *testing.T) {
		repo := newFakeWFRepo()
		seed(repo, 50, "t2", "deals", "before_save", true)
		seed(repo, 50, "t1", "deals", "before_delete", true)
		seed(repo, 50, "t1", "deals", "before_save", false)
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(true),
		})
		wantBeforeCode(t, err, "")
	})

	t.Run("a saved-off rule is not limited", func(t *testing.T) {
		repo := newFakeWFRepo()
		seed(repo, 50, "t1", "deals", "before_save", true)
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r51", EntityName: "deals", Trigger: "before_save", Actions: veto, IsActive: boolp(false),
		})
		wantBeforeCode(t, err, "")
	})
}

// setValueCatalog: one field of every type a before_save field_update may or may not target.
func setValueCatalog() *fakeWFCatalog {
	f := func(name, typ string, opts *string) metadata.FieldDefinition {
		return metadata.FieldDefinition{ID: "f-" + name, EntityID: "ent-sv", Name: name, FieldType: typ, Options: opts}
	}
	return &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-sv", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "f-name", EntityID: "ent-sv", Name: "name", FieldType: "text", IsRequired: true},
			f("stage", "picklist", strPtr(`{"values":["Proposal","Closed Won"]}`)),
			f("source", "picklist", strPtr(`{"values":["Web","Event"]}`)),
			f("probability", "number", strPtr(`{"min":0,"max":100}`)),
			f("close_date", "date", nil),
			f("note", "text", nil),
			f("email", "email", nil),
			f("summary", "rich_text", nil),
			f("amount", "currency", strPtr(`{"currencyCode":"VND"}`)),
			f("budget", "currency", strPtr(`{"currencyCode":"VND"}`)),
			f("account", "relation", strPtr(`{"relatedEntity":"accounts"}`)),
			f("related", "polymorphic_relation", strPtr(`{"targetEntities":["accounts"]}`)),
			f("assignee", "user", nil),
			f("docs", "file", nil),
			f("photo", "image", nil),
			f("weighted", "formula", strPtr(`{"formula":"probability * 2"}`)),
			f("industry", "lookup", strPtr(`{"via":"account","sourceField":"industry"}`)),
			f("contacts", "rollup", strPtr(`{"metric":"count","childEntity":"contacts","relationship":"r"}`)),
			f("code", "auto_number", strPtr(`{"prefix":"D-"}`)),
		},
	}
}

// T077 (specs/048 S4): a before_save field_update is checked when the rule is saved — the target
// (never a field another mechanism owns: a link, a person, a file, a computed value, a sequence),
// the value (typed and in range for its field, the way a record save would judge it), the source
// (a real field of the same type; a template only into text). A rule stored past these would fail at
// every save; better it never saves. onlyIfEmpty/valueSource mean nothing on an after_* rule, so they
// are refused there instead of dropped, and an after_* field_update without them passes as before.
func TestBuildWorkflowRule_BeforeFieldUpdate(t *testing.T) {
	set := func(field string, value any) WorkflowActionInput {
		return WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: field, Value: value}
	}
	src := func(field, source string, value any) WorkflowActionInput {
		return WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: field, Value: value, ValueSource: source}
	}
	tests := []struct {
		name       string
		trigger    string
		saveOn     string
		actions    []WorkflowActionInput
		wantCode   string
		wantField  string
		wantReason string
	}{
		{name: "constant number", trigger: "before_save", actions: []WorkflowActionInput{set("probability", 100.0)}},
		{name: "constant picklist option", trigger: "before_save",
			actions: []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true}}},
		{name: "constant null clears an optional field", trigger: "before_save", actions: []WorkflowActionInput{set("close_date", nil)}},
		{name: "explicit constant with a leading $", trigger: "before_save", actions: []WorkflowActionInput{src("note", "constant", "$record.name")}},
		{name: "copy a field of the same type", trigger: "before_save", actions: []WorkflowActionInput{src("budget", "field", "amount")}},
		{name: "template into text", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{name}} — {{stage}}")}},
		{name: "template into email", trigger: "before_save", actions: []WorkflowActionInput{src("email", "template", "{{name}}@example.com")}},
		{name: "ten value changes", trigger: "before_save", actions: []WorkflowActionInput{
			set("probability", 1.0), set("probability", 2.0), set("probability", 3.0), set("probability", 4.0), set("probability", 5.0),
			set("probability", 6.0), set("probability", 7.0), set("probability", 8.0), set("probability", 9.0), set("probability", 10.0)}},

		{name: "target relation", trigger: "before_save", actions: []WorkflowActionInput{set("account", "a-1")},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "account"},
		{name: "target polymorphic relation", trigger: "before_save", actions: []WorkflowActionInput{set("related", nil)},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "related"},
		{name: "target user", trigger: "before_save", actions: []WorkflowActionInput{set("assignee", "u-1")},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "assignee"},
		{name: "target file", trigger: "before_save", actions: []WorkflowActionInput{set("docs", nil)},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "docs"},
		{name: "target image", trigger: "before_save", actions: []WorkflowActionInput{set("photo", nil)},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "photo"},
		{name: "target formula", trigger: "before_save", actions: []WorkflowActionInput{set("weighted", 1.0)},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "weighted"},
		{name: "target lookup", trigger: "before_save", actions: []WorkflowActionInput{set("industry", "x")},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "industry"},
		{name: "target rollup", trigger: "before_save", actions: []WorkflowActionInput{set("contacts", 1.0)},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "contacts"},
		{name: "target auto_number", trigger: "before_save", actions: []WorkflowActionInput{set("code", "D-9")},
			wantCode: "WORKFLOW_BEFORE_TARGET_UNSUPPORTED", wantField: "code"},
		{name: "target unknown", trigger: "before_save", actions: []WorkflowActionInput{set("closing_day", "x")},
			wantCode: "RECORD_UNKNOWN_FIELD", wantField: "closing_day"},

		{name: "constant of the wrong type", trigger: "before_save", actions: []WorkflowActionInput{set("probability", "cao")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "probability", wantReason: "RECORD_INVALID_TYPE"},
		{name: "constant out of range", trigger: "before_save", actions: []WorkflowActionInput{set("probability", 101.0)},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "probability", wantReason: "RECORD_INVALID_RANGE"},
		{name: "picklist option that does not exist", trigger: "before_save", actions: []WorkflowActionInput{set("source", "Website")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "source", wantReason: "RECORD_INVALID_PICKLIST"},
		{name: "clearing a required field", trigger: "before_save", actions: []WorkflowActionInput{set("name", nil)},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "name", wantReason: "RECORD_REQUIRED_MISSING"},
		{name: "template into a number", trigger: "before_save", actions: []WorkflowActionInput{src("probability", "template", "{{name}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "probability", wantReason: "template_text_only"},
		{name: "template that is not text", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", 5.0)},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_text_only"},
		{name: "copy from a field of another type", trigger: "before_save", actions: []WorkflowActionInput{src("note", "field", "probability")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "source_type_mismatch"},
		{name: "copy from a field that does not exist", trigger: "before_save", actions: []WorkflowActionInput{src("note", "field", "subject")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "source_unknown"},
		{name: "unknown value source", trigger: "before_save", actions: []WorkflowActionInput{src("note", "formula", "x")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "value_source_invalid"},

		// A {{placeholder}} renders only a stored field of the entity whose value reads as text. Anything
		// else renders "" at every save — and a blank template would have cleared the target.
		{name: "template of numbers, picklists and dates", trigger: "before_save",
			actions: []WorkflowActionInput{src("note", "template", "{{name}}: {{probability}}% {{stage}} {{close_date}}")}},
		{name: "template of the sequence on an update-only rule", trigger: "before_save", saveOn: "update",
			actions: []WorkflowActionInput{src("note", "template", "{{code}} {{name}}")}},
		{name: "template placeholder that is not a field", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{nmae}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unknown"},
		{name: "template placeholder through a relation", trigger: "before_save",
			actions:  []WorkflowActionInput{src("note", "template", "Khách {{account.name}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unknown"},
		{name: "template placeholder of a lookup", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{industry}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template placeholder of a rollup", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{contacts}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template placeholder of a formula", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{weighted}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template placeholder of a currency object", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{amount}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template placeholder of a relation id", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "{{account}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template of the sequence on a rule that runs on create", trigger: "before_save",
			actions:  []WorkflowActionInput{src("note", "template", "{{code}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "template of the sequence on a create-only rule", trigger: "before_save", saveOn: "create",
			actions:  []WorkflowActionInput{src("note", "template", "{{code}}")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_source_unsupported"},
		{name: "blank template", trigger: "before_save", actions: []WorkflowActionInput{src("note", "template", "  ")},
			wantCode: "WORKFLOW_BEFORE_VALUE_INVALID", wantField: "note", wantReason: "template_empty"},
		{name: "eleven value changes", trigger: "before_save", actions: []WorkflowActionInput{
			set("probability", 1.0), set("probability", 2.0), set("probability", 3.0), set("probability", 4.0), set("probability", 5.0),
			set("probability", 6.0), set("probability", 7.0), set("probability", 8.0), set("probability", 9.0), set("probability", 10.0),
			set("probability", 11.0)}, wantCode: "WORKFLOW_TOO_MANY_ACTIONS"},

		// specs/058: a before rule may reshape only the record being saved — never a related one.
		{name: "field_update through a relation", trigger: "before_save",
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "name", Value: "x", Via: "account"}},
			wantCode: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER"},

		{name: "onlyIfEmpty on an after rule", trigger: "after_update",
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "valueSource on an after rule", trigger: "after_create", actions: []WorkflowActionInput{src("note", "template", "{{name}}")},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "after rule field_update unchanged", trigger: "after_update", actions: []WorkflowActionInput{set("probability", 100.0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(setValueCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "deals", Trigger: tt.trigger, SaveOn: tt.saveOn, Actions: tt.actions, IsActive: boolp(false),
			})
			ve := wantBeforeCode(t, err, tt.wantCode)
			if tt.wantField != "" && (ve.Field != tt.wantField || ve.Details["field"] != tt.wantField) {
				t.Fatalf("field = %q details=%v, want %q", ve.Field, ve.Details, tt.wantField)
			}
			if tt.wantReason != "" && ve.Details["reason"] != tt.wantReason {
				t.Fatalf("reason = %v (%q), want %q", ve.Details["reason"], ve.Msg, tt.wantReason)
			}
		})
	}
}

// The persisted form of a before_save field_update keeps onlyIfEmpty and valueSource (trimmed), and the
// constant as it was sent — the runtime reads exactly what was checked.
func TestBuildWorkflowRule_BeforeFieldUpdatePersistedShape(t *testing.T) {
	svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(setValueCatalog())
	rec, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "r", EntityName: "deals", Trigger: "before_save", IsActive: boolp(false),
		Actions: []WorkflowActionInput{
			{Type: workflow.ActionFieldUpdate, Field: " source ", Value: "Web", OnlyIfEmpty: true},
			{Type: workflow.ActionFieldUpdate, Field: "note", Value: "{{name}}", ValueSource: " template "},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	var acts []map[string]any
	if err := json.Unmarshal(rec.Actions, &acts); err != nil {
		t.Fatalf("actions %s: %v", rec.Actions, err)
	}
	want := `[map[field:source onlyIfEmpty:true type:field_update value:Web] map[field:note type:field_update value:{{name}} valueSource:template]]`
	if fmt.Sprint(acts) != want {
		t.Fatalf("actions = %v\nwant      %s", acts, want)
	}
}
