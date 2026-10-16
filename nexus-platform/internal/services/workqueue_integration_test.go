//go:build integration

// VIEC-HOM-NAY integration (../specs/012-work-queues/spec.md §8) on real MySQL: config-time validation parity
// (bad entity/field/op/token/sort/action rejected at SAVE time with the queue codes), and the
// end-user listing — visibility by role, ACL fail-closed hiding, and live counts that honor
// relative-date tokens under the caller's scope.
package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workqueue"
)

// wqRoles / wqACL are tiny fakes for the seams the harness does not exercise end-to-end here
// (role existence + module ACL); row-scope + counting run the REAL record engine.
type wqRoles struct{ known map[string]bool }

func (r wqRoles) RoleExists(_ context.Context, _, id string) (bool, error) { return r.known[id], nil }

type wqACL struct{ allow map[string]string } // entity → access level ("" = deny)

func (a wqACL) Authorize(_ context.Context, _, _, entity, _ string) (bool, string, error) {
	lvl, ok := a.allow[entity]
	return ok && lvl != "", lvl, nil
}

func wqSetup(t *testing.T) (*WorkQueueService, *RecordService, *EntityService, *workqueue.Repository, wqACL) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo)
	store := workqueue.New(gdb)
	acl := wqACL{allow: map[string]string{}}
	svc := NewWorkQueueService(store, recordSvc, acl, wqRoles{known: map[string]bool{"role-sales": true}}, zap.NewNop())
	return svc, recordSvc, entitySvc, store, acl
}

const wqTenant = "aaaaaaaa-1a1a-1a1a-1a1a-aaaaaaaaaaaa"
const wqUser = "00000000-0000-0000-0000-000000000001"

func mustCreateTicketEntity(t *testing.T, entitySvc *EntityService) {
	t.Helper()
	_, _, err := entitySvc.CreateEntity(context.Background(), wqTenant, CreateEntityInput{
		Name: "wq_tickets", Label: "Tickets",
		Fields: []FieldInput{
			{Name: "subject", Label: "Subject", FieldType: "text", IsRequired: true},
			{Name: "status", Label: "Status", FieldType: "picklist", Options: map[string]any{"values": []any{"Open", "Done"}}},
			{Name: "due_date", Label: "Due", FieldType: "date"},
		},
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
}

func TestWorkQueue_ValidationParity(t *testing.T) {
	svc, _, entitySvc, _, _ := wqSetup(t)
	mustCreateTicketEntity(t, entitySvc)
	ctx := context.Background()

	base := WorkQueueInput{Name: "q1", Label: "Q1", EntityName: "wq_tickets"}

	cases := []struct {
		name string
		mut  func(WorkQueueInput) WorkQueueInput
		code string
	}{
		{"unknown entity", func(in WorkQueueInput) WorkQueueInput { in.EntityName = "nope"; return in }, "WORKQUEUE_ENTITY_NOT_FOUND"},
		{"unknown field", func(in WorkQueueInput) WorkQueueInput {
			in.Filters = []workqueue.QueueFilter{{Field: "ghost", Operator: "eq", Value: "x"}}
			return in
		}, "WORKQUEUE_FILTER_INVALID"},
		{"bad relative token", func(in WorkQueueInput) WorkQueueInput {
			in.Filters = []workqueue.QueueFilter{{Field: "due_date", Operator: "lte", Value: "ngay-mai-kia"}}
			return in
		}, "WORKQUEUE_FILTER_INVALID"},
		{"op not allowed for type", func(in WorkQueueInput) WorkQueueInput {
			in.Filters = []workqueue.QueueFilter{{Field: "subject", Operator: "gt", Value: "5"}}
			return in
		}, "WORKQUEUE_FILTER_INVALID"},
		{"unsortable sort", func(in WorkQueueInput) WorkQueueInput { in.SortField = "subject"; return in }, "WORKQUEUE_SORT_NOT_SORTABLE"},
		{"bad action", func(in WorkQueueInput) WorkQueueInput { in.ActionVerb = "delete_all"; return in }, "WORKQUEUE_ACTION_INVALID"},
		{"claim queue with a filter", func(in WorkQueueInput) WorkQueueInput {
			in.ActionVerb = "claim"
			in.Filters = []workqueue.QueueFilter{{Field: "status", Operator: "eq", Value: "Open"}}
			return in
		}, "WORKQUEUE_CLAIM_NO_FILTER"},
		{"claim queue with a sort", func(in WorkQueueInput) WorkQueueInput {
			in.ActionVerb, in.SortField = "claim", "created_at"
			return in
		}, "WORKQUEUE_CLAIM_NO_FILTER"},
		{"claim queue with a descending order", func(in WorkQueueInput) WorkQueueInput {
			in.ActionVerb, in.SortOrder = "claim", "desc"
			return in
		}, "WORKQUEUE_CLAIM_NO_FILTER"},
		{"unknown role", func(in WorkQueueInput) WorkQueueInput { in.RoleID = "role-ghost"; return in }, "WORKQUEUE_ROLE_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, wqTenant, tc.mut(base))
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}

	// A VALID queue with a token filter + system sort saves fine.
	ok := base
	ok.Filters = []workqueue.QueueFilter{{Field: "due_date", Operator: "lte", Value: "today"}, {Field: "status", Operator: "eq", Value: "Open"}}
	ok.SortField = "created_at"
	ok.RoleID = "role-sales"
	if _, err := svc.Create(ctx, wqTenant, ok); err != nil {
		t.Fatalf("valid queue rejected: %v", err)
	}

	// The "claim" verb (specs/043 S5) saves like any other action - and a typo still does not.
	claim := base
	claim.Name, claim.ActionVerb = "q-claim", "claim"
	if _, err := svc.Create(ctx, wqTenant, claim); err != nil {
		t.Fatalf("claim queue rejected: %v", err)
	}
	// Duplicate name → 409 code.
	if _, err := svc.Create(ctx, wqTenant, ok); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestWorkQueue_ListForUser_VisibilityACLAndCounts(t *testing.T) {
	svc, recordSvc, entitySvc, store, acl := wqSetup(t)
	mustCreateTicketEntity(t, entitySvc)
	ctx := context.Background()

	// Seed: 2 open tickets due yesterday/today, 1 done, 1 open due next month.
	seed := func(subject, status, due string) {
		t.Helper()
		if _, err := recordSvc.Create(ctx, RecordScope{UserID: wqUser, AccessLevel: string(dynrepo.AccessAll), IsAdmin: true},
			wqTenant, wqUser, "wq_tickets", map[string]any{"subject": subject, "status": status, "due_date": due}); err != nil {
			t.Fatalf("seed %s: %v", subject, err)
		}
	}
	today := time.Now().In(appLocation()).Format("2006-01-02")
	seed("t1", "Open", today)
	seed("t2", "Open", time.Now().In(appLocation()).AddDate(0, 0, -1).Format("2006-01-02"))
	seed("t3", "Done", today)
	seed("t4", "Open", time.Now().In(appLocation()).AddDate(0, 1, 3).Format("2006-01-02"))

	mk := func(name, roleID string) {
		t.Helper()
		in := WorkQueueInput{
			Name: name, Label: name, EntityName: "wq_tickets", RoleID: roleID,
			Filters: []workqueue.QueueFilter{
				{Field: "status", Operator: "eq", Value: "Open"},
				{Field: "due_date", Operator: "lte", Value: "today"},
			},
		}
		if _, err := svc.Create(ctx, wqTenant, in); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	mk("overdue_all", "")             // visible to everyone
	mk("overdue_sales", "role-sales") // only role-sales (or admins)

	// Non-admin, role-sales, entity allowed at level "all" → sees BOTH queues, count = 2
	// (t1 today + t2 yesterday; t3 done + t4 future excluded by the queue's own filters).
	acl.allow["wq_tickets"] = string(dynrepo.AccessAll)
	views, err := svc.ListForUser(ctx, RecordScope{UserID: wqUser, RoleID: "role-sales", AccessLevel: ""}, wqTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 queues, got %d (%+v)", len(views), views)
	}
	for _, v := range views {
		if v.Count == nil || *v.Count != 2 {
			t.Fatalf("queue %s count = %v, want 2", v.Name, v.Count)
		}
		if v.Href == "" || v.ActionVerb != "open" {
			t.Fatalf("queue %s missing href/action: %+v", v.Name, v)
		}
	}

	// Different role → the role-scoped queue is hidden.
	views, err = svc.ListForUser(ctx, RecordScope{UserID: wqUser, RoleID: "role-other"}, wqTenant)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(views) != 1 || views[0].Name != "overdue_all" {
		t.Fatalf("role filter broken: %+v", views)
	}

	// Entity NOT authorized → every queue on it silently disappears (fail-closed, no leak).
	delete(acl.allow, "wq_tickets")
	views, err = svc.ListForUser(ctx, RecordScope{UserID: wqUser, RoleID: "role-sales"}, wqTenant)
	if err != nil {
		t.Fatalf("list denied: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("ACL hide broken: %+v", views)
	}

	// Admin bypasses both role and ACL gates.
	views, err = svc.ListForUser(ctx, RecordScope{UserID: wqUser, IsAdmin: true}, wqTenant)
	if err != nil || len(views) != 2 {
		t.Fatalf("admin view: %v %+v", err, views)
	}

	// Broken queue config (field dropped after save) → count null, card still returned.
	q3 := WorkQueueInput{Name: "broken", Label: "Broken", EntityName: "wq_tickets",
		Filters: []workqueue.QueueFilter{{Field: "status", Operator: "eq", Value: "Open"}}}
	created, err := svc.Create(ctx, wqTenant, q3)
	if err != nil {
		t.Fatalf("create broken: %v", err)
	}
	// Sabotage the stored filters directly (bypasses validation, simulating schema drift).
	created.Filters = []workqueue.QueueFilter{{Field: "ghost", Operator: "eq", Value: "x"}}
	if err := store.Update(ctx, wqTenant, created); err != nil {
		t.Fatalf("sabotage: %v", err)
	}
	views, err = svc.ListForUser(ctx, RecordScope{UserID: wqUser, IsAdmin: true}, wqTenant)
	if err != nil {
		t.Fatalf("list with broken: %v", err)
	}
	var broken *QueueView
	for i := range views {
		if views[i].Name == "broken" {
			broken = &views[i]
		}
	}
	if broken == nil || broken.Count != nil {
		t.Fatalf("broken queue should surface with null count: %+v", broken)
	}
}
