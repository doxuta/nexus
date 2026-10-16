package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
	"nexus/platform/internal/shares"
)

// RecordShareService is the slice of *shares.Service the handler needs (consumer-side, like
// NoteService).
type RecordShareService interface {
	List(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) ([]shares.Share, error)
	Create(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, userID string) (*shares.Share, error)
	CreateTeam(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, teamID string) (*shares.Share, error)
	Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, granteeID string) error
}

// ShareTeamLister names the teams of a tenant for the share list (tenant-pinned, ordered by name).
// Satisfied by *auth.TeamRepository.
type ShareTeamLister interface {
	ListTeams(ctx context.Context, tenantID string) ([]auth.Team, error)
}

// RecordShareHandler serves manual per-record READ shares (DAILY-QUEUE #34, specs/051):
//
//	GET    /:entity/:id/shares          who the record is shared with
//	POST   /:entity/:id/shares          share it with one user {"userId": "..."} OR one team {"teamId": "..."}
//	DELETE /:entity/:id/shares/:userId  revoke that grantee's share (the segment is a user's OR a team's id)
//
// All three are mounted behind RequireACLAction("edit") (router.go): managing shares is an act of
// someone who may EDIT the record (Q4a). The service re-resolves that grant and reads the record
// under the caller's edit scope regardless of the mount (shares.Service → ResolveShareTarget).
type RecordShareHandler struct {
	svc    RecordShareService
	labels NoteUserLabelFetcher
	teams  ShareTeamLister
	logger *zap.Logger
}

// NewRecordShareHandler wires the handler. logger may be nil.
func NewRecordShareHandler(svc RecordShareService, logger *zap.Logger) *RecordShareHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RecordShareHandler{svc: svc, logger: logger}
}

// WithUserLabels adds the grantees' display names/emails to the list (one batched query, the same
// primitive notes use). Optional: nil leaves userName/userEmail empty.
func (h *RecordShareHandler) WithUserLabels(f NoteUserLabelFetcher) *RecordShareHandler {
	h.labels = f
	return h
}

// WithTeams adds the team names to the list (DAILY-QUEUE #224). Optional: nil leaves teamName empty.
func (h *RecordShareHandler) WithTeams(t ShareTeamLister) *RecordShareHandler {
	h.teams = t
	return h
}

// RegisterRoutes mounts the three routes; the caller passes the edit-pinned group.
func (h *RecordShareHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity/:id/shares", h.List)
	rg.POST("/:entity/:id/shares", h.Create)
	rg.DELETE("/:entity/:id/shares/:userId", h.Delete)
}

// shareDTO is the wire shape (camelCase; no tenant/entity/internal ids).
type shareDTO struct {
	UserID    string    `json:"userId,omitempty"` // a person share; empty on a team share
	TeamID    string    `json:"teamId,omitempty"` // a team share (#224); empty on a person share
	TeamName  string    `json:"teamName,omitempty"`
	UserName  string    `json:"userName,omitempty"`
	UserEmail string    `json:"userEmail,omitempty"`
	SharedBy  string    `json:"sharedBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func (h *RecordShareHandler) toDTOs(ctx context.Context, tenantID string, rows []shares.Share) []shareDTO {
	out := make([]shareDTO, len(rows))
	ids := make([]string, 0, len(rows))
	hasTeam := false
	for i, r := range rows {
		out[i] = shareDTO{UserID: r.UserID, TeamID: r.TeamID, SharedBy: r.SharedBy, CreatedAt: r.CreatedAt}
		if r.TeamID != "" {
			hasTeam = true
		} else {
			ids = append(ids, r.UserID)
		}
	}
	if hasTeam && h.teams != nil {
		h.fillTeamNames(ctx, tenantID, out)
	}
	if h.labels == nil || len(ids) == 0 {
		return out
	}
	res, err := h.labels.BatchFetchUserLabels(ctx, tenantID, ids)
	if err != nil {
		// Degrade to ids only — the list must still render.
		h.logger.Debug("handlers.RecordShare.toDTOs: label fetch failed", zap.String("tenant_id", tenantID), zap.Error(err))
		return out
	}
	byID := make(map[string]dynrepo.UserLabelResult, len(res))
	for _, r := range res {
		byID[r.ID] = r
	}
	for i := range out {
		out[i].UserName, out[i].UserEmail = byID[out[i].UserID].Name, byID[out[i].UserID].Email
	}
	return out
}

// fillTeamNames degrades to ids only on a failed read: the list must still render.
func (h *RecordShareHandler) fillTeamNames(ctx context.Context, tenantID string, out []shareDTO) {
	teams, err := h.teams.ListTeams(ctx, tenantID)
	if err != nil {
		h.logger.Warn("handlers.RecordShare.fillTeamNames: team read failed", zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	names := make(map[string]string, len(teams))
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	for i := range out {
		if out[i].TeamID != "" {
			out[i].TeamName = names[out[i].TeamID]
		}
	}
}

// List handles GET /:entity/:id/shares.
func (h *RecordShareHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.List(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondError(c, err, "List")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTOs(c.Request.Context(), tenantID, rows)})
}

type createShareRequest struct {
	UserID string `json:"userId"`
	TeamID string `json:"teamId"`
}

// Create handles POST /:entity/:id/shares.
func (h *RecordShareHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createShareRequest
	if !bindJSON(c, &req) {
		return
	}
	// Exactly one grantee: a person or a team. Both set is ambiguous input, refused before any work.
	if strings.TrimSpace(req.UserID) != "" && strings.TrimSpace(req.TeamID) != "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Chỉ chia sẻ cho một người hoặc một nhóm trong mỗi lần", "code": "RECORD_SHARE_GRANTEE_AMBIGUOUS", "details": gin.H{}})
		return
	}
	var sh *shares.Share
	var err error
	if strings.TrimSpace(req.TeamID) != "" {
		sh, err = h.svc.CreateTeam(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), strings.TrimSpace(c.Param("id")), req.TeamID)
	} else {
		sh, err = h.svc.Create(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), strings.TrimSpace(c.Param("id")), req.UserID)
	}
	if err != nil {
		h.respondError(c, err, "Create")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": h.toDTOs(c.Request.Context(), tenantID, []shares.Share{*sh})[0]})
}

// Delete handles DELETE /:entity/:id/shares/:userId.
func (h *RecordShareHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), strings.TrimSpace(c.Param("id")), c.Param("userId")); err != nil {
		h.respondError(c, err, "Delete")
		return
	}
	c.Status(http.StatusNoContent)
}

// respondError: entity/record/share not found → 404 (same shape as the record routes); a
// *services.ValidationError (FORBIDDEN, RECORD_SHARED_READ_ONLY, RECORD_SHARE_*) via statusForCode;
// anything else an opaque logged 500.
func (h *RecordShareHandler) respondError(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
	case errors.Is(err, shares.ErrShareNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Hồ sơ không được chia sẻ cho người này", "code": "RECORD_SHARE_NOT_FOUND"})
	default:
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.RecordShare."+op, append(logFields(c), zap.Error(err))...)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi xử lý chia sẻ hồ sơ", "code": "RECORD_SHARE_OP_FAILED"})
	}
}
