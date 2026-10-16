package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// RelationRegistrar is the subset of services.RelationService the admin registration handler needs.
type RelationRegistrar interface {
	RegisterRelationship(ctx context.Context, tenantID string, input services.RegisterRelationshipInput) (*metadata.RelationshipDefinition, error)
}

// RelationLifecycle is the subset of services.RelationLifecycleService the W2 lifecycle
// endpoints need (impact / patch / delete by sys_relationships id). Declared consumer-side
// so the handler stays unit-testable without the concrete service.
type RelationLifecycle interface {
	Impact(ctx context.Context, tenantID, relID string) (*services.RelationImpact, error)
	Patch(ctx context.Context, tenantID, relID string, in services.PatchRelationshipInput) (*metadata.RelationshipDefinition, error)
	Delete(ctx context.Context, tenantID, relID string) error
	// SetViewConfig (ENG-RELVIEW) validates + persists the related-list view config; a JSON
	// `null` body clears it (returns nil, nil). See services.RelationLifecycleService.SetViewConfig.
	SetViewConfig(ctx context.Context, tenantID, relID string, raw json.RawMessage) (map[string]any, error)
}

// AdminRelationHandler serves the admin-only relationship endpoints:
//
//	POST   /admin/relationships                 — register a new N-N relationship between two entities
//	GET    /admin/relationships/:id/impact      — delete-impact pre-check (counts + dependents)
//	PATCH  /admin/relationships/:id             — edit surface (structurally immutable; see service doc)
//	DELETE /admin/relationships/:id             — remove a relationship ("gỡ nối", guarded soft delete)
//	PUT    /admin/relationships/:id/view-config — set/clear the related-list view config (ENG-RELVIEW)
type AdminRelationHandler struct {
	svc       RelationRegistrar
	lifecycle RelationLifecycle
	logger    *zap.Logger
}

// NewAdminRelationHandler wires the admin relation handler. lifecycle may be nil in legacy
// tests — the lifecycle routes are then not mounted (production always wires it).
func NewAdminRelationHandler(svc RelationRegistrar, lifecycle RelationLifecycle, logger *zap.Logger) *AdminRelationHandler {
	return &AdminRelationHandler{svc: svc, lifecycle: lifecycle, logger: logger}
}

// RegisterRoutes mounts the admin relationship endpoints.
// Must be called on a group that already has RequireAdmin in its middleware chain.
func (h *AdminRelationHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/relationships", h.Create)
	if h.lifecycle != nil {
		rg.GET("/admin/relationships/:id/impact", h.Impact)
		rg.PATCH("/admin/relationships/:id", h.Patch)
		rg.DELETE("/admin/relationships/:id", h.Delete)
		rg.PUT("/admin/relationships/:id/view-config", h.SetViewConfig)
	}
}

type createRelationBody struct {
	Name    string `json:"name"`
	EntityA string `json:"entityA"`
	EntityB string `json:"entityB"`
}

// Create handles POST /admin/relationships.
// Body: {"name":"...", "entityA":"...", "entityB":"..."}
// Returns 201 Created with the relationship definition.
func (h *AdminRelationHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var body createRelationBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body JSON không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	if body.Name == "" || body.EntityA == "" || body.EntityB == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "name, entityA, entityB là bắt buộc",
			"code":  "INVALID_BODY",
		})
		return
	}

	rel, err := h.svc.RegisterRelationship(c.Request.Context(), tenantID, services.RegisterRelationshipInput{
		Name:    body.Name,
		EntityA: body.EntityA,
		EntityB: body.EntityB,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.AdminRelation.Create", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Lỗi tạo quan hệ",
			"code":  "RELATION_CREATE_FAILED",
		})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": rel})
}

// relIDOf extracts and validates the :id path param shared by the lifecycle endpoints.
func relIDOf(c *gin.Context) (string, bool) {
	relID := strings.TrimSpace(c.Param("id"))
	if relID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu id quan hệ", "code": "RELATIONSHIP_ID_REQUIRED"})
		return "", false
	}
	return relID, true
}

// Impact handles GET /admin/relationships/:id/impact — the pre-delete manifest the Studio
// canvas shows BEFORE the destructive confirm. Read-only; 404 on unknown/cross-tenant id.
func (h *AdminRelationHandler) Impact(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	relID, ok := relIDOf(c)
	if !ok {
		return
	}

	impact, err := h.lifecycle.Impact(c.Request.Context(), tenantID, relID)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminRelation.Impact",
			zap.String("tenant_id", tenantID), zap.String("relationship_id", relID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể tính tác động của việc xoá quan hệ",
			"code":  "RELATION_IMPACT_FAILED",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": impact})
}

// patchRelationBody mirrors the RelationshipDefinition wire shape (camelCase, same keys as
// GET /metadata/relationships) with pointer semantics: absent = untouched. Every key is
// structurally immutable — a supplied value that differs from the stored row is rejected
// 422 FIELD_RELATION_OPTIONS_IMMUTABLE by the service (see its Patch doc comment).
type patchRelationBody struct {
	Name             *string `json:"name"`
	ParentEntityName *string `json:"parentEntityName"`
	ChildEntityName  *string `json:"childEntityName"`
	RelationshipType *string `json:"relationshipType"`
	InverseField     *string `json:"inverseField"`
	ChildFKColumn    *string `json:"childFkColumn"`
	JoinTableName    *string `json:"joinTableName"`
}

// Patch handles PATCH /admin/relationships/:id. Nothing structural is mutable (see
// services.RelationLifecycleService.Patch): equal/absent keys no-op (200 echoes the stored
// row); a changed key → 422 FIELD_RELATION_OPTIONS_IMMUTABLE. A 1-N relation's display
// label is edited on the relation FIELD via the existing PUT /admin/fields/:id.
func (h *AdminRelationHandler) Patch(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	relID, ok := relIDOf(c)
	if !ok {
		return
	}
	var body patchRelationBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body JSON không hợp lệ", "code": "INVALID_BODY"})
		return
	}

	rel, err := h.lifecycle.Patch(c.Request.Context(), tenantID, relID, services.PatchRelationshipInput{
		Name:             body.Name,
		ParentEntityName: body.ParentEntityName,
		ChildEntityName:  body.ChildEntityName,
		RelationshipType: body.RelationshipType,
		InverseField:     body.InverseField,
		ChildFKColumn:    body.ChildFKColumn,
		JoinTableName:    body.JoinTableName,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminRelation.Patch",
			zap.String("tenant_id", tenantID), zap.String("relationship_id", relID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể cập nhật quan hệ",
			"code":  "RELATION_UPDATE_FAILED",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rel})
}

// Delete handles DELETE /admin/relationships/:id ("gỡ nối"). 204 on success; 404 on
// unknown/cross-tenant/already-deleted id; 409 RELATIONSHIP_HAS_DEPENDENTS when rollup
// fields still reference the relationship (details.dependentRollups lists them).
func (h *AdminRelationHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	relID, ok := relIDOf(c)
	if !ok {
		return
	}

	if err := h.lifecycle.Delete(c.Request.Context(), tenantID, relID); err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminRelation.Delete",
			zap.String("tenant_id", tenantID), zap.String("relationship_id", relID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể xoá quan hệ",
			"code":  "RELATION_DELETE_FAILED",
		})
		return
	}
	// 204 No Content: registry row soft-deleted (1-N/1-1 additionally soft-deletes the
	// owning relation field); obj_*/rel_* data preserved per the soft-delete convention.
	c.Status(http.StatusNoContent)
}

// SetViewConfig handles PUT /admin/relationships/:id/view-config (ENG-RELVIEW). The body is
// the view_config document itself — an object {label?, order?, columns?, sort?, allowCreate?,
// allowLink?} to set, or JSON `null` to clear (back to the FE's default rendering). Responds
// 200 {data:{viewConfig}} echoing the stored config (null after a clear). Validation errors
// surface as 422 RELVIEW_INVALID / RELVIEW_COLUMN_UNKNOWN / RELVIEW_SORT_NOT_SORTABLE;
// unknown/cross-tenant/deleted id → 404 RELATIONSHIP_NOT_FOUND.
func (h *AdminRelationHandler) SetViewConfig(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	relID, ok := relIDOf(c)
	if !ok {
		return
	}
	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body JSON không hợp lệ", "code": "INVALID_BODY"})
		return
	}

	cfg, err := h.lifecycle.SetViewConfig(c.Request.Context(), tenantID, relID, raw)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminRelation.SetViewConfig",
			zap.String("tenant_id", tenantID), zap.String("relationship_id", relID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể lưu cấu hình hiển thị quan hệ",
			"code":  "RELATION_VIEWCONFIG_FAILED",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"viewConfig": cfg}})
}
