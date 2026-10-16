package handlers

// specs/043 S8 `bpm-reject-return` at the HTTP seam: the process API takes onReject on a step
// (stored as a graph, the default resume point written down) and on a graph node, refuses what the
// engine would not run with APPROVAL_STEP_INVALID located at the key the caller sent, and refuses a
// returnedValue nothing can produce. The decoder FEEDS the validator — the join nobody writes.

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

// outcomeMemRepo also keeps the outcome JSON the service persists.
type outcomeMemRepo struct {
	graphMemRepo
	outcome []byte
}

func (m *outcomeMemRepo) CreateProcess(ctx context.Context, in bpm.CreateProcessInput) (string, error) {
	m.outcome = in.Outcome
	return m.graphMemRepo.CreateProcess(ctx, in)
}

func TestApprovalProcessAPI_OnRejectStoredAsAGraph(t *testing.T) {
	repo := &outcomeMemRepo{}
	h := NewApprovalHandler(services.NewApprovalService(repo, nil, nil), zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: "admin-1", TenantID: "t-1", IsAdmin: true}, TokenType: auth.TokenTypeAccess})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterAdminRoutes(r.Group(""))
	const body = `{"name":"Duyệt báo giá","entityName":"crm_quotes","trigger":"manual",
		"outcome":{"statusField":"status","approvedValue":"Chấp nhận","returnedValue":"Cần bổ sung"},
		"steps":[
		{"id":"truong-phong","name":"Trưởng phòng","approverType":"role","approverId":"r-1"},
		{"id":"ke-toan","name":"Kế toán","approverType":"role","approverId":"r-2","onReject":{"to":"@submitter"}},
		{"name":"Giám đốc","approverType":"user","approverId":"u-1","onReject":{"to":"truong-phong"}}]}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
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
	if string(env.Data.Process.Steps) != "[]" || g == nil || len(g.Nodes) != 3 {
		t.Fatalf("answered steps=%s graph=%+v; want [] beside three nodes (a step with onReject cannot live in the linear column)", env.Data.Process.Steps, g)
	}
	if g.Nodes[0].OnReject != nil {
		t.Fatalf("node 0 grew an onReject: %+v", g.Nodes[0].OnReject)
	}
	if o := g.Nodes[1].OnReject; o == nil || o.To != bpm.RejectToSubmitter || o.ResumeAt != bpm.ResumeRejector {
		t.Fatalf("node 1 onReject = %+v; want @submitter with resumeAt rejector written down", o)
	}
	if o := g.Nodes[2].OnReject; o == nil || o.To != "truong-phong" || o.ResumeAt != "" {
		t.Fatalf("node 2 onReject = %+v; want a return to truong-phong", o)
	}
	var out bpm.Outcome
	if err := json.Unmarshal(repo.outcome, &out); err != nil || out.ReturnedValue != "Cần bổ sung" || out.ApprovedValue != "Chấp nhận" {
		t.Fatalf("stored outcome = %s (err %v); want returnedValue Cần bổ sung beside approvedValue", repo.outcome, err)
	}
	if len((bpm.Process{Graph: g}).Chain()) != 3 {
		t.Fatalf("the stored graph is not runnable by the engine: chain = %d", len((bpm.Process{Graph: g}).Chain()))
	}
}

func TestApprovalProcessAPI_OnRejectMistakes(t *testing.T) {
	const head = `{"name":"P","entityName":"crm_quotes","trigger":"manual",`
	step := func(extra string) string { return `{"id":"a","approverType":"user","approverId":"u-1"` + extra + `}` }
	node := func(extra string) string {
		return `{"id":"a","kind":"approval","approverType":"user","approverId":"u-1"` + extra + `}`
	}
	for name, tc := range map[string]struct{ body, code, field string }{
		"node ok": {head + `"graph":{"nodes":[` + node("") + `,{"kind":"approval","approverType":"user","approverId":"u-2","onReject":{"to":"a"}}]}}`, "", ""},
		"unknown target": {head + `"steps":[` + step(``) + `,{"approverType":"user","approverId":"u-2","onReject":{"to":"zzz"}}]}`,
			"APPROVAL_STEP_INVALID", "steps[1].onReject.to"},
		"later step": {head + `"steps":[` + step(`,"onReject":{"to":"b"}`) + `,{"id":"b","approverType":"user","approverId":"u-2"}]}`,
			"APPROVAL_STEP_INVALID", "steps[0].onReject.to"},
		"empty target": {head + `"steps":[` + step(`,"onReject":{}`) + `]}`,
			"APPROVAL_STEP_INVALID", "steps[0].onReject.to"},
		"resume on a node jump": {head + `"steps":[` + step(``) + `,{"approverType":"user","approverId":"u-2","onReject":{"to":"a","resumeAt":"start"}}]}`,
			"APPROVAL_STEP_INVALID", "steps[1].onReject.resumeAt"},
		"bad resume": {head + `"steps":[` + step(`,"onReject":{"to":"@submitter","resumeAt":"middle"}`) + `]}`,
			"APPROVAL_STEP_INVALID", "steps[0].onReject.resumeAt"},
		"node mistake points at the graph": {head + `"graph":{"nodes":[{"kind":"approval","approverType":"user","approverId":"u","onReject":{"to":"zzz"}}]}}`,
			"APPROVAL_STEP_INVALID", "graph.nodes[0].onReject.to"},
		"onReject on a task": {head + `"graph":{"nodes":[{"kind":"task","approverType":"user","approverId":"u","onReject":{"to":"@end"}}]}}`,
			"APPROVAL_STEP_INVALID", "graph.nodes[0].onReject.to"},
		"returnedValue with nothing to return": {head + `"outcome":{"statusField":"status","returnedValue":"Cần bổ sung"},"steps":[` + step(``) + `]}`,
			"APPROVAL_OUTCOME_VALUE_UNUSED", "outcome.returnedValue"},
		"returnedValue alone is a value": {head + `"outcome":{"statusField":"status","returnedValue":"Cần bổ sung"},"steps":[` + step(`,"onReject":{"to":"@submitter"}`) + `]}`, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := sendJSON(graphAdminRouter(&graphMemRepo{}), http.MethodPost, "/admin/approval-processes", tc.body)
			if tc.code == "" {
				if w.Code != http.StatusCreated {
					t.Fatalf("= %d %s, want 201", w.Code, w.Body.String())
				}
				return
			}
			var e struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != http.StatusUnprocessableEntity || e.Code != tc.code {
				t.Fatalf("= %d %s; want 422 %s", w.Code, w.Body.String(), tc.code)
			}
		})
	}
}
