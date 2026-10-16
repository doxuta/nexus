package bpm

import (
	"context"
	"encoding/json"
	"testing"
)

// specs/043 S10 — process versions, the unit half: what the engine pins and records, and the two
// pure helpers the repository leans on (id allocation across versions, "did the definition change").

// A step id means "the same step" across versions; one released by a deleted step must not come back
// as a DIFFERENT step (spec "Nợ còn mở" 15, tasks T165).
func TestNormalizeStepsAvoiding_NeverReissuesReservedID(t *testing.T) {
	step := func(id string) Step { return Step{ID: id, ApproverType: ApproverUser, ApproverID: "u"} }
	cases := []struct {
		name     string
		in       []Step
		reserved map[string]bool
		want     []string
	}{
		{"no history: positional, exactly NormalizeSteps", []Step{step(""), step("")}, nil, []string{"s1", "s2"}},
		{"s3 was published then deleted: the new step skips it", []Step{step("s1"), step("s2"), step("")}, map[string]bool{"s1": true, "s2": true, "s3": true}, []string{"s1", "s2", "s4"}},
		{"explicit id is never touched, even when reserved", []Step{step("s3")}, map[string]bool{"s3": true}, []string{"s3"}},
		{"id-less body over a history: fresh ids, not the old positional ones", []Step{step(""), step("")}, map[string]bool{"s1": true, "s2": true}, []string{"s3", "s4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeStepsAvoiding(tc.in, tc.reserved)
			for i, w := range tc.want {
				if got[i].ID != w {
					t.Fatalf("ids = %v; want %v", ids(got), tc.want)
				}
			}
		})
	}
	if got := NormalizeSteps([]Step{step(""), step("")}); got[0].ID != "s1" || got[1].ID != "s2" {
		t.Fatalf("NormalizeSteps changed: %v", ids(got))
	}
}

func ids(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}

// "Did the definition change" decides whether a legacy save publishes a new version: key order,
// spacing and the way MySQL re-serialises JSON must not count; a real edit must.
func TestSameDefinition(t *testing.T) {
	base := DefinitionDoc{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual",
		Criteria: json.RawMessage(`{"match":"and","rules":[{"field":"a","op":"eq","value":1}]}`),
		Steps:    json.RawMessage(`[{"id":"s1","approverType":"user","approverId":"u1"}]`),
	}
	reordered := base
	reordered.Criteria = json.RawMessage(`{ "rules": [ {"value":1,"op":"eq","field":"a"} ], "match": "and" }`)
	reordered.Steps = json.RawMessage(`[{"approverId":"u1","approverType":"user","id":"s1"}]`)
	renamed := base
	renamed.Name = "Q"
	moreSteps := base
	moreSteps.Steps = json.RawMessage(`[{"id":"s1","approverType":"user","approverId":"u1"},{"id":"s2","approverType":"user","approverId":"u2"}]`)
	withOutcome := base
	withOutcome.Outcome = json.RawMessage(`{"statusField":"status","approvedValue":"ok"}`)

	for _, tc := range []struct {
		name string
		b    DefinitionDoc
		same bool
	}{
		{"identical", base, true},
		{"same JSON, other key order and spacing", reordered, true},
		{"renamed", renamed, false},
		{"another step", moreSteps, false},
		{"outcome added", withOutcome, false},
	} {
		if got := sameDefinition(base, tc.b); got != tc.same {
			t.Errorf("%s: sameDefinition = %v; want %v", tc.name, got, tc.same)
		}
	}
}

// The version a process is on travels onto every instance armed from it (the pin), so "which
// version does this hồ sơ run" is answered by the hồ sơ, not by today's process row.
func TestArm_PinsProcessVersion(t *testing.T) {
	store := newFakeStore()
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate, IsActive: true, Version: 3,
		Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}},
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate, Data: map[string]any{}})
	if len(store.created) != 1 {
		t.Fatalf("armed %d instances; want 1", len(store.created))
	}
	if v := store.created[0].Definition.Version; v != 3 {
		t.Fatalf("pinned version = %d; want 3 (the process's active version)", v)
	}
}

// historyStore keeps the history the Decider wrote (fakeStore drops it).
type historyStore struct {
	*fakeStore
	history []byte
}

func (h *historyStore) DecideTaskTx(ctx context.Context, tenant, id string, g TaskGuard, next InstanceTransition, historyJSON []byte) error {
	h.history = historyJSON
	return h.fakeStore.DecideTaskTx(ctx, tenant, id, g, next, historyJSON)
}

// #70: the history entry of a decision names the version the hồ sơ runs under, and so does the
// audit event — read off the PIN, never off the live process (which may have moved on to v5).
func TestDecide_HistoryAndAuditCarryPinnedVersion(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	steps := []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}, {ApproverType: ApproverUser, ApproverID: "u-dir"}}
	procWithSteps(t, fs, "p1", "t1", steps)
	fs.processes["p1"].Version = 5
	pinnedInstance(fs, "i1", "t1", "p1", &Definition{Steps: steps, Version: 2})
	store := &historyStore{fakeStore: fs}
	rec := &fakeRecorder{}
	d := NewDecider(store, store, store, rec, nil)

	if _, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "ok", Actor{UserID: "u-mgr"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var hist []HistoryEntry
	if err := json.Unmarshal(store.history, &hist); err != nil || len(hist) != 1 {
		t.Fatalf("history %s (%v); want one entry", store.history, err)
	}
	if hist[0].Version != 2 {
		t.Errorf("history version = %d; want 2 (the pin), not the live 5", hist[0].Version)
	}
	if len(rec.events) != 1 || rec.events[0].Definition.Version != 2 {
		t.Fatalf("audit definition version = %+v; want 2", rec.events)
	}
}

// A hồ sơ armed before versions existed (NULL pin) is pinned to nothing: its entries carry no
// version rather than a guessed one.
func TestDecide_PreVersionPinHasNoVersion(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	steps := []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}
	procWithSteps(t, fs, "p1", "t1", steps)
	pinnedInstance(fs, "i1", "t1", "p1", &Definition{Steps: steps})
	store := &historyStore{fakeStore: fs}
	d := NewDecider(store, store, store, nil, nil)
	if _, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var hist []HistoryEntry
	if err := json.Unmarshal(store.history, &hist); err != nil || len(hist) != 1 || hist[0].Version != 0 {
		t.Fatalf("history %s (%v); want one entry without a version", store.history, err)
	}
}
