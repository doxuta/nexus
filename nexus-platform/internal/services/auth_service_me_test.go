package services

// auth_service_me_test.go — specs/064-preview-as-role-sample-data T040 (S9-2): GET /auth/me with a
// preview token answers the synthetic profile; every other token keeps its exact bytes.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// meServiceWithPreview wires an AuthService (real token service shared with the preview fixture, so
// one secret signs both kinds) with the preview profile door open (wire) or shut.
func meServiceWithPreview(f *previewFixture, wire bool) *AuthService {
	repo := &fakeUserRepo{
		tenants: map[string]*auth.Tenant{"acme": {ID: pvTenant, Slug: "acme", Name: "Acme", Status: "active"}},
		users: map[string]*auth.User{
			pvTenant + "\x00admin@acme.test": {ID: pvBuilder, TenantID: pvTenant, Email: "admin@acme.test", Status: "active", IsAdmin: true},
		},
	}
	svc := NewAuthService(repo, newFakeRefreshRepo(), f.tokens)
	if wire {
		svc = svc.WithPreview(f.svc)
	}
	return svc
}

func TestMe_PreviewToken_SyntheticProfile(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.start(t)
	svc := meServiceWithPreview(f, true)

	u, err := svc.Me(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Me(preview): %v", err)
	}
	if u.IsAdmin || u.ID != PreviewPrincipal(pvTenant, pvRole) || u.TenantID != pvTenant || u.RoleID != pvRole ||
		u.FullName != "Xem như: Nhân viên kinh doanh" || u.Email != "" {
		t.Errorf("profile = %+v", u)
	}
	if u.IsPlatformOperator == nil || *u.IsPlatformOperator {
		t.Errorf("isPlatformOperator = %v, want an explicit false", u.IsPlatformOperator)
	}
	if u.MFAEnabled != nil {
		t.Error("a preview profile must not claim an MFA state")
	}
	p := u.Preview
	if p == nil || p.SessionID != res.SessionID || p.RoleID != pvRole || p.RoleName != "Nhân viên kinh doanh" ||
		p.ActorName != "Người dựng" || !p.ExpiresAt.Equal(res.ExpiresAt) || p.Draft {
		t.Fatalf("preview block = %+v", p)
	}
}

func TestMe_PreviewToken_Refusals(t *testing.T) {
	isUnauthorized := func(e error) bool { return errors.Is(e, ErrUnauthorized) }
	cases := []struct {
		name    string
		wire    bool
		mutate  func(f *previewFixture, res *PreviewStartResult)
		wantErr func(error) bool
	}{
		{"door not wired", false, func(*previewFixture, *PreviewStartResult) {}, isUnauthorized},
		{"session ended", true, func(f *previewFixture, res *PreviewStartResult) {
			now := f.now
			f.store.rows[pvTenant+"|"+res.SessionID].EndedAt = &now
		}, isUnauthorized},
		{"session expired", true, func(f *previewFixture, res *PreviewStartResult) {
			f.store.rows[pvTenant+"|"+res.SessionID].ExpiresAt = f.now.Add(-time.Second)
		}, isUnauthorized},
		{"builder no longer live", true, func(f *previewFixture, _ *PreviewStartResult) { f.actors.err = auth.ErrActorNotLive }, isUnauthorized},
		{"DB error is a 500, never a pass", true, func(f *previewFixture, _ *PreviewStartResult) { f.store.getErr = errors.New("db down") },
			func(e error) bool { return e != nil && !errors.Is(e, ErrUnauthorized) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreviewFixture(t)
			res := f.start(t)
			tc.mutate(f, res)
			u, err := meServiceWithPreview(f, tc.wire).Me(context.Background(), res.Token)
			if u != nil || !tc.wantErr(err) {
				t.Fatalf("Me = (%+v, %v)", u, err)
			}
		})
	}
}

// Golden: a normal access token's /me body carries no `preview` key at all — same bytes as before S9-2.
func TestMe_AccessToken_NoPreviewKey(t *testing.T) {
	f := newPreviewFixture(t)
	svc := meServiceWithPreview(f, true)
	access, err := f.tokens.IssueAccessToken(auth.Subject{UserID: pvBuilder, TenantID: pvTenant, IsAdmin: true})
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	u, err := svc.Me(context.Background(), access)
	if err != nil {
		t.Fatalf("Me(access): %v", err)
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "preview") || u.Preview != nil {
		t.Errorf("access-token /me leaked a preview block: %s", b)
	}
	if u.ID != pvBuilder || u.Email != "admin@acme.test" {
		t.Errorf("profile = %+v", u)
	}
	// And a refresh token is still refused even with the door open.
	refresh, err := f.tokens.IssueRefreshToken(auth.Subject{UserID: pvBuilder, TenantID: pvTenant})
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	if _, err := svc.Me(context.Background(), refresh); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Me(refresh) = %v, want ErrUnauthorized", err)
	}
}
