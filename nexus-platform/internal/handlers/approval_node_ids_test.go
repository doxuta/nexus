package handlers

// specs/043 S0 bpm-node-ids at the HTTP seam: the admin process API carries step ids both ways,
// and a key outside the linear v1 shape — at step or process level — is refused with
// APPROVAL_NODE_KIND_UNSUPPORTED instead of being dropped by the typed bind (D-7). The service
// validator owns the rule; this proves the decoder actually FEEDS it (the join nobody writes).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

// memProcessRepo stores what the service persists so the response can be read back.
type memProcessRepo struct {
	rec *bpm.ProcessRecord
	// S11: the last repin the handler asked for, and the impact it reads back.
	lastRepin bpm.RepinInput
	impact    *bpm.Impact
}

func (m *memProcessRepo) CreateProcess(_ context.Context, in bpm.CreateProcessInput) (string, error) {
	m.rec = &bpm.ProcessRecord{ID: "p-1", TenantID: in.TenantID, Name: in.Name, EntityName: in.EntityName,
		Trigger: string(in.Trigger), Steps: in.Steps, IsActive: in.IsActive}
	return "p-1", nil
}
func (m *memProcessRepo) GetProcess(context.Context, string, string) (*bpm.ProcessRecord, error) {
	return m.rec, nil
}
func (m *memProcessRepo) ListProcesses(context.Context, string) ([]bpm.ProcessRecord, error) {
	return nil, nil
}
func (m *memProcessRepo) UpdateProcess(_ context.Context, _, _ string, in bpm.UpdateProcessInput) error {
	m.rec.Steps = in.Steps
	return nil
}
func (m *memProcessRepo) DeleteProcess(context.Context, string, string) error { return nil }

// S10 (specs/043): the version/draft half of the process store — a process with no history here.
func (m *memProcessRepo) SaveDraft(context.Context, string, string, bpm.SaveDraftInput) error {
	return nil
}
func (m *memProcessRepo) DiscardDraft(context.Context, string, string) error {
	return bpm.ErrDraftNotFound
}
func (m *memProcessRepo) Publish(context.Context, string, string, string, string) (int, error) {
	return 0, bpm.ErrNoDraft
}
func (m *memProcessRepo) ListVersions(context.Context, string, string) ([]bpm.VersionRecord, error) {
	return nil, nil
}
func (m *memProcessRepo) GetVersion(context.Context, string, string, int) (*bpm.VersionRecord, error) {
	return nil, bpm.ErrVersionNotFound
}
func (m *memProcessRepo) PublishedNodeIDs(context.Context, string, string) (map[string]bool, error) {
	return nil, nil
}

func (m *memProcessRepo) ProcessImpact(context.Context, string, string) (*bpm.Impact, error) {
	if m.impact != nil {
		return m.impact, nil
	}
	return &bpm.Impact{}, nil
}
func (m *memProcessRepo) ListProcessInstances(context.Context, string, string, *int, string, bool, int) ([]bpm.ProcessInstanceRow, error) {
	return nil, nil
}
func (m *memProcessRepo) Repin(_ context.Context, _, _ string, in bpm.RepinInput) (*bpm.RepinOutcome, error) {
	m.lastRepin = in
	return &bpm.RepinOutcome{Results: []bpm.RepinResult{}}, nil
}

func newApprovalAdminRouter(repo *memProcessRepo) *gin.Engine {
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

func sendJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestApprovalProcessAPI_StepIDsAndUnsupportedKeys(t *testing.T) {
	const legacy = `{"name":"Duyệt báo giá","entityName":"crm_quotes","trigger":"manual","steps":[
		{"name":"Trưởng phòng","approverType":"user","approverId":"u-1"},
		{"id":"giam-doc","approverType":"role","approverId":"r-1","slaSeconds":3600}]}`

	repo := &memProcessRepo{}
	r := newApprovalAdminRouter(repo)
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", legacy)
	if w.Code != http.StatusCreated {
		t.Fatalf("legacy-shaped create = %d %s, want 201", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Process struct {
				Steps []bpm.Step `json:"steps"`
			} `json:"process"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s := env.Data.Process.Steps; len(s) != 2 || s[0].ID != "s1" || s[1].ID != "giam-doc" || s[0].Name != "Trưởng phòng" {
		t.Fatalf("persisted steps = %+v, want ids [s1 giam-doc] with names kept", s)
	}

	cases := []struct{ name, body, wantField string }{
		{"step key outside v1", `{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[
			{"approverType":"user","approverId":"u-1","escalateTo":"s2"}]}`, "escalateTo"}, // approvers opened in S4, assignment in S6a, onReject in S8; escalateTo is a later slice
		{"process-level graph", `{"name":"P","entityName":"crm_quotes","trigger":"manual","graph":{"lanes":"a"},
			"steps":[{"approverType":"user","approverId":"u-1"}]}`, "graph"},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			path := "/admin/approval-processes"
			if method == http.MethodPut {
				path += "/p-1"
			}
			w := sendJSON(r, method, path, tc.body)
			var e struct{ Code, Error string }
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != http.StatusUnprocessableEntity || e.Code != "APPROVAL_NODE_KIND_UNSUPPORTED" {
				t.Errorf("%s %s: got %d %s, want 422 APPROVAL_NODE_KIND_UNSUPPORTED", tc.name, method, w.Code, w.Body.String())
			}
			if !bytes.Contains([]byte(e.Error), []byte(tc.wantField)) {
				t.Errorf("%s %s: message %q must name the key %q", tc.name, method, e.Error, tc.wantField)
			}
		}
	}

	// A bad id is refused over HTTP too, with the validator's own code.
	w = sendJSON(r, http.MethodPut, "/admin/approval-processes/p-1", `{"name":"P","entityName":"crm_quotes","trigger":"manual",
		"steps":[{"id":"a","approverType":"user","approverId":"u-1"},{"id":"a","approverType":"user","approverId":"u-2"}]}`)
	if w.Code != http.StatusUnprocessableEntity || !bytes.Contains(w.Body.Bytes(), []byte("APPROVAL_NODE_ID_DUPLICATE")) {
		t.Fatalf("duplicate id: got %d %s, want 422 APPROVAL_NODE_ID_DUPLICATE", w.Code, w.Body.String())
	}
}
