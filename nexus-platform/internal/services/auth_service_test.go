package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
)

// --- test doubles ------------------------------------------------------------

// fakeUserRepo is an in-memory stand-in for auth.UserRepository. It lets the service
// unit tests run without a database: lookups return canned rows or the same typed
// not-found sentinels the real GORM repo returns.
type fakeUserRepo struct {
	tenants map[string]*auth.Tenant // keyed by slug
	users   map[string]*auth.User   // keyed by tenantID + "\x00" + email
	byID    map[string]*auth.User   // keyed by tenantID + "\x00" + id
}

func (f *fakeUserRepo) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	t, ok := f.tenants[slug]
	if !ok || (t.Status != "" && t.Status != "active") {
		return nil, auth.ErrTenantNotFound
	}
	return t, nil
}

func (f *fakeUserRepo) GetTenantByID(_ context.Context, id string) (*auth.Tenant, error) {
	for _, t := range f.tenants {
		if t.ID == id {
			if t.Status != "" && t.Status != "active" {
				return nil, auth.ErrTenantNotFound
			}
			return t, nil
		}
	}
	return nil, auth.ErrTenantNotFound
}

func (f *fakeUserRepo) GetUserByEmail(_ context.Context, tenantID, email string) (*auth.User, error) {
	u, ok := f.users[tenantID+"\x00"+email]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeUserRepo) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if f.byID != nil {
		if u, ok := f.byID[tenantID+"\x00"+id]; ok {
			return u, nil
		}
	}
	// Fall back to scanning the email-keyed map so tests that only seed `users` still work.
	for _, u := range f.users {
		if u.TenantID == tenantID && u.ID == id {
			return u, nil
		}
	}
	return nil, auth.ErrUserNotFound
}

// findUser scans both maps (some tests seed only one of them).
func (f *fakeUserRepo) findUser(tenantID, id string) *auth.User {
	if f.byID != nil {
		if u, ok := f.byID[tenantID+"\x00"+id]; ok {
			return u
		}
	}
	for _, u := range f.users {
		if u.TenantID == tenantID && u.ID == id {
			return u
		}
	}
	return nil
}

// RecordFailedLogin mirrors the real repo's atomic increment-and-maybe-lock (mig 000043):
// bump failed_login_count; when the new count reaches threshold, stamp locked_until.
// A missing row is a silent no-op, like the real UPDATE affecting 0 rows.
func (f *fakeUserRepo) RecordFailedLogin(_ context.Context, tenantID, id string, threshold int, lockUntil time.Time) error {
	if u := f.findUser(tenantID, id); u != nil {
		u.FailedLoginCount++
		if u.FailedLoginCount >= threshold {
			lu := lockUntil
			u.LockedUntil = &lu
		}
	}
	return nil
}

// ResetLoginFailures mirrors the real repo's clear-on-successful-password-login.
func (f *fakeUserRepo) ResetLoginFailures(_ context.Context, tenantID, id string) error {
	if u := f.findUser(tenantID, id); u != nil {
		u.FailedLoginCount = 0
		u.LockedUntil = nil
	}
	return nil
}

func (f *fakeUserRepo) UpdatePasswordHash(_ context.Context, tenantID, id, newHash string) error {
	// Update in byID map if present.
	if f.byID != nil {
		if u, ok := f.byID[tenantID+"\x00"+id]; ok {
			u.PasswordHash = newHash
			return nil
		}
	}
	// Fall back to email-keyed map.
	for _, u := range f.users {
		if u.TenantID == tenantID && u.ID == id {
			u.PasswordHash = newHash
			return nil
		}
	}
	return auth.ErrUserNotFound
}

// fakeRefreshRepo is an in-memory stand-in for auth.RefreshTokenRepository. It models the
// tenant-scoped store with a revoked flag and an expiry so the rotate/revoke/expire paths
// can be exercised without a database. A mutex makes RevokeByHash atomic so the concurrency
// (single-use) test exercises the same one-winner contract the real conditional UPDATE gives.
type fakeRefreshRepo struct {
	mu   sync.Mutex
	rows map[string]*auth.RefreshToken // keyed by tenantID + "\x00" + tokenHash
	now  func() time.Time
	// rotateInsertErr makes the successor-insert half of Rotate fail (after a would-be claim): like
	// the real transaction, nothing is revoked then.
	rotateInsertErr error
}

func newFakeRefreshRepo() *fakeRefreshRepo {
	return &fakeRefreshRepo{rows: map[string]*auth.RefreshToken{}, now: time.Now}
}

func (f *fakeRefreshRepo) key(tenantID, hash string) string { return tenantID + "\x00" + hash }

func (f *fakeRefreshRepo) Store(_ context.Context, rt *auth.RefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *rt
	f.rows[f.key(rt.TenantID, rt.TokenHash)] = &cp
	return nil
}

func (f *fakeRefreshRepo) FindLiveByHash(_ context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.rows[f.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt != nil || !rt.ExpiresAt.After(f.now()) {
		return nil, auth.ErrRefreshTokenNotFound
	}
	cp := *rt
	return &cp, nil
}

func (f *fakeRefreshRepo) FindByHashIncludingRevoked(_ context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.rows[f.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt == nil || (rt.RevokeReason != nil && *rt.RevokeReason == auth.RevokeReasonEnded) { // ended session: not a theft signal (specs/055 FR-009)
		// Not found or not yet revoked (the row is still live — irrelevant here).
		return nil, auth.ErrRefreshTokenNotFound
	}
	cp := *rt
	return &cp, nil
}

func (f *fakeRefreshRepo) RevokeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.rows[f.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt != nil {
		return false, nil
	}
	t := f.now()
	rt.RevokedAt = &t
	return true, nil
}

// Rotate mirrors the real repository: claim + successor insert are one atomic step under the mutex,
// and a failing insert leaves the old row live.
func (f *fakeRefreshRepo) Rotate(_ context.Context, tenantID, oldHash string, next *auth.RefreshToken) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.rows[f.key(tenantID, oldHash)]
	if !ok || old.RevokedAt != nil {
		return false, nil
	}
	if f.rotateInsertErr != nil {
		return false, f.rotateInsertErr
	}
	t := f.now()
	old.RevokedAt = &t
	cp := *next
	f.rows[f.key(next.TenantID, next.TokenHash)] = &cp
	return true, nil
}

func (f *fakeRefreshRepo) RevokeAllForUser(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.now()
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil {
			rt.RevokedAt = &t
		}
	}
	return nil
}

// newTestService builds an AuthService over the fake repos and a real TokenService, plus a
// seeded tenant "acme" (id t-1) holding one active user alice@acme.test with password "s3cret".
func newTestService(t *testing.T) (*AuthService, string) {
	t.Helper()
	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	repo := &fakeUserRepo{
		tenants: map[string]*auth.Tenant{
			"acme": {ID: "t-1", Slug: "acme", Name: "Acme", Status: "active"},
		},
		users: map[string]*auth.User{
			"t-1\x00alice@acme.test": {
				ID:           "u-1",
				TenantID:     "t-1",
				Email:        "alice@acme.test",
				PasswordHash: string(hash),
				Status:       "active",
				IsAdmin:      true,
			},
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	return NewAuthService(repo, newFakeRefreshRepo(), tokens), password
}

// --- tests -------------------------------------------------------------------

func TestLogin_Success_ReturnsTokens(t *testing.T) {
	svc, password := newTestService(t)

	out, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "acme",
		Email:    "alice@acme.test",
		Password: password,
	})
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if out.AccessToken == "" {
		t.Error("expected non-empty access token")
	}
	if out.RefreshToken == "" {
		t.Error("expected non-empty refresh token")
	}
	if out.AccessToken == out.RefreshToken {
		t.Error("access and refresh tokens must differ")
	}
	if out.User == nil {
		t.Fatal("expected user in login result")
	}
	if out.User.ID != "u-1" || out.User.TenantID != "t-1" {
		t.Errorf("unexpected user identity: %+v", out.User)
	}
	if out.User.PasswordHash != "" {
		t.Error("password hash must never be exposed in the login result")
	}
}

func TestLogin_WrongPassword_Rejected(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "acme",
		Email:    "alice@acme.test",
		Password: "wrong-password",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLogin_UnknownUser_Rejected(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "acme",
		Email:    "nobody@acme.test",
		Password: "whatever",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
}

func TestLogin_UnknownTenant_Rejected(t *testing.T) {
	svc, password := newTestService(t)

	_, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "ghost",
		Email:    "alice@acme.test",
		Password: password,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown tenant, got %v", err)
	}
}

func TestLogin_DisabledUser_Rejected(t *testing.T) {
	svc, password := newTestService(t)
	// flip the seeded user to disabled
	svc.users.(*fakeUserRepo).users["t-1\x00alice@acme.test"].Status = "disabled"

	_, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "acme",
		Email:    "alice@acme.test",
		Password: password,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for disabled user, got %v", err)
	}
}

func TestLogin_MissingFields_Rejected(t *testing.T) {
	svc, password := newTestService(t)

	cases := []LoginInput{
		{Tenant: "", Email: "alice@acme.test", Password: password},
		{Tenant: "acme", Email: "", Password: password},
		{Tenant: "acme", Email: "alice@acme.test", Password: ""},
	}
	for i, in := range cases {
		if _, err := svc.Login(context.Background(), in); err == nil {
			t.Errorf("case %d: expected error for missing field, got nil", i)
		}
	}
}

// TestLogin_SuspendedTenant_Rejected covers GD1 bug #10 (login arm): a tenant whose status is
// not 'active' must not authenticate. The fake ResolveTenantBySlug mirrors the real repo's
// status filter (folds non-active into ErrTenantNotFound), so Login collapses to the generic
// ErrInvalidCredentials without revealing that the tenant exists-but-suspended.
func TestLogin_SuspendedTenant_Rejected(t *testing.T) {
	svc, password := newTestService(t)
	svc.users.(*fakeUserRepo).tenants["acme"].Status = "suspended"

	_, err := svc.Login(context.Background(), LoginInput{
		Tenant:   "acme",
		Email:    "alice@acme.test",
		Password: password,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for suspended tenant, got %v", err)
	}
}

// TestDummyBcryptHash_IsValidCost12 guards the constant-time login fix (GD1 bug #16): the
// package-level dummy hash run on the not-found branches must be a real cost-12 bcrypt digest
// so the timing of an unknown tenant/user matches a genuine wrong-password compare. A malformed
// or wrong-cost constant would silently reopen the enumeration side-channel.
func TestDummyBcryptHash_IsValidCost12(t *testing.T) {
	cost, err := bcrypt.Cost(dummyBcryptHash)
	if err != nil {
		t.Fatalf("dummyBcryptHash is not a valid bcrypt hash: %v", err)
	}
	if cost != 12 {
		t.Fatalf("dummyBcryptHash cost = %d, want 12 (must match prod bcryptCost)", cost)
	}
	// It must behave as a mismatch (never accidentally accept) — that is the path Login relies on.
	if err := bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte("any-password")); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("dummyBcryptHash compare = %v, want ErrMismatchedHashAndPassword", err)
	}
}

// TestLogin_NotFoundBranches_ReturnInvalidCredentials confirms the constant-time guards on the
// unknown-tenant and unknown-user branches still return the generic credential error (the
// dummy bcrypt compare must not alter the result, only equalize timing — GD1 bug #16).
func TestLogin_NotFoundBranches_ReturnInvalidCredentials(t *testing.T) {
	svc, password := newTestService(t)

	cases := []struct {
		name string
		in   LoginInput
	}{
		{"unknown tenant", LoginInput{Tenant: "ghost", Email: "alice@acme.test", Password: password}},
		{"unknown user", LoginInput{Tenant: "acme", Email: "nobody@acme.test", Password: "whatever"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Login(context.Background(), c.in); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("expected ErrInvalidCredentials, got %v", err)
			}
		})
	}
}
