package handlers

// specs/043 S1 (#39) at the HTTP seam: slaClock is a real step key now (it was refused until this
// slice, D-7), a mistyped one is refused at config time (B6), and both surfaces a non-admin
// approver reads — /approvals/pending and /approvals/:id — carry the BASIS of the deadline, so the
// screen can say whether "11:00 thứ Hai" is working hours or a wall-clock fallback.

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
	"nexus/platform/internal/services"
)

func TestApprovalProcessAPI_SLAClock(t *testing.T) {
	repo := &memProcessRepo{}
	r := newApprovalAdminRouter(repo)
	w := sendJSON(r, http.MethodPost, "/admin/approval-processes", `{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[
		{"approverType":"user","approverId":"u-1","slaSeconds":14400,"slaClock":"wall"},
		{"approverType":"user","approverId":"u-2","slaSeconds":14400}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with slaClock = %d %s, want 201", w.Code, w.Body.String())
	}
	var steps []bpm.Step
	if err := json.Unmarshal(repo.rec.Steps, &steps); err != nil || len(steps) != 2 {
		t.Fatalf("persisted steps = %s (err %v)", repo.rec.Steps, err)
	}
	// Persisted as sent: an explicit clock is kept, a missing one stays missing (it means
	// "business" for every instance started from now on — pinned explicitly at arm time).
	if steps[0].SLAClock != bpm.SLAClockWall || steps[1].SLAClock != "" {
		t.Fatalf("persisted clocks = [%q %q], want [wall \"\"]", steps[0].SLAClock, steps[1].SLAClock)
	}

	w = sendJSON(r, http.MethodPut, "/admin/approval-processes/p-1", `{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[
		{"approverType":"user","approverId":"u-1","slaSeconds":14400,"slaClock":"gio_lam"}]}`)
	var e struct{ Code, Error, Field string }
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusUnprocessableEntity || e.Code != "APPROVAL_STEP_INVALID" || !strings.Contains(e.Error, "slaClock") {
		t.Fatalf("mistyped slaClock: got %d %s, want 422 APPROVAL_STEP_INVALID naming slaClock", w.Code, w.Body.String())
	}
}

// detailRepo serves a single instance to both /approvals/pending and /approvals/:id.
type detailRepo struct{ inst bpm.Instance }

func (f *detailRepo) ListPendingForUser(context.Context, string, string, []string, bpm.EntityView) ([]bpm.Instance, error) {
	return []bpm.Instance{f.inst}, nil
}
func (f *detailRepo) GetInstance(_ context.Context, tenantID, id string) (*bpm.Instance, error) {
	if f.inst.ID != id || f.inst.TenantID != tenantID {
		return nil, bpm.ErrInstanceNotFound
	}
	cp := f.inst
	return &cp, nil
}
func (f *detailRepo) ListByRecordLane(context.Context, string, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

func (f *detailRepo) ListByRecord(context.Context, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

// ListOpenSeats: this fake holds no seats — the visibility gate answers through the projection.
func (f *detailRepo) ListOpenSeats(context.Context, string, string) ([]bpm.Seat, error) {
	return nil, nil
}

func TestApprovalReadSurfaces_CarrySLABasis(t *testing.T) {
	due := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC) // 11:00 Monday ICT
	inst := bpm.Instance{
		ID: "inst-1", TenantID: "t-1", ProcessID: "p-1", EntityName: "crm_quotes", RecordID: "q-1",
		Status: bpm.StatusPending, CurrentApproverType: bpm.ApproverUser, CurrentApproverID: "actor-1",
		SLADueAt:   &due,
		Definition: &bpm.Definition{Steps: []bpm.Step{{ID: "s1", ApproverType: bpm.ApproverUser, ApproverID: "actor-1", SLASeconds: 14400, SLAClock: "business"}}, FirstDueBasis: bpm.SLABasisBusiness},
	}
	gin.SetMode(gin.TestMode)
	svc := services.NewApprovalService(&memProcessRepo{}, &detailRepo{inst: inst}, nil)
	h := NewApprovalHandler(svc, zap.NewNop())
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

	get := func(path string) []byte {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}

	var pending struct {
		Data struct {
			Approvals []struct {
				ID       string `json:"ID"`
				SLADueAt string `json:"SLADueAt"`
				SLABasis string `json:"slaBasis"`
			} `json:"approvals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/approvals/pending"), &pending); err != nil || len(pending.Data.Approvals) != 1 {
		t.Fatalf("pending decode: %v %+v", err, pending)
	}
	if row := pending.Data.Approvals[0]; row.SLABasis != "business" || row.SLADueAt == "" {
		t.Errorf("pending row = %+v, want slaBasis business beside the legacy SLADueAt", row)
	}

	var detail struct {
		Data struct {
			Approval struct {
				SLADueAt string `json:"slaDueAt"`
				SLA      *struct {
					DueAt string `json:"dueAt"`
					Basis string `json:"basis"`
				} `json:"sla"`
			} `json:"approval"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/approvals/inst-1"), &detail); err != nil {
		t.Fatalf("detail decode: %v", err)
	}
	a := detail.Data.Approval
	if a.SLA == nil || a.SLA.Basis != "business" || a.SLA.DueAt != a.SLADueAt || a.SLADueAt == "" {
		t.Errorf("detail sla = %+v (slaDueAt %q), want {dueAt: slaDueAt, basis: business}", a.SLA, a.SLADueAt)
	}
}
