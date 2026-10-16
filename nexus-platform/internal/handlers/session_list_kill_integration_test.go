//go:build integration

// specs/055 session-list-kill — proof against a REAL MySQL 8 migrated by golang-migrate (the A4
// harness: production router, WithLiveSubject, real login/refresh). Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -p 4 -run 'TestSessions_' ./internal/handlers/
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	slTenantID   = "a4510000-0000-0000-0000-0000000005a1"
	slTenantSlug = "a4sl-acme"
	slUAMac      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	slUAIPhone   = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

type slFixture struct {
	rt       *a4Router
	adminTok string
	memberID string
}

func slSetup(t *testing.T) *slFixture {
	t.Helper()
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, slTenantID, slTenantSlug, "Acme Sessions")
	a4SeedUser(t, gdb, slTenantID, "admin@"+slTenantSlug, true, nil)
	memberID := a4SeedUser(t, gdb, slTenantID, "member@"+slTenantSlug, false, nil)
	adminTok := rt.login(t, slTenantSlug, "admin@"+slTenantSlug).Data.AccessToken
	return &slFixture{rt: rt, adminTok: adminTok, memberID: memberID}
}

// slSend fires one request from a given device (User-Agent + client address).
func slSend(t *testing.T, f *slFixture, method, path, token, body, ua, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.rt.engine.ServeHTTP(w, req)
	return w
}

type slTokens struct {
	Access, Refresh string
}

func slLogin(t *testing.T, f *slFixture, email, ua, remote string) slTokens {
	t.Helper()
	w := slSend(t, f, http.MethodPost, "/api/v1/auth/login", "",
		`{"tenant":"`+slTenantSlug+`","email":"`+email+`","password":"`+authPassword+`"}`, ua, remote)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, w.Code, w.Body.String())
	}
	var resp loginResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return slTokens{resp.Data.AccessToken, resp.Data.RefreshToken}
}

func slRefresh(t *testing.T, f *slFixture, refresh, ua, remote string) (slTokens, int) {
	t.Helper()
	w := slSend(t, f, http.MethodPost, "/api/v1/auth/refresh", "", `{"refreshToken":"`+refresh+`"}`, ua, remote)
	var resp loginResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return slTokens{resp.Data.AccessToken, resp.Data.RefreshToken}, w.Code
}

type slSession struct {
	ID         string    `json:"id"`
	Browser    string    `json:"browser"`
	OS         string    `json:"os"`
	UserAgent  string    `json:"userAgent"`
	IP         string    `json:"ip"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	Current    bool      `json:"current"`
}

func slList(t *testing.T, f *slFixture, path, token string) []slSession {
	t.Helper()
	w := slSend(t, f, http.MethodGet, path, token, "", slUAMac, "113.161.42.87:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var env struct {
		Data []slSession `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return env.Data
}

// slLivePerSession counts live refresh rows per session of one user straight from MySQL.
func slLivePerSession(t *testing.T, f *slFixture, userID string) map[string]int {
	t.Helper()
	rows, err := f.rt.sx.Query(`SELECT COALESCE(session_id, ''), COUNT(*) FROM refresh_tokens
		WHERE tenant_id = ? AND user_id = ? AND revoked_at IS NULL GROUP BY session_id`, slTenantID, userID)
	if err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sid string
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[sid] = n
	}
	return out
}

// US1 (#44): two browsers ⇒ two sessions with their device; a refresh keeps the chain (same id,
// same device label, one live row per chain) and moves last activity + IP.
func TestSessions_TwoDevicesListedAndRotationKeepsTheChain(t *testing.T) {
	f := slSetup(t)
	mac := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	phone := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")

	before := slList(t, f, "/api/v1/auth/sessions", mac.Access)
	if len(before) != 2 {
		t.Fatalf("member sees %d sessions, want 2: %+v", len(before), before)
	}
	byOS := map[string]slSession{}
	for _, s := range before {
		byOS[s.OS] = s
	}
	if s := byOS["macOS"]; s.Browser != "Chrome" || s.IP != "113.161.42.x" || s.UserAgent != slUAMac || s.LastUsedAt.IsZero() {
		t.Errorf("Mac session = %+v", s)
	}
	if s := byOS["iOS"]; s.Browser != "Safari" || s.IP != "42.118.7.x" {
		t.Errorf("iPhone session = %+v", s)
	}
	for sid, n := range slLivePerSession(t, f, f.memberID) {
		if sid == "" || n != 1 {
			t.Errorf("session %q has %d live rows, want exactly 1 with a non-empty id", sid, n)
		}
	}

	time.Sleep(1100 * time.Millisecond) // TIMESTAMP has second precision: let last_used_at move
	rotated, code := slRefresh(t, f, mac.Refresh, "SomeUpdatedUA/2.0", "14.232.1.2:40000")
	if code != http.StatusOK {
		t.Fatalf("refresh mac: %d", code)
	}

	after := slList(t, f, "/api/v1/auth/sessions", rotated.Access)
	if len(after) != 2 {
		t.Fatalf("after rotation: %d sessions, want still 2 (one per device): %+v", len(after), after)
	}
	var macAfter slSession
	for _, s := range after {
		if s.ID == byOS["macOS"].ID {
			macAfter = s
		}
	}
	if macAfter.ID == "" {
		t.Fatalf("the Mac's session id changed on rotation: before %q, after %+v", byOS["macOS"].ID, after)
	}
	if macAfter.UserAgent != slUAMac || macAfter.Browser != "Chrome" {
		t.Errorf("rotation rewrote the device label: %+v", macAfter)
	}
	if macAfter.IP != "14.232.1.x" {
		t.Errorf("IP after rotation = %q, want the refresh's network 14.232.1.x", macAfter.IP)
	}
	if !macAfter.LastUsedAt.After(byOS["macOS"].LastUsedAt) {
		t.Errorf("lastUsedAt did not move on rotation: %v → %v", byOS["macOS"].LastUsedAt, macAfter.LastUsedAt)
	}
	live := slLivePerSession(t, f, f.memberID)
	if len(live) != 2 || live[byOS["macOS"].ID] != 1 || live[byOS["iOS"].ID] != 1 {
		t.Errorf("live rows per session after rotation = %v, want exactly one per chain", live)
	}
	_ = phone
}

// A row whose device columns are NULL (issued before the device was recorded) still lists: GORM
// scans NULL into "" / nil and lastUsedAt falls back to created_at.
func TestSessions_RowWithNullDeviceColumnsStillLists(t *testing.T) {
	f := slSetup(t)
	tok := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	sid := uuid.NewString()
	if _, err := f.rt.sx.Exec(`INSERT INTO refresh_tokens (id, user_id, tenant_id, session_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), f.memberID, slTenantID, sid, strings.Repeat("a", 64),
		time.Now().Add(time.Hour).UTC(), time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	got := slList(t, f, "/api/v1/auth/sessions", tok.Access)
	var legacy *slSession
	for i := range got {
		if got[i].ID == sid {
			legacy = &got[i]
		}
	}
	if legacy == nil || legacy.UserAgent != "" || legacy.IP != "" || legacy.LastUsedAt.IsZero() {
		t.Fatalf("legacy row = %+v in %+v, want listed with empty device and lastUsedAt = created_at", legacy, got)
	}
}

// A User-Agent carrying a byte that is not UTF-8 (obs-text: net/http accepts it) must not fail the
// login or the refresh: the utf8mb4 column refuses such bytes in strict mode, so the device label is
// cleaned before it is stored. The refresh case is a legacy row (NULL user_agent, pre-055), whose
// rotation takes the REQUEST's User-Agent because the chain has none to keep.
func TestSessions_NonUTF8UserAgentStillLogsInAndRefreshes(t *testing.T) {
	f := slSetup(t)
	const badUA = "Mozilla/5.0 caf\xe9"
	tok := slLogin(t, f, "member@"+slTenantSlug, badUA, "113.161.42.87:51000")

	if _, err := f.rt.sx.Exec(`UPDATE refresh_tokens SET user_agent = NULL WHERE tenant_id = ? AND user_id = ?`, slTenantID, f.memberID); err != nil {
		t.Fatalf("make the row legacy: %v", err)
	}
	rotated, code := slRefresh(t, f, tok.Refresh, badUA, "113.161.42.87:51000")
	if code != http.StatusOK {
		t.Fatalf("refresh with a non-UTF-8 User-Agent = %d, want 200", code)
	}
	got := slList(t, f, "/api/v1/auth/sessions", rotated.Access)
	if len(got) != 1 || got[0].UserAgent != "Mozilla/5.0 caf" {
		t.Fatalf("sessions = %+v, want one with the cleaned label %q", got, "Mozilla/5.0 caf")
	}
}

// US3 (read half): the tenant admin lists a member's sessions; the member cannot use the admin
// route; a user of no tenant of the admin's is 404.
func TestSessions_AdminListsAMembersSessions(t *testing.T) {
	f := slSetup(t)
	member := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")

	got := slList(t, f, "/api/v1/admin/users/"+f.memberID+"/sessions", f.adminTok)
	if len(got) != 1 || got[0].OS != "iOS" {
		t.Fatalf("admin sees %+v, want the member's one iPhone session", got)
	}
	w := slSend(t, f, http.MethodGet, "/api/v1/admin/users/"+f.memberID+"/sessions", member.Access, "", slUAIPhone, "42.118.7.9:62000")
	if w.Code != http.StatusForbidden {
		t.Fatalf("member on the admin route = %d, want 403", w.Code)
	}
	w = slSend(t, f, http.MethodGet, "/api/v1/admin/users/"+uuid.NewString()+"/sessions", f.adminTok, "", slUAMac, "1.2.3.4:1")
	if w.Code != http.StatusNotFound || cfCode(w.Body.Bytes()) != "USER_NOT_FOUND" {
		t.Fatalf("unknown user = %d %s, want 404 USER_NOT_FOUND", w.Code, w.Body.String())
	}
}

// --- #45 session-list-kill-b ---------------------------------------------------------------------

// slCurrentSID returns the id of the session the list marks as the caller's ("phiên này").
func slCurrentSID(t *testing.T, f *slFixture, token string) string {
	t.Helper()
	for _, s := range slList(t, f, "/api/v1/auth/sessions", token) {
		if s.Current {
			return s.ID
		}
	}
	t.Fatal("no session marked current for this caller")
	return ""
}

func slGet(t *testing.T, f *slFixture, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return slSend(t, f, http.MethodGet, path, token, "", slUAMac, "113.161.42.87:51000")
}

// US2 (#45): browser A ends browser B. B's VERY NEXT request is 401 (no waiting for the access TTL),
// its /auth/me and /auth/refresh are 401 → the FE lands on /login; A keeps working AND keeps
// refreshing (no E5 family kill); only B's row is marked ended.
func TestSessions_EndingAnotherDeviceIsImmediateAndSparesTheCaller(t *testing.T) {
	f := slSetup(t)
	a := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	b := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")
	sidA := slCurrentSID(t, f, a.Access)
	sidB := slCurrentSID(t, f, b.Access)
	if sidA == sidB {
		t.Fatalf("two browsers share session %s", sidA)
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", b.Access); w.Code != http.StatusOK {
		t.Fatalf("B before = %d", w.Code)
	}

	if w := slSend(t, f, http.MethodDelete, "/api/v1/auth/sessions/"+sidB, a.Access, "", slUAMac, "113.161.42.87:51000"); w.Code != http.StatusNoContent {
		t.Fatalf("A ends B = %d %s, want 204", w.Code, w.Body.String())
	}

	if w := slGet(t, f, "/api/v1/auth/sessions", b.Access); w.Code != http.StatusUnauthorized || cfCode(w.Body.Bytes()) != "UNAUTHORIZED" {
		t.Fatalf("B's next request = %d %s, want 401 UNAUTHORIZED at once", w.Code, w.Body.String())
	}
	if w := slGet(t, f, "/api/v1/auth/me", b.Access); w.Code != http.StatusUnauthorized {
		t.Fatalf("B's /auth/me = %d, want 401", w.Code)
	}
	if _, code := slRefresh(t, f, b.Refresh, slUAIPhone, "42.118.7.9:62000"); code != http.StatusUnauthorized {
		t.Fatalf("B's refresh = %d, want 401", code)
	}

	if w := slGet(t, f, "/api/v1/auth/sessions", a.Access); w.Code != http.StatusOK {
		t.Fatalf("A after ending B = %d, want 200", w.Code)
	}
	rotated, code := slRefresh(t, f, a.Refresh, slUAMac, "113.161.42.87:51000")
	if code != http.StatusOK {
		t.Fatalf("A's refresh after B replayed its ended token = %d — the E5 alarm killed the family", code)
	}
	if got := slCurrentSID(t, f, rotated.Access); got != sidA {
		t.Errorf("A's current session after refresh = %s, want %s (label must not move)", got, sidA)
	}
	var reason string
	if err := f.rt.sx.QueryRow(`SELECT COALESCE(MAX(revoke_reason), '') FROM refresh_tokens WHERE tenant_id = ? AND session_id = ?`, slTenantID, sidB).Scan(&reason); err != nil {
		t.Fatalf("read reason: %v", err)
	}
	if reason != "ended" {
		t.Errorf("B's rows revoke_reason = %q, want ended", reason)
	}
}

// Nobody ends someone else's session through the self route: another user's id is 404, and that
// session keeps working.
func TestSessions_CannotEndAnotherUsersSession(t *testing.T) {
	f := slSetup(t)
	member := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	adminSID := slCurrentSID(t, f, f.adminTok)

	w := slSend(t, f, http.MethodDelete, "/api/v1/auth/sessions/"+adminSID, member.Access, "", slUAMac, "113.161.42.87:51000")
	if w.Code != http.StatusNotFound || cfCode(w.Body.Bytes()) != "SESSION_NOT_FOUND" {
		t.Fatalf("member ends admin's session = %d %s, want 404 SESSION_NOT_FOUND", w.Code, w.Body.String())
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", f.adminTok); w.Code != http.StatusOK {
		t.Fatalf("admin's session hurt: %d", w.Code)
	}
	w = slSend(t, f, http.MethodDelete, "/api/v1/admin/users/"+f.memberID+"/sessions/"+adminSID, member.Access, "", slUAMac, "113.161.42.87:51000")
	if w.Code != http.StatusForbidden {
		t.Fatalf("member on the admin end route = %d, want 403", w.Code)
	}
}

// US3: the tenant admin ends a member's session — the member's next request is 401.
func TestSessions_AdminEndsAMembersSession(t *testing.T) {
	f := slSetup(t)
	member := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")
	sid := slCurrentSID(t, f, member.Access)

	path := "/api/v1/admin/users/" + f.memberID + "/sessions/" + sid
	if w := slSend(t, f, http.MethodDelete, path, f.adminTok, "", slUAMac, "1.2.3.4:1"); w.Code != http.StatusNoContent {
		t.Fatalf("admin end = %d %s, want 204", w.Code, w.Body.String())
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", member.Access); w.Code != http.StatusUnauthorized {
		t.Fatalf("member's next request = %d, want 401", w.Code)
	}
	if w := slSend(t, f, http.MethodDelete, path, f.adminTok, "", slUAMac, "1.2.3.4:1"); w.Code != http.StatusNotFound {
		t.Fatalf("ending it again = %d, want 404", w.Code)
	}
}

// FR-012: /auth/logout ends the session. Two tabs of one browser share it; one tab logs out. The
// other tab's access token is refused at once, and its refresh with the same (now ended) token is a
// plain 401 — not the E5 theft alarm, which would sign the user out of the phone too.
func TestSessions_LogoutEndsTheSessionWithoutATheftAlarm(t *testing.T) {
	f := slSetup(t)
	tab := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	phone := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")
	sid := slCurrentSID(t, f, tab.Access)

	if w := slSend(t, f, http.MethodPost, "/api/v1/auth/logout", "", `{"refreshToken":"`+tab.Refresh+`"}`, slUAMac, "113.161.42.87:51000"); w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s, want 204", w.Code, w.Body.String())
	}
	if w := slGet(t, f, "/api/v1/auth/sessions", tab.Access); w.Code != http.StatusUnauthorized {
		t.Fatalf("the other tab's next request = %d, want 401 at once", w.Code)
	}
	if _, code := slRefresh(t, f, tab.Refresh, slUAMac, "113.161.42.87:51000"); code != http.StatusUnauthorized {
		t.Fatalf("the other tab's refresh = %d, want 401", code)
	}
	if _, code := slRefresh(t, f, phone.Refresh, slUAIPhone, "42.118.7.9:62000"); code != http.StatusOK {
		t.Fatalf("the phone's refresh after the tab replayed its logged-out token = %d — the E5 alarm killed the family", code)
	}
	var reason string
	if err := f.rt.sx.QueryRow(`SELECT COALESCE(MAX(revoke_reason), '') FROM refresh_tokens WHERE tenant_id = ? AND session_id = ?`, slTenantID, sid).Scan(&reason); err != nil {
		t.Fatalf("read reason: %v", err)
	}
	if reason != "ended" {
		t.Errorf("logged-out session revoke_reason = %q, want ended", reason)
	}
}

// FR-010: logout-all is immediate on every other device now.
func TestSessions_LogoutAllIsImmediate(t *testing.T) {
	f := slSetup(t)
	a := slLogin(t, f, "member@"+slTenantSlug, slUAMac, "113.161.42.87:51000")
	b := slLogin(t, f, "member@"+slTenantSlug, slUAIPhone, "42.118.7.9:62000")
	if w := slSend(t, f, http.MethodPost, "/api/v1/auth/logout-all", a.Access, "{}", slUAMac, "113.161.42.87:51000"); w.Code != http.StatusNoContent {
		t.Fatalf("logout-all = %d", w.Code)
	}
	for name, tok := range map[string]string{"other device": b.Access, "this device": a.Access} {
		if w := slGet(t, f, "/api/v1/auth/sessions", tok); w.Code != http.StatusUnauthorized {
			t.Errorf("%s after logout-all = %d, want 401 at once", name, w.Code)
		}
	}
}
