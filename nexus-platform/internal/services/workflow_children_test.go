package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/084 — field_update fanned out to every child record ("lớp Đã đóng ⇒ mọi ghi danh đổi
// trạng thái"): save-time guards (pure + apply path), the runtime child listing, the dry run.

func childrenCatalog() *relCatalog {
	return &relCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"classes":     {ID: "ent-cl", TenantID: "t1", Name: "classes"},
			"enrollments": {ID: "ent-en", TenantID: "t1", Name: "enrollments"},
			"teachers":    {ID: "ent-te", TenantID: "t1", Name: "teachers"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"ent-cl": {
				{Name: "status", FieldType: "picklist", Options: strOpt(`{"values":["open","closed"]}`)},
				{Name: "title", FieldType: "text"},
			},
			"ent-en": {
				{Name: "status", FieldType: "picklist", Options: strOpt(`{"values":["active","closed"]}`)},
				{Name: "class", FieldType: "relation", Options: strOpt(`{"relatedEntity":"classes"}`)},
				{Name: "teacher", FieldType: "relation", Options: strOpt(`{"relatedEntity":"teachers"}`)},
				{Name: "many_classes", FieldType: "relation", Options: strOpt(`{"relatedEntity":"classes","cardinality":"N-N"}`)},
				{Name: "note", FieldType: "text"},
				{Name: "total", FieldType: "formula", Options: strOpt(`{"expression":"1+1"}`)},
			},
			"ent-te": {{Name: "name", FieldType: "text"}},
		},
	}
}

func childrenInput(childEntity, childVia, field string, value any) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name: "close-class", EntityName: "classes", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "changedTo", Value: "closed"}},
		Actions: []WorkflowActionInput{{Type: "field_update", ChildEntity: childEntity, ChildVia: childVia, Field: field, Value: value}},
		ActorID: "u1",
	}
}

func classFieldTypes() map[string]string {
	return map[string]string{"status": "picklist", "title": "text"}
}

func TestValidateWorkflowConfig_FieldUpdateChildren(t *testing.T) {
	act := func(ce, cv, via string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "field_update", ChildEntity: ce, ChildVia: cv, Via: via, Field: "status", Value: "closed"}}
	}
	t.Run("a pair passes and field is NOT checked against the rule's entity", func(t *testing.T) {
		acts := []WorkflowActionInput{{Type: "field_update", ChildEntity: "enrollments", ChildVia: "class", Field: "note", Value: "x"}}
		if err := ValidateWorkflowConfig("classes", "after_update", nil, acts, classFieldTypes()); err != nil {
			t.Fatalf("err = %v; want nil", err)
		}
	})
	t.Run("half a pair", func(t *testing.T) {
		relWantCode(t, ValidateWorkflowConfig("classes", "after_update", nil, act("enrollments", "", ""), classFieldTypes()), childViaInvalidCode)
		relWantCode(t, ValidateWorkflowConfig("classes", "after_update", nil, act("", "class", ""), classFieldTypes()), childViaInvalidCode)
	})
	t.Run("not together with via", func(t *testing.T) {
		relWantCode(t, ValidateWorkflowConfig("classes", "after_update", nil, act("enrollments", "class", "title"), classFieldTypes()), childViaInvalidCode)
	})
	t.Run("another action type refuses the keys", func(t *testing.T) {
		acts := []WorkflowActionInput{{Type: "in_app_notification", UserID: "$owner", Message: "x", ChildEntity: "enrollments", ChildVia: "class"}}
		relWantCode(t, ValidateWorkflowConfig("classes", "after_update", nil, acts, classFieldTypes()), "WORKFLOW_ACTION_INVALID")
	})
	t.Run("a save-time rule may not write other records", func(t *testing.T) {
		relWantCode(t, ValidateWorkflowConfig("classes", "before_save", nil, act("enrollments", "class", ""), classFieldTypes()), "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER")
	})
}

func TestCreateWorkflow_FieldUpdateChildren_TargetGuards(t *testing.T) {
	tests := []struct {
		name, childEntity, childVia, field string
		value                              any
		code, codeField                    string
	}{
		{name: "child entity gone", childEntity: "ghosts", childVia: "class", field: "status", value: "closed", code: childViaInvalidCode, codeField: "class"},
		{name: "childVia unknown", childEntity: "enrollments", childVia: "nope", field: "status", value: "closed", code: childViaInvalidCode, codeField: "nope"},
		{name: "childVia not a relation", childEntity: "enrollments", childVia: "note", field: "status", value: "closed", code: childViaInvalidCode, codeField: "note"},
		{name: "childVia points at a different parent", childEntity: "enrollments", childVia: "teacher", field: "status", value: "closed", code: childViaInvalidCode, codeField: "teacher"},
		{name: "childVia many-valued", childEntity: "enrollments", childVia: "many_classes", field: "status", value: "closed", code: childViaInvalidCode, codeField: "many_classes"},
		{name: "unknown child field", childEntity: "enrollments", childVia: "class", field: "ghost", value: "x", code: "RECORD_UNKNOWN_FIELD", codeField: "ghost"},
		{name: "a rule-entity field is not a child field", childEntity: "enrollments", childVia: "class", field: "title", value: "x", code: "RECORD_UNKNOWN_FIELD", codeField: "title"},
		{name: "formula child field", childEntity: "enrollments", childVia: "class", field: "total", value: 3, code: "RECORD_FORMULA_READONLY", codeField: "total"},
		{name: "value outside the child picklist", childEntity: "enrollments", childVia: "class", field: "status", value: "open", code: "RECORD_INVALID_PICKLIST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(childrenCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", childrenInput(tt.childEntity, tt.childVia, tt.field, tt.value))
			ve := relWantCode(t, err, tt.code)
			if tt.codeField != "" && ve.Field != tt.codeField {
				t.Errorf("details.field = %q; want %q", ve.Field, tt.codeField)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rejected rule was persisted (%d)", len(repo.created))
			}
		})
	}
}

func TestCreateWorkflow_FieldUpdateChildren_PersistsThePair(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(childrenCatalog())
	if _, err := svc.CreateWorkflow(context.Background(), "t1", childrenInput("enrollments", "class", "status", "closed")); err != nil {
		t.Fatalf("CreateWorkflow = %v; want nil", err)
	}
	var acts []workflow.Action
	if err := json.Unmarshal(repo.created[0].Actions, &acts); err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].ChildEntity != "enrollments" || acts[0].ChildVia != "class" || acts[0].Field != "status" || acts[0].Via != "" {
		t.Fatalf("stored action = %+v; want childEntity=enrollments childVia=class field=status", acts)
	}
}

// fakeChildStore records what ListChildIDs asked the store.
type fakeChildStore struct {
	fakeRelMutStore
	got struct {
		entity, tenant, field, parent, after string
		limit                                int
	}
}

func (f *fakeChildStore) ListIDsByRelation(_ context.Context, entity, tenantID, field, parentID, afterID string, limit int) ([]string, error) {
	f.got.entity, f.got.tenant, f.got.field, f.got.parent, f.got.after, f.got.limit = entity, tenantID, field, parentID, afterID, limit
	return []string{"e1"}, nil
}

func TestListChildIDs_ResolvesTheRelationAndPinsTheTenant(t *testing.T) {
	store := &fakeChildStore{}
	mut := NewWorkflowRecordMutator(store).WithCatalog(childrenCatalog())
	ids, err := mut.ListChildIDs(context.Background(), "t1", "classes", "enrollments", "class", "cl-1", "e0", 50)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ListChildIDs = %v, %v", ids, err)
	}
	g := store.got
	if g.entity != "enrollments" || g.tenant != "t1" || g.field != "class" || g.parent != "cl-1" || g.after != "e0" || g.limit != 50 {
		t.Errorf("store asked %+v; want enrollments/t1/class/cl-1 after e0 limit 50", g)
	}
	for _, via := range []string{"teacher", "note", "ghost", "many_classes"} {
		store.got.entity = ""
		if _, err := mut.ListChildIDs(context.Background(), "t1", "classes", "enrollments", via, "cl-1", "", 50); err == nil {
			t.Errorf("via %q: err = nil; want a rejection", via)
		}
		if store.got.entity != "" {
			t.Errorf("via %q: the store was queried despite the rejection", via)
		}
	}
	if _, err := NewWorkflowRecordMutator(store).ListChildIDs(context.Background(), "t1", "classes", "enrollments", "class", "cl-1", "", 50); err == nil {
		t.Error("ListChildIDs without a catalog = nil error; it cannot know the relation and must refuse")
	}
}

func TestPreviewAction_FieldUpdateChildren(t *testing.T) {
	a := workflow.Action{Type: "field_update", ChildEntity: "enrollments", ChildVia: "class", Field: "status", Value: "closed"}
	evt := workflow.Event{TenantID: "t1", EntityName: "classes", Data: map[string]any{"status": "closed"}}
	detail, would := previewAction(context.Background(), "wf1", a, evt, true, classFieldTypes(), nil, nil)
	if !would || detail["error"] != nil {
		t.Fatalf("detail = %v would=%v; want it to apply (no WF-01 miss: status is not checked against classes)", detail, would)
	}
	if detail["childEntity"] != "enrollments" || detail["childVia"] != "class" || detail["after"] != "closed" || detail["budget"] != workflow.MaxChildUpdatesPerRun {
		t.Errorf("detail = %v; want childEntity/childVia/after/budget", detail)
	}
	if _, would := previewAction(context.Background(), "wf1", a, evt, false, classFieldTypes(), nil, nil); would {
		t.Error("condition not matched ⇒ must not apply")
	}
}
