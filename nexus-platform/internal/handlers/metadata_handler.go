package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// roleActionsLoader is the subset of auth.RoleRepository the permissions endpoint needs.
// Declaring it here (consumer-side) keeps MetadataHandler unit-testable without importing
// a concrete DB type.
type roleActionsLoader interface {
	ListActionsForRole(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)
}

// entityViewChecker is the slice of services.ACLService that ListEntities needs. Declared
// consumer-side like roleActionsLoader above, so the handler stays unit-testable AND the
// DECISION of "may this role view this entity" stays single-sourced in ACLService.CanViewEntity —
// the same decider middleware.RequireACL, AppService.GetNav and the ⌘K assistant already use.
type entityViewChecker interface {
	CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error)
}

// fieldViewChecker is the slice of services.FieldACLService the metadata reads need: the names
// of the fields the caller's role may not VIEW on an entity (sys_field_acl can_view=0). Declared
// consumer-side like entityViewChecker so the DECISION stays single-sourced in
// FieldACLService.HiddenFields — the SAME hidden set the record read strips with (RM-42 / A6).
// Shared by every handler in this package that must not name a hidden field (fields, layouts,
// approval labels).
type fieldViewChecker interface {
	HiddenFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (map[string]struct{}, error)
}

// fieldLockChecker is the names of the fields the caller's role may VIEW but not EDIT
// (sys_field_acl can_edit=0) — the SAME locked set FieldACLService.CheckWriteFields refuses with,
// so the field list can say it BEFORE the save instead of the 403 saying it after
// (field-lock-signal, DAILY-QUEUE #12). Kept as its OWN narrow optional interface rather than a
// second method on fieldViewChecker: nine test doubles implement HiddenFields alone and must keep
// compiling, and a handler that only needs "hidden" must not be forced to grow "locked".
type fieldLockChecker interface {
	LockedFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (map[string]struct{}, error)
}

// MetadataHandler exposes read endpoints over the metadata registry. The tenant is taken from
// the verified JWT claim that AuthMiddleware places in the request context (../specs/001-auth-security/spec.md
// §4 tenant-from-claim) — never from a ?tenantId= query param a client could forge.
type MetadataHandler struct {
	repo        *metadata.Repository
	roleActions roleActionsLoader // nil when no role-action loader is wired (permissions endpoint returns empty)
	// entityACL filters GET /metadata/entities down to what the caller may actually view.
	// nil = no filter, which is the pre-slice behaviour test harnesses rely on.
	entityACL entityViewChecker
	// fieldACL strips GET /metadata/fields/:entity down to the fields the caller may VIEW.
	// nil = every field, the pre-fix behaviour older harnesses construct.
	fieldACL fieldViewChecker
	// fieldLocks marks, per caller, the served fields the role may not EDIT (`readOnly: true`).
	// nil = no key at all, the pre-fix behaviour (the form draws an input and meets the 403 on save).
	fieldLocks fieldLockChecker
	// cutoff hides tables / fields / relationships created after the last publish from a live
	// reader of a delivered workspace (specs/063 S7-8). nil = never hide (pre-slice behaviour).
	cutoff schemaCutoffReader
	// fieldList serves the field list; production wires services.DraftFieldRepo so the builder's
	// admin reads carry their drafts. nil = the registry repo directly.
	fieldList fieldLister
	// entityMarks stamps draftState "deleted" on the tables the builder marked for deletion (S7-9).
	entityMarks entityMarker
	logger      *zap.Logger
}

// entityMarker is satisfied by *services.DraftFieldRepo (a no-op for every reader but the builder's overlay).
type entityMarker interface {
	MarkDeletedEntities(ctx context.Context, tenantID string, es []metadata.EntityDefinition) ([]metadata.EntityDefinition, error)
}

// WithEntityMarks wires the S7-9 "sẽ xoá khi xuất bản" marker of the entity list. Chainable.
func (h *MetadataHandler) WithEntityMarks(m entityMarker) *MetadataHandler {
	h.entityMarks = m
	return h
}

// schemaCutoffReader answers "hide what was created after this instant" for the caller's request:
// satisfied by *services.DraftGate (it already answers "show everything" to the builder's overlay).
type schemaCutoffReader interface {
	SchemaCutoff(ctx context.Context, tenantID string) (time.Time, bool, error)
}

// fieldLister is the one read GET /metadata/fields/:entity needs.
type fieldLister interface {
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// WithSchemaCutoff wires the S7-8 hiding of new tables, fields and relationships. Chainable.
func (h *MetadataHandler) WithSchemaCutoff(r schemaCutoffReader) *MetadataHandler {
	h.cutoff = r
	return h
}

// WithFieldLister overrides the field read (the draft-overlay repo in production). Chainable.
func (h *MetadataHandler) WithFieldLister(l fieldLister) *MetadataHandler {
	h.fieldList = l
	return h
}

// schemaCutoffFor returns (cutoff, true) when this request's reader must not see what was created
// after it; an error refuses the whole answer (fail closed: never widen what a customer sees).
func (h *MetadataHandler) schemaCutoffFor(c *gin.Context, tenantID string) (time.Time, bool, error) {
	if h.cutoff == nil {
		return time.Time{}, false, nil
	}
	return h.cutoff.SchemaCutoff(c.Request.Context(), tenantID)
}

// NewMetadataHandler builds a metadata handler over a GORM connection.
func NewMetadataHandler(db *gorm.DB, logger *zap.Logger) *MetadataHandler {
	return &MetadataHandler{repo: metadata.NewRepository(db), logger: logger}
}

// WithRoleActionsLoader wires an optional role-actions loader for GET /metadata/permissions/me
// (RM-05). When not supplied the endpoint returns an empty permissions array.
func (h *MetadataHandler) WithRoleActionsLoader(l roleActionsLoader) *MetadataHandler {
	h.roleActions = l
	return h
}

// WithEntityACL wires the per-entity "view" decider used to filter GET /metadata/entities.
// Optional on purpose: when nil the endpoint behaves exactly as before (every entity of the
// tenant), which is what the older integration harnesses construct. Any NEW harness that
// exercises non-admin metadata reads must wire it, or it silently tests the pre-fix world.
func (h *MetadataHandler) WithEntityACL(a entityViewChecker) *MetadataHandler {
	h.entityACL = a
	return h
}

// WithFieldACL wires the per-field "view" decider used to strip GET /metadata/fields/:entity.
// Optional on purpose (nil = every field), mirroring WithEntityACL — a NEW harness exercising
// non-admin metadata reads must wire it, or it silently tests the pre-fix world.
func (h *MetadataHandler) WithFieldACL(f fieldViewChecker) *MetadataHandler {
	h.fieldACL = f
	return h
}

// WithFieldLocks wires the per-field "edit" decider that marks GET /metadata/fields/:entity with
// `readOnly: true` for the caller (field-lock-signal). Optional on purpose (nil = no key),
// mirroring WithFieldACL. Production wires a FieldACLService over the SAME grant repository to
// both seams, so "hidden" and "locked" are two views of one sys_field_acl row set.
func (h *MetadataHandler) WithFieldLocks(l fieldLockChecker) *MetadataHandler {
	h.fieldLocks = l
	return h
}

// hiddenFieldsFor resolves the field names the CALLER of this request may not view on entity.
// "Ẩn" (can_view=0) must mean NOT SERVED, not merely "value stripped from the row": a layout that
// still placed the field and a field list that still described it made the edit form render it
// empty and resend it as null on save, so every save of a record holding a value there came back
// 403 FIELD_FORBIDDEN (../specs/001-auth-security/spec.md A6). (nil, nil) when no decider is wired
// or the caller is admin — the same bypass FieldACLService applies. A load error is returned so
// each caller fails CLOSED with its own neighbouring-ACL refusal, never with an unfiltered answer.
func hiddenFieldsFor(c *gin.Context, checker fieldViewChecker, tenantID, entity string) (map[string]struct{}, error) {
	if checker == nil {
		return nil, nil
	}
	sub, _ := middleware.SubjectFromContext(c)
	if sub.IsAdmin {
		return nil, nil
	}
	return checker.HiddenFields(c.Request.Context(), tenantID, sub.RoleID, entity, sub.IsAdmin)
}

// lockedFieldsFor resolves the field names the CALLER may view but not edit (the readOnly signal).
// (nil, nil) when no decider is wired or the caller is admin — same bypass as hiddenFieldsFor. The
// set it returns ALSO contains every hidden field (loadRestrictions: can't edit what you can't
// see), which is why ListFields strips hidden BEFORE it looks here: a hidden field must never be
// served, flagged or not. A load error is returned so the caller fails CLOSED — a list that
// cannot say what is locked must not claim everything is editable.
func lockedFieldsFor(c *gin.Context, checker fieldLockChecker, tenantID, entity string) (map[string]struct{}, error) {
	if checker == nil {
		return nil, nil
	}
	sub, _ := middleware.SubjectFromContext(c)
	if sub.IsAdmin {
		return nil, nil
	}
	return checker.LockedFields(c.Request.Context(), tenantID, sub.RoleID, entity, sub.IsAdmin)
}

// canSeeEntity answers "may the CALLER of this request see that a table named `name` exists?" for
// the three metadata reads. One helper, because closing only ONE of them is how the leak survives:
// measured 2026-07-30 on tenant northwind, a staff account could read 8 of 19 tables yet
// GET /metadata/entities named all 19, GET /metadata/relationships named 7 of the hidden ones, and
// GET /metadata/fields/<hidden table> returned its FULL schema with 200.
//
// (true, nil) when no decider is wired — the pre-slice behaviour older harnesses construct.
// Admin bypasses, mirroring middleware/acl.go's D3 super-gate (platform operators are a subset of
// admins), so Studio and the AI builder keep seeing the whole model.
func (h *MetadataHandler) canSeeEntity(c *gin.Context, tenantID, name string) (bool, error) {
	if h.entityACL == nil {
		return true, nil
	}
	sub, _ := middleware.SubjectFromContext(c)
	if sub.IsAdmin {
		return true, nil
	}
	return h.entityACL.CanViewEntity(c.Request.Context(), tenantID, sub.RoleID, name)
}

// RegisterRoutes mounts the metadata routes under the given group (e.g. /api/v1).
func (h *MetadataHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/metadata/entities", h.ListEntities)
	rg.GET("/metadata/fields/:entity", h.ListFields)
	rg.GET("/metadata/relationships", h.ListRelationships)
	rg.GET("/metadata/permissions/me", h.MyPermissions)
}

// ListRelationships handles GET /metadata/relationships[?entity=<name>] — every registered
// relationship (any type — 1-1, 1-N, N-N) for the tenant (or just those involving one entity).
// ENG-REL2 #4 (migration 000028) made sys_relationships the registry for 1-N `relation` fields
// too, not just N-N join tables — the lookups behind this endpoint (ListRelationshipsForEntity /
// ListAllRelationships) were broadened accordingly. Drives the App-side Related tab discovery of
// N-N links, the Studio ERD canvas edges, AND (for 1-N rows) the inverse-field/childFkColumn
// discovery an admin/Studio UI needs to surface "which field lists all the children" on the
// parent side.
func (h *MetadataHandler) ListRelationships(c *gin.Context) {
	tenantID, err := auth.GetContextTenantID(c)
	if err != nil || tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	entity := c.Query("entity")
	var rels []metadata.RelationshipDefinition
	if entity != "" {
		rels, err = h.repo.ListRelationshipsForEntity(c.Request.Context(), tenantID, entity)
	} else {
		rels, err = h.repo.ListAllRelationships(c.Request.Context(), tenantID)
	}
	if err != nil {
		h.logger.Error("handlers.ListRelationships", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn quan hệ", "code": "METADATA_QUERY_FAILED"})
		return
	}
	// S7-8: a relation created after the last publish would open a new "Liên quan" card on the
	// customer's parent page before the builder released it.
	if at, apply, cerr := h.schemaCutoffFor(c, tenantID); cerr != nil {
		h.logger.Error("handlers.ListRelationships: schema cutoff (fail closed)", zap.String("tenant_id", tenantID), zap.Error(cerr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn quan hệ", "code": "METADATA_QUERY_FAILED"})
		return
	} else if apply {
		rels = services.HideRelationshipsNewerThan(rels, at)
	}
	// A relationship row names BOTH of its ends, so returning it unfiltered handed a staff member
	// the names of tables they cannot view (measured: 7 hidden names via this endpoint alone, after
	// /metadata/entities had already been filtered — the leak simply moved next door). Require BOTH
	// ends visible: a row is only meaningful if the caller can reach both sides anyway.
	bothEndsVisible := func(rel metadata.RelationshipDefinition) (bool, error) {
		for _, end := range [2]string{rel.ParentEntityName, rel.ChildEntityName} {
			ok, aerr := h.canSeeEntity(c, tenantID, end)
			if aerr != nil || !ok {
				return false, aerr
			}
		}
		return true, nil
	}
	visibleRels := make([]metadata.RelationshipDefinition, 0, len(rels))
	for _, rel := range rels {
		ok, aerr := bothEndsVisible(rel)
		if aerr != nil {
			// Fail closed, same as the other two reads.
			h.logger.Error("handlers.ListRelationships: acl",
				zap.String("tenant_id", tenantID), zap.String("relationship_id", rel.ID), zap.Error(aerr))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn quan hệ", "code": "METADATA_QUERY_FAILED"})
			return
		}
		if ok {
			visibleRels = append(visibleRels, rel)
		}
	}
	rels = visibleRels

	responses := make([]relationshipResponse, 0, len(rels))
	for _, rel := range rels {
		r, convErr := toRelationshipResponse(rel)
		if convErr != nil {
			// RM-02 fail-soft (mirrors ListFields): a corrupt view_config string must not
			// hide the relationship — emit the row without viewConfig and log it.
			h.logger.Warn("handlers.ListRelationships: parse view_config",
				zap.String("relationship_id", rel.ID), zap.String("name", rel.Name), zap.Error(convErr))
		}
		responses = append(responses, r)
	}
	c.JSON(http.StatusOK, gin.H{"data": responses})
}

// relationshipResponse is the over-the-wire representation of a RelationshipDefinition.
// Same keys (and camelCase) as serializing the struct directly USED to produce, plus
// ENG-RELVIEW's viewConfig — decoded from the raw JSON *string stored in
// sys_relationships.view_config into a real object (never an escaped string, RM-02 —
// the same treatment fieldResponse gives sys_fields.options).
type relationshipResponse struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	ParentEntityName string         `json:"parentEntityName"`
	ChildEntityName  string         `json:"childEntityName"`
	RelationshipType string         `json:"relationshipType"`
	JoinTableName    *string        `json:"joinTableName,omitempty"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	InverseField     string         `json:"inverseField,omitempty"`
	ChildFKColumn    string         `json:"childFkColumn,omitempty"`
	ViewConfig       map[string]any `json:"viewConfig,omitempty"` // parsed from JSON string — never a raw string
}

// toRelationshipResponse converts a RelationshipDefinition (ViewConfig as *string) to
// relationshipResponse (ViewConfig as map). A nil/empty stored config yields nil (omitted
// from JSON). A non-parseable config string is treated as nil; the caller logs it.
func toRelationshipResponse(rel metadata.RelationshipDefinition) (relationshipResponse, error) {
	r := relationshipResponse{
		ID:               rel.ID,
		Name:             rel.Name,
		ParentEntityName: rel.ParentEntityName,
		ChildEntityName:  rel.ChildEntityName,
		RelationshipType: rel.RelationshipType,
		JoinTableName:    rel.JoinTableName,
		CreatedAt:        rel.CreatedAt,
		UpdatedAt:        rel.UpdatedAt,
		InverseField:     rel.InverseField,
		ChildFKColumn:    rel.ChildFKColumn,
	}
	if rel.ViewConfig != nil && *rel.ViewConfig != "" {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(*rel.ViewConfig), &cfg); err != nil {
			return r, err
		}
		r.ViewConfig = cfg
	}
	return r, nil
}

// fieldResponse is the over-the-wire representation of a FieldDefinition. The key
// difference from the raw struct: Options is decoded from a JSON *string stored in
// sys_fields into a real map[string]any object so the frontend receives structured data
// (e.g. picklist values array) rather than an escaped JSON string (RM-02).
type fieldResponse struct {
	ID           string         `json:"id"`
	EntityID     string         `json:"entityId"`
	Name         string         `json:"name"`
	Label        string         `json:"label"`
	FieldType    string         `json:"fieldType"`
	IsRequired   bool           `json:"isRequired"`
	IsUnique     bool           `json:"isUnique"`
	DefaultValue string         `json:"defaultValue,omitempty"`
	Options      map[string]any `json:"options,omitempty"` // parsed from JSON string — never a raw string
	// FieldOrder / Description (W5, mig 000033): admin display position (list is served
	// sorted by it, tie-break name) + per-field admin description (≤500 runes).
	FieldOrder  int    `json:"fieldOrder"`
	Description string `json:"description,omitempty"`
	// ReadOnly (field-lock-signal): the CALLER's role may view this field but not edit it
	// (sys_field_acl can_edit=0). Per caller, never persisted, ADDITIVE: the key is only present
	// when true, so admins and unrestricted roles receive the byte-identical list of before. A
	// hidden field is not served at all — there is no key that says "hidden", by design (A6).
	ReadOnly bool `json:"readOnly,omitempty"`
	// IsLabel (#272): this is the field that NAMES a record of the entity (the table's chosen
	// label field, else the derived default). Additive and only present when true, on exactly one
	// served field; absent when no field can name a record or the caller may not see it. The FE
	// reads it in lib/recordLabel.ts so titles, cards and chips agree with the server's expand.
	IsLabel bool `json:"isLabel,omitempty"`
	// IsLabelExplicit (#479): the builder CHOSE this field (vs the derived default). Only on the
	// IsLabel field and only when true — lets the FE rank a relation's own labelField below a table
	// choice but above an inferred default, exactly like the server's expand.
	IsLabelExplicit bool `json:"isLabelExplicit,omitempty"`
	// DraftState / PendingRequired (specs/063 S7-8): only on the builder's draft-overlay read.
	// "new" = born after the last publish (the customer does not see it yet), "edited" = has an
	// unpublished edit; PendingRequired = "bắt buộc" is switched on in the draft, not yet live.
	DraftState      string `json:"draftState,omitempty"`
	PendingRequired bool   `json:"pendingRequired,omitempty"`
}

// toFieldResponse converts a FieldDefinition (with Options as *string) to fieldResponse
// (with Options as map[string]any). A nil or empty Options string yields nil (omitted from
// JSON). A non-parseable Options string is treated as nil and logged by the caller.
func toFieldResponse(f metadata.FieldDefinition) (fieldResponse, error) {
	r := fieldResponse{
		ID:           f.ID,
		EntityID:     f.EntityID,
		Name:         f.Name,
		Label:        f.Label,
		FieldType:    f.FieldType,
		IsRequired:   f.IsRequired,
		IsUnique:     f.IsUnique,
		DefaultValue: f.DefaultVal,
		FieldOrder:   f.FieldOrder,
		Description:  f.Description,

		DraftState:      f.DraftState,
		PendingRequired: f.PendingRequired,
	}
	if f.Options != nil && *f.Options != "" {
		var opts map[string]any
		if err := json.Unmarshal([]byte(*f.Options), &opts); err != nil {
			return r, err
		}
		r.Options = opts
	}
	return r, nil
}

// permissionResponse is one grant entry in the GET /metadata/permissions/me response.
type permissionResponse struct {
	EntityName  string `json:"entityName"`
	Action      string `json:"action"`
	AccessLevel string `json:"accessLevel"`
}

// ListEntities returns the entity definitions registered for the authenticated tenant.
// The tenant comes from the verified JWT claim in context (set by AuthMiddleware); there is
// no client-supplied override. Response is camelCase JSON per the API conventions.
func (h *MetadataHandler) ListEntities(c *gin.Context) {
	tenantID, err := auth.GetContextTenantID(c)
	if err != nil || tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}

	entities, err := h.repo.ListEntities(tenantID)
	if err != nil {
		h.logger.Error("handlers.ListEntities", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể truy vấn danh sách entity",
			"code":  "METADATA_QUERY_FAILED",
		})
		return
	}

	// S7-8: a table created after the last publish is not listed for the customer yet.
	if at, apply, cerr := h.schemaCutoffFor(c, tenantID); cerr != nil {
		h.logger.Error("handlers.ListEntities: schema cutoff (fail closed)", zap.String("tenant_id", tenantID), zap.Error(cerr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn danh sách entity", "code": "METADATA_QUERY_FAILED"})
		return
	} else if apply {
		entities = services.HideEntitiesNewerThan(entities, at)
	}

	// METADATA-ACL-FILTER: a staff member must not learn the NAME of a table they cannot open.
	// Before this, a non-admin read all 19 entity names while being able to view 8 — they saw
	// the other 11 in "Tất cả dữ liệu", clicked, and hit 403. Both a metadata leak and a dead
	// end. The decision is NOT re-implemented here: it is the same ACLService.CanViewEntity that
	// RequireACL, GetNav and ⌘K call, so the four can never disagree.
	//
	// Admin bypass mirrors middleware/acl.go's D3 super-gate (and platform operators are a
	// subset of admins — platform_operator.go requires IsAdmin), so Studio still sees everything.
	// Entities are dropped SILENTLY rather than 403'd per row: a per-row error would re-leak the
	// existence it is meant to hide (same convention as AppService.GetNav).
	{
		visible := make([]metadata.EntityDefinition, 0, len(entities))
		for _, e := range entities {
			ok, aerr := h.canSeeEntity(c, tenantID, e.Name)
			if aerr != nil {
				// FAIL CLOSED. An ACL infrastructure error must never widen what someone can see;
				// skipping the row instead would hand back a silently-incomplete list.
				h.logger.Error("handlers.ListEntities: acl", zap.String("tenant_id", tenantID), zap.Error(aerr))
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "Không thể truy vấn danh sách entity",
					"code":  "METADATA_QUERY_FAILED",
				})
				return
			}
			if ok {
				visible = append(visible, e)
			}
		}
		entities = visible
	}

	// S7-9: the builder's draft-overlay list marks the tables going away at the next Publish.
	if h.entityMarks != nil {
		marked, merr := h.entityMarks.MarkDeletedEntities(c.Request.Context(), tenantID, entities)
		if merr != nil {
			h.logger.Error("handlers.ListEntities: draft marks (fail closed)", zap.String("tenant_id", tenantID), zap.Error(merr))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn danh sách entity", "code": "METADATA_QUERY_FAILED"})
			return
		}
		entities = marked
	}

	c.JSON(http.StatusOK, gin.H{
		"data": entities,
		"meta": gin.H{"tenantId": tenantID, "total": len(entities)},
	})
}

// ListFields returns the field definitions for a named entity belonging to the
// authenticated tenant. IDOR guard: GetEntityByName is called first (tenant-scoped), so
// an entity name that belongs to another tenant or does not exist returns 404 before any
// field data is returned. Options JSON strings are parsed to objects in the response so
// the frontend receives structured picklist values rather than an escaped string (RM-02).
func (h *MetadataHandler) ListFields(c *gin.Context) {
	tenantID, err := auth.GetContextTenantID(c)
	if err != nil || tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}

	entityName := c.Param("entity")
	ctx := c.Request.Context()

	// Step 0: may this caller even know the table exists? A staff member without a view grant used
	// to receive the FULL schema of any table whose name they could guess (measured: 200 on
	// /metadata/fields/tk_tickets for an account that gets 403 on the data itself). The answer is
	// 404 with the SAME body as a genuinely absent entity, not 403 — the project's anti-enumeration
	// rule (api-conventions §Error Codes #3): a refusal must not confirm existence.
	if visible, aerr := h.canSeeEntity(c, tenantID, entityName); aerr != nil {
		h.logger.Error("handlers.ListFields: acl",
			zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(aerr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn entity", "code": "METADATA_QUERY_FAILED"})
		return
	} else if !visible {
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	}

	// Step 0b: which of its fields may this caller even know about? Resolved BEFORE the reads so a
	// grant-load failure refuses the whole answer (fail closed — the same 500 the entity check
	// gives) rather than serving a field the role might be denied. Admin / unwired → nil (all).
	hidden, herr := hiddenFieldsFor(c, h.fieldACL, tenantID, entityName)
	if herr != nil {
		h.logger.Error("handlers.ListFields: field acl (fail closed)",
			zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(herr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn entity", "code": "METADATA_QUERY_FAILED"})
		return
	}
	// Step 0c: which of the SERVED fields may this caller not edit? Same fail-closed refusal as the
	// hidden load — a list that cannot say what is locked must not claim everything is editable.
	// ponytail: second grant load per request (same tiny indexed SELECT); fold both into one
	// loadRestrictions call if sys_field_acl ever shows up in a profile.
	locked, lerr := lockedFieldsFor(c, h.fieldLocks, tenantID, entityName)
	if lerr != nil {
		h.logger.Error("handlers.ListFields: field locks (fail closed)",
			zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(lerr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn entity", "code": "METADATA_QUERY_FAILED"})
		return
	}

	// Step 1: resolve entity within THIS tenant — 404 on cross-tenant or absent entity.
	entity, err := h.repo.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if err == metadata.ErrEntityNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.ListFields: get entity",
			zap.String("tenant_id", tenantID),
			zap.String("entity", entityName),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn entity", "code": "METADATA_QUERY_FAILED"})
		return
	}

	// Step 1b (S7-8): a table born after the last publish does not exist for the customer yet — the
	// same 404 as an absent table, so its fields are not even enumerable by name. A cutoff read
	// failure refuses the answer (fail closed).
	cutAt, cutApply, cerr := h.schemaCutoffFor(c, tenantID)
	if cerr != nil {
		h.logger.Error("handlers.ListFields: schema cutoff (fail closed)",
			zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(cerr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn entity", "code": "METADATA_QUERY_FAILED"})
		return
	}
	if cutApply && entity.CreatedAt.After(cutAt) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	}

	// Step 2: list fields by entity ID with explicit tenant predicate (RM-18 defense-in-depth).
	var lister fieldLister = h.repo
	if h.fieldList != nil {
		lister = h.fieldList
	}
	rawFields, err := lister.ListFields(ctx, tenantID, entity.ID)
	if err == nil && cutApply {
		rawFields = services.HideFieldsNewerThan(rawFields, cutAt)
	}
	if err != nil {
		h.logger.Error("handlers.ListFields: list fields",
			zap.String("tenant_id", tenantID),
			zap.String("entity_id", entity.ID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể truy vấn fields", "code": "METADATA_QUERY_FAILED"})
		return
	}

	// Step 3: convert to response DTOs — parse Options *string → map[string]any.
	labelName, labelExplicit := metadata.ResolveLabelField(entity.Options, rawFields)
	responses := make([]fieldResponse, 0, len(rawFields))
	for _, f := range rawFields {
		if _, isHidden := hidden[f.Name]; isHidden {
			continue // "ẩn" means not served — the form must never learn this field exists
		}
		r, convErr := toFieldResponse(f)
		if convErr != nil {
			h.logger.Warn("handlers.ListFields: parse options",
				zap.String("field_id", f.ID),
				zap.String("field_name", f.Name),
				zap.Error(convErr))
			// Emit the field without options rather than aborting the whole request;
			// a corrupt options string should not hide other well-formed fields.
		}
		// Hidden was stripped ABOVE, so the flag only ever lands on a field that is served.
		_, r.ReadOnly = locked[f.Name]
		r.IsLabel = labelName != "" && f.Name == labelName
		r.IsLabelExplicit = r.IsLabel && labelExplicit
		responses = append(responses, r)
	}

	c.JSON(http.StatusOK, gin.H{"data": responses})
}

// MyPermissions handles GET /metadata/permissions/me. It returns the current user's
// effective permission grants as a flat array [{entityName, action, accessLevel}].
//
// Admin users receive a single wildcard grant {entityName:"*", action:"*", accessLevel:"all"}
// so the frontend can apply a universal allow without needing to enumerate all entities.
//
// Non-admin users with a role get their actual acl_role_actions rows. A user with no role
// gets an empty array (strict default-deny until a role is assigned). All reads are
// tenant-scoped: the role_id is taken from the verified JWT subject, and
// ListActionsForRole verifies the role belongs to THIS tenant before returning grants
// (no cross-tenant action leak via a guessed role_id). (RM-05)
func (h *MetadataHandler) MyPermissions(c *gin.Context) {
	tenantID, err := auth.GetContextTenantID(c)
	if err != nil || tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}

	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}

	// Admins get a universal wildcard grant — no DB read needed.
	if sub.IsAdmin {
		c.JSON(http.StatusOK, gin.H{
			"data": []permissionResponse{
				{EntityName: "*", Action: "*", AccessLevel: "all"},
			},
		})
		return
	}

	// No role → empty grants (strict default-deny).
	if sub.RoleID == "" {
		c.JSON(http.StatusOK, gin.H{"data": []permissionResponse{}})
		return
	}

	if h.roleActions == nil {
		// Loader not wired — return empty rather than 500 (graceful degradation).
		c.JSON(http.StatusOK, gin.H{"data": []permissionResponse{}})
		return
	}

	actions, err := h.roleActions.ListActionsForRole(c.Request.Context(), tenantID, sub.RoleID)
	if err != nil {
		// A role that was deleted/revoked after the token was issued is a normal lifecycle
		// event — return empty grants rather than 500 (contract: no resolvable role → []).
		if errors.Is(err, auth.ErrRoleNotFound) {
			c.JSON(http.StatusOK, gin.H{"data": []permissionResponse{}})
			return
		}
		h.logger.Error("handlers.MyPermissions: load role actions",
			zap.String("tenant_id", tenantID),
			zap.String("role_id", sub.RoleID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể truy vấn quyền",
			"code":  "PERMISSIONS_QUERY_FAILED",
		})
		return
	}

	perms := make([]permissionResponse, 0, len(actions))
	for _, a := range actions {
		perms = append(perms, permissionResponse{
			EntityName:  a.EntityName,
			Action:      a.Action,
			AccessLevel: a.AccessLevel,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": perms})
}
