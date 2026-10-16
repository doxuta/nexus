package services

// Unit tests — APIKeyService + CompositeVerifier (APIKEY-1, docs/api-conventions.md §3e).
// Security assertions pinned here:
//   * issue stores ONLY the SHA-256 of the raw secret (raw never persisted), name + in-tenant
//     role are validated, the public prefix matches the secret;
//   * VerifyKey mints Claims{tenant,role,IsAdmin:false} — the IsAdmin privilege floor holds
//     no matter what role the key carries;
//   * revoked / expired / unknown keys all fail with the ONE opaque auth.ErrAPIKeyInvalid;
//   * the composite verifier routes nxk_ → key path and leaves the JWT path byte-untouched
//     (fall-through intact);
//   * last_used_at is a throttled write-behind (second verify inside the window writes nothing).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// fakeAPIKeyRepo is an in-memory apiKeyRepo recording every call.
type fakeAPIKeyRepo struct {
	mu      sync.Mutex
	rows    map[string]*auth.APIKey // by id
	touched []string                // ids passed to TouchLastUsed
	touchCh chan struct{}           // closed-loop signal for async touch assertions
	failDB  bool
	finds   int // FindByHash calls — the per-request sys_api_keys read
}

func newFakeAPIKeyRepo() *fakeAPIKeyRepo {
	return &fakeAPIKeyRepo{rows: map[string]*auth.APIKey{}, touchCh: make(chan struct{}, 8)}
}

func (f *fakeAPIKeyRepo) Create(_ context.Context, k *auth.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *k
	f.rows[k.ID] = &cp
	return nil
}

func (f *fakeAPIKeyRepo) FindByHash(_ context.Context, keyHash string) (*auth.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if f.failDB {
		return nil, errors.New("db down")
	}
	for _, k := range f.rows {
		if k.KeyHash == keyHash {
			cp := *k
			return &cp, nil
		}
	}
	return nil, auth.ErrAPIKeyInvalid
}

func (f *fakeAPIKeyRepo) List(_ context.Context, tenantID string) ([]auth.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]auth.APIKey, 0)
	for _, k := range f.rows {
		if k.TenantID == tenantID {
			out = append(out, *k)
		}
	}
	return out, nil
}

func (f *fakeAPIKeyRepo) GetByID(_ context.Context, tenantID, id string) (*auth.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.rows[id]
	if !ok || k.TenantID != tenantID {
		return nil, auth.ErrAPIKeyNotFound
	}
	cp := *k
	return &cp, nil
}

func (f *fakeAPIKeyRepo) Revoke(_ context.Context, tenantID, id string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.rows[id]
	if !ok || k.TenantID != tenantID || k.RevokedAt != nil {
		return false, nil
	}
	k.RevokedAt = &at
	return true, nil
}

func (f *fakeAPIKeyRepo) TouchLastUsed(_ context.Context, _, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.rows[id]; ok {
		k.LastUsedAt = &at
	}
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakeAPIKeyRepo) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touched)
}

// fakeRoleValidator answers GetRoleByID from a set of (tenant, role) pairs.
type fakeRoleValidator struct{ valid map[string]bool } // key "tenant/role"

func (f fakeRoleValidator) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if f.valid["("+tenantID+")/"+id] {
		return &auth.Role{ID: id, TenantID: tenantID}, nil
	}
	return nil, auth.ErrRoleNotFound
}

func newTestAPIKeyService(repo *fakeAPIKeyRepo) *APIKeyService {
	rv := fakeRoleValidator{valid: map[string]bool{"(t1)/role-1": true, "(t2)/role-2": true}}
	return NewAPIKeyService(repo, rv)
}

func TestAPIKeyService_Issue(t *testing.T) {
	tests := []struct {
		name     string
		tenant   string
		in       IssueAPIKeyInput
		wantErr  bool
		wantCode string
	}{
		{name: "issues with valid name + in-tenant role", tenant: "t1",
			in: IssueAPIKeyInput{Name: "CI importer", RoleID: "role-1", ActorID: "admin-1"}},
		{name: "name required", tenant: "t1",
			in: IssueAPIKeyInput{Name: "   ", RoleID: "role-1"}, wantErr: true, wantCode: "APIKEY_NAME_REQUIRED"},
		{name: "name too long", tenant: "t1",
			in: IssueAPIKeyInput{Name: strings.Repeat("ă", 101), RoleID: "role-1"}, wantErr: true, wantCode: "APIKEY_NAME_TOO_LONG"},
		{name: "role required", tenant: "t1",
			in: IssueAPIKeyInput{Name: "x", RoleID: ""}, wantErr: true, wantCode: "APIKEY_ROLE_INVALID"},
		{name: "cross-tenant role refused (R3 guard)", tenant: "t1",
			in: IssueAPIKeyInput{Name: "x", RoleID: "role-2"}, wantErr: true, wantCode: "APIKEY_ROLE_INVALID"},
		{name: "expiry in the past refused", tenant: "t1",
			in:      IssueAPIKeyInput{Name: "x", RoleID: "role-1", ExpiresAt: ptrTime(time.Now().Add(-time.Hour))},
			wantErr: true, wantCode: "APIKEY_EXPIRY_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeAPIKeyRepo()
			svc := newTestAPIKeyService(repo)
			key, secret, err := svc.IssueAPIKey(context.Background(), tt.tenant, tt.in)
			if tt.wantErr {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v, want ValidationError code %s", err, tt.wantCode)
				}
				if len(repo.rows) != 0 {
					t.Error("no row must be persisted on a validation failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("IssueAPIKey: %v", err)
			}
			// Secret contract: nxk_ + 43 base64url chars, hash-only storage, prefix match.
			if !strings.HasPrefix(secret, "nxk_") || len(secret) != len("nxk_")+43 {
				t.Errorf("secret shape: got %q (len %d)", secret[:8]+"…", len(secret))
			}
			stored := repo.rows[key.ID]
			if stored.KeyHash != auth.HashRefreshToken(secret) {
				t.Error("stored hash must be SHA-256(raw secret)")
			}
			if strings.Contains(stored.KeyHash, secret) || stored.KeyHash == secret {
				t.Error("raw secret must never be persisted")
			}
			if want := strings.TrimPrefix(secret, "nxk_")[:12]; stored.KeyPrefix != want {
				t.Errorf("keyPrefix: got %q, want first 12 chars of the random part %q", stored.KeyPrefix, want)
			}
			if stored.TenantID != "t1" || stored.RoleID != "role-1" || stored.CreatedBy != "admin-1" {
				t.Errorf("row fields wrong: %+v", stored)
			}
		})
	}
}

func TestAPIKeyService_VerifyKey(t *testing.T) {
	repo := newFakeAPIKeyRepo()
	svc := newTestAPIKeyService(repo)
	ctx := context.Background()

	_, secret, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "live", RoleID: "role-1"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	t.Run("valid key mints tenant+role claims with the IsAdmin floor", func(t *testing.T) {
		claims, err := svc.VerifyKey(ctx, secret)
		if err != nil {
			t.Fatalf("VerifyKey: %v", err)
		}
		if claims.TenantID != "t1" || claims.RoleID != "role-1" {
			t.Errorf("claims: got tenant=%q role=%q", claims.TenantID, claims.RoleID)
		}
		if claims.IsAdmin {
			t.Error("IsAdmin must be hard-floored to false for every API key")
		}
		if claims.TokenType != auth.TokenTypeAPIKey {
			t.Errorf("token type: got %q, want api_key", claims.TokenType)
		}
		if claims.UserID == "" {
			t.Error("UserID must carry the key id for attribution")
		}
	})

	t.Run("unknown key → opaque ErrAPIKeyInvalid", func(t *testing.T) {
		if _, err := svc.VerifyKey(ctx, "nxk_"+strings.Repeat("A", 43)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Errorf("got %v, want ErrAPIKeyInvalid", err)
		}
	})

	t.Run("non-nxk credential → opaque ErrAPIKeyInvalid (defense-in-depth)", func(t *testing.T) {
		if _, err := svc.VerifyKey(ctx, "eyJhbGciOi.something.jwt"); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Errorf("got %v, want ErrAPIKeyInvalid", err)
		}
	})

	t.Run("expired key → opaque ErrAPIKeyInvalid", func(t *testing.T) {
		repo2 := newFakeAPIKeyRepo()
		svc2 := newTestAPIKeyService(repo2)
		_, sec, err := svc2.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{
			Name: "short", RoleID: "role-1", ExpiresAt: ptrTime(time.Now().Add(time.Minute)),
		})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		svc2.now = func() time.Time { return time.Now().Add(2 * time.Minute) } // past expiry
		if _, err := svc2.VerifyKey(ctx, sec); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Errorf("got %v, want ErrAPIKeyInvalid", err)
		}
	})

	t.Run("revoked key → opaque ErrAPIKeyInvalid, fail-closed on the NEXT verify", func(t *testing.T) {
		key2, sec2, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "to revoke", RoleID: "role-1"})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := svc.VerifyKey(ctx, sec2); err != nil {
			t.Fatalf("pre-revoke verify should pass: %v", err)
		}
		if err := svc.RevokeAPIKey(ctx, "t1", key2.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := svc.VerifyKey(ctx, sec2); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Errorf("got %v, want ErrAPIKeyInvalid after revoke", err)
		}
		// Idempotent second revoke.
		if err := svc.RevokeAPIKey(ctx, "t1", key2.ID); err != nil {
			t.Errorf("second revoke must be idempotent success, got %v", err)
		}
	})

	t.Run("DB failure fails closed (error, never claims)", func(t *testing.T) {
		repo.failDB = true
		defer func() { repo.failDB = false }()
		claims, err := svc.VerifyKey(ctx, secret)
		if err == nil || claims != nil {
			t.Error("infra failure must fail closed")
		}
	})
}

func TestAPIKeyService_RevokeNotFound(t *testing.T) {
	repo := newFakeAPIKeyRepo()
	svc := newTestAPIKeyService(repo)
	err := svc.RevokeAPIKey(context.Background(), "t1", "nope")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "APIKEY_NOT_FOUND" {
		t.Fatalf("err = %v, want APIKEY_NOT_FOUND", err)
	}

	// Cross-tenant revoke: a key of t1 must be invisible to t2 (same 404, no existence leak).
	key, _, err2 := svc.IssueAPIKey(context.Background(), "t1", IssueAPIKeyInput{Name: "k", RoleID: "role-1"})
	if err2 != nil {
		t.Fatalf("issue: %v", err2)
	}
	err = svc.RevokeAPIKey(context.Background(), "t2", key.ID)
	if !errors.As(err, &ve) || ve.Code != "APIKEY_NOT_FOUND" {
		t.Fatalf("cross-tenant revoke: err = %v, want APIKEY_NOT_FOUND", err)
	}
}

func TestAPIKeyService_TouchThrottled(t *testing.T) {
	repo := newFakeAPIKeyRepo()
	svc := newTestAPIKeyService(repo)
	svc.touchDone = repo.touchCh // reuse the channel as the completion signal
	ctx := context.Background()

	_, secret, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "hot", RoleID: "role-1"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := svc.VerifyKey(ctx, secret); err != nil {
		t.Fatalf("verify 1: %v", err)
	}
	// Wait for the async write-behind of the FIRST verify.
	select {
	case <-repo.touchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first verify never touched last_used_at")
	}
	got := repo.touchCount()
	if got != 1 {
		t.Fatalf("touch count after first verify: got %d, want 1", got)
	}
	// Second verify inside the throttle window: NO new write may be scheduled.
	if _, err := svc.VerifyKey(ctx, secret); err != nil {
		t.Fatalf("verify 2: %v", err)
	}
	select {
	case <-repo.touchCh:
		t.Fatal("second verify inside the window must not write last_used_at")
	case <-time.After(50 * time.Millisecond):
	}
	// Advance the clock past the interval: the write-behind fires again.
	svc.now = func() time.Time { return time.Now().Add(apiKeyTouchInterval + time.Second) }
	if _, err := svc.VerifyKey(ctx, secret); err != nil {
		t.Fatalf("verify 3: %v", err)
	}
	select {
	case <-repo.touchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("verify after the window must write last_used_at again")
	}
}

func TestCompositeVerifier_Routing(t *testing.T) {
	repo := newFakeAPIKeyRepo()
	keySvc := newTestAPIKeyService(repo)
	_, secret, err := keySvc.IssueAPIKey(context.Background(), "t1", IssueAPIKeyInput{Name: "m2m", RoleID: "role-1"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	jwtSvc, err := auth.NewTokenService("composite-test-secret", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	jwtTok, err := jwtSvc.IssueAccessToken(auth.Subject{UserID: "u1", TenantID: "t9", IsAdmin: true})
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}

	v := NewCompositeVerifier(jwtSvc, keySvc)

	t.Run("nxk_ routes to the key path", func(t *testing.T) {
		claims, err := v.VerifyAccessToken(secret)
		if err != nil {
			t.Fatalf("VerifyAccessToken(key): %v", err)
		}
		if claims.TenantID != "t1" || claims.IsAdmin || claims.TokenType != auth.TokenTypeAPIKey {
			t.Errorf("key claims wrong: %+v", claims.Subject)
		}
	})

	t.Run("a JWT still verifies through the untouched fall-through", func(t *testing.T) {
		claims, err := v.VerifyAccessToken(jwtTok)
		if err != nil {
			t.Fatalf("VerifyAccessToken(jwt): %v", err)
		}
		if claims.TenantID != "t9" || !claims.IsAdmin || claims.TokenType != auth.TokenTypeAccess {
			t.Errorf("jwt claims wrong: %+v", claims.Subject)
		}
	})

	t.Run("garbage fails on the JWT path with a token sentinel (same 401 family)", func(t *testing.T) {
		if _, err := v.VerifyAccessToken("garbage"); !errors.Is(err, auth.ErrTokenMalformed) {
			t.Errorf("got %v, want ErrTokenMalformed", err)
		}
	})

	t.Run("bad nxk_ credential fails opaquely on the key path", func(t *testing.T) {
		if _, err := v.VerifyAccessToken("nxk_bogus"); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Errorf("got %v, want ErrAPIKeyInvalid", err)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

func (f *fakeAPIKeyRepo) findCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finds
}

// TestAPIKeyService_VerifyKey_RemembersDeadKeys (queue #155, specs/045 §5): an integration retrying
// a revoked / expired / never-issued key used to cost one sys_api_keys read per request (measured:
// a 64-worker storm tripled other users' latency against a no-DB 401 storm). Each of those
// outcomes is final, so the second presentation of the same string is refused without the read.
// A live key and an infrastructure failure are never remembered.
func TestAPIKeyService_VerifyKey_RemembersDeadKeys(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// dead returns the raw string to present, after putting the service in the right state.
		dead func(t *testing.T, svc *APIKeyService) string
	}{
		{"never issued", func(t *testing.T, _ *APIKeyService) string { return "nxk_" + strings.Repeat("Z", 43) }},
		{"revoked", func(t *testing.T, svc *APIKeyService) string {
			key, sec, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "gone", RoleID: "role-1"})
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			if err := svc.RevokeAPIKey(ctx, "t1", key.ID); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			return sec
		}},
		{"expired", func(t *testing.T, svc *APIKeyService) string {
			_, sec, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "old", RoleID: "role-1", ExpiresAt: ptrTime(time.Now().Add(time.Minute))})
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			svc.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			return sec
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeAPIKeyRepo()
			svc := newTestAPIKeyService(repo)
			raw := tc.dead(t, svc)
			for i := 1; i <= 5; i++ {
				if _, err := svc.VerifyKey(ctx, raw); !errors.Is(err, auth.ErrAPIKeyInvalid) {
					t.Fatalf("presentation %d: err = %v, want ErrAPIKeyInvalid", i, err)
				}
			}
			if got := repo.findCount(); got != 1 {
				t.Fatalf("5 presentations of a dead key read sys_api_keys %d times, want 1", got)
			}
		})
	}

	t.Run("a live key is read every time, so a revoke lands on the next request", func(t *testing.T) {
		repo := newFakeAPIKeyRepo()
		svc := newTestAPIKeyService(repo)
		key, sec, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "live", RoleID: "role-1"})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		for i := 0; i < 3; i++ {
			if _, err := svc.VerifyKey(ctx, sec); err != nil {
				t.Fatalf("verify %d: %v", i+1, err)
			}
		}
		if got := repo.findCount(); got != 3 {
			t.Fatalf("3 verifies of a live key read %d times, want 3", got)
		}
		if err := svc.RevokeAPIKey(ctx, "t1", key.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := svc.VerifyKey(ctx, sec); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Fatalf("after revoke: err = %v, want ErrAPIKeyInvalid", err)
		}
	})

	t.Run("an infrastructure failure is not remembered", func(t *testing.T) {
		repo := newFakeAPIKeyRepo()
		svc := newTestAPIKeyService(repo)
		_, sec, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "live", RoleID: "role-1"})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		repo.failDB = true
		if _, err := svc.VerifyKey(ctx, sec); err == nil {
			t.Fatal("verify during a DB failure succeeded")
		}
		repo.failDB = false
		if _, err := svc.VerifyKey(ctx, sec); err != nil {
			t.Fatalf("verify after the DB recovered: %v — a transient failure locked out a live key", err)
		}
	})

	t.Run("the memory is bounded", func(t *testing.T) {
		svc := newTestAPIKeyService(newFakeAPIKeyRepo())
		for i := 0; i <= apiKeyDeadMax; i++ {
			if _, err := svc.VerifyKey(ctx, fmt.Sprintf("nxk_%043d", i)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
				t.Fatalf("key %d: err = %v", i, err)
			}
		}
		if n := svc.deadKeyCount(); n > apiKeyDeadMax {
			t.Fatalf("remembered %d dead keys, cap is %d", n, apiKeyDeadMax)
		}
	})
}
