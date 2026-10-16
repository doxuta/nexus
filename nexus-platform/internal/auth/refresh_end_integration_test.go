//go:build integration

// #310: EndAllSessions(exceptSessionID) against a real MySQL 8 — the SQL half of "a password change
// ends every OTHER session" (the NULL-session branch is the one an in-memory fake cannot catch).
//
//	bash scripts/run-integration.sh -run 'TestEndAllSessions_' ./internal/auth/
package auth

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (f *rotateFixture) addSession(t *testing.T, sessionID string) {
	t.Helper()
	now := time.Now().UTC()
	rt := &RefreshToken{
		ID: uuid.NewString(), UserID: f.user, TenantID: f.tenant, SessionID: sessionID,
		TokenHash: HashRefreshToken("x-" + uuid.NewString()), LastUsedAt: &now,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := f.repo.Store(context.Background(), rt); err != nil {
		t.Fatalf("seed session %q: %v", sessionID, err)
	}
}

func TestEndAllSessions_ExceptKeepsThatSessionLive(t *testing.T) {
	f := rotateSetup(t) // session f.session (the caller's)
	other := uuid.NewString()
	f.addSession(t, other)
	f.addSession(t, "") // a row of a pre-055 binary: stored as session_id = '' ⇒ "no session id"
	if err := f.db.Exec(`UPDATE refresh_tokens SET session_id = NULL WHERE tenant_id = ? AND user_id = ? AND session_id = ''`, f.tenant, f.user).Error; err != nil {
		t.Fatalf("null the legacy row: %v", err)
	}

	sids, err := f.repo.EndAllSessions(context.Background(), f.tenant, f.user, f.session)
	if err != nil {
		t.Fatalf("EndAllSessions: %v", err)
	}
	if len(sids) != 1 || sids[0] != other {
		t.Errorf("ended session ids = %v, want exactly [%s] (the caller's %s is kept, the NULL one has no id to invalidate)", sids, other, f.session)
	}
	if n := f.liveRows(t); n != 1 {
		t.Errorf("caller's session live rows = %d, want 1", n)
	}
	var live int64
	if err := f.db.Model(&RefreshToken{}).Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL", f.tenant, f.user).Count(&live).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Errorf("user's live rows = %d, want 1 (the legacy NULL-session row must be ended too: `session_id <> ?` is NULL for it)", live)
	}
	if ended, _ := f.repo.IsSessionEnded(context.Background(), f.tenant, f.user, other); !ended {
		t.Error("the other session is not marked ended")
	}
	if ended, _ := f.repo.IsSessionEnded(context.Background(), f.tenant, f.user, f.session); ended {
		t.Error("the kept session was marked ended")
	}
}

func TestEndAllSessions_BlankExceptEndsEverySession(t *testing.T) {
	f := rotateSetup(t)
	other := uuid.NewString()
	f.addSession(t, other)

	sids, err := f.repo.EndAllSessions(context.Background(), f.tenant, f.user, "")
	if err != nil {
		t.Fatalf("EndAllSessions: %v", err)
	}
	sort.Strings(sids)
	want := []string{f.session, other}
	sort.Strings(want)
	if len(sids) != 2 || sids[0] != want[0] || sids[1] != want[1] {
		t.Errorf("ended = %v, want %v", sids, want)
	}
	var live int64
	if err := f.db.Model(&RefreshToken{}).Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL", f.tenant, f.user).Count(&live).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 0 {
		t.Errorf("live rows = %d, want 0", live)
	}
	// Tenant pin: another tenant's identical user id is untouched.
	if _, err := f.repo.EndAllSessions(context.Background(), uuid.NewString(), f.user, ""); err != nil {
		t.Fatalf("cross-tenant EndAllSessions: %v", err)
	}
}
