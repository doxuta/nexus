package services

import (
	"fmt"
	"slices"
	"strings"

	"nexus/platform/internal/metadata"
)

// Dependent picklist (specs/052-dependent-picklist): a picklist whose allowed values depend on
// the value of a sibling picklist on the same entity — Tỉnh → Quận/Huyện, Khoa → Chuyên ngành.
// The config lives on the CHILD field's options, next to its full `values` list:
//
//	"controllingField": "province",
//	"dependentValues":  {"Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"]}
//
// The parent is an ordinary picklist and carries nothing extra. GET /metadata/fields/:entity
// serves options verbatim, so the FE reads the same map to filter the child dropdown.
const (
	optControllingField = "controllingField"
	optDependentValues  = "dependentValues"

	codeFieldDependentInvalid  = "FIELD_DEPENDENT_PICKLIST_INVALID"
	codeRecordDependentInvalid = "RECORD_DEPENDENT_PICKLIST_INVALID"
)

// hasDependentKeys reports whether options carries either dependent-picklist key.
func hasDependentKeys(options map[string]any) bool {
	_, c := options[optControllingField]
	_, d := options[optDependentValues]
	return c || d
}

// validateDependentConfig is the single-field structural check of a picklist's dependent config
// (FR-2), run from validateFieldConfig on every create/add/edit path. A picklist without either
// key passes untouched. Both keys travel together; every child value in the map must be one of
// the field's OWN values (catches "Hai Chau" typed for "Hải Châu" — such an entry could never be
// stored, since checkType rejects it first, so it is config junk, not a harmless orphan).
// Cross-field rules (the parent exists, is a picklist, map keys ⊆ its values, no cycle) need the
// sibling list and live in validateDependentRefs.
func validateDependentConfig(name string, options map[string]any) error {
	if !hasDependentKeys(options) {
		return nil
	}
	parent, ok := options[optControllingField].(string)
	if !ok || strings.TrimSpace(parent) == "" {
		return invalid(codeFieldDependentInvalid,
			"field picklist %q: options.controllingField phải là tên field cha (chuỗi không rỗng) và đi cùng options.dependentValues", name)
	}
	if parent == name {
		return invalid(codeFieldDependentInvalid, "field picklist %q không thể phụ thuộc chính nó", name)
	}
	deps, ok := options[optDependentValues].(map[string]any)
	if !ok || len(deps) == 0 {
		return invalid(codeFieldDependentInvalid,
			"field picklist %q: options.dependentValues phải là object không rỗng (giá trị cha → danh sách giá trị con)", name)
	}
	for parentVal, raw := range deps {
		if strings.TrimSpace(parentVal) == "" {
			return invalid(codeFieldDependentInvalid, "field picklist %q: options.dependentValues có giá trị cha rỗng", name)
		}
		children, ok := raw.([]any)
		if !ok {
			return invalid(codeFieldDependentInvalid,
				"field picklist %q: options.dependentValues[%q] phải là mảng giá trị con", name, parentVal)
		}
		for _, c := range children {
			s, ok := c.(string)
			if !ok || !picklistContains(options, s) {
				return invalid(codeFieldDependentInvalid,
					"field picklist %q: options.dependentValues[%q] có %v không thuộc options.values của chính field", name, parentVal, c)
			}
		}
	}
	return nil
}

// validateDependentRefs checks f's dependent config against its LIVE siblings on the same entity
// (FR-3): the controlling field exists and is a picklist, every dependentValues key is one of its
// values at authoring time, and walking the controllingField chain upward never returns to f.
// siblings may contain f itself (ignored — f's own options are the ones under edit). A field
// without the config passes; validateDependentConfig has already pinned the shape.
//
// stored is f's options as persisted (nil on create/add). An edit judges only what it CHANGES:
// the Studio inspector resends every stored key, so a child's unrelated edit (a new value,
// indexed, valueLabels) carries its map verbatim. While controllingField is unchanged, a parent
// that is no longer live leaves the child flat (FR-4, nothing to judge against) and only map keys
// absent from the stored map are checked — a carried key the parent has since dropped is an inert
// orphan (FR-4), exactly as it is when the parent's own edit drops it.
func validateDependentRefs(f FieldInput, siblings []FieldInput, stored map[string]any) error {
	parent, _ := f.Options[optControllingField].(string)
	if parent == "" {
		return nil
	}
	carried := parent == stored[optControllingField]
	byName := make(map[string]FieldInput, len(siblings))
	for _, s := range siblings {
		if s.Name != f.Name {
			byName[s.Name] = s
		}
	}
	p, ok := byName[parent]
	if !ok || p.FieldType != "picklist" {
		if carried {
			return nil
		}
		return invalid(codeFieldDependentInvalid,
			"field picklist %q: options.controllingField %q phải là một field picklist đang có trên cùng entity", f.Name, parent)
	}
	deps, _ := f.Options[optDependentValues].(map[string]any)
	storedDeps, _ := stored[optDependentValues].(map[string]any)
	for parentVal := range deps {
		if _, kept := storedDeps[parentVal]; carried && kept {
			continue
		}
		if !picklistContains(p.Options, parentVal) {
			return invalid(codeFieldDependentInvalid,
				"field picklist %q: options.dependentValues có giá trị cha %q không thuộc options.values của %q", f.Name, parentVal, parent)
		}
	}
	// Bounded by the sibling count so a loop elsewhere in stored config can never spin.
	for cur, hops := parent, 0; cur != "" && hops <= len(byName); hops++ {
		next, _ := byName[cur].Options[optControllingField].(string)
		if next == f.Name {
			return invalid(codeFieldDependentInvalid,
				"field picklist %q: chuỗi controllingField tạo vòng (%q phụ thuộc ngược lại %q)", f.Name, cur, f.Name)
		}
		cur = next
	}
	return nil
}

// liveFieldInputs converts stored field definitions into FieldInputs (options parsed) for the
// cross-field check, dropping soft-deleted rows — a deleted parent is not a parent.
func liveFieldInputs(fields []metadata.FieldDefinition) ([]FieldInput, error) {
	out := make([]FieldInput, 0, len(fields))
	for _, fd := range fields {
		if fd.DeletedAt.Valid {
			continue
		}
		opts, err := parseOptionsJSON(fd.Options)
		if err != nil {
			return nil, fmt.Errorf("services.liveFieldInputs: field %q: %w", fd.Name, err)
		}
		out = append(out, FieldInput{Name: fd.Name, FieldType: fd.FieldType, Options: opts})
	}
	return out, nil
}

// hasDependentPicklist reports whether any picklist in specs declares a controlling field.
func hasDependentPicklist(specs []FieldSpec) bool {
	for _, f := range specs {
		if f.FieldType == "picklist" {
			if p, _ := f.Options[optControllingField].(string); p != "" {
				return true
			}
		}
	}
	return false
}

// dropDisallowedChildDefaults removes, from the create image clean, every dependent-picklist child
// that was filled from its DEFAULT (defaulted) when the controlling value in clean — sent or itself
// defaulted — is blank or does not list that default. A child the user picked is never touched
// (checkDependentPicklists judges it). It returns the name of a REQUIRED child left blank by the
// skip, "" otherwise. A child whose parent is not a live spec is flat (FR-4) and keeps its default.
func dropDisallowedChildDefaults(specs []FieldSpec, clean map[string]any, defaulted map[string]bool) string {
	if len(defaulted) == 0 {
		return ""
	}
	live := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		live[s.Name] = struct{}{}
	}
	// Repeat to a fixpoint: dropping a parent's default can invalidate its child's default, and the
	// field order (ListFields) does not promise parents come first.
	for changed := true; changed; {
		changed = false
		for _, f := range specs {
			if !defaulted[f.Name] || f.FieldType != "picklist" {
				continue
			}
			if _, still := clean[f.Name]; !still {
				continue
			}
			parent, _ := f.Options[optControllingField].(string)
			if _, ok := live[parent]; !ok || parent == "" {
				continue
			}
			deps, _ := f.Options[optDependentValues].(map[string]any)
			allowed, _ := deps[strValue(clean[parent])].([]any)
			if slices.Contains(allowed, clean[f.Name]) {
				continue
			}
			delete(clean, f.Name)
			changed = true
			if f.IsRequired {
				return f.Name
			}
		}
	}
	return ""
}

// checkDependentPicklists enforces every dependent picklist on a record write (FR-5/FR-6).
// Judged on the pair the write SENDS, never on the pair that merely sits in storage:
//   - a write carrying neither the parent nor the child key is not checked (a partial PUT of
//     another field saves a legacy mismatched record untouched);
//   - the effective pair is the sent value, else the stored one (`current`);
//   - an effective pair equal to the stored pair is a resend, not an edit — the form sends every
//     field on update, so without this a record holding a pre-config mismatch could never be
//     saved again (the same unchanged-value rule as validateRelationsAgainst);
//   - empty child: allowed. Child with empty parent, or child outside dependentValues[parent]:
//     RECORD_DEPENDENT_PICKLIST_INVALID with details.field = the child.
//
// current is nil on create, and on update when the best-effort read failed — then the payload is
// judged alone (fail closed). A child whose controlling field is no longer among the live specs
// (parent soft-deleted) is flat again (FR-4). Picklists without the config cost one map lookup.
func checkDependentPicklists(specs []FieldSpec, clean, current map[string]any) error {
	var live map[string]FieldSpec
	for _, f := range specs {
		if f.FieldType != "picklist" {
			continue
		}
		parent, _ := f.Options[optControllingField].(string)
		if parent == "" {
			continue
		}
		if live == nil {
			live = make(map[string]FieldSpec, len(specs))
			for _, s := range specs {
				live[s.Name] = s
			}
		}
		parentSpec, ok := live[parent]
		if !ok {
			continue
		}
		childV, childSent := clean[f.Name]
		parentV, parentSent := clean[parent]
		if !childSent && !parentSent {
			continue
		}
		if !childSent {
			childV = current[f.Name]
		}
		if !parentSent {
			parentV = current[parent]
		}
		child, par := strValue(childV), strValue(parentV)
		if current != nil && child == strValue(current[f.Name]) && par == strValue(current[parent]) {
			continue
		}
		if strings.TrimSpace(child) == "" {
			continue
		}
		// Messages name fields by their labels: this sentence reaches the customer's face.
		if strings.TrimSpace(par) == "" {
			return invalidField(codeRecordDependentInvalid, f.Name,
				"Chọn %q trước khi chọn %q", specLabel(parentSpec), specLabel(f))
		}
		deps, _ := f.Options[optDependentValues].(map[string]any)
		allowed, _ := deps[par].([]any)
		allowedHere := false
		for _, a := range allowed {
			if a == child {
				allowedHere = true
				break
			}
		}
		if !allowedHere {
			return invalidField(codeRecordDependentInvalid, f.Name,
				"%s không thuộc %s", labelled(f, child, childSent), labelled(parentSpec, par, parentSent))
		}
	}
	return nil
}

// dependentPicklistNamesHidden reports whether a RECORD_DEPENDENT_PICKLIST_INVALID raised for child
// would name a field the caller's role may not view (queue #321, api-conventions §4.4b): the child
// itself (details.field + its label) or its controlling parent (its label is in the message). The
// error needs no guess to reach — the child's stored default is materialized into every create, so
// an empty or non-allowing parent fails with the hidden child's name in it.
func dependentPicklistNamesHidden(specs []FieldSpec, hidden map[string]struct{}, child string) bool {
	if _, h := hidden[child]; h {
		return true
	}
	for _, f := range specs {
		if f.Name == child {
			parent, _ := f.Options[optControllingField].(string)
			_, h := hidden[parent]
			return parent != "" && h
		}
	}
	return false
}

// hiddenDependentPicklistRefusal is what a caller gets instead of the named error when it would
// have named a hidden field: the code (a dependent picklist did not fit) with no details.field and
// no label. The create cannot succeed, so the caller must learn that much — and where to send it.
func hiddenDependentPicklistRefusal() error {
	return invalid(codeRecordDependentInvalid,
		"Bản ghi chưa lưu được: một lựa chọn phụ thuộc mà vai trò của bạn không được xem không khớp với lựa chọn cha. Báo quản trị viên kiểm tra các trường phụ thuộc của đối tượng này")
}

// labelled renders `"Label" = "value"`, or just `"Label"` when the value was not in this write:
// an unsent value is the STORED one, and the field holding it may be hidden from the caller's role
// (field-level security) — the error must not read it back to them.
func labelled(f FieldSpec, value string, sent bool) string {
	if !sent {
		return fmt.Sprintf("%q", specLabel(f))
	}
	return fmt.Sprintf("%q = %q", specLabel(f), value)
}

// specLabel is the field's display label, falling back to its name for a field saved without one.
func specLabel(f FieldSpec) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// strValue returns v when it is a string, else "" (nil, or a legacy non-string value).
func strValue(v any) string {
	s, _ := v.(string)
	return s
}
