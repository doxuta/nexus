package handlers

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AdminEmailLogHandler serves "Nhật ký email" (specs/066 L1b): the tenant's log of the mail its
// automation rules sent or failed to send. Like "Việc nền" it MUST be mounted on adminGated, never
// builderGated: the persona is the customer's own tenant-admin, who is not a platform operator. The
// tenant always comes from the verified token.
type AdminEmailLogHandler struct {
	svc    *services.EmailLogService
	logger *zap.Logger
}

// NewAdminEmailLogHandler wires the handler over the email-log service.
func NewAdminEmailLogHandler(svc *services.EmailLogService, logger *zap.Logger) *AdminEmailLogHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminEmailLogHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the routes under the given (admin-gated) group.
func (h *AdminEmailLogHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/email-log", h.List)
	rg.POST("/admin/email-log/:id/resend", h.Resend)
}

// Resend handles POST /admin/email-log/:id/resend — "Gửi lại" one failed rule email of the caller's
// tenant. Body {dryRun?: boolean}: absent / empty / true = a PREVIEW (recipient masked, rendered
// subject, ruleChangedSinceSend; nothing is sent or written); {"dryRun":false} sends. The body can name
// nothing else (unknown keys are 400): the recipient and the text come out of the rule's templates.
//
//	200 {data:{dryRun:true, recipientMasked, subject, ruleChangedSinceSend}}
//	200 {data:{dryRun:false, entry:{…}}}
//	404 EMAIL_LOG_NOT_FOUND (identical for a foreign and an unknown id) · 409 EMAIL_NOT_RESENDABLE |
//	EMAIL_RESEND_RULE_GONE | EMAIL_RESEND_RULE_DISABLED | EMAIL_RESEND_RECORD_GONE · 422 EMAIL_RESEND_NO_RECIPIENT ·
//	503 MAIL_NOT_CONFIGURED · 500 EMAIL_RESEND_FAILED (+entry) | EMAIL_RESEND_UNAVAILABLE
func (h *AdminEmailLogHandler) Resend(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req struct {
		DryRun *bool `json:"dryRun"`
	}
	// An empty body (also a chunked one, ContentLength -1) is a preview, not a 400.
	raw, rerr := io.ReadAll(c.Request.Body)
	if rerr != nil {
		if middleware.IsMaxBytesError(rerr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Request body exceeds the maximum allowed size", "code": "REQUEST_TOO_LARGE"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body JSON không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		if !bindJSONStrict(c, &req) {
			return
		}
	}
	actorID := actorOf(c)
	res, err := h.svc.Resend(c.Request.Context(), tenantID, actorID, c.Param("id"), req.DryRun)
	if err != nil {
		var ve *services.ValidationError
		switch {
		case errors.As(err, &ve):
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		case errors.Is(err, services.ErrEmailResendUnavailable):
			h.logger.Error("handlers.AdminEmailLogHandler.Resend: not wired", zap.String("tenant_id", tenantID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Máy chủ chưa nối tính năng gửi lại email", "code": "EMAIL_RESEND_UNAVAILABLE"})
		default:
			h.logger.Error("handlers.AdminEmailLogHandler.Resend", zap.String("tenant_id", tenantID),
				zap.String("user_id", actorID), zap.String("log_id", c.Param("id")), zap.Error(err))
			body := gin.H{"error": "Không gửi lại được email", "code": "EMAIL_RESEND_FAILED"}
			if res != nil && res.Entry != nil {
				body["entry"] = res.Entry
			}
			c.JSON(http.StatusInternalServerError, body)
		}
		return
	}
	if res.DryRun {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"dryRun": true, "recipientMasked": res.RecipientMasked,
			"subject": res.Subject, "ruleChangedSinceSend": res.RuleChangedSinceSend}})
		return
	}
	h.logger.Info("rule email resent by tenant admin", zap.String("tenant_id", tenantID),
		zap.String("user_id", actorID), zap.String("log_id", c.Param("id")))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"dryRun": false, "entry": res.Entry}})
}

// List handles GET /admin/email-log?status=&source=&limit=&after= — newest first, keyset-paged. Returns
// {data:[…], pagination:{limit, nextCursor}, meta:{mailConfigured, queueEnabled}}.
func (h *AdminEmailLogHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	f := emaillog.Filter{
		Status: strings.TrimSpace(c.Query("status")),
		Source: strings.TrimSpace(c.Query("source")),
		Limit:  atoiDefault(c.Query("limit"), 50),
		After:  c.Query("after"),
	}
	rows, next, err := h.svc.List(c.Request.Context(), tenantID, f)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		if errors.Is(err, dynrepo.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cursor không hợp lệ", "code": "INVALID_CURSOR"})
			return
		}
		h.logger.Error("handlers.AdminEmailLogHandler.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không đọc được nhật ký email", "code": "EMAIL_LOG_QUERY_FAILED"})
		return
	}
	if rows == nil {
		rows = []emaillog.Row{} // never null: the FE maps over it
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       rows,
		"pagination": gin.H{"limit": emaillog.ClampLimit(f.Limit), "nextCursor": nextCursor},
		"meta":       gin.H{"mailConfigured": h.svc.MailConfigured(), "queueEnabled": h.svc.QueueEnabled()},
	})
}
