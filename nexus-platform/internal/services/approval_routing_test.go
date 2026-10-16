package services

// specs/043 S7 (T124, FR-003, FR-004): the graph form opens the machine nodes — gateway, auto, end —
// and the validator that every writer of a process goes through judges how they lead to each other:
// a default on every gateway, no loop of machine nodes alone, a way to the end, every target a node.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/metadata"
)

func gwNode(id, def string, branches ...ApprovalBranchInput) ApprovalNodeInput {
	return ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{ID: id}, Kind: "gateway", Default: def, Branches: branches}
}

func branchTo(name, to string, rules ...ApprovalCriteriaRuleInput) ApprovalBranchInput {
	if len(rules) == 0 {
		rules = []ApprovalCriteriaRuleInput{{Field: "amount", Op: "gt", Value: float64(1)}}
	}
	return ApprovalBranchInput{Name: name, Rules: rules, To: to}
}

func autoNode(id, next string, set ...ApprovalSetInput) ApprovalNodeInput {
	return ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{ID: id}, Kind: "auto", Next: next, Set: set}
}

func endNode(id, result string) ApprovalNodeInput {
	return ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{ID: id}, Kind: "end", Result: result}
}

func humanNode(id, next string) ApprovalNodeInput {
	n := approvalNode(id, "approval", "user", "u-"+id)
	n.Next = next
	return n
}

func createRouted(t *testing.T, g *ApprovalGraphInput) (*graphProcRepo, error) {
	t.Helper()
	repo := &graphProcRepo{}
	_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "P", EntityName: "crm_quotes", Trigger: "manual", Graph: g,
	})
	return repo, err
}

var statusSet = ApprovalSetInput{Field: "status", Value: "Chấp nhận"}

// TestCreateProcess_RoutedGraphStored: the spec's quote process — under 5 million approves itself,
// under 50 million goes to the manager, from 50 million the director follows — is accepted and
// stored with its routing exactly as sent.
func TestCreateProcess_RoutedGraphStored(t *testing.T) {
	repo, err := createRouted(t, &ApprovalGraphInput{Start: "route", Nodes: []ApprovalNodeInput{
		gwNode("route", "mgr",
			branchTo("tu-duyet", "auto-ok", ApprovalCriteriaRuleInput{Field: "amount", Op: "lt", Value: float64(5e6)}),
			branchTo("cap-1", "mgr", ApprovalCriteriaRuleInput{Field: "amount", Op: "lt", Value: float64(5e7)})),
		autoNode("auto-ok", "done", statusSet),
		humanNode("mgr", "big"),
		gwNode("big", "done", branchTo("tren-50tr", "dir", ApprovalCriteriaRuleInput{Field: "amount", Op: "gte", Value: float64(5e7)})),
		humanNode("dir", "done"),
		endNode("done", "approved"),
	}})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.created.Graph, &g); err != nil {
		t.Fatalf("stored graph %s: %v", repo.created.Graph, err)
	}
	if g.Start != "route" || len(g.Nodes) != 6 {
		t.Fatalf("stored graph %s; want start=route and six nodes", repo.created.Graph)
	}
	route := g.Nodes[0]
	if route.Kind != bpm.NodeGateway || route.Default != "mgr" || len(route.Branches) != 2 ||
		route.Branches[0].Name != "tu-duyet" || route.Branches[0].To != "auto-ok" || route.Branches[0].When.Rules[0].Op != "lt" {
		t.Fatalf("gateway stored as %+v", route)
	}
	if n := g.Nodes[1]; n.Kind != bpm.NodeAuto || n.Next != "done" || len(n.Set) != 1 || n.Set[0].Field != "status" || n.ApproverType != "" {
		t.Fatalf("auto node stored as %+v; want a status write, next=done and no approver", n)
	}
	if n := g.Nodes[5]; n.Kind != bpm.NodeEnd || n.Result != "approved" {
		t.Fatalf("end node stored as %+v", n)
	}
	// The stored form is one this binary's engine actually runs.
	if (&bpm.Process{Graph: &g}).Chain() == nil {
		t.Fatal("the stored graph is not runnable by the engine")
	}
}

// TestValidate_GatewayDefaultRequired (T124, spec US10): a gateway without a default fails right at
// the gateway, naming the key.
func TestValidate_GatewayDefaultRequired(t *testing.T) {
	_, err := createRouted(t, &ApprovalGraphInput{Nodes: []ApprovalNodeInput{
		gwNode("route", "", branchTo("x", "done")), endNode("done", "approved"),
	}})
	ve := wantApprovalCode(t, err, "APPROVAL_GATEWAY_DEFAULT_REQUIRED")
	if ve.Field != "graph.nodes[0].default" {
		t.Fatalf("field %q; want graph.nodes[0].default", ve.Field)
	}
}

// TestValidate_AutoOnlyCycleRejected (T124): a loop of machine nodes has no person to ask and would
// only spin — refused at save, not found at run time.
func TestValidate_AutoOnlyCycleRejected(t *testing.T) {
	repo, err := createRouted(t, &ApprovalGraphInput{Nodes: []ApprovalNodeInput{
		autoNode("a", "b", statusSet), autoNode("b", "a", ApprovalSetInput{Field: "note", Value: "x"}), humanNode("p", ""),
	}})
	wantApprovalCode(t, err, "APPROVAL_AUTO_CYCLE")
	if repo.created != nil {
		t.Fatal("a looping graph reached the repository")
	}
}

// TestValidate_EndUnreachable (T124): two people who approve each other forever never end.
func TestValidate_EndUnreachable(t *testing.T) {
	_, err := createRouted(t, &ApprovalGraphInput{Nodes: []ApprovalNodeInput{humanNode("a", "b"), humanNode("b", "a")}})
	wantApprovalCode(t, err, "APPROVAL_END_UNREACHABLE")
}

// TestValidate_RoutingShapes: every way a machine node can be mis-shaped is a 422 at the node, from
// the same body for every caller (B6 parity: nothing the engine would ignore is stored).
func TestValidate_RoutingShapes(t *testing.T) {
	approverOnGateway := gwNode("g", "done", branchTo("x", "done"))
	approverOnGateway.ApproverType, approverOnGateway.ApproverID = "user", "u-1"
	slaOnAuto := autoNode("a", "", statusSet)
	slaOnAuto.SLASeconds = 60
	setOnHuman := humanNode("h", "")
	setOnHuman.Set = []ApprovalSetInput{statusSet}
	gwNext := gwNode("g", "done", branchTo("x", "done"))
	gwNext.Next = "done"

	cases := []struct {
		name      string
		nodes     []ApprovalNodeInput
		wantCode  string
		wantField string
	}{
		{"unknown branch target", []ApprovalNodeInput{gwNode("g", "done", branchTo("x", "nowhere")), endNode("done", "approved")},
			"APPROVAL_TRANSITION_TARGET_UNKNOWN", "graph.nodes[0].branches[0].to"},
		{"unknown default target", []ApprovalNodeInput{gwNode("g", "nowhere", branchTo("x", "done")), endNode("done", "approved")},
			"APPROVAL_TRANSITION_TARGET_UNKNOWN", "graph.nodes[0].default"},
		{"unknown next", []ApprovalNodeInput{humanNode("a", "nowhere")}, "APPROVAL_TRANSITION_TARGET_UNKNOWN", "graph.nodes[0].next"},
		{"unknown start", []ApprovalNodeInput{humanNode("a", "")}, "APPROVAL_TRANSITION_TARGET_UNKNOWN", "graph.start"},
		{"gateway with no branch", []ApprovalNodeInput{gwNode("g", "done"), endNode("done", "approved")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].branches"},
		{"branch without a name", []ApprovalNodeInput{gwNode("g", "done", branchTo(" ", "done")), endNode("done", "approved")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].branches[0].name"},
		{"branch with no rule", []ApprovalNodeInput{gwNode("g", "done", ApprovalBranchInput{Name: "x", To: "done"}), endNode("done", "approved")},
			"APPROVAL_STEP_INVALID", "graph.nodes[0].branches[0].when"},
		{"two branches, one name", []ApprovalNodeInput{gwNode("g", "done", branchTo("x", "done"), branchTo("x", "done")), endNode("done", "approved")},
			"APPROVAL_STEP_INVALID", "graph.nodes[0].branches[1].name"},
		{"edge operator in a branch", []ApprovalNodeInput{gwNode("g", "done",
			branchTo("x", "done", ApprovalCriteriaRuleInput{Field: "status", Op: "changedTo", Value: "A"})), endNode("done", "approved")},
			"APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED", "graph.nodes[0].branches[0].when"},
		{"bad operator in a branch", []ApprovalNodeInput{gwNode("g", "done",
			branchTo("x", "done", ApprovalCriteriaRuleInput{Field: "amount", Op: "approx", Value: 1})), endNode("done", "approved")},
			"APPROVAL_CRITERIA_INVALID", "graph.nodes[0].branches[0].when"},
		{"gateway with next", []ApprovalNodeInput{gwNext, endNode("done", "approved")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].next"},
		{"auto with nothing to write", []ApprovalNodeInput{autoNode("a", ""), humanNode("p", "")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].set"},
		{"auto writes a field twice", []ApprovalNodeInput{autoNode("a", "", statusSet, statusSet), humanNode("p", "")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].set[1].field"},
		{"auto write without a value", []ApprovalNodeInput{autoNode("a", "", ApprovalSetInput{Field: "status"}), humanNode("p", "")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].set[0].value"},
		{"end with a bad result", []ApprovalNodeInput{endNode("e", "maybe")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].result"},
		{"approver on a gateway", []ApprovalNodeInput{approverOnGateway, endNode("done", "approved")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].approverType"},
		{"deadline on an auto node", []ApprovalNodeInput{slaOnAuto, humanNode("p", "")}, "APPROVAL_STEP_INVALID", "graph.nodes[0].slaSeconds"},
		{"write on a person's node", []ApprovalNodeInput{setOnHuman}, "APPROVAL_STEP_INVALID", "graph.nodes[0].set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &ApprovalGraphInput{Nodes: c.nodes}
			if c.name == "unknown start" {
				g.Start = "nowhere"
			}
			repo, err := createRouted(t, g)
			ve := wantApprovalCode(t, err, c.wantCode)
			if ve.Field != c.wantField {
				t.Fatalf("field %q; want %q (%s)", ve.Field, c.wantField, ve.Msg)
			}
			if repo.created != nil {
				t.Fatal("an invalid graph reached the repository")
			}
		})
	}
}

// routedEntities answers the entity/field lookups of checkRoutingRefs: quotes with a text amount-ish
// field, a picklist status and a formula (computed) total.
type routedEntities struct{}

func (routedEntities) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "e1", TenantID: tenantID, Name: name}, nil
}

func (routedEntities) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	opts := `{"values":["Nháp","Chấp nhận"]}`
	return []metadata.FieldDefinition{
		{Name: "amount", FieldType: "number"},
		{Name: "status", FieldType: "picklist", Options: &opts},
		{Name: "total", FieldType: "formula"},
	}, nil
}

// TestCreateProcess_RoutingFieldsJudgedAgainstTheEntity: a gateway may read only fields the entity
// has; an auto node may write only a field that exists, is writable and takes the value — so the
// "picklist has no such value" error surfaces at save instead of stalling an approval later.
func TestCreateProcess_RoutingFieldsJudgedAgainstTheEntity(t *testing.T) {
	good := &ApprovalGraphInput{Nodes: []ApprovalNodeInput{
		gwNode("g", "a", branchTo("big", "a", ApprovalCriteriaRuleInput{Field: "amount", Op: "gt", Value: float64(1)})),
		autoNode("a", "", statusSet), endNode("done", "approved"),
	}}
	cases := []struct {
		name      string
		mutate    func(g *ApprovalGraphInput)
		wantCode  string
		wantField string
	}{
		{"valid", func(*ApprovalGraphInput) {}, "", ""},
		{"gateway reads an unknown field", func(g *ApprovalGraphInput) { g.Nodes[0].Branches[0].Rules[0].Field = "nope" },
			"APPROVAL_FIELD_UNKNOWN", "graph.nodes[0].branches[0].when.rules[0].field"},
		{"auto writes an unknown field", func(g *ApprovalGraphInput) { g.Nodes[1].Set[0].Field = "nope" },
			"RECORD_UNKNOWN_FIELD", "graph.nodes[1].set[0].field"},
		{"auto writes a computed field", func(g *ApprovalGraphInput) { g.Nodes[1].Set[0] = ApprovalSetInput{Field: "total", Value: float64(1)} },
			"", "graph.nodes[1].set[0].field"},
		{"auto writes a value the picklist does not have", func(g *ApprovalGraphInput) { g.Nodes[1].Set[0].Value = "Không có" },
			"", "graph.nodes[1].set[0].value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := *good
			g.Nodes = append([]ApprovalNodeInput(nil), good.Nodes...)
			g.Nodes[0].Branches = append([]ApprovalBranchInput(nil), good.Nodes[0].Branches...)
			g.Nodes[0].Branches[0].Rules = append([]ApprovalCriteriaRuleInput(nil), good.Nodes[0].Branches[0].Rules...)
			g.Nodes[1].Set = append([]ApprovalSetInput(nil), good.Nodes[1].Set...)
			c.mutate(&g)
			repo := &graphProcRepo{}
			_, err := NewApprovalService(repo, nil, nil).WithEntityChecker(routedEntities{}).CreateProcess(context.Background(), "t1",
				CreateApprovalProcessInput{Name: "P", EntityName: "crm_quotes", Trigger: "manual", Graph: &g})
			if c.name == "valid" {
				if err != nil {
					t.Fatalf("valid routed graph refused: %v", err)
				}
				return
			}
			var ve *ValidationError
			if err == nil || !errors.As(err, &ve) {
				t.Fatalf("err = %v; want a ValidationError at %s", err, c.wantField)
			}
			if ve.Field != c.wantField || (c.wantCode != "" && ve.Code != c.wantCode) {
				t.Fatalf("got {%s %s %q}; want {%s %s}", ve.Code, ve.Field, ve.Msg, c.wantCode, c.wantField)
			}
			if repo.created != nil {
				t.Fatal("an invalid graph reached the repository")
			}
		})
	}
}
