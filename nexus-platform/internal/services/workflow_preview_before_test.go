package services

// specs/048 S6 — the dry-run (POST /admin/workflows/:id/test) of a before rule. The admin simulates
// a write — op (create/update) plus the fields it changes — and reads what the rule WOULD do to it:
// "Sẽ CHẶN: …" for a veto, the value before → after for a fill or a normalize. The simulated image is
// the record merged with the changes (formulas recomputed) and the edge operators compare it with the
// record as stored, so a changedTo rule — before or after — is finally something one can rehearse.
// Nothing is written: no record, no rule, no run.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

const previewVetoMsg = "Chốt Thắng phải có Ngày chốt"

// previewSvc is the dry-run over setValueCatalog's deals with ONE stored record (r1).
func previewSvc(t *testing.T, data map[string]any) (*WorkflowService, *fakeWFRepo, *fakeRecordReader, *fakeWFCatalog) {
	t.Helper()
	repo := newFakeWFRepo()
	cat := setValueCatalog()
	reader := &fakeRecordReader{rec: &dynrepo.Record{ID: "r1", TenantID: "t1", Data: data}}
	return NewWorkflowService(repo).WithEntityChecker(cat).WithRecordReader(reader), repo, reader, cat
}

// previewRule saves one before rule (switched off: the dry-run is how a rule is judged BEFORE it goes
// live — US5 #3) and returns its id.
func previewRule(t *testing.T, svc *WorkflowService, trigger, saveOn string, rules []WorkflowRuleInput, actions ...WorkflowActionInput) string {
	t.Helper()
	off := false
	wf, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: trigger + "/" + actions[0].Type + "/" + actions[0].Field, EntityName: "deals", Trigger: trigger, SaveOn: saveOn,
		Rules: rules, Actions: actions, IsActive: &off, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("seed %s rule: %v", trigger, err)
	}
	return wf.ID
}

// lastAction is the trace row of the rule's last action.
func lastAction(t *testing.T, got *WorkflowTestResult) map[string]any {
	t.Helper()
	st := got.Trace[len(got.Trace)-1]
	if st.Kind != "action" || st.Evaluated == nil {
		t.Fatalf("last trace step %+v is not an action row", st)
	}
	return st.Detail
}

func wantDetail(t *testing.T, detail map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(detail[k], v) {
			t.Errorf("detail[%q] = %#v, want %#v (detail %v)", k, detail[k], v, detail)
		}
	}
}

func previewStored() map[string]any {
	return map[string]any{"name": "Deal A", "stage": "Proposal", "probability": 40.0, "source": "Web", "note": "  Nguyễn   Văn  An "}
}

// T104: every kind of before action previews on record ⊕ changes.
func TestTestWorkflow_BeforeSave_ChangesPreview(t *testing.T) {
	closeWon := []WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}, {Field: "close_date", Op: workflow.OpIsNull}}
	veto := WorkflowActionInput{Type: workflow.ActionVeto, Field: "close_date", Message: previewVetoMsg}
	tests := []struct {
		name        string
		trigger     string
		saveOn      string
		rules       []WorkflowRuleInput
		action      WorkflowActionInput
		in          WorkflowTestInput
		wantMatched bool
		want        map[string]any
	}{
		{name: "veto on the simulated change would block", trigger: "before_save", saveOn: "update", rules: closeWon, action: veto,
			in: WorkflowTestInput{Changes: map[string]any{"stage": "Closed Won"}}, wantMatched: true,
			want: map[string]any{"type": "veto", "field": "close_date", "message": previewVetoMsg, "wouldBlock": true}},
		{name: "the simulated close date satisfies the veto", trigger: "before_save", saveOn: "update", rules: closeWon, action: veto,
			in:   WorkflowTestInput{Changes: map[string]any{"stage": "Closed Won", "close_date": "2026-10-01"}},
			want: map[string]any{"type": "veto", "wouldBlock": false}},
		{name: "no change: changedTo has nothing to have changed", trigger: "before_save", saveOn: "update", rules: closeWon, action: veto,
			want: map[string]any{"wouldBlock": false}},
		{name: "an update-only rule does not run on a simulated create", trigger: "before_save", saveOn: "update",
			rules: []WorkflowRuleInput{{Field: "close_date", Op: workflow.OpIsNull}}, action: veto,
			in:   WorkflowTestInput{Op: "create", Changes: map[string]any{"stage": "Closed Won"}},
			want: map[string]any{"wouldBlock": false}},
		{name: "a veto judges formulas recomputed on the change", trigger: "before_save",
			rules:  []WorkflowRuleInput{{Field: "weighted", Op: workflow.OpGt, Value: 150.0}},
			action: WorkflowActionInput{Type: workflow.ActionVeto, Message: "Quá hạn mức"},
			in:     WorkflowTestInput{Changes: map[string]any{"probability": 80.0}}, wantMatched: true,
			want: map[string]any{"wouldBlock": true}},
		{name: "fill shows before and after", trigger: "before_save",
			rules:  []WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}},
			action: WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0},
			in:     WorkflowTestInput{Changes: map[string]any{"stage": "Closed Won", "close_date": "2026-10-01"}}, wantMatched: true,
			want: map[string]any{"type": "field_update", "field": "probability", "before": 40.0, "after": 100.0, "wouldApply": true}},
		{name: "onlyIfEmpty on a filled field writes nothing", trigger: "before_save",
			action:      WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Event", OnlyIfEmpty: true},
			wantMatched: true,
			want:        map[string]any{"before": "Web", "after": "Web", "wouldApply": false}},
		{name: "normalize rewrites the stored text", trigger: "before_save",
			action:      WorkflowActionInput{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim", "collapse_spaces"}},
			wantMatched: true,
			want:        map[string]any{"type": "normalize", "before": "  Nguyễn   Văn  An ", "after": "Nguyễn Văn An", "wouldApply": true}},
		{name: "normalize reads the simulated change", trigger: "before_save",
			action: WorkflowActionInput{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim", "upper"}},
			in:     WorkflowTestInput{Changes: map[string]any{"note": " ha nội "}}, wantMatched: true,
			want: map[string]any{"before": " ha nội ", "after": "HA NỘI", "wouldApply": true}},
		{name: "a delete rule judges the stored record", trigger: "before_delete",
			rules:  []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Proposal"}},
			action: WorkflowActionInput{Type: workflow.ActionVeto, Message: "Không xoá"}, wantMatched: true,
			want: map[string]any{"type": "veto", "message": "Không xoá", "wouldBlock": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := previewSvc(t, previewStored())
			id := previewRule(t, svc, tt.trigger, tt.saveOn, tt.rules, tt.action)
			in := tt.in
			in.RecordID = "r1"
			got, err := svc.DryRun(context.Background(), "t1", id, in)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if got.Matched != tt.wantMatched {
				t.Errorf("Matched = %v, want %v (trace %+v)", got.Matched, tt.wantMatched, got.Trace)
			}
			wantDetail(t, lastAction(t, got), tt.want)
		})
	}
}

// T104: a fill whose value the validator now refuses (the option was removed after the rule was saved)
// previews the error the save would record, never "would apply".
func TestTestWorkflow_BeforeSave_BrokenValuePreviewsError(t *testing.T) {
	svc, _, _, cat := previewSvc(t, map[string]any{"name": "Deal A"})
	id := previewRule(t, svc, "before_save", "", nil, WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web"})
	for i, f := range cat.fields {
		if f.Name == "source" {
			cat.fields[i].Options = strPtr(`{"values":["Event"]}`)
		}
	}
	got, err := svc.DryRun(context.Background(), "t1", id, WorkflowTestInput{RecordID: "r1"})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	wantDetail(t, lastAction(t, got), map[string]any{"error": "RECORD_INVALID_PICKLIST", "wouldApply": false})
}

// T105: the after_update dry-run evaluated edges with no before-image, so changedTo was always false.
func TestTestWorkflow_AfterUpdate_ChangedToWithChanges(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"stage": "new"}}
	svc, wfID, _ := w6DryRunSvc(t,
		[]WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "won"}},
		[]WorkflowActionInput{{Type: "in_app_notification", UserID: "u9", Message: "hi"}}, rec)
	for _, tt := range []struct {
		name    string
		changes map[string]any
		want    bool
	}{
		{"the simulated change lands on won", map[string]any{"stage": "won"}, true},
		{"no change", nil, false},
		{"a change to the value it already has", map[string]any{"stage": "new"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.DryRun(context.Background(), "t1", wfID, WorkflowTestInput{RecordID: "r1", Changes: tt.changes})
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if got.Matched != tt.want {
				t.Fatalf("Matched = %v, want %v (trace %+v)", got.Matched, tt.want, got.Trace)
			}
			if got.Op != "update" {
				t.Errorf("Op = %q, want update (an after_update rule simulates an update)", got.Op)
			}
			if cond := got.Trace[1].Detail; cond["before"] != "new" {
				t.Errorf("condition detail %v: want the stored value as before", cond)
			}
		})
	}
}

// T106: a create needs no stored record — the image is the simulated change plus what the create's
// validator fills in (ValidateRecord: the default of every writable field left empty), so the
// rehearsal judges the record the real create would judge. A create-only rule (saveOn=create)
// simulates a create when no op is sent: the rule's own write, which needs no record either.
func TestTestWorkflow_OpCreate_NoRecordId(t *testing.T) {
	closeWon := []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}, {Field: "close_date", Op: workflow.OpIsNull}}
	veto := WorkflowActionInput{Type: workflow.ActionVeto, Field: "close_date", Message: previewVetoMsg}
	atProposal := []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Proposal"}}
	vetoProposal := WorkflowActionInput{Type: workflow.ActionVeto, Message: "Không tạo mới ở Proposal"}
	tests := []struct {
		name        string
		saveOn      string
		rules       []WorkflowRuleInput
		action      WorkflowActionInput
		defaults    map[string]string // field → DefaultVal set on the catalog
		in          WorkflowTestInput
		wantMatched bool
		want        map[string]any
	}{
		{name: "the image is the simulated change", rules: closeWon, action: veto,
			in: WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới", "stage": "Closed Won"}}, wantMatched: true,
			want: map[string]any{"wouldBlock": true}},
		{name: "a field left empty takes its default, as the create fills it", saveOn: "create", rules: atProposal, action: vetoProposal,
			defaults: map[string]string{"stage": "Proposal"},
			in:       WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới"}}, wantMatched: true,
			want: map[string]any{"wouldBlock": true}},
		{name: "an explicit null takes the default too", saveOn: "create", rules: atProposal, action: vetoProposal,
			defaults: map[string]string{"stage": "Proposal"},
			in:       WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới", "stage": nil}}, wantMatched: true,
			want: map[string]any{"wouldBlock": true}},
		{name: "a sent value beats the default", saveOn: "create", rules: atProposal, action: vetoProposal,
			defaults: map[string]string{"stage": "Proposal"},
			in:       WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới", "stage": "Closed Won"}},
			want:     map[string]any{"wouldBlock": false}},
		{name: "onlyIfEmpty sees the default already in the cell", saveOn: "create",
			action:   WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Event", OnlyIfEmpty: true},
			defaults: map[string]string{"source": "Web"},
			in:       WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới"}}, wantMatched: true,
			want: map[string]any{"before": "Web", "after": "Web", "wouldApply": false}},
		{name: "a create-only rule simulates a create without op or record", saveOn: "create", rules: closeWon, action: veto,
			in: WorkflowTestInput{Changes: map[string]any{"name": "Mới", "stage": "Closed Won"}}, wantMatched: true,
			want: map[string]any{"wouldBlock": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, reader, cat := previewSvc(t, previewStored())
			for i, f := range cat.fields {
				if dv, ok := tt.defaults[f.Name]; ok {
					cat.fields[i].DefaultVal = dv
				}
			}
			id := previewRule(t, svc, "before_save", tt.saveOn, tt.rules, tt.action)
			got, err := svc.DryRun(context.Background(), "t1", id, tt.in)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if got.RecordID != "" || got.Op != "create" || got.Matched != tt.wantMatched {
				t.Fatalf("header = recordId %q op %q matched %v; want \"\" / create / %v (trace %+v)",
					got.RecordID, got.Op, got.Matched, tt.wantMatched, got.Trace)
			}
			wantDetail(t, lastAction(t, got), tt.want)
			if reader.reads != 0 {
				t.Errorf("record reads = %d, want 0 (no recordId)", reader.reads)
			}
		})
	}
}

// A before_save rule narrowed to creates is rehearsed on a create by default — even with a record
// named (the stored values seed the simulated new record): judging it on an update would say "the
// rule does not run" while every real create is refused.
func TestTestWorkflow_SaveOnCreate_DefaultsToCreate(t *testing.T) {
	svc, _, _, _ := previewSvc(t, previewStored())
	id := previewRule(t, svc, "before_save", "create",
		[]WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}},
		WorkflowActionInput{Type: workflow.ActionVeto, Message: "Không tạo mới ở Closed Won"})
	got, err := svc.DryRun(context.Background(), "t1", id, WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"stage": "Closed Won"}})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if got.Op != "create" || !got.Matched {
		t.Fatalf("op %q matched %v, want create / true (trace %+v)", got.Op, got.Matched, got.Trace)
	}
	if trig := got.Trace[0]; trig.Kind != "trigger" || trig.Matched == nil || !*trig.Matched || trig.Detail["op"] != "create" {
		t.Errorf("trigger step %+v, want matched on op create", trig)
	}
	wantDetail(t, lastAction(t, got), map[string]any{"wouldBlock": true})
}

// T106: what a save would refuse, the simulation refuses — before any preview is drawn.
func TestTestWorkflow_ChangesInvalid422(t *testing.T) {
	tests := []struct {
		name       string
		trigger    string
		defaults   map[string]string // field → DefaultVal set on the catalog
		in         WorkflowTestInput
		wantCode   string
		wantField  string
		wantReason string
	}{
		{name: "unknown field", trigger: "before_save", in: WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"ghost": 1.0}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID", wantField: "ghost", wantReason: "RECORD_UNKNOWN_FIELD"},
		{name: "wrong type", trigger: "before_save", in: WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"probability": "abc"}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID", wantField: "probability", wantReason: "RECORD_INVALID_TYPE"},
		{name: "computed field", trigger: "before_save", in: WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"weighted": 1.0}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID", wantField: "weighted", wantReason: "RECORD_FORMULA_READONLY"},
		{name: "blanking a required field", trigger: "before_save", in: WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"name": nil}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID", wantField: "name", wantReason: "RECORD_REQUIRED_MISSING"},
		{name: "a delete changes no field", trigger: "before_delete", in: WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"stage": "Closed Won"}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID"},
		{name: "unknown op", trigger: "before_save", in: WorkflowTestInput{RecordID: "r1", Op: "delete"},
			wantCode: "WORKFLOW_TEST_OP_INVALID", wantField: "op"},
		{name: "update without a record", trigger: "before_save", in: WorkflowTestInput{Op: "update"},
			wantCode: "WORKFLOW_TEST_RECORD_REQUIRED"},
		{name: "default op without a record", trigger: "before_save", in: WorkflowTestInput{},
			wantCode: "WORKFLOW_TEST_RECORD_REQUIRED"},
		{name: "a default the create would refuse (legacy row)", trigger: "before_save", defaults: map[string]string{"probability": "abc"},
			in:       WorkflowTestInput{Op: "create", Changes: map[string]any{"name": "Mới"}},
			wantCode: "WORKFLOW_TEST_CHANGES_INVALID", wantField: "probability", wantReason: "RECORD_INVALID_TYPE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, cat := previewSvc(t, previewStored())
			for i, f := range cat.fields {
				if dv, ok := tt.defaults[f.Name]; ok {
					cat.fields[i].DefaultVal = dv
				}
			}
			id := previewRule(t, svc, tt.trigger, "", nil, WorkflowActionInput{Type: workflow.ActionVeto, Message: "chặn"})
			_, err := svc.DryRun(context.Background(), "t1", id, tt.in)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
			if tt.wantField != "" && ve.Details["field"] != tt.wantField {
				t.Errorf("details.field = %v, want %s", ve.Details["field"], tt.wantField)
			}
			if tt.wantReason != "" && ve.Details["reason"] != tt.wantReason {
				t.Errorf("details.reason = %v, want %s", ve.Details["reason"], tt.wantReason)
			}
		})
	}
}

// T107: the dry-run of every kind of before rule reads the rule and the record and writes nothing —
// not the record (its data map is shared with the fake store, so an in-place edit shows), not the rule.
func TestTestWorkflow_Before_ZeroSideEffect(t *testing.T) {
	stored := previewStored()
	snapshot := cloneData(stored)
	svc, repo, reader, _ := previewSvc(t, stored)
	ids := []string{
		previewRule(t, svc, "before_save", "", []WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}},
			WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0}),
		previewRule(t, svc, "before_save", "", nil, WorkflowActionInput{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim"}}),
		previewRule(t, svc, "before_save", "update", []WorkflowRuleInput{{Field: "close_date", Op: workflow.OpIsNull}},
			WorkflowActionInput{Type: workflow.ActionVeto, Field: "close_date", Message: previewVetoMsg}),
	}
	rules := map[string]string{}
	for _, id := range ids {
		r := repo.rows[id]
		rules[id] = string(r.Condition) + string(r.Actions)
	}
	created := len(repo.created)
	for _, id := range ids {
		if _, err := svc.DryRun(context.Background(), "t1", id, WorkflowTestInput{RecordID: "r1", Changes: map[string]any{"stage": "Closed Won", "note": " x "}}); err != nil {
			t.Fatalf("DryRun %s: %v", id, err)
		}
	}
	if !reflect.DeepEqual(stored, snapshot) {
		t.Errorf("stored record changed by a dry-run:\n got %v\nwant %v", stored, snapshot)
	}
	if reader.reads != len(ids) {
		t.Errorf("record reads = %d, want %d (one per dry-run)", reader.reads, len(ids))
	}
	if len(repo.created) != created {
		t.Errorf("a dry-run created a rule")
	}
	for _, id := range ids {
		r := repo.rows[id]
		if got := string(r.Condition) + string(r.Actions); got != rules[id] || r.IsActive {
			t.Errorf("rule %s changed by its dry-run: %s (active %v)", id, got, r.IsActive)
		}
	}
}
