package handlers

// specs/043 S4 `bpm-parallel-approval` at the HTTP seam: the process API takes `approvers` +
// `completion` on a step (stored as a graph) and on a graph node; POST /admin/approval-instances/
// :id/override asks for its reason (422) and answers the engine's refusals with their codes and
// status (409 for the state conflicts).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

func TestApprovalProcessAPI_ParallelStepsAreStoredAsAGraph(t *testing.T) {
	repo := &graphMemRepo{}
	r := graphAdminRouter(repo)

	const body = `{"name":"Hội đồng","entityName":"crm_quotes","trigger":"manual","steps":[
		{"name":"Trưởng phòng","approverType":"role","approverId":"r-1"},
		{"name":"Hội đồng","approvers":[{"type":"user","id":"u-1"},{"type":"user","id":"u-2"}],"completion":{"mode":"any"}}]}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("parallel create = %d %s, want 201", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Process struct {
				Steps json.RawMessage `json:"steps"`
				Graph *bpm.Graph      `json:"graph"`
			} `json:"process"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g := env.Data.Process.Graph
	if string(env.Data.Process.Steps) != "[]" || g == nil || len(g.Nodes) != 2 {
		t.Fatalf("answered steps=%s graph=%+v; want [] beside two nodes", env.Data.Process.Steps, g)
	}
	council := g.Nodes[1]
	if len(council.Approvers) != 2 || council.Completion == nil || council.Completion.Mode != bpm.CompletionAny || council.ApproverType != "" {
		t.Fatalf("council node = %+v; want 2 approvers, mode any, no single approver", council)
	}

	// The same step on a graph node, and the mistakes, over HTTP.
	for name, tc := range map[string]struct{ body, field string }{
		"graph node ok":                       {`{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"kind":"approval","approvers":[{"type":"user","id":"a"},{"type":"role","id":"b"}]}]}}`, ""},
		"one approver":                        {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{"approvers":[{"type":"user","id":"a"}]}]}`, "steps[0].approvers"},
		"both approver sources":               {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"user","approverId":"x","approvers":[{"type":"user","id":"a"},{"type":"user","id":"b"}]}]}`, "steps[0].approverType"},
		"unknown mode (quorum opened in S6b)": {`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{"approvers":[{"type":"user","id":"a"},{"type":"user","id":"b"}],"completion":{"mode":"majority","percent":60}}]}`, "steps[0].completion.mode"},
		"node mistake points at the graph":    {`{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"kind":"approval","approvers":[{"type":"user","id":"a"},{"type":"user","id":"a"}]}]}}`, "graph.nodes[0].approvers"},
	} {
		w := sendJSON(graphAdminRouter(&graphMemRepo{}), http.MethodPost, "/admin/approval-processes", tc.body)
		if tc.field == "" {
			if w.Code != http.StatusCreated {
				t.Errorf("%s = %d %s, want 201", name, w.Code, w.Body.String())
			}
			continue
		}
		var e struct {
			Code    string `json:"code"`
			Details struct {
				Field string `json:"field"`
			} `json:"details"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusUnprocessableEntity || e.Code != "APPROVAL_STEP_INVALID" {
			t.Errorf("%s = %d %s, want 422 APPROVAL_STEP_INVALID", name, w.Code, w.Body.String())
		}
	}
}

// overrideEngine is the decider seam: it answers Override with a scripted error.
type overrideEngine struct {
	spyDecider
	err         error
	gotKind     bpm.DecisionKind
	gotNote     string
	gotIsAdmin  bool
	gotExpected *int
	calls       int
}

func (o *overrideEngine) Override(_ context.Context, _, id string, kind bpm.DecisionKind, note string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	o.calls++
	o.gotKind, o.gotNote, o.gotIsAdmin, o.gotExpected = kind, note, actor.IsAdmin, expectedStep
	return &bpm.Instance{ID: id, Status: bpm.StatusApproved}, o.err
}

func overrideRouter(eng *overrideEngine) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewApprovalHandler(services.NewApprovalService(nil, nil, eng), zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "admin-1", TenantID: "t-1", IsAdmin: true},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterAdminRoutes(r.Group(""))
	return r
}

func TestApprovalOverrideAPI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		engErr   error
		wantCode int
		wantErr  string
		wantCall bool
	}{
		{"approve with a reason", `{"decision":"approve","note":"hội đồng đã họp"}`, nil, http.StatusOK, "", true},
		{"reject with a reason", `{"decision":"reject","note":"hồ sơ giả"}`, nil, http.StatusOK, "", true},
		{"no reason", `{"decision":"approve"}`, nil, http.StatusUnprocessableEntity, "APPROVAL_OVERRIDE_NOTE_REQUIRED", false},
		{"blank reason", `{"decision":"reject","note":"   "}`, nil, http.StatusUnprocessableEntity, "APPROVAL_OVERRIDE_NOTE_REQUIRED", false},
		{"unknown decision", `{"decision":"maybe","note":"x"}`, nil, http.StatusUnprocessableEntity, "APPROVAL_DECISION_INVALID", false},
		{"already finished", `{"decision":"approve","note":"x"}`, bpm.ErrNotPending, http.StatusUnprocessableEntity, "APPROVAL_NOT_PENDING", true},
		{"not found", `{"decision":"approve","note":"x"}`, bpm.ErrInstanceNotFound, http.StatusNotFound, "APPROVAL_INSTANCE_NOT_FOUND", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &overrideEngine{err: tc.engErr}
			w := sendJSON(overrideRouter(eng), http.MethodPost, "/admin/approval-instances/ai-1/override", tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("status %d %s; want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if tc.wantErr != "" {
				var e struct{ Code string }
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				if e.Code != tc.wantErr {
					t.Fatalf("code %q; want %q", e.Code, tc.wantErr)
				}
			}
			if (eng.calls == 1) != tc.wantCall {
				t.Fatalf("engine calls = %d; want called=%v (the reason is judged BEFORE the engine)", eng.calls, tc.wantCall)
			}
			if tc.wantCall && tc.engErr == nil && (!eng.gotIsAdmin || eng.gotNote == "") {
				t.Fatalf("engine got admin=%v note=%q", eng.gotIsAdmin, eng.gotNote)
			}
		})
	}
}

// The override carries the step the admin's screen showed (expectedStep) down to the engine; a
// body without it stays valid (older client, no guard).
func TestApprovalOverrideAPI_ExpectedStepReachesTheEngine(t *testing.T) {
	eng := &overrideEngine{}
	w := sendJSON(overrideRouter(eng), http.MethodPost, "/admin/approval-instances/ai-1/override", `{"decision":"approve","note":"x","expectedStep":2}`)
	if w.Code != http.StatusOK || eng.gotExpected == nil || *eng.gotExpected != 2 {
		t.Fatalf("status %d, expectedStep %v; want 200 and 2", w.Code, eng.gotExpected)
	}
	eng = &overrideEngine{}
	sendJSON(overrideRouter(eng), http.MethodPost, "/admin/approval-instances/ai-1/override", `{"decision":"approve","note":"x"}`)
	if eng.gotExpected != nil {
		t.Fatalf("no expectedStep in the body, engine got %d", *eng.gotExpected)
	}
	eng = &overrideEngine{err: bpm.ErrConcurrent}
	w = sendJSON(overrideRouter(eng), http.MethodPost, "/admin/approval-instances/ai-1/override", `{"decision":"approve","note":"x","expectedStep":0}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale override: status %d; want 409", w.Code)
	}
}

// Every row of /approvals/pending says awaitsMe: true - the list IS the caller's inbox (a seat they
// hold), and the single-holder projection that the screen used to re-derive it from is empty on a
// step with several seats (specs/043 S4).
func TestApprovalPending_RowsCarryAwaitsMe(t *testing.T) {
	r := newPendingRouter([]bpm.Instance{{ID: "ai-1", EntityName: "crm_quotes", RecordID: "q-1"}}, nil, nil)
	w := sendJSON(r, http.MethodGet, "/approvals/pending", "")
	if w.Code != http.StatusOK {
		t.Fatalf("pending = %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Approvals []struct {
				ID       string `json:"ID"`
				AwaitsMe *bool  `json:"awaitsMe"`
			} `json:"approvals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Approvals) != 1 || env.Data.Approvals[0].AwaitsMe == nil || !*env.Data.Approvals[0].AwaitsMe {
		t.Fatalf("rows = %+v; want one row with awaitsMe true", env.Data.Approvals)
	}
}

// The two state conflicts of S4 are 409 (like APPROVAL_CONFLICT), not 422.
func TestStatusForCode_ParallelConflicts(t *testing.T) {
	for _, code := range []string{"APPROVAL_ALREADY_DECIDED_BY_ACTOR", "APPROVAL_ADMIN_OVERRIDE_REQUIRED", "APPROVAL_DELEGATE_TARGET_SEATED"} {
		if got := statusForCode(code); got != http.StatusConflict {
			t.Errorf("statusForCode(%s) = %d; want 409", code, got)
		}
	}
}
