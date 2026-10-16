// VIEC-HOM-NAY unit tests — the pure helpers (deep-link building + operand splitting). The
// validation matrix + ACL-scoped counting run against real MySQL in workqueue_integration_test.go.
package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workqueue"
)

func TestQueueHref(t *testing.T) {
	q := workqueue.Queue{
		EntityName: "tk_tickets",
		ActionVerb: "open",
		Filters: []workqueue.QueueFilter{
			{Field: "status", Operator: "in", Value: "Open,Pending"},
			{Field: "due_date", Operator: "lte", Value: "today"},
		},
	}
	href := queueHref(q)
	// Bracket syntax the FE's parseUrlFilters understands; tokens ride through verbatim.
	if href != "/tk_tickets?"+"due_date%5Blte%5D=today&status%5Bin%5D=Open%2CPending" {
		t.Fatalf("href = %q", href)
	}

	q.ActionVerb = "approve"
	if queueHref(q) != "/approvals" {
		t.Fatalf("approve href = %q", queueHref(q))
	}

	if queueHref(workqueue.Queue{EntityName: "crm_leads", ActionVerb: "open"}) != "/crm_leads" {
		t.Fatal("no-filter href should be the bare list")
	}

	// Review follow-up: sort_field must not be an inert knob — it rides the deep-link so the
	// landing list opens sorted (LayoutList consumes ?sort=&order= from the URL).
	sf := "due_date"
	sorted := workqueue.Queue{EntityName: "tk_tickets", ActionVerb: "open", SortField: &sf, SortOrder: "desc"}
	if got := queueHref(sorted); got != "/tk_tickets?order=desc&sort=due_date" {
		t.Fatalf("sorted href = %q", got)
	}
}

func TestFilterValuesOf(t *testing.T) {
	if got := filterValuesOf(" Open , Pending ", dynrepo.FilterIn); len(got) != 2 || got[0] != "Open" || got[1] != "Pending" {
		t.Fatalf("in split = %v", got)
	}
	if got := filterValuesOf("2026-01-01,2026-12-31", dynrepo.FilterBetween); len(got) != 2 {
		t.Fatalf("between split = %v", got)
	}
	if got := filterValuesOf("x", dynrepo.FilterEq); len(got) != 1 || got[0] != "x" {
		t.Fatalf("eq = %v", got)
	}
	if got := filterValuesOf("", dynrepo.FilterEq); got != nil {
		t.Fatalf("empty eq = %v", got)
	}
	if got := filterValuesOf("anything", dynrepo.FilterIsNull); got != nil {
		t.Fatalf("isnull must carry no operands: %v", got)
	}
}

// fakeQueueStore serves a fixed set of active queues to ListForUser.
type fakeQueueStore struct{ active []workqueue.Queue }

func (f fakeQueueStore) Create(context.Context, *workqueue.Queue) error         { return nil }
func (f fakeQueueStore) Update(context.Context, string, *workqueue.Queue) error { return nil }
func (f fakeQueueStore) SoftDelete(context.Context, string, string) error       { return nil }
func (f fakeQueueStore) GetByID(context.Context, string, string) (*workqueue.Queue, error) {
	return nil, workqueue.ErrNotFound
}
func (f fakeQueueStore) ListAll(context.Context, string) ([]workqueue.Queue, error) {
	return f.active, nil
}
func (f fakeQueueStore) ListActive(context.Context, string) ([]workqueue.Queue, error) {
	return f.active, nil
}

type fakeSeatCounter struct {
	n    int64
	err  error
	seen [3]string // tenant, queue, entity
}

func (f *fakeSeatCounter) CountPoolSeats(_ context.Context, tenantID, queueID, entity string) (int64, error) {
	f.seen = [3]string{tenantID, queueID, entity}
	return f.n, f.err
}

// TestWorkQueue_ClaimVerbCounts (specs/043 S5, T096): a queue whose verb is "claim" is a valid
// config, links to the pool of work items waiting to be claimed, and its card number is the count
// of seats in ITS pool on ITS entity — read for the caller's tenant — not a record count. With no
// seat counter wired the number is null (the card renders "—"), never a made-up zero.
func TestWorkQueue_ClaimVerbCounts(t *testing.T) {
	in := &WorkQueueInput{Name: "cho_nhan", Label: "Chờ nhận", EntityName: "crm_quotes", ActionVerb: "claim"}
	if err := ValidateWorkQueueConfig(in, func(string) ([]FieldSpec, error) { return nil, nil }); err != nil {
		t.Fatalf("a claim queue was refused at save time: %v", err)
	}

	if got := queueHref(workqueue.Queue{EntityName: "crm_quotes", ActionVerb: "claim"}); got != "/approvals?view=pool" {
		t.Fatalf("claim href = %q; want the pool of the inbox", got)
	}

	q := workqueue.Queue{ID: "q1", Name: "cho_nhan", Label: "Chờ nhận", EntityName: "crm_quotes", ActionVerb: "claim"}
	store := fakeQueueStore{active: []workqueue.Queue{q}}
	allow := wqACLAllow{}
	counter := &fakeSeatCounter{n: 4}
	svc := NewWorkQueueService(store, nil, allow, nil, nil).WithSeatCounter(counter)
	views, err := svc.ListForUser(context.Background(), RecordScope{UserID: "u1", RoleID: "r1"}, "t1")
	if err != nil || len(views) != 1 || views[0].Count == nil || *views[0].Count != 4 {
		t.Fatalf("views = %+v (%v); want one card counting 4 waiting items", views, err)
	}
	if counter.seen != [3]string{"t1", "q1", "crm_quotes"} {
		t.Fatalf("counted %v; want the caller's tenant, this queue, its entity", counter.seen)
	}
	if views[0].Href != "/approvals?view=pool" || views[0].ActionVerb != "claim" {
		t.Fatalf("card = %+v", views[0])
	}

	// A claim queue counts seats, not records: a filter / sort would be silently ignored, so it is refused.
	for name, mut := range map[string]func(*WorkQueueInput){
		"filter": func(in *WorkQueueInput) {
			in.Filters = []workqueue.QueueFilter{{Field: "total", Operator: "gt", Value: "10000000"}}
		},
		"sort field": func(in *WorkQueueInput) { in.SortField = "created_at" },
		"sort order": func(in *WorkQueueInput) { in.SortOrder = "desc" },
	} {
		bad := &WorkQueueInput{Name: "cho_nhan", Label: "Chờ nhận", EntityName: "crm_quotes", ActionVerb: "claim"}
		mut(bad)
		err := ValidateWorkQueueConfig(bad, func(string) ([]FieldSpec, error) { return []FieldSpec{{Name: "total", FieldType: "number"}}, nil })
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "WORKQUEUE_CLAIM_NO_FILTER" {
			t.Errorf("claim queue with a %s: err = %v; want WORKQUEUE_CLAIM_NO_FILTER", name, err)
		}
	}

	// No counter wired → null, not 0.
	views, err = NewWorkQueueService(store, nil, allow, nil, nil).ListForUser(context.Background(), RecordScope{UserID: "u1", RoleID: "r1"}, "t1")
	if err != nil || len(views) != 1 || views[0].Count != nil {
		t.Fatalf("no counter: views = %+v (%v); want a card with a null count", views, err)
	}
}

type wqACLAllow struct{}

func (wqACLAllow) Authorize(context.Context, string, string, string, string) (bool, string, error) {
	return true, "all", nil
}
