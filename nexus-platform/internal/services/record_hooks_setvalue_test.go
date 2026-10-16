package services

// specs/048 S4 — before_save field_update ("tự điền") inside Create and Update, over fakes (no
// MySQL): the value rides the ONE store call of the save, formulas are recomputed from it, a veto
// judges the record with it, a value the record validator refuses is dropped alone (the save goes
// on, the rule's run says failed), a required field is never blanked, and a field the caller's role
// may not edit still takes the rule's value. record_hooks_setvalue_integration_test.go proves the
// update path on real MySQL.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// leadsCatalog: a required full_name and a source picklist whose options the test picks (so a rule
// can outlive the option it writes).
func leadsCatalog(sourceOptions string) *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-l", TenantID: "t1", Name: "leads", DataTable: "obj_leads"},
		fields: []metadata.FieldDefinition{
			{Name: "full_name", FieldType: "text", IsRequired: true},
			{Name: "company", FieldType: "text"},
			{Name: "source", FieldType: "picklist", Options: strPtr(`{"values":` + sourceOptions + `}`)},
		},
	}
}

// fillRule is an active before_save rule with no condition whose actions all change values.
func fillRule(id, entity string, actions ...workflow.Action) workflow.Workflow {
	return workflow.Workflow{ID: id, TenantID: "t1", EntityName: entity, Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Actions: actions}
}

func webIfEmpty() workflow.Action {
	return workflow.Action{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true}
}

// wonProb100 is the US6 rule: stage changedTo Closed Won ⇒ probability = 100.
func wonProb100(entity string) workflow.Workflow {
	wf := fillRule("rule-p100-"+entity, entity, workflow.Action{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0})
	wf.Condition = &workflow.Condition{Rules: []workflow.Rule{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}}}
	return wf
}

func runSummary(runs []workflow.RunInput) string {
	out := ""
	for _, r := range runs {
		out += fmt.Sprintf("[%s %s rec=%s err=%s %s]", r.WorkflowID, r.Status, r.RecordID, r.Error, r.ActionsResult)
	}
	return out
}

// T076: onlyIfEmpty (workflow.IsBlank) and the record validator's "not provided" (isEmpty) must read
// "empty" the same way — one table through both.
func TestIsBlank_ParityWithServicesIsEmpty(t *testing.T) {
	for _, v := range []any{nil, "", "   ", "\t\n", "x", " x ", 0.0, 1.0, false, true,
		[]any{}, []any{"a"}, map[string]any{}, map[string]any{"amount": 1.0}} {
		if workflow.IsBlank(v) != isEmpty(v) {
			t.Errorf("%#v: workflow.IsBlank = %v, services.isEmpty = %v", v, workflow.IsBlank(v), isEmpty(v))
		}
	}
}

// T079: "a lead with no source is a Website lead" — the rule fills it on create; a source the user
// chose is kept. The run says success, points at the record created, and names only the field.
func TestRecordCreate_BeforeSaveSetValue(t *testing.T) {
	sink, drain := newRunSink(t)
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
	svc := NewRecordService(leadsCatalog(`["Web","Event"]`), store).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-web", "leads", webIfEmpty())}}, sink)
	ctx := context.Background()

	id, err := svc.Create(ctx, rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An"})
	if err != nil {
		t.Fatalf("Create without source: %v", err)
	}
	if got, _ := store.at(0); got.data["source"] != "Web" {
		t.Fatalf("stored source = %v, want Web", got.data["source"])
	}
	if _, err := svc.Create(ctx, rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "Bình", "source": "Event"}); err != nil {
		t.Fatalf("Create with source: %v", err)
	}
	if got, _ := store.at(1); got.data["source"] != "Event" {
		t.Fatalf("stored source = %v, want Event kept", got.data["source"])
	}
	runs := drain()
	if len(runs) != 1 || runs[0].Status != workflow.RunSuccess || runs[0].RecordID != id ||
		runs[0].TriggerType != "before_save" || string(runs[0].ActionsResult) != `[{"type":"field_update","field":"source","ok":true}]` {
		t.Fatalf("runs = %s, want one success run on %s (the create that kept Event changed nothing)", runSummary(runs), id)
	}
}

// T078 (unit half): the rule's value, the formula computed from it and the user's own change leave in
// ONE store call, and the audit diff of that save carries both fields.
func TestRecordUpdate_BeforeSaveSetValue(t *testing.T) {
	sink, drain := newRunSink(t)
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
		before: map[string]any{"name": "Deal", "stage": "Proposal", "amount": 1000.0, "probability": 40.0, "weighted": 400.0}}
	spy := &spyAuditor{}
	svc := NewRecordService(oppsCatalog(), store).WithAuditor(spy).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonProb100("opps")}}, sink)
	if err := svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, updates := store.counts(); updates != 1 {
		t.Fatalf("UpdateRecord calls = %d, want 1", updates)
	}
	got := store.updates[0]
	if got["stage"] != "Closed Won" || got["probability"] != 100.0 || got["weighted"] != 1000.0 {
		t.Fatalf("written = %v, want stage Closed Won, probability 100, weighted 1000", got)
	}
	if len(spy.events) != 1 || spy.events[0].Diff == nil || fmt.Sprint(keysOf(spy.events[0].Diff.After)) != "[probability stage weighted]" {
		t.Fatalf("audit = %+v, want one update event over stage + probability (+ the formula)", spy.events)
	}
	runs := drain()
	if len(runs) != 1 || runs[0].Status != workflow.RunSuccess || runs[0].RecordID != "o1" {
		t.Fatalf("runs = %s, want one success run on o1", runSummary(runs))
	}

	t.Run("an update the rule does not match writes only what the user sent", func(t *testing.T) {
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "Deal", "stage": "Closed Won", "probability": 40.0}}
		svc := NewRecordService(oppsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonProb100("opps")}}, nil)
		if err := svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"name": "Renamed"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, has := store.updates[0]["probability"]; has {
			t.Fatalf("written = %v — changedTo did not hold, probability must not be written", store.updates[0])
		}
	})
}

// keysOf returns the sorted keys of a map.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// T080: the rule was saved when "Web" was an option; the option is gone now. The save still succeeds,
// source is simply not written, and the rule's run says failed with the validator's code — never
// the value.
func TestBeforeSave_InvalidRuntimeValue_SkippedRunFailed(t *testing.T) {
	sink, drain := newRunSink(t)
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
	fill := fillRule("rule-web", "leads", webIfEmpty(), workflow.Action{Type: workflow.ActionFieldUpdate, Field: "company", Value: "Khách web"})
	svc := NewRecordService(leadsCatalog(`["Referral","Event"]`), store).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fill}}, sink)
	id, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An"})
	if err != nil {
		t.Fatalf("Create: %v — a rule value gone stale must not refuse the save", err)
	}
	got, _ := store.at(0)
	if _, has := got.data["source"]; has || got.data["company"] != "Khách web" {
		t.Fatalf("stored = %v, want no source and the rule's other value kept", got.data)
	}
	runs := drain()
	want := `[{"type":"field_update","field":"source","ok":false,"error":"RECORD_INVALID_PICKLIST"},{"type":"field_update","field":"company","ok":true}]`
	if len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].RecordID != id ||
		runs[0].Error != "RECORD_INVALID_PICKLIST" || string(runs[0].ActionsResult) != want {
		t.Fatalf("runs = %s, want one failed run with the code", runSummary(runs))
	}
}

// T081: a veto judges the record the value-changing rules left — the rule that fills probability
// keeps the "probability 100 when Won" veto from refusing a save it would otherwise refuse (US6.5).
func TestBeforeSave_VetoSeesMutatedImage(t *testing.T) {
	veto := workflow.Workflow{ID: "rule-veto", TenantID: "t1", EntityName: "opps", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{Rules: []workflow.Rule{
			{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}, {Field: "probability", Op: workflow.OpLt, Value: 100.0}}},
		Actions: []workflow.Action{{Type: workflow.ActionVeto, Field: "probability", Message: "Thắng thì xác suất phải 100"}}}
	for _, tc := range []struct {
		name  string
		rules []workflow.Workflow
		veto  bool
	}{
		{"with the filling rule the save goes through", []workflow.Workflow{wonProb100("opps"), veto}, false},
		{"without it the same save is refused", []workflow.Workflow{veto}, true},
		{"rule order does not matter: the veto always reads the final record", []workflow.Workflow{veto, wonProb100("opps")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "Deal", "stage": "Proposal", "probability": 40.0}}
			svc := NewRecordService(oppsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: tc.rules}, sink)
			err := svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil)
			if tc.veto {
				wantVeto(t, err)
				return
			}
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if store.updates[0]["probability"] != 100.0 {
				t.Fatalf("written = %v", store.updates[0])
			}
			if runs := drain(); len(runs) != 1 || runs[0].WorkflowID != "rule-p100-opps" {
				t.Fatalf("runs = %s, want only the filling rule's success run", runSummary(runs))
			}
		})
	}

	t.Run("a refused save leaves no success run for the rule that filled a value", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "Deal", "stage": "Proposal"}}
		svc := NewRecordService(oppsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonProb100("opps"), wonNeedsDateOn("opps", "")}}, sink)
		wantVeto(t, svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil))
		runs := drain()
		if len(runs) != 1 || runs[0].Status != workflow.RunBlocked {
			t.Fatalf("runs = %s, want just the blocked run — nothing was written", runSummary(runs))
		}
	})
}

// T081: a value-changing rule can never blank a required field — the action is dropped (failed run),
// the save goes on with the value the user gave.
func TestBeforeSave_MutationCannotBlankRequired(t *testing.T) {
	for _, blank := range []any{nil, "", "   "} {
		t.Run(fmt.Sprintf("%q", blank), func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
			svc := NewRecordService(leadsCatalog(`["Web"]`), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
				fillRule("rule-blank", "leads", workflow.Action{Type: workflow.ActionFieldUpdate, Field: "full_name", Value: blank})}}, sink)
			if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An"}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got, _ := store.at(0); got.data["full_name"] != "An" {
				t.Fatalf("stored full_name = %v, want An kept", got.data["full_name"])
			}
			if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != "RECORD_REQUIRED_MISSING" {
				t.Fatalf("runs = %s, want one failed run RECORD_REQUIRED_MISSING", runSummary(runs))
			}
		})
	}
}

// T082 (R9): the rule is the tenant's data law, written by Nexus — it may set a field the saving
// user's role may not edit. The user still cannot send that field themselves.
func TestBeforeSave_MutationWritesFieldLockedForRole(t *testing.T) {
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "Deal", "stage": "Proposal", "probability": 40.0}}
	svc := NewRecordService(oppsCatalog(), store).WithFieldACL(&fakeFieldACL{deniedField: "probability"}).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonProb100("opps")}}, nil)
	ctx := context.Background()
	if err := svc.Update(ctx, rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if store.updates[0]["probability"] != 100.0 {
		t.Fatalf("written = %v, want probability 100 from the rule", store.updates[0])
	}
	if err := svc.Update(ctx, rep, "t1", "opps", "o1", map[string]any{"probability": 90.0}, nil); err == nil {
		t.Fatal("the user sent the locked field themselves and it was accepted")
	}
}

// copyFrom is a before_save field_update reading its value from another field (valueSource field /
// template).
func copyFrom(field, source string, value string) workflow.Action {
	return workflow.Action{Type: workflow.ActionFieldUpdate, Field: field, Value: value, ValueSource: source}
}

// A field deleted after the rule was saved is a soft delete: its old value stays in the stored JSON
// and so in the image the rules read. A rule copying it — or rendering it in a template — must not
// write that dead value over the target (update), nor clear what the user typed (create): the action
// is dropped and the run says failed with a code (FR-022), like a target that changed type.
func TestBeforeSave_DeletedSourceFieldFailsTheAction(t *testing.T) {
	for _, action := range []workflow.Action{
		copyFrom("company", workflow.ValueSourceField, "cmnd_cu"),
		copyFrom("company", workflow.ValueSourceTemplate, "Mã {{cmnd_cu}}"),
	} {
		t.Run(action.ValueSource+" on update", func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
				before: map[string]any{"full_name": "An", "company": "Nexus", "cmnd_cu": "012345678"}}
			svc := NewRecordService(leadsCatalog(`["Web"]`), store).
				WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-copy", "leads", action)}}, sink)
			if err := svc.Update(context.Background(), rep, "t1", "leads", "l1", map[string]any{"full_name": "An B"}, nil); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if v, has := store.updates[0]["company"]; has {
				t.Fatalf("written company = %v — the value of a deleted field leaked into a live one", v)
			}
			if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != "WORKFLOW_BEFORE_VALUE_INVALID" {
				t.Fatalf("runs = %s, want one failed run WORKFLOW_BEFORE_VALUE_INVALID", runSummary(runs))
			}
		})
		t.Run(action.ValueSource+" on create", func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
			svc := NewRecordService(leadsCatalog(`["Web"]`), store).
				WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-copy", "leads", action)}}, sink)
			if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An", "company": "Gõ tay"}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got, _ := store.at(0); got.data["company"] != "Gõ tay" {
				t.Fatalf("stored company = %v, want what the user typed", got.data["company"])
			}
			if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != "WORKFLOW_BEFORE_VALUE_INVALID" {
				t.Fatalf("runs = %s, want one failed run WORKFLOW_BEFORE_VALUE_INVALID", runSummary(runs))
			}
		})
	}
}

// A template that renders to nothing — its placeholders are empty on this record, or name nothing the
// rule may read ({{relation.field}}, a typo) — is not a value: it never clears the target field. A live
// placeholder that is merely empty leaves no run (nothing changed); one naming no live field is a
// failed run, so the rule's author can see it.
func TestBeforeSave_TemplateRenderingBlankKeepsTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name, tmpl, wantRun string
	}{
		{"live placeholder empty on the record", "{{source}}", ""},
		{"placeholder through a relation", "{{acount.name}}", "WORKFLOW_BEFORE_VALUE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
				before: map[string]any{"full_name": "An", "company": "Ghi chú viết tay"}}
			svc := NewRecordService(leadsCatalog(`["Web"]`), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
				fillRule("rule-tmpl", "leads", copyFrom("company", workflow.ValueSourceTemplate, tc.tmpl))}}, sink)
			if err := svc.Update(context.Background(), rep, "t1", "leads", "l1", map[string]any{"full_name": "An B"}, nil); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if v, has := store.updates[0]["company"]; has {
				t.Fatalf("written company = %v — a blank template cleared the field", v)
			}
			runs := drain()
			if tc.wantRun == "" {
				if len(runs) != 0 {
					t.Fatalf("runs = %s, want none (nothing changed)", runSummary(runs))
				}
				return
			}
			if len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != tc.wantRun {
				t.Fatalf("runs = %s, want one failed run %s", runSummary(runs), tc.wantRun)
			}
		})
	}
}

// addressCatalog: district is a dependent picklist of province (specs/052).
func addressCatalog() *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-a", TenantID: "t1", Name: "leads", DataTable: "obj_leads"},
		fields: []metadata.FieldDefinition{
			{Name: "full_name", FieldType: "text", IsRequired: true},
			{Name: "province", FieldType: "picklist", Options: strPtr(`{"values":["Hà Nội","Đà Nẵng"]}`)},
			{Name: "district", FieldType: "picklist", Options: strPtr(`{"values":["Ba Đình","Hoàn Kiếm","Hải Châu"],` +
				`"controllingField":"province","dependentValues":{"Hà Nội":["Ba Đình","Hoàn Kiếm"],"Đà Nẵng":["Hải Châu"]}}`)},
		},
	}
}

// FR-014 "kiểm lại các giá trị vừa sửa" includes the dependent picklist pairs a user's save is held
// to: a rule may not store {Đà Nẵng, Ba Đình} that the form refuses (RECORD_DEPENDENT_PICKLIST_INVALID),
// nor move the parent away from the child it leaves behind. The action is dropped, the save goes on,
// the run says failed with that code; a pair that fits is written.
func TestBeforeSave_MutationKeepsDependentPicklistPairs(t *testing.T) {
	baDinh := workflow.Action{Type: workflow.ActionFieldUpdate, Field: "district", Value: "Ba Đình", OnlyIfEmpty: true}
	t.Run("create: a child outside the parent's list is not written", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
		svc := NewRecordService(addressCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-d", "leads", baDinh)}}, sink)
		if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An", "province": "Đà Nẵng"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got, _ := store.at(0); got.data["district"] != nil {
			t.Fatalf("stored = %v — the rule stored a pair the form refuses", got.data)
		}
		if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != "RECORD_DEPENDENT_PICKLIST_INVALID" {
			t.Fatalf("runs = %s, want one failed run RECORD_DEPENDENT_PICKLIST_INVALID", runSummary(runs))
		}
	})
	t.Run("create: a child inside the parent's list is written", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
		svc := NewRecordService(addressCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-d", "leads", baDinh)}}, sink)
		if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads", map[string]any{"full_name": "An", "province": "Hà Nội"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got, _ := store.at(0); got.data["district"] != "Ba Đình" {
			t.Fatalf("stored = %v, want district Ba Đình", got.data)
		}
		if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunSuccess {
			t.Fatalf("runs = %s, want one success run", runSummary(runs))
		}
	})
	t.Run("update: moving the parent away from the stored child is not written", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
			before: map[string]any{"full_name": "An", "province": "Hà Nội", "district": "Ba Đình"}}
		svc := NewRecordService(addressCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
			fillRule("rule-p", "leads", workflow.Action{Type: workflow.ActionFieldUpdate, Field: "province", Value: "Đà Nẵng"})}}, sink)
		if err := svc.Update(context.Background(), rep, "t1", "leads", "l1", map[string]any{"full_name": "An B"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if v, has := store.updates[0]["province"]; has {
			t.Fatalf("written province = %v — the stored district no longer fits it", v)
		}
		if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunFailed || runs[0].Error != "RECORD_DEPENDENT_PICKLIST_INVALID" {
			t.Fatalf("runs = %s, want one failed run RECORD_DEPENDENT_PICKLIST_INVALID", runSummary(runs))
		}
	})
	// Each value is judged as it is applied, so a rule moving both halves of a pair clears the child
	// first (api-conventions §9.3) — every step is then a pair a user could have saved.
	t.Run("update: a rule moving both halves clears the child first", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
			before: map[string]any{"full_name": "An", "province": "Hà Nội", "district": "Ba Đình"}}
		svc := NewRecordService(addressCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{fillRule("rule-move", "leads",
			workflow.Action{Type: workflow.ActionFieldUpdate, Field: "district", Value: nil},
			workflow.Action{Type: workflow.ActionFieldUpdate, Field: "province", Value: "Đà Nẵng"},
			workflow.Action{Type: workflow.ActionFieldUpdate, Field: "district", Value: "Hải Châu"})}}, sink)
		if err := svc.Update(context.Background(), rep, "t1", "leads", "l1", map[string]any{"full_name": "An B"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := store.updates[0]; got["province"] != "Đà Nẵng" || got["district"] != "Hải Châu" {
			t.Fatalf("written = %v, want province Đà Nẵng + district Hải Châu", got)
		}
		if runs := drain(); len(runs) != 1 || runs[0].Status != workflow.RunSuccess {
			t.Fatalf("runs = %s, want one success run", runSummary(runs))
		}
	})
}
