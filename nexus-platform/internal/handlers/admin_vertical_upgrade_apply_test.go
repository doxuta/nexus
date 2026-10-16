package handlers

// POST …/upgrade and GET …/upgrades (specs/069 L2a): error mapping of the apply step, the body
// contract, the response/audit shape and the gate shared with install/upgrade-plan.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
)

type applySvc struct {
	fakeVerticalSvc
	mu        sync.Mutex
	err       error
	gotHash   string
	gotDec    []vertical_pack.UpgradeDecision
	gotTenant string
	gotLimit  int
	gotTick   bool
}

func (f *applySvc) ApplyUpgrade(ctx context.Context, tenantID, _, _ string, _ *vertical_pack.Pack, _, planHash string, d []vertical_pack.UpgradeDecision) (*vertical_pack.UpgradeRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotTenant, f.gotHash, f.gotDec = tenantID, planHash, d
	f.gotTick = vertical_pack.ImmediateConfirmed(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &vertical_pack.UpgradeRun{ID: "run-1", Status: "done", ToVersion: "1.1.0", Counts: map[string]int{"applied": 1}}, nil
}

func (f *applySvc) ListUpgrades(_ context.Context, tenantID, _ string, limit int) ([]vertical_pack.UpgradeRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotTenant, f.gotLimit = tenantID, limit
	return []vertical_pack.UpgradeRun{}, nil
}

func postApply(t *testing.T, svc verticalPackService, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := planRouter(svc, nil, nil, opSubject())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/verticals/installs/crm/upgrade", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestApplyUpgrade_HTTPContract(t *testing.T) {
	ve := func(code string, d map[string]any) error {
		return &services.ValidationError{Code: code, Msg: "m", Details: d}
	}
	tests := []struct {
		name     string
		body     string
		svcErr   error
		wantHTTP int
		wantCode string
		detail   string
	}{
		{"missing planHash", `{"builtin":"crm"}`, nil, 422, "PACK_UPGRADE_PLAN_HASH_REQUIRED", ""},
		{"neither source", `{"planHash":"h"}`, nil, 422, "VERTICAL_PACK_REQUIRED", ""},
		{"stale plan carries the new hash", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_PLAN_STALE", map[string]any{"planHash": "new"}), 409, "PACK_UPGRADE_PLAN_STALE", "planHash"},
		{"another run holds the lock", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_IN_PROGRESS", map[string]any{"runId": "r"}), 409, "PACK_UPGRADE_IN_PROGRESS", "runId"},
		{"decision outside the actions", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_DECISION_INVALID", map[string]any{"itemKey": "k"}), 422, "PACK_UPGRADE_DECISION_INVALID", "itemKey"},
		{"nothing to apply", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_NOTHING_TO_APPLY", map[string]any{}), 422, "PACK_UPGRADE_NOTHING_TO_APPLY", ""},
		{"delivered workspace", `{"builtin":"crm","planHash":"h"}`, ve("PROJECT_DELIVERED_INSTALL_BLOCKED", map[string]any{}), 409, "PROJECT_DELIVERED_INSTALL_BLOCKED", ""},
		{"cross-workspace into a delivered project", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_OPEN_WORKSPACE", map[string]any{}), 409, "PACK_UPGRADE_OPEN_WORKSPACE", ""},
		{"untraced change without the tick", `{"builtin":"crm","planHash":"h"}`, ve("PACK_UPGRADE_IMMEDIATE_UNCONFIRMED", map[string]any{"items": []any{}}), 422, "PACK_UPGRADE_IMMEDIATE_UNCONFIRMED", "items"},
		{"not installed", `{"builtin":"crm","planHash":"h"}`, vertical_pack.ErrInstallNotFound, 404, "VERTICAL_INSTALL_NOT_FOUND", ""},
		{"unexpected error is a 500 without detail", `{"builtin":"crm","planHash":"h"}`, context.DeadlineExceeded, 500, "VERTICAL_UPGRADE_FAILED", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := postApply(t, &applySvc{err: tc.svcErr}, tc.body)
			if w.Code != tc.wantHTTP {
				t.Fatalf("HTTP %d (%s), want %d", w.Code, w.Body.String(), tc.wantHTTP)
			}
			code, details := upgradeErrCode(t, w)
			if code != tc.wantCode {
				t.Fatalf("code %q, want %q", code, tc.wantCode)
			}
			if tc.detail != "" {
				if _, ok := details[tc.detail]; !ok {
					t.Errorf("details %v lack %q", details, tc.detail)
				}
			}
			if tc.wantHTTP == 500 && strings.Contains(w.Body.String(), "deadline") {
				t.Errorf("internal error text leaked: %s", w.Body.String())
			}
		})
	}
}

func TestApplyUpgrade_PassesHashAndDecisionsAndAudits(t *testing.T) {
	svc := &applySvc{}
	gr := planRouter(svc, nil, nil, opSubject())
	body := `{"builtin":"crm","planHash":"abc","decisions":[{"itemType":"workflow","itemKey":"r","action":"apply"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/verticals/installs/crm/upgrade", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gr.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	if svc.gotHash != "abc" || len(svc.gotDec) != 1 || svc.gotDec[0].ItemKey != "r" || svc.gotTenant != mscCallerTenant {
		t.Fatalf("service got hash=%q dec=%v tenant=%q", svc.gotHash, svc.gotDec, svc.gotTenant)
	}
	var resp struct {
		Data struct {
			Run struct{ ID, Status string } `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Data.Run.ID != "run-1" || resp.Data.Run.Status != "done" {
		t.Fatalf("response %s (%v)", w.Body.String(), err)
	}
}

type recAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recAuditor) Record(_ context.Context, e audit.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func routerFor(h *AdminVerticalHandler, sub auth.Subject) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", sub.TenantID)
		c.Set("authClaims", &auth.Claims{Subject: sub})
		c.Next()
	})
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

func TestApplyUpgrade_RecordsPackUpgradeAuditWithoutContent(t *testing.T) {
	svc := &applySvc{}
	h := NewAdminVerticalHandler(svc, nil)
	rec := &recAuditor{}
	h.auditor = rec
	gr := routerFor(h, opSubject())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/verticals/installs/crm/upgrade", strings.NewReader(`{"builtin":"crm","planHash":"h"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gr.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	if len(rec.events) != 1 || rec.events[0].EntityName != "__pack_upgrade__" || rec.events[0].Action != "apply" || rec.events[0].TenantID != mscCallerTenant {
		t.Fatalf("audit events = %+v; want one __pack_upgrade__/apply in the caller's tenant", rec.events)
	}
}

func TestListUpgrades_PassesLimitThrough(t *testing.T) {
	svc := &applySvc{}
	r := planRouter(svc, nil, nil, opSubject())
	for q, want := range map[string]int{"": 0, "?limit=7": 7, "?limit=zzz": 0} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/verticals/installs/crm/upgrades"+q, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || svc.gotLimit != want {
			t.Errorf("%q: HTTP %d limit=%d, want 200 and %d", q, w.Code, svc.gotLimit, want)
		}
	}
}

// L7: only an explicit confirmImmediate:true in the body reaches the service as the tick.
func TestApplyUpgrade_ConfirmImmediateReachesTheServiceOnlyWhenSet(t *testing.T) {
	for body, want := range map[string]bool{
		`{"builtin":"crm","planHash":"h"}`:                          false,
		`{"builtin":"crm","planHash":"h","confirmImmediate":false}`: false,
		`{"builtin":"crm","planHash":"h","confirmImmediate":true}`:  true,
	} {
		svc := &applySvc{}
		if w := postApply(t, svc, body); w.Code != 200 {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		if svc.gotTick != want {
			t.Errorf("%s: tick = %v, want %v", body, svc.gotTick, want)
		}
	}
}
