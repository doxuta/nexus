package bpm

import (
	"fmt"
	"strings"
)

// Codes of the routing problems CheckGraph reports (specs/043 plan "Validator"). The API validator
// (services.buildApprovalGraphJSON) turns a GraphProblem into a 422 carrying exactly this code and
// the field it points at — one body judges the graph for Studio, AI, packs and the raw API (FR-004).
const (
	CodeTransitionTargetUnknown = "APPROVAL_TRANSITION_TARGET_UNKNOWN"
	CodeGatewayDefaultRequired  = "APPROVAL_GATEWAY_DEFAULT_REQUIRED"
	CodeAutoCycle               = "APPROVAL_AUTO_CYCLE"
	CodeEndUnreachable          = "APPROVAL_END_UNREACHABLE"
	CodeStepInvalid             = "APPROVAL_STEP_INVALID"
)

// GraphProblem is one thing wrong with how a graph's nodes lead to each other.
type GraphProblem struct {
	Code string
	// Node is the index of the node at fault, -1 for the graph itself (its `start`).
	Node int
	// Field is the path below that node ("branches[1].to", "default", "next"); "" = the node.
	Field  string
	Reason string
}

// CheckGraph judges the routing of a graph whose nodes already carry unique ids (the service
// normalizes them first) and whose node shapes were checked: every `start` / `next` / branch `to` /
// `default` names a node; a gateway has a default and well-formed branches; no loop consists of
// machine nodes alone (nobody would ever be asked, the walk would spin); and every node the walk
// can reach has a way to the end by approving — falling off the array counts as the implicit
// approval, an end node as an explicit one. nil = the graph routes.
//
// A rejection ends an instance from any person's step, so "reaches the end" is judged along the
// approving path only: a pair of people who approve each other forever has no end.
func CheckGraph(g *Graph) *GraphProblem {
	if g == nil || len(g.Nodes) == 0 {
		return nil
	}
	if g.Start != "" && g.IndexOf(g.Start) < 0 {
		return &GraphProblem{Code: CodeTransitionTargetUnknown, Node: -1, Field: "start",
			Reason: fmt.Sprintf("start trỏ tới bước %q không có trong quy trình", g.Start)}
	}
	for i, n := range g.Nodes {
		if p := checkNodeRefs(g, i, n); p != nil {
			return p
		}
	}
	if p := machineCycle(g); p != nil {
		return p
	}
	return unreachableEnd(g)
}

func checkNodeRefs(g *Graph, i int, n Node) *GraphProblem {
	if n.Kind == NodeTask && n.Parallel() { // the engine refuses to walk it (approvalChain): refuse to store it too
		field := "approvers"
		if n.Assignment != nil {
			field = "assignment.strategy"
		}
		return &GraphProblem{Code: CodeStepInvalid, Node: i, Field: field,
			Reason: "một việc (task) do một người hoàn thành — nhiều người cùng giữ chỉ dành cho bước duyệt (approval)"}
	}
	unknown := func(field, to string) *GraphProblem {
		return &GraphProblem{Code: CodeTransitionTargetUnknown, Node: i, Field: field,
			Reason: fmt.Sprintf("trỏ tới bước %q không có trong quy trình", to)}
	}
	if n.Kind == NodeGateway {
		if strings.TrimSpace(n.Default) == "" {
			return &GraphProblem{Code: CodeGatewayDefaultRequired, Node: i, Field: "default",
				Reason: "bước rẽ nhánh phải có nhánh mặc định (dùng khi không nhánh nào khớp)"}
		}
		if g.IndexOf(n.Default) < 0 {
			return unknown("default", n.Default)
		}
		seen := make(map[string]int, len(n.Branches))
		for j, b := range n.Branches {
			name := strings.TrimSpace(b.Name)
			switch {
			case name == "":
				return &GraphProblem{Code: CodeStepInvalid, Node: i, Field: fmt.Sprintf("branches[%d].name", j), Reason: "nhánh cần có tên"}
			case seen[name] > 0:
				return &GraphProblem{Code: CodeStepInvalid, Node: i, Field: fmt.Sprintf("branches[%d].name", j),
					Reason: fmt.Sprintf("tên nhánh %q trùng với nhánh %d", name, seen[name])}
			case b.When == nil || len(b.When.Rules) == 0:
				return &GraphProblem{Code: CodeStepInvalid, Node: i, Field: fmt.Sprintf("branches[%d].when", j),
					Reason: fmt.Sprintf("nhánh %q cần ít nhất một điều kiện (nhánh không điều kiện là nhánh mặc định)", name)}
			case g.IndexOf(b.To) < 0:
				return unknown(fmt.Sprintf("branches[%d].to", j), b.To)
			}
			seen[name] = j + 1
		}
		return nil
	}
	if n.Next != "" && g.IndexOf(n.Next) < 0 {
		return unknown("next", n.Next)
	}
	return nil
}

// edges lists the nodes a walk can go to from node i along the normal (approving) path, and whether
// it can also END there (an end node, or falling off the array).
func edges(g *Graph, i int) (to []int, ends bool) {
	n := g.Nodes[i]
	switch n.Kind {
	case NodeEnd:
		return nil, true
	case NodeGateway:
		for _, b := range n.Branches {
			to = append(to, g.IndexOf(b.To))
		}
		return append(to, g.IndexOf(n.Default)), false
	}
	if n.Next != "" {
		return []int{g.IndexOf(n.Next)}, false
	}
	if i+1 >= len(g.Nodes) {
		return nil, true
	}
	return []int{i + 1}, false
}

// machineCycle finds a loop whose every node is a machine node (gateway / auto): a walk entering it
// never reaches a person, so it could only spin. A loop through a person's step is allowed — they
// are asked each time round.
func machineCycle(g *Graph) *GraphProblem {
	const (
		unseen = iota
		onPath
		done
	)
	state := make([]int, len(g.Nodes))
	var cycleAt = -1
	var visit func(i int)
	visit = func(i int) {
		state[i] = onPath
		to, _ := edges(g, i)
		for _, j := range to {
			if cycleAt >= 0 || j < 0 || !MachineKind(g.Nodes[j].Kind) {
				continue // a person's step (or the end) breaks the loop
			}
			switch state[j] {
			case onPath:
				cycleAt = j
			case unseen:
				visit(j)
			}
		}
		state[i] = done
	}
	for i, n := range g.Nodes {
		if cycleAt < 0 && MachineKind(n.Kind) && state[i] == unseen {
			visit(i)
		}
	}
	if cycleAt < 0 {
		return nil
	}
	return &GraphProblem{Code: CodeAutoCycle, Node: cycleAt, Field: "",
		Reason: "bước này nằm trong một vòng chỉ gồm bước tự động/rẽ nhánh — không có người nào được hỏi nên hồ sơ sẽ quay mãi; chèn một bước có người xử lý hoặc đổi đích"}
}

// unreachableEnd reports the first node the walk can reach from `start` that cannot get to an end.
func unreachableEnd(g *Graph) *GraphProblem {
	n := len(g.Nodes)
	canEnd := make([]bool, n)
	for changed := true; changed; {
		changed = false
		for i := 0; i < n; i++ {
			if canEnd[i] {
				continue
			}
			to, ends := edges(g, i)
			ok := ends
			for _, j := range to {
				if j >= 0 && canEnd[j] {
					ok = true
				}
			}
			if ok {
				canEnd[i], changed = true, true
			}
		}
	}
	reach := make([]bool, n)
	var walk func(i int)
	walk = func(i int) {
		if i < 0 || reach[i] {
			return
		}
		reach[i] = true
		to, _ := edges(g, i)
		for _, j := range to {
			walk(j)
		}
	}
	walk(g.StartIndex())
	for i := 0; i < n; i++ {
		if reach[i] && !canEnd[i] {
			return &GraphProblem{Code: CodeEndUnreachable, Node: i, Field: "",
				Reason: "từ bước này không đi tới được bước kết thúc (hoặc bước cuối) bằng đường duyệt — thêm bước kết thúc hoặc bỏ vòng"}
		}
	}
	return nil
}
