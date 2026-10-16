package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// RelationSvc is the subset of services.RelationService the handler needs.
type RelationSvc interface {
	Link(ctx context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity, sourceID, relName, targetID string, checker services.TargetChecker, aclChecker services.TargetACLChecker, fetcher services.RecordFetcher, sourceScope dynrepo.RowScope) (bool, error)
	Unlink(ctx context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity, sourceID, relName, targetID string, fetcher services.RecordFetcher, sourceScope dynrepo.RowScope) error
	ListLinked(ctx context.Context, tenantID, sourceEntity, sourceID, relName string, q services.RelListQuery, fetcher services.RecordFetcher) (*services.ListLinkedResult, error)
}

// RelationHandler serves the N-N relationship endpoints:
//
//	POST   /:entity/:id/relationships/:relName         — link
//	DELETE /:entity/:id/relationships/:relName/:targetId — unlink
//	GET    /:entity/:id/relationships/:relName         — list linked IDs
type RelationHandler struct {
	svc        RelationSvc
	dynRepo    *dynrepo.Repository       // used as TargetChecker for record-exists validation
	aclChecker services.TargetACLChecker // optional; nil disables ACL-on-target (tests)
	logger     *zap.Logger
}

// NewRelationHandler wires the relation handler.
// aclChecker is optional: pass nil in tests that don't wire ACL; pass the real
// *aclViewAdapter in production so the handler gates target-entity visibility.
func NewRelationHandler(svc RelationSvc, dynRepo *dynrepo.Repository, aclChecker services.TargetACLChecker, logger *zap.Logger) *RelationHandler {
	return &RelationHandler{svc: svc, dynRepo: dynRepo, aclChecker: aclChecker, logger: logger}
}

// RegisterRelationReadRoutes mounts the N-N read endpoint on a view-gated group (GET → "view").
func (h *RelationHandler) RegisterRelationReadRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity/:id/relationships/:relName", h.ListLinked)
}

// RegisterRelationWriteRoutes mounts link + unlink on a group pinned to the "edit" grant: both
// change the SOURCE record's relationships, so the stashed row scope must be the EDIT scope, not
// the method-mapped create/delete one (specs/060 #351). The group must already have Auth +
// RequireTenant + RequireACLAction("edit") in its middleware chain.
func (h *RelationHandler) RegisterRelationWriteRoutes(rg *gin.RouterGroup) {
	rg.POST("/:entity/:id/relationships/:relName", h.Link)
	rg.DELETE("/:entity/:id/relationships/:relName/:targetId", h.Unlink)
}

// link request body.
type linkBody struct {
	TargetID string `json:"targetId"`
}

// Link handles POST /:entity/:id/relationships/:relName.
// Body: {"targetId": "<uuid>"}
// Returns 201 on fresh link, 200 on idempotent (already linked).
func (h *RelationHandler) Link(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	scope := scopeOf(c)
	// roleID is needed for the ACL-on-target check (fix 5). SubjectFromContext reads from
	// the gin context (no side effects); scopeOf already calls it internally but does not
	// surface RoleID, so we extract it here directly.
	sub, _ := middleware.SubjectFromContext(c)

	var body linkBody
	if err := c.ShouldBindJSON(&body); err != nil || body.TargetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "targetId bắt buộc trong body JSON",
			"code":  "INVALID_BODY",
		})
		return
	}

	sourceEntity := c.Param("entity")
	sourceID := c.Param("id")
	relName := c.Param("relName")

	created, err := h.svc.Link(c.Request.Context(),
		tenantID, scope.UserID, sub.RoleID, scope.IsAdmin,
		sourceEntity, sourceID,
		relName, body.TargetID,
		h.dynRepo, h.aclChecker,
		h.dynRepo, scope.ToRowScope())
	if err != nil {
		h.respondError(c, err, "Link")
		return
	}

	if created {
		c.JSON(http.StatusCreated, gin.H{"linked": true})
	} else {
		c.JSON(http.StatusOK, gin.H{"linked": true})
	}
}

// Unlink handles DELETE /:entity/:id/relationships/:relName/:targetId.
// Returns 204 on success, 404 if not found.
func (h *RelationHandler) Unlink(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	scope := scopeOf(c)
	sub, _ := middleware.SubjectFromContext(c)

	if err := h.svc.Unlink(c.Request.Context(), tenantID, scope.UserID, sub.RoleID, scope.IsAdmin,
		c.Param("entity"), c.Param("id"),
		c.Param("relName"), c.Param("targetId"),
		h.dynRepo, scope.ToRowScope()); err != nil {
		h.respondError(c, err, "Unlink")
		return
	}
	c.Status(http.StatusNoContent)
}

// ListLinked handles GET /:entity/:id/relationships/:relName.
//
// Without ?expand=true: returns {data:[ids], pagination:{...}}.
// With    ?expand=true:  returns {data:[{full record objects}], pagination:{...}}.
func (h *RelationHandler) ListLinked(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	q := services.RelListQuery{
		Page:   atoiDefault(c.Query("page"), 1),
		Limit:  atoiDefault(c.Query("limit"), 20),
		Expand: c.Query("expand") == "true",
		Scope:  scopeOf(c), // thread caller's A5 row-level scope into expand GetRecord calls
	}

	result, err := h.svc.ListLinked(c.Request.Context(), tenantID,
		c.Param("entity"), c.Param("id"),
		c.Param("relName"), q, h.dynRepo)
	if err != nil {
		h.respondError(c, err, "ListLinked")
		return
	}

	page, limit := dynrepo.ClampPageLimit(q.Page, q.Limit)
	totalPages := 0
	if limit > 0 {
		totalPages = (result.Total + limit - 1) / limit
	}

	// When expand=true return full record objects; otherwise return bare ID strings.
	var data any
	if q.Expand && result.Records != nil {
		data = result.Records
	} else {
		data = result.IDs
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"pagination": gin.H{
			"page":       page,
			"limit":      limit,
			"total":      result.Total,
			"totalPages": totalPages,
		},
	})
}

// respondError maps relation service errors to HTTP responses.
func (h *RelationHandler) respondError(c *gin.Context, err error, op string) {
	// Known not-found sentinels.
	if errors.Is(err, services.ErrRelationshipNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Quan hệ không tồn tại hoặc không phải N-N",
			"code":  "RELATIONSHIP_NOT_FOUND",
		})
		return
	}
	if errors.Is(err, services.ErrRecordNotFound) || errors.Is(err, dynrepo.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Liên kết không tồn tại",
			"code":  "RECORD_NOT_FOUND",
		})
		return
	}

	// ValidationError (422 or 409 based on code).
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}

	h.logger.Error("handlers.Relation."+op, zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": "Lỗi xử lý quan hệ",
		"code":  "RELATION_OP_FAILED",
	})
}
