package workflow

// CREATE-RECORD unit tests — the recursion guard in isolation (no async, no DB). These prove the
// two in-package links of the depth loop deterministically:
//   (a) runCreateRecord SKIPS the action once evt.Depth >= maxDepth (the cap), and
//   (b) below the cap it invokes CreateRecord with ctx carrying evt.Depth+1 (WithChainDepth).
// The third link — services.emitCreateEvents reading that depth back onto the emitted event — is
// proven end-to-end by the real-chain crux test in the services package (create_record_test.go).

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// TestWithRecordCreator_ClampsNonPositiveMaxDepth pins the OTHER half of the recursion-guard's
// never-error contract (LoadWorkflowConfig's degrade-to-default is pinned separately in
// internal/config/workflow_config_test.go): a caller that wires the dispatcher directly with a
// non-positive maxDepth (0, negative, or a mis-plumbed zero-value) must clamp to
// DefaultMaxChainDepth — never leave the guard at 0 (which would skip EVERY create_record
// immediately) or unbounded. A leaked clamp here would silently neuter the recursion protection
// with no signal, exactly the failure mode this test exists to catch.
func TestWithRecordCreator_ClampsNonPositiveMaxDepth(t *testing.T) {
	tests := []struct {
		name  string
		given int
		want  int
	}{
		{name: "zero clamps to default", given: 0, want: DefaultMaxChainDepth},
		{name: "negative clamps to default", given: -3, want: DefaultMaxChainDepth},
		{name: "positive value passes through", given: 1, want: 1},
		{name: "larger positive value passes through", given: 12, want: 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, nil, nil, nil, zap.NewNop())
			defer func() { _ = d.Shutdown(context.Background()) }()
			d.WithRecordCreator(&crFakeCreator{t: t, ceiling: 0}, tt.given)
			if got := d.deps().maxDepth; got != tt.want {
				t.Fatalf("WithRecordCreator(maxDepth=%d) → deps().maxDepth=%d; want %d", tt.given, got, tt.want)
			}
		})
	}
}

// crFakeCreator simulates the production create → emit → re-process loop SYNCHRONOUSLY: each
// CreateRecord reads the chain depth the action stashed in ctx (exactly what emitCreateEvents does
// live), counts the create, records its arguments, and — when reprocess is wired — recursively
// re-runs the create_record action for the created record's after_create event at that depth. A
// broken guard would recurse without bound; the ceiling turns that into a loud t.Fatalf rather than
// a stack-overflow crash, so a regression FAILS the test instead of hanging it.
type crFakeCreator struct {
	t             *testing.T
	ceiling       int
	created       int
	reprocess     func(depth int)
	lastTenant    string
	lastEntity    string
	lastCreatedBy string
	lastValues    map[string]any
}

func (c *crFakeCreator) CreateRecord(ctx context.Context, tenantID, entity, createdBy string, values map[string]any) (string, error) {
	c.created++
	c.lastTenant, c.lastEntity, c.lastCreatedBy, c.lastValues = tenantID, entity, createdBy, values
	if c.created > c.ceiling {
		c.t.Fatalf("create_record recursion exceeded ceiling %d — guard leaked (would loop forever)", c.ceiling)
	}
	if c.reprocess != nil {
		// The created record's after_create event carries the depth the action stashed in ctx.
		c.reprocess(ChainDepthFromContext(ctx))
	}
	return "child-id", nil
}

// TestCreateRecord_RecursionCapStops is the deterministic crux: a self-referential A→A create_record
// chain must stop at exactly maxDepth firings, never loop. A broken guard trips the ceiling.
func TestCreateRecord_RecursionCapStops(t *testing.T) {
	const maxDepth = 5
	creator := &crFakeCreator{t: t, ceiling: 100}
	action := Action{Type: ActionCreateRecord, CreateEntity: "a", Values: map[string]any{"name": "child"}}

	var process func(depth int)
	process = func(depth int) {
		deps := actionDeps{creator: creator, maxDepth: maxDepth, logger: zap.NewNop()}
		evt := Event{TenantID: "t1", EntityName: "a", Trigger: TriggerAfterCreate, OwnerUserID: "u1", Depth: depth}
		if err := runCreateRecord(context.Background(), evt, action, deps); err != nil {
			t.Fatalf("runCreateRecord(depth=%d): %v", depth, err)
		}
	}
	creator.reprocess = process

	process(0) // seed the chain at depth 0 (a user-driven create)

	// Firings at depth 0..maxDepth-1 create a record each; the depth-maxDepth event skips. So the
	// automation creates EXACTLY maxDepth records — bounded, terminating, no infinite loop.
	if creator.created != maxDepth {
		t.Fatalf("create_record fired %d times; want exactly maxDepth=%d (chain must stop at the cap)", creator.created, maxDepth)
	}
}

// TestCreateRecord_SkipsAtCapNoError: at the cap the action is a logged no-op (nil error), NOT a
// failure — the rule worked as designed.
func TestCreateRecord_SkipsAtCapNoError(t *testing.T) {
	creator := &crFakeCreator{t: t, ceiling: 10}
	deps := actionDeps{creator: creator, maxDepth: 5, logger: zap.NewNop()}
	evt := Event{TenantID: "t1", EntityName: "a", Trigger: TriggerAfterCreate, OwnerUserID: "u1", Depth: 5}
	if err := runCreateRecord(context.Background(), evt, Action{Type: ActionCreateRecord, CreateEntity: "a", Values: map[string]any{"name": "x"}}, deps); err != nil {
		t.Fatalf("runCreateRecord at cap = %v; want nil (a cap-skip is a designed no-op)", err)
	}
	if creator.created != 0 {
		t.Fatalf("create ran %d times at the cap; want 0 (skipped)", creator.created)
	}
}

// TestCreateRecord_ResolvesOwnerAndAttributesCreatedBy: $owner resolves to the trigger record's
// owner, literals pass through, and created_by is the owner. tenant/entity are forwarded verbatim.
func TestCreateRecord_ResolvesOwnerAndAttributesCreatedBy(t *testing.T) {
	creator := &crFakeCreator{t: t, ceiling: 10}
	action := Action{Type: ActionCreateRecord, CreateEntity: "tasks", Values: map[string]any{
		"subject":  "Follow up",
		"assignee": OwnerSentinel,
	}}
	deps := actionDeps{creator: creator, maxDepth: 5, logger: zap.NewNop()}
	evt := Event{TenantID: "t1", EntityName: "leads", Trigger: TriggerAfterCreate, OwnerUserID: "owner-9", Depth: 0}
	if err := runCreateRecord(context.Background(), evt, action, deps); err != nil {
		t.Fatalf("runCreateRecord: %v", err)
	}
	if creator.created != 1 {
		t.Fatalf("created=%d; want 1", creator.created)
	}
	if creator.lastCreatedBy != "owner-9" {
		t.Errorf("createdBy=%q; want owner-9 (the trigger record's owner)", creator.lastCreatedBy)
	}
	if creator.lastTenant != "t1" || creator.lastEntity != "tasks" {
		t.Errorf("tenant/entity = %q/%q; want t1/tasks", creator.lastTenant, creator.lastEntity)
	}
	if got := creator.lastValues["assignee"]; got != "owner-9" {
		t.Errorf("$owner resolved to %v; want owner-9", got)
	}
	if got := creator.lastValues["subject"]; got != "Follow up" {
		t.Errorf("literal value mangled: %v", got)
	}
}

// TestCreateRecord_EmptyOwnerFallsBackToSystemActor: an unresolvable owner falls back to the system
// actor for created_by, and an unresolvable "$owner" value is OMITTED (never written literally).
func TestCreateRecord_EmptyOwnerFallsBackToSystemActor(t *testing.T) {
	creator := &crFakeCreator{t: t, ceiling: 10}
	action := Action{Type: ActionCreateRecord, CreateEntity: "tasks", Values: map[string]any{
		"assignee": OwnerSentinel,
		"subject":  "x",
	}}
	deps := actionDeps{creator: creator, maxDepth: 5, logger: zap.NewNop()}
	evt := Event{TenantID: "t1", EntityName: "leads", Trigger: TriggerAfterCreate, OwnerUserID: "", Depth: 0}
	if err := runCreateRecord(context.Background(), evt, action, deps); err != nil {
		t.Fatalf("runCreateRecord: %v", err)
	}
	if creator.lastCreatedBy != SystemActorID {
		t.Errorf("createdBy=%q; want SystemActorID fallback", creator.lastCreatedBy)
	}
	if v, present := creator.lastValues["assignee"]; present {
		t.Errorf("unresolvable $owner must be omitted, got %v", v)
	}
}

// TestCreateRecord_NoCreatorWired_Errors: a create_record action with no creator wired errors
// (logged fail-open by the dispatcher) rather than panicking.
func TestCreateRecord_NoCreatorWired_Errors(t *testing.T) {
	deps := actionDeps{maxDepth: 5, logger: zap.NewNop()}
	err := runCreateRecord(context.Background(),
		Event{TenantID: "t1", Depth: 0},
		Action{Type: ActionCreateRecord, CreateEntity: "a", Values: map[string]any{"x": 1}},
		deps)
	if err == nil {
		t.Fatal("want an error when no creator is wired")
	}
}
