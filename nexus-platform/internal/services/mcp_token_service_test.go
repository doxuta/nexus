package services

// Unit tests — MCPTokenService and the composite verifier's MCP refusal (issue #3). Pinned here:
//   * issue stores only the SHA-256 of the raw nxm_ token, opens the builder's thread, and caps the
//     life at 8 h (the default too);
//   * verify answers the one opaque auth.ErrMCPTokenInvalid for unknown, revoked, expired, and for
//     a builder who is gone, disabled, no longer admin or no longer a builder;
//   * an nxm_ token never authenticates on the API, even behind a JWT verifier that accepts anything.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

type fakeMCPRepo struct {
	rows    map[string]*auth.MCPToken
	touched int
	failDB  bool
}

func (f *fakeMCPRepo) Create(_ context.Context, t *auth.MCPToken) error {
	cp := *t
	f.rows[t.ID] = &cp
	return nil
}

func (f *fakeMCPRepo) FindByHash(_ context.Context, hash string) (*auth.MCPToken, error) {
	if f.failDB {
		return nil, errors.New("db down")
	}
	for _, t := range f.rows {
		if t.TokenHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, auth.ErrMCPTokenInvalid
}

func (f *fakeMCPRepo) ListLive(_ context.Context, tenantID, userID string, now time.Time) ([]auth.MCPToken, error) {
	var out []auth.MCPToken
	for _, t := range f.rows {
		if t.TenantID == tenantID && t.UserID == userID && t.RevokedAt == nil && t.ExpiresAt.After(now) {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeMCPRepo) Revoke(_ context.Context, tenantID, userID, id string, at time.Time) error {
	t, ok := f.rows[id]
	if !ok || t.TenantID != tenantID || t.UserID != userID {
		return auth.ErrMCPTokenNotFound
	}
	if t.RevokedAt == nil {
		t.RevokedAt = &at
	}
	return nil
}

func (f *fakeMCPRepo) Touch(_ context.Context, _, id string, at time.Time) error {
	f.rows[id].LastUsedAt = &at
	f.touched++
	return nil
}

type threadCall struct{ tenantID, userID, sessionID, title, user, assistant string }

type fakeThreads struct{ calls []threadCall }

func (f *fakeThreads) AppendTurn(_ context.Context, tenantID, userID, sessionID, title, user, assistant string, _ json.RawMessage) (string, error) {
	f.calls = append(f.calls, threadCall{tenantID, userID, sessionID, title, user, assistant})
	return "sess-1", nil
}

type fakeBuilders map[string]*auth.User // by user id

func (f fakeBuilders) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if u, ok := f[id]; ok && u.TenantID == tenantID {
		cp := *u
		return &cp, nil
	}
	return nil, auth.ErrUserNotFound
}

var mcpNow = time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)

func builder() *auth.User {
	return &auth.User{ID: "u-builder", TenantID: "t1", Status: "active", IsAdmin: true, IsPlatformOperator: true}
}

func newTestMCPService() (*MCPTokenService, *fakeMCPRepo, *fakeThreads, fakeBuilders) {
	repo := &fakeMCPRepo{rows: map[string]*auth.MCPToken{}}
	threads := &fakeThreads{}
	users := fakeBuilders{"u-builder": builder()}
	s := NewMCPTokenService(repo, threads, users)
	s.now = func() time.Time { return mcpNow }
	return s, repo, threads, users
}

var builderSub = auth.Subject{UserID: "u-builder", TenantID: "t1", IsAdmin: true}

func TestMCPToken_IssueStoresOnlyTheHashAndOpensTheThread(t *testing.T) {
	s, repo, threads, _ := newTestMCPService()
	tok, raw, err := s.Issue(context.Background(), builderSub, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(raw, "nxm_") || len(raw) != len("nxm_")+43 {
		t.Fatalf("raw token %q: want nxm_ + 43 base64url chars (32 random bytes)", raw)
	}
	stored := repo.rows[tok.ID]
	if stored.TokenHash != auth.HashRefreshToken(raw) || strings.Contains(stored.TokenHash, raw[4:]) {
		t.Fatal("the row must hold the SHA-256 of the token, never the token")
	}
	if stored.TokenPrefix != raw[4:16] {
		t.Fatalf("prefix %q is not the public start of the token", stored.TokenPrefix)
	}
	if got := stored.ExpiresAt.Sub(mcpNow); got != 8*time.Hour {
		t.Fatalf("default life = %v, want 8h", got)
	}
	if len(threads.calls) != 1 {
		t.Fatalf("thread writes = %d, want 1", len(threads.calls))
	}
	c := threads.calls[0]
	if c.tenantID != "t1" || c.userID != "u-builder" || c.sessionID != "" || stored.SessionID != "sess-1" {
		t.Fatalf("thread = %+v, token session %q: want a NEW thread of the issuing builder, stored on the token", c, stored.SessionID)
	}
	if strings.Contains(c.title+c.user+c.assistant, raw) {
		t.Fatal("the thread must never carry the raw token")
	}
}

func TestMCPToken_IssueExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"one hour", mcpNow.Add(time.Hour), true},
		{"exactly eight hours", mcpNow.Add(8 * time.Hour), true},
		{"past eight hours", mcpNow.Add(8*time.Hour + time.Second), false},
		{"now", mcpNow, false},
		{"past", mcpNow.Add(-time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := newTestMCPService()
			at := tc.at
			tok, _, err := s.Issue(context.Background(), builderSub, &at)
			if tc.ok {
				if err != nil || !tok.ExpiresAt.Equal(at) {
					t.Fatalf("issue: %v, expires %v; want %v", err, tok, at)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "MCP_TOKEN_EXPIRY_INVALID" {
				t.Fatalf("got %v, want MCP_TOKEN_EXPIRY_INVALID", err)
			}
		})
	}
}

func TestMCPToken_IssueRefusedInsideACustomerWorkspace(t *testing.T) {
	s, repo, threads, _ := newTestMCPService()
	sub := builderSub
	sub.WorkspaceSessionID = "ws-1"
	_, _, err := s.Issue(context.Background(), sub, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "MCP_TOKEN_WORKSPACE_UNSUPPORTED" || len(repo.rows)+len(threads.calls) != 0 {
		t.Fatalf("got %v (rows %d, threads %d); want MCP_TOKEN_WORKSPACE_UNSUPPORTED and nothing written", err, len(repo.rows), len(threads.calls))
	}
}

func TestMCPToken_Verify(t *testing.T) {
	cases := []struct {
		name  string
		setup func(repo *fakeMCPRepo, users fakeBuilders, raw string) string // returns the string to present
		ok    bool
	}{
		{"live token of an active builder", func(_ *fakeMCPRepo, _ fakeBuilders, raw string) string { return raw }, true},
		{"revoked", func(repo *fakeMCPRepo, _ fakeBuilders, raw string) string {
			for _, r := range repo.rows {
				r.RevokedAt = &mcpNow
			}
			return raw
		}, false},
		{"expired", func(repo *fakeMCPRepo, _ fakeBuilders, raw string) string {
			for _, r := range repo.rows {
				r.ExpiresAt = mcpNow
			}
			return raw
		}, false},
		{"never issued", func(_ *fakeMCPRepo, _ fakeBuilders, _ string) string { return "nxm_" + strings.Repeat("A", 43) }, false},
		{"an API key", func(_ *fakeMCPRepo, _ fakeBuilders, _ string) string { return "nxk_" + strings.Repeat("A", 43) }, false},
		{"a JWT", func(_ *fakeMCPRepo, _ fakeBuilders, _ string) string { return "eyJhbGciOiJIUzI1NiJ9.e30.x" }, false},
		{"builder deleted", func(_ *fakeMCPRepo, users fakeBuilders, raw string) string { delete(users, "u-builder"); return raw }, false},
		{"builder disabled", func(_ *fakeMCPRepo, users fakeBuilders, raw string) string {
			users["u-builder"].Status = "inactive"
			return raw
		}, false},
		{"builder no longer admin", func(_ *fakeMCPRepo, users fakeBuilders, raw string) string {
			users["u-builder"].IsAdmin = false
			return raw
		}, false},
		{"builder flag removed", func(_ *fakeMCPRepo, users fakeBuilders, raw string) string {
			users["u-builder"].IsPlatformOperator = false
			return raw
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, _, users := newTestMCPService()
			tok, raw, err := s.Issue(context.Background(), builderSub, nil)
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			p, err := s.Verify(context.Background(), tc.setup(repo, users, raw))
			if !tc.ok {
				if !errors.Is(err, auth.ErrMCPTokenInvalid) || p != nil {
					t.Fatalf("got %+v, %v; want auth.ErrMCPTokenInvalid", p, err)
				}
				return
			}
			want := MCPPrincipal{TokenID: tok.ID, TenantID: "t1", UserID: "u-builder", SessionID: "sess-1"}
			if err != nil || *p != want {
				t.Fatalf("got %+v, %v; want %+v", p, err, want)
			}
			if repo.touched != 1 || repo.rows[tok.ID].LastUsedAt == nil {
				t.Fatal("a verified request must record last_used_at")
			}
		})
	}
}

func TestMCPToken_VerifyInfrastructureFailureIsNotATokenVerdict(t *testing.T) {
	s, repo, _, _ := newTestMCPService()
	_, raw, _ := s.Issue(context.Background(), builderSub, nil)
	repo.failDB = true
	if _, err := s.Verify(context.Background(), raw); err == nil || errors.Is(err, auth.ErrMCPTokenInvalid) {
		t.Fatalf("got %v; a DB failure must not read as an invalid token", err)
	}
}

func TestMCPToken_ListAndRevoke(t *testing.T) {
	s, _, _, _ := newTestMCPService()
	ctx := context.Background()
	tok, raw, _ := s.Issue(ctx, builderSub, nil)
	if live, _ := s.List(ctx, "t1", "u-builder"); len(live) != 1 {
		t.Fatalf("live = %d, want 1", len(live))
	}
	if live, _ := s.List(ctx, "t1", "someone-else"); len(live) != 0 {
		t.Fatal("a builder sees only the tokens they issued")
	}
	var ve *ValidationError
	if err := s.Revoke(ctx, "t1", "someone-else", tok.ID); !errors.As(err, &ve) || ve.Code != "MCP_TOKEN_NOT_FOUND" {
		t.Fatalf("revoke by another user: %v, want MCP_TOKEN_NOT_FOUND", err)
	}
	if err := s.Revoke(ctx, "t1", "u-builder", tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.Revoke(ctx, "t1", "u-builder", tok.ID); err != nil {
		t.Fatalf("second revoke must succeed: %v", err)
	}
	if live, _ := s.List(ctx, "t1", "u-builder"); len(live) != 0 {
		t.Fatal("a revoked token is not live")
	}
	if _, err := s.Verify(ctx, raw); !errors.Is(err, auth.ErrMCPTokenInvalid) {
		t.Fatalf("verify after revoke: %v", err)
	}
}

// acceptAll is a JWT verifier that accepts any string: the composite's refusal of nxm_ must not
// depend on the JWT parser failing on it.
type acceptAll struct{}

func (acceptAll) VerifyAccessToken(string) (*auth.Claims, error) {
	return &auth.Claims{Subject: auth.Subject{UserID: "u", TenantID: "t1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}, nil
}

func TestCompositeVerifier_RefusesMCPTokens(t *testing.T) {
	v := NewCompositeVerifier(acceptAll{}, newTestAPIKeyService(newFakeAPIKeyRepo()))
	if c, err := v.VerifyAccessToken("nxm_" + strings.Repeat("A", 43)); err == nil || c != nil {
		t.Fatalf("got %+v, %v; an MCP token must never authenticate on the API", c, err)
	}
	if _, err := v.VerifyAccessToken("eyJ.e30.x"); err != nil {
		t.Fatalf("control: the permissive JWT path still answers: %v", err)
	}
}
