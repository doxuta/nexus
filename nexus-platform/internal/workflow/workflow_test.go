package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------------------------
// Condition evaluation (pure, no DB).
// ---------------------------------------------------------------------------------------------

func TestEvaluate_NilOrEmptyMatchesAll(t *testing.T) {
	data := map[string]any{"status": "won"}
	if !Evaluate(nil, data) {
		t.Errorf("nil condition should match all")
	}
	if !Evaluate(&Condition{}, data) {
		t.Errorf("empty rules should match all")
	}
}

func TestEvaluate_Operators(t *testing.T) {
	data := map[string]any{
		"status": "won",
		"amount": float64(1500),
		"note":   "high priority deal",
		"owner":  nil,
	}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"eq match", Rule{Field: "status", Op: OpEq, Value: "won"}, true},
		{"eq no match", Rule{Field: "status", Op: OpEq, Value: "lost"}, false},
		{"eq numeric-as-string", Rule{Field: "amount", Op: OpEq, Value: "1500"}, true},
		{"neq match", Rule{Field: "status", Op: OpNeq, Value: "lost"}, true},
		{"gt match", Rule{Field: "amount", Op: OpGt, Value: float64(1000)}, true},
		{"gt no match", Rule{Field: "amount", Op: OpGt, Value: float64(2000)}, false},
		{"gte boundary", Rule{Field: "amount", Op: OpGte, Value: float64(1500)}, true},
		{"lt match", Rule{Field: "amount", Op: OpLt, Value: float64(2000)}, true},
		{"lte boundary", Rule{Field: "amount", Op: OpLte, Value: float64(1500)}, true},
		{"gt on non-numeric field", Rule{Field: "status", Op: OpGt, Value: float64(1)}, false},
		{"contains match", Rule{Field: "note", Op: OpContains, Value: "priority"}, true},
		{"contains no match", Rule{Field: "note", Op: OpContains, Value: "low"}, false},
		{"isnull on nil field", Rule{Field: "owner", Op: OpIsNull}, true},
		{"isnull on absent field", Rule{Field: "missing", Op: OpIsNull}, true},
		{"isnull on present field", Rule{Field: "status", Op: OpIsNull}, false},
		{"notnull on present", Rule{Field: "status", Op: OpNotNull}, true},
		{"notnull on nil", Rule{Field: "owner", Op: OpNotNull}, false},
		{"unknown op fails safe", Rule{Field: "status", Op: "weird", Value: "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(&Condition{Rules: []Rule{tc.rule}}, data)
			if got != tc.want {
				t.Errorf("Evaluate(%+v) = %v; want %v", tc.rule, got, tc.want)
			}
		})
	}
}

func TestEvaluate_AndJoin(t *testing.T) {
	data := map[string]any{"status": "won", "amount": float64(1500)}
	// Both rules true → match.
	allTrue := &Condition{Match: "and", Rules: []Rule{
		{Field: "status", Op: OpEq, Value: "won"},
		{Field: "amount", Op: OpGte, Value: float64(1000)},
	}}
	if !Evaluate(allTrue, data) {
		t.Errorf("all-true AND condition should match")
	}
	// One rule false → no match (AND semantics).
	oneFalse := &Condition{Match: "and", Rules: []Rule{
		{Field: "status", Op: OpEq, Value: "won"},
		{Field: "amount", Op: OpGt, Value: float64(5000)},
	}}
	if Evaluate(oneFalse, data) {
		t.Errorf("one-false AND condition should NOT match")
	}
}

func TestEvaluate_OrJoin(t *testing.T) {
	data := map[string]any{"status": "won", "amount": float64(1500)}
	// One rule true (the other false) → match under OR.
	oneTrue := &Condition{Match: "or", Rules: []Rule{
		{Field: "status", Op: OpEq, Value: "lost"},         // false
		{Field: "amount", Op: OpGte, Value: float64(1000)}, // true
	}}
	if !Evaluate(oneTrue, data) {
		t.Errorf("OR condition with one true rule should match")
	}
	// All rules false → no match under OR.
	allFalse := &Condition{Match: "or", Rules: []Rule{
		{Field: "status", Op: OpEq, Value: "lost"},
		{Field: "amount", Op: OpGt, Value: float64(5000)},
	}}
	if Evaluate(allFalse, data) {
		t.Errorf("OR condition with all-false rules should NOT match")
	}
	// Case-insensitive "OR" is honoured.
	if !Evaluate(&Condition{Match: "OR", Rules: []Rule{
		{Field: "status", Op: OpEq, Value: "lost"},
		{Field: "status", Op: OpEq, Value: "won"},
	}}, data) {
		t.Errorf("case-insensitive OR should match when one rule is true")
	}
	// The SAME rule set that fails under AND (one rule false) passes under OR — the discriminator.
	andFails := []Rule{{Field: "status", Op: OpEq, Value: "won"}, {Field: "amount", Op: OpGt, Value: float64(5000)}}
	if Evaluate(&Condition{Match: "and", Rules: andFails}, data) {
		t.Errorf("sanity: this set must fail AND")
	}
	if !Evaluate(&Condition{Match: "or", Rules: andFails}, data) {
		t.Errorf("the same set must pass OR (one rule true)")
	}
}

// ---------------------------------------------------------------------------------------------
// Action execution (fakes — no DB).
// ---------------------------------------------------------------------------------------------

type fakeMutator struct {
	mu    sync.Mutex
	calls []struct {
		entity, tenant, record, field string
		value                         any
	}
	// owners records every SetOwner the engine performed, keyed by record id — assign is now
	// supposed to hand the record over, not just label it.
	owners   map[string]string
	ownerErr error
	err      error
	// related maps "entity.via" → the entity that relation points at (RelatedEntity, specs/058);
	// relatedErr makes every resolution fail.
	related    map[string]string
	relatedErr error
	// rows/versions back ReadForUpdate + UpdateFieldsIfVersion (#172 tighten_only). conflicts makes
	// that many conditional writes lose (ErrRecordChanged), each time first replacing the row with
	// raceRow — the save that slipped in between the rule's read and its write. casCalls counts the
	// conditional attempts; a successful one appends to calls in sorted field order.
	rows      map[string]map[string]any
	versions  map[string]int
	conflicts int
	raceRow   map[string]any
	casCalls  int
}

func (m *fakeMutator) ReadForUpdate(_ context.Context, _, _, recordID string) (map[string]any, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]any{}
	for k, v := range m.rows[recordID] {
		out[k] = v
	}
	return out, m.versions[recordID], nil
}

func (m *fakeMutator) UpdateFieldsIfVersion(_ context.Context, entity, tenantID, recordID, _ string, fields map[string]any, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.casCalls++
	if m.versions == nil {
		m.versions = map[string]int{}
	}
	if m.rows == nil {
		m.rows = map[string]map[string]any{}
	}
	if m.conflicts > 0 {
		m.conflicts--
		m.rows[recordID] = m.raceRow
		m.versions[recordID]++
		return ErrRecordChanged
	}
	if version != m.versions[recordID] {
		return ErrRecordChanged
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	row := map[string]any{}
	for k, v := range m.rows[recordID] {
		row[k] = v
	}
	for _, k := range keys {
		m.calls = append(m.calls, struct {
			entity, tenant, record, field string
			value                         any
		}{entity, tenantID, recordID, k, fields[k]})
		row[k] = fields[k]
	}
	m.rows[recordID] = row
	m.versions[recordID]++
	return m.err
}

func (m *fakeMutator) RelatedEntity(_ context.Context, _, entity, via string) (string, error) {
	if m.relatedErr != nil {
		return "", m.relatedErr
	}
	target, ok := m.related[entity+"."+via]
	if !ok {
		return "", fmt.Errorf("fakeMutator: %s.%s is not a relation", entity, via)
	}
	return target, nil
}

func (m *fakeMutator) SetOwner(_ context.Context, _, _, recordID, _ string, newOwnerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ownerErr != nil {
		return m.ownerErr
	}
	if m.owners == nil {
		m.owners = map[string]string{}
	}
	m.owners[recordID] = newOwnerID
	return nil
}

func (m *fakeMutator) UpdateField(_ context.Context, entity, tenantID, recordID, _ string, field string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, struct {
		entity, tenant, record, field string
		value                         any
	}{entity, tenantID, recordID, field, value})
	return m.err
}

type fakeNotif struct {
	mu    sync.Mutex
	rows  []Notification
	err   error
	calls int
}

func (n *fakeNotif) InsertNotification(_ context.Context, row Notification) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.err != nil {
		return "", n.err
	}
	n.rows = append(n.rows, row)
	return "notif-id", nil
}

func TestRunActions_FieldUpdate(t *testing.T) {
	mut := &fakeMutator{}
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}}}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate, Data: map[string]any{"status": "won"}}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions field_update err = %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("expected 1 UpdateField call, got %d", len(mut.calls))
	}
	c := mut.calls[0]
	if c.entity != "deals" || c.tenant != "t1" || c.record != "r1" || c.field != "stage" || c.value != "closed" {
		t.Errorf("UpdateField call = %+v; want deals/t1/r1/stage/closed", c)
	}
}

func TestRunActions_FieldUpdate_SkippedOnDelete(t *testing.T) {
	mut := &fakeMutator{}
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}}}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterDelete}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions on delete err = %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("field_update on after_delete should be a no-op, got %d calls", len(mut.calls))
	}
}

func TestRunActions_InAppNotification(t *testing.T) {
	notif := &fakeNotif{}
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionInAppNotification, UserID: "u9", Title: "Deal won", Message: "Deal r1 is won"}}}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{notif: notif}); err != nil {
		t.Fatalf("runActions notification err = %v", err)
	}
	if len(notif.rows) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.rows))
	}
	row := notif.rows[0]
	if row.TenantID != "t1" || row.UserID != "u9" || row.Message != "Deal r1 is won" ||
		row.EntityName != "deals" || row.RecordID != "r1" || row.WorkflowID != "wf1" {
		t.Errorf("notification row = %+v; want tenant/user/message/entity/record/workflow set", row)
	}
}

// TestRunActions_InAppNotification_OwnerSentinel_Resolved is the MED-1 fix proof (review
// 2026-07-08): an in_app_notification action whose userId is the "$owner" sentinel resolves to
// the EVENT's OwnerUserID (the record's created_by, populated by every caller of Event — see
// record_orchestration.go/scheduler.go/workflow_runs.go), NOT the literal string "$owner". Prior
// to this fix, the literal sentinel was inserted as user_id — a notification to a nonexistent
// user, silently dead on every rule using it (both hand-built FE rules and AI-proposed ones).
func TestRunActions_InAppNotification_OwnerSentinel_Resolved(t *testing.T) {
	notif := &fakeNotif{}
	wf := Workflow{ID: "wf1", Actions: []Action{
		{Type: ActionInAppNotification, UserID: "$owner", Title: "Xong", Message: "Hồ sơ đã đóng"},
	}}
	// user-x created this record — evt.OwnerUserID carries it (as every real emit site does).
	evt := Event{TenantID: "t1", EntityName: "treatments", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "done"}, OwnerUserID: "user-x"}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{notif: notif}); err != nil {
		t.Fatalf("runActions $owner notification err = %v", err)
	}
	if len(notif.rows) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.rows))
	}
	row := notif.rows[0]
	if row.UserID != "user-x" {
		t.Fatalf("UserID = %q; want the resolved owner %q, NOT the literal sentinel", row.UserID, "user-x")
	}
	if row.UserID == OwnerSentinel {
		t.Fatalf("the literal sentinel %q must NEVER be persisted as a notification recipient", OwnerSentinel)
	}
}

// TestRunActions_InAppNotification_OwnerSentinel_NoOwner_EmptyRecipientError proves the "$owner"
// resolution does NOT insert a dead row when the owner cannot be resolved (e.g. a best-effort
// before-image read failed upstream, leaving Event.OwnerUserID empty) — it falls through to the
// SAME empty-recipient error an empty a.UserID already produces, matching the "keep the existing
// empty-recipient error" requirement.
func TestRunActions_InAppNotification_OwnerSentinel_NoOwner_EmptyRecipientError(t *testing.T) {
	notif := &fakeNotif{}
	wf := Workflow{ID: "wf1", Actions: []Action{
		{Type: ActionInAppNotification, UserID: "$owner", Message: "no owner reachable"},
	}}
	evt := Event{TenantID: "t1", EntityName: "treatments", RecordID: "r1", Trigger: TriggerAfterUpdate} // OwnerUserID unset

	_, err := runActions(context.Background(), wf, evt, actionDeps{notif: notif})
	if err == nil {
		t.Fatal("expected the empty-recipient error when $owner cannot be resolved")
	}
	if len(notif.rows) != 0 {
		t.Fatalf("an unresolvable $owner must NEVER insert a dead notification row, got %d", len(notif.rows))
	}
}

func TestRunActions_ContinuesAfterError(t *testing.T) {
	// First action errors; the second must still run, and runActions returns the first error.
	mut := &fakeMutator{err: errors.New("boom")}
	notif := &fakeNotif{}
	wf := Workflow{ID: "wf1", Actions: []Action{
		{Type: ActionFieldUpdate, Field: "stage", Value: "x"},
		{Type: ActionInAppNotification, UserID: "u9", Message: "still fires"},
	}}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate}

	_, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut, notif: notif})
	if err == nil {
		t.Fatalf("expected an error from the failing field_update")
	}
	if len(notif.rows) != 1 {
		t.Errorf("second action (notification) must still run despite first action error; got %d rows", len(notif.rows))
	}
}

// ---------------------------------------------------------------------------------------------
// Async dispatcher — fail-open + correct routing (fakes).
// ---------------------------------------------------------------------------------------------

type fakeLoader struct {
	mu        sync.Mutex
	workflows []Workflow
	err       error
	calls     int
}

func (l *fakeLoader) ListActive(_ context.Context, tenantID, entityName string, trigger Trigger) ([]Workflow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	var out []Workflow
	for _, w := range l.workflows {
		if w.TenantID == tenantID && w.EntityName == entityName && w.Trigger == trigger {
			out = append(out, w)
		}
	}
	return out, nil
}

func drainDispatcher(t *testing.T, d *AsyncDispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain: %v", err)
	}
}

func TestDispatcher_MatchRunsAction(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate, IsActive: true,
		Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "won"}}},
		Actions:   []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}},
	}}}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop())

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "won"},
	})
	drainDispatcher(t, d)

	if len(mut.calls) != 1 {
		t.Fatalf("matching condition should run field_update once, got %d", len(mut.calls))
	}
}

func TestDispatcher_NoMatchSkips(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate, IsActive: true,
		Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "won"}}},
		Actions:   []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}},
	}}}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop())

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "lost"}, // condition wants "won"
	})
	drainDispatcher(t, d)

	if len(mut.calls) != 0 {
		t.Fatalf("non-matching condition should skip the action, got %d calls", len(mut.calls))
	}
}

func TestDispatcher_FailOpenOnLoadError(t *testing.T) {
	loader := &fakeLoader{err: errors.New("db down")}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop())

	// Must not panic / block even though the loader errors.
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate})
	drainDispatcher(t, d)

	if len(mut.calls) != 0 {
		t.Errorf("a load error must run no actions, got %d", len(mut.calls))
	}
}

func TestDispatcher_FailOpenOnActionError(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate, IsActive: true,
		Actions: []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}},
	}}}
	mut := &fakeMutator{err: errors.New("update failed")}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop())

	// An action error must be swallowed (logged) — no panic, drain completes.
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate})
	drainDispatcher(t, d)

	if len(mut.calls) != 1 {
		t.Errorf("action should have been attempted once, got %d", len(mut.calls))
	}
}

func TestDispatcher_EmitAfterShutdownDropped(t *testing.T) {
	loader := &fakeLoader{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, nil, nil, zap.NewNop())
	drainDispatcher(t, d)

	// Emitting after shutdown must NOT panic (no send on closed channel) and must be a no-op.
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate})
}

func TestDispatcher_NilLoaderIsNoOp(t *testing.T) {
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, nil, mut, nil, zap.NewNop())
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate})
	drainDispatcher(t, d)
	if len(mut.calls) != 0 {
		t.Errorf("nil loader should run nothing, got %d calls", len(mut.calls))
	}
}

// TestEvaluate_CurrencyCompositeComparesOnAmount: a currency field is {amount, currencyCode}; the
// ordered operators and eq read its amount (before this, every comparison on a money field was false).
func TestEvaluate_CurrencyCompositeComparesOnAmount(t *testing.T) {
	data := map[string]any{
		"price": map[string]any{"amount": float64(80000000), "currencyCode": "VND"},
		"odd":   map[string]any{"currencyCode": "VND"}, // no amount: not a number
	}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"gte holds", Rule{Field: "price", Op: OpGte, Value: float64(50000000)}, true},
		{"gt fails above", Rule{Field: "price", Op: OpGt, Value: float64(90000000)}, false},
		{"lt holds", Rule{Field: "price", Op: OpLt, Value: float64(100000000)}, true},
		{"eq on the amount", Rule{Field: "price", Op: OpEq, Value: float64(80000000)}, true},
		{"a composite without an amount is not numeric", Rule{Field: "odd", Op: OpGt, Value: float64(0)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Evaluate(&Condition{Rules: []Rule{tc.rule}}, data); got != tc.want {
				t.Errorf("Evaluate(%+v) = %v; want %v", tc.rule, got, tc.want)
			}
		})
	}
}
