package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metadata"
)

// LayoutMetaReader is the subset of metadata.Repository the layout service needs for
// reading entity + field metadata. Declared on the consumer side for DI / testability.
type LayoutMetaReader interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// LayoutRepo is the subset of metadata.LayoutRepository the layout service needs.
type LayoutRepo interface {
	GetByEntity(ctx context.Context, tenantID, entityName string) ([]metadata.LayoutDefinition, error)
	GetByID(ctx context.Context, tenantID, id string) (*metadata.LayoutDefinition, error)
	Upsert(ctx context.Context, layout *metadata.LayoutDefinition) error
	UpdateByID(ctx context.Context, tenantID, id, layoutJSON string) error
	HardDelete(ctx context.Context, tenantID, id string) error
}

// LayoutService orchestrates layout operations: default generation, validation, and CRUD.
type LayoutService struct {
	meta   LayoutMetaReader
	repo   LayoutRepo
	logger *zap.Logger
	// auditor receives one __layout__ row per committed write (specs/062 L3); noop until WithAuditor.
	auditor audit.Auditor
	// draftMode: nil = every write is audited. Layout IS a draft type since specs/063 S7-3, so main.go
	// passes the DraftGate and a builder's draft-only save leaves no row; see TestLayoutAudit_LayoutIsADraftType.
	draftMode docWriteModer
	// cutoff hides fields born after the last publish from a live reader (specs/063 S7-8); nil = never.
	cutoff schemaCutoffer
}

// WithSchemaCutoff wires the S7-8 hiding: GetLayout serves a live (non-overlay) reader of a delivered
// tenant only the fields created at or before the last publish, in the generated default AND in the
// "Mới thêm" section. Chainable.
func (s *LayoutService) WithSchemaCutoff(c schemaCutoffer) *LayoutService {
	s.cutoff = c
	return s
}

// NewLayoutService wires the layout service.
func NewLayoutService(meta LayoutMetaReader, repo LayoutRepo, logger *zap.Logger) *LayoutService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &LayoutService{meta: meta, repo: repo, logger: logger, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for layout writes (SaveLayout / UpdateLayout / DeleteLayout) and
// the optional draft mode that suppresses rows for draft-only writes (nil = audit every write).
// Chainable; a nil auditor keeps the noop.
func (s *LayoutService) WithAuditor(a audit.Auditor, mode docWriteModer) *LayoutService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

// layoutAuditDoc is the audit snapshot of one layout: keyed "<entity>/<view>" so the row names the
// screen even on an update diff (unchanged keys are dropped from it). The stored JSON is decoded so
// the diff is structured; an undecodable body (cannot happen for a validated save) stays a string.
func layoutAuditDoc(entityName, viewType, layoutJSON string) map[string]any {
	var body any
	if err := json.Unmarshal([]byte(layoutJSON), &body); err != nil {
		body = layoutJSON
	}
	return map[string]any{entityName + "/" + viewType: body}
}

// GetLayout returns the layout for all 3 views (or just the requested view when view is non-empty).
// If no custom layout is stored for a view, generateDefault is used (without persisting).
// Returns 404 ENTITY_NOT_FOUND if entity does not exist/is live for this tenant.
func (s *LayoutService) GetLayout(ctx context.Context, tenantID, entityName string, viewFilter string) (*GetLayoutResult, error) {
	// Resolve entity — validates tenant scoping and liveness
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return nil, fmt.Errorf("layout.GetLayout: resolve entity: %w", err)
	}

	// Fetch live fields for this entity (ListFields returns name ASC)
	liveFields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("layout.GetLayout: list fields: %w", err)
	}
	// S7-8: a field created after the last publish is not the customer's yet. Filtering the one list
	// covers generateDefault, appendFieldsNewerThan, the sortable allowlist and serve-time tolerance.
	if s.cutoff != nil {
		at, apply, cerr := s.cutoff.SchemaCutoff(ctx, tenantID)
		if cerr != nil {
			return nil, fmt.Errorf("layout.GetLayout: schema cutoff: %w", cerr)
		}
		if apply {
			liveFields = HideFieldsNewerThan(liveFields, at)
		}
	}

	// Fetch all stored custom layouts for this (tenant, entity)
	stored, err := s.repo.GetByEntity(ctx, tenantID, entityName)
	if err != nil {
		return nil, fmt.Errorf("layout.GetLayout: fetch layouts: %w", err)
	}

	// Index stored layouts by view_type for O(1) lookup
	storedByView := make(map[string]*metadata.LayoutDefinition, len(stored))
	for i := range stored {
		storedByView[stored[i].ViewType] = &stored[i]
	}

	// Determine which views to include
	views := []string{LayoutViewEdit, LayoutViewDetail, LayoutViewList}
	if vf := strings.TrimSpace(viewFilter); vf != "" {
		if _, ok := validViewTypes[vf]; !ok {
			return nil, invalid("LAYOUT_INVALID", "view_type %q không hợp lệ", viewFilter)
		}
		views = []string{vf}
	}

	result := &GetLayoutResult{
		EntityName:   entityName,
		Views:        make(map[string]*LayoutViewResult, len(views)),
		SystemFields: []string{"created_at", "updated_at", "created_by"},
	}

	// SORTABLE IS DECIDED HERE (DAILY-QUEUE #4): the list view's `sortable` flags come from the
	// engine's own ?sort= allowlist, never from what a writer stored. Before this, default-gen
	// hard-coded false on every column, the Studio builder only flagged system columns, and the
	// packs flagged plain text fields — so every header was either a dead button or a lying one
	// (click → 422 SORT_NOT_ALLOWED). One decider at serve time also means every layout saved
	// before this slice self-heals on its next GET. Fail-soft: a field whose options JSON does
	// not parse would break the record List anyway; here it only yields dead headers, never a 500.
	sortable, serr := serveTimeSortable(liveFields)
	if serr != nil {
		s.logger.Warn("layout.GetLayout: cannot derive sort allowlist, serving every column as not sortable",
			zap.String("tenant_id", tenantID), zap.String("entity_name", entityName), zap.Error(serr))
	}

	for _, vt := range views {
		if row, ok := storedByView[vt]; ok {
			// Custom layout exists — apply serve-time tolerance (§4.5)
			data, err := applyServeTimeTolerance(row.LayoutJSON, vt, liveFields)
			if err != nil {
				// Malformed stored JSON — fall back to default rather than 500
				s.logger.Warn("layout.GetLayout: malformed stored layout_json, falling back to default",
					zap.String("tenant_id", tenantID),
					zap.String("entity_name", entityName),
					zap.String("view_type", vt),
					zap.Error(err),
				)
				data = generateDefault(entityName, vt, liveFields)
				result.Views[vt] = &LayoutViewResult{IsDefault: true, LayoutData: applyServeTimeSortable(data, vt, sortable)}
				continue
			}
			// SYMMETRIC tolerance (EASE Nước 3): tolerance used to only PRUNE dead fields — a
			// field created AFTER the layout was last saved silently never appeared on the
			// customer's form/detail ("the saved-layout ratchet": add field → open the app →
			// nothing there → 'product is broken'). Now fields BORN after the stored row's
			// UpdatedAt append into a trailing "Mới thêm" section at serve time. Deliberate
			// omissions stay respected: any save bumps UpdatedAt above every existing field's
			// CreatedAt, so only genuinely-new fields auto-follow. edit/detail only — list
			// stays a curated column set (the builder palette surfaces unplaced fields there).
			if vt == "edit" || vt == "detail" {
				data = appendFieldsNewerThan(data, liveFields, row.UpdatedAt)
			}
			view := &LayoutViewResult{IsDefault: false, ID: row.ID, LayoutData: applyServeTimeSortable(data, vt, sortable)}
			if row.DraftState == draftStateEdited || row.DraftState == draftStateNew {
				view.DraftState = row.DraftState
			}
			result.Views[vt] = view
		} else {
			// No custom layout — generate default (not persisted)
			data := generateDefault(entityName, vt, liveFields)
			result.Views[vt] = &LayoutViewResult{IsDefault: true, LayoutData: applyServeTimeSortable(data, vt, sortable)}
		}
	}

	return result, nil
}

// serveTimeSortable returns the predicate "can the list endpoint sort on this field?" for the
// entity's live fields — the SAME allowlist RecordService.List enforces (buildSortAllowlist over
// toFieldSpecs: system columns + owner + unique + relation/user FK + D1 indexed + polymorphic
// sub-columns), so a header the layout marks sortable is a header whose ?sort= answers 200.
// There is deliberately no second rule here to drift from the engine's. On a conversion error
// the predicate answers false for everything (dead headers, never a 500 or a lying header); the
// caller logs the error.
func serveTimeSortable(liveFields []metadata.FieldDefinition) (func(string) bool, error) {
	specs, err := toFieldSpecs(liveFields)
	if err != nil {
		return func(string) bool { return false }, fmt.Errorf("layout.serveTimeSortable: %w", err)
	}
	allow := buildSortAllowlist(specs)
	return func(field string) bool { _, ok := allow[field]; return ok }, nil
}

// applyServeTimeSortable stamps `sortable` on every column of a LIST layout from the sortable
// predicate, overriding whatever a writer stored (a pack literal, an old builder save) in either
// direction. Other views pass through untouched. Serve-time only: nothing is persisted, and the
// input map is not mutated (columns are re-built as shallow copies).
func applyServeTimeSortable(layout map[string]any, viewType string, sortable func(string) bool) map[string]any {
	if viewType != LayoutViewList || layout == nil {
		return layout
	}
	columns, ok := layout["columns"].([]any)
	if !ok {
		return layout
	}
	newCols := make([]any, 0, len(columns))
	for _, colRaw := range columns {
		col, ok := colRaw.(map[string]any)
		if !ok {
			newCols = append(newCols, colRaw)
			continue
		}
		field, _ := col["field"].(string)
		out := cloneMapExcept(col)
		out["sortable"] = sortable(field)
		newCols = append(newCols, out)
	}
	result := cloneMapExcept(layout)
	result["columns"] = newCols
	// A saved defaultSort that stopped being sortable (field un-indexed or deleted after the save)
	// would 422 SORT_NOT_ALLOWED on every list load — fall back to the system default instead.
	if ds, ok := layout["defaultSort"].(map[string]any); ok {
		if f, _ := ds["field"].(string); !sortable(f) {
			result["defaultSort"] = map[string]any{"field": "created_at", "order": "desc"}
		}
	}
	return result
}

// SaveLayout validates and upserts a layout for (tenant, entity, view).
// POST = upsert: ON DUPLICATE KEY UPDATE at DB level, race-safe via UNIQUE constraint.
func (s *LayoutService) SaveLayout(ctx context.Context, tenantID string, in SaveLayoutInput) (*metadata.LayoutDefinition, error) {
	// Keep the raw view_type — validateLayout enforces strict enum (no trim).
	vt := in.ViewType

	// Resolve entity
	entity, err := s.meta.GetEntityByName(ctx, tenantID, in.EntityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", in.EntityName)
		}
		return nil, fmt.Errorf("layout.SaveLayout: resolve entity: %w", err)
	}

	// Fetch live fields for validation
	liveFields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("layout.SaveLayout: list fields: %w", err)
	}

	// Validate — §4.4 all rules via the EXPORTED wrapper (ONE function body shared with the AI
	// Builder's propose_layout tool: config-time parity is structural, not mirrored — M6 §13).
	// ValidateLayoutConfig applies the user-field filter internally.
	if err := ValidateLayoutConfig(vt, in.LayoutJSON, liveFields); err != nil {
		return nil, err
	}

	// Marshal layout_json for storage
	jsonBytes, err := json.Marshal(in.LayoutJSON)
	if err != nil {
		return nil, fmt.Errorf("layout.SaveLayout: marshal: %w", err)
	}
	jsonStr := string(jsonBytes)

	row := &metadata.LayoutDefinition{
		ID:         uuid.NewString(),
		TenantID:   tenantID,
		EntityName: in.EntityName,
		ViewType:   vt,
		LayoutJSON: jsonStr,
	}

	// The row this save replaces, for the audit diff (fail-open: a failed look-up must not block
	// the save — the row is then written as an update without a before side).
	var prev *metadata.LayoutDefinition
	prevKnown := true
	if stored, perr := s.repo.GetByEntity(ctx, tenantID, in.EntityName); perr != nil {
		prevKnown = false
		s.logger.Warn("layout.SaveLayout: cannot read the layout being replaced; auditing without a before side",
			zap.String("tenant_id", tenantID), zap.String("entity_name", in.EntityName), zap.Error(perr))
	} else {
		for i := range stored {
			if stored[i].ViewType == vt {
				prev = &stored[i]
				break
			}
		}
	}

	if err := s.repo.Upsert(ctx, row); err != nil {
		return nil, fmt.Errorf("layout.SaveLayout: upsert: %w", err)
	}

	action := audit.ActionCreate
	var before any
	if prev != nil {
		action = audit.ActionUpdate
		before = layoutAuditDoc(in.EntityName, vt, prev.LayoutJSON)
	} else if !prevKnown {
		action = audit.ActionUpdate
	}
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.LayoutMeta, row.ID, action,
		before, layoutAuditDoc(in.EntityName, vt, jsonStr))

	return row, nil
}

// UpdateLayout validates and updates an existing layout by id (tenant re-pinned).
// Returns 404 LAYOUT_NOT_FOUND if id is unknown or belongs to another tenant.
func (s *LayoutService) UpdateLayout(ctx context.Context, tenantID string, in UpdateLayoutInput) (*metadata.LayoutDefinition, error) {
	// Get the existing layout to determine entity_name and view_type for validation
	existing, err := s.repo.GetByID(ctx, tenantID, in.ID)
	if err != nil {
		if errors.Is(err, metadata.ErrLayoutNotFound) {
			return nil, invalid("LAYOUT_NOT_FOUND", "layout %q không tồn tại", in.ID)
		}
		return nil, fmt.Errorf("layout.UpdateLayout: get by id: %w", err)
	}

	// Resolve entity for field list
	entity, err := s.meta.GetEntityByName(ctx, tenantID, existing.EntityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", existing.EntityName)
		}
		return nil, fmt.Errorf("layout.UpdateLayout: resolve entity: %w", err)
	}

	liveFields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("layout.UpdateLayout: list fields: %w", err)
	}

	// Same exported wrapper as SaveLayout — one validator body across every layout write path.
	if err := ValidateLayoutConfig(existing.ViewType, in.LayoutJSON, liveFields); err != nil {
		return nil, err
	}

	jsonBytes, err := json.Marshal(in.LayoutJSON)
	if err != nil {
		return nil, fmt.Errorf("layout.UpdateLayout: marshal: %w", err)
	}
	jsonStr := string(jsonBytes)

	if err := s.repo.UpdateByID(ctx, tenantID, in.ID, jsonStr); err != nil {
		if errors.Is(err, metadata.ErrLayoutNotFound) {
			return nil, invalid("LAYOUT_NOT_FOUND", "layout %q không tồn tại", in.ID)
		}
		return nil, fmt.Errorf("layout.UpdateLayout: update: %w", err)
	}

	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.LayoutMeta, existing.ID, audit.ActionUpdate,
		layoutAuditDoc(existing.EntityName, existing.ViewType, existing.LayoutJSON),
		layoutAuditDoc(existing.EntityName, existing.ViewType, jsonStr))

	existing.LayoutJSON = jsonStr
	return existing, nil
}

// DeleteLayout hard-deletes a layout by id (tenant re-pinned for IDOR protection).
// Returns 404 LAYOUT_NOT_FOUND if id is unknown or belongs to another tenant.
func (s *LayoutService) DeleteLayout(ctx context.Context, tenantID, id string) error {
	// Read the row first: the audit row must say WHICH screen reverted to default, and the row is
	// gone after HardDelete. Unknown / cross-tenant id keeps its 404 (same sentinel as before).
	existing, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, metadata.ErrLayoutNotFound) {
			return invalid("LAYOUT_NOT_FOUND", "layout %q không tồn tại", id)
		}
		return fmt.Errorf("layout.DeleteLayout: get by id: %w", err)
	}
	if err := s.repo.HardDelete(ctx, tenantID, id); err != nil {
		if errors.Is(err, metadata.ErrLayoutNotFound) {
			return invalid("LAYOUT_NOT_FOUND", "layout %q không tồn tại", id)
		}
		return fmt.Errorf("layout.DeleteLayout: %w", err)
	}
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.LayoutMeta, id, audit.ActionDelete,
		layoutAuditDoc(existing.EntityName, existing.ViewType, existing.LayoutJSON), nil)
	return nil
}

// applyServeTimeTolerance decodes stored layout_json and silently drops cells/columns
// referencing soft-deleted (no longer live) fields. Empty sections after pruning are also
// dropped. This is the §4.5 serve-time tolerance — it never 500s, never re-persists.
func applyServeTimeTolerance(layoutJSONStr string, viewType string, liveFields []metadata.FieldDefinition) (map[string]any, error) {
	if layoutJSONStr == "" {
		return nil, fmt.Errorf("empty layout_json")
	}

	var layout map[string]any
	if err := json.Unmarshal([]byte(layoutJSONStr), &layout); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	// Build live allowlist (non-system)
	liveAllowlist := buildFieldAllowlist(liveFields)
	alive := func(name string) bool { _, ok := liveAllowlist[name]; return ok }

	switch viewType {
	case "edit", "detail":
		return pruneEditDetailLayout(layout, alive), nil
	case "list":
		return pruneListLayout(layout, alive), nil
	}
	return layout, nil
}

// StripHiddenLayoutFields returns layout (one decoded edit/detail/list view, as GetLayout serves
// it) without any cell or column naming a field in hidden — the caller's field-ACL hidden set
// (FieldACLService.HiddenFields). "Ẩn" must mean NOT SERVED: the record read already strips the
// VALUE, but a layout that still placed the field made the edit form render it empty and resend
// it as null on save, and every save of a record holding a value there came back 403
// FIELD_FORBIDDEN (../specs/001-auth-security/spec.md A6). Same walk as the serve-time prune of
// dead fields (spacers kept, a section left with no real field dropped). A list defaultSort on a
// hidden field falls back to created_at desc (defaultSort may name any engine-sortable field since
// #273, and the list allowlist excludes hidden fields — serving it would 422 SORT_NOT_ALLOWED on
// every open and leak the name). Serve-time only: nothing is
// persisted, and a nil/empty hidden set hands layout back untouched.
func StripHiddenLayoutFields(layout map[string]any, viewType string, hidden map[string]struct{}) map[string]any {
	if len(hidden) == 0 || layout == nil {
		return layout
	}
	visible := func(name string) bool { _, h := hidden[name]; return !h }
	switch viewType {
	case LayoutViewEdit, LayoutViewDetail:
		return pruneEditDetailLayout(layout, visible)
	case LayoutViewList:
		out := pruneListLayout(layout, visible)
		if ds, ok := out["defaultSort"].(map[string]any); ok {
			if f, _ := ds["field"].(string); !visible(f) {
				out = cloneMapExcept(out)
				out["defaultSort"] = map[string]any{"field": "created_at", "order": "desc"}
			}
		}
		return out
	}
	return layout
}

// pruneEditDetailLayout removes cells whose field fails keep and drops empty sections.
func pruneEditDetailLayout(layout map[string]any, keep func(string) bool) map[string]any {
	sectionsRaw, ok := layout["sections"]
	if !ok {
		return layout
	}
	sections, ok := sectionsRaw.([]any)
	if !ok {
		return layout
	}

	newSections := make([]any, 0, len(sections))
	for _, secRaw := range sections {
		sec, ok := secRaw.(map[string]any)
		if !ok {
			newSections = append(newSections, secRaw)
			continue
		}

		fieldsRaw, ok := sec["fields"]
		if !ok {
			continue
		}
		cells, ok := fieldsRaw.([]any)
		if !ok {
			continue
		}

		// Keep only cells that reference live fields (or are spacers)
		newCells := make([]any, 0, len(cells))
		hasNonSpacer := false
		for _, cellRaw := range cells {
			switch cell := cellRaw.(type) {
			case string:
				// String cell references a field by name
				if keep(cell) {
					newCells = append(newCells, cellRaw)
					hasNonSpacer = true
				}
				// Dead field: silently drop
			case map[string]any:
				if _, isSpacer := cell["spacer"]; isSpacer {
					// Spacers are kept regardless
					newCells = append(newCells, cellRaw)
					continue
				}
				fieldName, _ := cell["field"].(string)
				if fieldName == "" {
					continue
				}
				if keep(fieldName) {
					// specs/054 FR-9: a condition on a controller that is not served goes too.
					newCells = append(newCells, dropUnservedCondition(cell, keep))
					hasNonSpacer = true
				}
				// Dead field: silently drop
			}
		}

		// Drop section if no real fields remain after pruning
		if !hasNonSpacer {
			continue
		}

		newSec := cloneMapExcept(sec)
		newSec["fields"] = newCells
		newSections = append(newSections, newSec)
	}

	result := cloneMapExcept(layout)
	result["sections"] = newSections
	return result
}

// appendFieldsNewerThan is the SYMMETRIC half of serve-time tolerance (Nước 3): live fields
// created after the stored layout row's UpdatedAt that the layout does not reference get a
// trailing "Mới thêm" section, so a brand-new field is immediately visible on the customer's
// form/detail without re-saving every layout. Never mutates the input map's sections slice
// beyond appending; never persists. No-op when nothing qualifies.
func appendFieldsNewerThan(layout map[string]any, liveFields []metadata.FieldDefinition, layoutUpdatedAt time.Time) map[string]any {
	sections, _ := layout["sections"].([]any)

	// Collect every field name the layout already references (string or object cells).
	referenced := make(map[string]struct{})
	for _, secRaw := range sections {
		sec, ok := secRaw.(map[string]any)
		if !ok {
			continue
		}
		cells, _ := sec["fields"].([]any)
		for _, cellRaw := range cells {
			switch cell := cellRaw.(type) {
			case string:
				referenced[cell] = struct{}{}
			case map[string]any:
				if name, _ := cell["field"].(string); name != "" {
					referenced[name] = struct{}{}
				}
			}
		}
	}

	fresh := make([]any, 0, 2)
	for _, f := range liveFields {
		if _, seen := referenced[f.Name]; seen {
			continue
		}
		if f.CreatedAt.After(layoutUpdatedAt) {
			fresh = append(fresh, f.Name)
		}
	}
	if len(fresh) == 0 {
		return layout
	}

	out := cloneMapExcept(layout)
	out["sections"] = append(append([]any{}, sections...), map[string]any{
		"title":   "Mới thêm",
		"columns": float64(2), // decoded-JSON numeric shape (map[string]any parity)
		"fields":  fresh,
	})
	return out
}

// pruneListLayout removes columns whose field fails keep from a list layout.
func pruneListLayout(layout map[string]any, keep func(string) bool) map[string]any {
	// The declared view fields (kanbanField/calendarField, DAILY-QUEUE #5) name a single field
	// each, so they get the same treatment as a column referencing a hidden field: dropped, not
	// served. The renderer then falls back to its first-of-kind guess rather than grouping on a
	// field this user never receives. Done BEFORE the columns[] guards below: a malformed row
	// (written straight to the DB — the validator requires a columns array) would otherwise return
	// early and serve the hidden name anyway.
	layout = pruneDeclaredViewFields(layout, keep)

	colsRaw, ok := layout["columns"]
	if !ok {
		return layout
	}
	columns, ok := colsRaw.([]any)
	if !ok {
		return layout
	}

	newCols := make([]any, 0, len(columns))
	for _, colRaw := range columns {
		col, ok := colRaw.(map[string]any)
		if !ok {
			newCols = append(newCols, colRaw)
			continue
		}
		fieldName, _ := col["field"].(string)
		if keep(fieldName) {
			newCols = append(newCols, colRaw)
		}
		// Dead field: silently drop
	}

	result := cloneMapExcept(layout)
	result["columns"] = newCols
	return result
}

// pruneDeclaredViewFields drops kanbanField/calendarField when the field they name is hidden from
// this user. Copy-on-write like every other prune here: the stored layout is never mutated, and a
// layout that declares nothing (the common case) is handed straight back without allocating.
func pruneDeclaredViewFields(layout map[string]any, keep func(string) bool) map[string]any {
	result, cloned := layout, false
	for _, key := range []string{"kanbanField", "calendarField"} {
		name, ok := result[key].(string)
		if !ok || keep(name) {
			continue
		}
		if !cloned {
			result, cloned = cloneMapExcept(layout), true
		}
		delete(result, key)
	}
	return result
}

// cloneMapExcept makes a shallow copy of m (all key-value pairs). The "except" name is
// a reminder that callers then overwrite specific keys in the copy.
func cloneMapExcept(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
