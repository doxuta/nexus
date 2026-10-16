package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AdminLayoutHandler handles layout CRUD for admin endpoints and the metadata GET endpoint.
type AdminLayoutHandler struct {
	svc    *services.LayoutService
	logger *zap.Logger
	// entityACL answers "may this role view this entity". GET /metadata/layouts/:entity was the
	// only one of the four metadata reads with no such check: a user with no grant on an entity
	// could still read its section titles and field names — the shape of a table they cannot open.
	// Optional so existing constructions compile; nil keeps the pre-existing behaviour.
	entityACL entityViewChecker
	// fieldACL strips the served layout down to the fields the caller may VIEW (hiddenFieldsFor,
	// metadata_handler.go). Optional; nil keeps the pre-existing behaviour.
	fieldACL fieldViewChecker
}

// WithEntityACL arms the view check on the layout read. Chainable.
func (h *AdminLayoutHandler) WithEntityACL(c entityViewChecker) *AdminLayoutHandler {
	h.entityACL = c
	return h
}

// WithFieldACL arms the hidden-field strip on the layout read. Chainable.
func (h *AdminLayoutHandler) WithFieldACL(f fieldViewChecker) *AdminLayoutHandler {
	h.fieldACL = f
	return h
}

// NewAdminLayoutHandler wires the handler.
func NewAdminLayoutHandler(svc *services.LayoutService, logger *zap.Logger) *AdminLayoutHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminLayoutHandler{svc: svc, logger: logger}
}

// RegisterMetadataRoutes mounts the GET /metadata/layouts/:entity endpoint on the protected group.
// This is separate from admin routes so it can be mounted on the non-admin protected group.
func (h *AdminLayoutHandler) RegisterMetadataRoutes(rg *gin.RouterGroup) {
	rg.GET("/metadata/layouts/:entity", h.GetLayout)
}

// RegisterAdminRoutes mounts POST/PUT/DELETE layout routes on the admin-gated group.
func (h *AdminLayoutHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/layouts", h.SaveLayout)
	rg.PUT("/admin/layouts/:id", h.UpdateLayout)
	rg.DELETE("/admin/layouts/:id", h.DeleteLayout)
}

// GetLayout handles GET /api/v1/metadata/layouts/:entity
// Optional query param: ?view=edit|detail|list to filter to a single view.
func (h *AdminLayoutHandler) GetLayout(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	entityName := strings.TrimSpace(c.Param("entity"))
	if entityName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu entity name", "code": "ENTITY_NAME_REQUIRED"})
		return
	}

	// Same refusal the field read gives: 404 ENTITY_NOT_FOUND, never a 403, so a denial does not
	// confirm that the entity exists.
	if h.entityACL != nil {
		sub, _ := middleware.SubjectFromContext(c)
		if !sub.IsAdmin {
			ok, aerr := h.entityACL.CanViewEntity(c.Request.Context(), tenantID, sub.RoleID, entityName)
			if aerr != nil || !ok {
				if aerr != nil {
					h.logger.Warn("handlers.AdminLayout.GetLayout: acl check failed (fail closed)",
						zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(aerr))
				}
				c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
				return
			}
		}
	}

	// Same fail-closed refusal as the entity check above: a grant-load failure answers 404 rather
	// than a layout that might place a field the role cannot see. Admin / unwired → nil (all).
	hidden, herr := hiddenFieldsFor(c, h.fieldACL, tenantID, entityName)
	if herr != nil {
		h.logger.Warn("handlers.AdminLayout.GetLayout: field acl check failed (fail closed)",
			zap.String("tenant_id", tenantID), zap.String("entity", entityName), zap.Error(herr))
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	}

	viewFilter := strings.TrimSpace(c.Query("view"))

	result, err := h.svc.GetLayout(c.Request.Context(), tenantID, entityName, viewFilter)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.GetLayout",
			zap.String("tenant_id", tenantID),
			zap.String("entity_name", entityName),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể lấy layout", "code": "LAYOUT_FETCH_FAILED"})
		return
	}

	// Build camelCase response envelope per spec §3
	viewsOut := make(map[string]any, len(result.Views))
	for vt, vr := range result.Views {
		// "Ẩn" means not served: drop every cell/column naming a field the caller may not view
		// BEFORE shaping the response (serve-time only — nothing is persisted).
		vr.LayoutData = services.StripHiddenLayoutFields(vr.LayoutData, vt, hidden)
		viewsOut[vt] = buildViewResponse(vr)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"entityName":   result.EntityName,
			"views":        viewsOut,
			"systemFields": result.SystemFields,
		},
	})
}

// buildViewResponse shapes the per-view response including isDefault + layout fields.
func buildViewResponse(vr *services.LayoutViewResult) map[string]any {
	out := make(map[string]any, len(vr.LayoutData)+2)
	out["isDefault"] = vr.IsDefault
	if vr.ID != "" { // custom (non-default) view → expose the by-id handle for PUT/DELETE (S1)
		out["id"] = vr.ID
	}
	if vr.DraftState != "" { // specs/063 S7-3: only the builder's overlay read ever sets this
		out["draftState"] = vr.DraftState
	}
	for k, v := range vr.LayoutData {
		out[k] = v
	}
	return out
}

// saveLayoutRequest is the POST /admin/layouts body.
type saveLayoutRequest struct {
	EntityName string         `json:"entityName"`
	ViewType   string         `json:"viewType"`
	LayoutJSON map[string]any `json:"layoutJson"`
}

// SaveLayout handles POST /api/v1/admin/layouts (upsert).
func (h *AdminLayoutHandler) SaveLayout(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var req saveLayoutRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.SaveLayoutInput{
		EntityName: req.EntityName,
		ViewType:   req.ViewType,
		LayoutJSON: req.LayoutJSON,
	}

	layout, err := h.svc.SaveLayout(auditContext(c), tenantID, in)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.SaveLayout",
			zap.String("tenant_id", tenantID),
			zap.String("entity_name", req.EntityName),
			zap.String("view_type", req.ViewType),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể lưu layout", "code": "LAYOUT_SAVE_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":         layout.ID,
			"entityName": layout.EntityName,
			"viewType":   layout.ViewType,
			"tenantId":   layout.TenantID,
		},
	})
}

// updateLayoutRequest is the PUT /admin/layouts/:id body.
type updateLayoutRequest struct {
	LayoutJSON map[string]any `json:"layoutJson"`
}

// UpdateLayout handles PUT /api/v1/admin/layouts/:id.
func (h *AdminLayoutHandler) UpdateLayout(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu layout id", "code": "LAYOUT_ID_REQUIRED"})
		return
	}

	var req updateLayoutRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.UpdateLayoutInput{
		ID:         id,
		LayoutJSON: req.LayoutJSON,
	}

	layout, err := h.svc.UpdateLayout(auditContext(c), tenantID, in)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.UpdateLayout",
			zap.String("tenant_id", tenantID),
			zap.String("layout_id", id),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể cập nhật layout", "code": "LAYOUT_UPDATE_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":         layout.ID,
			"entityName": layout.EntityName,
			"viewType":   layout.ViewType,
			"tenantId":   layout.TenantID,
		},
	})
}

// DeleteLayout handles DELETE /api/v1/admin/layouts/:id (hard-delete).
func (h *AdminLayoutHandler) DeleteLayout(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu layout id", "code": "LAYOUT_ID_REQUIRED"})
		return
	}

	err := h.svc.DeleteLayout(auditContext(c), tenantID, id)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.DeleteLayout",
			zap.String("tenant_id", tenantID),
			zap.String("layout_id", id),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá layout", "code": "LAYOUT_DELETE_FAILED"})
		return
	}

	c.Status(http.StatusNoContent)
}
