package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// Formulas that read a lookup / rollup (queue #284, specs/080).
//
// A lookup mirrors a value from a related record and a rollup aggregates child rows; both are
// computed at READ time and are never in the stored record. A formula over them therefore cannot
// be computed at save (applyComputedFields would read 0 and STORE it). Such a "derived" formula
// is computed at read instead, right after applyLookups/applyRollups, and never written:
// "thành tiền = số lượng × đơn giá (tra cứu)" reads the service's CURRENT price.

// lookupValueTypes resolves, for every lookup in fields, the field type of the value it mirrors
// ("number", "currency", "text"…), keyed by the lookup's name. A lookup whose via/target/source
// cannot be resolved is left out (formula.Validate then refuses it, fail-closed). Target field
// lists are read once per target entity. selfEntity/fields supply the in-memory list for a
// lookup into the entity being created (it is not persisted yet).
func lookupValueTypes(ctx context.Context, meta lookupMetaResolver, tenantID, selfEntity string, fields []FieldInput) (map[string]string, error) {
	var out map[string]string
	rel := make(map[string]string) // relation field name -> relatedEntity
	for _, f := range fields {
		if f.FieldType == "relation" {
			t, _ := f.Options["relatedEntity"].(string)
			rel[f.Name] = strings.TrimSpace(t)
		}
	}
	targets := make(map[string]map[string]string) // target entity -> field name -> field type
	typesOf := func(target string) (map[string]string, error) {
		if m, ok := targets[target]; ok {
			return m, nil
		}
		m := map[string]string{}
		if target == selfEntity {
			for _, f := range fields {
				m[f.Name] = f.FieldType
			}
		} else {
			e, err := meta.GetEntityByName(ctx, tenantID, target)
			if err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					targets[target] = m
					return m, nil
				}
				return nil, fmt.Errorf("services.lookupValueTypes: resolve %q: %w", target, err)
			}
			defs, err := meta.ListFields(ctx, tenantID, e.ID)
			if err != nil {
				return nil, fmt.Errorf("services.lookupValueTypes: list fields of %q: %w", target, err)
			}
			for _, d := range defs {
				m[d.Name] = d.FieldType
			}
		}
		targets[target] = m
		return m, nil
	}
	for _, f := range fields {
		if f.FieldType != "lookup" {
			continue
		}
		via, _ := f.Options["via"].(string)
		src, _ := f.Options["sourceField"].(string)
		target := rel[strings.TrimSpace(via)]
		if target == "" {
			continue
		}
		m, err := typesOf(target)
		if err != nil {
			return nil, err
		}
		if ft := m[strings.TrimSpace(src)]; ft != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[f.Name] = ft
		}
	}
	return out, nil
}

// formulaInfos is the sibling list formula.Validate checks identifiers against: name + type, and
// for a lookup the type of the value it mirrors (lookupTypes, may be nil).
func formulaInfos(fields []FieldInput, lookupTypes map[string]string) []formula.FieldInfo {
	infos := make([]formula.FieldInfo, 0, len(fields))
	for _, f := range fields {
		infos = append(infos, formula.FieldInfo{Name: f.Name, FieldType: f.FieldType, ValueType: lookupTypes[f.Name]})
	}
	return infos
}

// derivedFormula reports the lookup/rollup fields a formula spec reads (nil = a stored formula).
func derivedFormula(sp FieldSpec, specs []FieldSpec) []string {
	if sp.FieldType != "formula" {
		return nil
	}
	expr, _ := sp.Options["formula"].(string)
	if expr == "" {
		return nil
	}
	infos := make([]formula.FieldInfo, 0, len(specs))
	for _, s := range specs {
		if s.FieldType == "lookup" || s.FieldType == "rollup" {
			infos = append(infos, formula.FieldInfo{Name: s.Name, FieldType: s.FieldType})
		}
	}
	if len(infos) == 0 {
		return nil
	}
	return formula.Derived(expr, infos)
}

// derivedRollupRefs returns the rollup fields read by derived formulas the caller may see, so the
// read path resolves them even when ?rollup= did not ask — otherwise the formula would read 0.
// ponytail: one aggregate per row per rollup, only for entities that define such a formula; a
// page-level batch is the upgrade path if lists of >100 rows show it (specs/005 #300).
func derivedRollupRefs(specs []FieldSpec, hidden map[string]struct{}) []string {
	isRollup := map[string]bool{}
	for _, s := range specs {
		if s.FieldType == "rollup" {
			isRollup[s.Name] = true
		}
	}
	if len(isRollup) == 0 {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, sp := range specs {
		if _, h := hidden[sp.Name]; h {
			continue
		}
		for _, ref := range derivedFormula(sp, specs) {
			if _, dup := seen[ref]; !dup && isRollup[ref] {
				seen[ref] = struct{}{}
				out = append(out, ref)
			}
		}
	}
	return out
}

// withDerivedRollups appends to the requested ?rollup= names the rollups derived formulas need.
func withDerivedRollups(requested []string, specs []FieldSpec, hidden map[string]struct{}) []string {
	extra := derivedRollupRefs(specs, hidden)
	if len(extra) == 0 {
		return requested
	}
	return append(append([]string(nil), requested...), extra...)
}

// applyDerivedFormulas computes, in place, every derived formula of the entity on records whose
// lookups/rollups have just been resolved. A formula in hidden (it, or an operand, is
// field-ACL denied — hiddenFormulaFields) is skipped, so the read strip stays authoritative; one
// whose lookup/rollup operand the caller may not see (denied) reads null instead of a wrong number.
// Null operands read as 0 (the same rule as a stored formula); division by zero gives null.
func applyDerivedFormulas(specs []FieldSpec, records []dynrepo.Record, hidden, denied map[string]struct{}) {
	for _, sp := range specs {
		refs := derivedFormula(sp, specs)
		if _, h := hidden[sp.Name]; h || len(refs) == 0 {
			continue
		}
		if anyDenied(refs, denied) {
			// An operand resolved to null/0 only because the caller may not see it: a number
			// computed from it would be silently wrong, so the formula reads null.
			for i := range records {
				if records[i].Data != nil {
					records[i].Data[sp.Name] = nil
				}
			}
			continue
		}
		for i := range records {
			if records[i].Data == nil {
				continue
			}
			data := records[i].Data
			data[sp.Name] = evalFormulaSpec(sp, func(name string) float64 { return numericValue(data[name]) }, textValues(data))
		}
	}
}

// numericValue reads a stored/resolved field value as a number: a float, a currency object's
// amount, or the exact decimal string a rollup/aggregate emits. Anything else is 0.
func numericValue(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n
	case map[string]any:
		return numericValue(x["amount"])
	}
	return 0
}

func markDenied(denied map[string]struct{}, name string) {
	if denied != nil {
		denied[name] = struct{}{}
	}
}

func anyDenied(refs []string, denied map[string]struct{}) bool {
	for _, r := range refs {
		if _, d := denied[r]; d {
			return true
		}
	}
	return false
}
