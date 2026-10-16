//go:build integration

// specs/043 S5 — `bpm-work-items-claim` (tasks T091–T095, T099–T101): the inbox, claim / release /
// complete of pool seats, task steps with required fields and work-queue assignees, proven against
// real MySQL through the production composition (bpmWire + WorkItemService + ApprovalService).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestWorkItem|TestApprovalDecide_.*_Integration'
package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
)

const (
	wiRoleAcc   = "88888888-0000-0000-0000-000000000b01" // "Kế toán" role
	wiRoleSales = "88888888-0000-0000-0000-000000000b02"
	wiUserAcc1  = "99999999-0000-0000-0000-000000000b01"
	wiUserAcc2  = "99999999-0000-0000-0000-000000000b02"
	wiUserSales = "99999999-0000-0000-0000-000000000b03"
)

// wiACL is the entity-view ACL: role → entities it may view.
type wiACL map[string]map[string]bool

func (a wiACL) Authorize(_ context.Context, _, roleID, entity, _ string) (bool, string, error) {
	return a[roleID][entity], "all", nil
}

type wiStack struct {
	*bpmStack
	svc     *WorkItemService
	decider *bpm.Decider
	acl     wiACL
}

func wiWire(t *testing.T) *wiStack {
	t.Helper()
	st := bpmWire(t)
	decider := bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil).WithRecordWriter(NewApprovalOutcomeWriter(st.recordSvc))
	acl := wiACL{}
	st.approval = NewApprovalService(st.repo, st.repo, decider).WithEntityChecker(st.approvalEntities()).WithQueueACL(acl).WithRequestFlow(st.requester, st.recordSvc).
		WithQueueChecker(func(ctx context.Context, tenantID, queueID string) (bool, error) {
			var n int
			err := st.sdb.GetContext(ctx, &n, `SELECT COUNT(*) FROM sys_work_queues WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL AND is_active = 1`, tenantID, queueID)
			return n == 1, err
		})
	return &wiStack{bpmStack: st, svc: NewWorkItemService(st.repo, decider, st.recordSvc, acl, nil), decider: decider, acl: acl}
}

// approvalEntities hands the stack's metadata cache to a rebuilt ApprovalService (the stack built
// its own over the same cache; a second one needs the same entity/field source).
func (s *bpmStack) approvalEntities() workflowEntityChecker { return s.approval.entities }

func taskNode(id, name, approverType, approverID string, required ...string) ApprovalNodeInput {
	return ApprovalNodeInput{Kind: "task", RequiredFields: required, ApprovalStepInput: ApprovalStepInput{
		ID: id, Name: name, ApproverType: approverType, ApproverID: approverID,
	}}
}

func wiApprovalNode(id, name, approverType, approverID string) ApprovalNodeInput {
	return graphNode(id, name, approverType, approverID, 0)
}

// wiEntity registers the entity the tests arm on: a status and a doc_date to fill.
func (s *wiStack) wiEntity(t *testing.T, tenant, entity string) {
	t.Helper()
	s.createEntity(t, context.Background(), tenant, entity, []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "doc_date", Label: "Ngày gửi chứng từ", FieldType: "text"},
	})
}

// wiProcess saves a graph process on the entity (after_create).
func (s *wiStack) wiProcess(t *testing.T, tenant, entity string, nodes ...ApprovalNodeInput) {
	t.Helper()
	if _, err := s.approval.CreateProcess(context.Background(), tenant, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: "after_create", Graph: graphNodes(nodes...),
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
}

// wiRecords creates n records on the entity (each arms one instance after drain) and returns their ids.
func (s *wiStack) wiRecords(t *testing.T, tenant, entity string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		rid, err := s.recordSvc.Create(context.Background(), bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "draft"})
		if err != nil {
			t.Fatalf("create record: %v", err)
		}
		ids[i] = rid
	}
	return ids
}

func acc1() WorkItemActor  { return WorkItemActor{UserID: wiUserAcc1, RoleID: wiRoleAcc} }
func acc2() WorkItemActor  { return WorkItemActor{UserID: wiUserAcc2, RoleID: wiRoleAcc} }
func sales() WorkItemActor { return WorkItemActor{UserID: wiUserSales, RoleID: wiRoleSales} }

func wantItemCode(t *testing.T, err error, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != code {
		t.Fatalf("err = %v; want %s", err, code)
	}
}

func listIDs(t *testing.T, s *wiStack, tenant string, actor WorkItemActor, view string) []string {
	t.Helper()
	page, err := s.svc.List(context.Background(), tenant, actor, view, "", 200, "")
	if err != nil {
		t.Fatalf("List %s: %v", view, err)
	}
	ids := make([]string, len(page.Items))
	for i, it := range page.Items {
		ids[i] = it.ID
	}
	return ids
}

// firstPoolItem arms one pool task on the role and returns the tenant and the single work item.
func firstPoolItem(t *testing.T, s *wiStack, entity string) (string, *bpm.WorkItem) {
	t.Helper()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, entity)
	s.wiProcess(t, tenant, entity, taskNode("check", "Kiểm tra chứng từ", "role", wiRoleAcc, "doc_date"))
	s.wiRecords(t, tenant, entity, 1)
	s.drain(t)
	page, err := s.svc.List(context.Background(), tenant, acc1(), "pool", "", 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("pool of the role = %v (%v); want the one task", page, err)
	}
	return tenant, &page.Items[0]
}

// TestWorkItemClaim_ExclusiveUnderRace (T091, -race): eight holders of the role press Nhận on the
// same pool item at once. Exactly one wins; the other seven are told it is taken; the seat names
// the winner and nobody else.
func TestWorkItemClaim_ExclusiveUnderRace(t *testing.T) {
	s := wiWire(t)
	tenant, item := firstPoolItem(t, s, "wiclaim")

	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.svc.Claim(context.Background(), tenant, item.ID, WorkItemActor{UserID: fmt.Sprintf("racer-%d", i), RoleID: wiRoleAcc})
		}(i)
	}
	close(start)
	wg.Wait()

	winners, taken := 0, 0
	for _, err := range errs {
		var ve *ValidationError
		switch {
		case err == nil:
			winners++
		case errors.As(err, &ve) && ve.Code == "WORK_ITEM_ALREADY_CLAIMED":
			taken++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if winners != 1 || taken != racers-1 {
		t.Fatalf("winners=%d taken=%d; want exactly 1 and %d", winners, taken, racers-1)
	}
	got, err := s.svc.visible(context.Background(), tenant, item.ID, WorkItemActor{IsAdmin: true})
	if err != nil || got.ClaimedBy == "" {
		t.Fatalf("seat after the race: %+v (%v); want one claimer", got, err)
	}
}

// TestWorkItemRelease_OnlyClaimerOrAdmin (T092): a claimed item is released by its claimer or an
// admin; another holder of the role is refused (403 shape), a stranger gets "not found", and an
// unclaimed item cannot be released. After a release the item is back in everyone's pool.
func TestWorkItemRelease_OnlyClaimerOrAdmin(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	tenant, item := firstPoolItem(t, s, "wirelease")

	_, err := s.svc.Release(ctx, tenant, item.ID, acc1())
	wantItemCode(t, err, "WORK_ITEM_NOT_CLAIMED")

	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc1()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ids := listIDs(t, s, tenant, acc2(), "pool"); len(ids) != 0 {
		t.Fatalf("pool of the other holder after the claim = %v; want empty", ids)
	}
	if ids := listIDs(t, s, tenant, acc1(), "mine"); len(ids) != 1 || ids[0] != item.ID {
		t.Fatalf("mine of the claimer = %v; want the claimed item", ids)
	}

	_, err = s.svc.Release(ctx, tenant, item.ID, acc2())
	wantItemCode(t, err, "WORK_ITEM_RELEASE_FORBIDDEN")
	_, err = s.svc.Release(ctx, tenant, item.ID, sales())
	wantItemCode(t, err, "WORK_ITEM_NOT_FOUND")

	if _, err := s.svc.Release(ctx, tenant, item.ID, acc1()); err != nil {
		t.Fatalf("release by the claimer: %v", err)
	}
	if ids := listIDs(t, s, tenant, acc2(), "pool"); len(ids) != 1 {
		t.Fatalf("pool after the release = %v; want the item back", ids)
	}

	// An admin may release someone else's claim.
	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc2()); err != nil {
		t.Fatalf("claim by the other holder: %v", err)
	}
	if _, err := s.svc.Release(ctx, tenant, item.ID, WorkItemActor{UserID: "boss", IsAdmin: true}); err != nil {
		t.Fatalf("release by an admin: %v", err)
	}
}

// TestApprovalDecide_PoolSeatClaimedByOther409_Integration (T092): an APPROVAL given to a role is a
// pool seat too. Once one holder claims it the other's approve through /approvals/:id/approve is
// 409 WORK_ITEM_ALREADY_CLAIMED, a stranger's is the old "not the approver", and the claimer's goes
// through (the engine finds their claimed pool seat — TestApprovalDecide_FindsActorsClaimedPoolSeat).
func TestApprovalDecide_PoolSeatClaimedByOther409_Integration(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wiappr")
	s.wiProcess(t, tenant, "wiappr", wiApprovalNode("kt", "Kế toán duyệt", "role", wiRoleAcc), wiApprovalNode("gd", "Giám đốc", "user", seatAp1))
	s.wiRecords(t, tenant, "wiappr", 1)
	s.drain(t)

	page, err := s.svc.List(ctx, tenant, acc1(), "pool", "", 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != bpm.SeatKindApproval {
		t.Fatalf("pool = %+v (%v); want the one approval seat", page, err)
	}
	item := page.Items[0]
	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc1()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	_, err = s.approval.Decide(ctx, tenant, item.InstanceID, bpm.DecisionApprove, "", acc2().bpmActor())
	wantItemCode(t, err, "WORK_ITEM_ALREADY_CLAIMED")
	_, err = s.approval.Decide(ctx, tenant, item.InstanceID, bpm.DecisionApprove, "", sales().bpmActor())
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED")
	// The other holder's inbox no longer lists the approval either.
	if pend, err := s.approval.ListPending(ctx, tenant, bpm.Actor{UserID: wiUserAcc2, RoleIDs: []string{wiRoleAcc}}); err != nil || len(pend) != 0 {
		t.Fatalf("pending of the other holder = %v (%v); want none", instanceIDs(pend), err)
	}
	// The claimer approves through the ordinary route; the instance moves to the director.
	got, err := s.approval.Decide(ctx, tenant, item.InstanceID, bpm.DecisionApprove, "ok", acc1().bpmActor())
	if err != nil || got.CurrentStep != 1 {
		t.Fatalf("claimer approve: %v step=%v; want step 1", err, got)
	}
}

// TestWorkItemComplete_RequiredFields422 (T093): a task with required fields cannot be completed
// while one is blank — the answer names the field(s) — and completes once the record has them; the
// instance then moves on. A task refuses approve; an approval seat refuses complete.
func TestWorkItemComplete_RequiredFields422(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wicomplete")
	s.wiProcess(t, tenant, "wicomplete",
		taskNode("check", "Kiểm tra chứng từ", "user", seatAp1, "doc_date"),
		wiApprovalNode("gd", "Giám đốc", "user", seatAp2))
	rid := s.wiRecords(t, tenant, "wicomplete", 1)[0]
	s.drain(t)
	ap1 := WorkItemActor{UserID: seatAp1}

	page, err := s.svc.List(ctx, tenant, ap1, "mine", "", 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("mine = %+v (%v); want the task", page, err)
	}
	item := page.Items[0]
	if item.Kind != bpm.SeatKindTask || item.NodeName != "Kiểm tra chứng từ" || len(item.RequiredFields) != 1 {
		t.Fatalf("item = %+v; want the named task with its required field", item)
	}

	// approve is the wrong verb for a task.
	_, err = s.approval.Decide(ctx, tenant, item.InstanceID, bpm.DecisionApprove, "", ap1.bpmActor())
	wantItemCode(t, err, "APPROVAL_STEP_KIND_MISMATCH")

	_, err = s.svc.Complete(ctx, tenant, item.ID, "xong", ap1)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "WORK_ITEM_REQUIRED_FIELDS" {
		t.Fatalf("complete with a blank field: err = %v; want WORK_ITEM_REQUIRED_FIELDS", err)
	}
	if fields, _ := ve.Details["fields"].([]string); len(fields) != 1 || fields[0] != "doc_date" {
		t.Fatalf("details.fields = %v; want [doc_date]", ve.Details["fields"])
	}
	if st := instState(t, s.sdb, tenant, item.InstanceID); st.CurrentStep != 0 {
		t.Fatalf("a refused completion moved the instance to step %d", st.CurrentStep)
	}

	if err := s.recordSvc.Update(ctx, bpmAdmin(seatAp1), tenant, "wicomplete", rid, map[string]any{"doc_date": "2026-10-08"}, nil); err != nil {
		t.Fatalf("fill the field: %v", err)
	}
	inst, err := s.svc.Complete(ctx, tenant, item.ID, "đủ chứng từ", ap1)
	if err != nil || inst.CurrentStep != 1 || inst.Status != bpm.StatusPending {
		t.Fatalf("complete: %v inst=%+v; want to advance to step 1", err, inst)
	}
	if done := listIDs(t, s, tenant, ap1, "done"); len(done) != 1 || done[0] != item.ID {
		t.Fatalf("done of the completer = %v; want the completed task", done)
	}
	if mine := listIDs(t, s, tenant, ap1, "mine"); len(mine) != 0 {
		t.Fatalf("mine after completing = %v; want none", mine)
	}

	// The next seat is the director's approval: complete is the wrong verb there.
	next, err := s.svc.List(ctx, tenant, WorkItemActor{UserID: seatAp2}, "mine", "", 10, "")
	if err != nil || len(next.Items) != 1 {
		t.Fatalf("director's mine = %+v (%v)", next, err)
	}
	_, err = s.svc.Complete(ctx, tenant, next.Items[0].ID, "", WorkItemActor{UserID: seatAp2})
	wantItemCode(t, err, "WORK_ITEM_NOT_COMPLETABLE")
}

// TestWorkItems_QueueVisibilityByRoleAndEntityACL (T094): a task given to a work queue is in the
// pool of the holders of the queue's role who may view the record's entity — not of a user without
// that role, not of one who cannot view the entity, and not once the queue is switched off. A
// role-less queue is everyone's (that can view). Claiming obeys the same rule.
func TestWorkItems_QueueVisibilityByRoleAndEntityACL(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wiqueue")
	queueID := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_work_queues (id, tenant_id, name, label, entity_name, role_id, filters)
		VALUES (?, ?, 'ke_toan', 'Kế toán', 'wiqueue', ?, '[]')`, queueID, tenant, wiRoleAcc); err != nil {
		t.Fatalf("queue: %v", err)
	}
	s.wiProcess(t, tenant, "wiqueue", taskNode("check", "Kiểm tra chứng từ", "queue", queueID))
	s.wiRecords(t, tenant, "wiqueue", 1)
	s.drain(t)
	s.acl[wiRoleAcc] = map[string]bool{"wiqueue": true}
	s.acl[wiRoleSales] = map[string]bool{"wiqueue": true}

	if ids := listIDs(t, s, tenant, acc1(), "pool"); len(ids) != 1 {
		t.Fatalf("pool of the queue's role = %v; want the item", ids)
	}
	if ids := listIDs(t, s, tenant, sales(), "pool"); len(ids) != 0 {
		t.Fatalf("pool of another role = %v; want none", ids)
	}
	// Same role, but the role may not view the record's entity: the item is not theirs to see.
	s.acl[wiRoleAcc] = map[string]bool{}
	if ids := listIDs(t, s, tenant, acc1(), "pool"); len(ids) != 0 {
		t.Fatalf("pool without entity view = %v; want none", ids)
	}
	item := bpm.WorkItem{}
	s.acl[wiRoleAcc] = map[string]bool{"wiqueue": true}
	if page, _ := s.svc.List(ctx, tenant, acc1(), "pool", "", 10, ""); len(page.Items) == 1 {
		item = page.Items[0]
	} else {
		t.Fatalf("pool = %+v", page)
	}
	if item.AssigneeType != bpm.ApproverQueue || item.AssigneeName != "Kế toán" {
		t.Fatalf("queue item = %+v; want a queue assignee named Kế toán", item)
	}

	s.acl[wiRoleAcc] = map[string]bool{}
	_, err := s.svc.Claim(ctx, tenant, item.ID, acc1())
	wantItemCode(t, err, "WORK_ITEM_NOT_FOUND")
	// Finishing a queue item is not a way around the entity ACL either.
	_, err = s.svc.Complete(ctx, tenant, item.ID, "", acc1())
	wantItemCode(t, err, "WORK_ITEM_NOT_FOUND")
	s.acl[wiRoleAcc] = map[string]bool{"wiqueue": true}

	// The home card counts the pool by the same rule.
	if c, err := s.svc.Counts(ctx, tenant, acc1()); err != nil || c.Pool != 1 || c.Mine != 0 {
		t.Fatalf("counts = %+v (%v); want pool 1, mine 0", c, err)
	}
	if c, err := s.svc.Counts(ctx, tenant, sales()); err != nil || c.Pool != 0 {
		t.Fatalf("counts of another role = %+v (%v); want pool 0", c, err)
	}

	// A "claim" work-queue card counts the pool of THAT queue on THAT entity, unclaimed only.
	if n, err := s.repo.CountPoolSeats(ctx, tenant, queueID, "wiqueue"); err != nil || n != 1 {
		t.Fatalf("pool count before the claim = %d (%v); want 1", n, err)
	}
	if n, _ := s.repo.CountPoolSeats(ctx, tenant, queueID, "other_entity"); n != 0 {
		t.Fatalf("pool count on another entity = %d; want 0", n)
	}
	if n, _ := s.repo.CountPoolSeats(ctx, o3aTenantB, queueID, "wiqueue"); n != 0 {
		t.Fatalf("pool count under another tenant = %d; want 0", n)
	}
	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc1()); err != nil {
		t.Fatalf("claim of a queue item: %v", err)
	}
	if n, _ := s.repo.CountPoolSeats(ctx, tenant, queueID, "wiqueue"); n != 0 {
		t.Fatalf("pool count after the claim = %d; want 0 (a claimed item left the pool)", n)
	}
	// The claimer approves/completes through the one engine path; a queue switched off hides nothing
	// already claimed, but takes the pool away from everyone else.
	if _, err := s.sdb.Exec(`UPDATE sys_work_queues SET is_active = 0 WHERE tenant_id = ? AND id = ?`, tenant, queueID); err != nil {
		t.Fatalf("switch the queue off: %v", err)
	}
	if ids := listIDs(t, s, tenant, acc2(), "pool"); len(ids) != 0 {
		t.Fatalf("pool of a switched-off queue = %v; want none", ids)
	}
}

// TestWorkItems_NonCandidate404 (T094): an item the caller is not a candidate of is "not found" on
// every verb — the same answer as an id that does not exist.
func TestWorkItems_NonCandidate404(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	tenant, item := firstPoolItem(t, s, "winon")
	for name, call := range map[string]func() error{
		"claim":    func() error { _, err := s.svc.Claim(ctx, tenant, item.ID, sales()); return err },
		"release":  func() error { _, err := s.svc.Release(ctx, tenant, item.ID, sales()); return err },
		"complete": func() error { _, err := s.svc.Complete(ctx, tenant, item.ID, "", sales()); return err },
		"bogus id": func() error { _, err := s.svc.Claim(ctx, tenant, uuid.NewString(), sales()); return err },
	} {
		wantItemCode(t, call(), "WORK_ITEM_NOT_FOUND")
		_ = name
	}
}

// TestWorkItems_CrossTenantInvisible (T094): the same item id asked for under another tenant is
// "not found", and the other tenant's lists and counts hold nothing of it.
func TestWorkItems_CrossTenantInvisible(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	_, item := firstPoolItem(t, s, "witenant")
	other := WorkItemActor{UserID: wiUserAcc1, RoleID: wiRoleAcc}
	_, err := s.svc.Claim(ctx, o3aTenantB, item.ID, other)
	wantItemCode(t, err, "WORK_ITEM_NOT_FOUND")
	if ids := listIDs(t, s, o3aTenantB, other, "pool"); len(ids) != 0 {
		t.Fatalf("pool under another tenant = %v; want none", ids)
	}
	if c, err := s.svc.Counts(ctx, o3aTenantB, other); err != nil || c.Pool != 0 || c.Mine != 0 {
		t.Fatalf("counts under another tenant = %+v (%v); want zeros", c, err)
	}
	if _, err := s.svc.Claim(ctx, o3aTenantB, item.ID, WorkItemActor{UserID: "boss", IsAdmin: true}); err == nil {
		t.Fatalf("an admin of another tenant claimed this tenant's item")
	}
}

// TestWorkItems_KeysetPagingHasNoGapOrDup (B1): seven items all stamped with the SAME created_at
// are read in pages of three; every item is seen exactly once, in a stable order, and the last page
// has no cursor. The tie-break is the id, in the direction of the sort.
func TestWorkItems_KeysetPagingHasNoGapOrDup(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wipage")
	s.wiProcess(t, tenant, "wipage", taskNode("check", "Kiểm tra", "role", wiRoleAcc))
	s.wiRecords(t, tenant, "wipage", 7)
	s.drain(t)
	if _, err := s.sdb.Exec(`UPDATE sys_bpm_tasks SET created_at = '2026-10-08 09:00:00.000' WHERE tenant_id = ?`, tenant); err != nil {
		t.Fatalf("force ties: %v", err)
	}

	seen := map[string]bool{}
	var order []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		page, err := s.svc.List(ctx, tenant, acc1(), "pool", "", 3, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Fatalf("item %s seen twice (page %d)", it.ID, pages)
			}
			seen[it.ID] = true
			order = append(order, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d of 7 items: %v", len(seen), order)
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] < order[i] { // newest first, ties broken by id DESC
			t.Fatalf("order not id DESC on a created_at tie: %v", order)
		}
	}
	if _, err := s.svc.List(ctx, tenant, acc1(), "pool", "", 3, "not-a-cursor"); err == nil {
		t.Fatalf("a malformed cursor was accepted")
	} else {
		wantItemCode(t, err, "WORK_ITEM_CURSOR_INVALID")
	}
}

// TestWorkItems_ConfigGuards (B6 parity): the config-time validator refuses what the runtime would
// turn into a task nobody can finish — a queue that does not exist, a required field that is not on
// the entity, required fields on an approval node, an unknown assignee type.
func TestWorkItems_ConfigGuards(t *testing.T) {
	s := wiWire(t)
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wiguard")
	cases := []struct {
		name  string
		node  ApprovalNodeInput
		code  string
		field string
	}{
		{"queue that does not exist", taskNode("a", "x", "queue", uuid.NewString()), "APPROVAL_ASSIGNEE_NOT_FOUND", "graph.nodes[0].approverId"},
		{"required field not on the entity", taskNode("a", "x", "user", seatAp1, "no_such_field"), "APPROVAL_REQUIRED_FIELD_UNKNOWN", "graph.nodes[0].requiredFields"},
		{"required fields on an approval node", ApprovalNodeInput{Kind: "approval", RequiredFields: []string{"doc_date"},
			ApprovalStepInput: ApprovalStepInput{ApproverType: "user", ApproverID: seatAp1}}, "APPROVAL_STEP_INVALID", "graph.nodes[0].requiredFields"},
		{"blank required field", taskNode("a", "x", "user", seatAp1, " "), "APPROVAL_STEP_INVALID", "graph.nodes[0].requiredFields"},
		{"duplicate required field", taskNode("a", "x", "user", seatAp1, "doc_date", "doc_date"), "APPROVAL_STEP_INVALID", "graph.nodes[0].requiredFields"},
		{"team is not an assignee yet", taskNode("a", "x", "team", seatAp1), "APPROVAL_STEP_INVALID", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.approval.CreateProcess(context.Background(), tenant, CreateApprovalProcessInput{
				Name: "guard-" + tc.name, EntityName: "wiguard", Trigger: "after_create", Graph: graphNodes(tc.node),
				IsActive: boolPtr(true), ActorID: seatSubmitter,
			})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.code || (tc.field != "" && ve.Field != tc.field) {
				t.Fatalf("err = %v (%+v); want %s at %q", err, ve, tc.code, tc.field)
			}
		})
	}
	// A linear step never names a queue (the linear v1 shape stays closed).
	_, err := s.approval.CreateProcess(context.Background(), tenant, CreateApprovalProcessInput{
		Name: "linear-queue", EntityName: "wiguard", Trigger: "after_create",
		Steps: []ApprovalStepInput{{ApproverType: "queue", ApproverID: uuid.NewString()}}, IsActive: boolPtr(true), ActorID: seatSubmitter,
	})
	wantItemCode(t, err, "APPROVAL_STEP_INVALID")
}

// TestWorkItems_BreachNoticeGoesToTheClaimer: an overdue pool item pings the claimer alone; before
// the claim it pings every holder of the role (S3c job + ResolveSeatUsers).
func TestWorkItems_BreachNoticeGoesToTheClaimer(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	tenant, item := firstPoolItem(t, s, "wibreach")
	if _, err := s.sdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'ke_toan', 'Kế toán')`, wiRoleAcc, tenant); err != nil {
		t.Fatalf("role: %v", err)
	}
	for _, u := range []string{wiUserAcc1, wiUserAcc2} {
		if _, err := s.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id) VALUES (?, ?, ?, 'x', 'active', ?)`,
			u, tenant, u+"@wi.test", wiRoleAcc); err != nil {
			t.Fatalf("user: %v", err)
		}
	}
	seats := func() []bpm.Seat {
		open, err := s.repo.ListOpenSeats(ctx, tenant, item.InstanceID)
		if err != nil {
			t.Fatalf("seats: %v", err)
		}
		return open
	}
	users, err := s.repo.ResolveSeatUsers(ctx, tenant, seats(), 50)
	if err != nil || len(users) != 2 {
		t.Fatalf("holders before the claim = %v (%v); want both", users, err)
	}
	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc2()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	users, err = s.repo.ResolveSeatUsers(ctx, tenant, seats(), 50)
	if err != nil || len(users) != 1 || users[0] != wiUserAcc2 {
		t.Fatalf("holders after the claim = %v (%v); want the claimer only", users, err)
	}
}

// TestWorkItems_OverdueCountSkipsPausedClock: the home card's "quá hạn" number is the list's —
// an item whose instance has its SLA clock paused is not running late, so it is not counted (its
// due_at is the value from before the pause).
func TestWorkItems_OverdueCountSkipsPausedClock(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wioverdue")
	s.wiProcess(t, tenant, "wioverdue", taskNode("check", "Kiểm tra", "user", seatAp1))
	s.wiRecords(t, tenant, "wioverdue", 1)
	s.drain(t)
	ap1 := WorkItemActor{UserID: seatAp1}
	if _, err := s.sdb.Exec(`UPDATE sys_bpm_tasks SET due_at = ? WHERE tenant_id = ? AND status = 'open'`,
		time.Now().UTC().Add(-time.Hour), tenant); err != nil {
		t.Fatalf("make the seat late: %v", err)
	}
	if c, err := s.svc.Counts(ctx, tenant, ap1); err != nil || c.Mine != 1 || c.Overdue != 1 {
		t.Fatalf("counts of a late item = %+v (%v); want mine 1, overdue 1", c, err)
	}
	if _, err := s.sdb.Exec(`UPDATE sys_approval_instances SET sla_paused_at = ? WHERE tenant_id = ? AND status = 'pending'`,
		time.Now().UTC(), tenant); err != nil {
		t.Fatalf("pause the clock: %v", err)
	}
	c, err := s.svc.Counts(ctx, tenant, ap1)
	if err != nil || c.Mine != 1 || c.Overdue != 0 {
		t.Fatalf("counts with the clock paused = %+v (%v); want mine 1, overdue 0", c, err)
	}
	page, err := s.svc.List(ctx, tenant, ap1, "mine", "", 10, "")
	if err != nil || len(page.Items) != 1 || !page.Items[0].Paused {
		t.Fatalf("list = %+v (%v); want the one item marked paused", page, err)
	}
}

// TestApprovalRoutes_QueueSeatNeedsEntityView: the rule the work-item routes apply to a queue seat
// — the queue's role AND view access on the record's entity — holds on the approval surface too.
// A holder of the queue's role who cannot view the entity does not see the seat in
// /approvals/pending, gets the detail's 404, and can neither approve, reject nor delegate it; with
// the view granted back every one of those works.
func TestApprovalRoutes_QueueSeatNeedsEntityView(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wiqapproval")
	queueID := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_work_queues (id, tenant_id, name, label, entity_name, role_id, filters)
		VALUES (?, ?, 'duyet_kt', 'Duyệt kế toán', 'wiqapproval', ?, '[]')`, queueID, tenant, wiRoleAcc); err != nil {
		t.Fatalf("queue: %v", err)
	}
	s.wiProcess(t, tenant, "wiqapproval", wiApprovalNode("kt", "Kế toán duyệt", "queue", queueID))
	s.wiRecords(t, tenant, "wiqapproval", 1)
	s.drain(t)
	acc := bpm.Actor{UserID: wiUserAcc1, RoleIDs: []string{wiRoleAcc}}

	s.acl[wiRoleAcc] = map[string]bool{"wiqapproval": true}
	pending, err := s.approval.ListPending(ctx, tenant, acc)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending with the view = %v (%v); want the queue approval", pending, err)
	}
	id := pending[0].ID
	if _, err := s.approval.GetApprovalDetail(ctx, RecordScope{}, tenant, id, acc); err != nil {
		t.Fatalf("detail with the view: %v", err)
	}

	s.acl[wiRoleAcc] = map[string]bool{}
	if pending, err := s.approval.ListPending(ctx, tenant, acc); err != nil || len(pending) != 0 {
		t.Fatalf("pending without the view = %v (%v); want none", pending, err)
	}
	_, err = s.approval.GetApprovalDetail(ctx, RecordScope{}, tenant, id, acc)
	wantItemCode(t, err, "APPROVAL_INSTANCE_NOT_FOUND")
	_, err = s.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "", acc)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED")
	_, err = s.approval.Decide(ctx, tenant, id, bpm.DecisionReject, "không", acc)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED")
	_, err = s.approval.DelegateApproval(ctx, tenant, id, wiUserAcc2, "", acc)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED")
	if st := instState(t, s.sdb, tenant, id); st.Status != string(bpm.StatusPending) {
		t.Fatalf("a refused decision moved the instance to %q", st.Status)
	}
	// An admin is not judged by the entity ACL.
	if pending, err := s.approval.ListPending(ctx, tenant, bpm.Actor{UserID: wiUserAcc1, RoleIDs: []string{wiRoleAcc}, IsAdmin: true}); err != nil || len(pending) != 1 {
		t.Fatalf("admin pending = %v (%v); want the queue approval", pending, err)
	}

	s.acl[wiRoleAcc] = map[string]bool{"wiqapproval": true}
	if _, err := s.approval.DelegateApproval(ctx, tenant, id, wiUserAcc2, "nhờ em", acc); err != nil {
		t.Fatalf("delegate with the view: %v", err)
	}
	done, err := s.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: wiUserAcc2})
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("the delegate's approval = %+v (%v); want approved", done, err)
	}
}

// racingDecider runs before() once, right before the real decision — the click of a second tab (or
// a retry) that commits between the service's checks and its decision.
type racingDecider struct {
	inner  approvalDecider
	before func()
}

func (r *racingDecider) DecideAt(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, comment string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	if b := r.before; b != nil {
		r.before = nil
		b()
	}
	return r.inner.DecideAt(ctx, tenantID, id, kind, comment, actor, expectedStep)
}

// TestWorkItemComplete_StaleClickCannotSkipTheNextTask: two consecutive tasks held by the same
// person, the second with a required field the record lacks. A completion of the FIRST that
// arrives after another completion of it already committed must not complete the SECOND (whose
// fields it never checked): it is a stale click, answered as a conflict, and the second task stays
// open.
func TestWorkItemComplete_StaleClickCannotSkipTheNextTask(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wistale")
	s.wiProcess(t, tenant, "wistale",
		taskNode("a", "Việc A", "user", seatAp1),
		taskNode("b", "Việc B", "user", seatAp1, "doc_date"))
	s.wiRecords(t, tenant, "wistale", 1)
	s.drain(t)
	ap1 := WorkItemActor{UserID: seatAp1}
	page, err := s.svc.List(ctx, tenant, ap1, "mine", "", 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].NodeID != "a" {
		t.Fatalf("mine = %+v (%v); want task A", page, err)
	}
	a := page.Items[0]
	if a.StepIndex != 0 {
		t.Fatalf("StepIndex of the first task = %d; want 0", a.StepIndex)
	}

	racing := NewWorkItemService(s.repo, &racingDecider{inner: s.decider, before: func() {
		if _, err := s.decider.DecideAt(ctx, tenant, a.InstanceID, bpm.DecisionComplete, "", ap1.bpmActor(), nil); err != nil {
			t.Errorf("the first click: %v", err)
		}
	}}, s.recordSvc, s.acl, nil)
	_, err = racing.Complete(ctx, tenant, a.ID, "", ap1)
	wantItemCode(t, err, "APPROVAL_CONFLICT")
	if st := instState(t, s.sdb, tenant, a.InstanceID); st.Status != string(bpm.StatusPending) || st.CurrentStep != 1 {
		t.Fatalf("instance after the stale click = %q step %d; want pending on task B", st.Status, st.CurrentStep)
	}
	mine, err := s.svc.List(ctx, tenant, ap1, "mine", "", 10, "")
	if err != nil || len(mine.Items) != 1 || mine.Items[0].NodeID != "b" {
		t.Fatalf("mine after = %+v (%v); want task B still open", mine, err)
	}
}

// TestWorkItems_MineDropsSeatsTheClaimerNoLongerHolds: an item claimed from a pool stays in "Của
// tôi" only while the claimer is still a candidate of the seat — the holder rule the approval inbox
// applies. A claimer who lost the role neither sees nor counts a ghost item they cannot act on.
func TestWorkItems_MineDropsSeatsTheClaimerNoLongerHolds(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	tenant, item := firstPoolItem(t, s, "wighost")
	if _, err := s.svc.Claim(ctx, tenant, item.ID, acc1()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ids := listIDs(t, s, tenant, acc1(), "mine"); len(ids) != 1 {
		t.Fatalf("mine of the claimer = %v; want the claimed item", ids)
	}
	demoted := WorkItemActor{UserID: wiUserAcc1, RoleID: wiRoleSales} // same person, role gone
	if ids := listIDs(t, s, tenant, demoted, "mine"); len(ids) != 0 {
		t.Fatalf("mine after losing the role = %v; want none", ids)
	}
	if c, err := s.svc.Counts(ctx, tenant, demoted); err != nil || c.Mine != 0 {
		t.Fatalf("counts after losing the role = %+v (%v); want mine 0", c, err)
	}
}

// TestWorkItems_StaleSeatDoesNotHideTheNextPage: an open seat left on an instance that is no longer
// pending (a pre-S3a binary's leftover) is filtered INSIDE the limited branch. Filtered after the
// LIMIT it ate one of the page's n+1 rows and the page reported "last page" with one still to come.
func TestWorkItems_StaleSeatDoesNotHideTheNextPage(t *testing.T) {
	s := wiWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	s.wiEntity(t, tenant, "wistaleseat")
	s.wiProcess(t, tenant, "wistaleseat", taskNode("check", "Kiểm tra", "role", wiRoleAcc))
	s.wiRecords(t, tenant, "wistaleseat", 4)
	s.drain(t)
	ids := listIDs(t, s, tenant, acc1(), "pool") // newest first
	if len(ids) != 4 {
		t.Fatalf("pool = %v; want 4 items", ids)
	}
	if _, err := s.sdb.Exec(`UPDATE sys_approval_instances SET status = 'rejected'
		WHERE tenant_id = ? AND id = (SELECT instance_id FROM sys_bpm_tasks WHERE tenant_id = ? AND id = ?)`, tenant, tenant, ids[0]); err != nil {
		t.Fatalf("strand the newest seat: %v", err)
	}
	p1, err := s.svc.List(ctx, tenant, acc1(), "pool", "", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1 = %d items, cursor %q (%v); want 2 items and a next page", len(p1.Items), p1.NextCursor, err)
	}
	p2, err := s.svc.List(ctx, tenant, acc1(), "pool", "", 2, p1.NextCursor)
	if err != nil || len(p2.Items) != 1 || p2.NextCursor != "" {
		t.Fatalf("page 2 = %d items, cursor %q (%v); want the last one", len(p2.Items), p2.NextCursor, err)
	}
}

func (r *racingDecider) Override(context.Context, string, string, bpm.DecisionKind, string, bpm.Actor, *int) (*bpm.Instance, error) {
	return nil, nil
}
