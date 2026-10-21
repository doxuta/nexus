package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// ApprovalHandler exposes the EPIC 5 BPM/Approval endpoints. It has TWO surfaces:
//
//   - ADMIN process CRUD under /admin/approval-processes — mounted on the admin-gated group
//     (Auth → Tenant → RequireAdmin). These define the approval rules.
//   - USER approval surface under /approvals — mounted on the protected (non-admin) group. Any
//     authenticated user can see THEIR pending approvals and approve/reject the ones assigned to
//     them; the service enforces that the caller is the current step's approver.
//
// The tenant is always the verified claim; the actor (and their role) come from the JWT subject.
type ApprovalHandler struct {
	svc    *services.ApprovalService
	logger *zap.Logger
	// Optional F7-4 label-enrichment deps (WithRecordLabels). nil → pending rows carry empty
	// labels and the FE falls back to the mono slug+id (honest fallback, never a fake name).
	meta   approvalMetaReader
	labels services.LabelFetcher
	// hidden resolves the caller's field-ACL hidden set (WithFieldACL). Optional: nil keeps the
	// pre-existing behaviour. Needed because the label decoration reads a field VALUE off another
	// entity's rows, and the preference list includes `email` — so an explicitly configured
	// sys_field_acl hide was ignored by this one surface while every record read honours it.
	hidden fieldViewChecker // the package-shared field-ACL slice (metadata_handler.go)
}

// WithFieldACL arms the hidden-field check on the pending-list label decoration. Chainable.
func (h *ApprovalHandler) WithFieldACL(f fieldViewChecker) *ApprovalHandler {
	h.hidden = f
	return h
}

// approvalMetaReader is the metadata slice the pending-list humanizer needs: the entity's
// display label + its field list (to pick a labelField). Satisfied by *services.MetaCache.
type approvalMetaReader interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// NewApprovalHandler wires the handler over the approval service.
func NewApprovalHandler(svc *services.ApprovalService, logger *zap.Logger) *ApprovalHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ApprovalHandler{svc: svc, logger: logger}
}

// WithRecordLabels injects the OPTIONAL display-label deps for GET /approvals/pending (F7-4
// "humanize approvals"): meta resolves entity label + labelField, labels is the SAME
// dynrepo.BatchFetchLabels path ?expand= uses (services.LabelFetcher — no second labeler).
// Chainable, mirroring the WithX wiring style.
func (h *ApprovalHandler) WithRecordLabels(meta approvalMetaReader, labels services.LabelFetcher) *ApprovalHandler {
	h.meta = meta
	h.labels = labels
	return h
}

// RegisterAdminRoutes mounts the approval-process CRUD under the admin-gated group.
func (h *ApprovalHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/approval-processes", h.ListProcesses)
	rg.GET("/admin/approval-processes/:id", h.GetProcess)
	rg.POST("/admin/approval-processes", h.CreateProcess)
	rg.PUT("/admin/approval-processes/:id", h.UpdateProcess)
	rg.DELETE("/admin/approval-processes/:id", h.DeleteProcess)
	// Draft / publish / version history (specs/043 S10).
	rg.PUT("/admin/approval-processes/:id/draft", h.SaveDraft)
	rg.DELETE("/admin/approval-processes/:id/draft", h.DiscardDraft)
	rg.POST("/admin/approval-processes/:id/publish", h.PublishDraft)
	rg.GET("/admin/approval-processes/:id/versions", h.ListVersions)
	rg.GET("/admin/approval-processes/:id/versions/:version", h.GetVersion)
	// An admin deciding a many-seat step in place of its seats (specs/043 S4, D-13).
	rg.POST("/admin/approval-instances/:id/override", h.Override)
	// specs/043 S7: a hồ sơ stalled on an automatic step — fix the cause, press "Thử lại"; or stop it.
	rg.POST("/admin/approval-instances/:id/retry", h.Retry)
	rg.POST("/admin/approval-instances/:id/cancel", h.Cancel)
	// Impact + repin of running hồ sơ (specs/043 S11).
	rg.GET("/admin/approval-processes/:id/impact", h.ProcessImpact)
	rg.GET("/admin/approval-processes/:id/instances", h.ListProcessInstances)
	rg.POST("/admin/approval-processes/:id/repin", h.RepinInstances)
}

// RegisterUserRoutes mounts the user-facing approval surface under the protected (non-admin) group.
// These static "approvals" segments must be registered BEFORE the dynamic /:entity dispatcher so
// they resolve ahead of the catch-all param route. GET /approvals/:id (W6 inbox deep-link) sits
// after the static "pending" sibling — gin resolves static segments ahead of the :id param.
func (h *ApprovalHandler) RegisterUserRoutes(rg *gin.RouterGroup) {
	rg.GET("/approvals/pending", h.ListPending)
	rg.GET("/approvals/:id", h.GetDetail)
	rg.POST("/approvals/:id/approve", h.Approve)
	rg.POST("/approvals/:id/reject", h.Reject)
	rg.POST("/approvals/:id/recall", h.Recall)
	rg.POST("/approvals/:id/delegate", h.Delegate)
}

// RegisterEntityRoutes mounts the per-record approval read route under the ACL-gated record group.
// GET /:entity/:id/approvals sits behind the same RequireACL gate as /:entity/:id GET so only
// users with view access to the entity may read its approval history.
func (h *ApprovalHandler) RegisterEntityRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity/:id/approvals", h.ListForRecord)
}

// RegisterSubmitRoute mounts POST /:entity/:id/approvals/submit on a group carrying the record's
// "edit" ACL stage.
//
// Edit, not view: asking for sign-off is an act on the record's lifecycle, and the person who may
// change a quote is the person who may send it. Mounting it on the read gate would let a
// read-only viewer start a workflow on someone else's work.
func (h *ApprovalHandler) RegisterSubmitRoute(rg *gin.RouterGroup) {
	rg.POST("/:entity/:id/approvals/submit", h.Submit)
	// The list of processes the send button may offer: same floor and same row probe as the send.
	rg.GET("/:entity/:id/approvals/startable", h.Startable)
}

// ---- request bodies -------------------------------------------------------------------------

type approvalCriteriaRuleRequest struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

type approvalCriteriaRequest struct {
	Match string                        `json:"match,omitempty"`
	Rules []approvalCriteriaRuleRequest `json:"rules,omitempty"`
}

// approvalApproverRequest is one approver of a parallel step (specs/043 S4).
type approvalApproverRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// approvalCompletionRequest is the policy of a parallel step: mode "all" | "any" | "quorum"
// (percent 1..100 with quorum only).
type approvalCompletionRequest struct {
	Mode    string `json:"mode"`
	Percent int    `json:"percent,omitempty"`
}

// approvalOnRejectRequest is a step's answer to a rejection (specs/043 S8): to = "@end" |
// "@submitter" | an earlier step's id; resumeAt = "rejector" | "start" (only with "@submitter").
type approvalOnRejectRequest struct {
	To       string `json:"to"`
	ResumeAt string `json:"resumeAt,omitempty"`
}

func toServiceOnReject(in *approvalOnRejectRequest) *services.ApprovalOnRejectInput {
	if in == nil {
		return nil
	}
	return &services.ApprovalOnRejectInput{To: in.To, ResumeAt: in.ResumeAt}
}

type approvalStepRequest struct {
	ID           string `json:"id,omitempty"` // stable step id (specs/043 S0); optional
	Name         string `json:"name,omitempty"`
	ApproverType string `json:"approverType"`
	ApproverID   string `json:"approverId"`
	SLASeconds   int    `json:"slaSeconds,omitempty"`
	SLAClock     string `json:"slaClock,omitempty"` // "business" | "wall" (specs/043 S1); "" = business
	// Approvers + Completion: a step held by several people (S4) instead of approverType/approverId.
	Approvers  []approvalApproverRequest  `json:"approvers,omitempty"`
	Completion *approvalCompletionRequest `json:"completion,omitempty"`
	Assignment *approvalAssignmentRequest `json:"assignment,omitempty"` // a rule instead of a name (S6a)
	// OnReject: where a rejection of this step goes (S8).
	OnReject    *approvalOnRejectRequest `json:"onReject,omitempty"`
	unsupported []string
}

// approvalAssignmentRequest is the rule form of a step's holder (specs/043 S6a); like the step it
// records the keys it was sent outside services-side bpm.AssignmentKeys.
type approvalAssignmentRequest struct {
	Type        string `json:"type"`
	ID          string `json:"id,omitempty"`
	Field       string `json:"field,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	unsupported []string
}

// UnmarshalJSON binds the assignment and records any key it does not know (refused by the validator).
func (a *approvalAssignmentRequest) UnmarshalJSON(b []byte) error {
	type plain approvalAssignmentRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*a = approvalAssignmentRequest(p)
	a.unsupported = services.UnsupportedApprovalKeys(b, bpm.AssignmentKeys...)
	return nil
}

func toServiceAssignment(in *approvalAssignmentRequest) *services.ApprovalAssignmentInput {
	if in == nil {
		return nil
	}
	return &services.ApprovalAssignmentInput{Type: in.Type, ID: in.ID, Field: in.Field, Strategy: in.Strategy, Unsupported: in.unsupported}
}

// approvalOptionsRequest is the process-level settings block (specs/043 S6a).
type approvalOptionsRequest struct {
	FallbackAssignee *approvalApproverRequest `json:"fallbackAssignee,omitempty"`
	Lane             string                   `json:"lane,omitempty"`
	CalendarID       string                   `json:"calendarId,omitempty"`
	PauseWhen        *workflow.Condition      `json:"pauseWhen,omitempty"`
	unsupported      []string
}

// UnmarshalJSON binds the options and records any key outside bpm.OptionsKeys.
func (o *approvalOptionsRequest) UnmarshalJSON(b []byte) error {
	type plain approvalOptionsRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*o = approvalOptionsRequest(p)
	o.unsupported = services.UnsupportedApprovalKeys(b, bpm.OptionsKeys...)
	return nil
}

func toServiceOptions(in *approvalOptionsRequest) *services.ApprovalOptionsInput {
	if in == nil {
		return nil
	}
	out := &services.ApprovalOptionsInput{Unsupported: in.unsupported, Lane: in.Lane, CalendarID: in.CalendarID, PauseWhen: in.PauseWhen}
	if in.FallbackAssignee != nil {
		out.FallbackAssignee = &services.ApprovalApproverInput{Type: in.FallbackAssignee.Type, ID: in.FallbackAssignee.ID}
	}
	return out
}

// UnmarshalJSON binds the v1 step keys AND records every other key the client sent. gin's typed
// bind drops unknown keys silently, so without this a step configured for a capability the
// engine does not run yet (parallel approvers, routing, slaClock before S1…) would save as a
// plain step and the admin would never know — the validator refuses it instead (D-7).
func (s *approvalStepRequest) UnmarshalJSON(b []byte) error {
	type plain approvalStepRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = approvalStepRequest(p)
	s.unsupported = services.UnsupportedApprovalKeys(b, services.ApprovalStepKeys...)
	return nil
}

// UnmarshalJSON records process-level keys outside services.ApprovalProcessKeys (options…) — see
// approvalStepRequest.UnmarshalJSON.
func (r *approvalProcessRequest) UnmarshalJSON(b []byte) error {
	type plain approvalProcessRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = approvalProcessRequest(p)
	r.unsupported = services.UnsupportedApprovalKeys(b, services.ApprovalProcessKeys...)
	return nil
}

// approvalGraphRequest is the graph form of a process chain (specs/043 S3a2, D-2).
type approvalGraphRequest struct {
	Nodes       []approvalNodeRequest `json:"nodes"`
	Start       string                `json:"start,omitempty"` // the node the walk begins on (S7); "" = the first
	unsupported []string
}

// UnmarshalJSON records graph-level keys outside services.ApprovalGraphKeys (start, next…) so the
// validator refuses them (D-7) instead of the typed bind dropping them.
func (g *approvalGraphRequest) UnmarshalJSON(b []byte) error {
	type plain approvalGraphRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*g = approvalGraphRequest(p)
	g.unsupported = services.UnsupportedApprovalKeys(b, services.ApprovalGraphKeys...)
	return nil
}

// approvalNodeRequest is one graph node: the step keys plus its kind. Its own fields rather than
// an embedded approvalStepRequest — that type's UnmarshalJSON would be promoted and swallow kind.
type approvalNodeRequest struct {
	ID           string                     `json:"id,omitempty"`
	Name         string                     `json:"name,omitempty"`
	Kind         string                     `json:"kind"`
	ApproverType string                     `json:"approverType"`
	ApproverID   string                     `json:"approverId"`
	SLASeconds   int                        `json:"slaSeconds,omitempty"`
	SLAClock     string                     `json:"slaClock,omitempty"`
	Approvers    []approvalApproverRequest  `json:"approvers,omitempty"`
	Completion   *approvalCompletionRequest `json:"completion,omitempty"`
	Assignment   *approvalAssignmentRequest `json:"assignment,omitempty"`
	// RequiredFields: the record fields a task node needs filled before it completes (specs/043 S5).
	RequiredFields []string `json:"requiredFields,omitempty"`
	// OnReject: where a rejection of this node goes (S8).
	OnReject *approvalOnRejectRequest `json:"onReject,omitempty"`
	// Routing (specs/043 S7): the node after this one; a gateway's branches + default; an auto
	// node's field writes; an end node's result.
	Next     string                  `json:"next,omitempty"`
	Branches []approvalBranchRequest `json:"branches,omitempty"`
	Default  string                  `json:"default,omitempty"`
	Set      []approvalSetRequest    `json:"set,omitempty"`
	Result   string                  `json:"result,omitempty"`
	// Until + SLA: a wait node's condition and deadline (S12a).
	Until       *approvalCriteriaRequest `json:"until,omitempty"`
	SLA         *approvalWaitSLARequest  `json:"sla,omitempty"`
	unsupported []string
}

// approvalSLARowRequest is one row of a wait node's priority matrix.
type approvalSLARowRequest struct {
	When   approvalCriteriaRequest `json:"when"`
	Amount int                     `json:"amount"`
	Unit   string                  `json:"unit"`
}

// approvalWaitSLARequest is a wait node's `sla` object; like the node it records the keys it was
// sent outside bpm.WaitSLAKeys so the validator refuses them instead of the bind dropping them.
type approvalWaitSLARequest struct {
	Amount      int                        `json:"amount,omitempty"`
	Unit        string                     `json:"unit,omitempty"`
	Matrix      []approvalSLARowRequest    `json:"matrix,omitempty"`
	WriteDueTo  string                     `json:"writeDueTo,omitempty"`
	OnChange    string                     `json:"onChange,omitempty"`
	Notify      *approvalAssignmentRequest `json:"notify,omitempty"`
	unsupported []string
}

// UnmarshalJSON binds the sla object and records any key outside services.ApprovalWaitSLAKeys.
func (w *approvalWaitSLARequest) UnmarshalJSON(b []byte) error {
	type plain approvalWaitSLARequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*w = approvalWaitSLARequest(p)
	w.unsupported = services.UnsupportedApprovalKeys(b, services.ApprovalWaitSLAKeys...)
	return nil
}

func toServiceCondition(c *approvalCriteriaRequest) *services.ApprovalConditionInput {
	if c == nil {
		return nil
	}
	match, rules := toServiceCriteria(c)
	return &services.ApprovalConditionInput{Match: match, Rules: rules}
}

func toServiceWaitSLA(in *approvalWaitSLARequest) *services.ApprovalWaitSLAInput {
	if in == nil {
		return nil
	}
	out := &services.ApprovalWaitSLAInput{Amount: in.Amount, Unit: in.Unit, WriteDueTo: in.WriteDueTo, OnChange: in.OnChange,
		Notify: toServiceAssignment(in.Notify), Unsupported: in.unsupported}
	for _, r := range in.Matrix {
		when := toServiceCondition(&r.When)
		out.Matrix = append(out.Matrix, services.ApprovalSLARowInput{When: *when, Amount: r.Amount, Unit: r.Unit})
	}
	return out
}

// approvalBranchRequest is one arm of a gateway: `when` is the entry-criteria vocabulary
// ({match, rules:[{field, op, value}]}) judged on the record's current values.
type approvalBranchRequest struct {
	Name string                  `json:"name"`
	When approvalCriteriaRequest `json:"when"`
	To   string                  `json:"to"`
}

// approvalSetRequest is one field write of an auto node.
type approvalSetRequest struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}

// UnmarshalJSON records node keys outside services.ApprovalNodeKeys (approvers, assignment,
// onReject…) for the validator to refuse at graph.nodes[i].<key>.
func (n *approvalNodeRequest) UnmarshalJSON(b []byte) error {
	type plain approvalNodeRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*n = approvalNodeRequest(p)
	n.unsupported = services.UnsupportedApprovalKeys(b, services.ApprovalNodeKeys...)
	return nil
}

// toServiceGraph maps the request graph to the service input (nil stays nil: the linear form).
func toServiceGraph(g *approvalGraphRequest) *services.ApprovalGraphInput {
	if g == nil {
		return nil
	}
	out := &services.ApprovalGraphInput{Nodes: make([]services.ApprovalNodeInput, len(g.Nodes)), Start: g.Start, Unsupported: g.unsupported}
	for i, n := range g.Nodes {
		out.Nodes[i] = services.ApprovalNodeInput{Kind: n.Kind, RequiredFields: n.RequiredFields,
			Next: n.Next, Default: n.Default, Result: n.Result,
			Until: toServiceCondition(n.Until), SLA: toServiceWaitSLA(n.SLA),
			Branches: toServiceBranches(n.Branches), Set: toServiceSet(n.Set),
			ApprovalStepInput: services.ApprovalStepInput{
				ID: n.ID, Name: n.Name, ApproverType: n.ApproverType, ApproverID: n.ApproverID,
				SLASeconds: n.SLASeconds, SLAClock: n.SLAClock, Unsupported: n.unsupported,
				Approvers: toServiceApprovers(n.Approvers), Completion: toServiceCompletion(n.Completion),
				OnReject:   toServiceOnReject(n.OnReject),
				Assignment: toServiceAssignment(n.Assignment),
			}}
	}
	return out
}

// approvalProcessRequest is the POST/PUT body for an approval process.
type approvalProcessRequest struct {
	Name       string                   `json:"name"`
	EntityName string                   `json:"entityName"`
	Trigger    string                   `json:"trigger"`
	Criteria   *approvalCriteriaRequest `json:"criteria,omitempty"`
	Steps      []approvalStepRequest    `json:"steps"`
	// Graph is the chain in graph form (specs/043 S3a2) — sent INSTEAD of steps.
	Graph *approvalGraphRequest `json:"graph,omitempty"`
	// Outcome (APPROVAL-OUTCOME): what a terminal decision WRITES on the record. Absent = the
	// decision changes nothing, which is how every process behaved before this existed.
	Outcome *approvalOutcomeRequest `json:"outcome,omitempty"`
	// Options: process-level settings (specs/043 S6a) — the fallback assignee of a step nobody holds.
	Options     *approvalOptionsRequest `json:"options,omitempty"`
	IsActive    *bool                   `json:"isActive,omitempty"`
	unsupported []string
}

// approvalOutcomeRequest is the write-back block of an approval process.
type approvalOutcomeRequest struct {
	StatusField   string `json:"statusField"`
	ApprovedValue string `json:"approvedValue,omitempty"`
	RejectedValue string `json:"rejectedValue,omitempty"`
	// ReturnedValue: written when a rejection returns the record to its submitter (S8).
	ReturnedValue string `json:"returnedValue,omitempty"`
}

// toServiceOutcome maps the request block to the service input (nil stays nil).
func toServiceOutcome(in *approvalOutcomeRequest) *services.ApprovalOutcomeInput {
	if in == nil {
		return nil
	}
	return &services.ApprovalOutcomeInput{
		StatusField:   in.StatusField,
		ApprovedValue: in.ApprovedValue,
		RejectedValue: in.RejectedValue,
		ReturnedValue: in.ReturnedValue,
	}
}

// approvalDecisionRequest is the POST body for approve/reject (an optional approver note). The FE
// sends the key "note" (lib/admin.ts approveApproval/rejectApproval); "comment" is accepted as a
// legacy alias so an older client keeps working. Whichever is non-empty is threaded into the
// HistoryEntry.Comment the timeline renders (the read side already normalizes note|comment →
// comment). Both keys are camelCase single words.
type approvalDecisionRequest struct {
	Note    string `json:"note,omitempty"`
	Comment string `json:"comment,omitempty"`
	// ExpectedStep is the step index the screen rendered (approval.currentStep). Optional: when
	// sent and the instance has since moved, the decision is refused with 409 APPROVAL_CONFLICT.
	ExpectedStep *int `json:"expectedStep,omitempty"`
}

// decisionComment resolves the approver note from the request body: the FE-native "note" wins when
// present; otherwise the legacy "comment" alias. Returns "" when neither is set (an optional note).
func decisionComment(req approvalDecisionRequest) string {
	if strings.TrimSpace(req.Note) != "" {
		return req.Note
	}
	return req.Comment
}

// ---- mapping --------------------------------------------------------------------------------

func toServiceCriteria(c *approvalCriteriaRequest) (string, []services.ApprovalCriteriaRuleInput) {
	if c == nil {
		return "", nil
	}
	rules := make([]services.ApprovalCriteriaRuleInput, len(c.Rules))
	for i, r := range c.Rules {
		rules[i] = services.ApprovalCriteriaRuleInput{Field: r.Field, Op: r.Op, Value: r.Value}
	}
	return c.Match, rules
}

func toServiceBranches(in []approvalBranchRequest) []services.ApprovalBranchInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]services.ApprovalBranchInput, len(in))
	for i, b := range in {
		match, rules := toServiceCriteria(&b.When)
		out[i] = services.ApprovalBranchInput{Name: b.Name, Match: match, Rules: rules, To: b.To}
	}
	return out
}

func toServiceSet(in []approvalSetRequest) []services.ApprovalSetInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]services.ApprovalSetInput, len(in))
	for i, w := range in {
		out[i] = services.ApprovalSetInput{Field: w.Field, Value: w.Value}
	}
	return out
}

func toServiceApprovers(in []approvalApproverRequest) []services.ApprovalApproverInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]services.ApprovalApproverInput, len(in))
	for i, a := range in {
		out[i] = services.ApprovalApproverInput{Type: a.Type, ID: a.ID}
	}
	return out
}

func toServiceCompletion(in *approvalCompletionRequest) *services.ApprovalCompletionInput {
	if in == nil {
		return nil
	}
	return &services.ApprovalCompletionInput{Mode: in.Mode, Percent: in.Percent}
}

func toServiceSteps(in []approvalStepRequest) []services.ApprovalStepInput {
	out := make([]services.ApprovalStepInput, len(in))
	for i, s := range in {
		out[i] = services.ApprovalStepInput{
			ID:           s.ID,
			Name:         s.Name,
			ApproverType: s.ApproverType,
			ApproverID:   s.ApproverID,
			SLASeconds:   s.SLASeconds,
			SLAClock:     s.SLAClock,
			Approvers:    toServiceApprovers(s.Approvers),
			Completion:   toServiceCompletion(s.Completion),
			OnReject:     toServiceOnReject(s.OnReject),
			Assignment:   toServiceAssignment(s.Assignment),
			Unsupported:  s.unsupported,
		}
	}
	return out
}

// ---- per-record approval read ---------------------------------------------------------------

// ListForRecord handles GET /:entity/:id/approvals: returns all approval instances for the
// (entity, record) pair in the caller's tenant, all statuses, newest first. Returns an empty
// instances array (200) when no approval process has ever been attached to this record.
// The ACL gate (RequireACL → view on :entity) is applied by the recordGroup middleware chain.
func (h *ApprovalHandler) ListForRecord(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	entityName := strings.TrimSpace(c.Param("entity"))
	recordID := strings.TrimSpace(c.Param("id"))

	// ?lane=independent (S12b) reads the SLA tracking lane; absent / "exclusive" = the approvals, as ever.
	var (
		rows any
		err  error
	)
	switch lane := strings.TrimSpace(c.Query("lane")); lane {
	case "", "exclusive":
		rows, err = h.svc.GetInstancesForRecord(c.Request.Context(), scopeOf(c), tenantID, entityName, recordID)
	case "independent":
		rows, err = h.svc.GetRecordSLA(c.Request.Context(), scopeOf(c), tenantID, entityName, recordID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "lane không hợp lệ (exclusive|independent)", "code": "APPROVAL_LANE_INVALID"})
		return
	}
	if err != nil {
		// The record-read refusal (404 for a row the caller may not see) must land as 404, exactly
		// like GET /:entity/:id — respondServiceError maps the ValidationError codes; the record
		// sentinels are mapped here for the same not-an-oracle reason the audit handler does.
		if errors.Is(err, services.ErrRecordNotFound) || errors.Is(err, services.ErrEntityNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
			return
		}
		h.respondServiceError(c, err, "ListForRecord", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"instances": rows}})
}

// ---- admin process CRUD handlers ------------------------------------------------------------

// ListProcesses handles GET /admin/approval-processes.
func (h *ApprovalHandler) ListProcesses(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListProcesses(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.Approval.ListProcesses", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê quy trình phê duyệt", "code": "APPROVAL_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"processes": rows}})
}

// GetProcess handles GET /admin/approval-processes/:id.
func (h *ApprovalHandler) GetProcess(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rec, err := h.svc.GetProcess(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "GetProcess", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"process": rec}})
}

// CreateProcess handles POST /admin/approval-processes.
func (h *ApprovalHandler) CreateProcess(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalProcessRequest
	if !bindJSON(c, &req) {
		return
	}
	match, rules := toServiceCriteria(req.Criteria)
	rec, err := h.svc.CreateProcess(c.Request.Context(), tenantID, services.CreateApprovalProcessInput{
		Name:        req.Name,
		EntityName:  req.EntityName,
		Trigger:     req.Trigger,
		Match:       match,
		Rules:       rules,
		Steps:       toServiceSteps(req.Steps),
		Graph:       toServiceGraph(req.Graph),
		Outcome:     toServiceOutcome(req.Outcome),
		Options:     toServiceOptions(req.Options),
		IsActive:    req.IsActive,
		ActorID:     actorOf(c),
		Unsupported: req.unsupported,
	})
	if err != nil {
		h.respondServiceError(c, err, "CreateProcess", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"process": rec, "warnings": h.saveWarnings(c, tenantID, rec.EntityName, req.Graph)}})
}

// saveWarnings are the things a saved process does that its author should know and that are not
// errors (specs/043 S12a: a wait node's deadline field also written by an automation rule). Fail-open:
// a rule list that cannot be read costs the warning, never the save that already committed.
func (h *ApprovalHandler) saveWarnings(c *gin.Context, tenantID, entity string, g *approvalGraphRequest) []string {
	warnings, err := h.svc.DueFieldConflicts(c.Request.Context(), tenantID, entity, toServiceGraph(g))
	if err != nil {
		h.logger.Warn("approval save warnings unavailable", zap.String("tenant_id", tenantID), zap.String("entity_name", entity), zap.Error(err))
		return []string{}
	}
	if warnings == nil {
		return []string{}
	}
	return warnings
}

// updateInputOf maps a full-replace process body to the service input — shared by the legacy save
// (PUT /:id) and the draft save (PUT /:id/draft), which take the SAME body.
func updateInputOf(c *gin.Context, req approvalProcessRequest) services.UpdateApprovalProcessInput {
	match, rules := toServiceCriteria(req.Criteria)
	return services.UpdateApprovalProcessInput{
		Name:       req.Name,
		EntityName: req.EntityName,
		Trigger:    req.Trigger,
		Match:      match,
		Rules:      rules,
		Steps:      toServiceSteps(req.Steps),
		Graph:      toServiceGraph(req.Graph),
		Outcome:    toServiceOutcome(req.Outcome),
		Options:    toServiceOptions(req.Options),
		IsActive:   req.IsActive,
		ActorID:    actorOf(c),
		// ?force=true: a linear-only body may replace a stored graph (else 409
		// APPROVAL_PROCESS_V2_OVERWRITE — specs/043 FR-004).
		Force:       c.Query("force") == "true",
		Unsupported: req.unsupported,
	}
}

// UpdateProcess handles PUT /admin/approval-processes/:id — the legacy "save = publish now".
func (h *ApprovalHandler) UpdateProcess(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalProcessRequest
	if !bindJSON(c, &req) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rec, err := h.svc.UpdateProcess(c.Request.Context(), tenantID, id, updateInputOf(c, req))
	if err != nil {
		h.respondServiceError(c, err, "UpdateProcess", tenantID)
		return
	}
	// impact: how many hồ sơ still run an older version (specs/043 S11) — beside process, so a
	// client reading data.process is untouched.
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"process": rec, "impact": h.svc.SaveImpact(c.Request.Context(), tenantID, id),
		"warnings": h.saveWarnings(c, tenantID, rec.EntityName, req.Graph)}})
}

// SaveDraft handles PUT /admin/approval-processes/:id/draft — an unpublished edit; no hồ sơ starts
// from it (specs/043 S10).
func (h *ApprovalHandler) SaveDraft(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalProcessRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.SaveDraft(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), updateInputOf(c, req))
	if err != nil {
		h.respondServiceError(c, err, "SaveDraft", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"process": rec}})
}

// DiscardDraft handles DELETE /admin/approval-processes/:id/draft.
func (h *ApprovalHandler) DiscardDraft(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.DiscardDraft(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id"))); err != nil {
		h.respondServiceError(c, err, "DiscardDraft", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// approvalPublishRequest is the optional body of POST .../publish.
type approvalPublishRequest struct {
	Note string `json:"note"`
}

// PublishDraft handles POST /admin/approval-processes/:id/publish {note}.
func (h *ApprovalHandler) PublishDraft(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalPublishRequest
	if !optionalBody(c, &req) {
		return
	}
	res, err := h.svc.PublishDraft(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), req.Note, actorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "PublishDraft", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// ListVersions handles GET /admin/approval-processes/:id/versions (newest first, no definitions).
func (h *ApprovalHandler) ListVersions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListVersions(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "ListVersions", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"versions": rows}})
}

// GetVersion handles GET /admin/approval-processes/:id/versions/:version.
func (h *ApprovalHandler) GetVersion(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	version, err := strconv.Atoi(strings.TrimSpace(c.Param("version")))
	if err != nil || version < 1 {
		// Not a version number at all is the same answer as one that does not exist.
		c.JSON(http.StatusNotFound, gin.H{"error": "không tìm thấy phiên bản này của quy trình", "code": "APPROVAL_VERSION_NOT_FOUND"})
		return
	}
	rec, err := h.svc.GetVersion(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), version)
	if err != nil {
		h.respondServiceError(c, err, "GetVersion", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"version": rec}})
}

// ProcessImpact handles GET /admin/approval-processes/:id/impact.
func (h *ApprovalHandler) ProcessImpact(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	im, err := h.svc.ProcessImpact(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "ProcessImpact", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": im})
}

// ListProcessInstances handles GET /admin/approval-processes/:id/instances?version=&status=&stuck=&limit=.
// stuck=true keeps only the hồ sơ stalled on an automatic step with an error (specs/043 S7).
func (h *ApprovalHandler) ListProcessInstances(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var version *int
	if raw := strings.TrimSpace(c.Query("version")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version phải là số nguyên không âm", "code": "APPROVAL_VERSION_INVALID"})
			return
		}
		version = &v
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit"))) // bad/absent → 0 → the repository default
	stuck := false
	switch strings.ToLower(strings.TrimSpace(c.Query("stuck"))) {
	case "", "false", "0":
	case "true", "1":
		stuck = true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "stuck phải là true hoặc false", "code": "APPROVAL_STUCK_FILTER_INVALID"})
		return
	}
	rows, err := h.svc.ListProcessInstances(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), version, strings.TrimSpace(c.Query("status")), stuck, limit)
	if err != nil {
		h.respondServiceError(c, err, "ListProcessInstances", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"instances": rows}})
}

// approvalRepinRequest is the body of POST .../repin. DryRun omitted means TRUE: moving running
// hồ sơ is applied only by an explicit "dryRun": false.
type approvalRepinRequest struct {
	InstanceIDs []string `json:"instanceIds"`
	ToVersion   int      `json:"toVersion"`
	DryRun      *bool    `json:"dryRun"`
	After       string   `json:"after"`
}

// RepinInstances handles POST /admin/approval-processes/:id/repin.
func (h *ApprovalHandler) RepinInstances(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalRepinRequest
	if !optionalBody(c, &req) {
		return
	}
	out, err := h.svc.RepinInstances(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), bpm.RepinInput{
		InstanceIDs: req.InstanceIDs, ToVersion: req.ToVersion, DryRun: req.DryRun == nil || *req.DryRun, By: actorOf(c), After: strings.TrimSpace(req.After),
	})
	if err != nil {
		h.respondServiceError(c, err, "RepinInstances", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// DeleteProcess handles DELETE /admin/approval-processes/:id.
func (h *ApprovalHandler) DeleteProcess(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteProcess(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id"))); err != nil {
		h.respondServiceError(c, err, "DeleteProcess", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- user approval handlers -----------------------------------------------------------------

// approvalInstanceView is the camelCase body of every approval MUTATION response
// (submit · approve/reject · recall · delegate) under data.approval — DAILY-QUEUE #443. These used
// to serialize bpm.Instance verbatim (PascalCase `ID`, `TenantID`…), against api-conventions.
// Only the two READ lists (`GET /approvals/pending`, `GET /:entity/:id/approvals`) keep the legacy
// PascalCase shape, because the FE parses it; `GET /approvals/:id` has its own camelCase detail.
// Same key set as ApprovalDetail minus the process/duration context a mutation does not compute.
type approvalInstanceView struct {
	ID                  string             `json:"id"`
	TenantID            string             `json:"tenantId"`
	ProcessID           string             `json:"processId"`
	EntityName          string             `json:"entityName"`
	RecordID            string             `json:"recordId"`
	CurrentStep         int                `json:"currentStep"`
	Status              bpm.InstanceStatus `json:"status"`
	CurrentApproverType bpm.ApproverType   `json:"currentApproverType,omitempty"`
	CurrentApproverID   string             `json:"currentApproverId,omitempty"`
	SLADueAt            *time.Time         `json:"slaDueAt"`
	Escalated           bool               `json:"escalated"`
	History             []bpm.HistoryEntry `json:"history"`
	ProcessVersion      int                `json:"processVersion,omitempty"`
	// Stuck: it stands still on an automatic step and an admin must act (specs/043 S7). The reason
	// (lastError) is engine vocabulary: it travels on the admin reads only (detail for an admin, the
	// per-process list), never on this body.
	Stuck     bool      `json:"stuck,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// toApprovalInstanceView projects an instance to its camelCase wire shape; history is always an
// array (never null), like ApprovalDetail. A nil instance stays nil (JSON null), not a panic.
func toApprovalInstanceView(in *bpm.Instance) *approvalInstanceView {
	return viewOfInstance(in, false)
}

// toAdminApprovalInstanceView is toApprovalInstanceView for the admin verbs (retry, cancel,
// override): the history keeps the machine's own lines (gateway, auto, effect_failed, retried) that
// the customer-facing view leaves out.
func toAdminApprovalInstanceView(in *bpm.Instance) *approvalInstanceView {
	return viewOfInstance(in, true)
}

func viewOfInstance(in *bpm.Instance, admin bool) *approvalInstanceView {
	if in == nil {
		return nil
	}
	hist := in.History
	if !admin {
		hist = bpm.PublicHistory(hist)
	}
	if hist == nil {
		hist = []bpm.HistoryEntry{}
	}
	return &approvalInstanceView{
		ID: in.ID, TenantID: in.TenantID, ProcessID: in.ProcessID, EntityName: in.EntityName,
		RecordID: in.RecordID, CurrentStep: in.CurrentStep, Status: in.Status,
		CurrentApproverType: in.CurrentApproverType, CurrentApproverID: in.CurrentApproverID,
		SLADueAt: in.SLADueAt, Escalated: in.Escalated, History: hist, ProcessVersion: in.ProcessVersion,
		Stuck:     in.Stuck(),
		CreatedBy: in.CreatedBy, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	}
}

// pendingApprovalRow decorates one bpm.Instance with best-effort display labels (F7-4 —
// "crm_opportunities / 550e8400" reads blind; "Cơ hội: Hợp đồng ABC" doesn't). The embedded
// Instance keeps the legacy PascalCase serialization the FE already parses (documented
// deviation, lib/api.ts ApprovalItem); ONLY the two new keys are camelCase per the API
// convention for new fields. Empty label = resolution unavailable — the FE falls back to the
// mono slug+id, never a fake name.
type pendingApprovalRow struct {
	bpm.Instance
	EntityLabel string `json:"entityLabel"`
	RecordLabel string `json:"recordLabel"`
	// SLABasis says what SLADueAt was computed on (specs/043 S1) — working hours, or why it is
	// wall clock — so the inbox never shows a fallback deadline as if it were working hours.
	SLABasis bpm.SLABasis `json:"slaBasis,omitempty"`
	// AwaitsMe is true on every row: ListPendingForUser returns exactly the approvals the caller
	// holds an open seat of (bpm.AwaitsActor). Sent so the screen does not re-derive "is it mine"
	// from current_approver_*, which is empty on a step with several seats (specs/043 S4).
	AwaitsMe bool `json:"awaitsMe"`
	// AwaitingRework: the row is a record returned to its submitter (specs/043 S8) — the only verb is
	// "Gửi lại", so the inbox offers no Duyệt / Từ chối (the engine answers STEP_KIND_MISMATCH).
	AwaitingRework bool `json:"awaitingRework,omitempty"`
}

// approvalLabelPreference mirrors the FE display-name engine's tier-1 list (lib/recordLabel.ts
// PREFERRED_NAME_FIELDS) so BE-chosen labels agree with what the record's own detail page
// shows. It only PICKS the labelField; the fetch itself is the same BatchFetchLabels path the
// ?expand= engine uses.
var approvalLabelPreference = []string{
	"name", "title", "label", "full_name", "display_name", "subject", "code", "email",
}

// pickApprovalLabelField returns the entity's display field: the shared resolver first (#272/#479 —
// the same field ?expand and record titles use), then the legacy preference list for tables whose
// name-like column is not a string type; "" when none (→ no record label, honest fallback).
func pickApprovalLabelField(entityOptions *string, fields []metadata.FieldDefinition) string {
	if lf, _ := metadata.ResolveLabelField(entityOptions, fields); lf != "" {
		return lf
	}
	byLower := make(map[string]string, len(fields))
	for _, f := range fields {
		byLower[strings.ToLower(f.Name)] = f.Name
	}
	for _, p := range approvalLabelPreference {
		if n, ok := byLower[p]; ok {
			return n
		}
	}
	return ""
}

// humanizePending wraps the pending instances with {entityLabel, recordLabel}, best-effort:
// ONE metadata read + ONE batched label fetch per distinct entity (no N+1 — the same batch-IN
// discipline as applyExpand), and ANY resolution failure degrades to empty labels instead of
// failing the request (the queue must render even when a label source hiccups).
func (h *ApprovalHandler) humanizePending(ctx context.Context, tenantID, roleID string, isAdmin bool, rows []bpm.Instance) []pendingApprovalRow {
	out := make([]pendingApprovalRow, len(rows))
	for i, r := range rows {
		out[i] = pendingApprovalRow{Instance: r, SLABasis: r.CurrentSLABasis(), AwaitsMe: true, AwaitingRework: r.InRework}
	}
	if h.meta == nil || h.labels == nil || len(rows) == 0 {
		return out
	}

	refs := make([]recordRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, recordRef{Entity: r.EntityName, RecordID: r.RecordID})
	}
	entityLabels, recordLabels := h.labeler().resolve(ctx, tenantID, roleID, isAdmin, refs)

	for i := range out {
		out[i].EntityLabel = entityLabels[out[i].EntityName]
		if m := recordLabels[out[i].EntityName]; m != nil {
			out[i].RecordLabel = m[out[i].RecordID]
		}
	}
	return out
}

// recordRef names one record whose display label a screen wants.
type recordRef struct{ Entity, RecordID string }

// recordLabeler resolves {entityLabel, recordLabel} for a set of records — the one body behind the
// approvals inbox (F7-4) and the work-item inbox (specs/043 S5): ONE metadata read and ONE batched
// label fetch per distinct entity, the field-ACL hidden set honoured, and ANY failure degrading to
// an empty label (never to a failed request).
type recordLabeler struct {
	meta   approvalMetaReader
	labels services.LabelFetcher
	hidden fieldViewChecker
	logger *zap.Logger
}

// labeler is the approvals handler's own deps as a recordLabeler.
func (h *ApprovalHandler) labeler() recordLabeler {
	return recordLabeler{meta: h.meta, labels: h.labels, hidden: h.hidden, logger: h.logger}
}

// resolve returns the entity label per entity name and the record label per (entity, record id).
// Missing deps or a record with no resolvable label simply leave it out.
func (l recordLabeler) resolve(ctx context.Context, tenantID, roleID string, isAdmin bool, refs []recordRef) (entityLabels map[string]string, recordLabels map[string]map[string]string) {
	idsByEntity := make(map[string][]string)
	entityLabels = map[string]string{}
	recordLabels = map[string]map[string]string{}
	if l.meta == nil || l.labels == nil || len(refs) == 0 {
		return entityLabels, recordLabels
	}
	for _, r := range refs {
		if r.Entity != "" && r.RecordID != "" {
			idsByEntity[r.Entity] = append(idsByEntity[r.Entity], r.RecordID)
		}
	}

	for ent, ids := range idsByEntity {
		def, err := l.meta.GetEntityByName(ctx, tenantID, ent)
		if err != nil || def == nil {
			continue // best-effort: labels stay empty for this entity
		}
		entityLabels[ent] = def.Label
		fields, err := l.meta.ListFields(ctx, tenantID, def.ID)
		if err != nil {
			continue
		}
		lf := pickApprovalLabelField(def.Options, fields)
		if lf == "" {
			continue // entity has no name-like field — id fallback is the honest answer
		}
		// Field ACL applies to a value no matter which surface reads it. This decoration fetches a
		// field VALUE off another entity's rows, and the preference list includes `email`, so a
		// role with that field hidden still saw it here — the one read in the product that skipped
		// the check every record read performs. Skipping the label leaves it empty, which is the
		// path this loop already takes for every other reason a label cannot be resolved.
		if l.hidden != nil {
			if hid, herr := l.hidden.HiddenFields(ctx, tenantID, roleID, ent, isAdmin); herr == nil {
				if _, isHidden := hid[lf]; isHidden {
					continue
				}
			}
		}
		res, err := l.labels.BatchFetchLabels(ctx, ent, tenantID, lf, ids)
		if err != nil {
			l.logger.Debug("handlers.Approval.humanizePending: label fetch failed (rows keep empty labels)",
				zap.String("tenant_id", tenantID), zap.String("entity", ent), zap.Error(err))
			continue
		}
		m := make(map[string]string, len(res))
		for _, lr := range res {
			m[lr.ID] = lr.Label
		}
		recordLabels[ent] = m
	}

	return entityLabels, recordLabels
}

// ListPending handles GET /approvals/pending: the instances awaiting THIS user's decision,
// each decorated with best-effort {entityLabel, recordLabel} (F7-4) so the inbox can show
// "Cơ hội: Hợp đồng ABC" instead of a machine slug + UUID fragment.
func (h *ApprovalHandler) ListPending(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	rows, err := h.svc.ListPending(c.Request.Context(), tenantID, approvalActorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "ListPending", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"approvals": h.humanizePending(c.Request.Context(), tenantID, sub.RoleID, sub.IsAdmin, rows),
	}})
}

// GetDetail handles GET /approvals/:id (W6): one approval instance + its process/step context +
// per-step history timeline, for the inbox deep-link. Visibility mirrors /approvals/pending:
// admin OR the current step's approver; everyone else gets the same 404 as an unknown id (no
// existence leak, no cross-tenant read — the service pins tenant_id).
func (h *ApprovalHandler) GetDetail(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	actor := bpm.Actor{
		UserID:  sub.UserID,
		RoleIDs: roleIDsOf(sub.RoleID),
		IsAdmin: sub.IsAdmin,
	}
	detail, err := h.svc.GetApprovalDetail(c.Request.Context(), scopeOf(c), tenantID, strings.TrimSpace(c.Param("id")), actor)
	if err != nil {
		h.respondServiceError(c, err, "GetDetail", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": detail}})
}

// Approve handles POST /approvals/:id/approve.
func (h *ApprovalHandler) Approve(c *gin.Context) {
	h.decide(c, bpm.DecisionApprove)
}

// Reject handles POST /approvals/:id/reject.
func (h *ApprovalHandler) Reject(c *gin.Context) {
	h.decide(c, bpm.DecisionReject)
}

// decide is the shared approve/reject path: it binds the optional comment, resolves the actor from
// the verified subject, and calls the service.
func (h *ApprovalHandler) decide(c *gin.Context, kind bpm.DecisionKind) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalDecisionRequest
	// The comment body is optional; an empty/absent body is fine, so a bind error on an empty
	// body must not 400. bindJSON tolerates an empty body (it only fails on malformed JSON).
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &req) {
			return
		}
	}
	sub, _ := middleware.SubjectFromContext(c)
	actor := bpm.Actor{
		UserID:  sub.UserID,
		RoleIDs: roleIDsOf(sub.RoleID),
		IsAdmin: sub.IsAdmin,
	}
	inst, err := h.svc.DecideAt(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")), kind, decisionComment(req), actor, req.ExpectedStep)
	if err != nil {
		h.respondServiceError(c, err, "Decide", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": toApprovalInstanceView(inst)}})
}

// overrideRequest is the body of POST /admin/approval-instances/:id/override.
type overrideRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// ExpectedStep is the step the admin's screen showed; a stale one ⇒ 409 APPROVAL_CONFLICT,
	// as for approve/reject. Absent = no guard (older client).
	ExpectedStep *int `json:"expectedStep,omitempty"`
}

// Override handles POST /admin/approval-instances/:id/override — an admin decides the current
// step of an approval in place of its seats, with a mandatory reason (specs/043 FR-023, D-13).
// Admin-gated by the group it is mounted on; the engine refuses a non-admin actor again.
func (h *ApprovalHandler) Override(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req overrideRequest
	if !bindJSON(c, &req) {
		return
	}
	inst, err := h.svc.OverrideApproval(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")),
		bpm.DecisionKind(strings.TrimSpace(req.Decision)), req.Note, approvalActorOf(c), req.ExpectedStep)
	if err != nil {
		h.respondServiceError(c, err, "Override", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": inst}})
}

// Retry handles POST /admin/approval-instances/:id/retry {note} — "Thử lại" on a hồ sơ stalled on an
// automatic step (specs/043 S7, FR-009). 409 APPROVAL_NOT_STUCK when it is waiting for a person.
func (h *ApprovalHandler) Retry(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalRequestBody
	if !optionalBody(c, &req) {
		return
	}
	inst, err := h.svc.RetryApproval(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")),
		decisionComment(req.approvalDecisionRequest), approvalActorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "Retry", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": toAdminApprovalInstanceView(inst)}})
}

// Cancel handles POST /admin/approval-instances/:id/cancel {note} — the Nexus team stops a hồ sơ for
// good: the submitter's recall, done by an admin (specs/043 S7).
func (h *ApprovalHandler) Cancel(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalRequestBody
	if !optionalBody(c, &req) {
		return
	}
	inst, err := h.svc.CancelApproval(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")),
		decisionComment(req.approvalDecisionRequest), approvalActorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "Cancel", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": toAdminApprovalInstanceView(inst)}})
}

// approvalActorOf builds the verified actor from the request's claims. One helper so no handler invents a
// different notion of "who is asking".
func approvalActorOf(c *gin.Context) bpm.Actor {
	sub, _ := middleware.SubjectFromContext(c)
	return bpm.Actor{UserID: sub.UserID, RoleIDs: roleIDsOf(sub.RoleID), IsAdmin: sub.IsAdmin}
}

// optionalBody binds a JSON body when one was sent. An absent/empty body is valid for all three
// request verbs (comment and delegate target are the only fields, and delegate validates its own).
func optionalBody(c *gin.Context, dst any) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	return bindJSON(c, dst)
}

// approvalRequestBody is the body of recall and delegate. It EMBEDS the decision body so the note
// field accepts the same two dialects approve/reject already do ("note" the FE sends, "comment" the
// legacy alias) — a request verb that silently dropped the reason while the decision verb kept it
// would be the accept-then-swallow shape this codebase keeps paying for.
type approvalRequestBody struct {
	approvalDecisionRequest
	ToUserID string `json:"toUserId"`
}

// Submit handles POST /:entity/:id/approvals/submit — "Gửi duyệt".
//
// This is the verb that did not exist: approval processes could only fire on after_create /
// after_update, so the system decided for you the moment you saved and the submitter had no way to
// ask, and therefore no way to un-ask. 201 with the created instance; a record that matches no
// manual process is 422 APPROVAL_NO_PROCESS, never a silent success.
func (h *ApprovalHandler) Submit(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req struct {
		ProcessID string `json:"processId"`
	}
	if !optionalBody(c, &req) {
		return
	}
	key, _, kerr := idempotencyKeyOfMax(c, maxApprovalIdempotencyKey)
	if kerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": kerr.Error(), "code": "IDEMPOTENCY_KEY_INVALID"})
		return
	}
	inst, err := h.svc.SubmitForApproval(c.Request.Context(), scopeOf(c), tenantID,
		c.Param("entity"), c.Param("id"), approvalActorOf(c),
		bpm.SubmitOptions{ProcessID: strings.TrimSpace(req.ProcessID), IdempotencyKey: key})
	if err != nil {
		h.respondSendError(c, err, "Submit", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"approval": toApprovalInstanceView(inst)}})
}

// maxApprovalIdempotencyKey is the width of sys_approval_instances.idempotency_key.
const maxApprovalIdempotencyKey = 128

// Startable handles GET /:entity/:id/approvals/startable — the manual processes this record may be
// sent through right now, for the "Gửi duyệt" picker (specs/043 S9b).
func (h *ApprovalHandler) Startable(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	out, err := h.svc.StartableProcesses(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id"))
	if err != nil {
		h.respondSendError(c, err, "Startable", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"processes": out}})
}

// respondSendError maps the row-visibility probe's refusal to 404 like GET /:entity/:id (it used to
// fall through to a 500: the sentinels were unmapped here, only ListForRecord mapped them).
func (h *ApprovalHandler) respondSendError(c *gin.Context, err error, op, tenantID string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordEditForbidden): // #43: readable, outside the edit scope
		respondRecordEditForbidden(c)
	default:
		h.respondServiceError(c, err, op, tenantID)
	}
}

// Recall handles POST /approvals/:id/recall — the submitter withdraws a pending request.
func (h *ApprovalHandler) Recall(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalRequestBody
	if !optionalBody(c, &req) {
		return
	}
	inst, err := h.svc.RecallApproval(c.Request.Context(), tenantID,
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(decisionComment(req.approvalDecisionRequest)), approvalActorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "Recall", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": toApprovalInstanceView(inst)}})
}

// Delegate handles POST /approvals/:id/delegate — the current approver hands THIS step on.
func (h *ApprovalHandler) Delegate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req approvalRequestBody
	if !optionalBody(c, &req) {
		return
	}
	if strings.TrimSpace(req.ToUserID) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":   "thiếu toUserId — cần chỉ đích danh người nhận uỷ quyền",
			"code":    "APPROVAL_DELEGATE_TARGET_REQUIRED",
			"details": gin.H{"field": "toUserId"},
		})
		return
	}
	inst, err := h.svc.DelegateApproval(c.Request.Context(), tenantID,
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(req.ToUserID),
		strings.TrimSpace(decisionComment(req.approvalDecisionRequest)), approvalActorOf(c))
	if err != nil {
		h.respondServiceError(c, err, "Delegate", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approval": toApprovalInstanceView(inst)}})
}

// roleIDsOf returns the actor's role ids as a slice (the JWT subject carries a single role id;
// returns an empty slice when the user holds none).
func roleIDsOf(roleID string) []string {
	if strings.TrimSpace(roleID) == "" {
		return nil
	}
	return []string{roleID}
}

// respondServiceError maps an approval-service error to the right HTTP status via statusForCode.
func (h *ApprovalHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.Approval."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu phê duyệt", "code": "APPROVAL_OP_FAILED"})
}
