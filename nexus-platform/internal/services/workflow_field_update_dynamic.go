package services

import (
	"strings"

	"nexus/platform/internal/workflow"
)

// checkDynamicFieldUpdateValue is the save-time half of a field_update value that is resolved at FIRE
// time (queue #333): "$record.<field>" (typed copy off the triggering record), a "{{field}}" template,
// or the "$owner" / "$record" sentinels. isDynamic=false means the value is a plain constant and the
// caller validates it as a literal.
//
// A dynamic value has no value yet, so what can be known now is checked: a computed/assigned target is
// refused as it is for a constant; "$record.<f>" must name a field of the RULE's entity (ruleTypes) of
// the SAME type as the target — the write validates the copied value again at fire time, but a rule
// copying text into a number would fail on every run, so it is refused here (B6 parity); a template
// may only fill a text target and read plain fields of the rule's entity (the renderer is flat — a
// "{{a.b}}" would render empty forever). Errors anchor on the TARGET field, where the Studio value
// box shows them.
func checkDynamicFieldUpdateValue(a WorkflowActionInput, ruleTypes map[string]string, target, targetType string) (isDynamic bool, err error) {
	str, ok := a.Value.(string)
	if !ok || (a.ValueSource != "" && a.ValueSource != workflow.ValueSourceConstant) {
		return false, nil
	}
	t := strings.TrimSpace(str)
	switch {
	case t == workflow.OwnerSentinel || t == workflow.RecordSentinel:
		isDynamic = true
	case strings.HasPrefix(t, workflow.RecordFieldPrefix):
		isDynamic = true
	case strings.Contains(t, "{{"):
		isDynamic = true
	default:
		return false, nil
	}
	if code, readonly := computedReadonlyCode(targetType); readonly {
		return true, invalidField(code, target, "field %q là computed/assigned field (%s) — field_update không được gán", target, targetType)
	}
	if t == workflow.OwnerSentinel || t == workflow.RecordSentinel {
		// Both resolve to an id string: only a text, user or link field can hold one.
		if targetType != "text" && targetType != "user" && targetType != "relation" {
			return true, invalidField("RECORD_INVALID_TYPE", target,
				"giá trị %q là một mã người dùng/bản ghi — field đích %q (%s) không nhận được", t, target, targetType)
		}
		return true, nil
	}
	if strings.HasPrefix(t, workflow.RecordFieldPrefix) {
		src := strings.TrimSpace(strings.TrimPrefix(t, workflow.RecordFieldPrefix))
		srcType, known := ruleTypes[src]
		if !known {
			return true, invalidField("RECORD_UNKNOWN_FIELD", target, "giá trị %q: field nguồn %q không có trong đối tượng của quy tắc", t, src)
		}
		if srcType != targetType {
			return true, invalidField("RECORD_INVALID_TYPE", target,
				"giá trị %q: field nguồn %q (%s) khác kiểu field đích %q (%s)", t, src, srcType, target, targetType)
		}
		return true, nil
	}
	if strings.Contains(t, "{{") {
		if !beforeTextTarget[targetType] {
			return true, invalidField("RECORD_INVALID_TYPE", target,
				"mẫu chữ {{field}} chỉ điền được vào field chữ (text, email, rich_text); %q là %s", target, targetType)
		}
		for _, name := range workflow.TemplatePlaceholders(t) {
			if strings.Contains(name, ".") {
				return true, invalidField("WORKFLOW_TEMPLATE_REF_INVALID", target,
					"{{%s}}: mẫu chữ của Cập nhật trường chỉ đọc field của chính bản ghi kích hoạt, không đọc qua liên kết", name)
			}
			srcType, known := ruleTypes[name]
			if !known {
				return true, invalidField("RECORD_UNKNOWN_FIELD", target, "mẫu chữ đọc {{%s}} — không phải field của đối tượng của quy tắc", name)
			}
			// Same readable set as a before_save template (B6 parity); auto_number is already assigned on after_*.
			if !beforeTemplateSource[srcType] {
				return true, invalidField("RECORD_INVALID_TYPE", target,
					"mẫu chữ không đọc được {{%s}} (%s) — chỉ field lưu trên bản ghi có giá trị chữ, số, ngày hoặc lựa chọn", name, srcType)
			}
		}
	}
	return true, nil
}
