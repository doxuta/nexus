package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminEntityHandler exposes the admin endpoints that register entities (and their initial
// fields) via the metadata engine. The tenant is taken from the verified JWT claim that
// AuthMiddleware places in the request context (../specs/001-auth-security/spec.md §4 tenant-from-claim) —
// never from a client-controlled header — via the package-shared tenantOf helper.
type AdminEntityHandler struct {
	svc      *services.EntityService
	fieldSvc *services.FieldService
	logger   *zap.Logger
	// aclSeed, when set, seeds an app's two default roles + grants a newly created,
	// app-scoped entity into them (TRỤ 2 mục 4 — "quyền không còn vô hình"). nil (the default
	// for every existing caller of NewAdminEntityHandler) leaves CreateEntity's behavior exactly
	// as before: no seeding attempted, no "aclSeed" key in the response. Wire it via
	// WithACLSeeder; production wiring lives in cmd/server/main.go.
	aclSeed appACLSeeder
}

// appACLSeeder is the subset of *services.AppACLSeedService the entity handler needs: seed an
// app's two default roles (idempotent, created on first use) and grant a freshly created entity
// into both. Declared consumer-side so the handler stays unit-testable with a fake and does not
// import a concrete service type it does not otherwise depend on.
type appACLSeeder interface {
	SeedForEntity(ctx context.Context, tenantID, appID, entityName string) (seeded bool, warning string, err error)
}

// NewAdminEntityHandler wires the handler over the entity service and the field service
// (the latter powers the /admin/fields endpoints — edit field, etc.).
func NewAdminEntityHandler(svc *services.EntityService, fieldSvc *services.FieldService, logger *zap.Logger) *AdminEntityHandler {
	return &AdminEntityHandler{svc: svc, fieldSvc: fieldSvc, logger: logger}
}

// WithACLSeeder attaches the TRỤ 2 mục 4 app-ACL seeder to CreateEntity. Chainable; a nil seeder
// is ignored (keeps the pre-existing no-op behavior for any caller that opts out).
func (h *AdminEntityHandler) WithACLSeeder(s appACLSeeder) *AdminEntityHandler {
	if s != nil {
		h.aclSeed = s
	}
	return h
}

// RegisterRoutes mounts the admin entity routes under the given group (e.g. /api/v1).
// NOTE: the static /admin/fields/reorder segment coexists with the param route
// /admin/fields/:id — gin resolves static segments first (same pattern as
// /admin/workflows/runs vs /admin/workflows/:id/runs).
func (h *AdminEntityHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/entities", h.CreateEntity)
	rg.PUT("/admin/entities/:name", h.UpdateEntity)
	rg.DELETE("/admin/entities/:name", h.DeleteEntity)
	rg.POST("/admin/fields", h.AddField)
	rg.PUT("/admin/fields/reorder", h.ReorderFields)
	rg.PUT("/admin/fields/:id", h.EditField)
	rg.DELETE("/admin/fields/:id", h.DeleteField)
	// specs/083: "Tính lại" — recompute a stored roll-up over every existing row.
	rg.POST("/admin/fields/:id/recompute", h.RecomputeField)
	// W5 §3.2 live formula editor: config-time dry-run, same codes as the field-create path.
	rg.POST("/admin/formula/validate", h.ValidateFormula)
}

type fieldRequest struct {
	Name       string         `json:"name"`
	Label      string         `json:"label"`
	FieldType  string         `json:"fieldType"`
	IsRequired bool           `json:"isRequired"`
	IsUnique   bool           `json:"isUnique"`
	Options    map[string]any `json:"options"`
	// FieldOrder (W5): explicit display position (≥0). Omitted/null = auto-assign by slice
	// position at entity-create (or append at add-field). Description: admin help text ≤500 runes.
	FieldOrder  *int   `json:"fieldOrder"`
	Description string `json:"description"`
}

type createEntityRequest struct {
	Name        string         `json:"name"`
	Label       string         `json:"label"`
	Description string         `json:"description"`
	Fields      []fieldRequest `json:"fields"`
	// AppID (TRỤ 2 mục 4, optional): the id of the app this entity is being built under. When
	// present AND a seeder is wired (WithACLSeeder), CreateEntity seeds the app's two default
	// roles and grants this entity into them so it is immediately visible to non-admin staff
	// instead of defaulting to invisible. Omitted/empty → no seeding, exactly like before this
	// field existed (vertical-pack installs and every other existing caller never set it).
	AppID string `json:"appId"`
}

func (r createEntityRequest) toInput() services.CreateEntityInput {
	fields := make([]services.FieldInput, len(r.Fields))
	for i, f := range r.Fields {
		fields[i] = services.FieldInput{
			Name:        f.Name,
			Label:       f.Label,
			FieldType:   f.FieldType,
			IsRequired:  f.IsRequired,
			IsUnique:    f.IsUnique,
			Options:     f.Options,
			FieldOrder:  f.FieldOrder,
			Description: f.Description,
		}
	}
	return services.CreateEntityInput{
		Name:        r.Name,
		Label:       r.Label,
		Description: r.Description,
		Fields:      fields,
	}
}

// CreateEntity handles POST /admin/entities: it registers the entity, generates its obj_*
// table, and returns the created definition. Validation/duplicate errors are mapped to the
// appropriate HTTP status.
func (h *AdminEntityHandler) CreateEntity(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var req createEntityRequest
	if !bindJSON(c, &req) {
		return
	}

	entity, fields, err := h.svc.CreateEntity(auditContext(c), tenantID, req.toInput())
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.CreateEntity",
			zap.String("tenant_id", tenantID), zap.String("name", req.Name), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tạo entity", "code": "ENTITY_CREATE_FAILED"})
		return
	}

	// TRỤ 2 mục 4: the entity is already created and committed at this point — everything below
	// is FAIL-OPEN by design (spec: "entity vẫn phải tạo được"). appId absent, or no seeder
	// wired, silently skips (aclSeed.seeded=false — the honest "chưa ai ngoài quản trị viên thấy
	// bảng này" signal FE reads, per spec item 2 — needs no extra query: a brand-new entity has
	// zero grants until this seed step runs).
	appID := strings.TrimSpace(req.AppID)
	aclSeed := gin.H{"appScoped": appID != "", "seeded": false}
	if appID != "" && h.aclSeed != nil {
		seeded, warning, serr := h.aclSeed.SeedForEntity(auditContext(c), tenantID, appID, entity.Name)
		if serr != nil {
			h.logger.Warn("handlers.CreateEntity.aclSeed",
				zap.String("tenant_id", tenantID), zap.String("entity", entity.Name), zap.String("app_id", appID), zap.Error(serr))
		}
		aclSeed["seeded"] = seeded
		if warning != "" {
			aclSeed["warning"] = warning
		}
	}

	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"entity": entity, "fields": fields, "aclSeed": aclSeed}})
}

// editFieldRequest is the PUT /admin/fields/:id body. fieldType is accepted only so the
// service can reject a change attempt (422); it is never written. Pointer fields use
// apply-when-non-nil semantics (spec §7.2 B-2): an omitted key stays nil and leaves the
// stored value untouched, while an explicit value (even false / "") is applied.
type editFieldRequest struct {
	Label        *string         `json:"label"`
	IsRequired   *bool           `json:"isRequired"`
	DefaultValue *string         `json:"defaultValue"`
	FieldType    string          `json:"fieldType"`
	Options      *map[string]any `json:"options"`
	// FieldOrder / Description (W5): mutable, apply-when-non-nil like the other pointers.
	FieldOrder  *int    `json:"fieldOrder"`
	Description *string `json:"description"`
	// Indexed (#56): set options.indexed only, merged into the options as they stand (a pending
	// draft's included). Not combinable with options (422 FIELD_INDEXED_WITH_OPTIONS).
	Indexed *bool `json:"indexed"`
}

// EditField handles PUT /admin/fields/:id: it updates a field's mutable attributes
// (label/isRequired/defaultValue/options) on the tenant-scoped field. Changing fieldType is
// rejected with 422; an unknown/cross-tenant field id returns 404. No schema ALTER runs.
func (h *AdminEntityHandler) EditField(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	fieldID := strings.TrimSpace(c.Param("id"))
	if fieldID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu id field", "code": "FIELD_ID_REQUIRED"})
		return
	}

	var req editFieldRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.EditFieldInput{
		FieldID:     fieldID,
		Label:       req.Label,
		IsRequired:  req.IsRequired,
		DefaultVal:  req.DefaultValue,
		FieldType:   req.FieldType,
		FieldOrder:  req.FieldOrder,
		Description: req.Description,
		Indexed:     req.Indexed,
	}
	if req.Options != nil {
		in.Options = *req.Options
	}

	field, err := h.fieldSvc.EditField(auditContext(c), tenantID, in)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.EditField",
			zap.String("tenant_id", tenantID), zap.String("field_id", fieldID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể sửa field", "code": "FIELD_EDIT_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"field": field}})
}

// RecomputeField handles POST /admin/fields/:id/recompute (specs/083): recomputes a stored roll-up
// on every row of its entity and answers how many rows changed. Tenant-scoped; a field that is not
// a stored roll-up is 422, an unknown/cross-tenant id is 404.
func (h *AdminEntityHandler) RecomputeField(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	fieldID := strings.TrimSpace(c.Param("id"))
	if fieldID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu id field", "code": "FIELD_ID_REQUIRED"})
		return
	}
	n, err := h.fieldSvc.RecomputeField(auditContext(c), tenantID, fieldID)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.RecomputeField",
			zap.String("tenant_id", tenantID), zap.String("field_id", fieldID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tính lại trường tổng hợp", "code": "FIELD_RECOMPUTE_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"updated": n}})
}

// addFieldRequest is the POST /admin/fields body: it names the target entity and carries
// the new field definition. The entity must already exist (and belong to the tenant).
type addFieldRequest struct {
	EntityName string         `json:"entityName"`
	Name       string         `json:"name"`
	Label      string         `json:"label"`
	FieldType  string         `json:"fieldType"`
	IsRequired bool           `json:"isRequired"`
	IsUnique   bool           `json:"isUnique"`
	Options    map[string]any `json:"options"`
	// FieldOrder (W5): explicit display position (≥0); omitted = append after the entity's
	// current max (+10). Description: admin help text ≤500 runes.
	FieldOrder  *int   `json:"fieldOrder"`
	Description string `json:"description"`
}

// AddField handles POST /admin/fields: it adds a single field to an already-registered,
// tenant-scoped entity. Ordinary fields are a pure metadata write; unique fields ALTER the
// obj_* table to add a generated column + index. Validation/duplicate/not-found errors map
// to the appropriate HTTP status.
func (h *AdminEntityHandler) AddField(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var req addFieldRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.AddFieldInput{
		EntityName: req.EntityName,
		Field: services.FieldInput{
			Name:        req.Name,
			Label:       req.Label,
			FieldType:   req.FieldType,
			IsRequired:  req.IsRequired,
			IsUnique:    req.IsUnique,
			Options:     req.Options,
			FieldOrder:  req.FieldOrder,
			Description: req.Description,
		},
	}

	field, err := h.fieldSvc.AddField(auditContext(c), tenantID, in)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AddField",
			zap.String("tenant_id", tenantID), zap.String("entity", req.EntityName), zap.String("field", req.Name), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể thêm field", "code": "FIELD_ADD_FAILED"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"field": field}})
}

// DeleteField handles DELETE /admin/fields/:id: it SOFT-deletes a field on the tenant-scoped
// entity (marks sys_fields.deleted_at). Per §4.4 NO schema ALTER runs and the underlying
// obj_* column/data is preserved, so the operation is reversible. An unknown/cross-tenant
// field id returns 404.
func (h *AdminEntityHandler) DeleteField(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	fieldID := strings.TrimSpace(c.Param("id"))
	if fieldID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu id field", "code": "FIELD_ID_REQUIRED"})
		return
	}

	err := h.fieldSvc.DeleteField(auditContext(c), tenantID, services.DeleteFieldInput{FieldID: fieldID})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.DeleteField",
			zap.String("tenant_id", tenantID), zap.String("field_id", fieldID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá field", "code": "FIELD_DELETE_FAILED"})
		return
	}

	// 204 No Content: the field row is soft-deleted (data preserved); no body per
	// api-conventions (DELETE returns 204 on success).
	c.Status(http.StatusNoContent)
}

// updateEntityRequest is the PUT /admin/entities/:name body. All fields are optional
// (pointer semantics): an omitted key leaves the stored value untouched, matching the
// same apply-when-non-nil pattern used by editFieldRequest. Name and tableName are
// rejected server-side (immutable schema-bound identifiers).
type updateEntityRequest struct {
	Label       *string        `json:"label"`
	Description *string        `json:"description"`
	Options     map[string]any `json:"options"`
}

// UpdateEntity handles PUT /admin/entities/:name: it updates the mutable attributes of a
// tenant-scoped entity (label/description/options). Changing the entity name is rejected
// at the service layer — names are schema-bound identifiers. An unknown/cross-tenant name
// returns 404; a missing required field (label) returns 422.
func (h *AdminEntityHandler) UpdateEntity(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	entityName := strings.TrimSpace(c.Param("name"))
	if entityName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu tên entity", "code": "ENTITY_NAME_REQUIRED"})
		return
	}

	var req updateEntityRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.UpdateEntityInput{
		Label:       req.Label,
		Description: req.Description,
		Options:     req.Options,
	}

	entity, err := h.svc.UpdateEntity(auditContext(c), tenantID, entityName, in)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.UpdateEntity",
			zap.String("tenant_id", tenantID), zap.String("name", entityName), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể cập nhật entity", "code": "ENTITY_UPDATE_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entity": entity}})
}

// DeleteEntity handles DELETE /admin/entities/:name: it SOFT-deletes a tenant-scoped entity
// (sets sys_entities.deleted_at and tombstone-renames the `name` column so the unique index
// frees the original name for future recreate). The physical obj_* table is preserved
// (data is never dropped by this endpoint). An unknown/cross-tenant name returns 404.
func (h *AdminEntityHandler) DeleteEntity(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	entityName := strings.TrimSpace(c.Param("name"))
	if entityName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu tên entity", "code": "ENTITY_NAME_REQUIRED"})
		return
	}

	err := h.svc.SoftDeleteEntity(auditContext(c), tenantID, entityName)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.DeleteEntity",
			zap.String("tenant_id", tenantID), zap.String("name", entityName), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá entity", "code": "ENTITY_DELETE_FAILED"})
		return
	}

	// 204 No Content: entity is soft-deleted; physical table preserved.
	c.Status(http.StatusNoContent)
}

// statusForCode maps a ValidationError code to an HTTP status. Duplicate -> 409; not-found
// -> 404; everything else validation-related (incl. FIELD_TYPE_IMMUTABLE and the M3 relation
// codes FIELD_RELATION_TARGET_REQUIRED / FIELD_RELATION_TARGET_INVALID /
// FIELD_RELATION_TARGET_NOT_FOUND / RECORD_RELATION_INVALID) -> 422 (well-formed but
// semantically invalid, per api-conventions). FIELD_RELATION_TARGET_NOT_FOUND is 422 (not
// 404): the request itself is the field-definition, which is unprocessable because it names a
// missing target — it is not a missing-resource lookup of the field being addressed.
// RELATIONSHIP_ALREADY_EXISTS (Finding A) belongs in the duplicate/409 group: it is raised by
// BOTH the N-N RegisterRelationship duplicate-name path (relation_service.go) and a relation
// field's auto-registered 1-N registry-row collision (field/entity orchestration) — the SAME
// code must map to the SAME status regardless of which writer raised it.
// ENG-REL2 #5 adds two more 409s: FIELD_RELATION_CARDINALITY_CONFLICT (existing shared-table
// rows already violate a new 1-1 field's exclusivity — mirrors CROSS_TENANT_SHARED_UNIQUE) and
// RECORD_RELATION_ALREADY_BOUND (a record write trips a 1-1 field's tenant-scoped UNIQUE key —
// mirrors RECORD_DUPLICATE). FIELD_RELATION_CARDINALITY_INVALID (config-time, bad
// options.cardinality) is left to the 422 default, like every other FIELD_* config-shape code.
func statusForCode(code string) int {
	switch code {
	case "ENTITY_ALREADY_EXISTS", "FIELD_ALREADY_EXISTS", "RECORD_DUPLICATE", "USER_ALREADY_EXISTS", "ROLE_ALREADY_EXISTS", "TEAM_ALREADY_EXISTS", "CROSS_TENANT_SHARED_UNIQUE", "RECORD_VERSION_CONFLICT", "RELATIONSHIP_ALREADY_EXISTS", "FIELD_RELATION_CARDINALITY_CONFLICT", "RECORD_RELATION_ALREADY_BOUND", "SSO_PROVIDER_ALREADY_EXISTS",
		// App container: a taken (tenant, name) slug slot is a state conflict, like the
		// other *_ALREADY_EXISTS duplicates.
		"APP_ALREADY_EXISTS",
		// CALENDAR-1: a taken (tenant, name) business-calendar slot.
		"CALENDAR_ALREADY_EXISTS",
		// DAILY-QUEUE #326: deleting a calendar a live rule still pins (details.usages names the rules).
		"CALENDAR_IN_USE",
		// AI-APPLY-IDEMPOTENT (3.1): repeated Apply/POST of a same-name workflow/report.
		"WORKFLOW_ALREADY_EXISTS", "REPORT_ALREADY_EXISTS",
		// W2 relationship lifecycle: deleting a relationship still referenced by rollup
		// fields conflicts with current state — remove the dependents first (the guard
		// runs BEFORE any mutation; details.dependentRollups lists them).
		"RELATIONSHIP_HAS_DEPENDENTS",
		// TENANT-PROVISION: a taken tenant slug is a state conflict (re-POST is not idempotent).
		"TENANT_ALREADY_EXISTS",
		// specs/043 S3a2: a linear-only save would erase the process's stored graph (?force=true).
		"APPROVAL_PROCESS_V2_OVERWRITE",
		// specs/043 S10: publish with no draft, or a draft whose base version has since moved.
		"APPROVAL_VERSION_NO_DRAFT", "APPROVAL_DRAFT_STALE",
		// specs/043 S3b: the optimistic guard of a decision/delegation/recall lost to another write
		// (a second tab, a delegation, an SLA escalation) — reload and look again. Was 422.
		"APPROVAL_CONFLICT",
		// specs/043 S5: a pool item somebody else took first, or one that already ended (decided,
		// withdrawn) — state the caller reloads, not bad input.
		"WORK_ITEM_ALREADY_CLAIMED", "WORK_ITEM_NOT_OPEN",
		// specs/043 S4: one person never decides two seats of a step; an admin without a seat
		// on a many-seat step must use the explicit override. Both are the state of the step.
		"APPROVAL_ALREADY_DECIDED_BY_ACTOR", "APPROVAL_ADMIN_OVERRIDE_REQUIRED",
		// ... and a council seat cannot be handed to someone who already has a say in the step.
		"APPROVAL_DELEGATE_TARGET_SEATED",
		// specs/043 S7: "Thử lại" on a hồ sơ that is waiting for a person, not stalled on an automatic step.
		"APPROVAL_NOT_STUCK",
		// specs/047: the first request with this Idempotency-Key is still running.
		"IDEMPOTENCY_IN_PROGRESS",
		// specs/050 named-credential vault: a taken live name; a delete while rules still use the key.
		"CREDENTIAL_NAME_TAKEN", "CREDENTIAL_IN_USE",
		// specs/050 S4: a rotation that must open a key sealed under a master key this server lacks.
		"CREDENTIAL_KEY_UNAVAILABLE",
		// specs/050 S2a: a workflow save that would silently drop a signing key / auth header
		// set on the rule by API conflicts with the stored rule (the old builder's {type,url}).
		"WEBHOOK_LEGACY_SECRET_PENDING",
		// DAILY-QUEUE #32: "Gửi lại" on a job that is not failed (or was just requeued).
		"JOB_NOT_RETRYABLE",
		// specs/056: purge of a record that is still live — bin it first (two steps, never one) —
		// or of one that LIVE records still require (details.blockers: bin those first).
		"RECORD_NOT_IN_RECYCLE_BIN", "RECORD_PURGE_LIVE_CHILDREN",
		// specs/063-draft-publish: state conflicts of the publish lifecycle (first publish twice, a
		// stale change list, a live document moved under a draft, nothing pending, a busy lock) and
		// installing a pack into an already-delivered workspace.
		"ALREADY_DELIVERED", "NOT_DELIVERED", "DRAFTS_CHANGED", "PUBLISH_CONFLICT", "NOTHING_TO_PUBLISH", "PUBLISH_BUSY",
		"PROJECT_DELIVERED_INSTALL_BLOCKED",
		// specs/063 S7-7: a rollback with drafts pending, after the head moved, or onto a name now taken.
		"DRAFTS_PENDING", "HEAD_MOVED", "PLAN_CHANGED", "ROLLBACK_NAME_TAKEN",
		// specs/063 S7-10: restoring a deleted field / table whose unique value two live records now share.
		"ROLLBACK_UNIQUE_CONFLICT",
		// specs/063 S7-7b: the live document moved since the entry being undone.
		"REVERT_STALE",
		// specs/063 S7-9: retry-schema on a version whose deletes are not in a failed state; editing a
		// field the builder has marked "sẽ xoá khi xuất bản".
		"SCHEMA_RETRY_NOT_NEEDED", "FIELD_PENDING_DELETE",
		// specs/069 L2a: a stale plan hash, or another upgrade of the same pack mid-run — state to reload / wait out.
		"PACK_UPGRADE_PLAN_STALE", "PACK_UPGRADE_IN_PROGRESS",
		// specs/069 L7: a cross-workspace upgrade into a delivered project - open its build session instead.
		"PACK_UPGRADE_OPEN_WORKSPACE",
		// specs/064 S9-4: another sample-data batch of the tenant is running.
		"SAMPLE_BUSY",
		// specs/064 S9-7: the sample count the wipe was confirmed against has moved.
		"SAMPLE_WIPE_STALE",
		// specs/068 per-customer backup: another backup/restore is running for the customer (also the
		// purge refusal), the backup is not ready / its file is gone from disk, or the backup disk is
		// below the estimate + reserve. All are STATE the operator waits out or fixes, not bad input.
		"BACKUP_IN_PROGRESS", "BACKUP_NOT_READY", "BACKUP_FILE_MISSING", "BACKUP_DISK_LOW", "BACKUP_DELETE_IN_USE",
		// specs/068 restore check: a retired customer, or a backup taken at another schema version.
		"TENANT_RESTORE_RETIRED", "TENANT_RESTORE_SCHEMA_MISMATCH", "TENANT_RESTORE_CONFLICT",
		// specs/066 L2 "Gửi lại" email: a row that is not (or no longer) resendable, a rule that lost
		// the email action or is switched off, a record that is gone — all STATE the admin reads and fixes, not bad input.
		"EMAIL_NOT_RESENDABLE", "EMAIL_RESEND_RULE_GONE", "EMAIL_RESEND_RULE_DISABLED", "EMAIL_RESEND_RECORD_GONE":
		return http.StatusConflict
	case "ENTITY_NOT_FOUND", "FIELD_NOT_FOUND", "USER_NOT_FOUND", "ROLE_NOT_FOUND", "TEAM_NOT_FOUND", "TEAM_PARENT_NOT_FOUND", "MEMBER_USER_NOT_FOUND", "MEMBERSHIP_NOT_FOUND",
		"LAYOUT_NOT_FOUND", "WORKFLOW_NOT_FOUND", "RELATIONSHIP_NOT_FOUND",
		// specs/063 S7-6: an unknown, malformed or other-tenant publish version id - one answer.
		"VERSION_NOT_FOUND",
		// W6: approval instance/process lookups are resource reads — an unknown or
		// cross-tenant id is a missing resource (404), matching api-conventions. Also
		// RECORD_NOT_FOUND raised as a ValidationError (workflow dry-run reading the sample
		// record) — the dynamic record handler maps its sentinel separately.
		"APPROVAL_INSTANCE_NOT_FOUND", "APPROVAL_PROCESS_NOT_FOUND", "RECORD_NOT_FOUND",
		// specs/043 S10: no draft to discard / an unknown version number.
		"APPROVAL_DRAFT_NOT_FOUND", "APPROVAL_VERSION_NOT_FOUND",
		// specs/043 S5: an unknown, cross-tenant or not-yours work item — one code, no existence leak.
		"WORK_ITEM_NOT_FOUND",
		// W7: team-set read API — an unknown or cross-tenant team_set id is a missing resource.
		"TEAM_SET_NOT_FOUND",
		// W9b saved views: an unknown / cross-tenant / another-user's-personal view is not-found
		// (no cross-user existence leak — see ViewService.mutableView).
		"VIEW_NOT_FOUND",
		// App container: an unknown or cross-tenant app id/slug is a missing resource.
		"APP_NOT_FOUND",
		// APIKEY-1 machine identity: an unknown or cross-tenant key id is a missing resource.
		"APIKEY_NOT_FOUND",
		// Issue #3: revoking an MCP token that is not one of the caller's.
		"MCP_TOKEN_NOT_FOUND",
		// CALENDAR-1: an unknown or cross-tenant business-calendar id is a missing resource.
		"CALENDAR_NOT_FOUND",
		// TENANT-PROVISION (milestone C): an unknown/suspended cross-tenant install target.
		"TENANT_NOT_FOUND",
		// specs/062 L6: POST /admin/projects names a built-in starter pack that does not exist.
		"VERTICAL_BUILTIN_NOT_FOUND",
		// specs/050: an unknown, deleted or cross-tenant credential id (existence never leaks).
		"CREDENTIAL_NOT_FOUND",
		// DAILY-QUEUE #32: a job id that is not the caller tenant's (or a system job).
		"JOB_NOT_FOUND",
		// specs/055: a login session that is not a live session of that user in that tenant
		// (unknown, ended, or someone else's — one code, no existence leak).
		"SESSION_NOT_FOUND",
		// specs/068: an unknown, deleted, or ANOTHER customer's backup id under this customer's path
		// is not found — deliberately not 403 (no probing).
		"BACKUP_NOT_FOUND",
		// specs/063: a pending change that is not the caller tenant's (or unknown) — one code, no leak.
		"DRAFT_NOT_FOUND",
		// specs/066 L2: a log row that is not the caller tenant's (or unknown) — one code, byte-identical body.
		"EMAIL_LOG_NOT_FOUND":
		return http.StatusNotFound
	case "EMAIL_LOG_FILTER_INVALID", "BACKUP_CURSOR_INVALID", "WORK_ITEM_CURSOR_INVALID",
		// specs/066 L4: from/to of the usage screen unparseable, reversed, in the future or over 366 days.
		"USAGE_RANGE_INVALID":
		// specs/066: a ?status= the log cannot match — a client bug, refused instead of an empty page.
		return http.StatusBadRequest
	case "CREDENTIALS_NOT_CONFIGURED",
		// specs/066 L2: the server has no mail transport (ALERTS_ENABLED off) — a resend would go nowhere.
		"MAIL_NOT_CONFIGURED":
		// specs/050 FR-011: the server runs without CREDENTIAL_ENC_KEY — the vault is inert, a
		// server-side condition the caller cannot fix, not a malformed request.
		return http.StatusServiceUnavailable
	case "FIELD_FORBIDDEN",
		// W9b saved views: a non-admin trying to create/mutate a SHARED (tenant-wide) view is an
		// authorization refusal (well-formed request), so 403 — consistent with FIELD_FORBIDDEN.
		"VIEW_FORBIDDEN",
		// specs/043 S5: releasing a pool item somebody else claimed (a candidate, not the claimer).
		"WORK_ITEM_RELEASE_FORBIDDEN",
		// REPORT-JOIN: the caller lacks entity-level "view" on the JOINED entity — an
		// authorization refusal at run time (per-hop ACL, fail-closed), not a config error.
		"REPORT_JOIN_FORBIDDEN",
		// Generic module-ACL refusal carried as a ValidationError (REPORT-RUN-ACL: the report
		// RUN/export path resolves the report entity's "view" ACL itself, since it is mounted
		// outside the /:entity RequireACL middleware; also the N-N Link target-view refusal). It
		// maps to 403 — the SAME status middleware.RequireACL emits directly for the code — so a
		// ValidationError-carried FORBIDDEN can never silently degrade to the 422 default.
		"FORBIDDEN",
		// specs/051: a write on a record the caller sees only through a manual (read-only) share.
		"RECORD_SHARED_READ_ONLY":
		// RM-42 field-level security: the role may not view/edit the named field. This is an
		// authorization refusal (the request is well-formed), so it maps to 403 — distinct from
		// the 422 validation default and consistent with the module-ACL FORBIDDEN.
		return http.StatusForbidden
	default:
		return http.StatusUnprocessableEntity
	}
}
