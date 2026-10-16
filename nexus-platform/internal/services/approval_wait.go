package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/workflow"
)

// The wait node at the API (specs/043 S12a): `until` + `sla{amount, unit, matrix, writeDueTo, notify}`.
// Shape is judged by bpm.CheckWait — the SAME body the engine refuses a stored graph by — so what the
// binary will not run, the API will not store; the tenant-dependent half (fields exist and have the
// right type, the notified user/role/team exists) is checkWaitRefs.

// ApprovalConditionInput is a condition in the entry-criteria vocabulary (match + rules).
type ApprovalConditionInput struct {
	Match string
	Rules []ApprovalCriteriaRuleInput
}

// ApprovalSLARowInput is one row of a wait node's priority matrix.
type ApprovalSLARowInput struct {
	When   ApprovalConditionInput
	Amount int
	Unit   string
}

// ApprovalWaitSLAInput is a wait node's `sla` object as received from the API. Unsupported lists the
// keys sent outside bpm.WaitSLAKeys (refused, never dropped silently).
type ApprovalWaitSLAInput struct {
	Amount      int
	Unit        string
	Matrix      []ApprovalSLARowInput
	WriteDueTo  string
	OnChange    string
	Notify      *ApprovalAssignmentInput
	Unsupported []string
}

// ApprovalWaitSLAKeys is the closed key set of the `sla` object on the wire — the engine's own list.
var ApprovalWaitSLAKeys = bpm.WaitSLAKeys

// buildWaitNode fills the wait-only half of a node and judges it by the engine's rule. node already
// carries the step half; at() points at graph.nodes[i].<f>.
func buildWaitNode(i int, node bpm.Node, in ApprovalNodeInput) (bpm.Node, error) {
	at := func(f string) string { return fmt.Sprintf("graph.nodes[%d].%s", i, f) }
	if in.Until == nil {
		return node, invalidField("APPROVAL_STEP_INVALID", at("until"), "bước chờ %d cần điều kiện `until` — hồ sơ phải đạt điều kiện đó để đi tiếp", i+1)
	}
	noEdge := func(where string) func(string) error {
		return func(op string) error {
			return invalidField("APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED", at(where),
				"«vừa đổi»/«vừa đổi thành» (%q) không dùng được ở bước chờ — nó xét giá trị hiện tại của hồ sơ; dùng «=»/«≠»", op)
		}
	}
	until, err := buildApprovalCondition(in.Until.Match, in.Until.Rules, noEdge("until"))
	if err != nil {
		return node, withField(err, at("until"))
	}
	node.Until = until
	if s := in.SLA; s != nil {
		if len(s.Unsupported) > 0 {
			return node, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", at("sla."+s.Unsupported[0]),
				"bước chờ %d có khoá sla.%s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này (sẽ mở ở lát sau)", i+1, strings.Join(s.Unsupported, ", sla."))
		}
		w := &bpm.WaitSLA{Amount: s.Amount, Unit: strings.TrimSpace(s.Unit), WriteDueTo: strings.TrimSpace(s.WriteDueTo), OnChange: strings.TrimSpace(s.OnChange)}
		for j, row := range s.Matrix {
			when, err := buildApprovalCondition(row.When.Match, row.When.Rules, noEdge(fmt.Sprintf("sla.matrix[%d].when", j)))
			if err != nil {
				return node, withField(err, at(fmt.Sprintf("sla.matrix[%d].when", j)))
			}
			w.Matrix = append(w.Matrix, bpm.SLARow{When: when, Amount: row.Amount, Unit: strings.TrimSpace(row.Unit)})
		}
		if n := s.Notify; n != nil {
			if len(n.Unsupported) > 0 {
				return node, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", at("sla.notify."+n.Unsupported[0]),
					"sla.notify của bước chờ %d có khoá %s ngoài type/id/field", i+1, strings.Join(n.Unsupported, ", "))
			}
			w.Notify = &bpm.Assignment{Type: bpm.AssignmentType(strings.TrimSpace(n.Type)), ID: strings.TrimSpace(n.ID),
				Field: strings.TrimSpace(n.Field), Strategy: strings.TrimSpace(n.Strategy)}
		}
		node.SLA = w
	}
	if f, why := bpm.CheckWait(node); f != "" {
		return node, waitProblem(i, f, why, node)
	}
	return node, nil
}

// waitProblem is the coded 422 for what bpm.CheckWait found at field (relative to the node).
func waitProblem(i int, field, reason string, n bpm.Node) error {
	code := "APPROVAL_STEP_INVALID"
	switch {
	case field == "sla.notify":
		code = "APPROVAL_SLA_NOTIFY_REQUIRED"
	case strings.HasSuffix(field, ".unit"):
		code = "APPROVAL_SLA_UNIT_INVALID"
	case strings.HasSuffix(field, ".amount") && n.SLA != nil && anyWaitAmountAbove(n.SLA):
		code = "APPROVAL_SLA_AMOUNT_TOO_LARGE"
	case field == "sla.writeDueTo":
		code = "APPROVAL_SLA_TARGET_FIELD_INVALID"
	}
	return invalidField(code, fmt.Sprintf("graph.nodes[%d].%s", i, field), "bước chờ %d: %s", i+1, reason)
}

func anyWaitAmountAbove(s *bpm.WaitSLA) bool {
	if s.Amount > bpm.MaxWaitAmount {
		return true
	}
	for _, r := range s.Matrix {
		if r.Amount > bpm.MaxWaitAmount {
			return true
		}
	}
	return false
}

// checkWaitRefs judges what a wait node names against the entity: the fields `until` and the matrix
// read exist, writeDueTo is a datetime field that is not computed, and sla.notify points at something
// that exists (a field must be a user-type field — the same rule an assignment field obeys).
func (s *ApprovalService) checkWaitRefs(ctx context.Context, tenantID, entity, at string, i int, n ApprovalNodeInput, fields func() (map[string]string, error)) error {
	set, err := fields()
	if err != nil {
		return err
	}
	read := func(c *ApprovalConditionInput, where string) error {
		if c == nil || set == nil {
			return nil
		}
		for k, r := range c.Rules {
			if f := strings.TrimSpace(r.Field); !fieldKnown(set, f) {
				return invalidField("APPROVAL_FIELD_UNKNOWN", fmt.Sprintf("%s[%d].%s.rules[%d].field", at, i, where, k),
					"điều kiện của bước chờ %d đọc trường %q không có trong đối tượng %s", i+1, f, entity)
			}
		}
		return nil
	}
	if err := read(n.Until, "until"); err != nil {
		return err
	}
	sla := n.SLA
	if sla == nil {
		return nil
	}
	for j := range sla.Matrix {
		if err := read(&sla.Matrix[j].When, fmt.Sprintf("sla.matrix[%d].when", j)); err != nil {
			return err
		}
	}
	if f := strings.TrimSpace(sla.WriteDueTo); f != "" && set != nil {
		ft, ok := set[f]
		where := fmt.Sprintf("%s[%d].sla.writeDueTo", at, i)
		switch {
		case !ok:
			return invalidField("APPROVAL_SLA_TARGET_FIELD_INVALID", where, "trường hạn %q của bước chờ %d không có trong đối tượng %s", f, i+1, entity)
		case ft != "datetime":
			return invalidField("APPROVAL_SLA_TARGET_FIELD_INVALID", where,
				"trường hạn %q của bước chờ %d kiểu %s — chỉ ghi được vào trường ngày giờ (datetime)", f, i+1, ft)
		}
	}
	if nf := sla.Notify; nf != nil {
		return s.checkAssignmentRef(ctx, tenantID, entity, fmt.Sprintf("%s[%d].sla.notify", at, i), i+1, nf, fields)
	}
	return nil
}

// ruleLister is the slice of the workflow repository DueFieldConflicts needs.
type ruleLister interface {
	List(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
}

// WithDueWriters arms the "two writers of one deadline field" warning (DueFieldConflicts). Optional.
func (s *ApprovalService) WithDueWriters(l ruleLister) *ApprovalService {
	s.rules = l
	return s
}

// DueFieldConflicts warns, at save time, about a wait node whose writeDueTo field a LIVE automation
// rule of the same entity also writes with set_deadline: two writers of one promise overwrite each
// other (specs/043 plan risk 12). A warning, not a refusal — tighten_only rules cooperate by design.
// Empty when nothing conflicts or no rule lister is wired.
func (s *ApprovalService) DueFieldConflicts(ctx context.Context, tenantID, entity string, g *ApprovalGraphInput) ([]string, error) {
	if s.rules == nil || g == nil {
		return nil, nil
	}
	want := map[string]string{} // field → node label
	for i, n := range g.Nodes {
		if bpm.NodeKind(strings.TrimSpace(n.Kind)) == bpm.NodeWait && n.SLA != nil && strings.TrimSpace(n.SLA.WriteDueTo) != "" {
			label := strings.TrimSpace(n.Name)
			if label == "" {
				label = fmt.Sprintf("bước %d", i+1)
			}
			want[strings.TrimSpace(n.SLA.WriteDueTo)] = label
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	rules, err := s.rules.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.DueFieldConflicts: %w", err)
	}
	var out []string
	for _, r := range rules {
		if !r.IsActive || r.EntityName != entity {
			continue
		}
		var actions []workflow.Action
		if err := json.Unmarshal(r.Actions, &actions); err != nil {
			continue // a rule that does not decode writes nothing anyone can name
		}
		for _, a := range actions {
			if a.Type != workflow.ActionSetDeadline {
				continue
			}
			if label, ok := want[strings.TrimSpace(a.Field)]; ok {
				out = append(out, fmt.Sprintf("Trường %q cũng được luật tự động «%s» ghi bằng set_deadline — hai nơi cùng ghi hạn (%s) sẽ đè lên nhau; chỉ giữ một nơi, hoặc đặt luật đó ở chế độ tighten_only.", a.Field, r.Name, label))
			}
		}
	}
	return out, nil
}
