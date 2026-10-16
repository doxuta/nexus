package services

// specs/048 S2 — the before_save veto on every create path, over fakes (no MySQL): Create judges
// after every field check and before the auto_number claim; CreateBulk atomic loads the rules once
// per batch and refuses the whole batch without claiming a number; CreateBulk partial goes through
// Create and is judged exactly once per row. The CSV import path is record_hooks_import_test.go;
// record_hooks_create_integration_test.go proves the same on real MySQL.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// oppsCatalog: a required name, the two fields the US3 rule reads, a formula the image must carry,
// a relation (error-order case) and an auto_number (a refused create must not burn one).
func oppsCatalog() *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-o", TenantID: "t1", Name: "opps", DataTable: "obj_opps"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
			{Name: "amount", FieldType: "number"},
			{Name: "probability", FieldType: "number"},
			{Name: "weighted", FieldType: "formula", Options: strPtr(`{"formula":"amount * probability / 100"}`)},
			{Name: "account", FieldType: "relation", Options: strPtr(`{"relatedEntity":"accounts"}`)},
			{Name: "code", FieldType: "auto_number", Options: strPtr(`{"prefix":"OPP-","padding":3}`)},
		},
	}
}

// wonNeedsDateOn is the US3 rule: stage eq Closed Won AND close_date empty ⇒ veto on close_date.
// eq (not changedTo): the rule must be able to fire on a create, where there is no "before".
func wonNeedsDateOn(entity, saveOn string) workflow.Workflow {
	return workflow.Workflow{ID: "rule-won-" + entity, TenantID: "t1", EntityName: entity, Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{SaveOn: saveOn, Rules: []workflow.Rule{
			{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"},
			{Field: "close_date", Op: workflow.OpIsNull},
		}},
		Actions: []workflow.Action{{Type: workflow.ActionVeto, Field: "close_date", Message: hookVetoMsg}}}
}

func newCreateHookService(rules ...workflow.Workflow) (*RecordService, *vetoStore, *fakeSequenceClaimer, *vetoLoader) {
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "Deal", "stage": "Proposal"}}
	claimer := &fakeSequenceClaimer{}
	loader := &vetoLoader{rules: rules}
	return NewRecordService(oppsCatalog(), store).WithSequenceClaimer(claimer).WithBeforeHooks(loader, nil), store, claimer, loader
}

func createOpp(svc *RecordService, scope RecordScope, payload map[string]any) error {
	_, err := svc.Create(context.Background(), scope, "t1", scope.UserID, "opps", payload)
	return err
}

// T050: a create the rule refuses writes nothing and claims no auto_number; the next valid create
// takes the very next number (FR-020).
func TestRecordCreate_BeforeSaveVeto(t *testing.T) {
	svc, store, claimer, _ := newCreateHookService(wonNeedsDateOn("opps", ""))
	ve := wantVeto(t, createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Closed Won"}))
	if ve.Msg != hookVetoMsg || ve.Field != "close_date" || ve.Details["field"] != "close_date" {
		t.Fatalf("veto = msg %q field %q details %v", ve.Msg, ve.Field, ve.Details)
	}
	if vs := violationsOf(t, ve); len(vs) != 1 || vs[0] != (workflow.Violation{Field: "close_date", Message: hookVetoMsg}) {
		t.Fatalf("violations = %+v (a non-admin never sees the rule id)", vs)
	}
	if store.count() != 0 || claimer.calls != 0 {
		t.Fatalf("refused create: %d inserts, %d sequence claims — want 0 and 0", store.count(), claimer.calls)
	}
	if err := createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Closed Won", "close_date": "2026-10-01"}); err != nil {
		t.Fatalf("compliant create: %v", err)
	}
	if got, _ := store.at(0); got.data["code"] != "OPP-001" {
		t.Fatalf("code = %v, want OPP-001 (the refused create burned no number)", got.data["code"])
	}

	t.Run("judged on the record as stored: formulas computed", func(t *testing.T) {
		low := workflow.Workflow{ID: "rule-low", TenantID: "t1", EntityName: "opps", Trigger: workflow.TriggerBeforeSave, IsActive: true,
			Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "weighted", Op: workflow.OpLt, Value: 10.0}}},
			Actions:   []workflow.Action{{Type: workflow.ActionVeto, Message: "Giá trị kỳ vọng quá thấp"}}}
		svc, store, _, _ := newCreateHookService(low)
		wantVeto(t, createOpp(svc, rep, map[string]any{"name": "x", "amount": 100.0, "probability": 5.0}))
		if err := createOpp(svc, rep, map[string]any{"name": "y", "amount": 1000.0, "probability": 5.0}); err != nil {
			t.Fatalf("weighted 50 refused: %v", err)
		}
		if store.count() != 1 {
			t.Fatalf("inserts = %d, want 1", store.count())
		}
	})
	t.Run("admin callers are judged too, and see the rule id", func(t *testing.T) {
		svc, _, _, _ := newCreateHookService(wonNeedsDateOn("opps", ""))
		admin := RecordScope{UserID: "u-admin", AccessLevel: "all", IsAdmin: true}
		ve := wantVeto(t, createOpp(svc, admin, map[string]any{"name": "Deal", "stage": "Closed Won"}))
		if vs := violationsOf(t, ve); vs[0].RuleID != "rule-won-opps" {
			t.Fatalf("admin violations = %+v, want the rule id", vs)
		}
	})
	t.Run("hidden anchor dropped", func(t *testing.T) {
		svc, _, _, _ := newCreateHookService(wonNeedsDateOn("opps", ""))
		svc.WithFieldACL(&fakeListHiddenACL{hidden: map[string]map[string]struct{}{"opps": {"close_date": {}}}})
		ve := wantVeto(t, createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Closed Won"}))
		if _, has := ve.Details["field"]; has || ve.Field != "" || violationsOf(t, ve)[0].Field != "" {
			t.Fatalf("hidden field leaked: Field %q details %v", ve.Field, ve.Details)
		}
	})
	t.Run("rules unreadable ⇒ refused unjudged, nothing claimed or written", func(t *testing.T) {
		svc, store, claimer, loader := newCreateHookService(wonNeedsDateOn("opps", ""))
		loader.err = errors.New("sys_workflows: connection refused")
		err := createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Proposal"})
		if !errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
		}
		if store.count() != 0 || claimer.calls != 0 {
			t.Fatalf("%d inserts, %d claims — want 0 and 0", store.count(), claimer.calls)
		}
	})
}

// T045: the field checks keep answering first — a payload that is invalid AND breaks a rule gets
// exactly the error it got before the feature.
func TestCreate_BeforeSaveVeto_ErrorOrderUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
	}{
		{"required field missing", map[string]any{"stage": "Closed Won"}},
		{"relation to a record that does not exist", map[string]any{"name": "Deal", "stage": "Closed Won", "account": "ghost"}},
		{"wrong type", map[string]any{"name": "Deal", "stage": "Closed Won", "amount": "nhiều"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain := NewRecordService(oppsCatalog(), &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}).WithSequenceClaimer(&fakeSequenceClaimer{})
			var want *ValidationError
			if !errors.As(createOpp(plain, rep, cloneData(tt.payload)), &want) {
				t.Fatal("baseline create did not fail with a ValidationError")
			}
			svc, store, _, _ := newCreateHookService(wonNeedsDateOn("opps", ""))
			var got *ValidationError
			if !errors.As(createOpp(svc, rep, cloneData(tt.payload)), &got) || got.Code != want.Code || got.Field != want.Field {
				t.Fatalf("err = %+v, want %s on %q as without the rule", got, want.Code, want.Field)
			}
			if store.count() != 0 {
				t.Fatal("an invalid create was written")
			}
		})
	}
}

// T049: saveOn narrows a rule to creates or to updates.
func TestBeforeSave_SaveOnCreateOnly(t *testing.T) {
	svc, store, _, _ := newCreateHookService(wonNeedsDateOn("opps", workflow.SaveOnCreate))
	wantVeto(t, createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Closed Won"}))
	if err := svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("a create-only rule judged an update: %v", err)
	}
	if _, updates := store.counts(); updates != 1 {
		t.Fatalf("UpdateRecord calls = %d, want 1", updates)
	}
}

func TestBeforeSave_SaveOnUpdateOnly(t *testing.T) {
	svc, store, _, _ := newCreateHookService(wonNeedsDateOn("opps", workflow.SaveOnUpdate))
	if err := createOpp(svc, rep, map[string]any{"name": "Deal", "stage": "Closed Won"}); err != nil {
		t.Fatalf("an update-only rule judged a create: %v", err)
	}
	if store.count() != 1 {
		t.Fatalf("inserts = %d, want 1", store.count())
	}
	wantVeto(t, svc.Update(context.Background(), rep, "t1", "opps", "o1", map[string]any{"stage": "Closed Won"}, nil))
}

// badName refuses a widget named BAD, anchored on name.
func badName(saveOn string) workflow.Workflow {
	return workflow.Workflow{ID: "rule-bad", TenantID: "tenant-1", EntityName: "widgets", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{SaveOn: saveOn, Rules: []workflow.Rule{{Field: "name", Op: workflow.OpEq, Value: "BAD"}}},
		Actions:   []workflow.Action{{Type: workflow.ActionVeto, Field: "name", Message: "Tên BAD không dùng được"}}}
}

func newBulkHookSvc(t *testing.T, rules ...workflow.Workflow) (*RecordService, *bulkTestStore, *fakeSequenceClaimer, *fakeBulkInserter, *vetoLoader) {
	store, claimer, inserter := &bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}
	loader := &vetoLoader{rules: rules}
	return newBulkSvc(store, claimer, inserter, nil, t).WithBeforeHooks(loader, nil), store, claimer, inserter, loader
}

func widgets(names ...string) []map[string]any {
	out := make([]map[string]any, len(names))
	for i, n := range names {
		out[i] = map[string]any{"name": n}
	}
	return out
}

// T046 (unit twin): one refused row in an all-or-nothing batch refuses the batch — nothing
// inserted, no number claimed, the row reported under its own index with the rule's words.
func TestCreateBulk_Atomic_BeforeSaveVeto(t *testing.T) {
	svc, _, claimer, inserter, loader := newBulkHookSvc(t, badName(""))
	res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "BAD", "C"), BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Created != 0 || res.Failed != 1 || res.Skipped != 2 {
		t.Fatalf("created/failed/skipped = %d/%d/%d, want 0/1/2", res.Created, res.Failed, res.Skipped)
	}
	row := res.Results[1]
	if row.Index != 1 || row.Status != "failed" || row.Code != "RECORD_HOOK_VETO" || row.Error != "Tên BAD không dùng được" || row.Details["field"] != "name" {
		t.Fatalf("row 1 = %+v, want RECORD_HOOK_VETO anchored on name", row)
	}
	if inserter.calls != 0 || claimer.calls != 0 {
		t.Fatalf("refused batch: %d insert calls, %d claims — want 0 and 0", inserter.calls, claimer.calls)
	}
	if loader.calls != 1 {
		t.Fatalf("rule loads = %d, want 1 per batch", loader.calls)
	}

	t.Run("every refused row reported; hidden anchors dropped with one lookup", func(t *testing.T) {
		svc, _, _, _, _ := newBulkHookSvc(t, badName(""))
		acl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{"widgets": {"name": {}}}}
		svc.WithFieldACL(acl)
		res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("BAD", "A", "BAD"), BulkModeAtomic)
		if err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		if res.Failed != 2 || res.Results[0].Code != "RECORD_HOOK_VETO" || res.Results[2].Code != "RECORD_HOOK_VETO" {
			t.Fatalf("results = %+v, want rows 0 and 2 refused", res.Results)
		}
		for _, i := range []int{0, 2} {
			if _, has := res.Results[i].Details["field"]; has {
				t.Fatalf("row %d leaked the hidden anchor: %v", i, res.Results[i].Details)
			}
			if vs, _ := res.Results[i].Details["violations"].([]workflow.Violation); len(vs) != 1 || vs[0].Field != "" {
				t.Fatalf("row %d violations = %#v, want one without an anchor", i, res.Results[i].Details["violations"])
			}
		}
		if acl.hiddenCalls != 1 {
			t.Fatalf("HiddenFields calls = %d for one batch, want 1", acl.hiddenCalls)
		}
	})
	t.Run("a compliant batch is inserted, one rule load", func(t *testing.T) {
		svc, _, claimer, inserter, loader := newBulkHookSvc(t, badName(""))
		res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "B", "C"), BulkModeAtomic)
		if err != nil || res.Created != 3 {
			t.Fatalf("CreateBulk = %+v, %v; want 3 created", res, err)
		}
		if inserter.calls != 1 || claimer.calls != 3 || loader.calls != 1 {
			t.Fatalf("insert calls %d, claims %d, rule loads %d — want 1, 3, 1", inserter.calls, claimer.calls, loader.calls)
		}
	})
	t.Run("an update-only rule does not judge the batch", func(t *testing.T) {
		svc, _, _, _, _ := newBulkHookSvc(t, badName(workflow.SaveOnUpdate))
		res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("BAD"), BulkModeAtomic)
		if err != nil || res.Created != 1 {
			t.Fatalf("CreateBulk = %+v, %v; want 1 created", res, err)
		}
	})
	t.Run("rules unreadable ⇒ the call fails unjudged, nothing claimed or inserted", func(t *testing.T) {
		svc, _, claimer, inserter, loader := newBulkHookSvc(t, badName(""))
		loader.err = errors.New("sys_workflows: connection refused")
		_, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A"), BulkModeAtomic)
		if !errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
		}
		if inserter.calls != 0 || claimer.calls != 0 {
			t.Fatalf("%d insert calls, %d claims — want 0 and 0", inserter.calls, claimer.calls)
		}
	})
}

// T047: best-effort mode inserts through Create, so a refused row fails alone — and is judged
// exactly once (Create's own hook; the shared Phase 1 is not hooked).
func TestCreateBulk_Partial_BeforeSaveVetoFailsRowOnly(t *testing.T) {
	svc, store, _, _, loader := newBulkHookSvc(t, badName(""))
	res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "BAD", "C"), BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Created != 2 || res.Failed != 1 || len(store.inserted) != 2 {
		t.Fatalf("created/failed/inserted = %d/%d/%d, want 2/1/2", res.Created, res.Failed, len(store.inserted))
	}
	if row := res.Results[1]; row.Status != "failed" || row.Code != "RECORD_HOOK_VETO" || row.Error != "Tên BAD không dùng được" {
		t.Fatalf("row 1 = %+v, want RECORD_HOOK_VETO", row)
	}
	if loader.calls != 3 {
		t.Fatalf("rule loads = %d for 3 rows, want 3 (one per Create, none in Phase 1)", loader.calls)
	}
}
