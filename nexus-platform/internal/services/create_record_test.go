package services

// CREATE-RECORD services tests. The authoritative crux test drives the REAL chain end-to-end (real
// RecordService.Create → real AsyncDispatcher → real WorkflowRecordCreator adapter → back into
// RecordService.Create) so it exercises ALL THREE links of the recursion-guard depth loop, including
// the production footgun: emitCreateEvents reading ChainDepthFromContext(ctx) onto the emitted event.
// A fake store (no MySQL) counts the cascade; a bounded-count + hard-cap + poll-timeout turns a
// leaked guard into a loud failure rather than an infinite loop / hang.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// crCountingStore is a RecordStore that records every InsertRecord (id + createdBy + data) so the
// crux test can count how many records the create_record cascade produced and inspect the first
// automation-created record. A hard cap fails closed (returns an error, stopping the cascade) AND
// flags overflow so a leaked guard cannot silently spin the CPU forever before the poll notices.
type crCountingStore struct {
	mu       sync.Mutex
	inserts  []crInsert
	hardCap  int
	overflow bool
}

type crInsert struct {
	id        string
	entity    string
	createdBy string
	data      map[string]any
}

func (s *crCountingStore) InsertRecord(_ context.Context, entity, _, userID string, data map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inserts) >= s.hardCap {
		s.overflow = true
		return "", fmt.Errorf("crCountingStore: hard cap %d exceeded — recursion guard leaked", s.hardCap)
	}
	id := fmt.Sprintf("rec-%d", len(s.inserts)+1)
	s.inserts = append(s.inserts, crInsert{id: id, entity: entity, createdBy: userID, data: data})
	return id, nil
}

func (s *crCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inserts)
}

func (s *crCountingStore) at(i int) (crInsert, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.inserts) {
		return crInsert{}, false
	}
	return s.inserts[i], true
}

// Remaining RecordStore methods are unused by Create (no relations/auto_number in the test entity).
func (s *crCountingStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return nil, nil
}
func (s *crCountingStore) ListRecords(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return nil, 0, nil
}
func (s *crCountingStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return nil, "", nil
}
func (s *crCountingStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *crCountingStore) SoftDeleteRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) error {
	return nil
}
func (s *crCountingStore) RestoreRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *crCountingStore) RecordExists(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}
func (s *crCountingStore) BatchRecordExists(_ context.Context, _, _ string, _ []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (s *crCountingStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	return dynrepo.AggregateScalar{}, nil
}
func (s *crCountingStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	return nil, nil
}
func (s *crCountingStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}
func (s *crCountingStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}

// crFakeLoader serves a fixed set of active workflow rules, filtered by (entityName, trigger) —
// like the real cache-backed loader. rules may span multiple entities (the A→B→A cascade test
// needs a rule on "a" AND a rule on "b").
type crFakeLoader struct {
	rules []workflow.Workflow
}

func (l *crFakeLoader) ListActive(_ context.Context, _, entityName string, trigger workflow.Trigger) ([]workflow.Workflow, error) {
	var out []workflow.Workflow
	for _, wf := range l.rules {
		if wf.EntityName == entityName && wf.Trigger == trigger {
			out = append(out, wf)
		}
	}
	return out, nil
}

// crMultiCatalog is an EntityCatalog serving MULTIPLE named entities, keyed by name (unlike
// fakeRecordCatalog in record_list_test.go, which returns ONE fixed entity regardless of the
// requested name — unusable here since the A→B→A cascade test resolves both "a" and "b").
type crMultiCatalog struct {
	entities map[string]*metadata.EntityDefinition
	fields   map[string][]metadata.FieldDefinition // keyed by entity ID
}

func (c *crMultiCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	e, ok := c.entities[name]
	if !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return e, nil
}

func (c *crMultiCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return c.fields[entityID], nil
}

// TestCreateRecord_RealChain_RecursionCapBoundsCascade is the mandatory crux: a real after_create
// A→A create_record rule, driven by a real RecordService.Create through the real async dispatcher,
// must produce a BOUNDED cascade that terminates at the depth cap — never an infinite loop.
func TestCreateRecord_RealChain_RecursionCapBoundsCascade(t *testing.T) {
	const maxDepth = 5

	loader := &crFakeLoader{rules: []workflow.Workflow{{
		ID:         "wf-cr",
		TenantID:   "t1",
		Name:       "spawn A",
		EntityName: "a",
		Trigger:    workflow.TriggerAfterCreate,
		Actions: []workflow.Action{{
			Type:         workflow.ActionCreateRecord,
			CreateEntity: "a",
			Values:       map[string]any{"name": "child"},
		}},
		IsActive: true,
	}}}

	// Single worker + a generous buffer so the cascade is serial (deterministic) and no event is
	// dropped for buffer-full (which would truncate the chain and flake the count).
	//
	// Deliberately NOT calling Shutdown: unlike the drain()-based integration tests (which emit only
	// from the test goroutine), this dispatcher RE-EMITS from inside its own worker (the create_record
	// cascade). Shutdown closes the channel, which the race detector flags against an in-flight
	// re-entrant Emit (an inherent send/close interaction of a cascading dispatcher). The poll below
	// already proves quiescence (the cascade has stopped), so we leave the now-idle worker blocked on
	// the empty channel — harmless in a unit test and race-clean (it accesses nothing further).
	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 256, Workers: 1}, loader, nil, nil, zap.NewNop())

	catalog := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-a", TenantID: "t1", Name: "a", DataTable: "obj_a"},
		fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
	}
	store := &crCountingStore{hardCap: 200}
	recordSvc := NewRecordService(catalog, store).WithEngine(dispatcher)
	// Break the construction cycle: wire the creator over the built service, then bound recursion.
	dispatcher.WithRecordCreator(NewWorkflowRecordCreator(recordSvc), maxDepth)

	// Seed the chain: a user-driven create (depth 0). This inserts rec-1 and emits after_create.
	if _, err := recordSvc.Create(context.Background(), RecordScope{IsAdmin: true}, "t1", "seeder", "a",
		map[string]any{"name": "seed"}); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// Poll until the cascade settles. Expected total = 1 seed + maxDepth automation records. A leaked
	// guard would exceed that (or trip the store hard cap) → loud failure; a stalled chain → timeout.
	want := 1 + maxDepth
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := store.count()
		if c > want {
			t.Fatalf("cascade produced %d records; recursion guard leaked (want exactly %d)", c, want)
		}
		if c == want {
			// Confirm it does NOT grow further (no late create slipped past the cap).
			time.Sleep(75 * time.Millisecond)
			if final := store.count(); final != want {
				t.Fatalf("record count grew to %d after settling; guard leaked (want %d)", final, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cascade did not reach %d records within timeout (got %d) — chain stalled", want, store.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if store.overflow {
		t.Fatal("store hard cap tripped — recursion guard leaked (unbounded creation)")
	}

	// Happy-path assertions on the first automation-created record (rec-2, index 1): the right values,
	// tenant scoping (single-tenant store), and created_by attributed to the trigger record's owner
	// (the seed's owner is "seeder", carried forward through the chain).
	if rec, ok := store.at(1); ok {
		if rec.createdBy != "seeder" {
			t.Errorf("automation record created_by = %q; want the trigger owner \"seeder\"", rec.createdBy)
		}
		if rec.data["name"] != "child" {
			t.Errorf("automation record name = %v; want \"child\" (the action's value)", rec.data["name"])
		}
	} else {
		t.Fatal("expected at least one automation-created record")
	}
}

// TestCreateRecord_RealChain_CrossEntityABAB_RecursionCapBounds hardens the invariant the A→A test
// above relies on by construction (self-reference): the guard's depth counter is carried on the
// EVENT and is entirely entity-agnostic, so an A→B→A→B… cascade across TWO distinct entities must be
// bounded by the SAME depth cap, not just a self-referential single-entity loop. Two rules — "a"
// creates "b", "b" creates "a" — alternate every hop; the chain must still stop at exactly maxDepth
// automation-created records (bounded total = 1 seed + maxDepth), never loop, and each
// automation-created record must be of the CORRECT alternating entity (not a leaked single-entity
// resolution — see crMultiCatalog's doc comment on why fakeRecordCatalog cannot serve this case).
func TestCreateRecord_RealChain_CrossEntityABAB_RecursionCapBounds(t *testing.T) {
	const maxDepth = 5

	loader := &crFakeLoader{rules: []workflow.Workflow{
		{
			ID: "wf-a-to-b", TenantID: "t1", Name: "a spawns b", EntityName: "a", Trigger: workflow.TriggerAfterCreate,
			Actions:  []workflow.Action{{Type: workflow.ActionCreateRecord, CreateEntity: "b", Values: map[string]any{"name": "toB"}}},
			IsActive: true,
		},
		{
			ID: "wf-b-to-a", TenantID: "t1", Name: "b spawns a", EntityName: "b", Trigger: workflow.TriggerAfterCreate,
			Actions:  []workflow.Action{{Type: workflow.ActionCreateRecord, CreateEntity: "a", Values: map[string]any{"name": "toA"}}},
			IsActive: true,
		},
	}}

	// Single worker + generous buffer — see the A→A test above for why Shutdown is deliberately
	// skipped (re-entrant Emit vs. graceful close is an inherent race of a cascading dispatcher; the
	// poll below already proves quiescence).
	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 256, Workers: 1}, loader, nil, nil, zap.NewNop())

	catalog := &crMultiCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"a": {ID: "ent-a", TenantID: "t1", Name: "a", DataTable: "obj_a"},
			"b": {ID: "ent-b", TenantID: "t1", Name: "b", DataTable: "obj_b"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"ent-a": {{Name: "name", FieldType: "text"}},
			"ent-b": {{Name: "name", FieldType: "text"}},
		},
	}
	store := &crCountingStore{hardCap: 200}
	recordSvc := NewRecordService(catalog, store).WithEngine(dispatcher)
	dispatcher.WithRecordCreator(NewWorkflowRecordCreator(recordSvc), maxDepth)

	// Seed: a user-driven create of "a" (depth 0) — kicks off a → b → a → b … alternating cascade.
	if _, err := recordSvc.Create(context.Background(), RecordScope{IsAdmin: true}, "t1", "seeder", "a",
		map[string]any{"name": "seed"}); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	want := 1 + maxDepth
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := store.count()
		if c > want {
			t.Fatalf("cross-entity cascade produced %d records; recursion guard leaked (want exactly %d)", c, want)
		}
		if c == want {
			time.Sleep(75 * time.Millisecond)
			if final := store.count(); final != want {
				t.Fatalf("record count grew to %d after settling; guard leaked (want %d)", final, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cross-entity cascade did not reach %d records within timeout (got %d) — chain stalled", want, store.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if store.overflow {
		t.Fatal("store hard cap tripped — recursion guard leaked (unbounded cross-entity creation)")
	}

	// Entity alternation: rec-1 is the seed ("a"), then every automation hop flips entity — b, a, b, …
	for i := 0; i < want; i++ {
		wantEntity := "a"
		if i%2 == 1 {
			wantEntity = "b"
		}
		rec, ok := store.at(i)
		if !ok {
			t.Fatalf("expected record at index %d, got none", i)
		}
		if rec.entity != wantEntity {
			t.Errorf("record %d entity = %q; want %q (chain must alternate a↔b, not collapse to one entity)", i, rec.entity, wantEntity)
		}
	}
}

// ---- config-time validation (checkCreateRecordTargets + buildActionsJSON) ---------------------

func crCreateInput(targetEntity string, values map[string]any) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name:       "cr",
		EntityName: "deals", // trigger entity (exists in w6Catalog)
		Trigger:    "after_create",
		Actions:    []WorkflowActionInput{{Type: "create_record", CreateEntity: targetEntity, Values: values}},
		ActorID:    "u1",
	}
}

// TestCreateRecord_ConfigTimeValidation rejects a create_record rule whose target entity is unknown
// or whose values name an unknown/computed/auto_number field or carry a bad value type — the SAME
// guard family field_update/assign use (WF-01), applied to the TARGET entity. It accepts a valid rule.
func TestCreateRecord_ConfigTimeValidation(t *testing.T) {
	tests := []struct {
		name     string
		entity   string
		values   map[string]any
		wantErr  bool
		wantCode string
	}{
		{name: "valid literal into text field", entity: "deals", values: map[string]any{"stage": "new"}, wantErr: false},
		{name: "empty values rejected", entity: "deals", values: map[string]any{}, wantErr: true, wantCode: "CREATE_RECORD_VALUES_REQUIRED"},
		{name: "empty target entity rejected", entity: "", values: map[string]any{"stage": "x"}, wantErr: true, wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "unknown target entity rejected", entity: "ghosts", values: map[string]any{"stage": "x"}, wantErr: true, wantCode: "CREATE_RECORD_ENTITY_INVALID"},
		{name: "auto_number target rejected", entity: "deals", values: map[string]any{"deal_no": "X-1"}, wantErr: true, wantCode: "RECORD_AUTONUMBER_READONLY"},
		{name: "formula target rejected", entity: "deals", values: map[string]any{"total": 5}, wantErr: true, wantCode: "RECORD_FORMULA_READONLY"},
		{name: "rollup target rejected", entity: "deals", values: map[string]any{"line_sum": 1}, wantErr: true, wantCode: "RECORD_ROLLUP_READONLY"},
		{name: "unknown field rejected", entity: "deals", values: map[string]any{"ghost": 1}, wantErr: true, wantCode: "RECORD_UNKNOWN_FIELD"},
		{name: "bad value type rejected", entity: "deals", values: map[string]any{"amount": []any{1, 2}}, wantErr: true, wantCode: "RECORD_INVALID_TYPE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", crCreateInput(tt.entity, tt.values))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("CreateWorkflow = %v; want nil", err)
				}
				if len(repo.created) != 1 {
					t.Fatalf("expected 1 persisted rule, got %d", len(repo.created))
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateWorkflow = nil error; want code %s", tt.wantCode)
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

// crCreateAction builds one create_record action targeting a dummy entity with one value. Config-time
// shape validation (buildActionsJSON) only needs a non-empty entityName + one value; nil fieldTypes
// skips the DB-dependent target checks, so no catalog wiring is needed.
func crCreateAction() WorkflowActionInput {
	return WorkflowActionInput{Type: "create_record", CreateEntity: "deals", Values: map[string]any{"stage": "x"}}
}

// TestValidateWorkflowConfig_CreateRecordCap covers the fan-out branch-factor cap (#79 partial):
// buildActionsJSON — the ONE config body shared by CreateWorkflow/UpdateWorkflow/ValidateWorkflowConfig
// AND pack-install AND the AI propose path — rejects a rule with more than maxCreateRecordActionsPerRule
// create_record actions. Non-create_record actions do not count toward the cap.
func TestValidateWorkflowConfig_CreateRecordCap(t *testing.T) {
	notify := WorkflowActionInput{Type: "in_app_notification", UserID: "u1", Message: "hi"}
	fieldUpd := WorkflowActionInput{Type: "field_update", Field: "stage", Value: "won"}
	tests := []struct {
		name     string
		actions  []WorkflowActionInput
		wantCode string // "" = expect ok
	}{
		{name: "4 create_record over cap", actions: []WorkflowActionInput{crCreateAction(), crCreateAction(), crCreateAction(), crCreateAction()}, wantCode: "WORKFLOW_TOO_MANY_CREATE_RECORD"},
		{name: "exactly cap (3) ok", actions: []WorkflowActionInput{crCreateAction(), crCreateAction(), crCreateAction()}},
		{name: "3 create_record + other-type actions ok", actions: []WorkflowActionInput{crCreateAction(), notify, crCreateAction(), fieldUpd, crCreateAction()}},
		{name: "0 create_record ok", actions: []WorkflowActionInput{notify, fieldUpd}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// nil fieldTypes → skip WF-01 target check; the cap is in the pure body, so it still applies.
			err := ValidateWorkflowConfig("deals", "after_create", nil, tt.actions, nil)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateWorkflowConfig = %v; want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error type = %T (%v); want *ValidationError", err, err)
			}
			if ve.Code != tt.wantCode {
				t.Errorf("code = %s; want %s", ve.Code, tt.wantCode)
			}
		})
	}
}

// TestCreateRecord_OwnerSentinelValueSkipsTypeCheck: a "$owner" value on a create_record target
// field is accepted at config time (it resolves to a user id at fire time, not a config-time literal).
func TestCreateRecord_OwnerSentinelValueSkipsTypeCheck(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog())
	// "stage" is a plain text field; "$owner" is a sentinel string — accepted without a type probe.
	_, err := svc.CreateWorkflow(context.Background(), "t1", crCreateInput("deals", map[string]any{"stage": workflow.OwnerSentinel}))
	if err != nil {
		t.Fatalf("CreateWorkflow with $owner value = %v; want nil (sentinel resolves at fire time)", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("expected the rule persisted, got %d", len(repo.created))
	}
}
