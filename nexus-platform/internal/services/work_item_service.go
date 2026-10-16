package services

// WorkItemService — "Việc của tôi" (specs/043 S5, FR-011..015): the inbox a person works from,
// read off the work seats, and the verbs on a pool seat — claim, release, complete.
//
// Approve / reject / delegate are NOT here. They keep their one route (/approvals/:id/*), where
// the engine finds the actor's seat itself — including a pool seat the actor has claimed — so
// there is one decision path, not two (`// ponytail:` plan.md "Mới — người dùng").
//
// Authority is judged before any business logic and answers "not found" to a stranger: an item
// the caller is not a candidate of looks exactly like an id that does not exist (FR-015).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// workItemStore is the consumer-side slice of the BPM repository the service needs. Every method is
// tenant-pinned in the real implementation (*bpm.SQLRepository).
type workItemStore interface {
	ListWorkItems(ctx context.Context, tenantID string, q bpm.WorkItemQuery) ([]bpm.WorkItem, error)
	CountWorkItems(ctx context.Context, tenantID string, q bpm.WorkItemQuery, now time.Time) (mine, pool, overdue int64, err error)
	PoolSeatEntities(ctx context.Context, tenantID string) ([]string, error)
	GetWorkItem(ctx context.Context, tenantID, seatID string) (*bpm.WorkItem, error)
	ClaimSeat(ctx context.Context, tenantID, seatID, userID string, roleIDs []string) error
	ReleaseSeat(ctx context.Context, tenantID, seatID, userID string, roleIDs []string, isAdmin bool) error
}

// workItemACL answers module-level view access for a role on an entity (services.ACLService).
type workItemACL interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
}

// WorkItemActor is the verified caller: user id, the single role the JWT carries, admin bit, and
// the teams they belong to (filled by the service from its team directory — a team pool is held by
// its members, specs/043 S6a).
type WorkItemActor struct {
	UserID  string
	RoleID  string
	IsAdmin bool
	TeamIDs []string
}

// holderIDs are the ids a seat can name the caller by: their role and their teams (bpm.Actor.HolderIDs).
func (a WorkItemActor) holderIDs() []string {
	return a.bpmActor().HolderIDs()
}

func (a WorkItemActor) roleIDs() []string {
	if strings.TrimSpace(a.RoleID) == "" {
		return nil
	}
	return []string{a.RoleID}
}

func (a WorkItemActor) bpmActor() bpm.Actor {
	return bpm.Actor{UserID: a.UserID, RoleIDs: a.roleIDs(), TeamIDs: a.TeamIDs, IsAdmin: a.IsAdmin}
}

// WorkItemService serves the work-item routes.
type WorkItemService struct {
	store   workItemStore
	decider approvalDecider
	records approvalRecordReader
	acl     workItemACL
	teams   teamDirectory // optional: nil = nobody is a member of any team (fail closed)
	logger  *zap.Logger
	now     func() time.Time
}

// WithTeamDirectory arms team membership: a team pool shows up for, and can be claimed by, its
// members. Chainable.
func (s *WorkItemService) WithTeamDirectory(d teamDirectory) *WorkItemService {
	s.teams = d
	return s
}

// withTeams returns the caller with their teams read (once per call, tenant-pinned).
func (s *WorkItemService) withTeams(ctx context.Context, tenantID string, actor WorkItemActor) WorkItemActor {
	actor.TeamIDs = teamIDsOf(ctx, s.teams, s.logger, tenantID, actor.UserID)
	return actor
}

// NewWorkItemService wires the service. decider and records are required for Complete; acl is
// required to list a pool of queue items (nil fails closed: no queue item is shown or claimed).
func NewWorkItemService(store workItemStore, decider approvalDecider, records approvalRecordReader, acl workItemACL, logger *zap.Logger) *WorkItemService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WorkItemService{store: store, decider: decider, records: records, acl: acl, logger: logger, now: time.Now}
}

const (
	defaultWorkItemLimit = 50
	maxWorkItemLimit     = 200
)

// WorkItemPage is one page of the inbox.
type WorkItemPage struct {
	Items      []bpm.WorkItem
	NextCursor string // "" = last page
	Now        time.Time
}

// WorkItemCounts are the home-card numbers.
type WorkItemCounts struct {
	Mine    int64
	Pool    int64
	Overdue int64
}

// queueScope narrows the queue seats a caller may see to the record entities they can view. An
// admin is unrestricted. With no ACL wired, or an entity the ACL cannot answer for, the entity is
// left out (fail closed).
func (s *WorkItemService) queueScope(ctx context.Context, tenantID string, actor WorkItemActor) (restrict bool, entities []string, err error) {
	if actor.IsAdmin {
		return false, nil, nil
	}
	if s.acl == nil {
		return true, nil, nil
	}
	all, err := s.store.PoolSeatEntities(ctx, tenantID)
	if err != nil {
		return false, nil, fmt.Errorf("services.WorkItemService.queueScope: %w", err)
	}
	for _, e := range all {
		ok, _, aerr := s.acl.Authorize(ctx, tenantID, actor.RoleID, e, "view")
		if aerr != nil {
			s.logger.Warn("work items: acl check failed — queue items of the entity left out",
				zap.String("tenant_id", tenantID), zap.String("entity_name", e), zap.Error(aerr))
			continue
		}
		if ok {
			entities = append(entities, e)
		}
	}
	return true, entities, nil
}

func (s *WorkItemService) query(ctx context.Context, tenantID string, actor WorkItemActor, view bpm.WorkItemView) (bpm.WorkItemQuery, error) {
	restrict, entities, err := s.queueScope(ctx, tenantID, actor)
	if err != nil {
		return bpm.WorkItemQuery{}, err
	}
	actor = s.withTeams(ctx, tenantID, actor)
	return bpm.WorkItemQuery{
		View: view, UserID: actor.UserID, RoleIDs: actor.holderIDs(),
		RestrictQueues: restrict, QueueEntities: entities,
	}, nil
}

// List returns one page of the caller's inbox. view is mine (default), pool or done.
func (s *WorkItemService) List(ctx context.Context, tenantID string, actor WorkItemActor, view, entity string, limit int, cursor string) (*WorkItemPage, error) {
	v := bpm.WorkItemView(strings.TrimSpace(view))
	if v == "" {
		v = bpm.ViewMine
	}
	if !bpm.ValidWorkItemView(v) {
		return nil, invalidField("WORK_ITEM_VIEW_INVALID", "view", "view phải là mine, pool hoặc done")
	}
	if limit <= 0 {
		limit = defaultWorkItemLimit
	}
	if limit > maxWorkItemLimit {
		limit = maxWorkItemLimit
	}
	q, err := s.query(ctx, tenantID, actor, v)
	if err != nil {
		return nil, err
	}
	q.Entity, q.Limit = strings.TrimSpace(entity), limit
	if c := strings.TrimSpace(cursor); c != "" {
		pos, ok := bpm.DecodeWorkItemCursor(c)
		if !ok {
			return nil, invalidField("WORK_ITEM_CURSOR_INVALID", "cursor", "cursor không hợp lệ")
		}
		q.After = &pos
	}
	items, err := s.store.ListWorkItems(ctx, tenantID, q)
	if err != nil {
		return nil, fmt.Errorf("services.WorkItemService.List: %w", err)
	}
	page := &WorkItemPage{Now: s.now().UTC()}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		at := last.CreatedAt
		if v == bpm.ViewDone && last.CompletedAt != nil {
			at = *last.CompletedAt
		}
		page.NextCursor = bpm.WorkItemCursor{At: at, ID: last.ID}.Encode()
	}
	page.Items = items
	return page, nil
}

// Counts returns the home-card numbers: mine, waiting to be claimed, and mine past their deadline.
func (s *WorkItemService) Counts(ctx context.Context, tenantID string, actor WorkItemActor) (*WorkItemCounts, error) {
	q, err := s.query(ctx, tenantID, actor, bpm.ViewMine)
	if err != nil {
		return nil, err
	}
	mine, pool, overdue, err := s.store.CountWorkItems(ctx, tenantID, q, s.now())
	if err != nil {
		return nil, fmt.Errorf("services.WorkItemService.Counts: %w", err)
	}
	return &WorkItemCounts{Mine: mine, Pool: pool, Overdue: overdue}, nil
}

// visible reads the item and answers "not found" unless the caller may know it exists: an admin, a
// candidate of its seat, or the user who claimed it. One body for claim, release and complete.
func (s *WorkItemService) visible(ctx context.Context, tenantID, id string, actor WorkItemActor) (*bpm.WorkItem, error) {
	it, err := s.store.GetWorkItem(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		return nil, mapWorkItemErr(err)
	}
	if !actor.IsAdmin && !it.Seat.CandidateFor(actor.UserID, actor.holderIDs()) && it.ClaimedBy != actor.UserID {
		return nil, mapWorkItemErr(bpm.ErrSeatNotFound)
	}
	return it, nil
}

// entityViewFor builds the entity-view rule a caller's queue seats are judged by (bpm.EntityView):
// an admin is not judged (nil); otherwise an entity is viewable when any of the caller's roles may
// "view" it. One body for the work-item routes and the approval surface, so the two cannot answer
// differently for the same seat. With no ACL wired, or an ACL that errors, nothing is viewable
// (fail closed). Answers are memoised for the call; the closure must not outlive it.
func entityViewFor(ctx context.Context, acl workItemACL, logger *zap.Logger, tenantID string, roleIDs []string, isAdmin bool) bpm.EntityView {
	if isAdmin {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	memo := map[string]bool{}
	return func(entity string) bool {
		if ok, seen := memo[entity]; seen {
			return ok
		}
		ok := false
		if acl != nil {
			for _, role := range roleIDs {
				allowed, _, err := acl.Authorize(ctx, tenantID, role, entity, "view")
				if err != nil {
					logger.Warn("queue seat: acl check failed — entity treated as not viewable",
						zap.String("tenant_id", tenantID), zap.String("entity_name", entity), zap.Error(err))
					continue
				}
				if allowed {
					ok = true
					break
				}
			}
		}
		memo[entity] = ok
		return ok
	}
}

// viewableQueueItem enforces, for a QUEUE item, the entity ACL the pool listing applies: a queue
// seat is shown and claimable only to someone who may view the record's entity (specs/043 plan
// "Quyền"). Role and user seats follow the seat alone. An admin passes.
func (s *WorkItemService) viewableQueueItem(ctx context.Context, tenantID string, it *bpm.WorkItem, actor WorkItemActor) error {
	if it.AssigneeType != bpm.ApproverQueue {
		return nil
	}
	if view := s.actorFor(ctx, tenantID, actor).QueueView; view != nil && !view(it.EntityName) {
		return mapWorkItemErr(bpm.ErrSeatNotFound)
	}
	return nil
}

// actorFor is the engine actor of a work-item caller, carrying the entity-view rule of queue seats.
func (s *WorkItemService) actorFor(ctx context.Context, tenantID string, actor WorkItemActor) bpm.Actor {
	a := actor.bpmActor()
	a.QueueView = entityViewFor(ctx, s.acl, s.logger, tenantID, a.RoleIDs, a.IsAdmin)
	return a
}

// Claim takes a pool item for the caller and returns it as it now stands.
func (s *WorkItemService) Claim(ctx context.Context, tenantID, id string, actor WorkItemActor) (*bpm.WorkItem, error) {
	actor = s.withTeams(ctx, tenantID, actor)
	it, err := s.visible(ctx, tenantID, id, actor)
	if err != nil {
		return nil, err
	}
	if err := s.viewableQueueItem(ctx, tenantID, it, actor); err != nil {
		return nil, err
	}
	if err := s.store.ClaimSeat(ctx, tenantID, it.ID, actor.UserID, actor.holderIDs()); err != nil {
		return nil, mapWorkItemErr(err)
	}
	s.logger.Info("work item claimed",
		zap.String("tenant_id", tenantID), zap.String("user_id", actor.UserID),
		zap.String("work_item_id", it.ID), zap.String("entity_name", it.EntityName))
	return s.store.GetWorkItem(ctx, tenantID, it.ID)
}

// Release puts a claimed item back in the pool. Only the claimer or an admin may.
func (s *WorkItemService) Release(ctx context.Context, tenantID, id string, actor WorkItemActor) (*bpm.WorkItem, error) {
	actor = s.withTeams(ctx, tenantID, actor)
	it, err := s.visible(ctx, tenantID, id, actor)
	if err != nil {
		return nil, err
	}
	if err := s.store.ReleaseSeat(ctx, tenantID, it.ID, actor.UserID, actor.holderIDs(), actor.IsAdmin); err != nil {
		return nil, mapWorkItemErr(err)
	}
	s.logger.Info("work item released",
		zap.String("tenant_id", tenantID), zap.String("user_id", actor.UserID), zap.String("work_item_id", it.ID))
	return s.store.GetWorkItem(ctx, tenantID, it.ID)
}

// Complete finishes a task (or rework) item: every required field must be filled on the record,
// then the engine closes the seat and moves the instance on, exactly as an approval would.
func (s *WorkItemService) Complete(ctx context.Context, tenantID, id, note string, actor WorkItemActor) (*bpm.Instance, error) {
	actor = s.withTeams(ctx, tenantID, actor)
	it, err := s.visible(ctx, tenantID, id, actor)
	if err != nil {
		return nil, err
	}
	// A queue item is for someone who may view the record: finishing it is not a way around that.
	if err := s.viewableQueueItem(ctx, tenantID, it, actor); err != nil {
		return nil, err
	}
	if it.Status != "open" {
		return nil, mapWorkItemErr(bpm.ErrSeatNotOpen)
	}
	if it.Kind != bpm.SeatKindTask && it.Kind != bpm.SeatKindRework {
		return nil, invalid("WORK_ITEM_NOT_COMPLETABLE", "việc này là bước duyệt — hãy Duyệt hoặc Từ chối thay vì Hoàn thành")
	}
	// The claim is judged here, not only inside the engine, so a candidate whose item someone else
	// took hears "taken" instead of an answer about the record's fields.
	if it.Seat.Claimable() && it.ClaimedBy != "" && it.ClaimedBy != actor.UserID && !actor.IsAdmin {
		return nil, mapWorkItemErr(bpm.ErrSeatClaimed)
	}
	if missing, merr := s.missingRequired(ctx, tenantID, it); merr != nil {
		return nil, merr
	} else if len(missing) > 0 {
		e := invalidField("WORK_ITEM_REQUIRED_FIELDS", missing[0], "còn trường bắt buộc chưa điền: %s", strings.Join(missing, ", ")).(*ValidationError)
		e.Details["fields"] = missing
		return nil, e
	}
	// Bound to the step the caller was SHOWN: a second click that lands after the first already moved
	// the instance on must not complete the next task, whose required fields were never checked.
	var expectedStep *int
	if it.StepIndex >= 0 {
		expectedStep = &it.StepIndex
	}
	inst, err := s.decider.DecideAt(ctx, tenantID, it.InstanceID, bpm.DecisionComplete, strings.TrimSpace(note), s.actorFor(ctx, tenantID, actor), expectedStep)
	if err != nil {
		if mapped := mapInstanceErr(err); mapped != nil {
			return nil, mapped
		}
		return nil, fmt.Errorf("services.WorkItemService.Complete: %w", err)
	}
	return inst, nil
}

// missingRequired lists, in the node's own order, the required fields the record has not filled.
// The record is read as the system (not under the caller's row scope): whether the work is done is
// a fact about the record, and the only thing returned is field NAMES the admin configured.
func (s *WorkItemService) missingRequired(ctx context.Context, tenantID string, it *bpm.WorkItem) ([]string, error) {
	if len(it.RequiredFields) == 0 {
		return nil, nil
	}
	if s.records == nil {
		return nil, fmt.Errorf("services.WorkItemService.missingRequired: record reader not wired")
	}
	rec, err := s.records.Get(ctx, RecordScope{IsAdmin: true, AccessLevel: string(dynrepo.AccessAll)}, tenantID, it.EntityName, it.RecordID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("services.WorkItemService.missingRequired: %w", err)
	}
	var missing []string
	for _, f := range it.RequiredFields {
		if isBlankValue(rec.Data[f]) {
			missing = append(missing, f)
		}
	}
	return missing, nil
}

// isBlankValue: a required field is unfilled when absent, null, an empty/whitespace string, or an
// empty list. Zero and false are filled values (a checkbox ticked off is an answer).
func isBlankValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case map[string]any:
		// A composite value (currency, full name…) is filled when any part is.
		for _, part := range x {
			if !isBlankValue(part) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// mapWorkItemErr turns the repository's seat sentinels into the API's coded errors. A seat that is
// not found, and one the caller may not know of, are the same answer.
func mapWorkItemErr(err error) error {
	switch {
	case errors.Is(err, bpm.ErrSeatNotFound):
		return invalid("WORK_ITEM_NOT_FOUND", "không tìm thấy việc này")
	case errors.Is(err, bpm.ErrSeatNotOpen):
		return invalid("WORK_ITEM_NOT_OPEN", "việc này đã được xử lý")
	case errors.Is(err, bpm.ErrSeatClaimed):
		return invalid("WORK_ITEM_ALREADY_CLAIMED", "đã có người nhận việc này")
	case errors.Is(err, bpm.ErrSeatNotClaimable):
		return invalid("WORK_ITEM_NOT_CLAIMABLE", "việc này đã giao đích danh cho một người — không có hàng chờ để nhận")
	case errors.Is(err, bpm.ErrSeatNotClaimed):
		return invalid("WORK_ITEM_NOT_CLAIMED", "việc này chưa có ai nhận")
	case errors.Is(err, bpm.ErrNotClaimer):
		return invalid("WORK_ITEM_RELEASE_FORBIDDEN", "chỉ người đã nhận việc (hoặc quản trị) mới trả lại được")
	default:
		return fmt.Errorf("services.WorkItemService: %w", err)
	}
}
