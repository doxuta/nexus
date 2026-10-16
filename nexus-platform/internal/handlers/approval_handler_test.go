package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// spyDecider captures the comment threaded into the synchronous approve/reject path so the handler
// test can assert the FE "note" body key reaches the engine (W6-BE-POLISH: the FE POSTs {"note":..}
// but the handler previously bound only "comment", silently dropping the approver's note). It
// structurally satisfies the (unexported) services approvalDecider interface.
type spyDecider struct {
	gotComment      string
	gotExpectedStep *int
	ret             *bpm.Instance
}

func (s *spyDecider) DecideAt(_ context.Context, _, _ string, kind bpm.DecisionKind, comment string, _ bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	s.gotComment = comment
	s.gotExpectedStep = expectedStep
	if s.ret != nil {
		s.ret.History = []bpm.HistoryEntry{{Step: 0, Action: string(kind), Comment: comment}}
	}
	return s.ret, nil
}

// newApprovalDecideRouter wires a real ApprovalService over a spy decider (the process/instance
// repos are unused by Decide) and mounts the user approval routes with an injected admin subject.
func newApprovalDecideRouter(spy *spyDecider) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := services.NewApprovalService(nil, nil, spy)
	h := NewApprovalHandler(svc, zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "actor-1", TenantID: "t-1", IsAdmin: true},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterUserRoutes(r.Group(""))
	return r
}

func postDecision(r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestApprovalDecide_ThreadsNote drives the real FE payload shapes through the handler and asserts
// the approver note reaches the decision engine (and comes back on the returned instance history).
func TestApprovalDecide_ThreadsNote(t *testing.T) {
	tests := []struct {
		name string
		path string
		body map[string]any
		want string
	}{
		{name: "FE note key on approve", path: "/approvals/i1/approve", body: map[string]any{"note": "nhìn ổn"}, want: "nhìn ổn"},
		{name: "FE note key on reject", path: "/approvals/i1/reject", body: map[string]any{"note": "sai số"}, want: "sai số"},
		{name: "legacy comment alias", path: "/approvals/i1/approve", body: map[string]any{"comment": "cũ"}, want: "cũ"},
		{name: "note wins over comment", path: "/approvals/i1/approve", body: map[string]any{"note": "n", "comment": "c"}, want: "n"},
		{name: "empty body → no note", path: "/approvals/i1/approve", body: map[string]any{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyDecider{ret: &bpm.Instance{ID: "i1", TenantID: "t-1"}}
			r := newApprovalDecideRouter(spy)
			w := postDecision(r, tt.path, tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body %s)", w.Code, w.Body.String())
			}
			if spy.gotComment != tt.want {
				t.Fatalf("threaded comment = %q, want %q", spy.gotComment, tt.want)
			}
			// The returned instance history must carry the note as `comment` (the key the FE
			// timeline normalizes note|comment → comment renders).
			var env struct {
				Data struct {
					Approval struct {
						History []struct {
							Comment string `json:"comment"`
						} `json:"history"`
					} `json:"approval"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(env.Data.Approval.History) != 1 || env.Data.Approval.History[0].Comment != tt.want {
				t.Fatalf("response history comment = %+v, want %q", env.Data.Approval.History, tt.want)
			}
		})
	}
}

// TestHandler_RejectEmptyNote_422Code (specs/043 S2, FR-027): POST /approvals/:id/reject without a
// reason — no body, an empty note, a blank note, a blank legacy comment — is 422
// APPROVAL_REJECT_REASON_REQUIRED and never reaches the engine, whoever holds the token.
func TestHandler_RejectEmptyNote_422Code(t *testing.T) {
	for _, tt := range []struct {
		name string
		body any
	}{
		{"no body", nil},
		{"empty note", map[string]any{"note": ""}},
		{"blank note", map[string]any{"note": "   \n"}},
		{"blank legacy comment", map[string]any{"comment": "\t"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const untouched = "\x00engine-not-called"
			spy := &spyDecider{gotComment: untouched, ret: &bpm.Instance{ID: "i1", TenantID: "t-1"}}
			r := newApprovalDecideRouter(spy)
			var w *httptest.ResponseRecorder
			if tt.body == nil {
				req := httptest.NewRequest(http.MethodPost, "/approvals/i1/reject", nil)
				w = httptest.NewRecorder()
				r.ServeHTTP(w, req)
			} else {
				w = postDecision(r, "/approvals/i1/reject", tt.body)
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if w.Code != http.StatusUnprocessableEntity || env.Code != "APPROVAL_REJECT_REASON_REQUIRED" {
				t.Fatalf("got %d %s; want 422 APPROVAL_REJECT_REASON_REQUIRED", w.Code, w.Body.String())
			}
			if spy.gotComment != untouched {
				t.Fatalf("engine was called with %q — a reasonless reject must be refused before it", spy.gotComment)
			}
		})
	}
}

// ---- F7-4: GET /approvals/pending label enrichment -------------------------------------------

// fakePendingRepo satisfies the services approvalInstanceRepo slice structurally: ListPendingForUser
// returns the canned instances; the other reads are unused by ListPending.
type fakePendingRepo struct {
	rows []bpm.Instance
}

func (f *fakePendingRepo) ListPendingForUser(context.Context, string, string, []string, bpm.EntityView) ([]bpm.Instance, error) {
	return f.rows, nil
}
func (f *fakePendingRepo) GetInstance(context.Context, string, string) (*bpm.Instance, error) {
	return nil, bpm.ErrInstanceNotFound
}
func (f *fakePendingRepo) ListByRecordLane(context.Context, string, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

func (f *fakePendingRepo) ListByRecord(context.Context, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

// ListOpenSeats: this fake holds no seats — the visibility gate answers through the projection.
func (f *fakePendingRepo) ListOpenSeats(context.Context, string, string) ([]bpm.Seat, error) {
	return nil, nil
}

// fakeApprovalMeta satisfies approvalMetaReader: one entity registry keyed by name.
type fakeApprovalMeta struct {
	defs    map[string]*metadata.EntityDefinition
	fields  map[string][]metadata.FieldDefinition // entityID → fields
	metaErr error
}

func (f *fakeApprovalMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	return f.defs[name], nil
}
func (f *fakeApprovalMeta) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return f.fields[entityID], nil
}

// fakeApprovalLabels satisfies services.LabelFetcher, recording the labelField it was asked for.
type fakeApprovalLabels struct {
	byID       map[string]string
	gotField   string
	fetchCalls int
	fetchErr   error
}

func (f *fakeApprovalLabels) BatchFetchLabels(_ context.Context, _, _, labelField string, ids []string) ([]dynrepo.LabelResult, error) {
	f.fetchCalls++
	f.gotField = labelField
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	out := make([]dynrepo.LabelResult, 0, len(ids))
	for _, id := range ids {
		if l, ok := f.byID[id]; ok {
			out = append(out, dynrepo.LabelResult{ID: id, Label: l})
		}
	}
	return out, nil
}

// newPendingRouter mounts /approvals/pending over canned instances + optional label deps.
func newPendingRouter(rows []bpm.Instance, meta *fakeApprovalMeta, labels *fakeApprovalLabels) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := services.NewApprovalService(nil, &fakePendingRepo{rows: rows}, nil)
	h := NewApprovalHandler(svc, zap.NewNop())
	if meta != nil && labels != nil {
		h.WithRecordLabels(meta, labels)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "actor-1", TenantID: "t-1", IsAdmin: false, RoleID: "r-1"},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterUserRoutes(r.Group(""))
	return r
}

type pendingRowJSON struct {
	ID          string `json:"ID"`
	EntityName  string `json:"EntityName"`
	RecordID    string `json:"RecordID"`
	EntityLabel string `json:"entityLabel"`
	RecordLabel string `json:"recordLabel"`
}

func getPending(t *testing.T, r *gin.Engine) []pendingRowJSON {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/approvals/pending", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Approvals []pendingRowJSON `json:"approvals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return env.Data.Approvals
}

// TestListPending_HumanizedLabels pins F7-4: rows gain {entityLabel, recordLabel} resolved via
// the entity's own label + the shared labelField preference, while every legacy PascalCase key
// the FE parses stays intact.
func TestListPending_HumanizedLabels(t *testing.T) {
	rows := []bpm.Instance{
		{ID: "i1", TenantID: "t-1", EntityName: "crm_opportunities", RecordID: "rec-1"},
		{ID: "i2", TenantID: "t-1", EntityName: "crm_opportunities", RecordID: "rec-404"}, // label unresolvable
	}
	meta := &fakeApprovalMeta{
		defs:   map[string]*metadata.EntityDefinition{"crm_opportunities": {ID: "e1", Label: "Cơ hội"}},
		fields: map[string][]metadata.FieldDefinition{"e1": {{Name: "amount", FieldType: "currency"}, {Name: "name", FieldType: "text"}}},
	}
	labels := &fakeApprovalLabels{byID: map[string]string{"rec-1": "Hợp đồng ABC"}}
	got := getPending(t, newPendingRouter(rows, meta, labels))

	if len(got) != 2 {
		t.Fatalf("rows = %d; want 2", len(got))
	}
	// Legacy keys intact (FE fromPendingItem contract).
	if got[0].ID != "i1" || got[0].EntityName != "crm_opportunities" || got[0].RecordID != "rec-1" {
		t.Errorf("legacy PascalCase keys broken: %+v", got[0])
	}
	if got[0].EntityLabel != "Cơ hội" || got[0].RecordLabel != "Hợp đồng ABC" {
		t.Errorf("row 0 labels = %q/%q; want Cơ hội/Hợp đồng ABC", got[0].EntityLabel, got[0].RecordLabel)
	}
	// Unresolvable record id → entityLabel still set, recordLabel empty (honest fallback).
	if got[1].EntityLabel != "Cơ hội" || got[1].RecordLabel != "" {
		t.Errorf("row 1 labels = %q/%q; want Cơ hội/(empty)", got[1].EntityLabel, got[1].RecordLabel)
	}
	if labels.gotField != "name" {
		t.Errorf("labelField = %q; want name (preference list pick)", labels.gotField)
	}
	if labels.fetchCalls != 1 {
		t.Errorf("BatchFetchLabels calls = %d; want 1 (batched per entity, no N+1)", labels.fetchCalls)
	}
}

// TestListPending_LabelPreferenceOrder: an entity without "name" picks the next preferred
// field ("title" here) — mirroring the FE recordLabel tier-1 list.
func TestListPending_LabelPreferenceOrder(t *testing.T) {
	rows := []bpm.Instance{{ID: "i1", TenantID: "t-1", EntityName: "lms_courses", RecordID: "c-1"}}
	meta := &fakeApprovalMeta{
		defs:   map[string]*metadata.EntityDefinition{"lms_courses": {ID: "e2", Label: "Khoá học"}},
		fields: map[string][]metadata.FieldDefinition{"e2": {{Name: "title", FieldType: "text"}, {Name: "code", FieldType: "text"}}},
	}
	labels := &fakeApprovalLabels{byID: map[string]string{"c-1": "Thiết kế UI/UX"}}
	got := getPending(t, newPendingRouter(rows, meta, labels))
	if labels.gotField != "title" {
		t.Errorf("labelField = %q; want title", labels.gotField)
	}
	if got[0].RecordLabel != "Thiết kế UI/UX" {
		t.Errorf("recordLabel = %q; want Thiết kế UI/UX", got[0].RecordLabel)
	}
}

// TestListPending_BestEffort: every label-source failure mode still returns 200 with the rows —
// empty labels, never a failed request (the queue must render).
func TestListPending_BestEffort(t *testing.T) {
	rows := []bpm.Instance{{ID: "i1", TenantID: "t-1", EntityName: "crm_opportunities", RecordID: "rec-1"}}

	t.Run("deps not wired → empty labels", func(t *testing.T) {
		got := getPending(t, newPendingRouter(rows, nil, nil))
		if got[0].EntityLabel != "" || got[0].RecordLabel != "" {
			t.Errorf("labels = %q/%q; want empty/empty", got[0].EntityLabel, got[0].RecordLabel)
		}
	})

	t.Run("metadata read fails → empty labels", func(t *testing.T) {
		meta := &fakeApprovalMeta{metaErr: errors.New("cache down")}
		got := getPending(t, newPendingRouter(rows, meta, &fakeApprovalLabels{}))
		if got[0].EntityLabel != "" || got[0].RecordLabel != "" {
			t.Errorf("labels = %q/%q; want empty/empty", got[0].EntityLabel, got[0].RecordLabel)
		}
	})

	t.Run("label fetch fails → entityLabel kept, recordLabel empty", func(t *testing.T) {
		meta := &fakeApprovalMeta{
			defs:   map[string]*metadata.EntityDefinition{"crm_opportunities": {ID: "e1", Label: "Cơ hội"}},
			fields: map[string][]metadata.FieldDefinition{"e1": {{Name: "name", FieldType: "text"}}},
		}
		got := getPending(t, newPendingRouter(rows, meta, &fakeApprovalLabels{fetchErr: errors.New("db down")}))
		if got[0].EntityLabel != "Cơ hội" || got[0].RecordLabel != "" {
			t.Errorf("labels = %q/%q; want Cơ hội/(empty)", got[0].EntityLabel, got[0].RecordLabel)
		}
	})

	t.Run("no name-like field → fetch never attempted, labels honest-empty", func(t *testing.T) {
		meta := &fakeApprovalMeta{
			defs:   map[string]*metadata.EntityDefinition{"crm_opportunities": {ID: "e1", Label: "Cơ hội"}},
			fields: map[string][]metadata.FieldDefinition{"e1": {{Name: "amount", FieldType: "currency"}}},
		}
		labels := &fakeApprovalLabels{byID: map[string]string{"rec-1": "leak"}}
		got := getPending(t, newPendingRouter(rows, meta, labels))
		if labels.fetchCalls != 0 {
			t.Errorf("fetchCalls = %d; want 0", labels.fetchCalls)
		}
		if got[0].RecordLabel != "" {
			t.Errorf("recordLabel = %q; want empty", got[0].RecordLabel)
		}
	})
}

// TestPickApprovalLabelField covers the pure labelField pick (case-insensitive, ranked).
func TestPickApprovalLabelField(t *testing.T) {
	f := func(names ...string) []metadata.FieldDefinition {
		out := make([]metadata.FieldDefinition, len(names))
		for i, n := range names {
			out[i] = metadata.FieldDefinition{Name: n}
		}
		return out
	}
	tests := []struct {
		name   string
		fields []metadata.FieldDefinition
		opts   string
		want   string
	}{
		{name: "name wins over title", fields: f("title", "name"), want: "name"},
		{name: "title when no name", fields: f("amount", "title"), want: "title"},
		{name: "full_name (clinic shape)", fields: f("full_name", "phone"), want: "full_name"},
		{name: "case-insensitive, returns real casing", fields: f("Subject"), want: "Subject"},
		{name: "nothing name-like → empty", fields: f("amount", "qty"), want: ""},
		{name: "no fields → empty", fields: nil, want: ""},
		// #479: no name-like field -> the shared resolver (first required text), not the old `code` guess.
		{name: "required text beats code (shared resolver)", fields: []metadata.FieldDefinition{
			{Name: "code", FieldType: "text"}, {Name: "ho_ten", FieldType: "text", IsRequired: true}}, want: "ho_ten"},
		{name: "builder's choice wins", opts: `{"labelField":"code"}`, fields: []metadata.FieldDefinition{
			{Name: "code", FieldType: "text"}, {Name: "name", FieldType: "text"}}, want: "code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts *string
			if tt.opts != "" {
				opts = &tt.opts
			}
			if got := pickApprovalLabelField(opts, tt.fields); got != tt.want {
				t.Fatalf("pickApprovalLabelField = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecisionComment covers the pure note|comment resolution in isolation.
func TestDecisionComment(t *testing.T) {
	tests := []struct {
		name string
		in   approvalDecisionRequest
		want string
	}{
		{name: "note only", in: approvalDecisionRequest{Note: "n"}, want: "n"},
		{name: "comment only", in: approvalDecisionRequest{Comment: "c"}, want: "c"},
		{name: "note wins", in: approvalDecisionRequest{Note: "n", Comment: "c"}, want: "n"},
		{name: "blank note falls back to comment", in: approvalDecisionRequest{Note: "   ", Comment: "c"}, want: "c"},
		{name: "neither", in: approvalDecisionRequest{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decisionComment(tt.in); got != tt.want {
				t.Fatalf("decisionComment(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestHandler_Decide_ThreadsExpectedStep: the step the screen SAW reaches the engine, and a body
// without it reaches it as nil (an older client keeps deciding on what the server reads).
func TestHandler_Decide_ThreadsExpectedStep(t *testing.T) {
	for _, tt := range []struct {
		name string
		body map[string]any
		want *int
	}{
		{"sent", map[string]any{"expectedStep": 1}, ptrInt(1)},
		{"step zero is a value, not absence", map[string]any{"expectedStep": 0}, ptrInt(0)},
		{"absent", map[string]any{"note": "x"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyDecider{ret: &bpm.Instance{ID: "i1", TenantID: "t-1"}}
			if w := postDecision(newApprovalDecideRouter(spy), "/approvals/i1/approve", tt.body); w.Code != http.StatusOK {
				t.Fatalf("status %d (body %s)", w.Code, w.Body.String())
			}
			if (spy.gotExpectedStep == nil) != (tt.want == nil) || (tt.want != nil && *spy.gotExpectedStep != *tt.want) {
				t.Fatalf("expectedStep threaded = %v, want %v", spy.gotExpectedStep, tt.want)
			}
		})
	}
}

func ptrInt(i int) *int { return &i }
