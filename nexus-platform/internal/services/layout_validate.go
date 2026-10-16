package services

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"nexus/platform/internal/metadata"
)

// knownTopKeysEditDetail are the ONLY allowed top-level keys for edit/detail layouts.
var knownTopKeysEditDetail = map[string]struct{}{
	"version":  {},
	"sections": {},
}

// knownTopKeysList are the ONLY allowed top-level keys for list layouts.
// kanbanField/calendarField are the DECLARED view fields (DAILY-QUEUE #5): without them the
// Kanban board grouped by whatever picklist happened to come first in metadata and the Calendar
// laid out on the first date field, with no way for a maker to say otherwise.
var knownTopKeysList = map[string]struct{}{
	"version":       {},
	"columns":       {},
	"defaultSort":   {},
	"kanbanField":   {},
	"calendarField": {},
}

// listViewFieldTypes maps each declared view-field key to the field types that key accepts.
// These predicates MUST stay identical to the renderer's, or a layout saves clean and silently
// does nothing: kanban groups by picklist (lib/kanbanModel.ts pickKanbanField) and calendar lays
// out on date|datetime (components/layout/CalendarView.tsx).
var listViewFieldTypes = map[string]map[string]struct{}{
	"kanbanField":   {"picklist": {}},
	"calendarField": {"date": {}, "datetime": {}},
}

// knownSectionKeys are the ONLY allowed keys in a section object.
var knownSectionKeys = map[string]struct{}{
	"title":     {},
	"columns":   {},
	"collapsed": {},
	"fields":    {},
}

// knownCellKeys are the ONLY allowed keys in an object cell (non-spacer).
// helpText (W5 §3.3 WYSIWYG cell props) is the per-VIEW help string rendered under the
// widget — distinct from sys_fields.description (the per-FIELD admin description). The
// other W5 cell props already existed: colspan (span, 1..section.columns) and readonly
// (display-only hint on edit views).
var knownCellKeys = map[string]struct{}{
	"field":         {},
	"labelOverride": {},
	"required":      {},
	"readonly":      {},
	"fullWidth":     {},
	"colspan":       {},
	"helpText":      {},
	// visibleWhen (specs/054): show this cell only when another field on the SAME layout meets a
	// condition — judged after the section walk by validateVisibleWhenCells.
	"visibleWhen": {},
}

// maxCellHelpTextRunes caps a cell's helpText (W5) — counted in runes per the codebase
// length convention.
const maxCellHelpTextRunes = 200

// knownColumnKeys are the ONLY allowed keys in a list column object.
var knownColumnKeys = map[string]struct{}{
	"field":         {},
	"labelOverride": {},
	"width":         {},
	"sortable":      {},
}

// knownSortKeys are the ONLY allowed keys in defaultSort.
var knownSortKeys = map[string]struct{}{
	"field": {},
	"order": {},
}

// knownSpacerKeys are the ONLY allowed keys in a spacer cell.
var knownSpacerKeys = map[string]struct{}{
	"spacer": {},
}

// ValidateLayoutConfig is the EXPORTED config-time layout validator — a thin wrapper around
// the unexported validateLayout so callers outside this package (the AI Builder's propose_layout
// tool, internal/ai) validate with EXACT parity to the apply path: SaveLayout and UpdateLayout
// call this SAME wrapper, so there is ONE function body and no hand-mirrored rule logic that
// could drift (M6, spec ../specs/020-e9-ai-builder/spec.md §13).
//
// liveFields is the RAW ListFields output for the (tenant, entity) — the user-field filter
// (system fields excluded) is applied HERE so every caller gets identical treatment.
// Returns a *ValidationError (code LAYOUT_INVALID, or REQUIRED_FIELD_OMITTED when an edit view
// omits a required field) on any violation; nil when the layout is valid.
func ValidateLayoutConfig(viewType string, layoutJSON map[string]any, liveFields []metadata.FieldDefinition) error {
	return validateLayout(viewType, layoutJSON, filterUserFields(liveFields))
}

// validateLayout validates the decoded layout_json map against ALL §4.4 rules.
// It receives the decoded layout (map[string]any), the trimmed view_type, the live fields
// for this (tenant, entity), and the original raw view_type (for the trim+normalize check).
// Returns a *ValidationError with code LAYOUT_INVALID on any violation.
//
// Parameter liveFields contains only live (non-deleted) non-system fields for this tenant+entity.
// The caller (LayoutService.SaveLayout) builds this slice before calling.
func validateLayout(rawViewType string, layout map[string]any, liveFields []metadata.FieldDefinition) error {
	// Rule 1: view_type — strict enum (no leading/trailing whitespace accepted).
	// The spec explicitly rejects ' list ' and 'Edit' — both have either whitespace or wrong case.
	// We check the raw value directly: only exact {edit, detail, list} tokens pass.
	vt := rawViewType
	if _, ok := validViewTypes[vt]; !ok {
		return invalid("LAYOUT_INVALID", "view_type %q không hợp lệ (phải là edit/detail/list)", rawViewType)
	}

	// Rule 2: version — bắt buộc khi save; major current = 1; future > 1 → 422
	versionRaw, hasVersion := layout["version"]
	if !hasVersion {
		return invalid("LAYOUT_INVALID", "thiếu trường version")
	}
	version, err := toInt(versionRaw)
	if err != nil || version < 1 {
		return invalid("LAYOUT_INVALID", "version phải là số nguyên ≥ 1")
	}
	if version > 1 {
		return invalid("LAYOUT_INVALID", "version %d chưa được hỗ trợ (phiên bản hiện tại là 1)", version)
	}

	// Rule 13: unknown-key strict-reject at top level
	var topKnown map[string]struct{}
	switch vt {
	case "edit", "detail":
		topKnown = knownTopKeysEditDetail
	case "list":
		topKnown = knownTopKeysList
	}
	for k := range layout {
		if _, ok := topKnown[k]; !ok {
			return invalid("LAYOUT_INVALID", "key không rõ %q ở cấp top-level", k)
		}
	}

	// Rule 3: shape per view
	switch vt {
	case "edit", "detail":
		if _, hasCols := layout["columns"]; hasCols {
			return invalid("LAYOUT_INVALID", "view %s không được có trường columns (dùng sections)", vt)
		}
		sectionsRaw, ok := layout["sections"]
		if !ok {
			return invalid("LAYOUT_INVALID", "view %s phải có trường sections", vt)
		}
		return validateSections(sectionsRaw, vt, liveFields)
	case "list":
		if _, hasSections := layout["sections"]; hasSections {
			return invalid("LAYOUT_INVALID", "view list không được có trường sections (dùng columns)")
		}
		colsRaw, ok := layout["columns"]
		if !ok {
			return invalid("LAYOUT_INVALID", "view list phải có trường columns")
		}
		if err := validateListViewFields(layout, liveFields); err != nil {
			return err
		}
		sortRaw := layout["defaultSort"]
		return validateListColumns(colsRaw, sortRaw, liveFields)
	}
	return nil
}

// validateSections validates the sections[] for an edit/detail layout.
func validateSections(sectionsRaw any, viewType string, liveFields []metadata.FieldDefinition) error {
	sections, ok := sectionsRaw.([]any)
	if !ok {
		return invalid("LAYOUT_INVALID", "sections phải là array")
	}

	// Rule 14: payload bounds — ≤50 sections
	if len(sections) > 50 {
		return invalid("LAYOUT_INVALID", "quá nhiều sections (tối đa 50, hiện tại %d)", len(sections))
	}

	// Build live field allowlist + required-field set
	liveAllowlist := buildFieldAllowlist(liveFields)
	requiredFields := buildRequiredSet(liveFields)

	// Rule 14 (cells): total cells ≤200 across all sections
	totalCells := 0

	// Track seen fields across all sections for duplicate detection (Rule 8)
	seenFields := make(map[string]bool)

	// Track which required fields appear in the layout (Rule 7, edit only)
	foundRequired := make(map[string]bool)

	// visibleWhen conditions met on the walk, judged once every cell is known (specs/054).
	var conditions []cellCondition

	for si, secRaw := range sections {
		sec, ok := secRaw.(map[string]any)
		if !ok {
			return invalid("LAYOUT_INVALID", "section[%d] phải là object", si)
		}

		// Rule 13: unknown keys in section
		for k := range sec {
			if _, ok := knownSectionKeys[k]; !ok {
				return invalid("LAYOUT_INVALID", "key không rõ %q trong section[%d]", k, si)
			}
		}

		// Rule 9: columns ∈ {1,2,3,4} — W5 §3.3 widened the grid from 3 to 4 columns so a
		// cell's colspan (bounded 1..sectionCols below) can span up to 4.
		colsRaw, hasCols := sec["columns"]
		if !hasCols {
			return invalid("LAYOUT_INVALID", "section[%d] thiếu trường columns", si)
		}
		sectionCols, err := toInt(colsRaw)
		if err != nil || sectionCols < 1 || sectionCols > 4 {
			return invalid("LAYOUT_INVALID", "section[%d].columns phải là 1, 2, 3, hoặc 4 (hiện tại: %v)", si, colsRaw)
		}

		// Validate optional title (must be string if present)
		if titleRaw, ok := sec["title"]; ok {
			if _, ok := titleRaw.(string); !ok {
				return invalid("LAYOUT_INVALID", "section[%d].title phải là chuỗi", si)
			}
		}

		// Validate optional collapsed (must be bool if present)
		if collapRaw, ok := sec["collapsed"]; ok {
			if _, ok := collapRaw.(bool); !ok {
				return invalid("LAYOUT_INVALID", "section[%d].collapsed phải là bool", si)
			}
		}

		fieldsRaw, hasFields := sec["fields"]
		if !hasFields {
			return invalid("LAYOUT_INVALID", "section[%d] thiếu trường fields", si)
		}
		cells, ok := fieldsRaw.([]any)
		if !ok {
			return invalid("LAYOUT_INVALID", "section[%d].fields phải là array", si)
		}

		totalCells += len(cells)
		if totalCells > 200 {
			return invalid("LAYOUT_INVALID", "quá nhiều cells (tối đa 200 trên tất cả sections)")
		}

		// Validate each cell; track non-spacer field references
		nonSpacerCount := 0
		for ci, cellRaw := range cells {
			fieldName, isSpacer, err := validateCell(cellRaw, ci, si, sectionCols, liveAllowlist, liveFields)
			if err != nil {
				return err
			}
			if isSpacer {
				continue
			}
			nonSpacerCount++

			// Rule 8: duplicate field check
			if seenFields[fieldName] {
				return invalid("LAYOUT_INVALID", "field %q xuất hiện nhiều lần trong layout", fieldName)
			}
			seenFields[fieldName] = true

			if requiredFields[fieldName] {
				foundRequired[fieldName] = true
			}

			if obj, ok := cellRaw.(map[string]any); ok {
				if raw, has := obj["visibleWhen"]; has {
					req, _ := obj["required"].(bool)
					conditions = append(conditions, cellCondition{
						target: fieldName, raw: raw, cellRequired: req,
						at: fmt.Sprintf("section[%d].fields[%d].visibleWhen", si, ci),
					})
				}
			}
		}

		// Rule 15: empty section (all spacers / truly empty) on save
		if nonSpacerCount == 0 {
			return invalid("LAYOUT_INVALID", "section[%d] không có field thực sự (chỉ spacer hoặc rỗng)", si)
		}
	}

	// specs/054 FR-2..FR-6: conditions reference a live controller cell on THIS layout, typed
	// operands, no cycles, and never hide a required field.
	if err := validateVisibleWhenCells(conditions, seenFields, liveFields); err != nil {
		return err
	}

	// Rule 7: required-omitted (edit only): all live required fields must appear
	if viewType == "edit" {
		for name := range requiredFields {
			if !foundRequired[name] {
				return invalid("REQUIRED_FIELD_OMITTED",
					"field bắt buộc %q phải có trong edit layout", name)
			}
		}
	}

	return nil
}

// validateCell validates a single cell in a section. Returns (fieldName, isSpacer, error).
// fieldName is "" for spacer cells.
func validateCell(
	cellRaw any, ci, si, sectionCols int,
	liveAllowlist map[string]struct{},
	liveFields []metadata.FieldDefinition,
) (string, bool, error) {
	// Case (a): string cell = {field: <name>}
	if name, ok := cellRaw.(string); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: cell chuỗi không được rỗng", si, ci)
		}
		// Rule 4: field allowlist
		if _, ok := liveAllowlist[name]; !ok {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: field %q không tồn tại hoặc đã xoá", si, ci, name)
		}
		return name, false, nil
	}

	// Case (b) or (c): object cell
	obj, ok := cellRaw.(map[string]any)
	if !ok {
		return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: cell phải là chuỗi hoặc object", si, ci)
	}

	// Determine if spacer: must have ONLY the key "spacer" with value true
	_, hasSpacer := obj["spacer"]
	_, hasField := obj["field"]

	// Rule 5: cell must have exactly one of {field, spacer}
	if hasSpacer && hasField {
		return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: cell không thể vừa có field vừa có spacer", si, ci)
	}
	if !hasSpacer && !hasField {
		return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: cell object phải có field hoặc spacer:true", si, ci)
	}

	if hasSpacer {
		// Rule 13: unknown keys in spacer
		for k := range obj {
			if _, ok := knownSpacerKeys[k]; !ok {
				return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: key không rõ %q trong spacer cell", si, ci, k)
			}
		}
		// spacer must be bool true
		sv, ok := obj["spacer"].(bool)
		if !ok || !sv {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: spacer phải là true (bool)", si, ci)
		}
		return "", true, nil
	}

	// Object cell with "field"
	// Rule 13: unknown keys in cell object
	for k := range obj {
		if _, ok := knownCellKeys[k]; !ok {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: key không rõ %q trong cell object", si, ci, k)
		}
	}

	// Rule 12: field must be a string
	fieldName, ok := obj["field"].(string)
	if !ok || strings.TrimSpace(fieldName) == "" {
		return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: cell.field phải là chuỗi không rỗng", si, ci)
	}
	fieldName = strings.TrimSpace(fieldName)

	// Rule 4: field allowlist
	if _, ok := liveAllowlist[fieldName]; !ok {
		return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: field %q không tồn tại hoặc đã xoá", si, ci, fieldName)
	}

	// Rule 12: labelOverride must be string if present
	if lvRaw, ok := obj["labelOverride"]; ok {
		if _, ok := lvRaw.(string); !ok {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: labelOverride phải là chuỗi", si, ci)
		}
	}

	// W5: helpText must be a string ≤200 runes if present (per-view help under the widget).
	if htRaw, ok := obj["helpText"]; ok {
		ht, isStr := htRaw.(string)
		if !isStr {
			return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: helpText phải là chuỗi", si, ci)
		}
		if n := utf8.RuneCountInString(ht); n > maxCellHelpTextRunes {
			return "", false, invalid("LAYOUT_INVALID",
				"section[%d].fields[%d]: helpText dài %d ký tự — tối đa %d", si, ci, n, maxCellHelpTextRunes)
		}
	}

	// Rule 12: required/readonly/fullWidth must be bool if present
	for _, boolKey := range []string{"required", "readonly", "fullWidth"} {
		if bRaw, ok := obj[boolKey]; ok {
			if _, ok := bRaw.(bool); !ok {
				return "", false, invalid("LAYOUT_INVALID", "section[%d].fields[%d]: %s phải là bool", si, ci, boolKey)
			}
		}
	}

	// Rule 9: colspan ∈ 1..sectionCols, mutually exclusive with fullWidth (only its presence +
	// range matter at L1 — the value drives horizontal span at L2, not anything server-side).
	hasColspan := false
	if csRaw, ok := obj["colspan"]; ok {
		hasColspan = true
		cs, err := toInt(csRaw)
		if err != nil || cs < 1 || cs > sectionCols {
			return "", false, invalid("LAYOUT_INVALID",
				"section[%d].fields[%d]: colspan %v phải là số nguyên 1..%d", si, ci, csRaw, sectionCols)
		}
	}

	hasFullWidth := false
	if fwRaw, ok := obj["fullWidth"]; ok {
		if fw, _ := fwRaw.(bool); fw {
			hasFullWidth = true
		}
	}

	if hasColspan && hasFullWidth {
		return "", false, invalid("LAYOUT_INVALID",
			"section[%d].fields[%d]: colspan và fullWidth không thể dùng cùng lúc", si, ci)
	}

	// Rule 6: FLOOR RULE — is_required field cannot have required:false
	reqRaw, hasReq := obj["required"]
	if hasReq {
		reqVal, _ := reqRaw.(bool)
		if !reqVal {
			// required:false — check if the field is required in sys_fields
			if isFieldRequired(fieldName, liveFields) {
				return "", false, invalid("LAYOUT_INVALID",
					"field %q là bắt buộc (sys_fields.is_required=true) — không thể override required:false", fieldName)
			}
		}
	}

	return fieldName, false, nil
}

// validateListColumns validates the columns[] and defaultSort for a list layout.
func validateListColumns(colsRaw any, sortRaw any, liveFields []metadata.FieldDefinition) error {
	columns, ok := colsRaw.([]any)
	if !ok {
		return invalid("LAYOUT_INVALID", "columns phải là array")
	}

	// Rule 14: ≤100 columns
	if len(columns) > 100 {
		return invalid("LAYOUT_INVALID", "quá nhiều columns (tối đa 100, hiện tại %d)", len(columns))
	}

	liveAllowlist := buildFieldAllowlist(liveFields)
	seenFields := make(map[string]bool)

	for ci, colRaw := range columns {
		col, ok := colRaw.(map[string]any)
		if !ok {
			return invalid("LAYOUT_INVALID", "columns[%d] phải là object", ci)
		}

		// Rule 13: unknown keys in column
		for k := range col {
			if _, ok := knownColumnKeys[k]; !ok {
				return invalid("LAYOUT_INVALID", "columns[%d]: key không rõ %q", ci, k)
			}
		}

		// field is required and must be a string
		fieldRaw, hasField := col["field"]
		if !hasField {
			return invalid("LAYOUT_INVALID", "columns[%d]: thiếu trường field", ci)
		}
		fieldName, ok := fieldRaw.(string)
		if !ok || strings.TrimSpace(fieldName) == "" {
			return invalid("LAYOUT_INVALID", "columns[%d]: field phải là chuỗi không rỗng", ci)
		}
		fieldName = strings.TrimSpace(fieldName)

		// Rule 4: field allowlist — list columns must reference live user fields
		if _, ok := liveAllowlist[fieldName]; !ok {
			return invalid("LAYOUT_INVALID", "columns[%d]: field %q không tồn tại hoặc đã xoá", ci, fieldName)
		}

		// Rule 8: duplicate field
		if seenFields[fieldName] {
			return invalid("LAYOUT_INVALID", "field %q xuất hiện nhiều lần trong list columns", fieldName)
		}
		seenFields[fieldName] = true

		// Rule 12: labelOverride must be string if present
		if lvRaw, ok := col["labelOverride"]; ok {
			if _, ok := lvRaw.(string); !ok {
				return invalid("LAYOUT_INVALID", "columns[%d]: labelOverride phải là chuỗi", ci)
			}
		}

		// Rule 11: width must be int >0 and ≤2000 if present
		if wRaw, ok := col["width"]; ok {
			w, err := toInt(wRaw)
			if err != nil || w <= 0 || w > 2000 {
				return invalid("LAYOUT_INVALID", "columns[%d]: width %v phải là số nguyên 1..2000", ci, wRaw)
			}
		}

		// Rule 12: sortable must be bool if present. The key is ACCEPTED for backward compatibility
		// (layouts exported before DAILY-QUEUE #4, older clients) but never trusted: GetLayout
		// re-stamps it on every served column from the engine's sort allowlist. No first-party
		// writer emits it any more (builder, default-gen, built-in packs).
		if sRaw, ok := col["sortable"]; ok {
			if _, ok := sRaw.(bool); !ok {
				return invalid("LAYOUT_INVALID", "columns[%d]: sortable phải là bool", ci)
			}
		}
	}

	// Validate defaultSort if present (required for list)
	if sortRaw == nil {
		return invalid("LAYOUT_INVALID", "view list phải có defaultSort")
	}
	sortObj, ok := sortRaw.(map[string]any)
	if !ok {
		return invalid("LAYOUT_INVALID", "defaultSort phải là object")
	}

	// Rule 13: unknown keys in defaultSort
	for k := range sortObj {
		if _, ok := knownSortKeys[k]; !ok {
			return invalid("LAYOUT_INVALID", "defaultSort: key không rõ %q", k)
		}
	}

	// Rule 10: defaultSort.field ∈ the engine's own ?sort= allowlist (system columns + unique +
	// relation/user FK + indexed — DAILY-QUEUE #273). ONE allowlist (serveTimeSortable ->
	// buildSortAllowlist), so a saved default sort can never be one the list endpoint refuses.
	dsField, _ := sortObj["field"].(string)
	sortable, err := serveTimeSortable(liveFields)
	if err != nil {
		return fmt.Errorf("layout.validateListColumns: %w", err)
	}
	if !sortable(dsField) {
		return invalid("LAYOUT_INVALID",
			"defaultSort.field %q không hợp lệ (chỉ sắp xếp được theo created_at, updated_at, id hoặc trường duy nhất / quan hệ / đã đánh chỉ mục)", dsField)
	}

	// Rule 10: defaultSort.order ∈ {asc, desc}
	dsOrder, _ := sortObj["order"].(string)
	if _, ok := validSortOrders[dsOrder]; !ok {
		return invalid("LAYOUT_INVALID",
			"defaultSort.order %q không hợp lệ (chỉ chấp nhận: asc, desc)", dsOrder)
	}

	return nil
}

// validateListViewFields validates the OPTIONAL declared view fields on a list layout
// (kanbanField, calendarField — DAILY-QUEUE #5). A declaration is strict-rejected when it names a
// field that does not exist or one of the wrong type, rather than tolerated-and-ignored the way
// the read-only `sortable` key is: silently dropping it would leave the maker looking at a board
// grouped by the wrong column with nothing on screen explaining why — the exact failure this
// contract exists to end. Absent keys keep the old first-of-kind guess.
func validateListViewFields(layout map[string]any, liveFields []metadata.FieldDefinition) error {
	types := make(map[string]string, len(liveFields))
	for _, f := range liveFields {
		if isSystemField(f.Name) {
			continue
		}
		types[f.Name] = f.FieldType
	}

	for _, key := range []string{"kanbanField", "calendarField"} {
		raw, ok := layout[key]
		if !ok {
			continue
		}
		name, ok := raw.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return invalid("LAYOUT_INVALID", "%s phải là chuỗi không rỗng", key)
		}
		// Look the name up EXACTLY as it will be stored — no TrimSpace-then-lookup. " status"
		// would otherwise validate against "status", save clean, and then be dropped at serve time
		// for every caller (the serve-time prune matches on the raw stored value), leaving the
		// declaration silently dead: precisely the failure strict-reject exists to prevent.
		ft, exists := types[name]
		if !exists {
			return invalid("LAYOUT_INVALID", "%s: field %q không tồn tại hoặc đã xoá", key, name)
		}
		if _, ok := listViewFieldTypes[key][ft]; !ok {
			return invalid("LAYOUT_INVALID", "%s: field %q kiểu %q không dùng được cho view này", key, name, ft)
		}
	}
	return nil
}

// buildFieldAllowlist builds a set of allowed field names for fast lookup.
// Live (non-deleted, non-system) fields are always included.
// "id" is also included as a special identifier fallback: the default generator uses "id" as
// the list lead-column fallback (when no user field exists) and as the required cell for a
// 0-field entity's edit/detail default. Both are spec §4.2 "cột định danh" semantics.
// All other system fields remain excluded to prevent leaking internal metadata columns.
func buildFieldAllowlist(liveFields []metadata.FieldDefinition) map[string]struct{} {
	out := make(map[string]struct{}, len(liveFields)+1)
	// Always permit "id" as the identifier fallback (spec §4.2).
	out["id"] = struct{}{}
	for _, f := range liveFields {
		if isSystemField(f.Name) {
			continue
		}
		out[f.Name] = struct{}{}
	}
	return out
}

// buildRequiredSet builds a set of required field names from live fields.
func buildRequiredSet(liveFields []metadata.FieldDefinition) map[string]bool {
	out := make(map[string]bool)
	for _, f := range liveFields {
		if isSystemField(f.Name) {
			continue
		}
		if f.IsRequired {
			out[f.Name] = true
		}
	}
	return out
}

// isFieldRequired reports whether fieldName has is_required=true in the live fields slice.
func isFieldRequired(fieldName string, liveFields []metadata.FieldDefinition) bool {
	for _, f := range liveFields {
		if f.Name == fieldName {
			return f.IsRequired
		}
	}
	return false
}

// toInt coerces a JSON-decoded value (float64 from encoding/json) to int.
// JSON numbers are always float64 when decoded into interface{}.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("not an integer: %v", v)
		}
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}
