package bpm

import "testing"

func person(id string) Node {
	return Node{Step: Step{ID: id, ApproverType: ApproverUser, ApproverID: "u-" + id}, Kind: NodeApproval}
}

func auto(id, next string) Node {
	return Node{Step: Step{ID: id}, Kind: NodeAuto, Set: []SetField{{Field: "f", Value: "v"}}, Next: next}
}

func gw(id, def string, to ...string) Node {
	n := Node{Step: Step{ID: id}, Kind: NodeGateway, Default: def}
	for i, t := range to {
		n.Branches = append(n.Branches, Branch{Name: "b" + string(rune('0'+i)), When: cond("", amount("gt", float64(i))), To: t})
	}
	return n
}

func end(id string) Node { return Node{Step: Step{ID: id}, Kind: NodeEnd, Result: ResultApproved} }

// TestCheckGraph is the routing judge behind TestValidate_AutoOnlyCycleRejected,
// TestValidate_GatewayDefaultRequired, TestValidate_EndUnreachable and the unknown-target check
// (services runs it for every writer of a graph).
func TestCheckGraph(t *testing.T) {
	tests := []struct {
		name     string
		g        *Graph
		wantCode string
		wantNode int
		wantFld  string
	}{
		{"the quote graph routes", quoteGraph(), "", 0, ""},
		{"linear people fall off the end", &Graph{Nodes: []Node{person("a"), person("b")}}, "", 0, ""},
		{"a loop through a person is allowed when it has an exit",
			&Graph{Nodes: []Node{person("a"), gw("g", "end", "a"), end("end")}}, "", 0, ""},

		{"start names no node", &Graph{Start: "zzz", Nodes: []Node{person("a")}}, CodeTransitionTargetUnknown, -1, "start"},
		{"next names no node", &Graph{Nodes: []Node{func() Node { n := person("a"); n.Next = "nope"; return n }()}}, CodeTransitionTargetUnknown, 0, "next"},
		{"branch target names no node", &Graph{Nodes: []Node{gw("g", "end", "nope"), end("end")}}, CodeTransitionTargetUnknown, 0, "branches[0].to"},
		{"default names no node", &Graph{Nodes: []Node{gw("g", "nope", "end"), end("end")}}, CodeTransitionTargetUnknown, 0, "default"},

		{"gateway without a default", &Graph{Nodes: []Node{gw("g", "", "end"), end("end")}}, CodeGatewayDefaultRequired, 0, "default"},
		{"branch without a name", &Graph{Nodes: []Node{
			{Step: Step{ID: "g"}, Kind: NodeGateway, Default: "end", Branches: []Branch{{Name: " ", When: cond("", amount("gt", 1)), To: "end"}}}, end("end")}},
			CodeStepInvalid, 0, "branches[0].name"},
		{"branch without a condition", &Graph{Nodes: []Node{
			{Step: Step{ID: "g"}, Kind: NodeGateway, Default: "end", Branches: []Branch{{Name: "x", When: cond(""), To: "end"}}}, end("end")}},
			CodeStepInvalid, 0, "branches[0].when"},

		{"two autos feeding each other", &Graph{Nodes: []Node{auto("a", "b"), auto("b", "a"), person("p")}}, CodeAutoCycle, 0, ""},
		{"an auto that points at itself", &Graph{Nodes: []Node{auto("a", "a"), person("p")}}, CodeAutoCycle, 0, ""},
		{"gateway and auto in a loop", &Graph{Nodes: []Node{gw("g", "a", "a"), auto("a", "g"), person("p")}}, CodeAutoCycle, 0, ""},

		{"two people approving each other forever",
			&Graph{Nodes: []Node{
				func() Node { n := person("a"); n.Next = "b"; return n }(),
				func() Node { n := person("b"); n.Next = "a"; return n }(),
			}}, CodeEndUnreachable, 0, ""},
		{"a branch that leads into a dead loop",
			&Graph{Nodes: []Node{
				gw("g", "end", "a"),
				func() Node { n := person("a"); n.Next = "a"; return n }(),
				end("end"),
			}}, CodeEndUnreachable, 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := CheckGraph(tc.g)
			if tc.wantCode == "" {
				if p != nil {
					t.Fatalf("CheckGraph = %+v; want nil", p)
				}
				return
			}
			if p == nil {
				t.Fatalf("CheckGraph = nil; want %s at node %d", tc.wantCode, tc.wantNode)
			}
			if p.Code != tc.wantCode || p.Node != tc.wantNode || p.Field != tc.wantFld {
				t.Fatalf("CheckGraph = {%s node %d %q}; want {%s node %d %q}", p.Code, p.Node, p.Field, tc.wantCode, tc.wantNode, tc.wantFld)
			}
		})
	}
}

// TestDefinition_RoutingHelpers: Successor / StartIndex / NodeAt are what the decider and the arm
// path walk by.
func TestDefinition_RoutingHelpers(t *testing.T) {
	g := quoteGraph().normalized()
	d := &Definition{Steps: []Step{}, Graph: g}
	if got := d.StartIndex(); got != 0 {
		t.Fatalf("StartIndex = %d; want 0 (route)", got)
	}
	mgr := g.IndexOf("mgr")
	if idx, end, ok := d.Successor(mgr); !ok || end || g.Nodes[idx].ID != "big" {
		t.Fatalf("Successor(mgr) = %d end=%v ok=%v; want the node named by next (big)", idx, end, ok)
	}
	// With no `next` the array order applies, and the last node ends the walk.
	lin := &Definition{Steps: []Step{}, Graph: (&Graph{Nodes: []Node{person("a"), person("b")}}).normalized()}
	if idx, end, ok := lin.Successor(0); !ok || end || idx != 1 {
		t.Fatalf("Successor(0) = %d end=%v ok=%v; want 1", idx, end, ok)
	}
	if _, end, ok := lin.Successor(1); !ok || !end {
		t.Fatalf("Successor(last) end=%v ok=%v; want the end of the walk", end, ok)
	}
	// A linear (steps) definition is unchanged: next index, end after the last.
	steps := &Definition{Steps: []Step{{ID: "s1", ApproverType: ApproverUser, ApproverID: "u"}, {ID: "s2", ApproverType: ApproverUser, ApproverID: "u"}}}
	if idx, end, ok := steps.Successor(0); !ok || end || idx != 1 {
		t.Fatalf("linear Successor(0) = %d end=%v ok=%v", idx, end, ok)
	}
	if steps.StartIndex() != 0 || steps.NodeAt(0) != nil {
		t.Fatalf("linear definition: StartIndex %d NodeAt %v; want 0 / nil", steps.StartIndex(), steps.NodeAt(0))
	}
}
