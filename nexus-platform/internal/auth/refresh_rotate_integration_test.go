//go:build integration

// refresh-rotate-atomic (#312): Rotate claims the old refresh row and inserts its successor in ONE
// transaction. Run against a real MySQL 8 (TestMain provisions a throwaway migrated database):
//
//	bash scripts/run-integration.sh -run 'TestRotate_' ./internal/auth/
package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type rotateFixture struct {
	repo           *RefreshTokenRepository
	db             *gorm.DB
	tenant, user   string
	session, oldID string
	oldHash        string
}

func rotateSetup(t *testing.T) *rotateFixture {
	t.Helper()
	db, err := gorm.Open(gormmysql.Open(testDSN), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping refresh rotate integration test", err)
	}
	f := &rotateFixture{
		repo: NewRefreshTokenRepository(db), db: db,
		tenant: uuid.NewString(), user: uuid.NewString(), session: uuid.NewString(), oldID: uuid.NewString(),
		oldHash: HashRefreshToken("old-" + uuid.NewString()),
	}
	slug := "rot-" + f.tenant[:8]
	if err := db.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`, f.tenant, slug, "Rotate").Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := db.Exec(`INSERT INTO users (id, tenant_id, email, password_hash) VALUES (?, ?, ?, ?)`,
		f.user, f.tenant, "u@"+slug, "x").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	now := time.Now().UTC()
	if err := f.repo.Store(context.Background(), &RefreshToken{
		ID: f.oldID, UserID: f.user, TenantID: f.tenant, SessionID: f.session, TokenHash: f.oldHash,
		LastUsedAt: &now, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	return f
}

func (f *rotateFixture) successor() *RefreshToken {
	now := time.Now().UTC()
	return &RefreshToken{
		ID: uuid.NewString(), UserID: f.user, TenantID: f.tenant, SessionID: f.session,
		TokenHash: HashRefreshToken("new-" + uuid.NewString()), LastUsedAt: &now,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
}

func (f *rotateFixture) liveRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(&RefreshToken{}).
		Where("tenant_id = ? AND session_id = ? AND revoked_at IS NULL", f.tenant, f.session).Count(&n).Error; err != nil {
		t.Fatalf("count live: %v", err)
	}
	return n
}

// Happy path: exactly one live row survives (the successor), the old one is revoked ordinarily.
func TestRotate_SwapsTheLiveRow(t *testing.T) {
	f := rotateSetup(t)
	next := f.successor()
	ok, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, next)
	if err != nil || !ok {
		t.Fatalf("Rotate = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := f.repo.FindLiveByHash(context.Background(), f.tenant, next.TokenHash); err != nil {
		t.Errorf("successor not live: %v", err)
	}
	if _, err := f.repo.FindLiveByHash(context.Background(), f.tenant, f.oldHash); err != ErrRefreshTokenNotFound {
		t.Errorf("old row still live: %v", err)
	}
	if n := f.liveRows(t); n != 1 {
		t.Errorf("live rows of the session = %d, want 1", n)
	}
}

// Losing the claim (already rotated / ended / unknown / another tenant) writes NOTHING.
func TestRotate_LostClaimInsertsNothing(t *testing.T) {
	f := rotateSetup(t)
	if _, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, f.successor()); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	loser := f.successor()
	ok, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, loser)
	if err != nil || ok {
		t.Fatalf("second Rotate of the same hash = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := f.repo.FindLiveByHash(context.Background(), f.tenant, loser.TokenHash); err != ErrRefreshTokenNotFound {
		t.Errorf("the loser's successor was inserted: %v", err)
	}
	// Tenant pin: the same hash under another tenant claims nothing.
	ok, err = f.repo.Rotate(context.Background(), uuid.NewString(), f.oldHash, f.successor())
	if err != nil || ok {
		t.Errorf("cross-tenant Rotate = (%v, %v), want (false, nil)", ok, err)
	}
}

// Store failure after the claim must not cost the user the session: the claim rolls back.
func TestRotate_InsertFailureKeepsTheOldRowLive(t *testing.T) {
	f := rotateSetup(t)
	dup := f.successor()
	dup.ID = f.oldID // primary-key clash ⇒ the INSERT fails after the claim succeeded
	ok, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, dup)
	if err == nil || ok {
		t.Fatalf("Rotate with a failing insert = (%v, %v), want (false, error)", ok, err)
	}
	if _, err := f.repo.FindLiveByHash(context.Background(), f.tenant, f.oldHash); err != nil {
		t.Fatalf("old row was revoked although the successor never landed (session lost): %v", err)
	}
}

// raceInTheGap installs the afterClaim hook: end() runs on another connection exactly between the
// claim and the successor insert, once (Rotate re-runs after a deadlock). The hook waits for it to
// finish only briefly — instant when nothing blocks it (the old two-statement bug), parked on the
// claim's row lock inside the transaction. The returned func joins the goroutine.
func (f *rotateFixture) raceInTheGap(t *testing.T, end func() error) (join func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	f.repo.afterClaim = func() {
		once.Do(func() {
			go func() {
				defer close(done)
				if err := end(); err != nil {
					t.Errorf("session end racing the rotation: %v", err)
				}
			}()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
	return func() { <-done }
}

// The gap itself: an EndSession landing between the claim and the successor insert must still end
// the session — the successor may not survive it. Inside the transaction the end waits for the
// claim's row lock and then sees the committed successor; without one it finds no live row, ends
// nothing, and the successor lives on.
func TestRotate_EndSessionInTheGapStillEndsTheSuccessor(t *testing.T) {
	f := rotateSetup(t)
	join := f.raceInTheGap(t, func() error {
		_, err := f.repo.EndSession(context.Background(), f.tenant, f.user, f.session)
		return err
	})
	if _, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, f.successor()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	join()
	if n := f.liveRows(t); n != 0 {
		t.Errorf("session still has %d live row(s) after EndSession raced the rotation — the end was missed", n)
	}
	ended, err := f.repo.IsSessionEnded(context.Background(), f.tenant, f.user, f.session)
	if err != nil || !ended {
		t.Errorf("IsSessionEnded = (%v, %v), want (true, nil)", ended, err)
	}
}

// Same gap for logout-all / password change (EndAllSessions): the successor must be ended too.
func TestRotate_EndAllSessionsInTheGapStillEndsTheSuccessor(t *testing.T) {
	f := rotateSetup(t)
	join := f.raceInTheGap(t, func() error {
		_, err := f.repo.EndAllSessions(context.Background(), f.tenant, f.user, "")
		return err
	})
	if _, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, f.successor()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	join()
	if n := f.liveRows(t); n != 0 {
		t.Errorf("user still has %d live row(s) after EndAllSessions raced the rotation", n)
	}
}

// Same gap for RevokeAllForUser (W1: role change / disable / delete).
func TestRotate_RevokeAllInTheGapStillRevokesTheSuccessor(t *testing.T) {
	f := rotateSetup(t)
	join := f.raceInTheGap(t, func() error {
		return f.repo.RevokeAllForUser(context.Background(), f.tenant, f.user)
	})
	if _, err := f.repo.Rotate(context.Background(), f.tenant, f.oldHash, f.successor()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	join()
	if n := f.liveRows(t); n != 0 {
		t.Errorf("user still has %d live row(s) after RevokeAllForUser raced the rotation", n)
	}
}
