//go:build integration

// specs/062-customer-workspace T015 — WorkspaceSessionRepository against MySQL 8: the per-request
// resolution (GetLive + ActorLive, uncached) and EVERY revocation path, one case each, plus the
// tenant pin of each query.
//
//	go test -tags=integration ./internal/auth/ -run TestWorkspaceSession
package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type wsFixture struct {
	db       *gorm.DB
	repo     *WorkspaceSessionRepository
	homeID   string // the builder's workshop
	custID   string // the customer tenant
	builder  string
	loginSID string
	wsID     string
}

func wsGorm(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormmysql.Open(testDSN), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newWSFixture seeds a live builder (active admin + operator in an active workshop with one live
// refresh row of its login session), an active customer tenant and an open workspace session.
func newWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	db := wsGorm(t)
	f := &wsFixture{
		db: db, repo: NewWorkspaceSessionRepository(db),
		homeID: uuid.NewString(), custID: uuid.NewString(), builder: uuid.NewString(),
		loginSID: uuid.NewString(), wsID: uuid.NewString(),
	}
	t.Cleanup(func() {
		for _, q := range []string{
			"DELETE FROM sys_workspace_sessions WHERE tenant_id IN (?, ?)",
			"DELETE FROM refresh_tokens WHERE tenant_id IN (?, ?)",
			"DELETE FROM users WHERE tenant_id IN (?, ?)",
			"DELETE FROM tenants WHERE id IN (?, ?)",
		} {
			if err := db.Exec(q, f.homeID, f.custID).Error; err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	now := time.Now()
	for _, tn := range []struct{ id, slug, name string }{
		{f.homeID, "ws-home-" + f.homeID[:8], "Xưởng Nexus"}, {f.custID, "ws-cust-" + f.custID[:8], "Khách IT"},
	} {
		if err := db.Create(&Tenant{ID: tn.id, Slug: tn.slug, Name: tn.name, Status: "active", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	if err := db.Create(&User{ID: f.builder, TenantID: f.homeID, Email: "b-" + f.builder[:8] + "@ws.test", PasswordHash: "x",
		FullName: "Người Dựng", Status: "active", IsAdmin: true, IsPlatformOperator: true, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed builder: %v", err)
	}
	if err := db.Create(&RefreshToken{ID: uuid.NewString(), UserID: f.builder, TenantID: f.homeID, SessionID: f.loginSID,
		TokenHash: HashRefreshToken(uuid.NewString()), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now}).Error; err != nil {
		t.Fatalf("seed refresh: %v", err)
	}
	if err := f.repo.Create(context.Background(), &WorkspaceSession{ID: f.wsID, TenantID: f.custID, ActorUserID: f.builder,
		ActorTenantID: f.homeID, ActorSessionID: f.loginSID, ActorLabel: "Người Dựng (Xưởng Nexus)",
		ExpiresAt: now.Add(8 * time.Hour), CreatedBy: f.builder}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return f
}

// resolve is what the auth stage does per request: the session row, then the builder behind it.
func (f *wsFixture) resolve(t *testing.T) error {
	t.Helper()
	ctx, now := context.Background(), time.Now()
	if _, err := f.repo.GetLive(ctx, f.custID, f.wsID, f.builder, f.homeID, f.loginSID, now); err != nil {
		return err
	}
	_, err := f.repo.ActorLive(ctx, f.homeID, f.builder, f.loginSID, now)
	return err
}

func (f *wsFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := f.db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestWorkspaceSession_ResolveLive(t *testing.T) {
	f := newWSFixture(t)
	ctx, now := context.Background(), time.Now()
	live, err := f.repo.GetLive(ctx, f.custID, f.wsID, f.builder, f.homeID, f.loginSID, now)
	if err != nil {
		t.Fatalf("GetLive on a fresh session: %v", err)
	}
	if live.TenantSlug == "" || live.TenantName != "Khách IT" || live.ActorLabel != "Người Dựng (Xưởng Nexus)" {
		t.Errorf("GetLive row incomplete: %+v", live)
	}
	actor, err := f.repo.ActorLive(ctx, f.homeID, f.builder, f.loginSID, now)
	if err != nil {
		t.Fatalf("ActorLive on a live builder: %v", err)
	}
	if actor.FullName != "Người Dựng" || actor.TenantName != "Xưởng Nexus" {
		t.Errorf("ActorLive row incomplete: %+v", actor)
	}
}

// One case per revocation path of the spec: every one must make the NEXT resolution fail.
func TestWorkspaceSession_RevocationPaths(t *testing.T) {
	cases := []struct {
		name string
		kill func(t *testing.T, f *wsFixture)
		want error
	}{
		{"closed (Về Nexus)", func(t *testing.T, f *wsFixture) {
			if ok, err := f.repo.Close(context.Background(), f.custID, f.wsID, f.builder, WorkspaceClosedByBuilder, time.Now()); err != nil || !ok {
				t.Fatalf("Close: ok=%v err=%v", ok, err)
			}
		}, ErrWorkspaceSessionNotLive},
		{"ordinary logout (refresh row revoked, reason NULL)", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE refresh_tokens SET revoked_at = NOW() WHERE tenant_id = ? AND session_id = ?", f.homeID, f.loginSID)
		}, ErrActorNotLive},
		{"home session ended (revoke_reason='ended')", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE refresh_tokens SET revoked_at = NOW(), revoke_reason = ? WHERE tenant_id = ? AND session_id = ?", RevokeReasonEnded, f.homeID, f.loginSID)
		}, ErrActorNotLive},
		{"refresh row expired", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE refresh_tokens SET expires_at = NOW() - INTERVAL 1 MINUTE WHERE tenant_id = ? AND session_id = ?", f.homeID, f.loginSID)
		}, ErrActorNotLive},
		{"operator flag removed", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE users SET is_platform_operator = 0 WHERE tenant_id = ? AND id = ?", f.homeID, f.builder)
		}, ErrActorNotLive},
		{"admin flag removed", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE users SET is_admin = 0 WHERE tenant_id = ? AND id = ?", f.homeID, f.builder)
		}, ErrActorNotLive},
		{"builder disabled", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE users SET status = 'disabled' WHERE tenant_id = ? AND id = ?", f.homeID, f.builder)
		}, ErrActorNotLive},
		{"builder deleted", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE users SET deleted_at = NOW() WHERE tenant_id = ? AND id = ?", f.homeID, f.builder)
		}, ErrActorNotLive},
		{"customer suspended", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", f.custID)
		}, ErrWorkspaceSessionNotLive},
		{"customer deleted (ngưng)", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE tenants SET deleted_at = NOW() WHERE id = ?", f.custID)
		}, ErrWorkspaceSessionNotLive},
		{"workshop not active", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", f.homeID)
		}, ErrActorNotLive},
		{"hard deadline passed", func(t *testing.T, f *wsFixture) {
			f.exec(t, "UPDATE sys_workspace_sessions SET expires_at = NOW() - INTERVAL 1 SECOND WHERE tenant_id = ? AND id = ?", f.custID, f.wsID)
		}, ErrWorkspaceSessionNotLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWSFixture(t)
			if err := f.resolve(t); err != nil {
				t.Fatalf("session must be live before the revocation: %v", err)
			}
			tc.kill(t, f)
			if err := f.resolve(t); !errors.Is(err, tc.want) {
				t.Fatalf("after revocation: err = %v, want %v on the very next resolution", err, tc.want)
			}
		})
	}
}

// A session id from the customer tenant never resolves under another tenant, nor for another
// builder / home tenant / login session (every claim is pinned by the row).
func TestWorkspaceSession_TenantAndClaimPins(t *testing.T) {
	f := newWSFixture(t)
	ctx, now := context.Background(), time.Now()
	other := uuid.NewString()
	for name, call := range map[string]func() error{
		"GetLive other tenant": func() error {
			_, e := f.repo.GetLive(ctx, other, f.wsID, f.builder, f.homeID, f.loginSID, now)
			return e
		},
		"GetLive other builder": func() error {
			_, e := f.repo.GetLive(ctx, f.custID, f.wsID, other, f.homeID, f.loginSID, now)
			return e
		},
		"GetLive other att": func() error {
			_, e := f.repo.GetLive(ctx, f.custID, f.wsID, f.builder, other, f.loginSID, now)
			return e
		},
		"GetLive other sid": func() error { _, e := f.repo.GetLive(ctx, f.custID, f.wsID, f.builder, f.homeID, other, now); return e },
		"GetLive other wsid": func() error {
			_, e := f.repo.GetLive(ctx, f.custID, other, f.builder, f.homeID, f.loginSID, now)
			return e
		},
		"FindOpen other tenant": func() error { _, e := f.repo.FindOpen(ctx, other, f.builder, f.homeID, f.loginSID, now); return e },
		"FindOpen other sid":    func() error { _, e := f.repo.FindOpen(ctx, f.custID, f.builder, f.homeID, other, now); return e },
		"FindResumable other tenant": func() error {
			_, e := f.repo.FindResumable(ctx, other, f.wsID, f.builder, f.homeID, f.loginSID, now)
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrWorkspaceSessionNotLive) {
			t.Errorf("%s: err = %v, want ErrWorkspaceSessionNotLive", name, err)
		}
	}
	// ActorLive pins the home tenant of the builder row and of its refresh rows.
	if _, err := f.repo.ActorLive(ctx, other, f.builder, f.loginSID, now); !errors.Is(err, ErrActorNotLive) {
		t.Errorf("ActorLive other att: err = %v, want ErrActorNotLive", err)
	}
}

func TestWorkspaceSession_FindOpenAndResumable(t *testing.T) {
	f := newWSFixture(t)
	ctx, now := context.Background(), time.Now()
	got, err := f.repo.FindOpen(ctx, f.custID, f.builder, f.homeID, f.loginSID, now)
	if err != nil || got.ID != f.wsID {
		t.Fatalf("FindOpen: %+v %v, want the seeded session", got, err)
	}
	if got, err := f.repo.FindResumable(ctx, f.custID, f.wsID, f.builder, f.homeID, f.loginSID, now); err != nil || got.ID != f.wsID {
		t.Fatalf("FindResumable: %+v %v", got, err)
	}
	// Close is a CAS: the first call wins, the second matches nothing and reports false.
	if ok, err := f.repo.Close(ctx, f.custID, f.wsID, f.builder, WorkspaceClosedByBuilder, now); err != nil || !ok {
		t.Fatalf("first Close: ok=%v err=%v", ok, err)
	}
	if ok, err := f.repo.Close(ctx, f.custID, f.wsID, f.builder, WorkspaceClosedByBuilder, now); err != nil || ok {
		t.Fatalf("second Close: ok=%v err=%v, want ok=false (only the winner writes audit rows)", ok, err)
	}
	// A closed session is never returned by either finder.
	if _, err := f.repo.FindOpen(ctx, f.custID, f.builder, f.homeID, f.loginSID, now); !errors.Is(err, ErrWorkspaceSessionNotLive) {
		t.Errorf("FindOpen after close: %v", err)
	}
	if _, err := f.repo.FindResumable(ctx, f.custID, f.wsID, f.builder, f.homeID, f.loginSID, now); !errors.Is(err, ErrWorkspaceSessionNotLive) {
		t.Errorf("FindResumable after close: %v", err)
	}
	// Close of another builder's id (same tenant) matches nothing.
	f2 := newWSFixture(t)
	if ok, err := f2.repo.Close(ctx, f2.custID, f2.wsID, uuid.NewString(), WorkspaceClosedByBuilder, now); err != nil || ok {
		t.Fatalf("Close by another builder: ok=%v err=%v, want false", ok, err)
	}
}
