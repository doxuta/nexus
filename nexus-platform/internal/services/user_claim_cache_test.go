package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// countingUserReader is the inner (DB) reader stand-in: it serves rows keyed by (tenant, user)
// and counts loads so a test can tell a cache hit from a miss.
type countingUserReader struct {
	rows  map[[2]string]*auth.User
	loads int
	err   error
}

func (r *countingUserReader) GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error) {
	r.loads++
	if err := ctx.Err(); err != nil { // GORM WithContext surfaces the caller's cancellation the same way
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	if u, ok := r.rows[[2]string{tenantID, id}]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, auth.ErrUserNotFound
}

func newCountingUserReader() *countingUserReader {
	return &countingUserReader{rows: map[[2]string]*auth.User{
		{"t1", "u1"}: {ID: "u1", TenantID: "t1", Status: "active", IsAdmin: true, PasswordHash: "$2a$hash"},
		{"t2", "u1"}: {ID: "u1", TenantID: "t2", Status: "active", IsAdmin: false},
	}}
}

// Second read is served from cache; Invalidate forces the next read back to the DB.
func TestCachedUserLoader_CachesUntilInvalidate(t *testing.T) {
	inner := newCountingUserReader()
	l := NewCachedUserLoader(inner, time.Minute)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := l.GetUserByID(ctx, "t1", "u1"); err != nil {
			t.Fatalf("get #%d: %v", i+1, err)
		}
	}
	if inner.loads != 1 {
		t.Fatalf("inner loads after two reads = %d; want 1 (second read cached)", inner.loads)
	}
	l.Invalidate("t1", "u1")
	if _, err := l.GetUserByID(ctx, "t1", "u1"); err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if inner.loads != 2 {
		t.Fatalf("inner loads after invalidate = %d; want 2 (invalidate must drop the entry)", inner.loads)
	}
}

// The key is (tenant, user): the same user id under another tenant is a different entry, so a
// forged-tenant token can never be served another tenant's cached row.
func TestCachedUserLoader_KeyIsTenantScoped(t *testing.T) {
	inner := newCountingUserReader()
	l := NewCachedUserLoader(inner, time.Minute)
	ctx := context.Background()

	a, err := l.GetUserByID(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("t1: %v", err)
	}
	b, err := l.GetUserByID(ctx, "t2", "u1")
	if err != nil {
		t.Fatalf("t2: %v", err)
	}
	if inner.loads != 2 {
		t.Fatalf("inner loads = %d; want 2 (one per tenant)", inner.loads)
	}
	if !a.IsAdmin || b.IsAdmin {
		t.Fatalf("rows crossed tenants: t1.IsAdmin=%v t2.IsAdmin=%v", a.IsAdmin, b.IsAdmin)
	}
}

// A not-found (deleted user) is returned as the sentinel and is NOT cached — the next read asks
// the DB again, so a restored/re-created row is seen at once.
func TestCachedUserLoader_NotFoundPropagatesAndIsNotCached(t *testing.T) {
	inner := newCountingUserReader()
	l := NewCachedUserLoader(inner, time.Minute)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := l.GetUserByID(ctx, "t1", "ghost"); !errors.Is(err, auth.ErrUserNotFound) {
			t.Fatalf("get #%d err = %v; want ErrUserNotFound", i+1, err)
		}
	}
	if inner.loads != 2 {
		t.Fatalf("inner loads = %d; want 2 (errors are never cached)", inner.loads)
	}
}

// Callers get their own copy: mutating a returned row must not poison the cached entry.
func TestCachedUserLoader_ReturnsCopy(t *testing.T) {
	inner := newCountingUserReader()
	l := NewCachedUserLoader(inner, time.Minute)
	ctx := context.Background()

	first, err := l.GetUserByID(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	first.IsAdmin = false // a careless consumer
	second, err := l.GetUserByID(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !second.IsAdmin {
		t.Fatal("cached row was mutated through a returned pointer; want a copy per read")
	}
}

// Nil-safe like CachedACLLoader: an unwired cache is a no-op invalidator.
func TestCachedUserLoader_NilSafe(t *testing.T) {
	var l *CachedUserLoader
	l.Invalidate("t1", "u1")
	l.InvalidateAll()
}

// The load runs single-flight for every coalesced caller, so it must survive the FIRST caller
// giving up: a request whose client already disconnected still yields the row (cached for the
// siblings) instead of propagating context.Canceled into a fail-closed 500 for all of them.
func TestCachedUserLoader_LoadIsDetachedFromCallerContext(t *testing.T) {
	inner := newCountingUserReader()
	l := NewCachedUserLoader(inner, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.GetUserByID(ctx, "t1", "u1"); err != nil {
		t.Fatalf("load under a cancelled caller ctx: %v; want the row (load detached from the caller)", err)
	}
	if inner.loads != 1 {
		t.Fatalf("inner loads = %d; want 1", inner.loads)
	}
}

// The auth stage needs status / is_admin / role_id only: the password hash never enters the cache.
func TestCachedUserLoader_StripsPasswordHash(t *testing.T) {
	l := NewCachedUserLoader(newCountingUserReader(), time.Minute)
	u, err := l.GetUserByID(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.PasswordHash != "" {
		t.Fatalf("PasswordHash = %q cached; want stripped", u.PasswordHash)
	}
}
