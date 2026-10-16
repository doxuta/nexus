package services

// auth_service_me_test.go — specs/062-customer-workspace T030. GET /auth/me with a builder
// workspace-session token: identity (id, e-mail, name, MFA) comes from the builder's HOME user row,
// the tenant is the CUSTOMER, the session block names the workspace; the same resolver as the Auth
// stage decides liveness (it sits before the home session guard, which would ask the wrong tenant).
// A home token's response is byte-identical to before.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

type fakeMeResolver struct {
	res   *auth.ResolvedWorkspace
	err   error
	calls int
}

func (f *fakeMeResolver) Resolve(_ context.Context, _ *auth.Claims) (*auth.ResolvedWorkspace, error) {
	f.calls++
	return f.res, f.err
}

func sessionAccessToken(t *testing.T, svc *AuthService, mutate func(*auth.Subject)) string {
	t.Helper()
	sub := auth.Subject{
		UserID: "u-1", TenantID: "customer-1", IsAdmin: true, SessionID: "login-sid",
		ActorUserID: "u-1", ActorTenantID: "t-1", WorkspaceSessionID: "ws-1",
	}
	if mutate != nil {
		mutate(&sub)
	}
	tok, err := svc.tokens.IssueAccessToken(sub)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func liveWorkspace() *auth.ResolvedWorkspace {
	return &auth.ResolvedWorkspace{
		Session: auth.WorkspaceSessionLive{
			ID: "ws-1", TenantID: "customer-1", TenantSlug: "khach", TenantName: "Khách Một",
			ExpiresAt: time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC),
		},
		Actor: auth.LiveActor{ID: "u-1"},
	}
}

func TestAuthService_Me_WorkspaceSession(t *testing.T) {
	svc, _ := newTestService(t)
	resolver := &fakeMeResolver{res: liveWorkspace()}
	svc = svc.WithWorkspaceResolver(resolver)
	// The ENDED guard must not be consulted for a session token (it would ask the customer tenant).
	svc = svc.WithSessionGuard(&recordingGuard{ended: map[string]bool{"login-sid": true}})

	u, err := svc.Me(context.Background(), sessionAccessToken(t, svc, nil))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.ID != "u-1" || u.Email != "alice@acme.test" {
		t.Errorf("identity must come from the HOME user row: %+v", u)
	}
	if u.TenantID != "customer-1" || !u.IsAdmin || u.RoleID != "" {
		t.Errorf("tenant/admin/role = %q/%v/%q, want the customer, admin, no role", u.TenantID, u.IsAdmin, u.RoleID)
	}
	if u.IsPlatformOperator == nil || !*u.IsPlatformOperator {
		t.Error("isPlatformOperator must be true inside a session")
	}
	if u.MFAEnabled == nil {
		t.Error("mfaEnabled must be present (the builder's own MFA state)")
	}
	if u.Workspace == nil || *u.Workspace != (WorkspaceInfo{
		SessionID: "ws-1", TenantID: "customer-1", TenantSlug: "khach", TenantName: "Khách Một",
		ExpiresAt: time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC),
	}) {
		t.Errorf("workspace block = %+v", u.Workspace)
	}
	if resolver.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", resolver.calls)
	}
	if u.PasswordHash != "" {
		t.Error("Me leaked the password hash")
	}
}

func TestAuthService_Me_WorkspaceSessionRefusals(t *testing.T) {
	dbErr := errors.New("db down")
	cases := []struct {
		name     string
		resolver *fakeMeResolver // nil pointer ⇒ none wired
		mutate   func(*auth.Subject)
		wantErr  error
		wantDB   bool
	}{
		{"no resolver wired", nil, nil, ErrUnauthorized, false},
		{"session not live", &fakeMeResolver{err: auth.ErrWorkspaceSessionNotLive}, nil, ErrUnauthorized, false},
		{"builder not live", &fakeMeResolver{err: auth.ErrActorNotLive}, nil, ErrUnauthorized, false},
		{"resolver DB error is not an auth failure", &fakeMeResolver{err: dbErr}, nil, dbErr, true},
		{"missing wsid", &fakeMeResolver{res: liveWorkspace()}, func(s *auth.Subject) { s.WorkspaceSessionID = "" }, ErrUnauthorized, false},
		{"act != uid", &fakeMeResolver{res: liveWorkspace()}, func(s *auth.Subject) { s.UserID = "someone" }, ErrUnauthorized, false},
		{"att == tid", &fakeMeResolver{res: liveWorkspace()}, func(s *auth.Subject) { s.TenantID = "t-1" }, ErrUnauthorized, false},
		{"missing sid", &fakeMeResolver{res: liveWorkspace()}, func(s *auth.Subject) { s.SessionID = "" }, ErrUnauthorized, false},
		{"builder row gone from the home tenant", &fakeMeResolver{res: liveWorkspace()}, func(s *auth.Subject) { s.ActorUserID, s.UserID = "ghost", "ghost" }, ErrUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t)
			if tc.resolver != nil {
				svc = svc.WithWorkspaceResolver(tc.resolver)
			}
			_, err := svc.Me(context.Background(), sessionAccessToken(t, svc, tc.mutate))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantDB && errors.Is(err, ErrUnauthorized) {
				t.Error("a DB failure must surface as an error (500), never as ErrUnauthorized")
			}
		})
	}
}

// A home token never touches the resolver and its JSON carries no workspace key: byte-identical.
func TestAuthService_Me_HomeTokenByteIdentical(t *testing.T) {
	svc, password := newTestService(t)
	resolver := &fakeMeResolver{err: auth.ErrWorkspaceSessionNotLive} // would refuse if consulted
	svc = svc.WithWorkspaceResolver(resolver)
	access, _ := loginAndGetRefresh(t, svc, password)
	u, err := svc.Me(context.Background(), access)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver consulted for a home token (%d)", resolver.calls)
	}
	raw, _ := json.Marshal(u)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, has := m["workspace"]; has {
		t.Errorf("home /auth/me response gained a workspace key: %s", raw)
	}
	if m["tenantId"] != "t-1" {
		t.Errorf("tenantId = %v, want t-1", m["tenantId"])
	}
}
