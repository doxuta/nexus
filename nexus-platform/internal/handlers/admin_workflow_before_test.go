package handlers

// specs/048 S1a (T010): a before_save veto rule survives the wire both ways. A body key the handler
// does not map (condition.saveOn) is dropped before the validator sees it — an update-only rule
// would come back as one that also blocks creates, with a 201 to show for it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// memWorkflowRepo is a tenant-pinned in-memory workflow store behind the real WorkflowService.
type memWorkflowRepo struct {
	rows map[string]*workflow.WorkflowRecord
	seq  int
}

func (m *memWorkflowRepo) Create(_ context.Context, in workflow.CreateInput) (string, error) {
	m.seq++
	id := fmt.Sprintf("wf-%d", m.seq)
	m.rows[id] = &workflow.WorkflowRecord{ID: id, TenantID: in.TenantID, Name: in.Name, EntityName: in.EntityName,
		Trigger: string(in.Trigger), Condition: in.Condition, Actions: in.Actions, IsActive: in.IsActive}
	return id, nil
}

func (m *memWorkflowRepo) Get(_ context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	r, ok := m.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, workflow.ErrWorkflowNotFound
	}
	return r, nil
}

func (m *memWorkflowRepo) List(_ context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	var out []workflow.WorkflowRecord
	for _, r := range m.rows {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (m *memWorkflowRepo) Update(_ context.Context, tenantID, id string, in workflow.UpdateInput) error {
	r, ok := m.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	r.Name, r.EntityName, r.Trigger = in.Name, in.EntityName, string(in.Trigger)
	r.Condition, r.Actions, r.IsActive = in.Condition, in.Actions, in.IsActive
	return nil
}

func (m *memWorkflowRepo) SetActive(_ context.Context, tenantID, id string, active bool, _ string) error {
	r, ok := m.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	r.IsActive = active
	return nil
}

func (m *memWorkflowRepo) Delete(_ context.Context, tenantID, id string) error {
	if r, ok := m.rows[id]; !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	delete(m.rows, id)
	return nil
}

func (m *memWorkflowRepo) CountActive(_ context.Context, tenantID, entityName string, trigger workflow.Trigger, excludeID string) (int, error) {
	n := 0
	for id, r := range m.rows {
		if id != excludeID && r.TenantID == tenantID && r.EntityName == entityName && r.Trigger == string(trigger) && r.IsActive {
			n++
		}
	}
	return n, nil
}

func beforeWorkflowRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := services.NewWorkflowService(&memWorkflowRepo{rows: map[string]*workflow.WorkflowRecord{}})
	h := NewAdminWorkflowHandler(svc, zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("tenantID", "t1"); c.Next() })
	h.RegisterRoutes(&r.RouterGroup)
	return r
}

func TestWorkflowAPI_BeforeSaveRoundTrip(t *testing.T) {
	r := beforeWorkflowRouter()
	body := `{"name":"Chốt Thắng cần Ngày chốt","entityName":"crm_opportunities","trigger":"before_save","isActive":false,
		"condition":{"saveOn":"update","rules":[{"field":"stage","op":"changedTo","value":"Closed Won"},{"field":"close_date","op":"isnull"}]},
		"actions":[{"type":"veto","field":"close_date","message":"Chốt Thắng phải có Ngày chốt"}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/workflows", bytes.NewBufferString(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Workflow struct{ ID string } `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.Workflow.ID == "" {
		t.Fatalf("decode POST: %v %s", err, w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/workflows/"+created.Data.Workflow.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", w.Code, w.Body.String())
	}
	var got struct {
		Data struct {
			Workflow struct {
				Trigger   string `json:"trigger"`
				IsActive  bool   `json:"isActive"`
				Condition struct {
					SaveOn string            `json:"saveOn"`
					Rules  []json.RawMessage `json:"rules"`
				} `json:"condition"`
				Actions []struct {
					Type, Field, Message string
				} `json:"actions"`
			} `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	wf := got.Data.Workflow
	if wf.Trigger != "before_save" || wf.IsActive || wf.Condition.SaveOn != "update" || len(wf.Condition.Rules) != 2 {
		t.Fatalf("GET workflow = %+v, want before_save / off / saveOn=update / 2 rules", wf)
	}
	if len(wf.Actions) != 1 || wf.Actions[0].Type != "veto" || wf.Actions[0].Field != "close_date" ||
		wf.Actions[0].Message != "Chốt Thắng phải có Ngày chốt" {
		t.Fatalf("actions = %+v", wf.Actions)
	}

	// S1b runs update-only before_save rules, so switching this one on is accepted.
	on := bytes.Replace([]byte(body), []byte(`"isActive":false`), []byte(`"isActive":true`), 1)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/admin/workflows/"+created.Data.Workflow.ID, bytes.NewReader(on)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT isActive:true saveOn=update status = %d, body %s", w.Code, w.Body.String())
	}

	// S2 runs before_save on creates too, so widening it (saveOn absent) is accepted as well.
	both := bytes.Replace(on, []byte(`"saveOn":"update",`), nil, 1)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/admin/workflows/"+created.Data.Workflow.ID, bytes.NewReader(both)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT isActive:true saveOn=both status = %d, body %s", w.Code, w.Body.String())
	}
}

// S4 (T087): a before_save field_update's onlyIfEmpty and valueSource survive the wire both ways. A
// body key the DTO does not declare is dropped before the validator sees it — "fill when empty"
// would come back as "always overwrite", with a 201 to show for it.
func TestWorkflowAPI_BeforeFieldUpdateRoundTrip(t *testing.T) {
	r := beforeWorkflowRouter()
	body := `{"name":"Lead không nguồn là Website","entityName":"crm_leads","trigger":"before_save",
		"actions":[{"type":"field_update","field":"source","value":"Web","onlyIfEmpty":true},
			{"type":"field_update","field":"company","value":"{{full_name}}","valueSource":"template"}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/workflows", bytes.NewBufferString(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Workflow struct {
				ID      string            `json:"id"`
				Actions []json.RawMessage `json:"actions"`
			} `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode POST: %v %s", err, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/workflows/"+created.Data.Workflow.ID, nil))
	var got struct {
		Data struct {
			Workflow struct {
				Actions []json.RawMessage `json:"actions"`
			} `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v %s", err, w.Body.String())
	}
	want := `[{"type":"field_update","field":"source","value":"Web","onlyIfEmpty":true} ` +
		`{"type":"field_update","field":"company","value":"{{full_name}}","valueSource":"template"}]`
	if fmt.Sprintf("%s", got.Data.Workflow.Actions) != want {
		t.Fatalf("GET actions = %s\nwant           %s", got.Data.Workflow.Actions, want)
	}
}

// T100 (specs/048 S5): a normalize action's ops cross the admin API both ways, in order. The handler
// DTO is an explicit struct — a key it does not declare never reaches the validator, and the rule
// would be refused for having no operation.
func TestWorkflowAPI_BeforeNormalizeRoundTrip(t *testing.T) {
	r := beforeWorkflowRouter()
	body := `{"name":"Chuẩn hoá email","entityName":"crm_leads","trigger":"before_save",
		"actions":[{"type":"normalize","field":"email","ops":["trim","lower"]}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/workflows", bytes.NewBufferString(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Workflow struct{ ID string } `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode POST: %v %s", err, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/workflows/"+created.Data.Workflow.ID, nil))
	var got struct {
		Data struct {
			Workflow struct {
				Actions []json.RawMessage `json:"actions"`
			} `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v %s", err, w.Body.String())
	}
	if want := `[{"type":"normalize","field":"email","ops":["trim","lower"]}]`; fmt.Sprintf("%s", got.Data.Workflow.Actions) != want {
		t.Fatalf("GET actions = %s\nwant           %s", got.Data.Workflow.Actions, want)
	}
}
