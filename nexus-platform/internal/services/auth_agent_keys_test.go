package services

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Issue #3: an MCP agent key minted from a stolen session must not survive the victim's incident
// response. Every credential-level event (they all go through endAllSessions) ends the user's keys.

type recordingAgentKeys struct {
	mu    sync.Mutex
	ended []string // "tenant/user"
	err   error
}

func (r *recordingAgentKeys) RevokeAllForUser(_ context.Context, tenantID, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, tenantID+"/"+userID)
	return r.err
}

func TestCredentialEvents_EndTheUsersAgentKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(svc *AuthService, password, keepSession string) error
	}{
		{"logout-all", func(svc *AuthService, _, _ string) error {
			return svc.LogoutEverywhere(context.Background(), "t-1", "u-1")
		}},
		{"password change (the calling device stays signed in, its agent keys do not)", func(svc *AuthService, password, keep string) error {
			return svc.ChangePassword(context.Background(), "t-1", "u-1", password, "Br4nd-New-Pw!", keep)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, password := newTestService(t)
			keys := &recordingAgentKeys{}
			svc = svc.WithAgentKeys(keys)
			a := loginAlice(t, svc, password)
			if err := tc.act(svc, password, sidOfAccess(t, svc, a.AccessToken)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(keys.ended) != 1 || keys.ended[0] != "t-1/u-1" {
				t.Fatalf("agent keys ended for %v; want exactly t-1/u-1", keys.ended)
			}
		})
	}
}

// A key that could not be revoked is not a silent success: logout-all reports the failure.
func TestLogoutEverywhere_AgentKeyRevokeFailureIsAnError(t *testing.T) {
	svc, password := newTestService(t)
	svc = svc.WithAgentKeys(&recordingAgentKeys{err: errors.New("db down")})
	loginAlice(t, svc, password)
	if err := svc.LogoutEverywhere(context.Background(), "t-1", "u-1"); err == nil {
		t.Fatal("logout-all succeeded while the agent keys could not be revoked")
	}
}
