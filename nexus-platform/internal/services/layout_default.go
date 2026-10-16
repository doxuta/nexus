package services

import (
	"nexus/platform/internal/metadata"
)

// generateDefault produces a valid layout for (entity, viewType) from live fields.
// It never persists: GetLayout calls this when no custom layout exists in sys_layouts.
//
// Rules (spec §4.2):
//   - System fields (systemFieldSet) are always excluded.
//   - Field order: field_order ASC, name ASC (W5 mig 000033 — ListFields serves this; supersedes D6's name ASC).
//   - edit/detail: one "Thông tin" section, columns=2 if ≥4 user fields else 1.
//     rich_text gets fullWidth:true. All is_required fields are always included (never readonly).
//   - list: fallback chain for first column (first text field → field named "name" → "id"),
//     then up to 4 more short-scalar fields (≤5 total), excluding heavy/relation types.
//     No `sortable` key on any column — GetLayout decides it at serve time from the engine's
//     sort allowlist (DAILY-QUEUE #4; no writer of layout JSON carries that opinion).
//     defaultSort = {created_at, desc}.
//   - 0-field entity: still returns a valid non-empty layout (never null/empty/500).
func generateDefault(entity string, viewType string, fields []metadata.FieldDefinition) map[string]any {
	switch viewType {
	case LayoutViewEdit, LayoutViewDetail:
		return generateEditOrDetailDefault(entity, viewType, fields)
	case LayoutViewList:
		return generateListDefault(fields)
	}
	// Fallback (should not happen — callers always pass a valid view type):
	return generateEditOrDetailDefault(entity, viewType, fields)
}

// generateEditOrDetailDefault builds the default edit or detail layout.
//
// MỘT XƯỞNG (2026-07-29, spec §TRỤ 2 mục 6 "màn hình đẹp sẵn"): the generator used to emit ONE
// flat "Thông tin" section for every entity, so every app Nexus produced looked identical — a wall
// of inputs with no business shape. Founder's words: the output doesn't look like real software.
//
// Now: entities with enough fields to benefit are grouped into business-meaningful sections by
// field SEMANTICS (not by name — names are tenant data and unknowable). Small entities are left
// EXACTLY as before (single section, byte-identical) — fragmenting a 3-field table into 3 headed
// groups is worse, not better. groupThreshold is the only knob.
const layoutGroupThreshold = 6

// defaultSectionSpec maps a section title to the field types that belong in it, in render order.
// A section is emitted only when it has at least one field. Order matters: identity first, then
// numbers, then dates, then links, then long-form content last (it is full-width and heavy).
var defaultSectionSpecs = []struct {
	title string
	types map[string]bool
}{
	{"Thông tin chung", map[string]bool{
		"text": true, "email": true, "picklist": true, "multi_select": true,
		"boolean": true, "rating": true, "auto_number": true, "composite": true,
	}},
	{"Số liệu", map[string]bool{
		"number": true, "currency": true, "formula": true, "rollup": true, "lookup": true,
	}},
	{"Thời gian", map[string]bool{"date": true, "datetime": true}},
	{"Liên kết", map[string]bool{
		"relation": true, "polymorphic_relation": true, "user": true,
	}},
	{"Mô tả & tệp", map[string]bool{"rich_text": true, "file": true, "image": true}},
}

func generateEditOrDetailDefault(_ string, _ string, fields []metadata.FieldDefinition) map[string]any {
	userFields := filterUserFields(fields)

	cols := 1
	if len(userFields) >= 4 {
		cols = 2
	}

	// Small entity (or nothing to group into): keep the original single-section shape verbatim.
	if len(userFields) < layoutGroupThreshold {
		return map[string]any{
			"version": 1,
			"sections": []any{map[string]any{
				"title":   "Thông tin",
				"columns": cols,
				"fields":  cellsFor(userFields),
			}},
		}
	}

	sections := make([]any, 0, len(defaultSectionSpecs))
	placed := make(map[string]bool, len(userFields))
	for _, spec := range defaultSectionSpecs {
		group := make([]metadata.FieldDefinition, 0, len(userFields))
		for _, f := range userFields {
			if spec.types[f.FieldType] {
				group = append(group, f)
				placed[f.Name] = true
			}
		}
		if len(group) == 0 {
			continue
		}
		secCols := cols
		if spec.title == "Mô tả & tệp" {
			secCols = 1 // long-form content reads badly in two narrow columns
		}
		sections = append(sections, map[string]any{
			"title":   spec.title,
			"columns": secCols,
			"fields":  cellsFor(group),
		})
	}

	// Any field type not covered above must still be rendered — never drop a field silently
	// (that is the accept-then-swallow bug class this campaign exists to kill). New field types
	// added later land here until someone assigns them a section.
	rest := make([]metadata.FieldDefinition, 0)
	for _, f := range userFields {
		if !placed[f.Name] {
			rest = append(rest, f)
		}
	}
	if len(rest) > 0 {
		sections = append(sections, map[string]any{
			"title":   "Khác",
			"columns": cols,
			"fields":  cellsFor(rest),
		})
	}

	if len(sections) == 0 { // unreachable given the threshold, but never return an empty layout
		sections = append(sections, map[string]any{
			"title": "Thông tin", "columns": cols, "fields": []any{"id"},
		})
	}
	return map[string]any{"version": 1, "sections": sections}
}

// cellsFor turns field definitions into layout cells, preserving the incoming order
// (field_order ASC, name ASC — served by ListFields). rich_text spans the full row.
func cellsFor(fields []metadata.FieldDefinition) []any {
	cells := make([]any, 0, len(fields)+1)
	for _, f := range fields {
		if f.FieldType == "rich_text" {
			cells = append(cells, map[string]any{"field": f.Name, "fullWidth": true})
			continue
		}
		// Simple string form for clean cells — round-trip invariant (string == {field:name}).
		cells = append(cells, f.Name)
	}
	// Spec §4.2: a 0-field entity still returns a valid non-empty layout — include the "id"
	// system-identifier column so the section is non-empty and the round-trip invariant holds.
	if len(cells) == 0 {
		cells = append(cells, "id")
	}
	return cells
}

// generateListDefault builds the default list layout.
func generateListDefault(fields []metadata.FieldDefinition) map[string]any {
	userFields := filterUserFields(fields)

	columns := buildListColumns(userFields)

	return map[string]any{
		"version": 1,
		"columns": columns,
		"defaultSort": map[string]any{
			"field": "created_at",
			"order": "desc",
		},
	}
}

// buildListColumns selects ≤5 columns for the list layout using the spec §4.2 fallback chain:
// 1. first live text field → or field named "name" → or "id"
// 2. up to 4 more short-scalar fields (short scalars + relation/user references; heavy types excluded)
//
// The "id" fallback column uses a sentinel — "id" is a system field but is explicitly
// the last resort so the list always has at least 1 column.
func buildListColumns(userFields []metadata.FieldDefinition) []any {
	const maxCols = 5

	var primary *metadata.FieldDefinition
	var nameField *metadata.FieldDefinition

	// Find candidates for primary column.
	for i := range userFields {
		f := &userFields[i]
		if primary == nil && f.FieldType == "text" {
			primary = f
		}
		if f.Name == "name" {
			nameField = f
		}
	}

	// Fallback chain: first text → "name" → "id" (system field sentinel)
	var leadField string
	switch {
	case primary != nil:
		leadField = primary.Name
	case nameField != nil:
		leadField = nameField.Name
	default:
		leadField = "id"
	}

	columns := make([]any, 0, maxCols)

	// No `sortable` here (or below): the generator is a writer, and serve time is the only
	// decider — "id" as the fallback lead IS engine-sortable and now serves as such.
	columns = append(columns, map[string]any{
		"field": leadField,
		"width": 200,
	})

	// Add up to 4 more short-scalar user fields, skipping the lead and heavy/relation types.
	seen := map[string]bool{leadField: true}
	for _, f := range userFields {
		if len(columns) >= maxCols {
			break
		}
		if seen[f.Name] {
			continue
		}
		if isHeavy(f.FieldType) {
			continue
		}
		if !isShortScalar(f.FieldType) && !isRefColumn(f.FieldType) {
			continue
		}
		seen[f.Name] = true
		columns = append(columns, map[string]any{
			"field": f.Name,
			"width": 120,
		})
	}

	return columns
}

// filterUserFields returns only the live non-system fields, in the order provided
// (ListFields returns field_order ASC, name ASC — W5; the result preserves that ordering).
func filterUserFields(fields []metadata.FieldDefinition) []metadata.FieldDefinition {
	out := make([]metadata.FieldDefinition, 0, len(fields))
	for _, f := range fields {
		if isSystemField(f.Name) {
			continue
		}
		out = append(out, f)
	}
	return out
}
