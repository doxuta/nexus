package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// AdminWorkflowHandler exposes the EPIC 4 workflow-management endpoints under /admin/workflows
// (CRUD). It sits behind the AuthMiddleware → RequireTenant → RequireAdmin chain (mounted in
// router.go on the admin-gated group), so the tenant is always the verified claim and every
// caller is already an admin. The rules it manages are what the Event Engine fires after a
// record mutation on the dynamic /:entity surface.
type AdminWorkflowHandler struct {
	svc    *services.WorkflowService
	logger *zap.Logger
	// schedulerInterval is what main wired (0 = scheduler off / never wired). Studio reads it so
	// a builder is told a "scheduled" rule will not fire BEFORE saving one (#291).
	schedulerInterval time.Duration
}

// WithSchedulerStatus records the background scheduler's tick interval (0 = disabled).
func (h *AdminWorkflowHandler) WithSchedulerStatus(interval time.Duration) *AdminWorkflowHandler {
	h.schedulerInterval = interval
	return h
}

// SchedulerStatus answers GET /admin/workflows/scheduler: whether "scheduled" rules can run.
func (h *AdminWorkflowHandler) SchedulerStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"enabled":         h.schedulerInterval > 0,
		"intervalSeconds": h.schedulerInterval.Seconds(),
	}})
}

// NewAdminWorkflowHandler wires the handler over the workflow service.
func NewAdminWorkflowHandler(svc *services.WorkflowService, logger *zap.Logger) *AdminWorkflowHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminWorkflowHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the workflow CRUD routes under the given (admin-gated) group. The W6
// additions: run history (per-rule + tenant-recent — the static "runs" segment resolves ahead of
// the :id param, gin static-over-param) and the side-effect-free dry-run.
func (h *AdminWorkflowHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/workflows", h.List)
	rg.GET("/admin/workflows/runs", h.RecentRuns)
	rg.GET("/admin/workflows/scheduler", h.SchedulerStatus)
	rg.GET("/admin/workflows/:id", h.Get)
	rg.GET("/admin/workflows/:id/runs", h.Runs)
	rg.POST("/admin/workflows", h.Create)
	rg.POST("/admin/workflows/:id/test", h.Test)
	rg.POST("/admin/workflows/:id/disable-now", h.DisableNow)
	rg.PUT("/admin/workflows/:id", h.Update)
	rg.PATCH("/admin/workflows/:id/active", h.SetActive)
	rg.DELETE("/admin/workflows/:id", h.Delete)
}

// workflowRuleRequest is one condition predicate in a workflow body.
type workflowRuleRequest struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// workflowActionRequest is one action in a workflow body.
type workflowActionRequest struct {
	Type  string `json:"type"`
	Field string `json:"field,omitempty"`
	Value any    `json:"value,omitempty"`
	// field_update through a relation (specs/058): the relation field of the rule's entity whose
	// record is written; `field` then names a field of the related entity.
	Via string `json:"via,omitempty"`
	// field_update fanned out to every child record (specs/084).
	ChildEntity string `json:"childEntity,omitempty"`
	ChildVia    string `json:"childVia,omitempty"`
	UserID      string `json:"userId,omitempty"`
	Title       string `json:"title,omitempty"`
	Message     string `json:"message,omitempty"`
	// email_alert
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	// outbound_webhook
	URL     string            `json:"url,omitempty"`
	Secret  string            `json:"secret,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// outbound_webhook through the named-credential vault (specs/050 S2b): the key's id + the path
	// appended to its base URL — instead of url/secret/headers.
	CredentialID string `json:"credentialId,omitempty"`
	Path         string `json:"path,omitempty"`
	// assign (ASSIGN-1): strategy ("round_robin") + team id (the target field reuses `field`).
	Strategy string `json:"strategy,omitempty"`
	Team     string `json:"team,omitempty"`
	// create_record (CREATE-RECORD): entityName is the target entity to insert into; values is the
	// field→value map for the new record (literals + the "$owner" sentinel resolved at fire time).
	CreateEntity string         `json:"entityName,omitempty"`
	Values       map[string]any `json:"values,omitempty"`
	// set_deadline: how far AFTER the fire instant the computed deadline lands. Same
	// {amount, unit} shape the timer block carries (`field` is the datetime target).
	Offset *struct {
		Amount int    `json:"amount"`
		Unit   string `json:"unit"`
	} `json:"offset,omitempty"`
	// set_deadline policy (queue #81): overwrite|tighten_only, the datetime field that keeps the
	// machine's stamp, and now|created_at as the anchor. Validated in buildActionsJSON.
	Mode       string `json:"mode,omitempty"`
	StampField string `json:"stampField,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	// set_deadline (queue #79): the working calendar a business_hours offset walks instead of the
	// tenant default. Existence is checked on save (SET_DEADLINE_CALENDAR_NOT_FOUND).
	CalendarID string `json:"calendarId,omitempty"`
	// before_save field_update (specs/048 S4): fill only when empty; how `value` is read
	// (constant | field | template). Refused on every other trigger by the validator.
	OnlyIfEmpty bool   `json:"onlyIfEmpty,omitempty"`
	ValueSource string `json:"valueSource,omitempty"`
	// before_save normalize (specs/048 S5): the text rewrites in order (trim | lower | upper |
	// collapse_spaces). Refused on any other action by the validator.
	Ops []string `json:"ops,omitempty"`
	// start_approval (specs/043 S9b): the manual approval process the rule starts.
	ProcessID string `json:"processId,omitempty"`
}

// workflowConditionRequest is the condition block of a workflow body. Rules may be empty/omitted
// (match-all). Match is reserved for "or"; M1 supports "and"/empty. Timer is the TIMER-1
// date_offset config, carried on the wire exactly where it is stored (inside the condition JSON):
// required when trigger=date_offset, rejected on any other trigger.
type workflowConditionRequest struct {
	Match string                `json:"match,omitempty"`
	Rules []workflowRuleRequest `json:"rules,omitempty"`
	Timer *workflowTimerRequest `json:"timer,omitempty"`
	// SaveOn narrows a before_save rule to "create" | "update" (specs/048); absent = both.
	SaveOn string `json:"saveOn,omitempty"`
}

// workflowTimerRequest is the date_offset trigger config (TIMER-1):
// {dateField, offset:{amount,unit}, direction, calendarId?}.
type workflowTimerRequest struct {
	DateField string `json:"dateField"`
	Offset    struct {
		Amount int    `json:"amount"`
		Unit   string `json:"unit"`
	} `json:"offset"`
	Direction  string `json:"direction"`
	CalendarID string `json:"calendarId,omitempty"`
	// Repeat (optional) makes the reminder recurring: every N days/weeks, Count times in total.
	// Absent = fire once, which is every rule written before recurrence existed.
	Repeat *struct {
		Every int    `json:"every"`
		Unit  string `json:"unit"`
		Count int    `json:"count"`
	} `json:"repeat,omitempty"`
}

// workflowRequest is the POST/PUT body for a workflow rule. isActive defaults to true when omitted.
// intervalSeconds is the scheduled-rule cadence (ignored for event triggers).
type workflowRequest struct {
	Name            string                    `json:"name"`
	EntityName      string                    `json:"entityName"`
	Trigger         string                    `json:"trigger"`
	Condition       *workflowConditionRequest `json:"condition,omitempty"`
	Actions         []workflowActionRequest   `json:"actions"`
	IsActive        *bool                     `json:"isActive,omitempty"`
	IntervalSeconds int                       `json:"intervalSeconds,omitempty"`
}

// toServiceRules maps request rules + the TIMER-1 timer block + the specs/048 saveOn to the
// service input types.
func toServiceRules(c *workflowConditionRequest) (string, []services.WorkflowRuleInput, *services.WorkflowTimerInput, string) {
	if c == nil {
		return "", nil, nil, ""
	}
	rules := make([]services.WorkflowRuleInput, len(c.Rules))
	for i, r := range c.Rules {
		rules[i] = services.WorkflowRuleInput{Field: r.Field, Op: r.Op, Value: r.Value}
	}
	var timer *services.WorkflowTimerInput
	if c.Timer != nil {
		timer = &services.WorkflowTimerInput{
			DateField:  c.Timer.DateField,
			Offset:     services.WorkflowTimerOffsetInput{Amount: c.Timer.Offset.Amount, Unit: c.Timer.Offset.Unit},
			Direction:  c.Timer.Direction,
			CalendarID: c.Timer.CalendarID,
		}
		if r := c.Timer.Repeat; r != nil {
			timer.Repeat = &services.WorkflowTimerRepeatInput{Every: r.Every, Unit: r.Unit, Count: r.Count}
		}
	}
	return c.Match, rules, timer, c.SaveOn
}

// toServiceActions maps request actions to the service input type.
func toServiceWorkflowActions(in []workflowActionRequest) []services.WorkflowActionInput {
	out := make([]services.WorkflowActionInput, len(in))
	for i, a := range in {
		out[i] = services.WorkflowActionInput{
			Type:         a.Type,
			Field:        a.Field,
			Value:        a.Value,
			Via:          a.Via,
			ChildEntity:  a.ChildEntity,
			ChildVia:     a.ChildVia,
			UserID:       a.UserID,
			Title:        a.Title,
			Message:      a.Message,
			To:           a.To,
			Subject:      a.Subject,
			Body:         a.Body,
			URL:          a.URL,
			Secret:       a.Secret,
			Headers:      a.Headers,
			CredentialID: a.CredentialID,
			Path:         a.Path,
			Strategy:     a.Strategy,
			Team:         a.Team,
			CreateEntity: a.CreateEntity,
			Values:       a.Values,
			Mode:         a.Mode,
			StampField:   a.StampField,
			Anchor:       a.Anchor,
			CalendarID:   a.CalendarID,
			OnlyIfEmpty:  a.OnlyIfEmpty,
			ValueSource:  a.ValueSource,
			Ops:          a.Ops,
			ProcessID:    a.ProcessID,
		}
		if o := a.Offset; o != nil {
			out[i].Offset = &services.WorkflowTimerOffsetInput{Amount: o.Amount, Unit: o.Unit}
		}
	}
	return out
}

// List handles GET /admin/workflows: returns every workflow in the tenant.
func (h *AdminWorkflowHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListWorkflows(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminWorkflow.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê workflow", "code": "WORKFLOW_LIST_FAILED"})
		return
	}
	for i := range rows {
		rows[i].Actions = workflow.RedactActionsJSON(rows[i].Actions)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"workflows": rows}})
}

// redacted is rec as this API may show it (specs/050 S3a, D-11): a copy whose actions went through
// workflow.RedactActionsJSON. The mask is applied HERE, at the HTTP boundary, and nowhere below —
// the service's GetWorkflow feeds the dry-run and the repository feeds the timer / webhook job
// handlers, which must send the real key.
func redacted(rec *workflow.WorkflowRecord) *workflow.WorkflowRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	out.Actions = workflow.RedactActionsJSON(rec.Actions)
	return &out
}

// Get handles GET /admin/workflows/:id: returns one tenant-scoped workflow, 404 unknown.
func (h *AdminWorkflowHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rec, err := h.svc.GetWorkflow(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"workflow": redacted(rec)}})
}

// Create handles POST /admin/workflows: validates and registers a workflow rule. An invalid
// trigger/condition/action or missing name/entity is 422; an unknown entity is 422. The created
// rule is returned with 201.
func (h *AdminWorkflowHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req workflowRequest
	if !bindJSON(c, &req) {
		return
	}
	match, rules, timer, saveOn := toServiceRules(req.Condition)
	rec, err := h.svc.CreateWorkflow(auditContext(c), tenantID, services.CreateWorkflowInput{
		Name:            req.Name,
		EntityName:      req.EntityName,
		Trigger:         req.Trigger,
		Match:           match,
		Rules:           rules,
		Timer:           timer,
		SaveOn:          saveOn,
		Actions:         toServiceWorkflowActions(req.Actions),
		IsActive:        req.IsActive,
		IntervalSeconds: req.IntervalSeconds,
		ActorID:         actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"workflow": redacted(rec)}})
}

// Update handles PUT /admin/workflows/:id: full-replace of a workflow rule. Unknown id is 404;
// invalid input 422.
func (h *AdminWorkflowHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req workflowRequest
	if !bindJSON(c, &req) {
		return
	}
	match, rules, timer, saveOn := toServiceRules(req.Condition)
	rec, err := h.svc.UpdateWorkflow(auditContext(c), tenantID, id, services.UpdateWorkflowInput{
		Name:            req.Name,
		EntityName:      req.EntityName,
		Trigger:         req.Trigger,
		Match:           match,
		Rules:           rules,
		Timer:           timer,
		SaveOn:          saveOn,
		Actions:         toServiceWorkflowActions(req.Actions),
		IsActive:        req.IsActive,
		IntervalSeconds: req.IntervalSeconds,
		ActorID:         actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"workflow": redacted(rec)}})
}

// DisableNow handles POST /admin/workflows/:id/disable-now ("Tắt ngay cho khách", specs/063 FR-017):
// switches the LIVE rule off at once, leaving every draft alone. 404 for an unknown id and for a rule
// that exists only as a draft. Returns the live record.
func (h *AdminWorkflowHandler) DisableNow(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rec, err := h.svc.DisableNow(auditContext(c), tenantID, id, actorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "DisableNow", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"workflow": redacted(rec)}})
}

// workflowActiveRequest is the body of the pause / resume switch.
type workflowActiveRequest struct {
	IsActive *bool `json:"isActive" binding:"required"`
}

// SetActive handles PATCH /admin/workflows/:id/active {isActive}: pause or resume one rule without
// resending its actions (a PUT of a rule that still holds a pre-vault key is refused with 409 — the
// way out the Doctor C10 refusal names). Tenant-scoped; unknown / cross-tenant id → 404.
func (h *AdminWorkflowHandler) SetActive(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req workflowActiveRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.SetWorkflowActive(auditContext(c), tenantID, id, *req.IsActive, actorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "SetActive", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"workflow": redacted(rec)}})
}

// Runs handles GET /admin/workflows/:id/runs?limit=&offset=: one rule's run history, newest
// first. Tenant-scoped; unknown / cross-tenant workflow → 404. limit defaults to 20, hard cap
// 100; offset ≥ 0 (clamped in the service).
func (h *AdminWorkflowHandler) Runs(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	limit := atoiDefault(c.Query("limit"), 0)
	offset := atoiDefault(c.Query("offset"), 0)
	rows, total, err := h.svc.ListWorkflowRuns(c.Request.Context(), tenantID, id, limit, offset)
	if err != nil {
		h.respondServiceError(c, err, "Runs", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"runs": rows,
		"pagination": gin.H{
			"limit":  clampedRunsLimit(limit),
			"offset": maxInt(offset, 0),
			"total":  total,
		},
	}})
}

// RecentRuns handles GET /admin/workflows/runs?limit=&workflowIds=: the tenant's most recent runs
// (admin overview strip). Cheap — no COUNT. workflowIds (comma-separated) narrows the read to those
// rules — the automation panel of one app (queue #297); present but empty = no rule = no run.
func (h *AdminWorkflowHandler) RecentRuns(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	limit := atoiDefault(c.Query("limit"), 0)
	var workflowIDs []string // nil = param absent = every rule
	if raw, present := c.GetQuery("workflowIds"); present {
		workflowIDs = []string{}
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				workflowIDs = append(workflowIDs, id)
			}
		}
	}
	rows, err := h.svc.ListRecentWorkflowRuns(c.Request.Context(), tenantID, limit, workflowIDs)
	if err != nil {
		h.respondServiceError(c, err, "RecentRuns", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"runs":       rows,
		"pagination": gin.H{"limit": clampedRunsLimit(limit)},
	}})
}

// clampedRunsLimit mirrors the service clamp for the pagination echo (default 20, cap 100).
func clampedRunsLimit(limit int) int {
	if limit < 1 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// maxInt returns the larger of a and b (offset echo helper).
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// workflowTestRequest is the POST /admin/workflows/:id/test body: the EXISTING record of the
// rule's entity to evaluate against, and (specs/048 S6) the simulated write — op create|update and
// the fields it changes. recordId may be omitted on op=create.
type workflowTestRequest struct {
	RecordID string         `json:"recordId"`
	Op       string         `json:"op"`
	Changes  map[string]any `json:"changes"`
	// WithEntityRules (#442): judge a before_save rule inside the entity's whole before_save chain.
	WithEntityRules bool `json:"withEntityRules"`
}

// Test handles POST /admin/workflows/:id/test: dry-runs the rule on a simulated write with ZERO
// side effects (no mutation, no sends, no run — previews only). 404 unknown workflow/record; 422
// blank recordId, bad op, or a change a save would refuse. Returns the per-step trace
// {step, kind, matched/evaluated, detail}.
func (h *AdminWorkflowHandler) Test(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req workflowTestRequest
	if !bindJSON(c, &req) {
		return
	}
	result, err := h.svc.DryRun(c.Request.Context(), tenantID, id,
		services.WorkflowTestInput{RecordID: req.RecordID, Op: req.Op, Changes: req.Changes, WithEntityRules: req.WithEntityRules})
	if err != nil {
		h.respondServiceError(c, err, "Test", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"test": result}})
}

// Delete handles DELETE /admin/workflows/:id: soft-deletes the rule. Unknown id is 404.
func (h *AdminWorkflowHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteWorkflow(auditContext(c), tenantID, id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	noContentWV(c)
}

// respondServiceError maps a workflow-service error to the right HTTP status: a ValidationError →
// its code's status (404/422 via statusForCode); anything else → a logged 500. It forwards
// ve.Details so a field-level reject (the WF-01 field_update guard: RECORD_UNKNOWN_FIELD /
// RECORD_*_READONLY built via invalidField) carries details.field per api-conventions §9.1 — the
// FE reads details.field directly instead of regex-parsing the message. invalid() sets Details to
// an empty object, so non-field errors emit "details": {} consistently.
func (h *AdminWorkflowHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminWorkflow."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu workflow", "code": "WORKFLOW_OP_FAILED"})
}
