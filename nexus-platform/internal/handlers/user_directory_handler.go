package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// UserDirectoryHandler exposes GET /users/directory — an org-wide "who can I assign this to"
// lookup available to ANY authenticated user of the tenant (no admin gate), unlike GET
// /admin/users. See internal/services/user_directory.go doc for why this is a separate surface
// rather than a new method on the admin user service/repo.
type UserDirectoryHandler struct {
	svc    *services.UserDirectoryService
	teams  ShareTeamLister
	logger *zap.Logger
}

// WithTeams adds GET /teams/directory (DAILY-QUEUE #224): the picker of the "Chia sẻ" dialog needs the
// tenant's team names for a NON-admin who may share a record they edit, and /admin/teams is admin-only.
func (h *UserDirectoryHandler) WithTeams(t ShareTeamLister) *UserDirectoryHandler {
	h.teams = t
	return h
}

// NewUserDirectoryHandler wires the handler over the directory service.
func NewUserDirectoryHandler(svc *services.UserDirectoryService, logger *zap.Logger) *UserDirectoryHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &UserDirectoryHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts GET /users/directory on the protected (non-admin) group. This static
// "users" segment MUST be registered BEFORE the dynamic /:entity dispatcher so it resolves ahead
// of the catch-all param route — same precedent as /notifications, /search, /apps.
func (h *UserDirectoryHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/users/directory", h.List)
	if h.teams != nil {
		rg.GET("/teams/directory", h.ListTeams)
	}
}

// ListTeams handles GET /teams/directory: {id, name} of every team of the caller's OWN tenant, by name.
// Nothing else about a team (parent, members) leaves this surface.
func (h *UserDirectoryHandler) ListTeams(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	teams, err := h.teams.ListTeams(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.UserDirectory.ListTeams", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê danh sách nhóm", "code": "TEAM_DIRECTORY_FAILED"})
		return
	}
	items := make([]gin.H, 0, len(teams))
	for _, t := range teams {
		items = append(items, gin.H{"id": t.ID, "name": t.Name})
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

// List handles GET /users/directory: {id, fullName, email} for every ACTIVE user of the caller's
// OWN tenant, optionally filtered by ?q= (substring match on name or email). Query params: page
// (default 1), limit (default 20, max 100). Deliberately returns NOTHING beyond those three
// fields — no status, isAdmin, roleId, or MFA state (this is a directory, not an admin view).
func (h *UserDirectoryHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(strings.TrimSpace(c.Query("page")))
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	q := strings.TrimSpace(c.Query("q"))

	result, err := h.svc.ListDirectory(c.Request.Context(), tenantID, q, page, limit)
	if err != nil {
		h.logger.Error("handlers.UserDirectory.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê danh bạ người dùng", "code": "USER_DIRECTORY_FAILED"})
		return
	}

	items := result.Items
	if items == nil {
		items = []auth.DirectoryUser{}
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
