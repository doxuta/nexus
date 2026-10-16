//go:build integration

// ../specs/050-named-credentials S7 (tasks T096/T097) over REAL MySQL: rows sealed under a retired
// master key are re-sealed by ResealCredentials through the repository's compare-and-swap, the
// key can then be dropped (ResolveAuth works on the new key alone), and Doctor's key health reads
// the same rows. Run with:
//
//	go test -tags=integration ./internal/services/ -run 'TestCredentialReseal_Integration' -race -count=1
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
)

func TestCredentialReseal_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	repo := credential.NewRepository(gdb)
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	bothRing, err := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	if err != nil {
		t.Fatal(err)
	}
	newOnly, _ := crypto.NewKeyring("v2", newK, nil)

	oldSvc := NewCredentialService(repo, oldRing, false)
	secret := strongKey + "-integration"
	var ids []string
	for _, tenant := range []string{tenantA, tenantB} {
		v, _, err := oldSvc.Create(ctx, tenant, CredentialInput{
			Name: "k" + uuid.NewString()[:8], BaseURL: "https://hook.example.vn", AuthType: "hmac", Secret: &secret,
			ExtraHeaders: map[string]string{"X-Partner": "p1"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, v.ID)
	}

	newSvc := NewCredentialService(repo, bothRing, false)
	retired, bad, err := newSvc.KeyHealth(ctx, tenantA)
	if err != nil || len(retired) != 1 || len(bad) != 0 {
		t.Fatalf("before reseal: retired=%v bad=%v err=%v, want 1/0", retired, bad, err)
	}
	// Dropping the retired key before the reseal: the row is undecryptable (C12), not silently ignored.
	if _, bad, _ := NewCredentialService(repo, newOnly, false).KeyHealth(ctx, tenantA); len(bad) != 1 {
		t.Fatalf("new key alone, before reseal: bad=%v, want 1", bad)
	}

	n, err := ResealCredentials(ctx, repo, bothRing, nil)
	if err != nil || n < 2 {
		t.Fatalf("resealed=%d err=%v, want >=2", n, err)
	}
	if retired, bad, _ := newSvc.KeyHealth(ctx, tenantA); len(retired) != 0 || len(bad) != 0 {
		t.Fatalf("after reseal: retired=%v bad=%v, want 0/0", retired, bad)
	}

	// The retired key is gone: every row still resolves, on the new key alone.
	alone := NewCredentialService(repo, newOnly, false)
	for i, tenant := range []string{tenantA, tenantB} {
		auth, err := alone.ResolveAuth(ctx, tenant, ids[i])
		if err != nil || auth.SigningSecret != secret || auth.Headers["X-Partner"] != "p1" {
			t.Fatalf("resolve on new key alone (tenant %d): %+v err=%v", i, auth, err)
		}
	}
	// Idempotent: nothing left to do.
	if n, err := ResealCredentials(ctx, repo, bothRing, nil); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v, want 0", n, err)
	}
}
