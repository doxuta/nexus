package services

// LayoutViewType is the canonical type for a layout view name.
type LayoutViewType = string

const (
	LayoutViewEdit   LayoutViewType = "edit"
	LayoutViewDetail LayoutViewType = "detail"
	LayoutViewList   LayoutViewType = "list"
)

// systemFieldSet is the centralized set of system-managed field names that are NEVER
// included in user-facing layout cells. It is shared by generateDefault and validateLayout
// to guarantee they never diverge (spec §4.2: "SYSTEM-FIELD SET tập trung").
var systemFieldSet = map[string]struct{}{
	"id":          {},
	"tenant_id":   {},
	"team_set_id": {},
	"created_by":  {},
	"modified_by": {},
	"created_at":  {},
	"updated_at":  {},
	"deleted_at":  {},
}

// isSystemField reports whether name is a system-managed field.
func isSystemField(name string) bool {
	_, ok := systemFieldSet[name]
	return ok
}

// shortScalarTypes are the field types allowed as list columns (spec §4.2).
var shortScalarTypes = map[string]struct{}{
	"text":     {},
	"number":   {},
	"date":     {},
	"datetime": {},
	"boolean":  {},
	"email":    {},
	"picklist": {},
	"currency": {},
	"rating":   {},
}

// isShortScalar reports whether a field type is allowed as a list column.
func isShortScalar(ft string) bool {
	_, ok := shortScalarTypes[ft]
	return ok
}

// heavyTypes are field types excluded from list columns (spec §4.2).
var heavyTypes = map[string]struct{}{
	"rich_text":    {},
	"multi_select": {},
	"composite":    {},
}

// isHeavy reports whether a field type is excluded from list columns.
func isHeavy(ft string) bool {
	_, ok := heavyTypes[ft]
	return ok
}

// isRefColumn reports whether a field type is a relation/user reference: a legal list column
// whose bare id the FE turns into a label through ?expand= (DAILY-QUEUE #273).
func isRefColumn(ft string) bool { return ft == "relation" || ft == "user" }

// validSortOrders are the only values allowed for defaultSort.order.
var validSortOrders = map[string]struct{}{
	"asc":  {},
	"desc": {},
}

// validViewTypes are the only values allowed for view_type.
var validViewTypes = map[string]struct{}{
	"edit":   {},
	"detail": {},
	"list":   {},
}

// SaveLayoutInput is the payload for POST /admin/layouts.
type SaveLayoutInput struct {
	EntityName string
	ViewType   string
	LayoutJSON map[string]any // decoded from request body
}

// UpdateLayoutInput is the payload for PUT /admin/layouts/:id.
type UpdateLayoutInput struct {
	ID         string
	LayoutJSON map[string]any
}

// LayoutViewResult is the per-view response data in GetLayout output.
type LayoutViewResult struct {
	IsDefault  bool           `json:"isDefault"`
	ID         string         `json:"id,omitempty"` // persisted sys_layouts row id — set ONLY for a custom (non-default) view, so a client can PUT/DELETE it by id without re-POSTing (S1)
	ViewType   string         `json:"viewType,omitempty"`
	LayoutData map[string]any `json:"layoutData"` // the decoded layout_json
	// DraftState is "edited" or "new" when this custom view is the builder's unpublished draft
	// (specs/063-draft-publish S7-3); absent everywhere else, so the customer's payload never changes.
	DraftState string `json:"draftState,omitempty"`
}

// GetLayoutResult is the full response for GET /metadata/layouts/:entity.
type GetLayoutResult struct {
	EntityName   string                       `json:"entityName"`
	Views        map[string]*LayoutViewResult `json:"views"`
	SystemFields []string                     `json:"systemFields"`
}
