package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// W5 Builder core v2 handlers (docs/nexus-ux-revolution-plan.md §3.2): field reorder +
// formula dry-run validate. Both live on AdminEntityHandler (they are field-metadata
// operations powered by the same FieldService) and are mounted admin-gated via
// RegisterRoutes → adminGated group (Auth → Tenant → RequireAdmin).

// reorderFieldsRequest is the PUT /admin/fields/reorder body: the target entity (by name,
// tenant-scoped) and its field IDs in the new display order (renumbered (i+1)*10 in one
// transaction).
type reorderFieldsRequest struct {
	EntityName string   `json:"entityName"`
	Order      []string `json:"order"`
}

// ReorderFields handles PUT /admin/fields/reorder: renumbers field_order for the listed
// fields of ONE tenant-scoped entity in a single transaction. Unknown entity → 404; an id
// that is not an active field of that entity (incl. cross-tenant ids) → 422
// FIELD_REORDER_UNKNOWN_FIELD; malformed list (empty/duplicate/blank id) → 422
// FIELD_REORDER_INVALID. Success → 200 {data:{entityName, updated}}.
func (h *AdminEntityHandler) ReorderFields(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var req reorderFieldsRequest
	if !bindJSON(c, &req) {
		return
	}

	updated, err := h.fieldSvc.ReorderFields(auditContext(c), tenantID, services.ReorderFieldsInput{
		EntityName: req.EntityName,
		Order:      req.Order,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.ReorderFields",
			zap.String("tenant_id", tenantID), zap.String("entity", req.EntityName), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể sắp xếp lại field", "code": "FIELD_REORDER_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entityName": req.EntityName, "updated": updated}})
}

// validateFormulaRequest is the POST /admin/formula/validate body. resultType is optional
// ("number" | "currency" | "text"); when omitted the record-time default "number" is echoed back.
type validateFormulaRequest struct {
	EntityName string `json:"entityName"`
	Formula    string `json:"formula"`
	ResultType string `json:"resultType"`
}

// ValidateFormula handles POST /admin/formula/validate (W5 live formula editor): a
// zero-write dry-run of the SAME config-time validation the field-create path performs.
// Success → 200 {data:{valid:true, fields:[refs], resultType}}; failure → 422 with the
// exact FIELD_FORMULA_* code/message a real POST /admin/fields would return (unknown
// entity → 404 ENTITY_NOT_FOUND).
func (h *AdminEntityHandler) ValidateFormula(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var req validateFormulaRequest
	if !bindJSON(c, &req) {
		return
	}

	res, err := h.fieldSvc.ValidateFormula(auditContext(c), tenantID, services.ValidateFormulaInput{
		EntityName: req.EntityName,
		Formula:    req.Formula,
		ResultType: req.ResultType,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.ValidateFormula",
			zap.String("tenant_id", tenantID), zap.String("entity", req.EntityName), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể kiểm tra công thức", "code": "FORMULA_VALIDATE_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": res})
}
