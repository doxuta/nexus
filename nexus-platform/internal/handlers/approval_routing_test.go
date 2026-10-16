package handlers

// specs/043 S6a at the HTTP seam: a node's `assignment` and the process `options` survive the
// decoder (a typed bind drops unknown keys silently — the requiredFields lesson of S5 only showed up
// at HTTP), reach the repository in storage form, and every shape the engine would refuse is refused
// here with the key that names it. The checks that need the tenant (does that team exist?) are
// proven in services/bpm_routing_test.go; the SQL on MySQL.

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

// routingRepo records what the service handed the repository for create and update.
type routingRepo struct {
	memProcessRepo
	graph, options []byte
}

func (m *routingRepo) CreateProcess(ctx context.Context, in bpm.CreateProcessInput) (string, error) {
	m.graph, m.options = in.Graph, in.Options
	id, err := m.memProcessRepo.CreateProcess(ctx, in)
	m.rec.Graph, m.rec.Options = in.Graph, in.Options
	return id, err
}

func (m *routingRepo) UpdateProcess(ctx context.Context, tenant, id string, in bpm.UpdateProcessInput) error {
	m.graph, m.options = in.Graph, in.Options
	return m.memProcessRepo.UpdateProcess(ctx, tenant, id, in)
}

// newRoutingRouter is graphAdminRouter over the routing repository.
func newRoutingRouter(repo *routingRepo) *gin.Engine {
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

func TestApprovalProcessAPI_AssignmentAndOptionsRoundTrip(t *testing.T) {
	repo := &routingRepo{}
	rr := newRoutingRouter(repo)

	const graphBody = `{"name":"Duyệt theo quy tắc","entityName":"crm_quotes","trigger":"manual",
		"options":{"fallbackAssignee":{"type":"role","id":"r-ops"}},
		"graph":{"nodes":[
		{"id":"nhom","name":"Ban duyệt","kind":"approval","assignment":{"type":"team","id":"tm-ban","strategy":"pool"}},
		{"id":"nguoi-phu-trach","kind":"task","assignment":{"type":"field","field":"owner_id"}},
		{"id":"gui","kind":"approval","assignment":{"type":"submitter"}}]}}`
	w := sendJSON(rr, http.MethodPost, "/admin/approval-processes", graphBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.graph, &g); err != nil || len(g.Nodes) != 3 {
		t.Fatalf("stored graph %s (%v); want 3 nodes", repo.graph, err)
	}
	if a := g.Nodes[0].Assignment; a == nil || a.Type != bpm.AssignTeam || a.ID != "tm-ban" || a.Strategy != "pool" {
		t.Fatalf("node 0 assignment = %+v; want the team pool kept", a)
	}
	if a := g.Nodes[1].Assignment; a == nil || a.Type != bpm.AssignField || a.Field != "owner_id" || g.Nodes[1].Kind != bpm.NodeTask {
		t.Fatalf("node 1 = %+v; want a task routed by the owner field", g.Nodes[1])
	}
	if a := g.Nodes[2].Assignment; a == nil || a.Type != bpm.AssignSubmitter {
		t.Fatalf("node 2 assignment = %+v; want the submitter", a)
	}
	if string(repo.options) != `{"fallbackAssignee":{"type":"role","id":"r-ops"}}` {
		t.Fatalf("stored options = %s; want the fallback kept", repo.options)
	}
	// and the read answers with both (the Studio round-trips what it loaded)
	if !strings.Contains(w.Body.String(), `"options":{"fallbackAssignee"`) || !strings.Contains(w.Body.String(), `"assignment":{"type":"team"`) {
		t.Fatalf("response %s lost options or assignment", w.Body.String())
	}

	// a linear steps body carrying an assignment is lifted to the graph form, options kept on PUT
	const linear = `{"name":"Tuyến tính","entityName":"crm_quotes","trigger":"manual",
		"options":{"fallbackAssignee":{"type":"user","id":"u-fb"}},
		"steps":[{"name":"Người gửi","assignment":{"type":"submitter"}}]}`
	w = sendJSON(rr, http.MethodPut, "/admin/approval-processes/p-1", linear)
	if w.Code != http.StatusOK {
		t.Fatalf("linear PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	if !strings.Contains(string(repo.graph), `"assignment":{"type":"submitter"}`) || string(repo.options) != `{"fallbackAssignee":{"type":"user","id":"u-fb"}}` {
		t.Fatalf("PUT stored graph=%s options=%s; want the lifted assignment and the new fallback", repo.graph, repo.options)
	}
}

func TestApprovalProcessAPI_AssignmentAndOptionsRefused(t *testing.T) {
	repo := &routingRepo{}
	rr := newRoutingRouter(repo)
	node := func(assignment string) string {
		return `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"id":"a","kind":"approval","assignment":` + assignment + `}]}}`
	}
	cases := []struct {
		name, body, code, field string
	}{
		{"both approverType and assignment", `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u","assignment":{"type":"submitter"}}]}}`, "APPROVAL_STEP_INVALID", "graph.nodes[0].assignment"},
		{"unknown assignment type", node(`{"type":"boss","id":"x"}`), "APPROVAL_STEP_INVALID", "graph.nodes[0].assignment.type"},
		{"team without id", node(`{"type":"team"}`), "APPROVAL_STEP_INVALID", "graph.nodes[0].assignment.id"},
		{"unknown strategy (round_robin opened in S6b)", node(`{"type":"team","id":"tm","strategy":"lottery"}`), "APPROVAL_STEP_INVALID", "graph.nodes[0].assignment.strategy"},
		{"a strategy on a role is refused", node(`{"type":"role","id":"r","strategy":"round_robin"}`), "APPROVAL_STEP_INVALID", "graph.nodes[0].assignment.strategy"},
		{"key inside assignment of a later slice", node(`{"type":"team","id":"tm","quorum":50}`), "APPROVAL_NODE_KIND_UNSUPPORTED", "graph.nodes[0].assignment.quorum"},
		{"fallback with a bad type", `{"name":"P","entityName":"crm_quotes","trigger":"manual","options":{"fallbackAssignee":{"type":"boss","id":"x"}},"steps":[{"approverType":"user","approverId":"u"}]}`, "APPROVAL_OPTIONS_INVALID", "options.fallbackAssignee.type"},
		{"fallback without an id", `{"name":"P","entityName":"crm_quotes","trigger":"manual","options":{"fallbackAssignee":{"type":"user"}},"steps":[{"approverType":"user","approverId":"u"}]}`, "APPROVAL_OPTIONS_INVALID", "options.fallbackAssignee.id"},
		{"option of a later slice", `{"name":"P","entityName":"crm_quotes","trigger":"manual","options":{"onChange":"keep"},"steps":[{"approverType":"user","approverId":"u"}]}`, "APPROVAL_NODE_KIND_UNSUPPORTED", "options.onChange"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := sendJSON(rr, http.MethodPost, "/admin/approval-processes", c.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("= %d %s, want 422", w.Code, w.Body.String())
			}
			// the admin handler answers {error, code}; the key that names the fault (c.field) is on the
			// service's ValidationError and is pinned in services/bpm_routing_test.go
			var e struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != c.code {
				t.Fatalf("code = %q (%v) %s, want %s (%s)", e.Code, err, w.Body.String(), c.code, c.field)
			}
		})
	}
	if repo.graph != nil || repo.options != nil {
		t.Fatalf("a refused body reached the repository: graph=%s options=%s", repo.graph, repo.options)
	}
}
