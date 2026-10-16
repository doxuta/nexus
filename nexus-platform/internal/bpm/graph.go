package bpm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"nexus/platform/internal/workflow"
)

// Graph form of an approval definition (specs/043 D-2, lát S3a2).
//
// A process is stored in ONE of two forms: the linear v1 form in `steps` (what every process has
// been since M1), or the graph form in `graph` with `steps = '[]'`. The empty steps array is the
// point: a binary older than S3a2 reading a graph process finds no steps and skips it (arm: WARN;
// decide on a graph-pinned instance: refused) — fail-closed, instead of walking nodes it does not
// understand as if they were plain steps.
//
// D-7: the validator opens a node kind only in the slice whose engine runs it. At S3a2 that is
// the approval node with one approver — exactly what a linear step is — so today a graph is
// walked as the chain of its nodes, in order. Later slices add kinds (task S5, gateway/auto/end
// S7, wait S12a) and keys (approvers+completion — open since S4, assignment S6a, start/next/branches
// S7, onReject S8, an sla object S12a); a node — or a graph — carrying any still-closed one is not one THIS
// binary can run, and approvalChain refuses the whole graph. encoding/json drops a key the struct
// does not bind, so Node and Graph record those keys as they decode (UnmarshalJSON, against the
// closed sets below — the SAME sets the API refuses with): a node with `completion: all` must
// never walk as one approver, nor `onReject` as a final reject. This binary is the first that
// reads `graph`, hence the rollback target of every later slice: it cannot be patched afterwards.

// StepKeys is the closed JSON key set of one linear (v1) step. Step also binds ParallelKeys (S4),
// but those are graph-only in storage: the API lifts a step that carries them into a graph.
var StepKeys = []string{"id", "name", "approverType", "approverId", "slaSeconds", "slaClock"}

// ParallelKeys are the keys S4 adds to a node: several approvers and the policy that ends the step.
var ParallelKeys = []string{"approvers", "completion"}

// StepKeysParallel is the key set a step may carry on the API wire from S4: the v1 keys plus
// ParallelKeys, plus `assignment` (S6a). A linear step that carries any of the latter is lifted to a
// graph by the service (GraphFromSteps) — the linear column itself stays v1 (what a pre-S4 binary
// can read).
var StepKeysParallel = append(append(append([]string{}, StepKeys...), ParallelKeys...), "assignment")

// RoutingKeys are the keys S7 adds to a node: where it goes next (next), the gateway's branches
// and default, an auto node's field writes (set) and an end node's result.
var RoutingKeys = []string{"next", "branches", "default", "set", "result"}

// StepKeysWire is the key set a step may carry on the API wire from S8: StepKeysParallel plus
// onReject. Like the S4 keys, a step that carries onReject is lifted to the graph form.
var StepKeysWire = append(append([]string{}, StepKeysParallel...), "onReject")

// NodeKeys is the closed key set of one graph node: a step, its parallel keys (S4), onReject (S8),
// its kind (S3a2), the fields a task requires before it completes (S5) and the routing keys (S7).
var NodeKeys = append(append(append(append([]string{}, StepKeysWire...), "kind", "requiredFields"), RoutingKeys...), WaitKeys...)

// GraphKeys is the closed key set of the graph object: nodes, and (S7) the node the walk starts
// on (`start`, default the first).
var GraphKeys = []string{"nodes", "start"}

// UnknownKeys returns the top-level keys of the JSON object raw that are not in allowed, sorted.
// Comparison is case-insensitive because encoding/json BINDS keys that way — a key the typed
// decode understood must never be reported. A non-object yields nil: the typed decode is the one
// that reports malformed input.
func UnknownKeys(raw []byte, allowed ...string) []string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []string
	for k := range obj {
		known := false
		for _, a := range allowed {
			if strings.EqualFold(k, a) {
				known = true
				break
			}
		}
		if !known {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// NodeKind is what a graph node does.
type NodeKind string

// NodeApproval: a person (user), anyone holding a role, or a member of a work queue decides
// approve / reject.
const NodeApproval NodeKind = "approval"

// NodeTask: a piece of work to COMPLETE rather than a decision (specs/043 S5) — "Kiểm tra chứng
// từ". Same assignees as an approval; may name record fields (requiredFields) that must be filled
// before it can be completed.
const NodeTask NodeKind = "task"

// The machine nodes (specs/043 S7): nobody holds them. The engine walks through them without
// stopping — a gateway picks a branch from the record's CURRENT values, an auto node writes fields
// onto the record, an end node settles the instance — and stops only on a node a person acts on.
const (
	NodeGateway NodeKind = "gateway"
	NodeAuto    NodeKind = "auto"
	NodeEnd     NodeKind = "end"
)

// NodeWait: a node nobody holds that waits for the record to meet a condition (specs/043 S12a) —
// "phản hồi đầu tiên": it rests until `until` holds on the record, with an optional deadline
// (`sla`) whose breach is told to the people `sla.notify` names, not to a seat holder.
const NodeWait NodeKind = "wait"

// MachineKind reports whether k is a node the engine runs by itself. A wait node is one: no seat, no
// approver — the walk lands on it like on any machine node, and Runner.runWait either goes on (the
// condition holds) or lets the instance rest there until a record signal wakes it.
func MachineKind(k NodeKind) bool {
	return k == NodeGateway || k == NodeAuto || k == NodeEnd || k == NodeWait
}

// RunnableNodeKind reports whether this binary runs a node of kind k.
func RunnableNodeKind(k NodeKind) bool { return k == NodeApproval || k == NodeTask || MachineKind(k) }

// End results (Node.Result).
const (
	ResultApproved = "approved"
	ResultRejected = "rejected"
)

// Branch is one arm of a gateway: the first whose When holds on the record's current values wins.
type Branch struct {
	Name string              `json:"name"`
	When *workflow.Condition `json:"when"`
	To   string              `json:"to"`
}

// SetField is one field write of an auto node.
type SetField struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}

// Node is one node of a graph-form definition: a Step (stable id, name, one approver, SLA and
// its clock — the same JSON keys) plus its kind.
type Node struct {
	Step
	Kind NodeKind `json:"kind"`
	// RequiredFields (task nodes only, specs/043 S5): the record fields that must be filled before
	// the task can be completed. On the node, not on Step, so a Step stays comparable and the linear
	// v1 shape cannot carry it.
	RequiredFields []string `json:"requiredFields,omitempty"`
	// OnReject (approval nodes only, specs/043 S8): where a rejection of this node goes — back to
	// an earlier node or to the submitter to fix and send again. nil = the rejection is final. On
	// the node, not on Step, for the same reason as RequiredFields: the linear v1 shape cannot
	// carry it (a binary older than S8 would read it as a final reject).
	OnReject *OnReject `json:"onReject,omitempty"`
	// Next is the node reached when this one is done (default: the next in the array). Not on a
	// gateway (its branches decide) nor an end node (nothing follows).
	Next string `json:"next,omitempty"`
	// Branches + Default are a gateway's arms and the node taken when none holds (S7).
	Branches []Branch `json:"branches,omitempty"`
	Default  string   `json:"default,omitempty"`
	// Set is an auto node's field writes, in order (S7).
	Set []SetField `json:"set,omitempty"`
	// Result is how an end node settles the instance: "approved" | "rejected" (S7).
	Result string `json:"result,omitempty"`
	// Until + SLA are a wait node's condition and deadline (S12a).
	Until *workflow.Condition `json:"until,omitempty"`
	SLA   *WaitSLA            `json:"sla,omitempty"`
	// unknown lists the keys the decoded JSON carried outside NodeKeys (a later slice's). Never
	// written back: a node carrying any is refused, so it is never pinned.
	unknown []string
}

// UnmarshalJSON decodes the node and records its keys outside NodeKeys.
func (n *Node) UnmarshalJSON(b []byte) error {
	type plain Node
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("bpm.Node.UnmarshalJSON: %w", err)
	}
	*n = Node(p)
	n.unknown = UnknownKeys(b, NodeKeys...)
	return nil
}

// Graph is the graph form of a definition. A node is followed by its `next`, else by the next in
// the array; the walk starts on `start`, else the first node.
type Graph struct {
	Nodes []Node `json:"nodes"`
	// Start is the id of the node the walk begins on (S7); "" = the first.
	Start string `json:"start,omitempty"`
	// unknown lists the graph-level keys outside GraphKeys.
	unknown []string
}

// UnmarshalJSON decodes the graph (each node through Node.UnmarshalJSON) and records its keys
// outside GraphKeys.
func (g *Graph) UnmarshalJSON(b []byte) error {
	type plain Graph
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("bpm.Graph.UnmarshalJSON: %w", err)
	}
	*g = Graph(p)
	g.unknown = UnknownKeys(b, GraphKeys...)
	return nil
}

// approvalChain returns the nodes as the step chain the engine walks, with stable ids (a node
// without one gets the positional id, like NormalizeSteps). nil when the graph or ANY node carries
// a key outside the closed sets, or any node is not an approval node with one user/role approver:
// this binary cannot run that graph, and every caller treats "no chain" as refuse (arm skips,
// Decide errors, CreateInstance refuses).
func (g *Graph) approvalChain() []Step {
	if g == nil || len(g.Nodes) == 0 || len(g.unknown) > 0 {
		return nil
	}
	steps := make([]Step, len(g.Nodes))
	for i, n := range g.Nodes {
		if len(n.unknown) > 0 || !RunnableNodeKind(n.Kind) {
			return nil
		}
		if !n.shapeOK() {
			return nil
		}
		if MachineKind(n.Kind) {
			steps[i] = n.Step
			steps[i].NodeKind = n.Kind
			continue
		}
		if n.Assignment != nil {
			// a rule naming its holder (S6a): on an approval or a task node, instead of approverType/approverId
			if f, _ := CheckAssignment(n.Step); f != "" {
				return nil
			}
			if n.Parallel() && n.Kind != NodeApproval { // each_member (S6b): a council decides, it does not complete a task
				return nil
			}
		} else if n.Parallel() {
			// several approvers (S4) belong to an approval node only; a task or queue seat is one assignee.
			if n.Kind != NodeApproval {
				return nil
			}
			if f, _ := CheckApprovers(n.Step); f != "" {
				return nil
			}
		} else if n.Completion != nil || !ValidAssigneeType(n.ApproverType) || n.ApproverID == "" {
			return nil
		}
		// requiredFields belongs to a task: on an approval node it is a config this binary cannot
		// honour, so it is refused like any unknown key (fail-closed, D-7).
		if n.Kind != NodeTask && len(n.RequiredFields) > 0 {
			return nil
		}
		steps[i] = n.Step
		steps[i].NodeKind = n.Kind
	}
	steps = NormalizeSteps(steps)
	// onReject is judged against the ids the chain really has (an id-less node got its positional
	// one above): a target this binary cannot honour is refused like an unknown key, never read as a
	// final reject (D-7, fail-closed).
	for i, n := range g.Nodes {
		if n.OnReject != nil && !n.OnReject.validAt(n.Kind, i, steps) {
			return nil
		}
	}
	return steps
}

// normalized returns a copy of the graph whose nodes all carry stable ids (NormalizeSteps over the
// step half of each node — the same ids approvalChain hands out) and keep their unknown keys, so
// the copy is refused exactly like the original. nil stays nil.
func (g *Graph) normalized() *Graph {
	if g == nil {
		return nil
	}
	steps := make([]Step, len(g.Nodes))
	for i, n := range g.Nodes {
		steps[i] = n.Step
	}
	steps = NormalizeSteps(steps)
	out := &Graph{Nodes: make([]Node, len(g.Nodes)), Start: g.Start, unknown: g.unknown}
	for i, n := range g.Nodes {
		out.Nodes[i] = n // every field, routing included, then the step half with its stable id
		out.Nodes[i].Step = steps[i]
	}
	return out
}

// shapeOK reports whether the node carries only what its kind may: a machine node names no
// approver, deadline or required field; a gateway has branches and a default and no next; an auto
// node has writes; an end node a result and nothing after it; a person's node none of the routing
// of a machine. Anything else is a config this binary cannot honour, refused like an unknown key.
func (n Node) shapeOK() bool {
	human := n.Kind == NodeApproval || n.Kind == NodeTask
	if (n.Until != nil || n.SLA != nil) && n.Kind != NodeWait {
		return false // a wait node's keys on any other kind: a config this binary cannot honour
	}
	if MachineKind(n.Kind) {
		// nobody holds a machine node, so there is nobody to reject it either (onReject, S8)
		if n.ApproverType != "" || n.ApproverID != "" || len(n.Approvers) > 0 || n.Completion != nil ||
			n.SLASeconds != 0 || len(n.RequiredFields) > 0 || n.OnReject != nil || n.Assignment != nil {
			return false
		}
	}
	switch n.Kind {
	case NodeGateway:
		return len(n.Branches) > 0 && n.Default != "" && n.Next == "" && len(n.Set) == 0 && n.Result == ""
	case NodeAuto:
		return len(n.Set) > 0 && len(n.Branches) == 0 && n.Default == "" && n.Result == ""
	case NodeEnd:
		return (n.Result == ResultApproved || n.Result == ResultRejected) &&
			n.Next == "" && len(n.Branches) == 0 && n.Default == "" && len(n.Set) == 0
	case NodeWait:
		f, _ := CheckWait(n)
		return f == "" && len(n.Branches) == 0 && n.Default == "" && len(n.Set) == 0 && n.Result == ""
	}
	return human && len(n.Branches) == 0 && n.Default == "" && len(n.Set) == 0 && n.Result == ""
}

// HasRouting reports whether the graph is more than a straight line of people: a machine node, a
// `next` or a `start`. Such a graph has no flat `steps` shape — the pack exporter leaves it out by
// name rather than ship it as a chain that would drop its branches.
func (g *Graph) HasRouting() bool {
	if g == nil {
		return false
	}
	if g.Start != "" {
		return true
	}
	for _, n := range g.Nodes {
		if MachineKind(n.Kind) || n.Next != "" {
			return true
		}
	}
	return false
}

// IndexOf returns the position of the node with this id, -1 when none.
func (g *Graph) IndexOf(id string) int {
	if g == nil {
		return -1
	}
	for i, n := range g.Nodes {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// StartIndex is the position of the node the walk begins on: `start`, else the first. -1 when
// `start` names no node (a definition this binary refuses to run).
func (g *Graph) StartIndex() int {
	if g == nil || len(g.Nodes) == 0 {
		return -1
	}
	if g.Start == "" {
		return 0
	}
	return g.IndexOf(g.Start)
}

// NodeAt returns the graph node at chain index i, nil for a linear definition, a graph this binary
// cannot run or an index outside it. The chain of a graph IS its node array, so the index of a
// step in Chain() is the index of its node.
func (d *Definition) NodeAt(i int) *Node {
	if d == nil || d.Graph == nil || i < 0 || i >= len(d.Graph.Nodes) || len(d.Chain()) == 0 {
		return nil
	}
	return &d.Graph.Nodes[i]
}

// StartIndex is the chain index a NEW instance of the definition stands on: the graph's start
// node, else the first step. -1 when the definition has nothing this binary runs.
func (d *Definition) StartIndex() int {
	if d == nil || len(d.Chain()) == 0 {
		return -1
	}
	if d.Graph != nil {
		return d.Graph.StartIndex()
	}
	return 0
}

// Successor is where the walk goes after chain index i finishes normally (a person approved or
// completed it, an auto node wrote its fields): the node's `next`, else the following one in the
// array. end=true when that runs off the array — the definition's implicit approval (the way a
// linear chain approves after its last step). ok=false when `next` names no node.
func (d *Definition) Successor(i int) (idx int, end, ok bool) {
	n := d.NodeAt(i)
	if n == nil || n.Next == "" {
		idx = i + 1
		return idx, idx >= len(d.Chain()), true
	}
	idx = d.Graph.IndexOf(n.Next)
	return idx, false, idx >= 0
}

// RequiredFieldsAt returns the fields step i of the definition's chain must have filled before it
// can be completed (a task node's requiredFields); nil for a linear definition, an approval node,
// or an index outside the chain. A graph walks in array order, so chain index i is node i.
func (d *Definition) RequiredFieldsAt(i int) []string {
	if d == nil || d.Graph == nil || i < 0 || i >= len(d.Graph.Nodes) || len(d.Chain()) == 0 {
		return nil
	}
	return d.Graph.Nodes[i].RequiredFields
}

// OnRejectAt returns where a rejection of step i of the definition's chain goes; nil (the rejection
// is final) for a linear definition, a node without onReject, or an index outside the chain. A graph
// walks in array order, so chain index i is node i.
func (d *Definition) OnRejectAt(i int) *OnReject {
	if d == nil || d.Graph == nil || i < 0 || i >= len(d.Graph.Nodes) || len(d.Chain()) == 0 {
		return nil
	}
	return d.Graph.Nodes[i].OnReject
}

// chainOf is the chain a definition in either form walks: the graph's (graph > steps, D-2), else
// the steps. A graph this binary cannot run yields nil — never the steps beside it.
func chainOf(steps []Step, g *Graph) []Step {
	if g != nil {
		return g.approvalChain()
	}
	return steps
}

// Chain returns the steps a process walks, in either storage form.
func (p Process) Chain() []Step { return chainOf(p.Steps, p.Graph) }

// Chain returns the steps a pinned (or live-read) definition walks, in either storage form. nil
// for a nil definition, an empty one, or a graph this binary cannot run.
func (d *Definition) Chain() []Step {
	if d == nil {
		return nil
	}
	return chainOf(d.Steps, d.Graph)
}
