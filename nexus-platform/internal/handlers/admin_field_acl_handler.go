package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminFieldACLHandler exposes the W7 field-level-security config surface under
// /admin/field-acl: the grants matrix read, the bulk matrix save, and the filtered clear. It
// sits behind Auth → Tenant → RequireAdmin (mounted on the admin-gated group in router.go),
// so the tenant is always the verified claim and every caller is an admin. The rows it
// manages are enforced by FieldACLService on every non-admin record request (A6/RM-42) — the
// enforcement reads the DB per request, so a save here is live immediately.
type AdminFieldACLHandler struct {
	svc    *services.FieldACLAdminService
	logger *zap.Logger
}

// NewAdminFieldACLHandler wires the handler over the field-ACL admin service.
func NewAdminFieldACLHandler(svc *services.FieldACLAdminService, logger *zap.Logger) *AdminFieldACLHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminFieldACLHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the field-ACL config routes under the given (admin-gated) group.
func (h *AdminFieldACLHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/field-acl", h.List)
	rg.PUT("/admin/field-acl", h.Put)
	rg.DELETE("/admin/field-acl", h.Delete)
}

// List handles GET /admin/field-acl?entity=&roleId= — the matrix data: every RESTRICTION row
// of the tenant (optionally narrowed). Envelope documents the default-allow semantics so the
// FE renders "no grant = full access" without guessing:
//
//	{"data":{"grants":[{roleId,entityName,fieldName,canView,canEdit}], "defaultPolicy":"allow",
//	         "deadEndIfLocked":["fieldName",…]}}
//
// deadEndIfLocked (only when ?entity= is given; queue #306): the entity's fields a lock would
// strand a create on — the server's own requiredEngineCannotFill, so the matrix needs no mirror.
func (h *AdminFieldACLHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	grants, err := h.svc.ListGrants(c.Request.Context(), tenantID, c.Query("roleId"), c.Query("entity"))
	if err != nil {
		h.respondServiceError(c, err, "List", tenantID)
		return
	}
	if grants == nil {
		grants = []services.FieldACLGrantView{}
	}
	data := gin.H{
		// A field with NO grant row is fully visible + editable — rows are the exceptions.
		"defaultPolicy": "allow",
		"grants":        grants,
	}
	if entity := strings.TrimSpace(c.Query("entity")); entity != "" {
		dead, derr := h.svc.DeadEndFields(c.Request.Context(), tenantID, entity)
		if derr != nil {
			h.respondServiceError(c, derr, "List", tenantID)
			return
		}
		data["deadEndIfLocked"] = dead
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// putFieldACLRequest is the PUT /admin/field-acl body (bulk matrix save).
type putFieldACLRequest struct {
	Grants []services.FieldACLGrantInput `json:"grants"`
}

// Put handles PUT /admin/field-acl: validates and applies the whole grant set in one
// transaction (all-or-nothing). A (canView=true, canEdit=true) grant clears its restriction
// row (full access = default). Returns {"data":{"applied":N,"cleared":M}}.
func (h *AdminFieldACLHandler) Put(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req putFieldACLRequest
	if !bindJSON(c, &req) {
		return
	}
	// auditContext attributes the __field_acl__ audit row to the verified caller.
	res, err := h.svc.PutGrants(auditContext(c), tenantID, req.Grants)
	if err != nil {
		h.respondServiceError(c, err, "Put", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": res})
}

// Delete handles DELETE /admin/field-acl?roleId=&entity=&field= — removes restriction rows by
// filter (whole role / role+entity / single field). Deleting nothing is idempotent success.
// Returns {"data":{"deleted":N}}.
func (h *AdminFieldACLHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	n, err := h.svc.DeleteGrants(auditContext(c), tenantID,
		strings.TrimSpace(c.Query("roleId")), strings.TrimSpace(c.Query("entity")), strings.TrimSpace(c.Query("field")))
	if err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"deleted": n}})
}

// respondServiceError maps a field-ACL admin service error: ValidationError → its code's
// status (all FIELD_ACL_* codes default to 422 via statusForCode, with details.index /
// details.field marking the offending cell); anything else → logged 500.
func (h *AdminFieldACLHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		body := gin.H{"error": ve.Msg, "code": ve.Code}
		if len(ve.Details) > 0 {
			body["details"] = ve.Details
		}
		c.JSON(statusForCode(ve.Code), body)
		return
	}
	h.logger.Error("handlers.AdminFieldACL."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu field-acl", "code": "FIELD_ACL_OP_FAILED"})
}
