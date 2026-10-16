package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/services"
)

// NoteService is the slice of *notes.Service the handler needs. Declared here (consumer-side) so
// the handler depends on an interface — the real implementation is *notes.Service — and tests can
// substitute a fake, mirroring FileUploader.
type NoteService interface {
	List(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string, page, limit int) ([]notes.Note, int, error)
	Create(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, body string) (*notes.Note, error)
	Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, noteID string) error
}

// NoteUserLabelFetcher resolves note authors' display names in ONE batched query (no N+1 across a
// page of notes) — the SAME primitive the `user` field type's ?expand= uses
// (dynrepo.Repository.BatchFetchUserLabels). Optional: nil leaves createdByName empty — the FE
// falls back to the raw id, never a fake name (mirrors ApprovalHandler's WithRecordLabels
// degrade-to-empty convention).
type NoteUserLabelFetcher interface {
	BatchFetchUserLabels(ctx context.Context, tenantID string, ids []string) ([]dynrepo.UserLabelResult, error)
}

// NoteHandler serves the RECORD-NOTES engine capability (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d
// mục 1 "Trao đổi trên hồ sơ") — a free-text note any user can attach to ANY record, for EVERY
// entity (not a CRM-only feature):
//
//	GET    /:entity/:id/notes           list, newest first, simple offset pagination
//	POST   /:entity/:id/notes           add a note (body {"body": "..."})
//	DELETE /:entity/:id/notes/:noteId   soft-delete (note author OR entity delete-grant)
//
// Every operation is authorized by the SAME parent-record gate GET/PUT/DELETE /:entity/:id use
// (services.RecordService.ResolveNoteTarget, invoked inside notes.Service) — a caller who cannot
// view the record gets the exact same 404 the record endpoint would give, never a note; row-scope
// (A5) is therefore inherited from the parent record, not re-implemented here.
type NoteHandler struct {
	svc    NoteService
	labels NoteUserLabelFetcher
	logger *zap.Logger
}

// NewNoteHandler wires the handler over the notes service. logger may be nil.
func NewNoteHandler(svc NoteService, logger *zap.Logger) *NoteHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &NoteHandler{svc: svc, logger: logger}
}

// WithUserLabels injects the OPTIONAL author-name enrichment dep (point 6 of the spec: "trả về
// kèm tên người viết"). Chainable, mirroring ApprovalHandler.WithRecordLabels.
func (h *NoteHandler) WithUserLabels(f NoteUserLabelFetcher) *NoteHandler {
	h.labels = f
	return h
}

// RegisterEntityRoutes mounts the list/create routes. router.go mounts them on a group pinned to
// the "view" action (specs/060 #349): reading notes rides the same gate as reading the record, and
// adding a note needs only that the caller can read the record — NOT the "create" grant the default
// POST→create map of the record group would ask for (that opens a NEW record).
func (h *NoteHandler) RegisterEntityRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity/:id/notes", h.List)
	rg.POST("/:entity/:id/notes", h.Create)
}

// RegisterDeleteRoute mounts DELETE /:entity/:id/notes/:noteId on a group pinned to the "view"
// action (middleware.RequireACLAction) rather than the default DELETE→"delete" mapping: the
// delete rule is "note author OR entity delete-grant" (enforced inside notes.Service.Delete), and
// a caller who only holds view/create — not delete — on the entity must still be able to remove
// their OWN note. Requiring "delete" at the middleware layer would block that caller before the
// handler ever runs. "view" is the floor every notes operation needs anyway
// (services.RecordService.ResolveNoteTarget re-resolves it inside the service regardless), so this
// mount is defense in depth, not the load-bearing check — see internal/services/record_note.go.
func (h *NoteHandler) RegisterDeleteRoute(rg *gin.RouterGroup) {
	rg.DELETE("/:entity/:id/notes/:noteId", h.Delete)
}

// noteDTO is the wire shape of a note — deliberately NOT notes.Note serialized directly (that
// struct carries `db` tags only, PascalCase field names, and internal columns like tenant_id/
// deleted_at that must never reach the client). camelCase JSON per api-conventions.md.
type noteDTO struct {
	ID            string    `json:"id"`
	Body          string    `json:"body"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByName string    `json:"createdByName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// toDTOs maps notes.Note rows to the wire shape and best-effort decorates each with the author's
// display name: ONE batched BatchFetchUserLabels call over the page's DISTINCT created_by ids (no
// N+1), matching the humanizePending discipline in approval_handler.go. Any resolution failure
// (fetcher unwired, or the batch call errors) degrades to empty createdByName rather than failing
// the request — the notes tab must still render.
func (h *NoteHandler) toDTOs(ctx context.Context, tenantID string, rows []notes.Note) []noteDTO {
	out := make([]noteDTO, len(rows))
	for i, r := range rows {
		out[i] = noteDTO{
			ID: r.ID, Body: r.Body, CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
	}
	if h.labels == nil || len(rows) == 0 {
		return out
	}
	seen := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.CreatedBy == "" {
			continue
		}
		if _, ok := seen[r.CreatedBy]; ok {
			continue
		}
		seen[r.CreatedBy] = struct{}{}
		ids = append(ids, r.CreatedBy)
	}
	if len(ids) == 0 {
		return out
	}
	res, err := h.labels.BatchFetchUserLabels(ctx, tenantID, ids)
	if err != nil {
		h.logger.Debug("handlers.Note.toDTOs: author label fetch failed (rows keep empty createdByName)",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return out
	}
	names := make(map[string]string, len(res))
	for _, r := range res {
		names[r.ID] = r.Name
	}
	for i := range out {
		out[i].CreatedByName = names[out[i].CreatedBy]
	}
	return out
}

// List handles GET /:entity/:id/notes — simple offset pagination (?page=&limit=), newest first.
func (h *NoteHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	entity := strings.TrimSpace(c.Param("entity"))
	recordID := strings.TrimSpace(c.Param("id"))
	page := atoiDefault(c.Query("page"), 1)
	limit := atoiDefault(c.Query("limit"), 20)

	rows, total, err := h.svc.List(c.Request.Context(), scopeOf(c), tenantID, entity, recordID, page, limit)
	if err != nil {
		h.respondError(c, err, "List")
		return
	}
	page, limit = dynrepo.ClampPageLimit(page, limit)
	c.JSON(http.StatusOK, gin.H{
		"data": h.toDTOs(c.Request.Context(), tenantID, rows),
		"pagination": gin.H{
			"page":       page,
			"limit":      limit,
			"total":      total,
			"totalPages": (total + limit - 1) / limit,
		},
	})
}

// createNoteRequest is the POST /:entity/:id/notes body.
type createNoteRequest struct {
	Body string `json:"body"`
}

// Create handles POST /:entity/:id/notes.
func (h *NoteHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createNoteRequest
	if !bindJSON(c, &req) {
		return
	}
	entity := strings.TrimSpace(c.Param("entity"))
	recordID := strings.TrimSpace(c.Param("id"))

	n, err := h.svc.Create(c.Request.Context(), scopeOf(c), tenantID, entity, recordID, req.Body)
	if err != nil {
		h.respondError(c, err, "Create")
		return
	}
	dtos := h.toDTOs(c.Request.Context(), tenantID, []notes.Note{*n})
	c.JSON(http.StatusCreated, gin.H{"data": dtos[0]})
}

// Delete handles DELETE /:entity/:id/notes/:noteId.
func (h *NoteHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	entity := strings.TrimSpace(c.Param("entity"))
	recordID := strings.TrimSpace(c.Param("id"))
	noteID := strings.TrimSpace(c.Param("noteId"))

	if err := h.svc.Delete(c.Request.Context(), scopeOf(c), tenantID, entity, recordID, noteID); err != nil {
		h.respondError(c, err, "Delete")
		return
	}
	c.Status(http.StatusNoContent)
}

// respondError maps notes/record errors to HTTP: entity/record/note not found → 404 (no
// existence leak — identical shape to a direct record GET); a *services.ValidationError (
// RECORD_NOTE_EMPTY/RECORD_NOTE_TOO_LONG/FORBIDDEN) maps via the shared statusForCode; anything
// else is an opaque 500, logged with request context (RM-36 OBS-1 discipline).
func (h *NoteHandler) respondError(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
	case errors.Is(err, notes.ErrNoteNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Ghi chú không tồn tại", "code": "RECORD_NOTE_NOT_FOUND"})
	default:
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.Note."+op, append(logFields(c), zap.Error(err))...)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi xử lý ghi chú", "code": "NOTE_OP_FAILED"})
	}
}
