package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/048 — config-time rules for "before" workflows (before_save / before_delete). A before rule
// runs synchronously inside every save or delete of its entity (S1b onwards), so the shape that
// keeps it fast, side-effect free and never silently dead is enforced here, when the rule itself is
// saved. checkBeforeRule is part of the ONE buildWorkflowRule body (so the AI Builder's
// ValidateWorkflowConfig and the pack installer see the same verdicts); the DB-dependent half
// (active-rule cap) runs on the apply path in checkBeforeActivation.

const (
	// maxBeforeActions caps the actions of one before rule (FR-006). after_* rules are not capped.
	maxBeforeActions = 10
	// maxActiveBeforeRules caps the ACTIVE before rules per (tenant, entity, trigger) (FR-007).
	maxActiveBeforeRules = 50
	// maxVetoMessageRunes bounds a veto message (FR-004) — it is shown verbatim under a form input.
	maxVetoMessageRunes = 500
)

// beforeAllowedActions is the final trigger → action table (FR-004/FR-005): nothing with an outside
// effect (mail, HTTP, a notification row, another record, an owner/deadline stamp) may run inside a
// save that can still be vetoed. before_delete can only refuse.
var beforeAllowedActions = map[workflow.Trigger]map[string]bool{
	workflow.TriggerBeforeSave:   {workflow.ActionVeto: true, workflow.ActionFieldUpdate: true, workflow.ActionNormalize: true},
	workflow.TriggerBeforeDelete: {workflow.ActionVeto: true},
}

// beforeMutationShipped lists the value-changing before_save actions whose runtime is on main: S4
// shipped field_update, S5 normalize. An action outside it is refused rather than stored and never run.
var beforeMutationShipped = map[string]bool{workflow.ActionFieldUpdate: true, workflow.ActionNormalize: true}

// normalizeTarget are the field types a normalize may rewrite (FR-009): free text a person typed.
// rich_text is left out on purpose — it is HTML, and casing it rewrites entities (&Agrave; is À,
// &agrave; is à) and markup; a picklist value is a code the validator checks against its list.
var normalizeTarget = map[string]bool{"text": true, "email": true}

// beforeTargetUnsupported are the field types a before_save field_update may not write (FR-009): a
// link or a person must point at something that exists (a probe the pure hook does not make), a file
// is a catalogued blob, and a computed value or a sequence belongs to the engine.
var beforeTargetUnsupported = map[string]bool{
	"relation": true, "polymorphic_relation": true, "user": true, "file": true, "image": true,
	"formula": true, "lookup": true, "rollup": true, "auto_number": true,
}

// beforeTextTarget are the free-text field types a {{field}} template may be rendered into.
var beforeTextTarget = map[string]bool{"text": true, "email": true, "rich_text": true}

// beforeTemplateSource are the field types a {{placeholder}} may read: stored on the record, with a
// value that renders as text. Everything else renders "" at every save — a computed value is not in
// the record the rule reads (lookup/rollup are read-time; a formula is recomputed only after the
// rules ran), an object or a list (currency, composite, multi_select, file…) has no text form — or
// renders an id nobody can read (relation, user). auto_number is assigned after the rules on a create,
// so it is readable only by a rule that runs on update alone (checkBeforeFieldUpdate).
var beforeTemplateSource = map[string]bool{
	"text": true, "email": true, "rich_text": true, "number": true, "rating": true, "boolean": true,
	"date": true, "datetime": true, "picklist": true, "auto_number": true,
}

// checkBeforeRule validates the before-specific shape of a rule whose trigger is already valid.
// For after_*/scheduled/date_offset rules it only refuses the before-only keys (saveOn, the
// veto/normalize actions, a field_update's onlyIfEmpty/valueSource) and an edge operator the trigger
// can never satisfy, and otherwise changes nothing.
// saveOn is the raw condition.saveOn; fieldTypes is the entity's field → type map (nil = the caller
// cannot list fields, the type checks are skipped like every other WF-01 guard).
func checkBeforeRule(t workflow.Trigger, saveOn string, rules []WorkflowRuleInput, timer *WorkflowTimerInput, actions []WorkflowActionInput, fieldTypes map[string]string) error {
	saveOn = strings.TrimSpace(saveOn)
	switch {
	case saveOn != "" && t != workflow.TriggerBeforeSave:
		return invalid("WORKFLOW_SAVE_ON_INVALID", "condition.saveOn chỉ dùng cho trigger before_save (trigger hiện tại: %q)", t)
	case saveOn != "" && saveOn != workflow.SaveOnCreate && saveOn != workflow.SaveOnUpdate:
		return invalid("WORKFLOW_SAVE_ON_INVALID", "condition.saveOn không hợp lệ: %q (create, update, hoặc bỏ trống = cả hai)", saveOn)
	}
	// "changed"/"changedTo" compare against the record as it stood before this write, and only an
	// update has one: after_update, or a before_save rule that also runs on updates. A create has
	// none, a delete none worth comparing, a scheduled/date_offset run none at all — the dispatcher
	// evaluates those with a nil before-image, so the rule would be stored active and skipped
	// forever (FR-003). Refused on every trigger, not just the before ones: the AI Builder invites
	// these operators and can land them on after_create/after_delete.
	if t != workflow.TriggerAfterUpdate && (t != workflow.TriggerBeforeSave || saveOn == workflow.SaveOnCreate) {
		for _, r := range rules {
			if op := strings.TrimSpace(r.Op); op == workflow.OpChanged || op == workflow.OpChangedTo {
				field := strings.TrimSpace(r.Field)
				return &ValidationError{Code: "WORKFLOW_CONDITION_NEVER_TRUE", Field: field,
					Msg:     fmt.Sprintf("điều kiện %q trên %q không bao giờ đúng với trigger %s — chỉ lần SỬA bản ghi mới có giá trị cũ để so; dùng \"bằng\" thay cho \"vừa đổi\"", op, field, t),
					Details: map[string]any{"field": field, "op": op, "trigger": string(t)}}
			}
		}
	}
	if !workflow.IsBeforeTrigger(t) {
		for _, a := range actions {
			typ := strings.TrimSpace(a.Type)
			if typ == workflow.ActionVeto || typ == workflow.ActionNormalize {
				return actionNotAllowed(typ, t, "chỉ dùng cho quy tắc trước khi lưu / trước khi xoá")
			}
			// Refused rather than dropped: an after_* field_update always overwrites with a constant.
			if typ == workflow.ActionFieldUpdate && (a.OnlyIfEmpty || strings.TrimSpace(a.ValueSource) != "") {
				return invalid("WORKFLOW_ACTION_INVALID",
					"onlyIfEmpty / valueSource của field_update chỉ dùng cho quy tắc trước khi lưu (trigger hiện tại: %q)", t)
			}
		}
		return nil
	}
	if timer != nil {
		return invalid("WORKFLOW_BEFORE_TIMER_INVALID", "quy tắc %s chạy ngay lúc lưu — không nhận condition.timer", t)
	}
	if len(actions) > maxBeforeActions {
		return &ValidationError{Code: "WORKFLOW_TOO_MANY_ACTIONS",
			Msg:     fmt.Sprintf("một quy tắc %s có tối đa %d hành động (đang có %d)", t, maxBeforeActions, len(actions)),
			Details: map[string]any{"max": maxBeforeActions}}
	}
	vetoes := 0
	for _, a := range actions {
		typ := strings.TrimSpace(a.Type)
		if !beforeAllowedActions[t][typ] {
			return actionNotAllowed(typ, t, "quy tắc chạy lúc lưu không được có tác dụng ra ngoài bản ghi")
		}
		if typ == workflow.ActionVeto {
			vetoes++
		}
	}
	if vetoes > 0 && len(actions) > 1 {
		return invalid("WORKFLOW_BEFORE_ACTIONS_MIXED",
			"quy tắc chặn có đúng một hành động chặn và không kèm hành động sửa giá trị — tách thành hai quy tắc")
	}
	for _, a := range actions {
		typ := strings.TrimSpace(a.Type)
		if typ != workflow.ActionVeto && !beforeMutationShipped[typ] {
			return actionNotAllowed(typ, t, "chưa hỗ trợ ở phiên bản này")
		}
		switch typ {
		case workflow.ActionFieldUpdate:
			if err := checkBeforeFieldUpdate(a, fieldTypes, saveOn); err != nil {
				return err
			}
		case workflow.ActionNormalize:
			if err := checkBeforeNormalize(a, fieldTypes); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkBeforeNormalize is the config check of a normalize action (FR-009): one to four distinct known
// operations (workflow.Normalize*), on a free-text field of the entity (normalizeTarget). The keys a
// field_update reads (onlyIfEmpty, valueSource) mean nothing here and are refused, not dropped.
func checkBeforeNormalize(a WorkflowActionInput, fieldTypes map[string]string) error {
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return invalid("WORKFLOW_ACTION_INVALID", "action normalize cần 'field'")
	}
	if a.OnlyIfEmpty || strings.TrimSpace(a.ValueSource) != "" {
		return invalid("WORKFLOW_ACTION_INVALID", "onlyIfEmpty / valueSource chỉ dùng cho field_update, không dùng cho normalize")
	}
	if n := len(a.Ops); n == 0 || n > workflow.MaxNormalizeOps {
		return invalidField("NORMALIZE_OPS_INVALID", field,
			"chuẩn hoá field %q cần 1–%d phép (đang có %d)", field, workflow.MaxNormalizeOps, n)
	}
	seen := make(map[string]bool, len(a.Ops))
	for _, raw := range a.Ops {
		op := strings.TrimSpace(raw)
		if !workflow.ValidNormalizeOp(op) {
			return invalidField("NORMALIZE_OPS_INVALID", field,
				"phép chuẩn hoá %q không hợp lệ (trim, lower, upper, collapse_spaces)", raw)
		}
		if seen[op] {
			return invalidField("NORMALIZE_OPS_INVALID", field, "phép chuẩn hoá %q lặp lại trên field %q", op, field)
		}
		seen[op] = true
	}
	if fieldTypes == nil {
		return nil
	}
	ft, known := fieldTypes[field]
	if !known {
		return invalidField("RECORD_UNKNOWN_FIELD", field, "field %q không tồn tại trong metadata", field)
	}
	if !normalizeTarget[ft] {
		return invalidField("NORMALIZE_FIELD_TYPE_INVALID", field,
			"chuẩn hoá chỉ áp cho field chữ (text, email); %q là %s", field, ft)
	}
	return nil
}

// checkBeforeFieldUpdate is the pure half of a before_save field_update (FR-008/FR-009): a target the
// hook may write, a known value source, a template only into free text and only over placeholders it
// can render (beforeTemplateSource), a copied field of the same type. Whether a constant fits its
// field (type, range, picklist option, required) needs the full field definitions — checkFieldUpdateValues,
// on the apply path. saveOn is the rule's trimmed condition.saveOn.
func checkBeforeFieldUpdate(a WorkflowActionInput, fieldTypes map[string]string, saveOn string) error {
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return invalid("WORKFLOW_ACTION_INVALID", "action field_update cần 'field'")
	}
	source := strings.TrimSpace(a.ValueSource)
	switch source {
	case "", workflow.ValueSourceConstant, workflow.ValueSourceField, workflow.ValueSourceTemplate:
	default:
		return beforeValueInvalid(field, "value_source_invalid",
			"valueSource %q không hợp lệ (constant, field, template)", source)
	}
	if fieldTypes == nil {
		return nil
	}
	ft, known := fieldTypes[field]
	if !known {
		return invalidField("RECORD_UNKNOWN_FIELD", field, "field %q không tồn tại trong metadata", field)
	}
	if beforeTargetUnsupported[ft] {
		return invalidField("WORKFLOW_BEFORE_TARGET_UNSUPPORTED", field,
			"quy tắc trước khi lưu không tự điền được field %q (%s) — liên kết, người dùng, tệp, trường tính toán và mã tự tăng do engine quản lý", field, ft)
	}
	switch source {
	case workflow.ValueSourceTemplate:
		tmpl, isText := a.Value.(string)
		if !isText || !beforeTextTarget[ft] {
			return beforeValueInvalid(field, "template_text_only",
				"mẫu chữ {{field}} chỉ điền được vào field chữ (text, email, rich_text); %q là %s", field, ft)
		}
		if strings.TrimSpace(tmpl) == "" {
			return beforeValueInvalid(field, "template_empty", "mẫu chữ cho field %q đang trống", field)
		}
		for _, name := range workflow.TemplatePlaceholders(tmpl) {
			srcType, ok := fieldTypes[name]
			if !ok {
				return beforeValueInvalid(field, "template_source_unknown",
					"mẫu chữ đọc {{%s}} — không phải field của đối tượng này (mẫu chữ không đọc qua liên kết)", name)
			}
			if !beforeTemplateSource[srcType] || (srcType == "auto_number" && saveOn != workflow.SaveOnUpdate) {
				return beforeValueInvalid(field, "template_source_unsupported",
					"mẫu chữ không đọc được {{%s}} (%s) — chỉ field lưu trên bản ghi có giá trị chữ, số, ngày hoặc lựa chọn; mã tự tăng chỉ khi quy tắc chỉ áp khi sửa", name, srcType)
			}
		}
	case workflow.ValueSourceField:
		name, _ := a.Value.(string)
		name = strings.TrimSpace(name)
		srcType, ok := fieldTypes[name]
		if !ok {
			return beforeValueInvalid(field, "source_unknown", "field nguồn %q không tồn tại trong metadata", name)
		}
		if srcType != ft {
			return beforeValueInvalid(field, "source_type_mismatch",
				"field nguồn %q (%s) khác kiểu field đích %q (%s)", name, srcType, field, ft)
		}
	}
	return nil
}

// checkFieldUpdateValues is the DB-dependent half (apply path, like checkCreateRecordTargets): every
// constant a field_update writes onto the record that triggered the rule must pass the SAME validator a
// record save runs on that field — type, range, picklist option, and never blank a required field.
// On before_save (specs/048 S4) a {{field}} template or a copied field has no value yet; the save
// validates it (a refused value is a failed run, FR-022). On every other trigger the value is written
// as is by WorkflowRecordMutator.validateFieldUpdate → ValidatePartialRecord, so without this check a
// rule saved green and failed on every firing (queue #282: "100" into a number field); the refusal
// carries that validator's own code, as a related target's does (checkRelatedFieldTargets, which owns
// the `via` actions).
func (s *WorkflowService) checkFieldUpdateValues(ctx context.Context, tenantID string, def *metadata.EntityDefinition, t workflow.Trigger, actions []WorkflowActionInput) error {
	if def == nil {
		return nil
	}
	lister, ok := s.entites.(workflowFieldLister)
	if !ok {
		return nil
	}
	var specs []FieldSpec
	for _, a := range actions {
		if strings.TrimSpace(a.Type) != workflow.ActionFieldUpdate || strings.TrimSpace(a.Via) != "" || strings.TrimSpace(a.ChildEntity) != "" {
			continue
		}
		if src := strings.TrimSpace(a.ValueSource); src != "" && src != workflow.ValueSourceConstant {
			continue
		}
		if specs == nil {
			fields, err := lister.ListFields(ctx, tenantID, def.ID)
			if err != nil {
				return fmt.Errorf("services.WorkflowService.checkFieldUpdateValues: %w", err)
			}
			if specs, err = toFieldSpecs(fields); err != nil {
				return fmt.Errorf("services.WorkflowService.checkFieldUpdateValues: %w", err)
			}
		}
		field := strings.TrimSpace(a.Field)
		if t != workflow.TriggerBeforeSave { // before_save reads a value through valueSource; "$..." there is a literal
			ruleTypes := make(map[string]string, len(specs))
			for _, sp := range specs {
				ruleTypes[sp.Name] = sp.FieldType
			}
			if dyn, err := checkDynamicFieldUpdateValue(a, ruleTypes, field, ruleTypes[field]); err != nil {
				return err
			} else if dyn {
				continue
			}
		}
		if _, err := ValidatePartialRecord(specs, map[string]any{field: a.Value}); err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				return fmt.Errorf("services.WorkflowService.checkFieldUpdateValues: %w", err)
			}
			if t != workflow.TriggerBeforeSave {
				return err
			}
			return beforeValueInvalid(field, ve.Code, "giá trị tự điền cho field %q không hợp lệ: %s", field, ve.Msg)
		}
	}
	return nil
}

// beforeValueInvalid is WORKFLOW_BEFORE_VALUE_INVALID anchored on the target field, with why.
func beforeValueInvalid(field, reason, format string, args ...any) error {
	return &ValidationError{Code: "WORKFLOW_BEFORE_VALUE_INVALID", Field: field, Msg: fmt.Sprintf(format, args...),
		Details: map[string]any{"field": field, "reason": reason}}
}

// actionNotAllowed builds WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER naming both halves of the pair.
func actionNotAllowed(actionType string, t workflow.Trigger, why string) error {
	return &ValidationError{Code: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER",
		Msg:     fmt.Sprintf("hành động %q không dùng được với trigger %q — %s", actionType, t, why),
		Details: map[string]any{"actionType": actionType, "trigger": string(t)}}
}

// checkBeforeActivation is the apply-path half for an ACTIVE before rule: the per-(entity, trigger)
// cap. excludeID is the rule being edited so re-saving the 50th rule does not count it twice. (Until
// S3 it also refused to switch on a kind of before rule whose runtime was not on main yet — R13;
// every kind runs now, so that gate and its code are gone.)
func (s *WorkflowService) checkBeforeActivation(ctx context.Context, tenantID, entity string, t workflow.Trigger, isActive bool, excludeID string) error {
	if !isActive || !workflow.IsBeforeTrigger(t) {
		return nil
	}
	n, err := s.repo.CountActive(ctx, tenantID, entity, t, excludeID)
	if err != nil {
		return fmt.Errorf("services.WorkflowService.checkBeforeActivation: %w", err)
	}
	if n >= maxActiveBeforeRules {
		return &ValidationError{Code: "WORKFLOW_BEFORE_RULE_LIMIT",
			Msg:     fmt.Sprintf("entity %q đã có %d quy tắc %s đang bật (tối đa %d) — tắt bớt quy tắc cũ trước", entity, n, t, maxActiveBeforeRules),
			Details: map[string]any{"limit": maxActiveBeforeRules}}
	}
	return nil
}
