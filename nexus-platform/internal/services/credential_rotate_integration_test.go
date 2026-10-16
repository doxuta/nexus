//go:build integration

// ../specs/050-named-credentials S4 (tasks T078) — Rotate over REAL MySQL: the SELECT … FOR UPDATE
// transaction serializes two rotations of one key (each "previous key" is the secret the other left,
// never the original read twice), every envelope of the row carries the active master-key id, and
// a refused rotation rolls back. Run with:
//
//	go test -tags=integration ./internal/services/ -run 'TestCredentialService_ConcurrentRotate|TestCredentialRepository_Rotate' -race -count=1
package services

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
)

// lockedAuditor is a goroutine-safe spy (the shared spyAuditor appends without a lock).
type lockedAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *lockedAuditor) Record(_ context.Context, e audit.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func TestCredentialService_ConcurrentRotateSerialized(t *testing.T) {
	gdb, _ := o3aSetup(t)
	spy := &lockedAuditor{}
	svc := NewCredentialService(credential.NewRepository(gdb), testKeyring(t), false).WithAuditor(spy)
	ctx := context.Background()
	tenant := uuid.NewString()

	const rounds, writers = 8, 2
	for round := 0; round < rounds; round++ {
		orig := strongKey + "-orig-" + uuid.NewString()
		v, _, err := svc.Create(ctx, tenant, CredentialInput{Name: "k" + uuid.NewString()[:8], BaseURL: "https://hook.example.vn", AuthType: "hmac", Secret: &orig})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		secrets := make([]string, writers)
		for i := range secrets {
			secrets[i] = strongKey + "-w" + string(rune('A'+i)) + "-" + uuid.NewString()
		}
		start := make(chan struct{})
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, _, errs[i] = svc.Rotate(ctx, tenant, v.ID, CredentialRotation{Secret: &secrets[i], ActorID: "u"})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: rotation %d failed (they must queue, not fail): %v", round, i, err)
			}
		}
		a, err := svc.ResolveAuth(ctx, tenant, v.ID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// Serialized: the last writer's secret is current and the FIRST writer's secret is the previous
		// one. Unserialized, both read `orig` and the previous key would be `orig`.
		var other string
		switch a.SigningSecret {
		case secrets[0]:
			other = secrets[1]
		case secrets[1]:
			other = secrets[0]
		default:
			t.Fatalf("round %d: current key is neither writer's secret", round)
		}
		if a.PrevSigningSecret != other {
			t.Fatalf("round %d: previous key = original %v, other writer's %v — the rotations were not serialized",
				round, a.PrevSigningSecret == orig, a.PrevSigningSecret == other)
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	rotates := 0
	for _, e := range spy.events {
		if e.Action == audit.ActionRotate {
			rotates++
		}
	}
	if rotates != rounds*writers {
		t.Fatalf("audit rotate rows = %d, want %d (one per rotation)", rotates, rounds*writers)
	}
}

// TestCredentialRepository_RotateKeepsSingleKeyID: after the operator moves the master key, a rotation
// that keeps an extra header and a live previous key re-seals ALL envelopes under the new active key
// in the database, and a rotation refused by validation leaves every column as it was.
func TestCredentialRepository_RotateKeepsSingleKeyID(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	repo := credential.NewRepository(gdb)
	ctx := context.Background()
	tenant := uuid.NewString()

	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	copy(oldKey, uuid.NewString())
	copy(newKey, uuid.NewString())
	oldRing, err := crypto.NewKeyring("v1", oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRing, err := crypto.NewKeyring("v2", newKey, map[string][]byte{"v1": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	before := NewCredentialService(repo, oldRing, false)
	v, _, err := before.Create(ctx, tenant, CredentialInput{Name: "signed", BaseURL: "https://hook.example.vn", AuthType: "hmac",
		Secret: strPtr(strongKey), ExtraHeaders: map[string]string{"X-Doi-Tac": "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := before.Rotate(ctx, tenant, v.ID, CredentialRotation{GenerateSecret: true}); err != nil {
		t.Fatal(err)
	}

	type row struct {
		SecretEnc, PrevSecretEnc, ExtraHeadersEnc, KeyID, ExtraHeaderNames *string
		PrevExpiresAt, RotatedAt                                           *string
	}
	read := func() row {
		var r row
		if err := sdb.Get(&r, `SELECT secret_enc AS secretenc, prev_secret_enc AS prevsecretenc, extra_headers_enc AS extraheadersenc,
			key_id AS keyid, CAST(extra_header_names AS CHAR) AS extraheadernames, CAST(prev_expires_at AS CHAR) AS prevexpiresat,
			CAST(rotated_at AS CHAR) AS rotatedat FROM sys_named_credentials WHERE tenant_id = ? AND id = ?`, tenant, v.ID); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return r
	}
	r := read()
	if r.KeyID == nil || *r.KeyID != oldRing.ActiveID() || r.PrevSecretEnc == nil || r.PrevExpiresAt == nil || r.RotatedAt == nil {
		t.Fatalf("fixture row = %+v", r)
	}

	after := NewCredentialService(repo, newRing, false)
	if _, _, err := after.Rotate(ctx, tenant, v.ID, CredentialRotation{}); err == nil {
		t.Fatal("a rotation with no new material must be refused")
	}
	if got := read(); !reflect.DeepEqual(got, r) {
		t.Fatalf("a refused rotation changed the row:\n before %+v\n after  %+v", r, got)
	}

	if _, _, err := after.Rotate(ctx, tenant, v.ID, CredentialRotation{ExtraHeaders: map[string]string{"X-Doi-Tac": "q"}}); err != nil {
		t.Fatalf("rotate under the new master key: %v", err)
	}
	r = read()
	for slot, env := range map[string]*string{"secret": r.SecretEnc, "prev_secret": r.PrevSecretEnc, "extra_headers": r.ExtraHeadersEnc} {
		if env == nil {
			t.Fatalf("slot %s lost its envelope", slot)
		}
		if kid, _ := crypto.KeyIDOf(*env); kid != newRing.ActiveID() {
			t.Fatalf("slot %s sealed under %s, want %s", slot, kid, newRing.ActiveID())
		}
	}
	if r.KeyID == nil || *r.KeyID != newRing.ActiveID() {
		t.Fatalf("key_id = %v, want %s", r.KeyID, newRing.ActiveID())
	}
	if r.ExtraHeaderNames == nil || *r.ExtraHeaderNames != `["X-Doi-Tac"]` {
		t.Fatalf("extra_header_names = %v", r.ExtraHeaderNames)
	}
	if a, err := after.ResolveAuth(ctx, tenant, v.ID); err != nil || a.PrevSigningSecret != strongKey || a.Headers["X-Doi-Tac"] != "q" {
		t.Fatalf("resolve after the master-key move = %+v err=%v", a, err)
	}
}

// TestCredentialRepository_RotatePersistsHostAndUsername: the two non-secret columns a rotation may
// move (base_url, username) are written by the real UPDATE — the unit fake copies its own column list,
// so only MySQL proves rotationColumns still names them.
func TestCredentialRepository_RotatePersistsHostAndUsername(t *testing.T) {
	gdb, _ := o3aSetup(t)
	repo := credential.NewRepository(gdb)
	svc := NewCredentialService(repo, testKeyring(t), false)
	ctx, tenant := context.Background(), uuid.NewString()

	v, _, err := svc.Create(ctx, tenant, CredentialInput{Name: "shop", BaseURL: "https://old.example.vn", AuthType: "basic",
		Username: strPtr("old-user"), Secret: strPtr("pw-old-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Rotate(ctx, tenant, v.ID, CredentialRotation{BaseURL: strPtr("https://new.example.vn"),
		Username: strPtr("new-user"), Secret: strPtr("pw-new-0123456789"), ActorID: "u-9"}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, err := repo.Get(ctx, tenant, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://new.example.vn" || got.Username != "new-user" || got.UpdatedBy != "u-9" {
		t.Fatalf("stored row = base %q user %q updated_by %q; want the moved host, username and actor", got.BaseURL, got.Username, got.UpdatedBy)
	}
}
