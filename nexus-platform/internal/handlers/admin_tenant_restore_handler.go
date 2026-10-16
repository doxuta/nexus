package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/tenantbackup"
)

// restoreRequest is the body of POST /admin/tenants/:id/restores. DryRun is a pointer and silence
// means true (the purgeTenantRequest pattern): the safe reading of a missing flag on a destructive
// endpoint is "only look".
type restoreRequest struct {
	BackupID    string `json:"backupId"`
	ConfirmSlug string `json:"confirmSlug"`
	DryRun      *bool  `json:"dryRun"`
}

// Restore handles POST /admin/tenants/:id/restores. dryRun omitted or true answers 200 with the
// plan and writes nothing; dryRun:false with the typed confirmSlug starts the restore: 202.
func (h *AdminTenantBackupHandler) Restore(c *gin.Context) {
	actor, operatorTenant := h.actor(c)
	var req restoreRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.BackupID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	if req.DryRun != nil && !*req.DryRun {
		started, err := h.svc.StartRestore(c.Request.Context(), tenantbackup.RestoreInput{
			TenantID: c.Param("id"), BackupID: req.BackupID, ConfirmSlug: req.ConfirmSlug, Actor: actor,
		})
		if err != nil {
			h.fail(c, "Restore", err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"data": started})
		return
	}
	plan, err := h.svc.Preview(c.Request.Context(), tenantbackup.PreviewInput{
		TenantID: c.Param("id"), BackupID: req.BackupID, CallerTenantID: operatorTenant,
	})
	if err != nil {
		h.fail(c, "Restore", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": plan})
}
