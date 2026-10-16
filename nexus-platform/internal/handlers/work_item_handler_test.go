package handlers

// specs/043 S5 at the HTTP seam (T095, T098): the five /work-items routes, their status codes and
// error shapes, and the record label's field-ACL. The seat/claim rules themselves are proven in
// internal/bpm (unit) and internal/services/work_item_integration_test.go (MySQL).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// fakeWIStore answers the work-item service's store seam from memory.
type fakeWIStore struct {
	items    []bpm.WorkItem
	claimErr error
	relErr   error
	lastQ    bpm.WorkItemQuery
	claimed  []string
}

func (f *fakeWIStore) ListWorkItems(_ context.Context, _ string, q bpm.WorkItemQuery) ([]bpm.WorkItem, error) {
	f.lastQ = q
	return f.items, nil
}
func (f *fakeWIStore) CountWorkItems(context.Context, string, bpm.WorkItemQuery, time.Time) (int64, int64, int64, error) {
	return 3, 2, 1, nil
}
func (f *fakeWIStore) PoolSeatEntities(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeWIStore) GetWorkItem(_ context.Context, _, id string) (*bpm.WorkItem, error) {
	for i := range f.items {
		if f.items[i].ID == id {
			it := f.items[i]
			return &it, nil
		}
	}
	return nil, bpm.ErrSeatNotFound
}
func (f *fakeWIStore) ClaimSeat(_ context.Context, _, id, user string, _ []string) error {
	f.claimed = append(f.claimed, id+":"+user)
	return f.claimErr
}
func (f *fakeWIStore) ReleaseSeat(context.Context, string, string, string, []string, bool) error {
	return f.relErr
}

type spyWIDecider struct {
	kind bpm.DecisionKind
	note string
	hits int
}

func (s *spyWIDecider) DecideAt(_ context.Context, _, id string, kind bpm.DecisionKind, comment string, _ bpm.Actor, _ *int) (*bpm.Instance, error) {
	s.kind, s.note = kind, comment
	s.hits++
	return &bpm.Instance{ID: id, Status: bpm.StatusPending, CurrentStep: 1}, nil
}

type wiRecords struct{ data map[string]any }

func (r wiRecords) Get(context.Context, services.RecordScope, string, string, string, []string, []string) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: "rec-1", Data: r.data}, nil
}
func (wiRecords) AuthorizeView(_ context.Context, _ string, sc services.RecordScope, _ string) (services.RecordScope, error) {
	return sc, nil
}

type allowACL struct{}

func (allowACL) Authorize(context.Context, string, string, string, string) (bool, string, error) {
	return true, "all", nil
}

func poolItem(id string) bpm.WorkItem {
	return bpm.WorkItem{
		ID: id, InstanceID: "inst-" + id, ProcessName: "Duyệt báo giá", EntityName: "crm_quotes", RecordID: "rec-1",
		NodeID: "check", NodeName: "Kiểm tra chứng từ", Kind: bpm.SeatKindTask, Status: "open",
		AssigneeType: bpm.ApproverRole, AssigneeID: "role-acc", AssigneeName: "Kế toán",
		CreatedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), RequiredFields: []string{"doc_date"},
		Seat: bpm.Seat{NodeID: "check", AssigneeType: bpm.ApproverRole, AssigneeID: "role-acc", Kind: bpm.SeatKindTask},
	}
}

func newWIRouter(store *fakeWIStore, dec *spyWIDecider, records wiRecords, meta *fakeApprovalMeta, labels *fakeApprovalLabels, hidden fieldViewChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := services.NewWorkItemService(store, dec, records, allowACL{}, zap.NewNop())
	h := NewWorkItemHandler(svc, zap.NewNop())
	if meta != nil {
		h.WithRecordLabels(meta, labels)
	}
	if hidden != nil {
		h.WithFieldACL(hidden)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "duyet1", TenantID: "t-1", RoleID: "role-acc"},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterUserRoutes(r.Group(""))
	return r
}

func doWI(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func bodyOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

// TestWorkItemHandler_ListShapeAndQuery: GET /work-items answers {data:{items, nextCursor}} in
// camelCase with names, passes the caller's identity and the view/entity/limit down, and refuses a
// view it does not know with 422; /work-items/counts is its own route, not :id.
func TestWorkItemHandler_ListShapeAndQuery(t *testing.T) {
	store := &fakeWIStore{items: []bpm.WorkItem{poolItem("w1")}}
	r := newWIRouter(store, &spyWIDecider{}, wiRecords{}, nil, nil, nil)

	w := doWI(r, http.MethodGet, "/work-items?view=pool&entity=crm_quotes&limit=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	data := bodyOf(t, w)["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 || data["nextCursor"] != nil {
		t.Fatalf("data = %v; want one item and a null cursor", data)
	}
	it := items[0].(map[string]any)
	for _, k := range []string{"id", "instanceId", "processName", "nodeName", "kind", "assigneeName", "claimedByMe", "overdue", "paused", "requiredFields", "entityLabel", "recordLabel"} {
		if _, ok := it[k]; !ok {
			t.Errorf("item lacks key %q: %v", k, it)
		}
	}
	if it["assigneeName"] != "Kế toán" || it["nodeName"] != "Kiểm tra chứng từ" || it["kind"] != "task" {
		t.Errorf("item = %v; want names, not codes", it)
	}
	if store.lastQ.View != bpm.ViewPool || store.lastQ.UserID != "duyet1" || store.lastQ.Entity != "crm_quotes" || store.lastQ.Limit != 5 ||
		len(store.lastQ.RoleIDs) != 1 || store.lastQ.RoleIDs[0] != "role-acc" {
		t.Errorf("query = %+v; want the caller's identity and the filters", store.lastQ)
	}

	if w := doWI(r, http.MethodGet, "/work-items?view=bogus", ""); w.Code != http.StatusUnprocessableEntity ||
		bodyOf(t, w)["code"] != "WORK_ITEM_VIEW_INVALID" {
		t.Errorf("bad view = %d %s; want 422 WORK_ITEM_VIEW_INVALID", w.Code, w.Body.String())
	}
	if w := doWI(r, http.MethodGet, "/work-items?cursor=%21%21", ""); w.Code != http.StatusBadRequest ||
		bodyOf(t, w)["code"] != "WORK_ITEM_CURSOR_INVALID" {
		t.Errorf("bad cursor = %d %s; want 400 WORK_ITEM_CURSOR_INVALID", w.Code, w.Body.String())
	}

	w = doWI(r, http.MethodGet, "/work-items/counts", "")
	counts := bodyOf(t, w)["data"].(map[string]any)
	if w.Code != http.StatusOK || counts["mine"] != float64(3) || counts["pool"] != float64(2) || counts["overdue"] != float64(1) {
		t.Errorf("counts = %d %v; want {mine:3, pool:2, overdue:1}", w.Code, counts)
	}
}

// TestWorkItemHandler_VerbStatuses: claim / release map the repository's refusals to the codes the
// plan promises — 409 taken or already ended, 404 not yours, 403 not the claimer, 422 not a pool
// item / not claimed — and a success returns the item.
func TestWorkItemHandler_VerbStatuses(t *testing.T) {
	cases := []struct {
		name   string
		verb   string
		claim  error
		rel    error
		status int
		code   string
	}{
		{"claim ok", "claim", nil, nil, 200, ""},
		{"claim taken", "claim", bpm.ErrSeatClaimed, nil, 409, "WORK_ITEM_ALREADY_CLAIMED"},
		{"claim of a finished item", "claim", bpm.ErrSeatNotOpen, nil, 409, "WORK_ITEM_NOT_OPEN"},
		{"claim of a user's own item", "claim", bpm.ErrSeatNotClaimable, nil, 422, "WORK_ITEM_NOT_CLAIMABLE"},
		{"claim of a stranger's item", "claim", bpm.ErrSeatNotFound, nil, 404, "WORK_ITEM_NOT_FOUND"},
		{"release ok", "release", nil, nil, 200, ""},
		{"release by a candidate who is not the claimer", "release", nil, bpm.ErrNotClaimer, 403, "WORK_ITEM_RELEASE_FORBIDDEN"},
		{"release of an unclaimed item", "release", nil, bpm.ErrSeatNotClaimed, 422, "WORK_ITEM_NOT_CLAIMED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeWIStore{items: []bpm.WorkItem{poolItem("w1")}, claimErr: tc.claim, relErr: tc.rel}
			r := newWIRouter(store, &spyWIDecider{}, wiRecords{}, nil, nil, nil)
			w := doWI(r, http.MethodPost, "/work-items/w1/"+tc.verb, "")
			if w.Code != tc.status {
				t.Fatalf("%s = %d %s; want %d", tc.name, w.Code, w.Body.String(), tc.status)
			}
			if tc.code != "" && bodyOf(t, w)["code"] != tc.code {
				t.Fatalf("code = %v; want %s", bodyOf(t, w)["code"], tc.code)
			}
			if tc.status == 200 && bodyOf(t, w)["data"].(map[string]any)["workItem"] == nil {
				t.Fatalf("success carries no workItem: %s", w.Body.String())
			}
		})
	}
	// An id that is not in the tenant at all is the stranger's answer.
	r := newWIRouter(&fakeWIStore{}, &spyWIDecider{}, wiRecords{}, nil, nil, nil)
	if w := doWI(r, http.MethodPost, "/work-items/nope/claim", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d; want 404", w.Code)
	}
}

// TestWorkItemHandler_CompleteRequiredFields422: a blank required field is 422 with the field
// NAMES in details.fields and the engine untouched; filled, the note reaches the engine as a
// complete decision.
func TestWorkItemHandler_CompleteRequiredFields422(t *testing.T) {
	store := &fakeWIStore{items: []bpm.WorkItem{poolItem("w1")}}
	dec := &spyWIDecider{}
	r := newWIRouter(store, dec, wiRecords{data: map[string]any{"doc_date": "  "}}, nil, nil, nil)
	w := doWI(r, http.MethodPost, "/work-items/w1/complete", `{"note":"xong"}`)
	body := bodyOf(t, w)
	details, _ := body["details"].(map[string]any)
	fields, _ := details["fields"].([]any)
	if w.Code != http.StatusUnprocessableEntity || body["code"] != "WORK_ITEM_REQUIRED_FIELDS" || len(fields) != 1 || fields[0] != "doc_date" {
		t.Fatalf("blank field = %d %s; want 422 WORK_ITEM_REQUIRED_FIELDS details.fields [doc_date]", w.Code, w.Body.String())
	}
	if dec.hits != 0 {
		t.Fatalf("the engine was called despite the missing field")
	}

	r = newWIRouter(store, dec, wiRecords{data: map[string]any{"doc_date": "2026-10-08"}}, nil, nil, nil)
	w = doWI(r, http.MethodPost, "/work-items/w1/complete", `{"note":"đủ chứng từ"}`)
	if w.Code != http.StatusOK || dec.kind != bpm.DecisionComplete || dec.note != "đủ chứng từ" {
		t.Fatalf("complete = %d %s kind=%s note=%q; want a complete decision with the note", w.Code, w.Body.String(), dec.kind, dec.note)
	}

	// An approval seat is decided on /approvals, not completed here.
	appr := poolItem("w2")
	appr.Kind, appr.RequiredFields = bpm.SeatKindApproval, nil
	r = newWIRouter(&fakeWIStore{items: []bpm.WorkItem{appr}}, dec, wiRecords{}, nil, nil, nil)
	w = doWI(r, http.MethodPost, "/work-items/w2/complete", "")
	if w.Code != http.StatusUnprocessableEntity || bodyOf(t, w)["code"] != "WORK_ITEM_NOT_COMPLETABLE" {
		t.Fatalf("complete of an approval = %d %s; want 422 WORK_ITEM_NOT_COMPLETABLE", w.Code, w.Body.String())
	}
}

// TestWorkItems_FieldACLFiltersRecordFields (T095): the record label on a work item is read off a
// field of the record. A role that may not view that field gets an empty label — the value is
// never fetched — while an admin and a role that may view it get the label.
func TestWorkItems_FieldACLFiltersRecordFields(t *testing.T) {
	labelOf := func(hidden fieldViewChecker) (string, bool) {
		labels := &fakeApprovalLabels{byID: map[string]string{"rec-1": "Báo giá ABC"}}
		meta := &fakeApprovalMeta{
			defs:   map[string]*metadata.EntityDefinition{"crm_quotes": {ID: "e1", Name: "crm_quotes", Label: "Báo giá"}},
			fields: map[string][]metadata.FieldDefinition{"e1": {{Name: "name", FieldType: "text"}}},
		}
		r := newWIRouter(&fakeWIStore{items: []bpm.WorkItem{poolItem("w1")}}, &spyWIDecider{}, wiRecords{}, meta, labels, hidden)
		w := doWI(r, http.MethodGet, "/work-items?view=pool", "")
		it := bodyOf(t, w)["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
		return it["recordLabel"].(string), labels.fetchCalls > 0
	}
	if got, fetched := labelOf(fixedHidden{hidden: map[string]struct{}{"name": {}}}); got != "" || fetched {
		t.Errorf("hidden label field: label=%q fetched=%v; want empty and never fetched", got, fetched)
	}
	if got, fetched := labelOf(fixedHidden{hidden: map[string]struct{}{}}); got != "Báo giá ABC" || !fetched {
		t.Errorf("visible label field: label=%q fetched=%v; want the label", got, fetched)
	}
}
