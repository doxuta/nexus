package bpm

// S0 bpm-node-ids (specs/043-bpm-process-engine tasks T004/T009/T012): every step carries a
// stable id. A legacy step without one gets its POSITIONAL id (s1, s2, …) — the same rule
// Migration A's backfill uses (COALESCE(steps[i].id, CONCAT('s', i+1))) — and an id that exists
// is never rewritten, so reordering / inserting steps cannot make a pinned instance point at a
// different step. Every new history entry names the node it happened on.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func stepIDs(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}

func sameIDs(t *testing.T, got []Step, want ...string) {
	t.Helper()
	ids := stepIDs(got)
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestNormalizeSteps_LegacyGetsPositionalIDs(t *testing.T) {
	legacy := []Step{
		{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u1"},
		{Name: "Giám đốc", ApproverType: ApproverRole, ApproverID: "r1", SLASeconds: 60},
		{ApproverType: ApproverUser, ApproverID: "u2"},
	}
	got := NormalizeSteps(legacy)
	sameIDs(t, got, "s1", "s2", "s3")
	if got[1].Name != "Giám đốc" || got[1].SLASeconds != 60 || got[1].ApproverID != "r1" {
		t.Fatalf("normalize must keep every other key, got %+v", got[1])
	}
	// The input is a stored/pinned definition — normalizing must not mutate it in place.
	if legacy[0].ID != "" {
		t.Fatalf("NormalizeSteps mutated its input: %+v", legacy)
	}
	if NormalizeSteps(nil) != nil {
		t.Fatal("nil steps must stay nil")
	}
}

func TestNormalizeSteps_KeepsExistingIDsOnReorder(t *testing.T) {
	cases := []struct {
		name string
		in   []Step
		want []string
	}{
		{"reordered ids stay with their step", []Step{{ID: "s2"}, {ID: "s1"}}, []string{"s2", "s1"}},
		{"custom ids untouched", []Step{{ID: "ke-toan"}, {ID: "giam_doc"}}, []string{"ke-toan", "giam_doc"}},
		// A step inserted in FRONT of an id'd one: its positional candidate s1 is taken, so it
		// takes the next free number instead of colliding.
		{"insert before an id'd step never collides", []Step{{}, {ID: "s1"}}, []string{"s2", "s1"}},
		// Step 2 deleted, a new step appended: s3 is still held by the survivor.
		{"append after a delete skips a held id", []Step{{ID: "s1"}, {ID: "s3"}, {}}, []string{"s1", "s3", "s4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sameIDs(t, NormalizeSteps(tc.in), tc.want...)
		})
	}
}

func TestValidNodeID(t *testing.T) {
	long := make([]byte, 61)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		id   string
		want bool
	}{
		{"s1", true}, {"ke-toan_2", true}, {string(long[:60]), true},
		{"", false}, {string(long), false}, {"Kế toán", false}, {"S1", false}, {"a b", false}, {"a/b", false},
	}
	for _, tc := range cases {
		if got := ValidNodeID(tc.id); got != tc.want {
			t.Errorf("ValidNodeID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestReadSide_LegacyRowsGetPositionalIDs: every decode point hands the engine and the admin API
// id'd steps, so a legacy row (never re-saved) still answers GET /admin/approval-processes/:id
// with s1, s2 — without any row being rewritten.
func TestReadSide_LegacyRowsGetPositionalIDs(t *testing.T) {
	legacy := json.RawMessage(`[{"approverType":"user","approverId":"u1"},{"approverType":"role","approverId":"r1","slaSeconds":60}]`)

	p, err := rowToProcess(&processRow{ID: "p1", Steps: legacy})
	if err != nil {
		t.Fatalf("rowToProcess: %v", err)
	}
	sameIDs(t, p.Steps, "s1", "s2")

	rec := processRowToRecord(&processRow{ID: "p1", Steps: legacy})
	var recSteps []Step
	if err := json.Unmarshal(rec.Steps, &recSteps); err != nil {
		t.Fatalf("admin view steps: %v", err)
	}
	sameIDs(t, recSteps, "s1", "s2")
	if recSteps[1].SLASeconds != 60 {
		t.Fatalf("admin view lost a key: %s", rec.Steps)
	}

	// Undecodable stored JSON is forwarded raw (the admin read must not 500 on one bad row).
	bad := json.RawMessage(`{"not":"an array"}`)
	if got := processRowToRecord(&processRow{ID: "p2", Steps: bad}); string(got.Steps) != string(bad) {
		t.Fatalf("bad JSON must be forwarded verbatim, got %s", got.Steps)
	}

	snap := json.RawMessage(`{"steps":[{"approverType":"user","approverId":"u1"},{"approverType":"user","approverId":"u2"}]}`)
	inst, err := rowToInstance(&instanceRow{ID: "i1", Status: "pending", ProcessSnapshot: &snap})
	if err != nil {
		t.Fatalf("rowToInstance: %v", err)
	}
	sameIDs(t, inst.Definition.Steps, "s1", "s2")
}

// claimSpy records the history JSON the SLA claim writes (the shared fake drops it).
type claimSpy struct {
	*fakeStore
	history []byte
}

func (c *claimSpy) ClaimBreach(ctx context.Context, tenant, id string, expectSeq int64, h []byte) (bool, error) {
	c.history = h
	return c.fakeStore.ClaimBreach(ctx, tenant, id, expectSeq, h)
}

func lastEntry(t *testing.T, inst *Instance) HistoryEntry {
	t.Helper()
	if inst == nil || len(inst.History) == 0 {
		t.Fatal("no history entry written")
	}
	return inst.History[len(inst.History)-1]
}

func wantNode(t *testing.T, what string, e HistoryEntry, node, name string) {
	t.Helper()
	if e.Node != node || e.StepName != name {
		t.Errorf("%s: history node/stepName = %q/%q, want %q/%q", what, e.Node, e.StepName, node, name)
	}
}

// TestHistory_CarriesNodeAndStepName (T012): submit / approve / delegate / recall / escalate each
// stamp the node id + step name of the step they acted on. A LEGACY pinned definition (no ids)
// gets the positional id — the same one GET returns and Migration A backfills.
func TestHistory_CarriesNodeAndStepName(t *testing.T) {
	ctx := context.Background()
	legacySteps := []Step{
		{Name: "Trưởng phòng duyệt", ApproverType: ApproverUser, ApproverID: "mgr"},
		{Name: "Giám đốc duyệt", ApproverType: ApproverUser, ApproverID: "dir"},
	}

	// Submit + approve (step 1 → step 2) + delegate + recall on a manual process.
	proc := Process{ID: "p1", TenantID: "t1", EntityName: "quotes", Trigger: TriggerManual, Steps: legacySteps}
	req, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {proc}})
	inst, err := req.Submit(ctx, "t1", "quotes", "q1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	wantNode(t, "submitted", lastEntry(t, inst), "s1", "Trưởng phòng duyệt")
	if def := store.created[0].Definition; def == nil || def.Steps[0].ID != "s1" || def.Steps[1].ID != "s2" {
		t.Fatalf("submit must PIN id'd steps, got %+v", def)
	}

	d := NewDecider(store, store, store, nil, nil)
	inst, err = d.Decide(ctx, "t1", inst.ID, DecisionApprove, "ok", Actor{UserID: "mgr"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	wantNode(t, "approved", lastEntry(t, inst), "s1", "Trưởng phòng duyệt")

	inst, err = req.Delegate(ctx, "t1", inst.ID, "dep", "đi công tác", Actor{UserID: "dir"})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	wantNode(t, "delegated", lastEntry(t, inst), "s2", "Giám đốc duyệt")

	inst, err = req.Recall(ctx, "t1", inst.ID, "gửi nhầm", Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	wantNode(t, "recalled", lastEntry(t, inst), "s2", "Giám đốc duyệt")

	// Arm (after_create) pins id'd steps too.
	arm := newFakeStore()
	eng := NewAsyncEngine(EngineConfig{}, fakeLoader{procs: []Process{{ID: "p2", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate, Steps: legacySteps}}}, arm, nil, nil)
	eng.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate})
	if err := eng.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(arm.created) != 1 || arm.created[0].Definition.Steps[1].ID != "s2" {
		t.Fatalf("arm must PIN id'd steps, got %+v", arm.created)
	}

	// Escalation: the SLA claim's history entry names the breached node.
	spy := &claimSpy{fakeStore: newFakeStore()}
	past := time.Now().UTC().Add(-time.Hour)
	pinnedInstance(spy.fakeStore, "i-sla", "t1", "p1", &Definition{Steps: legacySteps})
	spy.instances["i-sla"].SLADueAt = &past
	NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, spy, spy, nil, nil).runBreaches(ctx, time.Now().UTC())
	var h []HistoryEntry
	if err := json.Unmarshal(spy.history, &h); err != nil || len(h) != 1 {
		t.Fatalf("escalation history = %s (err %v)", spy.history, err)
	}
	wantNode(t, "escalated", h[0], "s1", "Trưởng phòng duyệt")

	// A pre-000065 instance (no pin) still decides; the node comes from the live process.
	live := newFakeStore()
	procWithSteps(t, live, "p3", "t1", legacySteps)
	live.instances["i-old"] = &Instance{ID: "i-old", TenantID: "t1", ProcessID: "p3", EntityName: "deals", RecordID: "r9",
		Status: StatusPending, CurrentApproverType: ApproverUser, CurrentApproverID: "mgr"}
	got, err := NewDecider(live, live, nil, nil, nil).Decide(ctx, "t1", "i-old", DecisionReject, "không", Actor{UserID: "mgr"})
	if err != nil {
		t.Fatalf("decide on unpinned: %v", err)
	}
	wantNode(t, "rejected (live fallback)", lastEntry(t, got), "s1", "Trưởng phòng duyệt")
}

// TestHistoryEntry_LegacyJSONUnchanged: an entry with no node serializes exactly as before —
// history rows written by older binaries and the FE timeline's index fallback keep working.
func TestHistoryEntry_LegacyJSONUnchanged(t *testing.T) {
	at := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	b, err := json.Marshal(HistoryEntry{Step: 1, Action: HistoryApproved, Actor: "u1", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"step":1,"action":"approved","actor":"u1","at":"2026-09-29T08:00:00Z"}`; string(b) != want {
		t.Fatalf("legacy entry JSON = %s, want %s", b, want)
	}
}
