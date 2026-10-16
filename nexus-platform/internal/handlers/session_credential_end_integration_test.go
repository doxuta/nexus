//go:build integration

// #310 password-change-ends-sessions — a self password change and an admin-set password END sessions
// (specs/055 FR-013), proven against a REAL MySQL 8 on the production auth stage (the A4 harness with
// the SAME session guard + ender wiring as cmd/server/main.go). Run with:
//
//	bash scripts/run-integration.sh -run 'TestSessionsCredential_' ./internal/handlers/
package handlers

import (
	"net/http"
	"testing"
)

// A password change from device A ends device B at once: B's VERY NEXT request is 401 (not after the
// 15-minute access TTL), its refresh is a plain 401; A, which proved the old password, keeps working
// and keeps refreshing — and B replaying its ended token is not an E5 theft that signs A out.
func TestSessionsCredential_PasswordChangeEndsTheOtherDevicesAtOnce(t *testing.T) {
	f := slSetup(t)
	a := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	b := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")
	if w := slGet(t, f, "/api/v1/auth/sessions", b.Access); w.Code != http.StatusOK {
		t.Fatalf("B before = %d", w.Code)
	}

	body := `{"currentPassword":"` + authPassword + `","newPassword":"Br4nd-New-Pw!"}`
	if w := slSend(t, f, http.MethodPut, "/api/v1/auth/me/password", a.Access, body, slUAMac, "113.161.42.87:51000"); w.Code != http.StatusOK {
		t.Fatalf("A changes the password = %d %s", w.Code, w.Body.String())
	}

	if w := slGet(t, f, "/api/v1/auth/sessions", b.Access); w.Code != http.StatusUnauthorized || cfCode(w.Body.Bytes()) != "UNAUTHORIZED" {
		t.Fatalf("B's next request = %d %s, want 401 UNAUTHORIZED at once", w.Code, w.Body.String())
	}
	if _, code := slRefresh(t, f, b.Refresh, slUAIPhone, "42.118.7.9:62000"); code != http.StatusUnauthorized {
		t.Fatalf("B's refresh = %d, want 401", code)
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", a.Access); w.Code != http.StatusOK {
		t.Fatalf("A (the device that changed it) = %d, want 200", w.Code)
	}
	if _, code := slRefresh(t, f, a.Refresh, slUAMac, "113.161.42.87:51000"); code != http.StatusOK {
		t.Fatalf("A's refresh after B replayed its ended token = %d — the E5 alarm killed the family", code)
	}
}

// An admin setting the member's password signs EVERY device of the member out at once.
func TestSessionsCredential_AdminSetPasswordEndsEveryDeviceAtOnce(t *testing.T) {
	f := slSetup(t)
	a := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	b := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")

	w := slSend(t, f, http.MethodPut, "/api/v1/admin/users/"+f.memberID, f.adminTok, `{"password":"Str0ng-Pass-2026!"}`, slUAMac, "1.2.3.4:1")
	if w.Code != http.StatusOK {
		t.Fatalf("admin sets the password = %d %s", w.Code, w.Body.String())
	}
	for name, tok := range map[string]string{"device A": a.Access, "device B": b.Access} {
		if w := slGet(t, f, "/api/v1/auth/sessions", tok); w.Code != http.StatusUnauthorized {
			t.Errorf("%s after an admin password set = %d, want 401 at once", name, w.Code)
		}
	}
}

// A role change is NOT a sign-out: the plain revoke stays, and the next request is a 403 from the
// fresh claims (claim-freshness), not a 401.
func TestSessionsCredential_RoleChangeIsNotASignOut(t *testing.T) {
	f := slSetup(t)
	a := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")

	// isAdmin flips => adminChanged => the plain-revoke branch of UpdateUser (same branch as a role change).
	w := slSend(t, f, http.MethodPut, "/api/v1/admin/users/"+f.memberID, f.adminTok, `{"isAdmin":true}`, slUAMac, "1.2.3.4:1")
	if w.Code != http.StatusOK {
		t.Fatalf("admin promotes the user = %d %s", w.Code, w.Body.String())
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", a.Access); w.Code != http.StatusOK {
		t.Fatalf("a privilege change signed the member out: %d", w.Code)
	}
}
