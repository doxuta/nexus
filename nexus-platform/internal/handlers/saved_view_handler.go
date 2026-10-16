package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// SavedViewHandler exposes the W9b saved-view CRUD surface (../specs/018-saved-views/spec.md). Views are a
// per-user UI preset (filters/sort/columns/density) kept against a dynamic entity, so they are
// ENTITY-SCOPED in the URL (/:entity/views[/:id]). The surface is mounted behind
// RequireACLAction("view") — a caller needs the SAME entity-level view grant as the list surface a
// view is applied to; on top of that the shared-vs-personal ownership gate is enforced INSIDE the
// service (ViewService), not by the coarse RequireAdmin middleware. The static "views" segment
// resolves ahead of the /:entity/:id param route (like /aggregate, /export.csv, /deleted).
type SavedViewHandler struct {
	svc    *services.ViewService
	logger *zap.Logger
}

// NewSavedViewHandler wires the handler over the view service.
func NewSavedViewHandler(svc *services.ViewService, logger *zap.Logger) *SavedViewHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SavedViewHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the entity-scoped saved-view routes under the given group. "views" is a
// static child of /:entity (like /aggregate, /export.csv, /deleted), so gin resolves it ahead of
// the /:entity/:id wildcard; a record whose id is literally "views" is unreachable by id
// (acceptable — ids are server-generated UUIDs).
func (h *SavedViewHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity/views", h.List)
	rg.POST("/:entity/views", h.Create)
	rg.PUT("/:entity/views/:id", h.Update)
	rg.DELETE("/:entity/views/:id", h.Delete)
}

// createViewRequest is the POST /:entity/views body. The target entity comes from the URL, not the
// body. Config is forwarded verbatim (FE-defined shape).
type createViewRequest struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	Shared bool            `json:"shared"`
}

// updateViewRequest is the PUT /:entity/views/:id body (name + config replace).
type updateViewRequest struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
}

// List handles GET /:entity/views: the views visible to the caller for the URL entity —
// shared ∪ the caller's own personal — newest-first.
func (h *SavedViewHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	rows, err := h.svc.List(c.Request.Context(), tenantID, sub.UserID, c.Param("entity"))
	if err != nil {
		h.respondError(c, err, "List", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// Create handles POST /:entity/views: creates a personal view (or a shared view when shared=true
// and the caller is an admin). The target entity is the URL segment. Returns 201 with the new id.
func (h *SavedViewHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	var req createViewRequest
	if !bindJSON(c, &req) {
		return
	}
	id, err := h.svc.Create(c.Request.Context(), tenantID, sub.UserID, sub.IsAdmin, services.CreateViewInput{
		EntityName: c.Param("entity"),
		Name:       req.Name,
		Config:     req.Config,
		Shared:     req.Shared,
	})
	if err != nil {
		h.respondError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": id}})
}

// Update handles PUT /:entity/views/:id: replaces the view's name + config. The URL entity must
// match the view (else 404); ownership is enforced in the service (own personal, or admin for a
// shared view).
func (h *SavedViewHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	var req updateViewRequest
	if !bindJSON(c, &req) {
		return
	}
	err := h.svc.Update(c.Request.Context(), tenantID, sub.UserID, sub.IsAdmin, c.Param("entity"), c.Param("id"), services.UpdateViewInput{
		Name:   req.Name,
		Config: req.Config,
	})
	if err != nil {
		h.respondError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": c.Param("id")}})
}

// Delete handles DELETE /:entity/views/:id: soft-deletes the view. The URL entity must match the
// view (else 404); ownership is enforced in the service.
func (h *SavedViewHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	if err := h.svc.Delete(c.Request.Context(), tenantID, sub.UserID, sub.IsAdmin, c.Param("entity"), c.Param("id")); err != nil {
		h.respondError(c, err, "Delete", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// respondError maps a ViewService error to HTTP: a ValidationError is classified by statusForCode
// (VIEW_NOT_FOUND→404, VIEW_FORBIDDEN→403, ENTITY_NOT_FOUND→404, the VIEW_* validation codes→422),
// carrying details.field when present; anything else is a logged 500.
func (h *SavedViewHandler) respondError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.SavedView."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi xử lý view", "code": "VIEW_OP_FAILED"})
}
