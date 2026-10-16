package middleware

// specs/055 FR-008: an access token whose login session was ended (DELETE /auth/sessions/:id, the
// admin twin, logout-all) is refused on its VERY NEXT request — the auth stage asks the session
// guard, it does not wait for the token to expire.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

type fakeSessions struct {
	ended bool
	err   error
	calls [][3]string
}

func (f *fakeSessions) SessionEnded(_ context.Context, tenantID, userID, sessionID string) (bool, error) {
	f.calls = append(f.calls, [3]string{tenantID, userID, sessionID})
	return f.ended, f.err
}

func runLiveSessions(t *testing.T, claims *auth.Claims, sessions *fakeSessions) (int, bool) {
	t.Helper()
	v := &fakeVerifier{claims: claims}
	users := &fakeLiveUsers{user: &auth.User{Status: "active"}}
	c, rec := newRequest("Bearer good.token.here")
	AuthMiddlewareLive(v, users, sessions, nil)(c)
	return rec.Code, !c.IsAborted()
}

func claimsWithSID(sid string) *auth.Claims {
	c := sampleClaims()
	c.SessionID = sid
	return c
}

func TestAuthMiddlewareLive_EndedSession_401(t *testing.T) {
	sessions := &fakeSessions{ended: true}
	code, nextRan := runLiveSessions(t, claimsWithSID("s-1"), sessions)
	if nextRan || code != http.StatusUnauthorized {
		t.Fatalf("ended session: status %d nextRan=%v, want 401 and no handler", code, nextRan)
	}
	if want := [3]string{"tenant-abc", "user-123", "s-1"}; len(sessions.calls) != 1 || sessions.calls[0] != want {
		t.Errorf("guard calls = %v, want exactly one for %v (all three from the verified token)", sessions.calls, want)
	}
}

func TestAuthMiddlewareLive_LiveSession_Passes(t *testing.T) {
	code, nextRan := runLiveSessions(t, claimsWithSID("s-1"), &fakeSessions{})
	if !nextRan {
		t.Fatalf("live session refused (status %d)", code)
	}
}

// A token minted before 055 carries no sid: nothing to look up, it rides out its own short TTL.
func TestAuthMiddlewareLive_NoSID_NotChecked(t *testing.T) {
	sessions := &fakeSessions{ended: true} // would refuse IF consulted
	if _, nextRan := runLiveSessions(t, claimsWithSID(""), sessions); !nextRan || len(sessions.calls) != 0 {
		t.Fatalf("sid-less token: nextRan=%v guard calls=%d, want pass-through with no lookup", nextRan, len(sessions.calls))
	}
}

func TestAuthMiddlewareLive_APIKey_SessionNotChecked(t *testing.T) {
	keyClaims := &auth.Claims{
		Subject:   auth.Subject{UserID: "key-1", TenantID: "tenant-abc", SessionID: "s-weird"},
		TokenType: auth.TokenTypeAPIKey,
	}
	sessions := &fakeSessions{ended: true}
	if _, nextRan := runLiveSessions(t, keyClaims, sessions); !nextRan || len(sessions.calls) != 0 {
		t.Fatalf("api-key: nextRan=%v guard calls=%d, want pass-through with no lookup", nextRan, len(sessions.calls))
	}
}

func TestAuthMiddlewareLive_SessionGuardError_FailsClosed500(t *testing.T) {
	code, nextRan := runLiveSessions(t, claimsWithSID("s-1"), &fakeSessions{err: errors.New("db down")})
	if nextRan || code != http.StatusInternalServerError {
		t.Fatalf("guard error: status %d nextRan=%v, want 500 fail-closed", code, nextRan)
	}
}
