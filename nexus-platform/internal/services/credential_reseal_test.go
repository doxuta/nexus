package services

// specs/050 S7 (T096): boot-time reseal of vault rows sealed under a retired master key. Real
// keyring, in-memory store that mirrors the repository's tenant pin and key_id compare-and-swap.

import (
	"context"
	"crypto/rand"
	"errors"
	"sort"
	"testing"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
)

func resealKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeResealStore records every tenant id it was asked about (the tenant-pin assertion).
type fakeResealStore struct {
	rows      map[string]*credential.Credential
	asked     []string // tenant ids of every per-tenant call
	beforeCAS func(id string)
}

func (f *fakeResealStore) TenantIDsSealedUnderOtherKey(_ context.Context, activeID string) ([]string, error) {
	seen := map[string]bool{}
	for _, c := range f.rows {
		if c.DeletedAt == nil && c.KeyID != nil && *c.KeyID != activeID {
			seen[c.TenantID] = true
		}
	}
	out := []string{}
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeResealStore) ListSealedUnderOtherKey(_ context.Context, tenantID, activeID string) ([]credential.Credential, error) {
	f.asked = append(f.asked, tenantID)
	out := []credential.Credential{}
	for _, c := range f.rows {
		if c.TenantID == tenantID && c.DeletedAt == nil && c.KeyID != nil && *c.KeyID != activeID {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeResealStore) Reseal(_ context.Context, tenantID string, from string, c *credential.Credential) (bool, error) {
	f.asked = append(f.asked, tenantID)
	if f.beforeCAS != nil {
		f.beforeCAS(c.ID)
	}
	cur, ok := f.rows[c.ID]
	if !ok || cur.TenantID != tenantID || cur.DeletedAt != nil || cur.KeyID == nil || *cur.KeyID != from {
		return false, nil
	}
	cur.SecretEnc, cur.PrevSecretEnc, cur.ExtraHeadersEnc, cur.KeyID = c.SecretEnc, c.PrevSecretEnc, c.ExtraHeadersEnc, c.KeyID
	return true, nil
}

func sealedRow(t *testing.T, kr *crypto.Keyring, tenant, id, secret string) *credential.Credential {
	t.Helper()
	env, err := kr.SealAAD([]byte(secret), credential.AAD(tenant, id, credential.SlotSecret))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := kr.SealAAD([]byte(`{"X-A":"1"}`), credential.AAD(tenant, id, credential.SlotExtraHeaders))
	if err != nil {
		t.Fatal(err)
	}
	kid := kr.ActiveID()
	return &credential.Credential{ID: id, TenantID: tenant, Name: id, SecretEnc: &env, ExtraHeadersEnc: &extra, KeyID: &kid}
}

func TestCredentialReseal_BootResealsRetiredRows(t *testing.T) {
	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	ring, err := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeResealStore{rows: map[string]*credential.Credential{
		"c1": sealedRow(t, oldRing, "t1", "c1", "secret-one"),
		"c2": sealedRow(t, oldRing, "t2", "c2", "secret-two"),
		"c3": sealedRow(t, ring, "t1", "c3", "already-new"),
	}}
	n, err := ResealCredentials(context.Background(), st, ring, nil)
	if err != nil || n != 2 {
		t.Fatalf("resealed=%d err=%v, want 2 nil", n, err)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if *st.rows[id].KeyID != ring.ActiveID() {
			t.Errorf("%s key_id=%s, want active", id, *st.rows[id].KeyID)
		}
	}
	pt, err := ring.OpenAAD(*st.rows["c1"].SecretEnc, credential.AAD("t1", "c1", credential.SlotSecret))
	if err != nil || string(pt) != "secret-one" {
		t.Fatalf("open after reseal: %q %v", pt, err)
	}
	// Opens with the NEW key alone: the retired key can be dropped.
	alone, _ := crypto.NewKeyring("v2", newK, nil)
	if _, err := alone.OpenAAD(*st.rows["c2"].ExtraHeadersEnc, credential.AAD("t2", "c2", credential.SlotExtraHeaders)); err != nil {
		t.Fatalf("extra headers not under the new key: %v", err)
	}
}

func TestCredentialReseal_CASSkipsConcurrentRotate(t *testing.T) {
	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	ring, _ := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	st := &fakeResealStore{rows: map[string]*credential.Credential{"c1": sealedRow(t, oldRing, "t1", "c1", "old")}}
	fresh := sealedRow(t, ring, "t1", "c1", "rotated-meanwhile")
	st.beforeCAS = func(id string) { st.rows[id] = fresh } // a rotation lands between read and write
	n, err := ResealCredentials(context.Background(), st, ring, nil)
	if err != nil || n != 0 {
		t.Fatalf("resealed=%d err=%v, want 0 nil", n, err)
	}
	pt, err := ring.OpenAAD(*st.rows["c1"].SecretEnc, credential.AAD("t1", "c1", credential.SlotSecret))
	if err != nil || string(pt) != "rotated-meanwhile" {
		t.Fatalf("rotated secret clobbered: %q %v", pt, err)
	}
}

func TestCredentialReseal_PerTenantPinned(t *testing.T) {
	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	ring, _ := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	st := &fakeResealStore{rows: map[string]*credential.Credential{
		"c1": sealedRow(t, oldRing, "t1", "c1", "a"),
		"c2": sealedRow(t, oldRing, "t2", "c2", "b"),
	}}
	if _, err := ResealCredentials(context.Background(), st, ring, nil); err != nil {
		t.Fatal(err)
	}
	if len(st.asked) == 0 {
		t.Fatal("no per-tenant call made")
	}
	for _, tid := range st.asked {
		if tid == "" {
			t.Fatal("a statement ran without tenant_id")
		}
	}
}

func TestCredentialReseal_NoRetiredIsNoop(t *testing.T) {
	k := resealKey(t)
	ring, _ := crypto.NewKeyring("v1", k, nil)
	st := &fakeResealStore{rows: map[string]*credential.Credential{"c1": sealedRow(t, ring, "t1", "c1", "a")}}
	n, err := ResealCredentials(context.Background(), st, ring, nil)
	if err != nil || n != 0 || len(st.asked) != 0 {
		t.Fatalf("n=%d err=%v asked=%v, want a pure no-op", n, err, st.asked)
	}
	// A nil keyring (vault inert) is a no-op as well, never a panic.
	if n, err := ResealCredentials(context.Background(), st, nil, nil); err != nil || n != 0 {
		t.Fatalf("nil keyring: n=%d err=%v", n, err)
	}
}

func TestCredentialReseal_UnopenableRowSkipped(t *testing.T) {
	oldK, newK, lostK := resealKey(t), resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	lostRing, _ := crypto.NewKeyring("v1", lostK, nil) // same label, other key
	ring, _ := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	st := &fakeResealStore{rows: map[string]*credential.Credential{
		"c1": sealedRow(t, oldRing, "t1", "c1", "ok"),
		"c2": sealedRow(t, lostRing, "t1", "c2", "unreachable"),
	}}
	n, err := ResealCredentials(context.Background(), st, ring, nil)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1 nil (the unopenable row is left for Doctor C12)", n, err)
	}
	if *st.rows["c2"].KeyID == ring.ActiveID() {
		t.Fatal("unopenable row must keep its key_id")
	}
}

func TestCredentialReseal_StoreErrorWrapped(t *testing.T) {
	ring, _ := crypto.NewKeyring("v2", resealKey(t), map[string][]byte{"v1": resealKey(t)})
	boom := errors.New("boom")
	_, err := ResealCredentials(context.Background(), errResealStore{boom}, ring, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want wrapped boom", err)
	}
}

type errResealStore struct{ err error }

func (e errResealStore) TenantIDsSealedUnderOtherKey(context.Context, string) ([]string, error) {
	return nil, e.err
}
func (e errResealStore) ListSealedUnderOtherKey(context.Context, string, string) ([]credential.Credential, error) {
	return nil, e.err
}
func (e errResealStore) Reseal(context.Context, string, string, *credential.Credential) (bool, error) {
	return false, e.err
}

// failTenantStore fails the list for one tenant; the others must still be re-sealed.
type failTenantStore struct {
	*fakeResealStore
	bad string
}

func (f failTenantStore) ListSealedUnderOtherKey(ctx context.Context, tenantID, activeID string) ([]credential.Credential, error) {
	if tenantID == f.bad {
		return nil, errors.New("db down")
	}
	return f.fakeResealStore.ListSealedUnderOtherKey(ctx, tenantID, activeID)
}

func TestCredentialReseal_OneTenantErrorDoesNotStrandOthers(t *testing.T) {
	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	ring, _ := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	base := &fakeResealStore{rows: map[string]*credential.Credential{
		"c1": sealedRow(t, oldRing, "t1", "c1", "one"),
		"c2": sealedRow(t, oldRing, "t2", "c2", "two"),
	}}
	n, err := ResealCredentials(context.Background(), failTenantStore{base, "t1"}, ring, nil)
	if err == nil {
		t.Fatal("want the tenant error reported")
	}
	if n != 1 || *base.rows["c2"].KeyID != ring.ActiveID() {
		t.Fatalf("resealed=%d c2 key=%s, want t2 re-sealed despite t1 failing", n, *base.rows["c2"].KeyID)
	}
}
