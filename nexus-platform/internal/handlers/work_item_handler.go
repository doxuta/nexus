package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// WorkItemHandler serves "Việc của tôi" (specs/043 S5): the inbox read off the work seats, and
// claim / release / complete of a pool seat. Mounted on the protected (non-admin) group — any
// signed-in user; the service answers "not found" to anyone who is not a candidate of an item.
//
// Approve / reject / delegate are deliberately NOT routes here: the inbox calls the existing
// /approvals/:instanceId/approve|reject|delegate, where the server finds the actor's seat
// (including a pool seat they claimed).
type WorkItemHandler struct {
	svc    *services.WorkItemService
	logger *zap.Logger
	// Optional display-label deps (WithRecordLabels): nil → items carry empty labels and the FE
	// falls back to its honest placeholder, never a fake name.
	meta   approvalMetaReader
	labels services.LabelFetcher
	hidden fieldViewChecker
}

// NewWorkItemHandler wires the handler over the work-item service.
func NewWorkItemHandler(svc *services.WorkItemService, logger *zap.Logger) *WorkItemHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WorkItemHandler{svc: svc, logger: logger}
}

// WithRecordLabels arms the entity/record label decoration — the SAME resolver the approvals inbox
// uses (one metadata read + one batched label fetch per entity). Chainable.
func (h *WorkItemHandler) WithRecordLabels(meta approvalMetaReader, labels services.LabelFetcher) *WorkItemHandler {
	h.meta, h.labels = meta, labels
	return h
}

// WithFieldACL arms the hidden-field check on the label decoration: a record label read off a field
// the caller's role may not view is left empty (FR-015). Chainable.
func (h *WorkItemHandler) WithFieldACL(f fieldViewChecker) *WorkItemHandler {
	h.hidden = f
	return h
}

// RegisterUserRoutes mounts the work-item routes on the protected group BEFORE the dynamic
// /:entity dispatcher. The path has a hyphen on purpose: an entity name can never contain one, so
// it cannot be shadowed by (or shadow) a tenant's own entity called "tasks".
func (h *WorkItemHandler) RegisterUserRoutes(rg *gin.RouterGroup) {
	rg.GET("/work-items", h.List)
	rg.GET("/work-items/counts", h.Counts)
	rg.POST("/work-items/:id/claim", h.Claim)
	rg.POST("/work-items/:id/release", h.Release)
	rg.POST("/work-items/:id/complete", h.Complete)
}

// workItemJSON is one work item on the wire (camelCase). Names, not codes, for everything a person
// reads: the process, the step, the record, who it is for.
type workItemJSON struct {
	ID             string     `json:"id"`
	InstanceID     string     `json:"instanceId"`
	ProcessName    string     `json:"processName"`
	NodeID         string     `json:"nodeId"`
	NodeName       string     `json:"nodeName"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	EntityName     string     `json:"entityName"`
	EntityLabel    string     `json:"entityLabel"`
	RecordID       string     `json:"recordId"`
	RecordLabel    string     `json:"recordLabel"`
	AssigneeType   string     `json:"assigneeType"`
	AssigneeName   string     `json:"assigneeName"`
	ClaimedBy      string     `json:"claimedBy,omitempty"`
	ClaimedByMe    bool       `json:"claimedByMe"`
	DueAt          *time.Time `json:"dueAt"`
	DueBasis       string     `json:"dueBasis,omitempty"`
	Overdue        bool       `json:"overdue"`
	Paused         bool       `json:"paused"`
	CreatedAt      time.Time  `json:"createdAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	RequiredFields []string   `json:"requiredFields,omitempty"`
	// ReturnReason: on an open rework item, why the approval was returned (the rejector's note).
	ReturnReason string `json:"returnReason,omitempty"`
}

func toWorkItemJSON(it bpm.WorkItem, userID string, now time.Time) workItemJSON {
	return workItemJSON{
		ID: it.ID, InstanceID: it.InstanceID, ProcessName: it.ProcessName,
		NodeID: it.NodeID, NodeName: it.NodeName, Kind: it.Kind, Status: it.Status,
		EntityName: it.EntityName, RecordID: it.RecordID,
		AssigneeType: string(it.AssigneeType), AssigneeName: it.AssigneeName,
		ClaimedBy: it.ClaimedBy, ClaimedByMe: it.ClaimedBy != "" && it.ClaimedBy == userID,
		DueAt: it.DueAt, DueBasis: string(it.DueBasis),
		Overdue:   it.Status == "open" && !it.Paused && it.DueAt != nil && it.DueAt.Before(now),
		Paused:    it.Paused,
		CreatedAt: it.CreatedAt, CompletedAt: it.CompletedAt, RequiredFields: it.RequiredFields, ReturnReason: it.ReturnReason,
	}
}

// decorate fills entityLabel/recordLabel on the page (best-effort, field-ACL honoured).
func (h *WorkItemHandler) decorate(ctx context.Context, tenantID string, sub auth.Subject, items []workItemJSON) {
	if len(items) == 0 {
		return
	}
	refs := make([]recordRef, len(items))
	for i, it := range items {
		refs[i] = recordRef{Entity: it.EntityName, RecordID: it.RecordID}
	}
	l := recordLabeler{meta: h.meta, labels: h.labels, hidden: h.hidden, logger: h.logger}
	entityLabels, recordLabels := l.resolve(ctx, tenantID, sub.RoleID, sub.IsAdmin, refs)
	for i := range items {
		items[i].EntityLabel = entityLabels[items[i].EntityName]
		if m := recordLabels[items[i].EntityName]; m != nil {
			items[i].RecordLabel = m[items[i].RecordID]
		}
	}
}

func workItemActorOf(c *gin.Context) (services.WorkItemActor, auth.Subject) {
	sub, _ := middleware.SubjectFromContext(c)
	return services.WorkItemActor{UserID: sub.UserID, RoleID: sub.RoleID, IsAdmin: sub.IsAdmin}, sub
}

// List handles GET /work-items?view=mine|pool|done&entity=&limit=&cursor=.
func (h *WorkItemHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	actor, sub := workItemActorOf(c)
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	page, err := h.svc.List(c.Request.Context(), tenantID, actor, c.Query("view"), c.Query("entity"), limit, c.Query("cursor"))
	if err != nil {
		h.respondError(c, err, "List", tenantID)
		return
	}
	items := make([]workItemJSON, len(page.Items))
	for i, it := range page.Items {
		items[i] = toWorkItemJSON(it, actor.UserID, page.Now)
	}
	h.decorate(c.Request.Context(), tenantID, sub, items)
	var next any
	if page.NextCursor != "" {
		next = page.NextCursor
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items, "nextCursor": next}})
}

// Counts handles GET /work-items/counts — the home card: {mine, pool, overdue}.
func (h *WorkItemHandler) Counts(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	actor, _ := workItemActorOf(c)
	counts, err := h.svc.Counts(c.Request.Context(), tenantID, actor)
	if err != nil {
		h.respondError(c, err, "Counts", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"mine": counts.Mine, "pool": counts.Pool, "overdue": counts.Overdue}})
}

// Claim handles POST /work-items/:id/claim.
func (h *WorkItemHandler) Claim(c *gin.Context) {
	h.act(c, "Claim", func(ctx context.Context, tenantID, id string, actor services.WorkItemActor) (*bpm.WorkItem, error) {
		return h.svc.Claim(ctx, tenantID, id, actor)
	})
}

// Release handles POST /work-items/:id/release.
func (h *WorkItemHandler) Release(c *gin.Context) {
	h.act(c, "Release", func(ctx context.Context, tenantID, id string, actor services.WorkItemActor) (*bpm.WorkItem, error) {
		return h.svc.Release(ctx, tenantID, id, actor)
	})
}

// act is the shared claim/release shape: run the verb, answer with the item as it now stands.
func (h *WorkItemHandler) act(c *gin.Context, op string, run func(ctx context.Context, tenantID, id string, actor services.WorkItemActor) (*bpm.WorkItem, error)) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	actor, sub := workItemActorOf(c)
	it, err := run(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), actor)
	if err != nil {
		h.respondError(c, err, op, tenantID)
		return
	}
	items := []workItemJSON{toWorkItemJSON(*it, actor.UserID, time.Now().UTC())}
	h.decorate(c.Request.Context(), tenantID, sub, items)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"workItem": items[0]}})
}

// workItemCompleteRequest is the body of complete: an optional note ("comment" accepted as the
// legacy alias the approval verbs also take).
type workItemCompleteRequest struct {
	Note    string `json:"note,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Complete handles POST /work-items/:id/complete {note}.
func (h *WorkItemHandler) Complete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req workItemCompleteRequest
	if !optionalBody(c, &req) {
		return
	}
	note := req.Note
	if strings.TrimSpace(note) == "" {
		note = req.Comment
	}
	actor, _ := workItemActorOf(c)
	inst, err := h.svc.Complete(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), note, actor)
	if err != nil {
		h.respondError(c, err, "Complete", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": inst}})
}

// respondError maps a service error to the right status; a coded validation error keeps its
// details (complete lists the unfilled fields in details.fields).
func (h *WorkItemHandler) respondError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		body := gin.H{"error": ve.Msg, "code": ve.Code}
		if len(ve.Details) > 0 {
			body["details"] = ve.Details
		}
		c.JSON(statusForCode(ve.Code), body)
		return
	}
	h.logger.Error("handlers.WorkItem."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu", "code": "WORK_ITEM_OP_FAILED"})
}
