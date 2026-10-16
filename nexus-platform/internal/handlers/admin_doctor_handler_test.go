package handlers

// httptest coverage for AdminDoctorHandler (../specs/008-nexus-doctor/spec.md §2.3): the admin gate
// (RequireAdmin refuses a non-admin with 403 before the handler runs), the scan/fix envelope
// shapes (camelCase, scannedAt from the handler-owned clock), and the 422 DOCTOR_CHECK_UNKNOWN
// mapping. The service behaviour itself is pinned in internal/services/doctor_service_test.go —
// here the service is a three-method fake behind the consumer-side DoctorRunner interface.

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
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

type fakeDoctorRunner struct {
	scanResults []doctor.CheckResult
	scanErr     error
	fixResults  []doctor.FixResult
	fixErr      error
	gotCheckIDs []string
	gotConfirm  bool
	flushed     []string
	flushCalls  int
	flushTenant string
}

func (f *fakeDoctorRunner) Scan(_ context.Context, _ string) ([]doctor.CheckResult, error) {
	return f.scanResults, f.scanErr
}

func (f *fakeDoctorRunner) Fix(_ context.Context, _ string, checkIDs []string, confirm bool) ([]doctor.FixResult, error) {
	f.gotCheckIDs, f.gotConfirm = checkIDs, confirm
	return f.fixResults, f.fixErr
}

func (f *fakeDoctorRunner) FlushCaches(_ context.Context, tenantID string) []string {
	f.flushCalls++
	f.flushTenant = tenantID
	return f.flushed
}

// doctorServe runs one request through a fresh engine wearing the principal shim + the REAL
// RequireAdmin gate in front of the doctor routes — so the admin refusal is exercised by the
// same middleware production mounts (router.go adminGated group).
func doctorServe(t *testing.T, sub auth.Subject, svc DoctorRunner, clock func() time.Time, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewAdminDoctorHandler(svc, zap.NewNop())
	if clock != nil {
		h.now = clock
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", sub.TenantID)
		c.Set("authClaims", &auth.Claims{Subject: sub})
		c.Next()
	})
	api := r.Group("/api/v1")
	gated := api.Group("")
	gated.Use(middleware.RequireAdmin(zap.NewNop()))
	h.RegisterRoutes(gated)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var doctorAdmin = auth.Subject{UserID: "u-admin", TenantID: "tenant-a", IsAdmin: true}
var doctorUser = auth.Subject{UserID: "u-1", TenantID: "tenant-a", RoleID: "role-1"}

func TestDoctorHandler_AdminGate(t *testing.T) {
	svc := &fakeDoctorRunner{}
	for _, path := range []string{"/api/v1/admin/doctor/scan", "/api/v1/admin/doctor/fix", "/api/v1/admin/doctor/flush-caches"} {
		w := doctorServe(t, doctorUser, svc, nil, http.MethodPost, path, `{}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s non-admin: status = %d (%s); want 403", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"FORBIDDEN"`) {
			t.Fatalf("%s non-admin body = %s; want code FORBIDDEN", path, w.Body.String())
		}
	}
	if svc.flushCalls != 0 {
		t.Fatalf("flush chạy %d lần dù non-admin; want 0", svc.flushCalls)
	}
}

func TestDoctorHandler_FlushCachesEnvelope(t *testing.T) {
	svc := &fakeDoctorRunner{flushed: []string{"meta", "acl", "workflow"}}
	w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/flush-caches", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Flushed []string `json:"flushed"`
			Note    string   `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(resp.Data.Flushed) != 3 || resp.Data.Flushed[0] != "meta" {
		t.Fatalf("flushed = %v; want [meta acl workflow]", resp.Data.Flushed)
	}
	// The replica caveat is STATIC and always shipped (no replica detection — stay honest).
	if resp.Data.Note != doctorFlushNote {
		t.Fatalf("note = %q; want %q", resp.Data.Note, doctorFlushNote)
	}
	if svc.flushCalls != 1 || svc.flushTenant != "tenant-a" {
		t.Fatalf("service nhận calls=%d tenant=%q; want 1 lần với tenant từ JWT", svc.flushCalls, svc.flushTenant)
	}
}

func TestDoctorHandler_ScanEnvelope(t *testing.T) {
	fixedNow := time.Date(2026, 7, 10, 8, 30, 0, 0, time.UTC)
	svc := &fakeDoctorRunner{scanResults: []doctor.CheckResult{{
		CheckID: "B1_PHANTOM_RELATIONSHIPS", Severity: "warn", AutoFixSafe: true,
		Count: 1, Items: []doctor.Finding{{Key: "rel-1", Detail: "phantom"}}, Truncated: false,
	}}}
	w := doctorServe(t, doctorAdmin, svc, func() time.Time { return fixedNow }, http.MethodPost, "/api/v1/admin/doctor/scan", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Findings []struct {
				CheckID     string `json:"checkId"`
				Severity    string `json:"severity"`
				AutoFixSafe bool   `json:"autoFixSafe"`
				Count       int    `json:"count"`
				Items       []struct {
					Key    string `json:"key"`
					Detail string `json:"detail"`
				} `json:"items"`
				Truncated bool `json:"truncated"`
			} `json:"findings"`
			ScannedAt string `json:"scannedAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(resp.Data.Findings) != 1 {
		t.Fatalf("findings = %d; want 1", len(resp.Data.Findings))
	}
	f := resp.Data.Findings[0]
	if f.CheckID != "B1_PHANTOM_RELATIONSHIPS" || f.Count != 1 || !f.AutoFixSafe ||
		len(f.Items) != 1 || f.Items[0].Key != "rel-1" {
		t.Fatalf("finding = %+v; wire shape sai (camelCase contract)", f)
	}
	if resp.Data.ScannedAt != fixedNow.Format(time.RFC3339) {
		t.Fatalf("scannedAt = %q; want %q (clock handler sở hữu)", resp.Data.ScannedAt, fixedNow.Format(time.RFC3339))
	}
}

func TestDoctorHandler_ScanServiceError(t *testing.T) {
	svc := &fakeDoctorRunner{scanErr: context.DeadlineExceeded}
	w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/scan", `{}`)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "DOCTOR_SCAN_FAILED") {
		t.Fatalf("status = %d body = %s; want 500 DOCTOR_SCAN_FAILED", w.Code, w.Body.String())
	}
}

func TestDoctorHandler_FixEnvelopeAndErrors(t *testing.T) {
	t.Run("happy path → 200 results envelope, body forwarded", func(t *testing.T) {
		svc := &fakeDoctorRunner{fixResults: []doctor.FixResult{{
			CheckID: "C2_ORPHAN_NAV_ITEMS", Fixed: 2, Skipped: 1, Failed: 0, Errors: []string{},
		}}}
		w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/fix",
			`{"checkIds":["C2_ORPHAN_NAV_ITEMS"],"confirm":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s); want 200", w.Code, w.Body.String())
		}
		var resp struct {
			Data struct {
				Results []struct {
					CheckID string   `json:"checkId"`
					Fixed   int      `json:"fixed"`
					Skipped int      `json:"skipped"`
					Failed  int      `json:"failed"`
					Errors  []string `json:"errors"`
				} `json:"results"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if len(resp.Data.Results) != 1 || resp.Data.Results[0].Fixed != 2 || resp.Data.Results[0].Skipped != 1 {
			t.Fatalf("results = %+v; wire shape sai", resp.Data.Results)
		}
		if len(svc.gotCheckIDs) != 1 || svc.gotCheckIDs[0] != "C2_ORPHAN_NAV_ITEMS" || !svc.gotConfirm {
			t.Fatalf("service nhận checkIds=%v confirm=%v; body không được forward đúng", svc.gotCheckIDs, svc.gotConfirm)
		}
	})

	t.Run("checkId lạ → 422 DOCTOR_CHECK_UNKNOWN", func(t *testing.T) {
		svc := &fakeDoctorRunner{fixErr: &services.ValidationError{Code: "DOCTOR_CHECK_UNKNOWN", Msg: "checkId không tồn tại"}}
		w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/fix", `{"checkIds":["NOPE"]}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "DOCTOR_CHECK_UNKNOWN") {
			t.Fatalf("status = %d body = %s; want 422 DOCTOR_CHECK_UNKNOWN", w.Code, w.Body.String())
		}
	})

	t.Run("needs-confirm thiếu confirm → 422 DOCTOR_CONFIRM_REQUIRED", func(t *testing.T) {
		svc := &fakeDoctorRunner{fixErr: &services.ValidationError{Code: "DOCTOR_CONFIRM_REQUIRED", Msg: "check này cần confirm=true"}}
		w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/fix", `{"checkIds":["A6_X"]}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "DOCTOR_CONFIRM_REQUIRED") {
			t.Fatalf("status = %d body = %s; want 422 DOCTOR_CONFIRM_REQUIRED", w.Code, w.Body.String())
		}
	})

	t.Run("body hỏng → 400 INVALID_BODY", func(t *testing.T) {
		svc := &fakeDoctorRunner{}
		w := doctorServe(t, doctorAdmin, svc, nil, http.MethodPost, "/api/v1/admin/doctor/fix", `{"checkIds": "not-an-array"}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_BODY") {
			t.Fatalf("status = %d body = %s; want 400 INVALID_BODY", w.Code, w.Body.String())
		}
	})
}
