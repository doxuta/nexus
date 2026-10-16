package bpm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// specs/043 S3a2 (T059, D-2): a definition may be stored in GRAPH form — {nodes:[…]} with
// steps '[]' — and the engine must run it exactly like the linear form it is equivalent to today
// (every node an approval node with one approver, in order). Anything a node carries that this
// binary does not run fails CLOSED: never armed, never decided.

func twoNodeGraph() *Graph {
	return &Graph{Nodes: []Node{
		{Step: Step{ID: "truong-phong", Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr", SLASeconds: 3600}, Kind: NodeApproval},
		{Step: Step{ID: "giam-doc", Name: "Giám đốc", ApproverType: ApproverRole, ApproverID: "r-dir", SLAClock: SLAClockWall}, Kind: NodeApproval},
	}}
}

func TestGraphProcess_ArmsAndDecidesLikeSteps(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	out := &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}
	loader := fakeLoader{procs: []Process{{
		ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterCreate,
		Graph: twoNodeGraph(), Outcome: out, IsActive: true,
	}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate, Data: map[string]any{}})

	if len(store.created) != 1 {
		t.Fatalf("a graph process armed %d instance(s); want 1", len(store.created))
	}
	in := store.created[0]
	if in.CurrentApproverType != ApproverUser || in.CurrentApproverID != "u-mgr" || in.SLADueAt == nil {
		t.Fatalf("first node: approver %s/%s due %v; want user/u-mgr with a deadline", in.CurrentApproverType, in.CurrentApproverID, in.SLADueAt)
	}
	if len(in.Seats) != 1 || in.Seats[0].NodeID != "truong-phong" {
		t.Fatalf("first seats %+v; want one on node truong-phong", in.Seats)
	}
	// The pin carries the graph, with every clock explicit, and an EMPTY steps array: a binary
	// older than S3a2 reads that as "no steps" and refuses to decide (D-2 fail-closed) instead of
	// walking a chain it does not understand.
	pin, err := json.Marshal(in.Definition)
	if err != nil {
		t.Fatalf("marshal pin: %v", err)
	}
	if !strings.Contains(string(pin), `"steps":[]`) || in.Definition.Graph == nil || len(in.Definition.Graph.Nodes) != 2 {
		t.Fatalf("pin %s; want steps [] and the two-node graph", pin)
	}
	if c := in.Definition.Graph.Nodes[0].SLAClock; c != SLAClockBusiness {
		t.Fatalf("pinned node clock %q; want business written explicitly (D-9)", c)
	}
	if c := in.Definition.Graph.Nodes[1].SLAClock; c != SLAClockWall {
		t.Fatalf("pinned node clock %q; want the wall clock the node chose", c)
	}

	d := NewDecider(store, store, store, nil, nil)
	inst, err := d.Decide(ctx, "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("approve node 1: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverType != ApproverRole || inst.CurrentApproverID != "r-dir" {
		t.Fatalf("after node 1: %s@%d %s/%s; want pending@1 role/r-dir", inst.Status, inst.CurrentStep, inst.CurrentApproverType, inst.CurrentApproverID)
	}
	if h := inst.History[len(inst.History)-1]; h.Node != "truong-phong" || h.StepName != "Trưởng phòng" {
		t.Fatalf("history names node %q %q; want truong-phong / Trưởng phòng", h.Node, h.StepName)
	}
	inst, err = d.Decide(ctx, "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-9", RoleIDs: []string{"r-dir"}})
	if err != nil {
		t.Fatalf("approve node 2: %v", err)
	}
	if inst.Status != StatusApproved {
		t.Fatalf("after the last node: %s; want approved", inst.Status)
	}
}

func TestGraphProcess_SubmitPinsTheGraph(t *testing.T) {
	store := newFakeStore()
	loader := fakeLoader{procs: []Process{{ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerManual, Graph: twoNodeGraph(), IsActive: true}}}
	r := NewRequester(loader, store, store, store, nil)
	inst, err := r.Submit(context.Background(), "t1", "crm_quotes", "q1", map[string]any{}, Actor{UserID: "u-sub"})
	if err != nil {
		t.Fatalf("Submit on a graph process: %v", err)
	}
	if inst.CurrentApproverID != "u-mgr" || inst.Definition == nil || inst.Definition.Graph == nil {
		t.Fatalf("submitted instance %+v; want node 1's approver and the pinned graph", inst)
	}
	if node, name := inst.Definition.StepRef(0); node != "truong-phong" || name != "Trưởng phòng" {
		t.Fatalf("StepRef(0) = %q %q; want the graph's first node", node, name)
	}
}

// TestDefinitionOf_LiveGraphBeforeSteps: an instance with no pin reads the live process — graph
// first, then steps (T059: graph > steps > live).
func TestDefinitionOf_LiveGraphBeforeSteps(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	g, err := json.Marshal(twoNodeGraph())
	if err != nil {
		t.Fatal(err)
	}
	store.processes["p1"] = &ProcessRecord{ID: "p1", TenantID: "t1", Steps: json.RawMessage(`[]`), Graph: g}
	id, _ := store.CreateInstance(ctx, CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr",
	})
	inst, err := NewDecider(store, store, store, nil, nil).Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("decide on the live graph: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentApproverID != "r-dir" {
		t.Fatalf("live graph walk: %s approver %q; want pending on r-dir", inst.Status, inst.CurrentApproverID)
	}
}

// TestGraphProcess_UnrunnableNodeFailsClosed: a graph this binary does not run is never armed and
// never decided — a later binary's graph read by this one (rollback: THIS binary is the first that
// reads `graph`, so it is where S4–S12 roll back to), a hand edit, a pack from the future. That
// covers a node kind of a later slice, a node without an approver, AND a later slice's key on an
// otherwise plain approval node or on the graph: encoding/json drops unknown keys silently, so a
// node with `completion: all` would otherwise walk as ONE approver, `branches` as a straight walk.
// Every path reads the same decode: the process row, the pinned snapshot, the live process.
func TestGraphProcess_UnrunnableNodeFailsClosed(t *testing.T) {
	ctx := context.Background()
	const mgr = `{"id":"truong-phong","kind":"approval","approverType":"user","approverId":"u-mgr"}`
	for name, raw := range map[string]string{
		"a gateway node":                     `{"nodes":[` + mgr + `,{"id":"re-nhanh","kind":"gateway"}]}`,
		"an approval without owner":          `{"nodes":[` + mgr + `,{"id":"hoi-dong","kind":"approval"}]}`,
		"approvers + completion (S4)":        `{"nodes":[{"id":"truong-phong","kind":"approval","approverType":"user","approverId":"u-mgr","approvers":[{"type":"user","id":"u-2"}],"completion":{"mode":"all"}}]}`,
		"assignment (S6a)":                   `{"nodes":[{"id":"truong-phong","kind":"approval","approverType":"user","approverId":"u-mgr","assignment":{"type":"field","field":"owner_id"}}]}`,
		"an sla object (S12a)":               `{"nodes":[{"id":"truong-phong","kind":"approval","approverType":"user","approverId":"u-mgr","sla":{"seconds":3600}}]}`,
		"a graph-level key of a later slice": `{"lanes":["a"],"nodes":[` + mgr + `]}`,
		"a later key, any letter case":       `{"nodes":[{"id":"truong-phong","kind":"approval","approverType":"user","approverId":"u-mgr","Assignment":{}}]}`,
		"a graph key beside nodes (S7)":      `{"Nodes":[` + mgr + `],"branches":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var g Graph
			if err := json.Unmarshal([]byte(raw), &g); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if c := g.approvalChain(); c != nil {
				t.Fatalf("chain of %s = %+v; want nil (this binary cannot run it)", raw, c)
			}

			// The process row and the pinned snapshot keep the refusal through normalization.
			graphCol := json.RawMessage(raw)
			p, err := rowToProcess(&processRow{ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: string(TriggerAfterCreate),
				Steps: json.RawMessage(`[]`), Graph: &graphCol, IsActive: true})
			if err != nil {
				t.Fatalf("rowToProcess: %v", err)
			}
			if c := p.Chain(); c != nil {
				t.Fatalf("process row chain = %+v; want nil", c)
			}
			snap := json.RawMessage(`{"steps":[],"graph":` + raw + `}`)
			in, err := rowToInstance(&instanceRow{ID: "i1", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q2",
				Status: string(StatusPending), History: json.RawMessage(`[]`), ProcessSnapshot: &snap})
			if err != nil {
				t.Fatalf("rowToInstance: %v", err)
			}
			if c := in.Definition.Chain(); c != nil {
				t.Fatalf("pinned snapshot chain = %+v; want nil", c)
			}

			store := newFakeStore()
			loader := fakeLoader{procs: []Process{p}}
			NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, loader, store, store, nil).
				arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate, Data: map[string]any{}})
			if len(store.created) != 0 {
				t.Fatalf("armed %d instance(s) on a graph this binary cannot run", len(store.created))
			}

			d := NewDecider(store, store, store, nil, nil)
			// Pinned on that graph.
			in.CurrentApproverType, in.CurrentApproverID = ApproverUser, "u-mgr"
			store.instances["i1"] = &in
			if _, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-mgr"}); err == nil {
				t.Fatalf("decided an instance pinned on a graph this binary cannot run")
			}
			if st := store.instances["i1"]; st.Status != StatusPending || st.CurrentStep != 0 {
				t.Fatalf("refused decision changed state: %s@%d", st.Status, st.CurrentStep)
			}
			// Not pinned: the live process is that graph.
			store.processes["p1"] = &ProcessRecord{ID: "p1", TenantID: "t1", Steps: json.RawMessage(`[]`), Graph: graphCol}
			store.instances["i2"] = &Instance{ID: "i2", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q3",
				Status: StatusPending, CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr"}
			if _, err := d.Decide(ctx, "t1", "i2", DecisionApprove, "", Actor{UserID: "u-mgr"}); err == nil {
				t.Fatalf("decided an unpinned instance whose live process is a graph this binary cannot run")
			}
			if st := store.instances["i2"]; st.Status != StatusPending || st.CurrentStep != 0 {
				t.Fatalf("refused decision changed state: %s@%d", st.Status, st.CurrentStep)
			}
		})
	}
}

// TestGraph_KnownKeysStillRun: the refusal above is about keys OUTSIDE the closed set — a node
// with every key this binary binds (any letter case, as encoding/json binds) still runs.
func TestGraph_KnownKeysStillRun(t *testing.T) {
	raw := `{"NODES":[{"ID":"a","Name":"A","kind":"approval","approverType":"user","approverId":"u","slaSeconds":60,"slaClock":"wall"}]}`
	var g Graph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c := g.normalized().approvalChain(); len(c) != 1 || c[0].ID != "a" || c[0].SLAClock != SLAClockWall {
		t.Fatalf("chain = %+v; want the one node", c)
	}
}
