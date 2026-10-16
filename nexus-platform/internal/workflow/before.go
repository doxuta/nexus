package workflow

// specs/048 — the pure half of the synchronous "before" rules. RecordService loads the rules
// (CachedLoader) and calls these right before its single store call. Nothing here does I/O or
// mutates its inputs: the rule slice comes straight from the cache and is shared by every
// concurrent save of the entity.

import (
	"reflect"
	"strings"
	"unicode"
)

// SaveOp is the write a before rule is judged against.
type SaveOp string

const (
	SaveOpCreate SaveOp = "create"
	SaveOpUpdate SaveOp = "update"
	SaveOpDelete SaveOp = "delete"
)

// Violation is one matched veto: the message the user reads, the field it is anchored under
// (optional) and the rule that raised it. RecordService strips Field when the caller's role may
// not see that field and RuleID for every non-admin caller before it leaves the server.
type Violation struct {
	RuleID  string `json:"ruleId,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// AppliesTo reports whether a loaded rule runs on op: before_save on create and update (narrowed
// by Condition.SaveOn — absent means both), before_delete on delete. Every other trigger never
// applies, so an after_* rule handed in by mistake is inert rather than a veto.
func AppliesTo(wf Workflow, op SaveOp) bool {
	switch wf.Trigger {
	case TriggerBeforeDelete:
		return op == SaveOpDelete
	case TriggerBeforeSave:
		if op != SaveOpCreate && op != SaveOpUpdate {
			return false
		}
		saveOn := ""
		if wf.Condition != nil {
			saveOn = strings.TrimSpace(wf.Condition.SaveOn)
		}
		return saveOn == "" || saveOn == string(op)
	}
	return false
}

// CollectVetoes returns the veto of EVERY rule that applies to op and whose condition holds, in
// rule order (the loader serves created_at, id) — never just the first, so the user fixes all of
// them in one round trip (FR-015). image is the record as it is about to be written (on update
// the stored record merged with the change, computed fields recomputed); before is the stored
// record on update and nil otherwise, so changed/changedTo can only ever match an update.
func CollectVetoes(rules []Workflow, op SaveOp, image, before map[string]any) []Violation {
	var out []Violation
	for _, wf := range rules {
		if !AppliesTo(wf, op) || !EvaluateWithBefore(wf.Condition, image, before) {
			continue
		}
		for _, a := range wf.Actions {
			if a.Type == ActionVeto {
				out = append(out, Violation{RuleID: wf.ID, Field: strings.TrimSpace(a.Field), Message: a.Message})
			}
		}
	}
	return out
}

// MutationOutcome is one value-changing action that did something: it changed Field, or it could not
// (Error = the machine code the record validator gave). An action whose condition held but that left
// the value as it was has no outcome — it did nothing worth a run (FR-030).
type MutationOutcome struct {
	RuleID string
	Type   string
	Field  string
	Error  string
}

// ValueCheck validates one value about to be written into field and returns it as it will be stored
// (the record validator's coercion). image is the record as the rules before this action left it
// (read only), for a check that judges the value against another field — a dependent picklist's
// parent. A non-empty code refuses it: that action is dropped and the save goes on (FR-022). nil
// accepts every value as is.
type ValueCheck func(field string, value any, image map[string]any) (stored any, code string)

// codeBeforeValueInvalid is the outcome of a field_update whose value source is gone at run time: a
// copied field or a {{placeholder}} that is no longer a live field of the entity (FR-022). The same
// code refuses such a source when the rule is saved.
const codeBeforeValueInvalid = "WORKFLOW_BEFORE_VALUE_INVALID"

// ApplyMutations runs the value-changing actions (field_update, normalize) of every rule that applies
// to op, ONE pass in rule order (the loader serves created_at, id). Each rule's condition reads the
// image as the rules before it left it — so a later rule sees an earlier one's value (and a normalize
// rewrites what an earlier fill wrote), and changedTo compares that progressing image with before
// (the stored record; nil on a create). A written value never re-triggers a rule.
//
// sources are the fields a value may be read from (valueSource field, {{placeholders}}): the entity's
// live fields. The image cannot tell: a deleted field is a soft delete, so its old value still sits in
// the stored record — copying it would write a dead value over a live field, or clear what the user
// typed on a create. A source outside the set fails the action (codeBeforeValueInvalid).
//
// patch holds the fields whose final value differs from image (nil = cleared); outcomes lists every
// action that changed a value or was refused. image, before and the rules are only read: the rules
// come from the cache and are shared by every concurrent save, so object constants are copied before
// they leave. Veto actions are CollectVetoes' business — the caller judges them on the image with this
// patch applied.
func ApplyMutations(rules []Workflow, op SaveOp, image, before map[string]any, sources map[string]bool, check ValueCheck) (map[string]any, []MutationOutcome) {
	cur := make(map[string]any, len(image))
	for k, v := range image {
		cur[k] = v
	}
	touched := map[string]bool{}
	var outcomes []MutationOutcome
	for _, wf := range rules {
		if !AppliesTo(wf, op) || !EvaluateWithBefore(wf.Condition, cur, before) {
			continue
		}
		for _, st := range mutateRule(wf, cur, sources, check) {
			switch {
			case st.Error != "":
				outcomes = append(outcomes, MutationOutcome{RuleID: wf.ID, Type: st.Type, Field: st.Field, Error: st.Error})
			case st.Changed:
				touched[st.Field] = true
				outcomes = append(outcomes, MutationOutcome{RuleID: wf.ID, Type: st.Type, Field: st.Field})
			}
		}
	}
	patch := make(map[string]any, len(touched))
	for field := range touched {
		if !sameValue(image[field], cur[field]) {
			patch[field] = cur[field]
		}
	}
	return patch, outcomes
}

// MutationStep is what ONE value-changing action (Action = its index in the rule) does to the image
// it runs on: Field's value Before it and After it (After = Before when it writes nothing), whether
// that is a change, or the code that refused the value. Unlike MutationOutcome it carries values: it
// is the dry-run's (admin-only) answer, never a run's.
type MutationStep struct {
	Action  int
	Type    string
	Field   string
	Before  any
	After   any
	Changed bool
	Error   string
}

// PreviewMutations is what wf's value-changing actions would do to image, one step per action in
// order, each seeing the image as the actions before it left it — through the very body a save runs
// (mutateRule), so the dry-run cannot drift from it. The condition is not judged here: the dry-run
// shows what a rule would write AND whether it would run. image is only read.
func PreviewMutations(wf Workflow, image map[string]any, sources map[string]bool, check ValueCheck) []MutationStep {
	cur := make(map[string]any, len(image))
	for k, v := range image {
		cur[k] = v
	}
	return mutateRule(wf, cur, sources, check)
}

// mutateRule runs wf's value-changing actions in order on cur — each accepted value is written into
// cur, so the next action reads it — and reports every one of them. Other actions (veto) are skipped.
func mutateRule(wf Workflow, cur map[string]any, sources map[string]bool, check ValueCheck) []MutationStep {
	var steps []MutationStep
	for i, a := range wf.Actions {
		if a.Type != ActionFieldUpdate && a.Type != ActionNormalize {
			continue
		}
		field := strings.TrimSpace(a.Field)
		st := MutationStep{Action: i, Type: a.Type, Field: field, Before: cur[field], After: cur[field]}
		v, code, ok := mutationValue(a, field, cur, sources)
		if ok && check != nil {
			v, code = check(field, v, cur)
		}
		switch {
		case code != "":
			st.Error = code
		case ok && !sameValue(cur[field], v):
			if v == nil {
				delete(cur, field)
			} else {
				cur[field] = v
			}
			st.After, st.Changed = v, true
		}
		steps = append(steps, st)
	}
	return steps
}

// mutationValue is the value one value-changing action wants field to hold (ok=false: the action has
// nothing to write this time; a code: it cannot run). field_update reads its source (beforeValue),
// or skips a field that is not empty under onlyIfEmpty. normalize rewrites the text the field holds
// right now — what the user sent or an earlier rule wrote; a value that is not text is left alone.
func mutationValue(a Action, field string, cur map[string]any, sources map[string]bool) (v any, code string, ok bool) {
	switch a.Type {
	case ActionFieldUpdate:
		if a.OnlyIfEmpty && !IsBlank(cur[field]) {
			return nil, "", false
		}
		return beforeValue(a, cur, sources)
	case ActionNormalize:
		s, isText := cur[field].(string)
		if !isText {
			return nil, "", false
		}
		return normalizeText(s, a.Ops), "", true
	}
	return nil, "", false
}

// normalizeText applies ops to s in order (Normalize*). An operation it does not know is inert: the
// rule's ops were checked when it was saved, and a save never fails for a normalize.
func normalizeText(s string, ops []string) string {
	for _, op := range ops {
		switch op {
		case NormalizeTrim:
			s = strings.TrimSpace(s)
		case NormalizeLower:
			s = strings.ToLower(s)
		case NormalizeUpper:
			s = strings.ToUpper(s)
		case NormalizeCollapseSpaces:
			s = collapseSpaces(s)
		}
	}
	return s
}

// collapseSpaces turns every run of whitespace (unicode.IsSpace — tabs, newlines, no-break spaces)
// into one ordinary space, the edges included: trimming them is the trim operation's job.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// beforeValue reads a field_update's value by its source (Action.ValueSource). A copied field or a
// placeholder outside sources is a code (the action fails). A template that renders blank is not a
// value (ok=false, no code): the action does nothing rather than clear the target — its placeholders
// are empty on this record, and a template exists to fill text, not to erase it. A copied field that
// is empty does clear: the rule mirrors that field.
func beforeValue(a Action, cur map[string]any, sources map[string]bool) (v any, code string, ok bool) {
	switch strings.TrimSpace(a.ValueSource) {
	case ValueSourceField:
		name, _ := a.Value.(string)
		name = strings.TrimSpace(name)
		if !sources[name] {
			return nil, codeBeforeValueInvalid, false
		}
		return copyValue(cur[name]), "", true
	case ValueSourceTemplate:
		tmpl, _ := a.Value.(string)
		for _, name := range TemplatePlaceholders(tmpl) {
			if !sources[name] {
				return nil, codeBeforeValueInvalid, false
			}
		}
		out := renderTemplate(tmpl, cur)
		if strings.TrimSpace(out) == "" {
			return nil, "", false
		}
		return out, "", true
	default:
		return copyValue(a.Value), "", true
	}
}

// copyValue deep-copies the JSON object/array shapes a record value can take, so nothing downstream
// can write through a patch value into a cached rule or the caller's image.
func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = copyValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = copyValue(e)
		}
		return out
	}
	return v
}

// sameValue is looseEqual for mixed scalars ("40" = 40), byte equality for two strings and deep
// equality for objects/arrays, which looseEqual cannot tell apart (they all render to ""). Two
// strings are text: looseEqual reads " 0901234567 " and "0901234567" as the same number, and "007"
// as 7 — a trimmed phone or a zero-padded code would never be written.
func sameValue(a, b any) bool {
	switch a.(type) {
	case map[string]any, []any:
		return reflect.DeepEqual(a, b)
	}
	switch b.(type) {
	case map[string]any, []any:
		return false
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return as == bs
		}
	}
	return looseEqual(a, b)
}

// IsBlank is "empty" for onlyIfEmpty: nil, a whitespace-only string, [] or {} — the same reading as
// the record validator's services.isEmpty (the package cannot import services; the parity is pinned
// by TestIsBlank_ParityWithServicesIsEmpty there).
func IsBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}
