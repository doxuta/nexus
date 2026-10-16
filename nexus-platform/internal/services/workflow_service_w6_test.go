package services

// W6 unit tests — WF-01 (field_update must not target computed/assigned fields) + the
// POST /admin/workflows/:id/test dry-run trace. All fakes, no DB.
//
// WF-01 (bug): the field_update action validator did not check the target field against
// sys_fields, so an admin could point a rule at a formula/lookup/rollup/auto_number field and
// the engine would write straight into obj_*.data — bypassing computedReadonlyCode, which every
// OTHER write path (POST/PUT/CSV import) enforces. auto_number has NO read gate and is never
// re-assigned on Update, so a workflow overwrite corrupts it PERMANENTLY. The fix rejects such
// targets at config time (CreateWorkflow/UpdateWorkflow) with the SAME error-code family the
// record paths use, and unknown fields with RECORD_UNKNOWN_FIELD.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// ---- fakes ------------------------------------------------------------------------------------

// fakeWFRepo is an in-memory workflowRepo. Create/Update store the validated JSON so tests can
// assert what would be persisted; Get returns the stored row.
type fakeWFRepo struct {
	rows    map[string]*workflow.WorkflowRecord
	nextID  int
	created []workflow.CreateInput
}

func newFakeWFRepo() *fakeWFRepo {
	return &fakeWFRepo{rows: map[string]*workflow.WorkflowRecord{}}
}

func (f *fakeWFRepo) Create(_ context.Context, in workflow.CreateInput) (string, error) {
	f.nextID++
	id := fmt.Sprintf("wf-%d", f.nextID)
	f.created = append(f.created, in)
	f.rows[id] = &workflow.WorkflowRecord{
		ID: id, TenantID: in.TenantID, Name: in.Name, EntityName: in.EntityName,
		Trigger: string(in.Trigger), Condition: in.Condition, Actions: in.Actions,
		IsActive: in.IsActive,
	}
	return id, nil
}

func (f *fakeWFRepo) Get(_ context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, workflow.ErrWorkflowNotFound
	}
	return r, nil
}

func (f *fakeWFRepo) List(_ context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	var out []workflow.WorkflowRecord
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeWFRepo) Update(_ context.Context, tenantID, id string, in workflow.UpdateInput) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	r.Name, r.EntityName, r.Trigger = in.Name, in.EntityName, string(in.Trigger)
	r.Condition, r.Actions, r.IsActive = in.Condition, in.Actions, in.IsActive
	return nil
}

func (f *fakeWFRepo) SetActive(_ context.Context, tenantID, id string, active bool, _ string) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	r.IsActive = active
	return nil
}

func (f *fakeWFRepo) Delete(_ context.Context, tenantID, id string) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	delete(f.rows, id)
	return nil
}

// CountActive counts the tenant's active rows of (entity, trigger) other than excludeID (specs/048).
func (f *fakeWFRepo) CountActive(_ context.Context, tenantID, entityName string, trigger workflow.Trigger, excludeID string) (int, error) {
	n := 0
	for id, r := range f.rows {
		if id != excludeID && r.TenantID == tenantID && r.EntityName == entityName && r.Trigger == string(trigger) && r.IsActive {
			n++
		}
	}
	return n, nil
}

// fakeWFCatalog implements BOTH GetEntityByName and ListFields (like the production MetaCache),
// so the WF-01 field-target guard is armed.
type fakeWFCatalog struct {
	entity *metadata.EntityDefinition
	fields []metadata.FieldDefinition
}

func (c *fakeWFCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if c.entity == nil || c.entity.Name != name {
		return nil, metadata.ErrEntityNotFound
	}
	return c.entity, nil
}

func (c *fakeWFCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	if c.entity == nil || c.entity.ID != entityID {
		return nil, fmt.Errorf("unknown entity id %q", entityID)
	}
	return c.fields, nil
}

// fakeWFEntityOnly implements ONLY GetEntityByName (no ListFields): the field-target guard must
// then be skipped (legacy behaviour preserved for checkers that cannot list fields).
type fakeWFEntityOnly struct {
	entity *metadata.EntityDefinition
}

func (c *fakeWFEntityOnly) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if c.entity == nil || c.entity.Name != name {
		return nil, metadata.ErrEntityNotFound
	}
	return c.entity, nil
}

// w6Catalog builds a catalog whose "deals" entity has one field of every computed/assigned type
// plus a plain text field.
func w6Catalog() *fakeWFCatalog {
	relOpts := `{"relatedEntity":"contacts"}`
	return &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "f1", EntityID: "ent-1", Name: "stage", FieldType: "text"},
			{ID: "f2", EntityID: "ent-1", Name: "total", FieldType: "formula"},
			{ID: "f3", EntityID: "ent-1", Name: "owner_email", FieldType: "lookup"},
			{ID: "f4", EntityID: "ent-1", Name: "line_sum", FieldType: "rollup"},
			{ID: "f5", EntityID: "ent-1", Name: "deal_no", FieldType: "auto_number"},
			{ID: "f6", EntityID: "ent-1", Name: "amount", FieldType: "number"},
			{ID: "f7", EntityID: "ent-1", Name: "primary_contact", FieldType: "relation", Options: &relOpts},
		},
	}
}

func w6CreateInput(field string) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name:       "rule",
		EntityName: "deals",
		Trigger:    "after_update",
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: field, Value: "x"}},
		ActorID:    "u1",
	}
}

// ---- WF-01: config-time rejection -------------------------------------------------------------

func TestWF01_CreateWorkflow_FieldUpdateTargetGuard(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		wantErr  bool
		wantCode string
	}{
		{name: "auto_number target rejected (permanent corruption)", field: "deal_no", wantErr: true, wantCode: "RECORD_AUTONUMBER_READONLY"},
		{name: "formula target rejected", field: "total", wantErr: true, wantCode: "RECORD_FORMULA_READONLY"},
		{name: "lookup target rejected", field: "owner_email", wantErr: true, wantCode: "RECORD_LOOKUP_READONLY"},
		{name: "rollup target rejected", field: "line_sum", wantErr: true, wantCode: "RECORD_ROLLUP_READONLY"},
		{name: "unknown field rejected", field: "ghost", wantErr: true, wantCode: "RECORD_UNKNOWN_FIELD"},
		{name: "plain text target allowed", field: "stage", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", w6CreateInput(tt.field))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("CreateWorkflow(%q) err = %v; want nil", tt.field, err)
				}
				if len(repo.created) != 1 {
					t.Fatalf("expected 1 persisted rule, got %d", len(repo.created))
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateWorkflow(%q) = nil error; want code %s (WF-01 guard missing)", tt.field, tt.wantCode)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error type = %T; want *ValidationError", err)
			}
			if ve.Code != tt.wantCode {
				t.Errorf("code = %s; want %s", ve.Code, tt.wantCode)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rejected rule must NOT be persisted (got %d creates)", len(repo.created))
			}
		})
	}
}

func TestWF01_UpdateWorkflow_FieldUpdateTargetGuard(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog())
	rec, err := svc.CreateWorkflow(context.Background(), "t1", w6CreateInput("stage"))
	if err != nil {
		t.Fatalf("seed CreateWorkflow: %v", err)
	}

	// Re-pointing the rule at the auto_number field via UPDATE must be rejected too (same guard
	// on both write paths — otherwise Update is a bypass).
	_, err = svc.UpdateWorkflow(context.Background(), "t1", rec.ID, UpdateWorkflowInput{
		Name:       "rule",
		EntityName: "deals",
		Trigger:    "after_update",
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "deal_no", Value: "HACK-1"}},
		ActorID:    "u1",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("UpdateWorkflow → err %v; want RECORD_AUTONUMBER_READONLY", err)
	}
}

func TestWF01_CheckerWithoutListFields_SkipsGuard(t *testing.T) {
	// A checker that can't list fields (only GetEntityByName) keeps the legacy behaviour: the
	// rule is accepted (unit-test fakes / partial wirings). Production wires MetaCache which
	// implements ListFields, so the guard is always armed there.
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(&fakeWFEntityOnly{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "deals"},
	})
	if _, err := svc.CreateWorkflow(context.Background(), "t1", w6CreateInput("anything")); err != nil {
		t.Fatalf("CreateWorkflow without ListFields capability err = %v; want nil (guard skipped)", err)
	}
}

// ---- WF-01: execution-time defense in depth ---------------------------------------------------

// fakeGuardStore records UpdateRecord calls (the raw write the mutator would perform).
type fakeGuardStore struct {
	calls int
	last  map[string]any
}

func (f *fakeGuardStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.calls++
	f.last = data
	return nil
}

func TestWF01_MutatorGuard_BlocksComputedTargetAtExecution(t *testing.T) {
	// Defense in depth: a BAD rule that predates the config-time guard (already stored in
	// sys_workflows) must be blocked when the engine executes it — the mutator refuses to write
	// a computed/assigned or unknown field.
	store := &fakeGuardStore{}
	mut := NewWorkflowRecordMutator(store).WithCatalog(w6Catalog())

	tests := []struct {
		name    string
		field   string
		wantErr bool
	}{
		{name: "auto_number blocked", field: "deal_no", wantErr: true},
		{name: "formula blocked", field: "total", wantErr: true},
		{name: "lookup blocked", field: "owner_email", wantErr: true},
		{name: "rollup blocked", field: "line_sum", wantErr: true},
		{name: "unknown field blocked", field: "ghost", wantErr: true},
		{name: "plain field writes", field: "stage", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := store.calls
			err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", tt.field, "v")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("UpdateField(%q) = nil; want error (execution guard missing)", tt.field)
				}
				if store.calls != before {
					t.Fatalf("store was written despite guard (calls %d→%d)", before, store.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateField(%q) err = %v; want nil", tt.field, err)
			}
			if store.calls != before+1 {
				t.Fatalf("plain field write did not reach store")
			}
		})
	}
}

func TestWF01_MutatorWithoutCatalog_LegacyPassThrough(t *testing.T) {
	// No catalog wired → the mutator behaves exactly as before (unit fakes / legacy wiring).
	store := &fakeGuardStore{}
	mut := NewWorkflowRecordMutator(store)
	if err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", "anything", "v"); err != nil {
		t.Fatalf("UpdateField without catalog err = %v; want nil", err)
	}
	if store.calls != 1 {
		t.Fatalf("write did not reach store")
	}
}

// ---- WF-VALUE-TYPECHECK: execution-time value validation --------------------------------------

// TestWFVALUE_MutatorTypeChecksValue proves the mutator now type-checks the field_update VALUE
// against the target field's spec (not just the target field's readonly-ness). A rule setting a
// string into a number field must FAIL with the right validation code and leave the record
// untouched; a correctly-typed value still writes. The value flows in the SAME representation the
// real dispatcher hands the mutator (Action.Value decodes from JSON: number → float64, string →
// string), so these cases mirror production exactly.
func TestWFVALUE_MutatorTypeChecksValue(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		value     any
		wantErr   bool
		wantCode  string // asserted via errors.As when wantErr
		wantWrite bool
	}{
		{
			name:     "string into number field is rejected as junk",
			field:    "amount",
			value:    "abc", // Action.Value for a string decodes as string
			wantErr:  true,
			wantCode: "RECORD_INVALID_TYPE",
		},
		{
			name:      "correctly-typed number still writes",
			field:     "amount",
			value:     float64(42), // Action.Value for a number decodes as float64
			wantErr:   false,
			wantWrite: true,
		},
		{
			name:      "valid text into text field still writes",
			field:     "stage",
			value:     "closed",
			wantErr:   false,
			wantWrite: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeGuardStore{}
			mut := NewWorkflowRecordMutator(store).WithCatalog(w6Catalog())

			err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", tt.field, tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("UpdateField(%q=%v) = nil; want error (value not type-checked)", tt.field, tt.value)
				}
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("error %v is not a *ValidationError; want code %s", err, tt.wantCode)
				}
				if ve.Code != tt.wantCode {
					t.Fatalf("code = %q; want %q (err=%v)", ve.Code, tt.wantCode, err)
				}
				// The bad value must NEVER reach the store (record unchanged).
				if store.calls != 0 {
					t.Fatalf("store was written despite an invalid value (calls=%d)", store.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateField(%q=%v) err = %v; want nil", tt.field, tt.value, err)
			}
			if tt.wantWrite && store.calls != 1 {
				t.Fatalf("valid value did not reach store (calls=%d)", store.calls)
			}
			// The stored value round-trips (normalized) under the field key.
			if tt.wantWrite {
				if _, ok := store.last[tt.field]; !ok {
					t.Fatalf("stored data %#v missing field %q", store.last, tt.field)
				}
			}
		})
	}
}

// fakeRelMutStore is a mutator store that ALSO answers relation-existence AND (USER-FIELD)
// user-existence probes (satisfies the relationExister / userExister the mutator type-asserts)
// — so it can exercise the WF relation-FK and user-FK parity guards.
type fakeRelMutStore struct {
	calls       int
	last        map[string]any
	exists      map[string]bool // id → live (relationExister)
	activeUsers map[string]bool // id → active user (userExister) — USER-FIELD
}

func (f *fakeRelMutStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.calls++
	f.last = data
	return nil
}

func (f *fakeRelMutStore) BatchRecordExists(_ context.Context, _entity, _tenant string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = f.exists[id]
	}
	return out, nil
}

// BatchActiveUserExists (USER-FIELD): mirrors BatchRecordExists above, over activeUsers instead.
func (f *fakeRelMutStore) BatchActiveUserExists(_ context.Context, _tenant string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = f.activeUsers[id]
	}
	return out, nil
}

// TestWFVALUE_MutatorValidatesRelationTarget proves the workflow field_update mutator enforces the
// SAME relation-FK existence guard the HTTP PUT path runs (review 2026-07-03, LOW): a rule setting a
// relation field to a DANGLING/cross-tenant id fails RECORD_RELATION_INVALID and never writes; a
// live in-tenant target writes. Without this, an automation could merge a dangling FK that a manual
// PUT would reject — corrupting downstream lookup/rollup that dereference it.
func TestWFVALUE_MutatorValidatesRelationTarget(t *testing.T) {
	t.Run("dangling relation FK is rejected and never written", func(t *testing.T) {
		store := &fakeRelMutStore{exists: map[string]bool{"contact-live": true}}
		mut := NewWorkflowRecordMutator(store).WithCatalog(w6Catalog())

		err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", "primary_contact", "contact-ghost")
		if err == nil {
			t.Fatalf("UpdateField(primary_contact=contact-ghost) = nil; want RECORD_RELATION_INVALID (dangling FK not probed)")
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("error %v is not a *ValidationError", err)
		}
		if ve.Code != "RECORD_RELATION_INVALID" {
			t.Fatalf("code = %q; want RECORD_RELATION_INVALID (err=%v)", ve.Code, err)
		}
		if store.calls != 0 {
			t.Fatalf("store was written despite a dangling relation FK (calls=%d)", store.calls)
		}
	})

	t.Run("live in-tenant relation target writes", func(t *testing.T) {
		store := &fakeRelMutStore{exists: map[string]bool{"contact-live": true}}
		mut := NewWorkflowRecordMutator(store).WithCatalog(w6Catalog())

		if err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", "primary_contact", "contact-live"); err != nil {
			t.Fatalf("UpdateField(primary_contact=contact-live) err = %v; want nil", err)
		}
		if store.calls != 1 {
			t.Fatalf("valid relation target did not reach store (calls=%d)", store.calls)
		}
	})
}

// w6CatalogWithUserField is a LOCAL catalog fixture (not the shared w6Catalog(), to avoid any
// risk of side-effects on its many other consumers in this file) for a "deals" entity carrying
// one `user` field, "owner".
func w6CatalogWithUserField() *fakeWFCatalog {
	return &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-u1", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "fu1", EntityID: "ent-u1", Name: "stage", FieldType: "text"},
			{ID: "fu2", EntityID: "ent-u1", Name: "owner", FieldType: "user"},
		},
	}
}

// TestWFVALUE_MutatorValidatesUserTarget is the USER-FIELD analogue of
// TestWFVALUE_MutatorValidatesRelationTarget: the workflow field_update mutator enforces the SAME
// ACTIVE-user existence guard the HTTP PUT path runs (checkUserTargets) — an automation cannot
// merge a dangling/cross-tenant/disabled user id into a `user` field that a manual PUT would
// reject with RECORD_USER_INVALID.
func TestWFVALUE_MutatorValidatesUserTarget(t *testing.T) {
	t.Run("dangling user FK is rejected and never written", func(t *testing.T) {
		store := &fakeRelMutStore{activeUsers: map[string]bool{"user-live": true}}
		mut := NewWorkflowRecordMutator(store).WithCatalog(w6CatalogWithUserField())

		err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", "owner", "user-ghost")
		if err == nil {
			t.Fatalf("UpdateField(owner=user-ghost) = nil; want RECORD_USER_INVALID (dangling id not probed)")
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("error %v is not a *ValidationError", err)
		}
		if ve.Code != "RECORD_USER_INVALID" {
			t.Fatalf("code = %q; want RECORD_USER_INVALID (err=%v)", ve.Code, err)
		}
		if store.calls != 0 {
			t.Fatalf("store was written despite a dangling user FK (calls=%d)", store.calls)
		}
	})

	t.Run("live active user target writes", func(t *testing.T) {
		store := &fakeRelMutStore{activeUsers: map[string]bool{"user-live": true}}
		mut := NewWorkflowRecordMutator(store).WithCatalog(w6CatalogWithUserField())

		if err := mut.UpdateField(context.Background(), "deals", "t1", "rec-1", "sys", "owner", "user-live"); err != nil {
			t.Fatalf("UpdateField(owner=user-live) err = %v; want nil", err)
		}
		if store.calls != 1 {
			t.Fatalf("valid user target did not reach store (calls=%d)", store.calls)
		}
	})
}

// ---- dry-run (POST /admin/workflows/:id/test) --------------------------------------------------

// fakeRecordReader serves one record and counts reads; it must be the ONLY store the dry-run
// touches (no mutator, no senders — the WorkflowService does not even hold those).
type fakeRecordReader struct {
	rec   *dynrepo.Record
	reads int
}

func (f *fakeRecordReader) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	f.reads++
	if f.rec == nil || f.rec.ID != id || f.rec.TenantID != tenantID {
		return nil, dynrepo.ErrRecordNotFound
	}
	cp := *f.rec
	return &cp, nil
}

func w6DryRunSvc(t *testing.T, rules []WorkflowRuleInput, actions []WorkflowActionInput, rec *dynrepo.Record) (*WorkflowService, string, *fakeRecordReader) {
	t.Helper()
	repo := newFakeWFRepo()
	reader := &fakeRecordReader{rec: rec}
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(reader)
	created, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "rule", EntityName: "deals", Trigger: "after_update",
		Rules: rules, Actions: actions, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("seed CreateWorkflow: %v", err)
	}
	return svc, created.ID, reader
}

func TestDryRun_TraceMatchAndPreviews(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "won", "stage": "new", "owner": "an@example.com"}}
	svc, wfID, reader := w6DryRunSvc(t,
		[]WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		[]WorkflowActionInput{
			{Type: "field_update", Field: "stage", Value: "closed"},
			{Type: "email_alert", To: "{{owner}}", Subject: "Deal {{status}}", Body: "Stage: {{stage}}"},
		},
		rec)

	got, err := svc.TestWorkflow(context.Background(), "t1", wfID, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow err = %v", err)
	}
	if !got.Matched {
		t.Fatalf("Matched = false; want true (status==won)")
	}
	if got.EntityName != "deals" || got.RecordID != "r1" || got.Trigger != "after_update" {
		t.Errorf("header = %+v; want deals/r1/after_update", got)
	}
	// trace: 1 trigger + 1 condition + 2 actions = 4 steps, numbered 1..4.
	if len(got.Trace) != 4 {
		t.Fatalf("trace length = %d; want 4 (%+v)", len(got.Trace), got.Trace)
	}
	for i, st := range got.Trace {
		if st.Step != i+1 {
			t.Errorf("trace[%d].Step = %d; want %d", i, st.Step, i+1)
		}
	}
	if got.Trace[0].Kind != "trigger" || got.Trace[0].Matched == nil || !*got.Trace[0].Matched {
		t.Errorf("trigger step = %+v; want matched", got.Trace[0])
	}
	cond := got.Trace[1]
	if cond.Kind != "condition" || cond.Matched == nil || !*cond.Matched ||
		cond.Detail["field"] != "status" || cond.Detail["actual"] != "won" {
		t.Errorf("condition step = %+v; want matched with actual=won", cond)
	}
	fu := got.Trace[2]
	if fu.Kind != "action" || fu.Evaluated == nil || !*fu.Evaluated ||
		fu.Detail["field"] != "stage" || fu.Detail["before"] != "new" || fu.Detail["after"] != "closed" {
		t.Errorf("field_update preview = %+v; want before=new after=closed evaluated", fu)
	}
	mail := got.Trace[3]
	if mail.Detail["to"] != "an@example.com" || mail.Detail["subject"] != "Deal won" || mail.Detail["body"] != "Stage: new" {
		t.Errorf("email preview = %+v; want rendered {{field}} interpolation", mail.Detail)
	}
	if ws, _ := mail.Detail["wouldSend"].(bool); !ws {
		t.Errorf("email wouldSend = %v; want true", mail.Detail["wouldSend"])
	}
	// ZERO side effects: the only store touched is the record READ.
	if reader.reads != 1 {
		t.Errorf("record reads = %d; want exactly 1", reader.reads)
	}
}

func TestDryRun_ConditionFalse_PerRuleTrace(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "lost", "stage": "new"}}
	svc, wfID, _ := w6DryRunSvc(t,
		[]WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		[]WorkflowActionInput{{Type: "in_app_notification", UserID: "u9", Message: "hi"}},
		rec)

	got, err := svc.TestWorkflow(context.Background(), "t1", wfID, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow err = %v", err)
	}
	if got.Matched {
		t.Fatalf("Matched = true; want false (status=lost)")
	}
	cond := got.Trace[1]
	if cond.Matched == nil || *cond.Matched {
		t.Errorf("condition step must be matched=false: %+v", cond)
	}
	act := got.Trace[2]
	if act.Evaluated == nil || *act.Evaluated {
		t.Errorf("action must be evaluated=false when the condition fails: %+v", act)
	}
	if ws, _ := act.Detail["wouldSend"].(bool); ws {
		t.Errorf("notification wouldSend must be false on non-match")
	}
}

// TestDryRun_OrJoinVerdict proves the dry-run verdict tracks the condition's join mode: a rule set
// where only ONE rule matches → FALSE under AND but TRUE under OR (the discriminator). The per-rule
// trace still shows each predicate; detail.match records the mode. Guards the preview↔dispatch
// parity for OR (WF-OR).
func TestDryRun_OrJoinVerdict(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "won", "stage": "new"}}
	rules := []WorkflowRuleInput{
		{Field: "status", Op: "eq", Value: "won"},   // true
		{Field: "stage", Op: "eq", Value: "closed"}, // false
	}
	actions := []WorkflowActionInput{{Type: "in_app_notification", UserID: "u9", Message: "hi"}}

	// AND → false (one rule fails).
	repo := newFakeWFRepo()
	svcAnd := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(&fakeRecordReader{rec: rec})
	andWF, err := svcAnd.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "and", EntityName: "deals", Trigger: "after_update", Match: "and", Rules: rules, Actions: actions, ActorID: "u1"})
	if err != nil {
		t.Fatalf("create AND: %v", err)
	}
	if gotAnd, err := svcAnd.TestWorkflow(context.Background(), "t1", andWF.ID, "r1"); err != nil || gotAnd.Matched {
		t.Fatalf("AND verdict = %v (err %v); want false", gotAnd.Matched, err)
	}

	// OR → true (one rule matches). Same rules, same record.
	svcOr := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(w6Catalog()).WithRecordReader(&fakeRecordReader{rec: rec})
	orWF, err := svcOr.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "or", EntityName: "deals", Trigger: "after_update", Match: "or", Rules: rules, Actions: actions, ActorID: "u1"})
	if err != nil {
		t.Fatalf("create OR: %v", err)
	}
	gotOr, err := svcOr.TestWorkflow(context.Background(), "t1", orWF.ID, "r1")
	if err != nil {
		t.Fatalf("OR TestWorkflow: %v", err)
	}
	if !gotOr.Matched {
		t.Fatalf("OR verdict = false; want true (one rule matches)")
	}
	// Both rules still traced individually, tagged with the join mode.
	if gotOr.Trace[1].Detail["match"] != "or" {
		t.Errorf("condition trace detail.match = %v; want or", gotOr.Trace[1].Detail["match"])
	}
	// The gated action WOULD run under OR (condition matched).
	if act := gotOr.Trace[len(gotOr.Trace)-1]; act.Evaluated == nil || !*act.Evaluated {
		t.Errorf("action should be evaluated=true under a matched OR condition: %+v", act)
	}
}

func TestDryRun_Errors(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "won", "stage": "new"}}
	svc, wfID, _ := w6DryRunSvc(t, nil,
		[]WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "x"}}, rec)

	cases := []struct {
		name     string
		wf, rid  string
		wantCode string
	}{
		{name: "unknown workflow → 404", wf: "ghost", rid: "r1", wantCode: "WORKFLOW_NOT_FOUND"},
		{name: "blank recordId → 422", wf: wfID, rid: "  ", wantCode: "WORKFLOW_TEST_RECORD_REQUIRED"},
		{name: "unknown record → 404", wf: wfID, rid: "no-such", wantCode: "RECORD_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.TestWorkflow(context.Background(), "t1", tc.wf, tc.rid)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.wantCode {
				t.Fatalf("err = %v; want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestDryRun_PreexistingBadRule_FieldUpdateErrorInTrace(t *testing.T) {
	// A rule stored BEFORE the WF-01 config-time guard (seeded straight through the repo,
	// bypassing CreateWorkflow) must surface the bad target as an error in the trace — not as a
	// bogus "would apply".
	repo := newFakeWFRepo()
	reader := &fakeRecordReader{rec: &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "won"}}}
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(reader)
	id, err := repo.Create(context.Background(), workflow.CreateInput{
		TenantID: "t1", Name: "bad", EntityName: "deals", Trigger: workflow.TriggerAfterUpdate,
		Actions:  []byte(`[{"type":"field_update","field":"deal_no","value":"HACK-9"}]`),
		IsActive: true,
	})
	if err != nil {
		t.Fatalf("seed bad rule: %v", err)
	}

	got, err := svc.TestWorkflow(context.Background(), "t1", id, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow err = %v", err)
	}
	act := got.Trace[len(got.Trace)-1]
	if act.Evaluated == nil || *act.Evaluated {
		t.Fatalf("bad-target action must be evaluated=false: %+v", act)
	}
	errText, _ := act.Detail["error"].(string)
	if !strings.Contains(errText, "RECORD_AUTONUMBER_READONLY") {
		t.Errorf("trace error = %q; want the computed-readonly code surfaced", errText)
	}
	if wa, _ := act.Detail["wouldApply"].(bool); wa {
		t.Errorf("wouldApply must be false for a computed target")
	}
}

// ---- runs list clamps --------------------------------------------------------------------------

type fakeRunReader struct {
	lastLimit, lastOffset int
	recentCalls           int
	lastIDs               []string
}

func (f *fakeRunReader) ListRuns(_ context.Context, _, _ string, limit, offset int) ([]workflow.RunRecord, int, error) {
	f.lastLimit, f.lastOffset = limit, offset
	return nil, 0, nil
}

func (f *fakeRunReader) ListRecentRuns(_ context.Context, _ string, limit int, workflowIDs []string) ([]workflow.RunRecord, error) {
	f.lastLimit, f.lastIDs = limit, workflowIDs
	f.recentCalls++
	return nil, nil
}

// Queue #297: the recent-runs read narrowed to the rules one app's automation panel lists.
// nil = every rule (the old read) · non-nil empty = no rule, so no run and no query · over the cap
// = refused before any read, never truncated into a wrong answer.
func TestListRecentWorkflowRuns_ScopedByWorkflowIDs(t *testing.T) {
	tooMany := make([]string, runsFilterMaxIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("wf-%d", i)
	}
	cases := []struct {
		name      string
		ids       []string
		wantCalls int
		wantIDs   []string
		wantCode  string
	}{
		{name: "nil = every rule", ids: nil, wantCalls: 1, wantIDs: nil},
		{name: "the listed rules only", ids: []string{"wf-a", "wf-b"}, wantCalls: 1, wantIDs: []string{"wf-a", "wf-b"}},
		{name: "exactly the cap passes", ids: tooMany[:runsFilterMaxIDs], wantCalls: 1, wantIDs: tooMany[:runsFilterMaxIDs]},
		{name: "empty = no rule, no query", ids: []string{}, wantCalls: 0},
		{name: "over the cap is refused", ids: tooMany, wantCalls: 0, wantCode: "WORKFLOW_RUNS_FILTER_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeRunReader{}
			svc := NewWorkflowService(newFakeWFRepo()).WithRunReader(reader)
			rows, err := svc.ListRecentWorkflowRuns(context.Background(), "t1", 8, tc.ids)
			if tc.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v; want %s", err, tc.wantCode)
				}
				if ve.Details["max"] != runsFilterMaxIDs {
					t.Errorf("details.max = %v; want %d", ve.Details["max"], runsFilterMaxIDs)
				}
			} else {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if rows == nil {
					t.Errorf("rows must be non-nil (JSON [])")
				}
			}
			if reader.recentCalls != tc.wantCalls {
				t.Fatalf("repo calls = %d; want %d", reader.recentCalls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && !reflect.DeepEqual(reader.lastIDs, tc.wantIDs) {
				t.Errorf("repo ids = %v; want %v", reader.lastIDs, tc.wantIDs)
			}
		})
	}
}

func TestListWorkflowRuns_ClampsAndNotFound(t *testing.T) {
	repo := newFakeWFRepo()
	reader := &fakeRunReader{}
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRunReader(reader)
	created, err := svc.CreateWorkflow(context.Background(), "t1", w6CreateInput("stage"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// limit hard-capped at 100; negative offset → 0; zero limit → default 20.
	rows, _, err := svc.ListWorkflowRuns(context.Background(), "t1", created.ID, 5000, -3)
	if err != nil {
		t.Fatalf("ListWorkflowRuns err = %v", err)
	}
	if rows == nil {
		t.Errorf("rows must be non-nil (JSON [])")
	}
	if reader.lastLimit != 100 || reader.lastOffset != 0 {
		t.Errorf("clamp = limit %d offset %d; want 100/0", reader.lastLimit, reader.lastOffset)
	}
	if _, _, err := svc.ListWorkflowRuns(context.Background(), "t1", created.ID, 0, 0); err != nil {
		t.Fatalf("default limit err = %v", err)
	}
	if reader.lastLimit != 20 {
		t.Errorf("default limit = %d; want 20", reader.lastLimit)
	}

	// Unknown workflow → WORKFLOW_NOT_FOUND (the runs read never probes another tenant's rule).
	var ve *ValidationError
	_, _, err = svc.ListWorkflowRuns(context.Background(), "t1", "ghost", 10, 0)
	if !errors.As(err, &ve) || ve.Code != "WORKFLOW_NOT_FOUND" {
		t.Fatalf("unknown workflow err = %v; want WORKFLOW_NOT_FOUND", err)
	}
	// Cross-tenant → same not-found.
	_, _, err = svc.ListWorkflowRuns(context.Background(), "t2", created.ID, 10, 0)
	if !errors.As(err, &ve) || ve.Code != "WORKFLOW_NOT_FOUND" {
		t.Fatalf("cross-tenant err = %v; want WORKFLOW_NOT_FOUND", err)
	}
}

// SetOwner satisfies fieldUpdater (assign hands the record over as well as labelling it).
// These tests drive field_update, so it records nothing.
func (f *fakeGuardStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}

// SetOwner satisfies fieldUpdater (assign hands the record over as well as labelling it).
// These tests drive field_update, so it records nothing.
func (f *fakeRelMutStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}
