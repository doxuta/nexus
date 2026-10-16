package handlers

// MS-C gate unit tests (../specs/027-handover-wizard/spec.md §3.1 + §6): POST /admin/verticals/install
// ?targetTenant=<slug> over fakes —
//   (b) non-operator + targetTenant → 403 (uniform admin message, resolver NEVER consulted);
//   (c) unknown/inactive slug → 404 TENANT_NOT_FOUND (one code, anti-enumeration);
//   operator + valid slug → the SAME install path runs with tenantID = TARGET + a __handover__
//   audit event lands in the SOURCE tenant;
//   (d) targetTenant resolving to the caller's own tenant ≡ no param (no audit event);
//   absent param → byte-identical legacy behavior (gate/resolver never touched);
//   unwired deps + targetTenant → fail-closed 403;
//   dryRun + targetTenant → DryRunInstall against the TARGET, no audit (zero writes).
// The cross-tenant ISOLATION invariant (§6a — rows land only in B) is the MySQL integration
// test in internal/services/vertical_pack (handover_msc_integration_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services/vertical_pack"
)

// fakeVerticalSvc records Install/DryRunInstall tenant targets; other methods are unused here.
type fakeVerticalSvc struct {
	mu             sync.Mutex
	installTenants []string
	installUsers   []string
	dryRunTenants  []string
}

func (f *fakeVerticalSvc) Export(context.Context, string, []string, bool, *vertical_pack.ExportManifest) (*vertical_pack.Pack, []string, error) {
	return nil, nil, nil
}

func (f *fakeVerticalSvc) EntityDependencies(context.Context, string, []string) ([]vertical_pack.EntityDependency, error) {
	return nil, nil
}

func (f *fakeVerticalSvc) Install(_ context.Context, tenantID, userID string, _ *vertical_pack.Pack) (*vertical_pack.InstallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installTenants = append(f.installTenants, tenantID)
	f.installUsers = append(f.installUsers, userID)
	return &vertical_pack.InstallResult{}, nil
}

func (f *fakeVerticalSvc) DryRunInstall(_ context.Context, tenantID string, _ *vertical_pack.Pack) (*vertical_pack.InstallPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dryRunTenants = append(f.dryRunTenants, tenantID)
	return &vertical_pack.InstallPlan{}, nil
}

func (f *fakeVerticalSvc) ListInstalls(context.Context, string) ([]vertical_pack.InstallRecord, error) {
	return nil, nil
}

func (f *fakeVerticalSvc) DiffInstalled(context.Context, string, string) (*vertical_pack.DiffReport, error) {
	return nil, nil
}

func (f *fakeVerticalSvc) PlanUpgrade(context.Context, string, string, *vertical_pack.Pack, string) (*vertical_pack.UpgradePlan, error) {
	return nil, nil
}

func (f *fakeVerticalSvc) ApplyUpgrade(context.Context, string, string, string, *vertical_pack.Pack, string, string, []vertical_pack.UpgradeDecision) (*vertical_pack.UpgradeRun, error) {
	return nil, nil
}

func (f *fakeVerticalSvc) ListUpgrades(context.Context, string, string, int) ([]vertical_pack.UpgradeRun, error) {
	return nil, nil
}

// fakeOperatorGate serves the live caller row; failIfCalled trips the test when the gate must
// stay untouched (the no-param path).
type fakeOperatorGate struct {
	t            *testing.T
	user         *auth.User
	err          error
	failIfCalled bool
	calls        int
}

func (f *fakeOperatorGate) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	f.calls++
	if f.failIfCalled {
		f.t.Fatalf("operator gate consulted on a request without targetTenant (tenant=%s user=%s)", tenantID, id)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

// fakeTenantResolver maps slug → tenant; unknown slugs return the auth sentinel. failIfCalled
// pins the anti-enumeration order (gate BEFORE resolution).
type fakeTenantResolver struct {
	t            *testing.T
	bySlug       map[string]*auth.Tenant
	failIfCalled bool
	calls        int
}

func (f *fakeTenantResolver) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	f.calls++
	if f.failIfCalled {
		f.t.Fatalf("tenant resolver consulted (slug=%s) although it must not be", slug)
	}
	if t, ok := f.bySlug[slug]; ok {
		return t, nil
	}
	return nil, auth.ErrTenantNotFound
}

// fakeAuditor captures Record calls synchronously.
type fakeAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeAuditor) Record(_ context.Context, evt audit.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
}

const (
	mscCallerTenant = "tenant-A"
	mscCallerUser   = "op-user-1"
	mscTargetTenant = "tenant-B"
)

// mscRouter mounts the vertical handler behind a shim that injects the verified claims the way
// AuthMiddleware would (tenant + subject), with the MS-C deps wired.
func mscRouter(svc verticalPackService, gate operatorGate, resolver tenantSlugResolver, aud audit.Auditor) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewAdminVerticalHandler(svc, zap.NewNop())
	if gate != nil || resolver != nil || aud != nil {
		h.WithCrossTenantInstall(gate, resolver, aud)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", mscCallerTenant)
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: mscCallerUser, TenantID: mscCallerTenant, IsAdmin: true,
		}})
		c.Next()
	})
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

func postInstall(t *testing.T, r *gin.Engine, query string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/verticals/install"+query, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func operatorUser() *auth.User {
	return &auth.User{ID: mscCallerUser, TenantID: mscCallerTenant, Status: "active", IsAdmin: true, IsPlatformOperator: true}
}

// TestInstall_TargetTenant_NonOperator403 — §6b: an ordinary admin with ?targetTenant gets the
// uniform 403 and the slug is NEVER resolved (no existence probing for non-operators).
func TestInstall_TargetTenant_NonOperator403(t *testing.T) {
	svc := &fakeVerticalSvc{}
	nonOp := operatorUser()
	nonOp.IsPlatformOperator = false
	resolver := &fakeTenantResolver{t: t, failIfCalled: true}
	r := mscRouter(svc, &fakeOperatorGate{t: t, user: nonOp}, resolver, &fakeAuditor{})

	w := postInstall(t, r, "?targetTenant=khach-b", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Byte-identical to RequireAdmin/RequirePlatformOperator (anti-enumeration).
	if resp.Code != "FORBIDDEN" || resp.Error != "Yêu cầu quyền quản trị viên" {
		t.Errorf("403 body = %+v, want the uniform admin refusal", resp)
	}
	if len(svc.installTenants)+len(svc.dryRunTenants) != 0 {
		t.Errorf("service reached despite 403: installs=%v dryRuns=%v", svc.installTenants, svc.dryRunTenants)
	}
}

// TestInstall_TargetTenant_UnknownSlug404 — §6c: an operator naming an unknown (or inactive —
// same sentinel) slug gets ONE code: 404 TENANT_NOT_FOUND.
func TestInstall_TargetTenant_UnknownSlug404(t *testing.T) {
	svc := &fakeVerticalSvc{}
	r := mscRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, &fakeTenantResolver{t: t}, &fakeAuditor{})

	w := postInstall(t, r, "?targetTenant=khong-ton-tai", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "TENANT_NOT_FOUND" {
		t.Errorf("code = %q, want TENANT_NOT_FOUND", resp.Code)
	}
	if len(svc.installTenants) != 0 {
		t.Errorf("install ran despite 404: %v", svc.installTenants)
	}
}

// TestInstall_TargetTenant_OperatorInstallsIntoTarget: the whole install path runs with
// tenantID = TARGET, and the SOURCE tenant gets the __handover__ audit event naming the target
// slug + pack, actor = the operator.
func TestInstall_TargetTenant_OperatorInstallsIntoTarget(t *testing.T) {
	svc := &fakeVerticalSvc{}
	aud := &fakeAuditor{}
	resolver := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{
		"khach-b": {ID: mscTargetTenant, Slug: "khach-b", Status: "active"},
	}}
	r := mscRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, resolver, aud)

	w := postInstall(t, r, "?targetTenant=khach-b", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(svc.installTenants) != 1 || svc.installTenants[0] != mscTargetTenant {
		t.Fatalf("install tenants = %v, want [%s]", svc.installTenants, mscTargetTenant)
	}
	if svc.installUsers[0] != mscCallerUser {
		t.Errorf("install user = %q, want the operator %q (ledger installedBy)", svc.installUsers[0], mscCallerUser)
	}
	if len(aud.events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 __handover__ in the source tenant", len(aud.events))
	}
	evt := aud.events[0]
	if evt.TenantID != mscCallerTenant || evt.EntityName != "__handover__" || evt.Action != "install" || evt.ActorUserID != mscCallerUser {
		t.Errorf("audit event = %+v, want source-tenant __handover__ install by the operator", evt)
	}
	if evt.Diff == nil || evt.Diff.After["targetTenant"] != "khach-b" {
		t.Errorf("audit diff = %+v, want after.targetTenant=khach-b", evt.Diff)
	}
	if evt.Diff.After["pack"] != "CRM" { // builtin crm manifest name
		t.Errorf("audit diff pack = %v, want the pack display name CRM", evt.Diff.After["pack"])
	}
}

// TestInstall_TargetTenant_SelfEqualsNoParam — §6d: targetTenant resolving to the CALLER's own
// tenant behaves exactly like the absent param — install into self, and NO __handover__ event.
func TestInstall_TargetTenant_SelfEqualsNoParam(t *testing.T) {
	svc := &fakeVerticalSvc{}
	aud := &fakeAuditor{}
	resolver := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{
		"minh": {ID: mscCallerTenant, Slug: "minh", Status: "active"},
	}}
	r := mscRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, resolver, aud)

	w := postInstall(t, r, "?targetTenant=minh", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(svc.installTenants) != 1 || svc.installTenants[0] != mscCallerTenant {
		t.Fatalf("install tenants = %v, want caller's own [%s]", svc.installTenants, mscCallerTenant)
	}
	if len(aud.events) != 0 {
		t.Errorf("audit events = %+v, want none for a self-target (≡ no param)", aud.events)
	}
}

// TestInstall_NoTargetTenant_LegacyPathUntouched: without the param the gate and resolver are
// NEVER consulted — zero breaking for every existing caller.
func TestInstall_NoTargetTenant_LegacyPathUntouched(t *testing.T) {
	svc := &fakeVerticalSvc{}
	gate := &fakeOperatorGate{t: t, failIfCalled: true}
	resolver := &fakeTenantResolver{t: t, failIfCalled: true}
	r := mscRouter(svc, gate, resolver, &fakeAuditor{})

	w := postInstall(t, r, "", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(svc.installTenants) != 1 || svc.installTenants[0] != mscCallerTenant {
		t.Fatalf("install tenants = %v, want the caller tenant", svc.installTenants)
	}
}

// TestInstall_TargetTenant_UnwiredFailsClosed: WithCrossTenantInstall never called → any
// ?targetTenant request is refused 403 (honest-disabled, fail-closed).
func TestInstall_TargetTenant_UnwiredFailsClosed(t *testing.T) {
	svc := &fakeVerticalSvc{}
	r := mscRouter(svc, nil, nil, nil)

	w := postInstall(t, r, "?targetTenant=khach-b", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when MS-C deps are unwired, got %d (%s)", w.Code, w.Body.String())
	}
	if len(svc.installTenants) != 0 {
		t.Errorf("install ran despite unwired gate: %v", svc.installTenants)
	}
}

// TestInstall_TargetTenant_DryRunPlansAgainstTarget: ?dryRun=true&targetTenant=… plans against
// the TARGET tenant (wizard step 4) and emits NO audit event (zero writes anywhere).
func TestInstall_TargetTenant_DryRunPlansAgainstTarget(t *testing.T) {
	svc := &fakeVerticalSvc{}
	aud := &fakeAuditor{}
	resolver := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{
		"khach-b": {ID: mscTargetTenant, Slug: "khach-b", Status: "active"},
	}}
	r := mscRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, resolver, aud)

	w := postInstall(t, r, "?targetTenant=khach-b&dryRun=true", map[string]any{"builtin": "crm"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(svc.dryRunTenants) != 1 || svc.dryRunTenants[0] != mscTargetTenant {
		t.Fatalf("dry-run tenants = %v, want [%s]", svc.dryRunTenants, mscTargetTenant)
	}
	if len(svc.installTenants) != 0 {
		t.Errorf("real install ran on a dry-run: %v", svc.installTenants)
	}
	if len(aud.events) != 0 {
		t.Errorf("audit events = %+v, want none on a dry-run", aud.events)
	}
}
