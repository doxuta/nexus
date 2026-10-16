package bpm

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// --- fakes -------------------------------------------------------------------------------------

// fakeStore is an in-memory InstanceStore + ProcessGetter + BreachStore + InboxWriter for unit
// tests (no DB). It records the arming insert + the guarded transition so a test can assert the
// sequential chain, rejection, and SLA breach without MySQL.
type fakeStore struct {
	pending     map[string]bool // key: tenant|entity|record → pending?
	created     []CreateInstanceInput
	instances   map[string]*Instance      // by id (for GetInstance/DecideTaskTx)
	processes   map[string]*ProcessRecord // by id (for GetProcess)
	notes       []Notification
	advanceCnt  int
	breachClaim map[string]bool // id → claimed
	// S3b seams: the open seats ListOpenSeats answers with, a hook that runs after every
	// GetInstance (the window between a read and the write that follows it), and the tenants
	// each per-tenant breach scan was asked about.
	seats      map[string][]Seat
	onGet      func(*fakeStore, *Instance)
	scanned    []string
	claimedSeq map[string]int64    // id → the seq the claim presented
	roleUsers  map[string][]string // role id → the users carrying it (ResolveSeatUsers)
	teamUsers  map[string][]string // team id → its members (ResolveSeatUsers, specs/043 S6a)
	// lastNext is the transition the last DecideTaskTx applied (seat side included), for tests that
	// assert what a decision did to the seats.
	lastNext InstanceTransition
	// S4 seams: the seats (any state) of each instance's current visit, and the votes taken.
	visits  map[string][]VisitSeat
	voteCnt int
	// beforeVote runs at the top of VoteTaskTx: the window in which a sibling's vote can land between
	// a caller's read and its transaction.
	beforeVote func(*fakeStore)
	// S7 seams: every transition DecideTaskTx applied, in order (the machine-node walk is asserted
	// on them — which node each hop landed on, whether a continue job was parked).
	transitions []InstanceTransition
	// S9a seams: errors the arm-side store calls answer with.
	openErr, createErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		pending:     map[string]bool{},
		instances:   map[string]*Instance{},
		processes:   map[string]*ProcessRecord{},
		breachClaim: map[string]bool{},
		seats:       map[string][]Seat{},
		claimedSeq:  map[string]int64{},
		visits:      map[string][]VisitSeat{},
	}
}

func pkey(tenant, entity, record string) string { return tenant + "|" + entity + "|" + record }

func (f *fakeStore) HasOpenExclusive(_ context.Context, tenant, entity, record string) (bool, error) {
	if f.openErr != nil {
		return false, f.openErr
	}
	return f.pending[pkey(tenant, entity, record)], nil
}

func (f *fakeStore) CreateInstance(_ context.Context, in CreateInstanceInput) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, in)
	if in.Lane != LaneIndependent { // a tracker never occupies the approval slot (uq_ai_open keys it by process)
		f.pending[pkey(in.TenantID, in.EntityName, in.RecordID)] = true
	}
	id := "inst-" + in.RecordID
	f.instances[id] = &Instance{
		ID: id, TenantID: in.TenantID, ProcessID: in.ProcessID, EntityName: in.EntityName,
		RecordID: in.RecordID, CurrentStep: in.StartStep, Status: StatusPending, Seq: 1,
		CurrentApproverType: in.CurrentApproverType, CurrentApproverID: in.CurrentApproverID,
		SLADueAt: in.SLADueAt,
		// CreatedBy is what "only the submitter may recall" reads. The fake dropped it, so a
		// recall test failed here while the live API answered correctly — a fake that models the
		// row incompletely produces false red exactly as easily as false green.
		CreatedBy:  in.CreatedBy,
		LastError:  in.LastError,
		Definition: in.Definition, // the pin round-trips exactly like the real column
	}
	return id, nil
}

func (f *fakeStore) FindByIdempotencyKey(_ context.Context, tenant, key string) (*Instance, string, error) {
	for _, in := range f.created {
		if in.TenantID == tenant && in.IdempotencyKey == key {
			return f.instances["inst-"+in.RecordID], in.RequestHash, nil
		}
	}
	return nil, "", ErrInstanceNotFound
}

func (f *fakeStore) GetInstance(_ context.Context, tenant, id string) (*Instance, error) {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return nil, ErrInstanceNotFound
	}
	cp := *inst
	if f.onGet != nil {
		f.onGet(f, &cp)
	}
	return &cp, nil
}

// ListPendingByRecord mirrors the repository's wait-signal read: the pending instances of ONE
// record of ONE tenant.
func (f *fakeStore) ListPendingByRecord(_ context.Context, tenant, entity, record string) ([]Instance, error) {
	var out []Instance
	for _, inst := range f.instances {
		if inst.TenantID == tenant && inst.EntityName == entity && inst.RecordID == record && inst.Status == StatusPending {
			out = append(out, *inst)
		}
	}
	return out, nil
}

// ListOpenSeats answers with the seats a test planted (none = the instance has no seat, so the
// projection decides, exactly as for a pre-S3a instance).
func (f *fakeStore) ListOpenSeats(_ context.Context, tenant, id string) ([]Seat, error) {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return nil, nil
	}
	return f.seats[id], nil
}

// ResolveSeatUsers mirrors the repository: user seat → itself, role seat → roleUsers[role],
// deduplicated, capped.
func (f *fakeStore) ResolveSeatUsers(_ context.Context, _ string, seats []Seat, limit int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, st := range seats {
		ids := []string{}
		switch st.AssigneeType {
		case ApproverUser:
			ids = append(ids, st.AssigneeID)
		case ApproverRole:
			ids = append(ids, f.roleUsers[st.AssigneeID]...)
		case ApproverTeam:
			ids = append(ids, f.teamUsers[st.AssigneeID]...)
		}
		for _, id := range ids {
			if id != "" && !seen[id] && len(out) < limit {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// DecideTaskTx mirrors the real transaction's contract: not pending → ErrNotPending, the seq the
// caller read is not the seq now → ErrConcurrent, an actor with no seat (and no admin bit) →
// ErrNotAuthorized; otherwise the transition is applied and seq moves.
func (f *fakeStore) DecideTaskTx(_ context.Context, tenant, id string, g TaskGuard, next InstanceTransition, historyJSON []byte) error {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return ErrInstanceNotFound
	}
	if inst.Status != StatusPending {
		return ErrNotPending
	}
	if inst.Seq != g.ExpectSeq {
		return ErrConcurrent
	}
	if g.Actor != nil {
		if err := authorize(inst, f.seats[id], *g.Actor); err != nil {
			return err
		}
	}
	f.advanceCnt++
	f.lastNext = next
	f.transitions = append(f.transitions, next)
	inst.LastError = next.LastError
	if next.ContextJSON != nil {
		var c InstanceContext
		if err := json.Unmarshal(next.ContextJSON, &c); err == nil {
			inst.Context = c
		}
	}
	if len(historyJSON) > 0 {
		var h []HistoryEntry
		if err := json.Unmarshal(historyJSON, &h); err == nil {
			inst.History = h
		}
	}
	inst.Seq++
	inst.CurrentStep = next.CurrentStep
	inst.Status = next.Status
	inst.CurrentApproverType = next.CurrentApproverType
	inst.CurrentApproverID = next.CurrentApproverID
	inst.SLADueAt = next.SLADueAt
	inst.Escalated = next.Escalated
	if next.Clock != nil { // S13: the frozen-clock state, as commitTransition writes it
		inst.SLAPausedAt, inst.SLARemainingSeconds = next.Clock.PausedAt, next.Clock.RemainingSeconds
	} else if next.SLADueAt != nil {
		inst.SLAPausedAt, inst.SLARemainingSeconds = nil, nil
	}
	if len(next.Seats.Open) > 0 { // the seats the transition opens replace the ones it closed (S8)
		f.seats[id] = next.Seats.Open
	}
	if next.Status != StatusPending {
		f.pending[pkey(inst.TenantID, inst.EntityName, inst.RecordID)] = false
	}
	return nil
}

// AppendHistory mirrors the repository: a line on the history of an instance in any state, no seq bump.
func (f *fakeStore) AppendHistory(_ context.Context, tenant, id string, e HistoryEntry) error {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return ErrInstanceNotFound
	}
	inst.History = append(inst.History, e)
	return nil
}

func (f *fakeStore) GetProcess(_ context.Context, tenant, id string) (*ProcessRecord, error) {
	p, ok := f.processes[id]
	if !ok || p.TenantID != tenant {
		return nil, ErrProcessNotFound
	}
	return p, nil
}

func (f *fakeStore) InsertNotification(_ context.Context, n Notification) (string, error) {
	f.notes = append(f.notes, n)
	return "note-" + n.UserID, nil
}

func (f *fakeStore) ListBreachTenants(_ context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, inst := range f.instances {
		if !seen[inst.TenantID] {
			seen[inst.TenantID] = true
			out = append(out, inst.TenantID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) ListBreachedForTenant(_ context.Context, tenant string, now time.Time) ([]Instance, error) {
	f.scanned = append(f.scanned, tenant)
	var out []Instance
	for _, inst := range f.instances {
		if inst.TenantID == tenant && inst.Status == StatusPending && !inst.Escalated && inst.SLADueAt != nil && !inst.SLADueAt.After(now) {
			out = append(out, *inst)
		}
	}
	return out, nil
}

func (f *fakeStore) ClaimBreach(_ context.Context, tenant, id string, expectSeq int64, _ []byte) (bool, error) {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant || inst.Status != StatusPending || inst.Seq != expectSeq || inst.Escalated {
		return false, nil
	}
	if f.breachClaim[id] {
		return false, nil
	}
	f.breachClaim[id] = true
	f.claimedSeq[id] = expectSeq
	inst.Escalated = true
	inst.Seq++
	return true, nil
}

// fakeLoader serves a fixed process list for the arming engine.
type fakeLoader struct{ procs []Process }

func (f fakeLoader) ListActiveProcesses(_ context.Context, _, _ string, _ Trigger) ([]Process, error) {
	return f.procs, nil
}

// fakeRecorder captures audit events.
type fakeRecorder struct{ events []ApprovalAuditEvent }

func (f *fakeRecorder) RecordApprovalDecision(_ context.Context, ev ApprovalAuditEvent) {
	f.events = append(f.events, ev)
}

// procWithSteps builds a ProcessRecord with the given steps marshalled into the Steps JSON.
func procWithSteps(t *testing.T, store *fakeStore, id, tenant string, steps []Step) {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	store.processes[id] = &ProcessRecord{ID: id, TenantID: tenant, Steps: b}
}

// --- arming tests ------------------------------------------------------------------------------

func TestArm_MatchCreatesInstanceOnFirstStep(t *testing.T) {
	store := newFakeStore()
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate,
		Criteria: &workflow.Condition{Rules: []workflow.Rule{{Field: "amount", Op: "gt", Value: float64(1000)}}},
		Steps:    []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr", SLASeconds: 3600}},
		IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)

	e.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate,
		Data: map[string]any{"amount": float64(5000)}})

	if len(store.created) != 1 {
		t.Fatalf("want 1 instance armed, got %d", len(store.created))
	}
	got := store.created[0]
	if got.CurrentApproverID != "u-mgr" || got.CurrentApproverType != ApproverUser {
		t.Fatalf("first step approver = %s/%s; want user/u-mgr", got.CurrentApproverType, got.CurrentApproverID)
	}
	if got.SLADueAt == nil {
		t.Fatal("first step has SLASeconds>0 but sla_due_at is nil")
	}
	if len(store.notes) != 1 || store.notes[0].UserID != "u-mgr" {
		t.Fatalf("want a notice to u-mgr, got %+v", store.notes)
	}
}

func TestArm_CriteriaMissSkips(t *testing.T) {
	store := newFakeStore()
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate,
		Criteria: &workflow.Condition{Rules: []workflow.Rule{{Field: "amount", Op: "gt", Value: float64(1000)}}},
		Steps:    []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}},
		IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate,
		Data: map[string]any{"amount": float64(500)}}) // below threshold
	if len(store.created) != 0 {
		t.Fatalf("criteria miss should not arm; got %d", len(store.created))
	}
}

func TestArm_AlreadyPendingNotDoubleArmed(t *testing.T) {
	store := newFakeStore()
	store.pending[pkey("t1", "deals", "r1")] = true // already under approval
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate,
		Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}, IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{}})
	if len(store.created) != 0 {
		t.Fatalf("already-pending record should not be re-armed; got %d", len(store.created))
	}
}

// --- specs/043 S2b: start on a field that JUST changed to a value -------------------------------

// changedToProcess is an after_update process that starts when status BECOMES "Chờ duyệt".
func changedToProcess() Process {
	return Process{
		ID: "p-edge", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterUpdate,
		Criteria: &workflow.Condition{Rules: []workflow.Rule{{Field: "status", Op: workflow.OpChangedTo, Value: "Chờ duyệt"}}},
		Steps:    []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}},
		IsActive: true,
	}
}

// TestArm_ChangedTo_FiresOnEdgeOnly (T040): an edit that leaves the status alone does not start
// the process; the save that moves it INTO the value does — exactly once.
func TestArm_ChangedTo_FiresOnEdgeOnly(t *testing.T) {
	store := newFakeStore()
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{changedToProcess()}}, store, store, nil)

	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterUpdate,
		Before: map[string]any{"status": "Nháp", "note": "a"},
		Data:   map[string]any{"status": "Nháp", "note": "b"}})
	if len(store.created) != 0 {
		t.Fatalf("editing the note armed %d instance(s); want 0 — status did not change", len(store.created))
	}

	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterUpdate,
		Before: map[string]any{"status": "Nháp", "note": "b"},
		Data:   map[string]any{"status": "Chờ duyệt", "note": "b"}})
	if len(store.created) != 1 || store.created[0].RecordID != "q1" {
		t.Fatalf("status Nháp→Chờ duyệt armed %d instance(s); want exactly 1", len(store.created))
	}
}

// TestArm_ChangedTo_NotOnLaterEdits (T040): once the approval is over, a later edit of a record
// that is STILL "Chờ duyệt" must not start a second approval. This is the whole point of the edge
// operator: the same criteria written with eq re-arms here (the control proves it).
func TestArm_ChangedTo_NotOnLaterEdits(t *testing.T) {
	later := Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterUpdate,
		Before: map[string]any{"status": "Chờ duyệt", "note": "b"},
		Data:   map[string]any{"status": "Chờ duyệt", "note": "c"}}

	store := newFakeStore() // nothing pending: the first approval already ended
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{changedToProcess()}}, store, store, nil)
	e.arm(later)
	if len(store.created) != 0 {
		t.Fatalf("a later edit re-armed %d instance(s); want 0", len(store.created))
	}

	eq := changedToProcess()
	eq.Criteria = &workflow.Condition{Rules: []workflow.Rule{{Field: "status", Op: workflow.OpEq, Value: "Chờ duyệt"}}}
	ctl := newFakeStore()
	NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{eq}}, ctl, ctl, nil).arm(later)
	if len(ctl.created) != 1 {
		t.Fatalf("control: eq criteria armed %d; want 1 (the re-fire changedTo exists to prevent)", len(ctl.created))
	}
}

// TestArm_DepthCapStopsLoop (T042): an event whose automation chain already reached the cap
// shared with the workflow dispatcher (workflow.ChainDepthFromContext → Event.Depth) never arms;
// one below the cap still does. The zero config falls back to workflow.DefaultMaxChainDepth, never
// to "no cap" and never to "arm nothing".
func TestArm_DepthCapStopsLoop(t *testing.T) {
	proc := Process{ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate,
		Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}, IsActive: true}
	cases := []struct {
		name     string
		maxDepth int
		depth    int
		wantArm  bool
	}{
		{"person's edit (depth 0)", 3, 0, true},
		{"one hop below the cap", 3, 2, true},
		{"at the cap", 3, 3, false},
		{"past the cap", 3, 7, false},
		{"unset cap = default, below", 0, workflow.DefaultMaxChainDepth - 1, true},
		{"unset cap = default, at", 0, workflow.DefaultMaxChainDepth, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1, MaxChainDepth: tc.maxDepth}, fakeLoader{procs: []Process{proc}}, store, store, nil)
			e.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate,
				Data: map[string]any{}, Depth: tc.depth})
			if got := len(store.created) == 1; got != tc.wantArm {
				t.Fatalf("depth %d, cap %d: armed=%v, want %v", tc.depth, tc.maxDepth, got, tc.wantArm)
			}
		})
	}
}

// --- decision tests ----------------------------------------------------------------------------

func TestDecide_TwoLevelSequentialApproval(t *testing.T) {
	store := newFakeStore()
	steps := []Step{
		{ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{ApproverType: ApproverRole, ApproverID: "role-director", SLASeconds: 7200},
	}
	procWithSteps(t, store, "p1", "t1", steps)
	// arm an instance at step 0.
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	rec := &fakeRecorder{}
	d := NewDecider(store, store, store, rec, nil)

	// Step 0: the manager approves → advances to step 1 (director role, with SLA).
	inst, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "ok", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("step0 approve: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 {
		t.Fatalf("after step0 approve want pending@step1, got %s@%d", inst.Status, inst.CurrentStep)
	}
	if inst.CurrentApproverType != ApproverRole || inst.CurrentApproverID != "role-director" {
		t.Fatalf("step1 approver = %s/%s; want role/role-director", inst.CurrentApproverType, inst.CurrentApproverID)
	}
	if inst.SLADueAt == nil {
		t.Fatal("step1 has SLASeconds>0 but sla_due_at nil")
	}

	// Step 1: a user holding role-director approves → fully approved (terminal).
	inst, err = d.Decide(context.Background(), "t1", id, DecisionApprove, "final", Actor{UserID: "u-dir", RoleIDs: []string{"role-director"}})
	if err != nil {
		t.Fatalf("step1 approve: %v", err)
	}
	if inst.Status != StatusApproved {
		t.Fatalf("after final approve want approved, got %s", inst.Status)
	}
	if len(rec.events) != 2 {
		t.Fatalf("want 2 audit events, got %d", len(rec.events))
	}
}

func TestDecide_RejectIsTerminal(t *testing.T) {
	store := newFakeStore()
	steps := []Step{
		{ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{ApproverType: ApproverUser, ApproverID: "u-dir"},
	}
	procWithSteps(t, store, "p1", "t1", steps)
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	d := NewDecider(store, store, store, &fakeRecorder{}, nil)
	inst, err := d.Decide(context.Background(), "t1", id, DecisionReject, "no", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if inst.Status != StatusRejected {
		t.Fatalf("want rejected, got %s", inst.Status)
	}
	// A further decision on a terminal instance must be refused.
	if _, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u-mgr"}); err != ErrNotPending {
		t.Fatalf("decision on terminal want ErrNotPending, got %v", err)
	}
}

func TestDecide_NotApproverRefused(t *testing.T) {
	store := newFakeStore()
	procWithSteps(t, store, "p1", "t1", []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}})
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	d := NewDecider(store, store, store, &fakeRecorder{}, nil)
	if _, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u-other"}); err != ErrNotAuthorized {
		t.Fatalf("non-approver want ErrNotAuthorized, got %v", err)
	}
	// An admin bypasses the approver check.
	if _, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u-admin", IsAdmin: true}); err != nil {
		t.Fatalf("admin approve should succeed, got %v", err)
	}
}

func TestDecide_CrossTenantInstanceNotFound(t *testing.T) {
	store := newFakeStore()
	procWithSteps(t, store, "p1", "t1", []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}})
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	d := NewDecider(store, store, store, &fakeRecorder{}, nil)
	// tenant t2 must not see t1's instance.
	if _, err := d.Decide(context.Background(), "t2", id, DecisionApprove, "", Actor{UserID: "u-mgr"}); err != ErrInstanceNotFound {
		t.Fatalf("cross-tenant want ErrInstanceNotFound, got %v", err)
	}
}

// --- DAILY-QUEUE #8: an instance finishes the process it STARTED under ------------------------
//
// The decision path used to index the LIVE steps array by the instance's own current_step, so an
// admin shortening a 3-step process to 2 while a record waited at step 2 ended it 'approved' —
// one step skipped, the outcome written, no warning. These tests pin the definition on the
// instance at arm time and prove the pin is what the Decider walks.

// pinnedInstance seeds a pending instance at step 0 that carries its own pinned definition.
func pinnedInstance(store *fakeStore, id, tenant, procID string, def *Definition) {
	store.instances[id] = &Instance{
		ID: id, TenantID: tenant, ProcessID: procID, EntityName: "crm_quotes", RecordID: "q1",
		CurrentStep: 0, Status: StatusPending,
		CurrentApproverType: def.Steps[0].ApproverType, CurrentApproverID: def.Steps[0].ApproverID,
		Definition: def,
	}
	store.pending[pkey(tenant, "crm_quotes", "q1")] = true
}

func TestDecide_PinnedDefinitionSurvivesProcessEdit(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	three := []Step{
		{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir"},
		{Name: "CFO", ApproverType: ApproverUser, ApproverID: "u-cfo"},
	}
	procWithSteps(t, store, "p1", "t1", three)
	pinnedInstance(store, "i1", "t1", "p1", &Definition{
		Steps:   three,
		Outcome: &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"},
	})
	w := &spyWriter{}
	d := NewDecider(store, store, store, &fakeRecorder{}, nil).WithRecordWriter(w)

	// Step 0 approved → waiting on step 1.
	inst, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("step0 approve: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 {
		t.Fatalf("after step0 want pending@1, got %s@%d", inst.Status, inst.CurrentStep)
	}

	// An admin now shortens the LIVE process to two steps and points its outcome elsewhere —
	// the exact edit that used to end the instance 'approved' at the next decision.
	procWithSteps(t, store, "p1", "t1", three[:2])
	store.processes["p1"].Outcome = json.RawMessage(`{"statusField":"stage","approvedValue":"Won"}`)

	// Step 1 approved → the instance must still be waiting on step 2 (the CFO), not 'approved'.
	inst, err = d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-dir"})
	if err != nil {
		t.Fatalf("step1 approve: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 2 || inst.CurrentApproverID != "u-cfo" {
		t.Fatalf("live process shortened to 2 steps mid-flight: got %s@%d approver=%q; want pending@2/u-cfo — a pinned step was skipped",
			inst.Status, inst.CurrentStep, inst.CurrentApproverID)
	}
	if len(w.calls) != 0 {
		t.Fatalf("outcome written before the last PINNED step cleared: %v", w.calls)
	}

	// Step 2 approved → terminal, and the PINNED outcome is what gets written, not the live one.
	inst, err = d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-cfo"})
	if err != nil {
		t.Fatalf("step2 approve: %v", err)
	}
	if inst.Status != StatusApproved {
		t.Fatalf("after the last pinned step want approved, got %s", inst.Status)
	}
	if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=Chấp nhận" {
		t.Fatalf("writes = %v, want the PINNED outcome (status=Chấp nhận), not the live one (stage=Won)", w.calls)
	}
}

// TestDecide_NoSnapshotReadsLiveProcess: an instance armed before migration 000065 carries no
// snapshot — the Decider must read the live process exactly as before, so every in-flight
// approval from before the column existed keeps working.
// TestDecide_EmptyPinnedDefinitionRefused: CreateInstance refuses a 0-step definition, so this
// state is unreachable through our own writes — but `next >= len(steps)` means a 0-step pin would
// make the very next approve TERMINAL-approve the instance and fire the outcome write, i.e. exactly
// the skipped-step bug 000065 closes. definitionOf must refuse the decision instead of falling back
// to the live process (which is the bug) or walking an empty chain.
func TestDecide_EmptyPinnedDefinitionRefused(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	procWithSteps(t, store, "p1", "t1", []Step{
		{ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{ApproverType: ApproverUser, ApproverID: "u-dir"},
	})
	id, _ := store.CreateInstance(ctx, CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
		Definition: &Definition{Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}},
	})
	// Simulate a row that came back from the DB with a 0-step snapshot (hand-written SQL, a bad
	// backfill) — the fake store hands out the same pointer, so blank the steps in place.
	store.instances[id].Definition = &Definition{Steps: nil}
	w := &spyWriter{}
	d := NewDecider(store, store, store, nil, nil).WithRecordWriter(w)

	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err == nil {
		t.Fatalf("Decide accepted a 0-step pinned definition: got %+v; want a refusal", inst)
	}
	if got := store.instances[id].Status; got != StatusPending {
		t.Errorf("instance status = %s; want it left pending — the refusal must change no state", got)
	}
	if len(w.calls) != 0 {
		t.Errorf("outcome writes = %v; want none — a refused decision must not touch the record", w.calls)
	}
}

func TestDecide_NoSnapshotReadsLiveProcess(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	procWithSteps(t, store, "p1", "t1", []Step{
		{ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{ApproverType: ApproverUser, ApproverID: "u-dir"},
	})
	store.processes["p1"].Outcome = json.RawMessage(`{"statusField":"status","approvedValue":"Chấp nhận"}`)
	id, _ := store.CreateInstance(ctx, CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	if store.instances[id].Definition != nil {
		t.Fatal("the fake pinned a definition nobody passed")
	}
	w := &spyWriter{}
	d := NewDecider(store, store, store, nil, nil).WithRecordWriter(w)

	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("step0 approve: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("live fallback: got %s@%d approver=%q; want pending@1/u-dir", inst.Status, inst.CurrentStep, inst.CurrentApproverID)
	}
	inst, err = d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u-dir"})
	if err != nil {
		t.Fatalf("step1 approve: %v", err)
	}
	if inst.Status != StatusApproved {
		t.Fatalf("want approved, got %s", inst.Status)
	}
	if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=Chấp nhận" {
		t.Fatalf("writes = %v, want the LIVE outcome for a pre-snapshot instance", w.calls)
	}
}

// TestArm_PinsDefinition: the engine's arming path copies steps + outcome onto the instance.
func TestArm_PinsDefinition(t *testing.T) {
	store := newFakeStore()
	steps := []Step{
		{ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{ApproverType: ApproverRole, ApproverID: "role-director", SLASeconds: 7200},
	}
	out := &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"}
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterCreate,
		Steps: steps, Outcome: out, IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate, Data: map[string]any{}})

	if len(store.created) != 1 {
		t.Fatalf("want 1 instance armed, got %d", len(store.created))
	}
	def := store.created[0].Definition
	if def == nil || len(def.Steps) != 2 || def.Steps[1].ApproverID != "role-director" {
		t.Fatalf("armed instance did not pin the process steps: %+v", def)
	}
	if def.Outcome == nil || def.Outcome.StatusField != "status" || def.Outcome.RejectedValue != "Từ chối" {
		t.Fatalf("armed instance did not pin the process outcome: %+v", def.Outcome)
	}
}

// --- SLA breach test ---------------------------------------------------------------------------

func TestSLAScheduler_BreachEscalatesOnce(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Hour)
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr", SLADueAt: &past,
	})
	rec := &fakeRecorder{}
	s := NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, rec, nil)

	s.runBreaches(context.Background(), time.Now().UTC())
	if !store.instances[id].Escalated {
		t.Fatal("breached instance should be escalated")
	}
	if len(store.notes) != 1 || store.notes[0].UserID != "u-mgr" {
		t.Fatalf("want a breach notice to u-mgr, got %+v", store.notes)
	}
	if len(rec.events) != 1 || rec.events[0].Action != HistoryEscalated {
		t.Fatalf("want 1 escalation audit, got %+v", rec.events)
	}

	// A second pass must NOT re-escalate (at-most-once per step).
	store.notes = nil
	rec.events = nil
	s.runBreaches(context.Background(), time.Now().UTC())
	if len(store.notes) != 0 || len(rec.events) != 0 {
		t.Fatalf("second pass must not re-escalate; notes=%d events=%d", len(store.notes), len(rec.events))
	}
}

// MOT-XUONG (2026-07-29): sys_notifications.type was added on purpose by migration 000026 — its
// header says the FE inbox needs it "to render different icons per category" — and the column
// exists in the live DB. But neither INSERT path ever listed it, so every row was NULL and not one
// of NotificationBell's five branches ever matched. Migration landed, FE landed, the join in the
// middle was never written. Pin the type at both bpm sources so it cannot silently go NULL again.
func TestNotification_TypeIsSet(t *testing.T) {
	f := &fakeStore{}
	e := &AsyncEngine{inbox: f, logger: zap.NewNop()}
	e.notifyApprover(context.Background(), "t1", "crm_quotes", "r1", Step{
		ApproverType: ApproverUser, ApproverID: "u1",
	})
	if len(f.notes) != 1 {
		t.Fatalf("want 1 notification, got %d", len(f.notes))
	}
	if f.notes[0].Type != "approval" {
		t.Fatalf("approver notice Type = %q; want %q (NULL type = dead FE icon branches)",
			f.notes[0].Type, "approval")
	}
}
