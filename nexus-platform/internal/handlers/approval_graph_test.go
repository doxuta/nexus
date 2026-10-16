package handlers

// specs/043 S3a2 at the HTTP seam (T054, T062): the admin process API takes a chain in GRAPH form,
// its decoder feeds every key outside the node/graph shape to the validator (D-7), a linear-only
// PUT onto a stored graph answers 409 APPROVAL_PROCESS_V2_OVERWRITE, and ?force=true reaches the
// repository. The repository contract is modelled by graphMemRepo; the SQL guard itself is proven
// against MySQL in services/bpm_graph_integration_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

type graphMemRepo struct {
	memProcessRepo
	graph []byte
	force bool
}

func (m *graphMemRepo) CreateProcess(ctx context.Context, in bpm.CreateProcessInput) (string, error) {
	id, err := m.memProcessRepo.CreateProcess(ctx, in)
	m.graph = in.Graph
	m.rec.Graph = in.Graph
	return id, err
}

// UpdateProcess models bpm.SQLRepository's contract: a linear-only update onto a stored graph is
// refused unless forced.
func (m *graphMemRepo) UpdateProcess(ctx context.Context, tenant, id string, in bpm.UpdateProcessInput) error {
	m.force = in.Force
	if m.graph != nil && in.Graph == nil && !in.Force {
		return bpm.ErrProcessGraphOverwrite
	}
	m.graph = in.Graph
	m.rec.Graph = in.Graph
	return m.memProcessRepo.UpdateProcess(ctx, tenant, id, in)
}

// graphAdminRouter is newApprovalAdminRouter over the graph-aware repository.
func graphAdminRouter(repo *graphMemRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewApprovalHandler(services.NewApprovalService(repo, nil, nil), zap.NewNop())
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

// TestApprovalProcessAPI_WaitNodeOverHTTP (specs/043 S12a): the wait node's `until` and `sla` objects
// reach the validator from the wire — stored when valid (with a `warnings` list beside `process`),
// refused with the coded 422 when the deadline names nobody to tell, or carries a key of a later slice.
func TestApprovalProcessAPI_WaitNodeOverHTTP(t *testing.T) {
	r := graphAdminRouter(&graphMemRepo{})
	const wait = `{"id":"phan-hoi","kind":"wait","until":{"rules":[{"field":"status","op":"neq","value":"Open"}]},"sla":%s}`
	body := func(sla string) string {
		return `{"name":"SLA phiếu","entityName":"tk_tickets","trigger":"after_create","graph":{"nodes":[` +
			strings.Replace(wait, "%s", sla, 1) + `,{"id":"xong","kind":"end","result":"approved"}]}}`
	}
	const ok = `{"amount":4,"unit":"hours","matrix":[{"when":{"rules":[{"field":"priority","op":"eq","value":"Khẩn"}]},"amount":1,"unit":"business_hours"}],"writeDueTo":"first_response_due","notify":{"type":"field","field":"assigned_to"}}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body(ok))
	if w.Code != http.StatusCreated {
		t.Fatalf("wait create = %d %s, want 201", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Process struct {
				Graph *bpm.Graph `json:"graph"`
			} `json:"process"`
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	n := env.Data.Process.Graph.Nodes[0]
	if n.Kind != bpm.NodeWait || n.SLA == nil || n.SLA.Notify == nil || n.SLA.Notify.Field != "assigned_to" || len(n.SLA.Matrix) != 1 || n.Until == nil {
		t.Fatalf("stored wait node %+v; want until, matrix and notify field assigned_to", n)
	}
	if env.Data.Warnings == nil {
		t.Fatal("create answered no `warnings` list (want [] beside process)")
	}
	for name, tc := range map[string]struct{ sla, code string }{
		"no notify":                {`{"amount":4,"unit":"hours"}`, "APPROVAL_SLA_NOTIFY_REQUIRED"},
		"sla key of a later slice": {`{"amount":4,"unit":"hours","notify":{"type":"submitter"},"bogusKey":"x"}`, "APPROVAL_NODE_KIND_UNSUPPORTED"},
		"bad unit":                 {`{"amount":4,"unit":"weeks","notify":{"type":"submitter"}}`, "APPROVAL_SLA_UNIT_INVALID"},
	} {
		w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body(tc.sla))
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusUnprocessableEntity || e.Code != tc.code {
			t.Fatalf("%s = %d %s, want 422 %s", name, w.Code, w.Body.String(), tc.code)
		}
	}
}

func TestApprovalProcessAPI_GraphAndOverwrite409(t *testing.T) {
	repo := &graphMemRepo{}
	r := graphAdminRouter(repo)

	const graphBody = `{"name":"Duyệt đồ thị","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[
		{"id":"truong-phong","name":"Trưởng phòng","kind":"approval","approverType":"role","approverId":"r-1","slaSeconds":3600},
		{"kind":"approval","approverType":"user","approverId":"u-1","slaClock":"wall"}]}}`
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", graphBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("graph create = %d %s, want 201", w.Code, w.Body.String())
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
	if string(env.Data.Process.Steps) != "[]" || g == nil || len(g.Nodes) != 2 || g.Nodes[0].ID != "truong-phong" || g.Nodes[1].ID != "s2" || g.Nodes[1].SLAClock != "wall" {
		t.Fatalf("graph create answered steps=%s graph=%+v; want [] beside two nodes (s2 assigned, wall kept)", env.Data.Process.Steps, g)
	}

	for name, body := range map[string]string{
		"node key of a later slice":  `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"kind":"approval","approverType":"role","approverId":"r","escalateTo":"s2"}]}}`, // approvers/completion opened in S4, assignment in S6a, onReject in S8; next/start/gateway in S7; escalateTo is a later slice
		"graph key of a later slice": `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"lanes":["a"],"nodes":[{"id":"a","kind":"approval","approverType":"role","approverId":"r"}]}}`,
		"node kind of a later slice": `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"id":"a","kind":"approval","approverType":"role","approverId":"r"},{"id":"cho","kind":"subprocess"}]}}`,
		"option of a later slice":    `{"name":"P","entityName":"crm_quotes","trigger":"manual","options":{"calendar":"x"},"steps":[{"approverType":"role","approverId":"r"}]}`, // options: fallbackAssignee (S6a) and lane (S12b) only; the pinned calendar is S13
	} {
		w := sendJSON(r, http.MethodPost, "/admin/approval-processes", body)
		if w.Code != http.StatusUnprocessableEntity || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s = %d %s, want 422", name, w.Code, w.Body.String())
		}
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if e.Code != "APPROVAL_NODE_KIND_UNSUPPORTED" {
			t.Fatalf("%s code = %q, want APPROVAL_NODE_KIND_UNSUPPORTED", name, e.Code)
		}
	}

	const linear = `{"name":"Duyệt đồ thị","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"role","approverId":"r-1"}]}`
	w = sendJSON(r, http.MethodPut, "/admin/approval-processes/p-1", linear)
	if w.Code != http.StatusConflict {
		t.Fatalf("linear PUT onto a graph = %d %s, want 409", w.Code, w.Body.String())
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != "APPROVAL_PROCESS_V2_OVERWRITE" {
		t.Fatalf("409 code = %q (%v), want APPROVAL_PROCESS_V2_OVERWRITE", e.Code, err)
	}
	if repo.graph == nil {
		t.Fatalf("the refused PUT erased the graph")
	}

	w = sendJSON(r, http.MethodPut, "/admin/approval-processes/p-1?force=true", linear)
	if w.Code != http.StatusOK || !repo.force || repo.graph != nil {
		t.Fatalf("forced linear PUT = %d force=%v graph=%s, want 200, forced, graph gone", w.Code, repo.force, repo.graph)
	}
}
