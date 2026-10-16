package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/notification"
	"nexus/platform/internal/services"
)

// NotificationHandler exposes the P0 notification-read endpoints. All routes sit on the
// protected (non-admin) group scoped to the calling user — no cross-user or cross-tenant reads.
type NotificationHandler struct {
	svc    *services.NotificationService
	logger *zap.Logger
}

// NewNotificationHandler wires the handler over the notification service.
func NewNotificationHandler(svc *services.NotificationService, logger *zap.Logger) *NotificationHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &NotificationHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the notification routes under the given protected group.
// These static "notifications" segments must be registered BEFORE the dynamic /:entity
// dispatcher so they resolve ahead of the catch-all param route.
func (h *NotificationHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/notifications", h.List)
	rg.GET("/notifications/unread-count", h.UnreadCount)
	rg.POST("/notifications/:id/read", h.MarkOneRead)
	rg.POST("/notifications/read-all", h.MarkAllRead)
}

// List handles GET /notifications: paginated list of the caller's notifications, newest first.
// Query params: page (default 1), limit (default 20, max 100), unread=true for unread-only.
func (h *NotificationHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	page, _ := strconv.Atoi(strings.TrimSpace(c.Query("page")))
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	unreadOnly := strings.ToLower(strings.TrimSpace(c.Query("unread"))) == "true"

	result, err := h.svc.ListNotifications(c.Request.Context(), tenantID, userID, unreadOnly, page, limit)
	if err != nil {
		h.logger.Error("handlers.Notification.List",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê thông báo", "code": "NOTIFICATION_LIST_FAILED"})
		return
	}

	// Normalize nil slice to empty JSON array, and compute each row's deep-link so the FE can land
	// on the record instead of the generic inbox.
	items := result.Items
	if items == nil {
		items = []notification.Notification{}
	}
	for i := range items {
		items[i].Href = items[i].DeepLink()
	}

	c.JSON(http.StatusOK, gin.H{
		"data": items,
		"pagination": gin.H{
			"page":       result.Page,
			"limit":      result.Limit,
			"total":      result.Total,
			"totalPages": result.TotalPages,
		},
	})
}

// UnreadCount handles GET /notifications/unread-count: returns {data:{count:N}}.
func (h *NotificationHandler) UnreadCount(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	n, err := h.svc.UnreadCount(c.Request.Context(), tenantID, userID)
	if err != nil {
		h.logger.Error("handlers.Notification.UnreadCount",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đếm thông báo chưa đọc", "code": "NOTIFICATION_COUNT_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"count": n}})
}

// MarkOneRead handles POST /notifications/:id/read: marks a single notification as read.
// Idempotent — already-read returns 200. Returns 404 if the notification is not found or
// does not belong to the caller.
func (h *NotificationHandler) MarkOneRead(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)
	id := strings.TrimSpace(c.Param("id"))

	if err := h.svc.MarkOneRead(c.Request.Context(), tenantID, userID, id); err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.Notification.MarkOneRead",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.String("id", id),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đánh dấu thông báo đã đọc", "code": "NOTIFICATION_MARK_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// MarkAllRead handles POST /notifications/read-all: marks all caller's unread notifications as read.
// Returns {data:{updated:N}} where N is the number of rows marked (0 is valid).
func (h *NotificationHandler) MarkAllRead(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	n, err := h.svc.MarkAllRead(c.Request.Context(), tenantID, userID)
	if err != nil {
		h.logger.Error("handlers.Notification.MarkAllRead",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đánh dấu tất cả thông báo đã đọc", "code": "NOTIFICATION_MARKALL_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"updated": n}})
}
