package services

// visibleWhen — a layout cell's display condition (specs/054-layout-visible-when):
//
//	{"field": "tax_code", "visibleWhen": {"field": "customer_type", "op": "eq", "value": "business"}}
//
// Four consumers, one grammar: the config-time validator (validateVisibleWhenCells, via
// validateLayout — so SaveLayout, UpdateLayout and the AI Builder's propose_layout all agree), the
// shared evaluator (fieldVisible — the FE runs a TS port of it against the SAME golden file,
// testdata/visible_when_cases.json), the create path (a required field the form hides is not
// demanded) and the serve-time prune (a condition on a controller the caller is not served is
// dropped).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
)

const codeLayoutVisibleWhenInvalid = "LAYOUT_VISIBLE_WHEN_INVALID"

// maxVisibleWhenListValues bounds an in/notIn operand — a payload bound like the layout's others.
const maxVisibleWhenListValues = 50

// visibleWhen is one decoded condition. Value is a scalar (string/float64/bool) for eq/neq, a
// []any of scalars for in/notIn, and nil for empty/notEmpty.
type visibleWhen struct {
	Field string
	Op    string
	Value any
}

// Operand shapes per operator.
const (
	vwOperandNone = iota
	vwOperandScalar
	vwOperandList
)

// visibleWhenOps is the closed operator set. Anything else is rejected at config time, so a
// stored condition never carries an operator the FE evaluator does not know.
var visibleWhenOps = map[string]int{
	"eq": vwOperandScalar, "neq": vwOperandScalar,
	"in": vwOperandList, "notIn": vwOperandList,
	"empty": vwOperandNone, "notEmpty": vwOperandNone,
}

// visibleWhenControllerTypes are the field types a condition may read: single-valued types whose
// equality means the same thing in Go and in the browser (no currency objects, no multi-value
// arrays, no dates whose string form depends on who formatted them).
var visibleWhenControllerTypes = map[string]struct{}{
	"picklist": {}, "boolean": {}, "text": {}, "email": {}, "number": {}, "rating": {},
}

var knownVisibleWhenKeys = map[string]struct{}{"field": {}, "op": {}, "value": {}}

// decodeVisibleWhen reads a condition structurally. reason is "" when it parses; otherwise it
// says what is wrong, in the words the config-time 422 uses. Field names are taken VERBATIM (no
// TrimSpace-then-lookup): " customer_type" must fail the lookup rather than validate against
// "customer_type" and then never match anything at render time.
func decodeVisibleWhen(raw any) (vc visibleWhen, reason string) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return vc, "phải là object {field, op, value}"
	}
	for k := range obj {
		if _, ok := knownVisibleWhenKeys[k]; !ok {
			return vc, fmt.Sprintf("khoá không rõ %q (chỉ field, op, value)", k)
		}
	}
	field, _ := obj["field"].(string)
	if strings.TrimSpace(field) == "" {
		return vc, "field phải là tên trường điều khiển"
	}
	op, _ := obj["op"].(string)
	shape, ok := visibleWhenOps[op]
	if !ok {
		return vc, fmt.Sprintf("op %q không hợp lệ (eq, neq, in, notIn, empty, notEmpty)", op)
	}
	value, hasValue := obj["value"]
	switch shape {
	case vwOperandNone:
		if hasValue {
			return vc, fmt.Sprintf("op %q không nhận value", op)
		}
	case vwOperandScalar:
		if !isVisibleWhenScalar(value) {
			return vc, fmt.Sprintf("op %q cần value là MỘT giá trị không rỗng (chuỗi, số hoặc true/false) — hỏi rỗng thì dùng op empty", op)
		}
	case vwOperandList:
		list, ok := value.([]any)
		if !ok || len(list) == 0 || len(list) > maxVisibleWhenListValues {
			return vc, fmt.Sprintf("op %q cần value là danh sách 1..%d giá trị", op, maxVisibleWhenListValues)
		}
		for _, v := range list {
			if !isVisibleWhenScalar(v) {
				return vc, fmt.Sprintf("op %q: mỗi phần tử của value phải là một giá trị không rỗng (chuỗi, số hoặc true/false)", op)
			}
		}
	}
	return visibleWhen{Field: field, Op: op, Value: value}, ""
}

// parseVisibleWhen is decodeVisibleWhen for the tolerant readers (create path, golden tests):
// a condition that does not parse is skipped, never guessed at.
func parseVisibleWhen(raw any) (visibleWhen, bool) {
	vc, reason := decodeVisibleWhen(raw)
	return vc, reason == ""
}

func isVisibleWhenScalar(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case float64, bool:
		return true
	}
	return false
}

// cellCondition is a visibleWhen met during the section walk, judged once every cell is known
// (the controller must be ON the layout, and cycles need the whole map).
type cellCondition struct {
	target       string
	raw          any
	cellRequired bool
	at           string
}

// validateVisibleWhenCells is the config-time check (specs/054 FR-2..FR-6). onLayout = every field
// with a cell in this view; liveFields = live user fields (the validator's own input).
func validateVisibleWhenCells(pending []cellCondition, onLayout map[string]bool, liveFields []metadata.FieldDefinition) error {
	if len(pending) == 0 {
		return nil
	}
	byName := make(map[string]metadata.FieldDefinition, len(liveFields))
	for _, f := range liveFields {
		byName[f.Name] = f
	}
	conds := make(map[string]visibleWhen, len(pending))
	for _, p := range pending {
		vc, reason := decodeVisibleWhen(p.raw)
		if reason != "" {
			return invalid(codeLayoutVisibleWhenInvalid, "%s: %s", p.at, reason)
		}
		if vc.Field == p.target {
			return invalid(codeLayoutVisibleWhenInvalid, "%s: trường %q không thể tự điều khiển chính nó", p.at, p.target)
		}
		ctrl, ok := byName[vc.Field]
		if !ok {
			return invalid(codeLayoutVisibleWhenInvalid, "%s: trường điều khiển %q không tồn tại hoặc đã xoá", p.at, vc.Field)
		}
		if !onLayout[vc.Field] {
			return invalid(codeLayoutVisibleWhenInvalid,
				"%s: trường điều khiển %q phải có ô trong cùng bố cục (người dùng phải thấy và đổi được nó)", p.at, vc.Field)
		}
		if _, ok := visibleWhenControllerTypes[ctrl.FieldType]; !ok {
			return invalid(codeLayoutVisibleWhenInvalid,
				"%s: trường %q kiểu %q không dùng làm điều kiện được (chỉ picklist, boolean, text, email, number, rating)",
				p.at, vc.Field, ctrl.FieldType)
		}
		if err := checkVisibleWhenOperand(vc, ctrl, p.at); err != nil {
			return err
		}
		if p.cellRequired || isFieldRequired(p.target, liveFields) {
			return invalid(codeLayoutVisibleWhenInvalid,
				"%s: trường %q là bắt buộc — trường bắt buộc không được ẩn theo điều kiện (bỏ bắt buộc hoặc bỏ điều kiện)",
				p.at, p.target)
		}
		conds[p.target] = vc
	}
	// FR-5: no controller chain may lead back to where it started. A chain has at most len(conds)
	// links, so that many steps either end it or find the loop.
	for _, p := range pending {
		name := p.target
		for i := 0; i < len(conds); i++ {
			c, ok := conds[name]
			if !ok {
				break
			}
			if name = c.Field; name == p.target {
				return invalid(codeLayoutVisibleWhenInvalid, "%s: điều kiện của %q vòng lại chính nó qua chuỗi trường điều khiển", p.at, p.target)
			}
		}
	}
	return nil
}

// checkVisibleWhenOperand holds every operand to the controller's OWN record-time type check
// (checkType): a picklist value outside options.values ("Business" for "business"), a string for
// a number, "true" for a boolean — each would save clean and then never match (luật B6).
func checkVisibleWhenOperand(vc visibleWhen, ctrl metadata.FieldDefinition, at string) error {
	var operands []any
	switch v := vc.Value.(type) {
	case nil:
		return nil
	case []any:
		operands = v
	default:
		operands = []any{v}
	}
	specs, err := toFieldSpecs([]metadata.FieldDefinition{ctrl})
	if err != nil {
		return fmt.Errorf("services.checkVisibleWhenOperand: %w", err)
	}
	for _, v := range operands {
		if terr := checkType(specs[0], v); terr != nil {
			why := terr.Error()
			var ve *ValidationError
			if errors.As(terr, &ve) {
				why = ve.Msg
			}
			return invalid(codeLayoutVisibleWhenInvalid, "%s: giá trị %v không phải một giá trị của trường %q (%s)", at, v, ctrl.Name, why)
		}
	}
	return nil
}

// conditionHolds evaluates one condition on values (FR-7). "Empty" is the required check's own
// notion (isEmpty); equality is strict on JSON type — "5" is not 5 — so the FE can mirror it with
// ===.
func conditionHolds(c visibleWhen, values map[string]any) bool {
	v := values[c.Field]
	switch c.Op {
	case "empty":
		return isEmpty(v)
	case "notEmpty":
		return !isEmpty(v)
	case "eq":
		return !isEmpty(v) && sameScalar(v, c.Value)
	case "neq":
		return isEmpty(v) || !sameScalar(v, c.Value)
	case "in", "notIn":
		in := false
		if list, ok := c.Value.([]any); ok && !isEmpty(v) {
			for _, x := range list {
				if sameScalar(v, x) {
					in = true
					break
				}
			}
		}
		return in == (c.Op == "in")
	}
	return true // unreachable after decode; showing is the safe side (required stays demanded)
}

func sameScalar(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// fieldVisible reports whether name shows given conds (target → condition) and values (FR-7).
// Transitive: a field shows only when its condition holds AND its controller shows — so a draft
// value kept in a hidden controller never reveals anything. A cycle (only reachable through rows
// written straight to the DB) ends after len(conds)+1 links and counts as visible.
func fieldVisible(name string, conds map[string]visibleWhen, values map[string]any) bool {
	for steps := 0; steps <= len(conds); steps++ {
		c, ok := conds[name]
		if !ok {
			return true
		}
		if !conditionHolds(c, values) {
			return false
		}
		name = c.Field
	}
	return true
}

// layoutConditions collects target → condition from a decoded edit/detail layout, keeping only
// conditions that parse and whose controller passes keep. Tolerant: never errors.
func layoutConditions(layout map[string]any, keep func(string) bool) map[string]visibleWhen {
	conds := map[string]visibleWhen{}
	sections, _ := layout["sections"].([]any)
	for _, secRaw := range sections {
		sec, _ := secRaw.(map[string]any)
		cells, _ := sec["fields"].([]any)
		for _, cellRaw := range cells {
			cell, ok := cellRaw.(map[string]any)
			if !ok {
				continue
			}
			target, _ := cell["field"].(string)
			if vc, ok := parseVisibleWhen(cell["visibleWhen"]); ok && target != "" && keep(vc.Field) {
				conds[target] = vc
			}
		}
	}
	return conds
}

// dropUnservedCondition returns cell without its visibleWhen when the controller fails keep (dead,
// or hidden from the caller by field-ACL — FR-9): the renderer is never handed a condition it
// cannot evaluate, nor the name of a field it may not see, nor a way to infer that field's value
// from whether another cell shows. Copy-on-write; a cell without a condition comes back as is.
func dropUnservedCondition(cell map[string]any, keep func(string) bool) map[string]any {
	raw, has := cell["visibleWhen"]
	if !has {
		return cell
	}
	if c, ok := raw.(map[string]any); ok {
		if ctrl, _ := c["field"].(string); keep(ctrl) {
			return cell
		}
	}
	out := cloneMapExcept(cell)
	delete(out, "visibleWhen")
	return out
}

// layoutReader is the edit-layout read the create path needs (FR-8). *metadata.LayoutRepository
// satisfies it; cmd/server/main.go wires it with WithLayoutReader.
type layoutReader interface {
	GetByEntity(ctx context.Context, tenantID, entityName string) ([]metadata.LayoutDefinition, error)
}

var _ layoutReader = (*metadata.LayoutRepository)(nil)

// WithLayoutReader wires the edit-layout read behind the create path's visibleWhen rule. Unwired
// (unit tests, tools) = every required field is demanded, exactly as before specs/054.
func (s *RecordService) WithLayoutReader(r layoutReader) *RecordService {
	s.layouts = r
	return s
}

// hiddenRequiredRejudge returns the create path's second look at a RECORD_REQUIRED_MISSING (FR-8):
// when the missing field carries a condition in the entity's edit layout, the payload is judged
// again with that field required only if the condition shows it. The layout loads lazily — only
// once a required field is actually missing, at most once per returned func (a bulk batch shares
// one) — so a create that passes ValidateRecord costs no extra query. Every other error, and every
// missing field without a condition, is returned untouched.
func (s *RecordService) hiddenRequiredRejudge(ctx context.Context, tenantID, entity string, specs []FieldSpec) func(payload map[string]any, err error) (map[string]any, error) {
	var loaded bool
	var conds map[string]visibleWhen
	return func(payload map[string]any, err error) (map[string]any, error) {
		var ve *ValidationError
		if s.layouts == nil || !errors.As(err, &ve) || ve.Code != "RECORD_REQUIRED_MISSING" {
			return nil, err
		}
		if !loaded {
			loaded = true
			conds = s.editLayoutConditions(ctx, tenantID, entity, specs)
		}
		if _, conditional := conds[ve.Field]; !conditional {
			return nil, err
		}
		return validateRecordWithVisibility(specs, payload, conds)
	}
}

// editLayoutConditions reads the entity's stored edit layout and returns its conditions whose
// controller is still a live field (a condition on a deleted controller is ignored, as the
// serve-time prune drops it). A read/decode failure logs and returns nil: the caller keeps its
// RECORD_REQUIRED_MISSING — fail closed, nothing is exempted unverified.
func (s *RecordService) editLayoutConditions(ctx context.Context, tenantID, entity string, specs []FieldSpec) map[string]visibleWhen {
	warn := func(msg string, err error) {
		if s.logger != nil {
			s.logger.Warn("services.RecordService.editLayoutConditions: "+msg+"; keeping RECORD_REQUIRED_MISSING",
				zap.String("tenant_id", tenantID), zap.String("entity_name", entity), zap.Error(err))
		}
	}
	rows, err := s.layouts.GetByEntity(ctx, tenantID, entity)
	if err != nil {
		warn("layout read failed", err)
		return nil
	}
	live := make(map[string]struct{}, len(specs))
	for _, sp := range specs {
		live[sp.Name] = struct{}{}
	}
	conds, err := storedEditConditions(rows, func(name string) bool { _, ok := live[name]; return ok })
	if err != nil {
		warn("stored edit layout does not decode", err)
		return nil
	}
	return conds
}

// storedEditConditions is the visibleWhen conditions of the entity's stored EDIT view whose
// controller `live` still accepts (nil when there is no edit row). Shared by the create path
// (editLayoutConditions) and the field-edit guard (FieldService.guardRequiredHasCondition).
func storedEditConditions(rows []metadata.LayoutDefinition, live func(string) bool) (map[string]visibleWhen, error) {
	for _, row := range rows {
		if row.ViewType != LayoutViewEdit {
			continue
		}
		var layout map[string]any
		if err := json.Unmarshal([]byte(row.LayoutJSON), &layout); err != nil {
			return nil, fmt.Errorf("services.storedEditConditions: stored edit layout does not decode: %w", err)
		}
		return layoutConditions(layout, live), nil
	}
	return nil, nil
}

// createImage is THE image a create is judged on (specs/054 FR-7 "one image"): the payload as sent
// plus, for every field it omits or sends blank, the field's default coerced to its type — exactly
// the record ValidateRecord would store — with a conditional field's required flag set aside so
// the caller can decide it from visibility. The FE judges visibility on this same image in create
// mode (buildPayload's wire body + defaults, golden createCases), NOT on what its widgets show: the
// create form prefills defaults since #275 and a blank required number is omitted from the payload
// (never sent as 0), so the server default applies. The caller's specs are never mutated.
func createImage(specs []FieldSpec, payload map[string]any, conds map[string]visibleWhen) (map[string]any, error) {
	relaxed := make([]FieldSpec, len(specs))
	copy(relaxed, specs)
	for i := range relaxed {
		if _, ok := conds[relaxed[i].Name]; ok {
			relaxed[i].IsRequired = false
		}
	}
	return ValidateRecord(relaxed, payload)
}

// validateRecordWithVisibility is ValidateRecord where a required field carrying a condition is
// demanded only when the condition shows it on the create image (createImage).
func validateRecordWithVisibility(specs []FieldSpec, payload map[string]any, conds map[string]visibleWhen) (map[string]any, error) {
	clean, err := createImage(specs, payload, conds)
	if err != nil {
		return nil, err
	}
	for _, f := range specs {
		if _, ok := conds[f.Name]; ok && f.IsRequired && isEmpty(clean[f.Name]) && fieldVisible(f.Name, conds, clean) {
			return nil, invalidField("RECORD_REQUIRED_MISSING", f.Name, "field %q là bắt buộc", f.Name)
		}
	}
	return clean, nil
}
