// WorkQueueService — VIEC-HOM-NAY (../specs/012-work-queues/spec.md): role-based work queues as pure
// metadata over the existing record engine. Config-time validation runs the SAME code the
// runtime uses (validator-parity: resolveDateFilters + validateFilterOperator + the RM-09 sort
// allowlist), so a queue that saves is a queue that runs. The end-user listing authorizes each
// queue's entity through ACLService (ACL-by-construction, mirroring the ⌘K adapter) and counts
// via RecordService.Aggregate UNDER THE CALLER'S SCOPE — a queue can never leak a row count the
// caller could not see by opening the list themselves.
package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workqueue"
)

// workQueueStore is the persistence seam (satisfied by *workqueue.Repository).
type workQueueStore interface {
	Create(ctx context.Context, q *workqueue.Queue) error
	Update(ctx context.Context, tenantID string, q *workqueue.Queue) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	GetByID(ctx context.Context, tenantID, id string) (*workqueue.Queue, error)
	ListAll(ctx context.Context, tenantID string) ([]workqueue.Queue, error)
	ListActive(ctx context.Context, tenantID string) ([]workqueue.Queue, error)
}

// workQueueRoleChecker verifies a role id exists in the tenant (auth.RoleRepository.GetRoleByID).
type workQueueRoleChecker interface {
	RoleExists(ctx context.Context, tenantID, roleID string) (bool, error)
}

// workQueueACL mirrors the assistant adapter's ACL seam (services.ACLService).
type workQueueACL interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
}

// workQueueSeatCounter counts the work items waiting in a queue's pool (satisfied by
// *bpm.SQLRepository) — what a queue with the "claim" verb shows as its number.
type workQueueSeatCounter interface {
	CountPoolSeats(ctx context.Context, tenantID, queueID, entity string) (int64, error)
}

// WorkQueueService validates, persists and serves work queues.
type WorkQueueService struct {
	store  workQueueStore
	rec    *RecordService
	acl    workQueueACL
	roles  workQueueRoleChecker
	seats  workQueueSeatCounter // optional: nil → a claim queue's count is null (honest "—")
	logger *zap.Logger
}

// WithSeatCounter arms the count of a queue with the "claim" verb. Chainable.
func (s *WorkQueueService) WithSeatCounter(c workQueueSeatCounter) *WorkQueueService {
	s.seats = c
	return s
}

// NewWorkQueueService wires the service. rec is REQUIRED (validation + counts); acl/roles are
// required for serving/validating but nil-tolerated with fail-closed behavior.
func NewWorkQueueService(store workQueueStore, rec *RecordService, acl workQueueACL, roles workQueueRoleChecker, logger *zap.Logger) *WorkQueueService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WorkQueueService{store: store, rec: rec, acl: acl, roles: roles, logger: logger}
}

// WorkQueueInput is the create/update payload (handler-mapped).
type WorkQueueInput struct {
	Name       string
	Label      string
	EntityName string
	RoleID     string // "" = visible to everyone
	Filters    []workqueue.QueueFilter
	SortField  string
	SortOrder  string
	ActionVerb string
	Position   int
	IsActive   *bool // nil = true on create / keep semantics on update handled by handler passing full state
}

// v1 action allowlist (../specs/012-work-queues/spec.md §1): deep-links only. "claim" (specs/043
// S5) links to the pool of work items waiting to be claimed and counts them instead of records.
var workQueueActions = map[string]bool{"open": true, "approve": true, "claim": true}

// ValidateWorkQueueConfig is the exported config-time validator for a work queue's shape +
// filters + sort — ONE body with the admin apply path: WorkQueueService.validate runs THIS
// function (validator-parity), and the AI Builder's propose_workqueue tool runs it too, so a
// draft that buffers is exactly what POST /admin/work-queues accepts. It normalizes in place
// (trims, actionVerb/sortOrder defaults) exactly as the apply path persists. resolveSpecs
// supplies the entity's field specs: the admin path resolves through RecordService.resolve (the
// SAME path the list/aggregate runtime uses); the AI tool supplies registry-backed specs, or
// shape-only specs for an entity drafted this turn. Two checks are deliberately NOT here, being
// tenant-scoped + DB-dependent: entity existence (each caller's resolveSpecs maps its own
// not-found) and role-id existence (validate does it after; the AI tool proposes a role NAME
// resolved to an id at Apply).
func ValidateWorkQueueConfig(in *WorkQueueInput, resolveSpecs func(entity string) ([]FieldSpec, error)) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Label = strings.TrimSpace(in.Label)
	in.EntityName = strings.TrimSpace(in.EntityName)
	in.ActionVerb = strings.TrimSpace(in.ActionVerb)
	if in.ActionVerb == "" {
		in.ActionVerb = "open"
	}
	in.SortOrder = strings.ToLower(strings.TrimSpace(in.SortOrder))
	if in.SortOrder == "" {
		in.SortOrder = "asc"
	}

	if in.Name == "" || in.Label == "" {
		return invalid("WORKQUEUE_INVALID", "name và label là bắt buộc")
	}
	if utf8.RuneCountInString(in.Name) > 64 || utf8.RuneCountInString(in.Label) > 150 {
		return invalid("WORKQUEUE_INVALID", "name tối đa 64, label tối đa 150 ký tự")
	}
	if !workQueueActions[in.ActionVerb] {
		return invalid("WORKQUEUE_ACTION_INVALID", "action %q không hỗ trợ — chỉ có open/approve/claim", in.ActionVerb)
	}
	if in.SortOrder != "asc" && in.SortOrder != "desc" {
		return invalid("WORKQUEUE_INVALID", "sortOrder phải là asc hoặc desc")
	}
	// A claim queue counts the seats of its pool, not records: a filter or a sort would be accepted
	// and then silently ignored by the count (the "swallowed parameter" class), so refuse them here.
	if in.ActionVerb == "claim" && (len(in.Filters) > 0 || strings.TrimSpace(in.SortField) != "" || in.SortOrder != "asc") {
		return invalid("WORKQUEUE_CLAIM_NO_FILTER", "hàng đợi «claim» đếm việc đang chờ nhận — không dùng bộ lọc hay sắp xếp")
	}

	specs, err := resolveSpecs(in.EntityName)
	if err != nil {
		return err
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	// Each filter: field exists (incl. polymorphic sub-columns), operator legal for the type,
	// date operands (token or literal) resolve — all via the runtime's own functions.
	for _, f := range in.Filters {
		field := strings.TrimSpace(f.Field)
		if field == "" {
			return invalidField("WORKQUEUE_FILTER_INVALID", field, "filter thiếu field")
		}
		spec, ok := byName[field]
		if !ok {
			if _, isSub := polymorphicSubField(byName, field); isSub {
				spec = polymorphicSubSpec(field)
			} else {
				return invalidField("WORKQUEUE_FILTER_INVALID", field, "field %q không có trong entity", field)
			}
		}
		op := dynrepo.FilterOperator(strings.TrimSpace(f.Operator))
		vals := filterValuesOf(f.Value, op)
		if spec.FieldType == "date" || spec.FieldType == "datetime" {
			var derr error
			op, vals, derr = resolveDateFilters(spec.FieldType, op, vals, time.Now())
			if derr != nil {
				return invalidField("WORKQUEUE_FILTER_INVALID", field, "%s", derr.Error())
			}
		}
		if _, _, verr := validateFilterOperator(spec, op, vals); verr != nil {
			return invalidField("WORKQUEUE_FILTER_INVALID", field, "%s", verr.Error())
		}
	}

	// Sort field: RM-09 parity — must be in the same allowlist the list endpoint enforces.
	if sf := strings.TrimSpace(in.SortField); sf != "" {
		if _, ok := buildSortAllowlist(specs)[sf]; !ok {
			return invalid("WORKQUEUE_SORT_NOT_SORTABLE",
				"field %q không thể sắp xếp (cần unique/relation/indexed hoặc cột hệ thống)", sf)
		}
	}
	return nil
}

// validate runs the FULL config-time validation (one body with the runtime — spec §4): the
// exported ValidateWorkQueueConfig over specs resolved through RecordService.resolve, then the
// tenant-scoped role-existence check.
func (s *WorkQueueService) validate(ctx context.Context, tenantID string, in *WorkQueueInput) error {
	if err := ValidateWorkQueueConfig(in, func(entity string) ([]FieldSpec, error) {
		// Entity + field specs through the SAME resolve path the list/aggregate runtime uses.
		_, specs, err := s.rec.resolve(ctx, tenantID, entity)
		if err != nil {
			var ve *ValidationError
			if errors.Is(err, ErrEntityNotFound) || errors.As(err, &ve) {
				return nil, invalid("WORKQUEUE_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
			}
			return nil, fmt.Errorf("services.WorkQueueService.validate: resolve: %w", err)
		}
		return specs, nil
	}); err != nil {
		return err
	}

	// Role must exist in the tenant when set.
	if rid := strings.TrimSpace(in.RoleID); rid != "" {
		if s.roles == nil {
			return fmt.Errorf("services.WorkQueueService.validate: role checker not wired")
		}
		ok, err := s.roles.RoleExists(ctx, tenantID, rid)
		if err != nil {
			return fmt.Errorf("services.WorkQueueService.validate: role check: %w", err)
		}
		if !ok {
			return invalid("WORKQUEUE_ROLE_NOT_FOUND", "role %q không tồn tại trong tenant", rid)
		}
	}
	return nil
}

// filterValuesOf splits a stored single-string value into operator-appropriate operands
// (between/in are comma-joined in storage, mirroring the FE FilterBuilder wire shape).
func filterValuesOf(v string, op dynrepo.FilterOperator) []string {
	v = strings.TrimSpace(v)
	switch op {
	case dynrepo.FilterIsNull, dynrepo.FilterIsNotNull:
		return nil
	case dynrepo.FilterBetween, dynrepo.FilterIn:
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		if v == "" {
			return nil
		}
		return []string{v}
	}
}

// Create validates + persists a queue.
func (s *WorkQueueService) Create(ctx context.Context, tenantID string, in WorkQueueInput) (*workqueue.Queue, error) {
	if err := s.validate(ctx, tenantID, &in); err != nil {
		return nil, err
	}
	q := queueFromInput(tenantID, uuid.NewString(), in)
	if err := s.store.Create(ctx, q); err != nil {
		if errors.Is(err, workqueue.ErrDuplicateName) {
			return nil, invalid("WORKQUEUE_ALREADY_EXISTS", "queue %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.WorkQueueService.Create: %w", err)
	}
	return q, nil
}

// Update validates + full-replaces a queue.
func (s *WorkQueueService) Update(ctx context.Context, tenantID, id string, in WorkQueueInput) (*workqueue.Queue, error) {
	if err := s.validate(ctx, tenantID, &in); err != nil {
		return nil, err
	}
	q := queueFromInput(tenantID, id, in)
	if err := s.store.Update(ctx, tenantID, q); err != nil {
		switch {
		case errors.Is(err, workqueue.ErrNotFound):
			return nil, invalid("WORKQUEUE_NOT_FOUND", "queue không tồn tại")
		case errors.Is(err, workqueue.ErrDuplicateName):
			return nil, invalid("WORKQUEUE_ALREADY_EXISTS", "queue %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.WorkQueueService.Update: %w", err)
	}
	return s.store.GetByID(ctx, tenantID, id)
}

// Delete soft-deletes a queue.
func (s *WorkQueueService) Delete(ctx context.Context, tenantID, id string) error {
	if err := s.store.SoftDelete(ctx, tenantID, id); err != nil {
		if errors.Is(err, workqueue.ErrNotFound) {
			return invalid("WORKQUEUE_NOT_FOUND", "queue không tồn tại")
		}
		return fmt.Errorf("services.WorkQueueService.Delete: %w", err)
	}
	return nil
}

// ListAdmin returns every queue (admin screen).
func (s *WorkQueueService) ListAdmin(ctx context.Context, tenantID string) ([]workqueue.Queue, error) {
	return s.store.ListAll(ctx, tenantID)
}

// QueueView is one end-user queue card: metadata + live count + deep-link.
type QueueView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Label      string `json:"label"`
	EntityName string `json:"entityName"`
	ActionVerb string `json:"actionVerb"`
	// Count is the live record count under the CALLER's row scope; null when counting failed
	// (stale queue config) — the card renders "—" instead of killing the whole home screen.
	Count *int64 `json:"count"`
	Href  string `json:"href"`
}

// ListForUser returns the queues the caller may see, each with a live count computed under the
// caller's own ACL scope. Entity not viewable by the caller → the queue is silently omitted
// (fail-closed, no existence leak — same posture as the ⌘K assistant).
func (s *WorkQueueService) ListForUser(ctx context.Context, scope RecordScope, tenantID string) ([]QueueView, error) {
	rows, err := s.store.ListActive(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.WorkQueueService.ListForUser: %w", err)
	}
	out := make([]QueueView, 0, len(rows))
	for i := range rows {
		q := rows[i]
		// Visibility: role NULL = everyone; admins see all; else exact role match.
		if q.RoleID != nil && *q.RoleID != "" && !scope.IsAdmin && *q.RoleID != scope.RoleID {
			continue
		}
		// Module-level ACL on the queue's entity (middleware does this for /:entity routes;
		// this route must do it by construction).
		qScope := scope
		if !scope.IsAdmin {
			if s.acl == nil {
				continue // cannot authorize → fail closed
			}
			allowed, level, aerr := s.acl.Authorize(ctx, tenantID, scope.RoleID, q.EntityName, "view")
			if aerr != nil || !allowed {
				continue
			}
			qScope.AccessLevel = level
		} else {
			qScope.AccessLevel = string(dynrepo.AccessAll)
		}
		// The card opens the entity's list, which shows records manually shared with the caller
		// (specs/051) — the count must include them or it reads lower than the list.
		qScope.IncludeShares = true

		view := QueueView{
			ID: q.ID, Name: q.Name, Label: q.Label,
			EntityName: q.EntityName, ActionVerb: q.ActionVerb,
			Href: queueHref(q),
		}
		// A claim queue counts the work items waiting in ITS pool (open, unclaimed, on its entity) —
		// the caller's entity view was authorized above, and the pool is the queue's own. No filter
		// applies: a filter selects records, a pool holds seats.
		if q.ActionVerb == "claim" {
			if s.seats != nil {
				if n, cerr := s.seats.CountPoolSeats(ctx, tenantID, q.ID, q.EntityName); cerr != nil {
					s.logger.Warn("workqueue pool count failed",
						zap.String("tenant_id", tenantID), zap.String("queue", q.Name), zap.Error(cerr))
				} else {
					view.Count = &n
				}
			}
			out = append(out, view)
			continue
		}
		// Live count under the caller's scope. Per-queue fail-open to null (a broken queue must
		// not blank the whole home screen) — but the failure is logged, never silent.
		agg, cerr := s.rec.Aggregate(ctx, qScope, tenantID, q.EntityName, AggregateQuery{
			Metric:  AggMetricCount,
			Filters: queueFilterInputs(q.Filters),
		})
		if cerr != nil {
			s.logger.Warn("workqueue count failed",
				zap.String("tenant_id", tenantID), zap.String("queue", q.Name), zap.Error(cerr))
		} else {
			c := agg.Scalar.Count
			view.Count = &c
		}
		out = append(out, view)
	}
	return out, nil
}

// queueFilterInputs converts stored filters to the engine's FilterInput shape.
func queueFilterInputs(fs []workqueue.QueueFilter) []FilterInput {
	out := make([]FilterInput, 0, len(fs))
	for _, f := range fs {
		op := dynrepo.FilterOperator(strings.TrimSpace(f.Operator))
		out = append(out, FilterInput{Field: strings.TrimSpace(f.Field), Operator: op, Values: filterValuesOf(f.Value, op)})
	}
	return out
}

// queueHref builds the deep-link the card opens: approve → the approvals inbox; open → the
// entity list pre-filtered via the FE's bracket syntax (?field[op]=value — parseUrlFilters).
func queueHref(q workqueue.Queue) string {
	if q.ActionVerb == "approve" {
		return "/approvals"
	}
	if q.ActionVerb == "claim" {
		return "/approvals?view=pool"
	}
	v := url.Values{}
	for _, f := range q.Filters {
		op := strings.TrimSpace(f.Operator)
		if op == "" {
			op = "eq"
		}
		v.Set(fmt.Sprintf("%s[%s]", strings.TrimSpace(f.Field), op), strings.TrimSpace(f.Value))
	}
	if q.SortField != nil && strings.TrimSpace(*q.SortField) != "" {
		v.Set("sort", strings.TrimSpace(*q.SortField))
		v.Set("order", q.SortOrder)
	}
	if qs := v.Encode(); qs != "" {
		return "/" + q.EntityName + "?" + qs
	}
	return "/" + q.EntityName
}

func queueFromInput(tenantID, id string, in WorkQueueInput) *workqueue.Queue {
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	var roleID *string
	if rid := strings.TrimSpace(in.RoleID); rid != "" {
		roleID = &rid
	}
	var sortField *string
	if sf := strings.TrimSpace(in.SortField); sf != "" {
		sortField = &sf
	}
	return &workqueue.Queue{
		ID: id, TenantID: tenantID,
		Name: in.Name, Label: in.Label, EntityName: in.EntityName,
		RoleID: roleID, Filters: in.Filters,
		SortField: sortField, SortOrder: in.SortOrder,
		ActionVerb: in.ActionVerb, Position: in.Position, IsActive: active,
	}
}
