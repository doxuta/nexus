//go:build integration

// A4 RBAC — W1 refresh-token revocation integration tests (coverage point T6) against a REAL
// MySQL 8 brought up with golang-migrate (see a4_rbac_harness_integration_test.go). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// W1 (../specs/001-auth-security/spec.md §5 claim-freshness): an access token CARRIES role_id/is_admin
// in its payload. Before #14 that payload was authoritative, so a demoted or disabled user kept the
// old privilege until the token expired; now the Auth stage re-reads those fields from the users row
// on every request, so the change bites on the NEXT request. Revoking refresh tokens on a
// role_id/is_admin change, a disable, a delete or a password change is still required — it stops the
// user minting a fresh token, and for a PASSWORD change it is the only bound there is, because
// refreshSubject does not read the password.
//
// This file proves the revoke END TO END through the live stack: a real login issues a refresh
// token, an admin performs the privilege change via /admin/users, and the user's PRE-CHANGE refresh
// token is then rejected by /auth/refresh (401). A control case (a benign profile-only update) must
// NOT revoke — the old refresh token still works — so the revoke is targeted, not a blanket logout.
package handlers

import (
	"net/http"
	"sync"
	"testing"

	"nexus/platform/internal/auth"
)

const (
	w1TenantID   = "a4w10000-0000-0000-0000-0000000000c3"
	w1TenantSlug = "a4w1-acme"
)

// w1RefreshStillWorks reports whether the given refresh token can still be exchanged at
// /auth/refresh (200). A revoked / single-used / unknown refresh token yields a non-200.
func w1RefreshStillWorks(t *testing.T, rt *a4Router, refreshToken string) (bool, int) {
	t.Helper()
	w := postRefresh(t, rt.engine, `{"refreshToken":"`+refreshToken+`"}`)
	return w.Code == http.StatusOK, w.Code
}

// TestA4_W1_RoleChangeRevokesRefresh is T6 (role change): a member logs in, an admin changes the
// member's role_id via PUT /admin/users/:id, and the member's PRE-CHANGE refresh token is then
// rejected by /auth/refresh — they cannot mint a fresh access token under the old role.
func TestA4_W1_RoleChangeRevokesRefresh(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	a4SeedUser(t, gdb, w1TenantID, "admin@"+w1TenantSlug, true, nil)

	oldRole := a4SeedRole(t, gdb, w1TenantID, "old-role", nil)
	newRole := a4SeedRole(t, gdb, w1TenantID, "new-role", nil)
	memberID := a4SeedUser(t, gdb, w1TenantID, "member@"+w1TenantSlug, false, &oldRole)

	adminTok := rt.login(t, w1TenantSlug, "admin@"+w1TenantSlug).Data.AccessToken

	// Member logs in → obtains a refresh token tied to the OLD role.
	memberLogin := rt.login(t, w1TenantSlug, "member@"+w1TenantSlug)
	oldRefresh := memberLogin.Data.RefreshToken

	// Pre-change sanity: the refresh token works (and rotates). Re-login so we keep an un-rotated
	// token for the post-change assertion (the rotation here single-uses oldRefresh).
	if ok, code := w1RefreshStillWorks(t, rt, oldRefresh); !ok {
		t.Fatalf("pre-change refresh should work, got %d", code)
	}
	memberLogin2 := rt.login(t, w1TenantSlug, "member@"+w1TenantSlug)
	liveRefresh := memberLogin2.Data.RefreshToken

	// Admin changes the member's role_id → W1 revoke fires for the member.
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/users/"+memberID, adminTok,
		map[string]any{"roleId": newRole}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin change member role: %d %s", wu.Code, wu.Body.String())
	}

	// The member's live (pre-change) refresh token must now be REJECTED.
	if ok, code := w1RefreshStillWorks(t, rt, liveRefresh); ok {
		t.Fatalf("refresh after role change still worked (code %d); W1 revoke did not fire", code)
	}
}

// TestA4_W1_DemoteAdminRevokesRefresh is T6 (is_admin change): demoting a user from admin revokes
// their refresh tokens. A sibling admin is seeded so the anti-lock-out guard permits the demotion.
func TestA4_W1_DemoteAdminRevokesRefresh(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	a4SeedUser(t, gdb, w1TenantID, "boss@"+w1TenantSlug, true, nil)               // sibling admin (survives)
	demoteID := a4SeedUser(t, gdb, w1TenantID, "demote@"+w1TenantSlug, true, nil) // the one demoted

	bossTok := rt.login(t, w1TenantSlug, "boss@"+w1TenantSlug).Data.AccessToken
	demoteLogin := rt.login(t, w1TenantSlug, "demote@"+w1TenantSlug)
	liveRefresh := demoteLogin.Data.RefreshToken

	// Demote: is_admin → false. W1 revoke fires for the demoted user.
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/users/"+demoteID, bossTok,
		map[string]any{"isAdmin": false}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin demote: %d %s", wu.Code, wu.Body.String())
	}

	if ok, code := w1RefreshStillWorks(t, rt, liveRefresh); ok {
		t.Fatalf("refresh after demote still worked (code %d); W1 revoke did not fire", code)
	}
}

// TestA4_W1_DeleteUserRevokesRefresh is T6 (delete): deleting a user revokes their refresh tokens,
// so a removed account cannot re-authenticate via a still-live refresh token.
func TestA4_W1_DeleteUserRevokesRefresh(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	a4SeedUser(t, gdb, w1TenantID, "admin@"+w1TenantSlug, true, nil) // sole admin survives
	victimID := a4SeedUser(t, gdb, w1TenantID, "victim@"+w1TenantSlug, false, nil)

	adminTok := rt.login(t, w1TenantSlug, "admin@"+w1TenantSlug).Data.AccessToken
	victimLogin := rt.login(t, w1TenantSlug, "victim@"+w1TenantSlug)
	liveRefresh := victimLogin.Data.RefreshToken

	// Pre-delete sanity.
	if ok, code := w1RefreshStillWorks(t, rt, victimLogin.Data.RefreshToken); !ok {
		t.Fatalf("pre-delete refresh should work, got %d", code)
	}
	// Re-login to keep an un-rotated live token (the sanity check above single-used the first one).
	victimLogin2 := rt.login(t, w1TenantSlug, "victim@"+w1TenantSlug)
	liveRefresh = victimLogin2.Data.RefreshToken

	wd := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/users/"+victimID, adminTok, nil, nil)
	if wd.Code != http.StatusNoContent {
		t.Fatalf("admin delete victim: %d %s; want 204", wd.Code, wd.Body.String())
	}

	if ok, code := w1RefreshStillWorks(t, rt, liveRefresh); ok {
		t.Fatalf("refresh after delete still worked (code %d); W1 revoke did not fire", code)
	}
}

// TestA4_W1_BenignUpdateDoesNotRevoke is the T6 control: an update that touches NO privilege claim
// (only full_name) must NOT revoke — a benign profile edit should not invalidate the user's live
// refresh token.
func TestA4_W1_BenignUpdateDoesNotRevoke(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	a4SeedUser(t, gdb, w1TenantID, "admin@"+w1TenantSlug, true, nil)
	memberID := a4SeedUser(t, gdb, w1TenantID, "member@"+w1TenantSlug, false, nil)

	adminTok := rt.login(t, w1TenantSlug, "admin@"+w1TenantSlug).Data.AccessToken
	memberLogin := rt.login(t, w1TenantSlug, "member@"+w1TenantSlug)
	liveRefresh := memberLogin.Data.RefreshToken

	// Benign: change only full_name.
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/users/"+memberID, adminTok,
		map[string]any{"fullName": "Renamed Member"}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin benign update: %d %s", wu.Code, wu.Body.String())
	}

	// The member's refresh token must STILL work — no revoke on a non-privilege change.
	if ok, code := w1RefreshStillWorks(t, rt, liveRefresh); !ok {
		t.Fatalf("benign update revoked the refresh token (code %d); W1 should NOT fire on full_name-only", code)
	}
}

// TestA4_W1_DisableUserRevokesRefresh covers bug §6: disabling a user (status → "disabled") is the
// strongest deprovisioning, so it MUST revoke their refresh tokens — otherwise the disabled user
// keeps minting fresh access tokens via /auth/refresh for the full RefreshTTL even though login is
// blocked. An admin disables a member via PUT /admin/users/:id; the member's pre-change refresh
// token must then be rejected by /auth/refresh.
func TestA4_W1_DisableUserRevokesRefresh(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	a4SeedUser(t, gdb, w1TenantID, "admin@"+w1TenantSlug, true, nil)
	memberID := a4SeedUser(t, gdb, w1TenantID, "member@"+w1TenantSlug, false, nil)

	adminTok := rt.login(t, w1TenantSlug, "admin@"+w1TenantSlug).Data.AccessToken
	memberLogin := rt.login(t, w1TenantSlug, "member@"+w1TenantSlug)
	liveRefresh := memberLogin.Data.RefreshToken

	// Admin disables the member → W1 revoke must fire for the member (status leaving "active").
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/users/"+memberID, adminTok,
		map[string]any{"status": "disabled"}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin disable member: %d %s", wu.Code, wu.Body.String())
	}

	// The disabled member's pre-change refresh token must now be REJECTED — they cannot mint a
	// fresh access token after being deprovisioned.
	if ok, code := w1RefreshStillWorks(t, rt, liveRefresh); ok {
		t.Fatalf("refresh after disable still worked (code %d); §6 W1-on-disable did not fire", code)
	}
}

// TestA4_LastAdminTOCTOU_ConcurrentDemoteKeepsOneAdmin covers bug §19: the last-admin guard used to
// be a service-side check-then-act with no lock, so two demotions racing at the count==2 boundary
// could BOTH pass and drop the tenant to ZERO active admins. The fix runs the count under a FOR
// UPDATE lock in the SAME transaction as the write (UpdateUserGuardLastAdmin), serializing the two
// transactions so exactly one demotion succeeds and at least one active admin always survives.
//
// The test seeds exactly two active admins (A, B) and fires two concurrent PUT demotions — A
// demotes B, B demotes A — through the live router. Without the lock both would 200 and leave zero
// admins; with it, exactly one returns 200 and the other 422 LAST_ADMIN, and the DB ends with ≥1
// active admin.
func TestA4_LastAdminTOCTOU_ConcurrentDemoteKeepsOneAdmin(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w1TenantID, w1TenantSlug, "Acme W1")
	adminAID := a4SeedUser(t, gdb, w1TenantID, "admin-a@"+w1TenantSlug, true, nil)
	adminBID := a4SeedUser(t, gdb, w1TenantID, "admin-b@"+w1TenantSlug, true, nil)

	tokA := rt.login(t, w1TenantSlug, "admin-a@"+w1TenantSlug).Data.AccessToken
	tokB := rt.login(t, w1TenantSlug, "admin-b@"+w1TenantSlug).Data.AccessToken

	// Two concurrent demotions, each targeting the OTHER admin. Both targets are active admins and
	// the starting active-admin count is exactly 2 — the precise TOCTOU boundary.
	type result struct{ code int }
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	fire := func(actorTok, targetID string) {
		start.Wait() // release both goroutines together to maximize transaction overlap
		w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/users/"+targetID, actorTok,
			map[string]any{"isAdmin": false}, nil)
		results <- result{code: w.Code}
	}
	go fire(tokA, adminBID)
	go fire(tokB, adminAID)
	start.Done()

	codes := []int{(<-results).code, (<-results).code}
	ok200, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok200++
		// 422 LAST_ADMIN: the loser reached the service, which refused to remove the last admin.
		// 403 FORBIDDEN: since #14 the Auth stage re-reads is_admin from the users row on every
		// request (refreshSubject, middleware/auth.go:150-165), so if the winner's transaction
		// commits first the loser's OWN privilege is already gone and RequireAdmin
		// (middleware/auth.go:206-224) refuses at the gate, before the handler can answer 422.
		// Both are correct refusals of the same attempt; which one appears is pure timing, so
		// pinning the loser to 422 would have made this test flake under CI load — exactly where
		// a race-dependent assertion is most likely to fire and least likely to be reproducible.
		case http.StatusUnprocessableEntity, http.StatusForbidden:
			refused++
		}
	}
	// EXACTLY one demotion may win; the other must be refused. (If the race were unguarded we'd
	// see two 200s here.)
	if ok200 != 1 || refused != 1 {
		t.Fatalf("concurrent demotes gave codes %v; want exactly one 200 and one refusal (422 LAST_ADMIN or 403 FORBIDDEN)", codes)
	}

	// And the tenant must still have at least one active admin in the DB — never zero.
	var remaining int64
	if err := gdb.Model(&auth.User{}).
		Where("tenant_id = ? AND is_admin = ? AND status = ?", w1TenantID, true, "active").
		Count(&remaining).Error; err != nil {
		t.Fatalf("count remaining admins: %v", err)
	}
	if remaining < 1 {
		t.Fatalf("tenant left with %d active admins after concurrent demotes; want >= 1 (TOCTOU not closed)", remaining)
	}
}
