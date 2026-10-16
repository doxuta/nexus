package handlers

// Purge is the only endpoint in this codebase with no undo, so the thing under test here is not
// the happy path — it is what happens when the CALLER IS SLOPPY. An absent "dryRun" key must not
// be read as Go's zero-value false and delete a customer's data; that is why the request field is
// a pointer, and this is the test that would notice if someone "simplified" it back to a bool.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

func purgeRouter(svc tenantProvisioner) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "factory-tenant")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "op-1", TenantID: "factory-tenant", IsAdmin: true,
		}})
		c.Next()
	})
	h := NewAdminTenantHandler(svc, nil, zap.NewNop())
	r.POST("/api/v1/admin/tenants/:id/purge", h.Purge)
	return r
}

func doPurge(r *gin.Engine, id, body string) (int, map[string]any) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+id+"/purge", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestPurge_SilenceMeansDryRun is the guard on the irreversible default. A body with no dryRun key
// (or an empty object, or a body that only carries the confirmation) must reach the service with
// dryRun=true. Only an explicit false may delete.
func TestPurge_SilenceMeansDryRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"empty object", `{}`, true},
		{"confirmation only, no dryRun key", `{"confirmSlug":"khachb"}`, true},
		{"explicit true", `{"confirmSlug":"khachb","dryRun":true}`, true},
		{"explicit false is the ONLY way to delete", `{"confirmSlug":"khachb","dryRun":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProvisioner{}
			code, _ := doPurge(purgeRouter(f), "t-1", tc.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if f.gotDryRun != tc.want {
				t.Fatalf("service saw dryRun=%v, want %v — an absent key must never delete", f.gotDryRun, tc.want)
			}
		})
	}
}

// TestPurge_ForwardsIdentityAndConfirmation: every guard lives in the service, so the handler's
// contract is simply that it hands over WHO is asking and WHAT they typed, unaltered.
func TestPurge_ForwardsIdentityAndConfirmation(t *testing.T) {
	f := &fakeProvisioner{}
	code, _ := doPurge(purgeRouter(f), "target-9", `{"confirmSlug":"khachb","dryRun":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if f.purgeCall != 1 {
		t.Fatalf("service called %d times, want 1", f.purgeCall)
	}
	if f.gotCaller != "factory-tenant" || f.gotActor != "op-1" || f.gotTarget != "target-9" {
		t.Errorf("identity mangled: caller=%q actor=%q target=%q", f.gotCaller, f.gotActor, f.gotTarget)
	}
	if f.gotConfirm != "khachb" {
		t.Errorf("confirmSlug = %q, want the typed value verbatim", f.gotConfirm)
	}
}

// TestPurge_StatusMapping pins each refusal to the status a client can act on: a not-yet-retired
// workspace and a mistyped slug are things the operator FIXES (422), an unknown id is 404.
func TestPurge_StatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{
			"must retire first",
			&services.ValidationError{Code: "TENANT_PURGE_NOT_RETIRED", Msg: "còn hoạt động"},
			http.StatusUnprocessableEntity, "TENANT_PURGE_NOT_RETIRED",
		},
		{
			"typed the wrong slug",
			&services.ValidationError{Code: "TENANT_PURGE_CONFIRM_MISMATCH", Msg: "gõ đúng slug"},
			http.StatusUnprocessableEntity, "TENANT_PURGE_CONFIRM_MISMATCH",
		},
		{
			"schema grew a child table that would be orphaned",
			&services.ValidationError{Code: "TENANT_PURGE_SCHEMA_DRIFT", Msg: "không cascade"},
			http.StatusUnprocessableEntity, "TENANT_PURGE_SCHEMA_DRIFT",
		},
		{
			"unknown workspace",
			&services.ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy"},
			http.StatusNotFound, "TENANT_NOT_FOUND",
		},
		{
			"purging the workspace you are standing in",
			&services.ValidationError{Code: "TENANT_SELF_PURGE", Msg: "không thể xoá chính mình"},
			http.StatusUnprocessableEntity, "TENANT_SELF_PURGE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProvisioner{purgeErrOnly: tc.err}
			code, body := doPurge(purgeRouter(f), "t-1", `{"confirmSlug":"x","dryRun":false}`)
			if code != tc.want {
				t.Fatalf("status = %d, want %d (%v)", code, tc.want, body)
			}
			if body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}

// TestPurge_MalformedBodyNeverReachesTheService: a broken body on an irreversible endpoint stops
// at the door. Reaching the service with zero-value everything would mean dryRun=false.
func TestPurge_MalformedBodyNeverReachesTheService(t *testing.T) {
	f := &fakeProvisioner{}
	code, body := doPurge(purgeRouter(f), "t-1", `{"confirmSlug":`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", code, body)
	}
	if f.purgeCall != 0 {
		t.Error("a malformed body reached the destructive service call")
	}
}
