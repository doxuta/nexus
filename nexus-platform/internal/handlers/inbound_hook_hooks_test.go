package handlers

// specs/048 (queue #227) — what an outside sender is told when a before_save rule refuses or cannot
// be read, through the public receive route over a REAL RecordService and InboundHookService.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

const hookTestSecret = "s3cret"

type hookStoreFake struct{ hook *inboundhook.Hook }

func (f *hookStoreFake) Create(context.Context, *inboundhook.Hook) error            { return nil }
func (f *hookStoreFake) Update(context.Context, string, *inboundhook.Hook) error    { return nil }
func (f *hookStoreFake) UpdateSecret(context.Context, string, string, string) error { return nil }
func (f *hookStoreFake) SoftDelete(context.Context, string, string) error           { return nil }
func (f *hookStoreFake) GetByID(context.Context, string, string) (*inboundhook.Hook, error) {
	return f.hook, nil
}
func (f *hookStoreFake) GetByName(context.Context, string, string) (*inboundhook.Hook, error) {
	return f.hook, nil
}
func (f *hookStoreFake) ListAll(context.Context, string) ([]inboundhook.Hook, error) { return nil, nil }
func (f *hookStoreFake) RecordDelivery(context.Context, *inboundhook.Delivery) error { return nil }
func (f *hookStoreFake) ListDeliveries(context.Context, string, string, int) ([]inboundhook.Delivery, error) {
	return nil, nil
}

type slugFake struct{}

func (slugFake) ResolveTenantBySlug(context.Context, string) (*auth.Tenant, error) {
	return &auth.Tenant{ID: "t1", Slug: "acme", Status: "active"}, nil
}

// receiveSignal POSTs a signed body to the public route of a hook feeding `contacts` under loader.
func receiveSignal(t *testing.T, loader workflow.WorkflowLoader) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
		},
	}
	rs := services.NewRecordService(cat, &vetoHTTPStore{}).WithBeforeHooks(loader, nil)
	hook := &inboundhook.Hook{ID: "h1", TenantID: "t1", Name: "feed", EntityName: "contacts", Secret: hookTestSecret,
		IsActive: true, CreatedBy: "u-admin",
		Mapping: []inboundhook.Mapping{{Source: "name", Field: "name"}, {Source: "stage", Field: "stage"}}}
	h := NewInboundHookHandler(services.NewInboundHookService(&hookStoreFake{hook: hook}, rs, slugFake{}, zap.NewNop()), zap.NewNop())
	r := gin.New()
	h.RegisterPublicRoute(r.Group("/api/v1"))

	body := []byte(`{"name":"Acme","stage":"Closed Won"}`)
	mac := hmac.New(sha256.New, []byte(hookTestSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/acme/feed", bytes.NewReader(body))
	req.Header.Set("X-Nexus-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// A signal whose entity rules cannot be read is refused unjudged: 503 + Retry-After so the sender
// retries, never the 500 HOOK_FAILED that tells it the hook is broken.
func TestInboundHookReceive_HooksUnavailable503(t *testing.T) {
	rec := receiveSignal(t, vetoHTTPLoader{err: errors.New("sys_workflows: connection refused")})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var env struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != "RECORD_HOOKS_UNAVAILABLE" {
		t.Fatalf("envelope = %s (err %v), want code RECORD_HOOKS_UNAVAILABLE", rec.Body.String(), err)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("infrastructure error leaked to the partner: %s", rec.Body.String())
	}
}

// A veto reaches the partner as 422 with the message they can act on, and nothing internal: not the
// rule's UUID, not the anchored field (the write ran under the hook owner's admin scope, so the
// per-role filter had nothing to hide).
func TestInboundHookReceive_VetoLeaksNoInternals(t *testing.T) {
	rule := workflow.Workflow{ID: "rule-uuid-1234", TenantID: "t1", EntityName: "contacts", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{SaveOn: workflow.SaveOnCreate, Rules: []workflow.Rule{
			{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"},
			{Field: "close_date", Op: workflow.OpIsNull},
		}},
		Actions: []workflow.Action{{Type: workflow.ActionVeto, Field: "close_date", Message: "Chốt Thắng phải có Ngày chốt"}}}
	rec := receiveSignal(t, vetoHTTPLoader{rules: []workflow.Workflow{rule}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Details struct {
			Field      string `json:"field"`
			Violations []map[string]any
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if env.Code != "RECORD_HOOK_VETO" || env.Error != "Chốt Thắng phải có Ngày chốt" || len(env.Details.Violations) != 1 {
		t.Fatalf("envelope = %s", rec.Body.String())
	}
	for _, leak := range []string{"rule-uuid-1234", "ruleId", "close_date"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("veto body leaks %q to the partner: %s", leak, rec.Body.String())
		}
	}
}
