package services

// specs/050 S4 (tasks T075) — CredentialService.Rotate: the only door that changes a secret, an
// extra header or the host of a stored key. A rotation is on the very next send (no cache), keeps
// the old HMAC key signing V2-Previous for the grace window, empties a slot, and never lets a new
// host receive what was sealed for the old one.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/workflow"
)

func mustCreate(t *testing.T, svc *CredentialService, in CredentialInput) string {
	t.Helper()
	v, _, err := svc.Create(context.Background(), credTenantA, in)
	if err != nil {
		t.Fatalf("create %s: %v", in.Name, err)
	}
	return v.ID
}

func mustResolve(t *testing.T, svc *CredentialService, id string) workflow.WebhookAuth {
	t.Helper()
	a, err := svc.ResolveAuth(context.Background(), credTenantA, id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return a
}

// TestCredentialService_RotateTakesEffectNextSend: a header token rotated now is what the very next
// ResolveAuth returns; the extra header it was not asked to touch stays; no previous key exists for
// a non-hmac type; the audit row says "rotate" and carries neither the old nor the new token.
func TestCredentialService_RotateTakesEffectNextSend(t *testing.T) {
	svc, _, _, spy := newCredSvc(t)
	ctx := context.Background()
	id := mustCreate(t, svc, CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn/api", AuthType: "header",
		Secret: strPtr("tok-OLD-0123456789"), ExtraHeaders: map[string]string{"X-Doi-Tac": "p-9"}})
	if a := mustResolve(t, svc, id); a.Headers["Authorization"] != "Bearer tok-OLD-0123456789" {
		t.Fatalf("before rotation: %+v", a.Headers)
	}

	v, generated, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{Secret: strPtr("tok-NEW-9876543210"), ActorID: "u-1"})
	if err != nil || generated != "" {
		t.Fatalf("rotate = %v generated=%q; a typed-in secret is never echoed", err, generated)
	}
	a := mustResolve(t, svc, id)
	if a.Headers["Authorization"] != "Bearer tok-NEW-9876543210" || a.Headers["X-Doi-Tac"] != "p-9" || a.PrevSigningSecret != "" {
		t.Fatalf("after rotation: %+v", a)
	}
	if v.RotatedAt == nil || v.Status != credential.StatusReady || v.PreviousExpiresAt != nil {
		t.Fatalf("view after rotation = %+v; want rotatedAt set, ready, no previous", v)
	}

	var rot *audit.Event
	for i := range spy.events {
		if spy.events[i].Action == audit.ActionRotate {
			rot = &spy.events[i]
		}
	}
	if rot == nil || rot.EntityName != audit.NamedCredentialMeta || rot.RecordID != id {
		t.Fatalf("audit events = %+v; want one %q row on the key", spy.events, audit.ActionRotate)
	}
	raw, _ := json.Marshal(rot)
	for _, secret := range []string{"tok-OLD-0123456789", "tok-NEW-9876543210", "p-9"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("audit row carries %q: %s", secret, raw)
		}
	}
}

// TestCredentialService_RotateGraceKeepsPrevious: an hmac key rotated with a grace keeps its
// predecessor — sealed under the SAME master key, bound to the prev_secret slot — as the
// previous signing key until the window ends; after it the resolver drops it (the clock is the
// only thing that ends a grace) and the status returns to ready. graceHours 0 keeps none.
func TestCredentialService_RotateGraceKeepsPrevious(t *testing.T) {
	svc, store, keys, _ := newCredSvc(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return t0 }
	id := mustCreate(t, svc, CredentialInput{Name: "signed", BaseURL: "https://hook.example.vn", AuthType: "hmac", Secret: strPtr(strongKey)})

	v, generated, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{GenerateSecret: true, GraceHours: intPtr(2), ActorID: "u-1"})
	if err != nil || !strings.HasPrefix(generated, "nxw_") {
		t.Fatalf("rotate = %v generated=%q; want a Nexus-minted key returned once", err, generated)
	}
	if v.Status != credential.StatusGrace || v.PreviousExpiresAt == nil || !v.PreviousExpiresAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("view = %+v; want grace until t0+2h", v)
	}
	a := mustResolve(t, svc, id)
	if a.SigningSecret != generated || a.PrevSigningSecret != strongKey {
		t.Fatalf("during grace: signing=new %v previous=old %v", a.SigningSecret == generated, a.PrevSigningSecret == strongKey)
	}
	row := store.rows[id]
	if row.KeyID == nil || *row.KeyID != keys.ActiveID() {
		t.Fatalf("key_id = %v; every envelope of the row is sealed under the active master key", row.KeyID)
	}
	if _, err := keys.OpenAAD(*row.PrevSecretEnc, credential.AAD(credTenantA, id, credential.SlotSecret)); err == nil {
		t.Fatal("the previous-key envelope opens under the secret slot's AAD — slots are not bound")
	}

	svc.now = func() time.Time { return t0.Add(2*time.Hour + time.Second) }
	a = mustResolve(t, svc, id)
	if a.SigningSecret != generated || a.PrevSigningSecret != "" {
		t.Fatalf("after the grace: %+v; the previous key must stop signing", a)
	}
	if got, _ := svc.Describe(ctx, credTenantA, id); got.Status != credential.StatusReady {
		t.Fatalf("status after the grace = %s; want ready", got.Status)
	}

	// default grace is 24h; 0 means none.
	svc.now = func() time.Time { return t0 }
	if v, _, err = svc.Rotate(ctx, credTenantA, id, CredentialRotation{GenerateSecret: true}); err != nil ||
		v.PreviousExpiresAt == nil || !v.PreviousExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("default grace: %+v err=%v; want t0+24h", v, err)
	}
	if v, _, err = svc.Rotate(ctx, credTenantA, id, CredentialRotation{Secret: strPtr(strongKey + "2"), GraceHours: intPtr(0)}); err != nil ||
		v.PreviousExpiresAt != nil || store.rows[id].PrevSecretEnc != nil {
		t.Fatalf("graceHours 0: %+v err=%v; want no previous key kept", v, err)
	}
}

// TestCredentialService_RotateOriginChangeDropsSealedMaterial (plan D-12): moving a key to another
// host takes a fresh secret, forgets the old extra headers and the old key in its grace — what was
// sealed for the old host must not be sent to the new one.
func TestCredentialService_RotateOriginChangeDropsSealedMaterial(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	id := mustCreate(t, svc, CredentialInput{Name: "signed", BaseURL: "https://old.example.vn", AuthType: "hmac",
		Secret: strPtr(strongKey), ExtraHeaders: map[string]string{"Authorization": "Bearer partner-old"}})
	if _, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{GenerateSecret: true}); err != nil { // now in grace
		t.Fatal(err)
	}

	_, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{BaseURL: strPtr("https://new.example.vn")})
	wantCredCode(t, err, "CREDENTIAL_SECRET_REQUIRED", "secret")
	_, _, err = svc.Rotate(ctx, credTenantA, id, CredentialRotation{BaseURL: strPtr("https://new.example.vn"), Secret: strPtr(strongKey + "9"), GraceHours: intPtr(1)})
	wantCredCode(t, err, "CREDENTIAL_GRACE_INVALID", "graceHours")
	if a := mustResolve(t, svc, id); a.BaseURL != "https://old.example.vn" {
		t.Fatalf("a refused rotation moved the host: %s", a.BaseURL)
	}

	v, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{BaseURL: strPtr("https://new.example.vn"), Secret: strPtr(strongKey + "9")})
	if err != nil || v.BaseURL != "https://new.example.vn" || len(v.ExtraHeaderNames) != 0 || v.PreviousExpiresAt != nil || v.Status != credential.StatusReady {
		t.Fatalf("origin-change rotation = %+v err=%v", v, err)
	}
	a := mustResolve(t, svc, id)
	if a.SigningSecret != strongKey+"9" || a.PrevSigningSecret != "" || len(a.Headers) != 0 || a.BaseURL != "https://new.example.vn" {
		t.Fatalf("after moving the host: %+v; old headers and old key must be gone", a)
	}

	// New headers sent with the move are the new host's own and are kept; a same-origin path change
	// through rotate keeps what was sealed.
	v, _, err = svc.Rotate(ctx, credTenantA, id, CredentialRotation{BaseURL: strPtr("https://third.example.vn"), Secret: strPtr(strongKey + "8"),
		ExtraHeaders: map[string]string{"X-Doi-Tac": "third"}})
	if err != nil || len(v.ExtraHeaderNames) != 1 || v.ExtraHeaderNames[0] != "X-Doi-Tac" {
		t.Fatalf("move with new headers = %+v err=%v", v, err)
	}
	if _, _, err = svc.Rotate(ctx, credTenantA, id, CredentialRotation{BaseURL: strPtr("https://third.example.vn/v2"), ExtraHeaders: map[string]string{"X-Doi-Tac": "third"}}); err != nil {
		t.Fatalf("same-origin path change: %v", err)
	}
	if a = mustResolve(t, svc, id); a.SigningSecret != strongKey+"8" || a.Headers["X-Doi-Tac"] != "third" {
		t.Fatalf("same-origin rotation lost sealed material: %+v", a)
	}
}

// TestCredentialService_RotateFillsSlot: a slot created without a secret becomes ready when the
// customer admin enters one — no previous key, since there was no old one.
func TestCredentialService_RotateFillsSlot(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	id := mustCreate(t, svc, CredentialInput{Name: "slot", BaseURL: "https://slot.example.vn", AuthType: "hmac"})
	if _, err := svc.ResolveAuth(ctx, credTenantA, id); err == nil {
		t.Fatal("a slot resolved before it was filled")
	}
	v, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{Secret: strPtr(strongKey), ActorID: "u-1"})
	if err != nil || v.Status != credential.StatusReady || v.PreviousExpiresAt != nil || store.rows[id].PrevSecretEnc != nil {
		t.Fatalf("fill = %+v err=%v; want ready and no previous key", v, err)
	}
	if a := mustResolve(t, svc, id); a.SigningSecret != strongKey {
		t.Fatalf("filled slot signs with %q", a.SigningSecret)
	}
}

// TestCredentialService_RotateExtraHeaders: extraHeaders present replaces the whole set ({} clears
// it), absent keeps it; a rotation that changes headers only leaves the secret alone.
func TestCredentialService_RotateExtraHeaders(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	id := mustCreate(t, svc, CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn", AuthType: "header",
		Secret: strPtr("tok-1"), ExtraHeaders: map[string]string{"X-A": "1", "X-B": "2"}})

	if _, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{ExtraHeaders: map[string]string{"X-C": "3"}}); err != nil {
		t.Fatal(err)
	}
	a := mustResolve(t, svc, id)
	if a.Headers["Authorization"] != "Bearer tok-1" || a.Headers["X-C"] != "3" || len(a.Headers) != 2 {
		t.Fatalf("replace set: %+v; want the secret untouched and ONLY X-C as extra", a.Headers)
	}
	if _, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{Secret: strPtr("tok-2")}); err != nil {
		t.Fatal(err)
	}
	if a = mustResolve(t, svc, id); a.Headers["X-C"] != "3" || a.Headers["Authorization"] != "Bearer tok-2" {
		t.Fatalf("absent extraHeaders must keep the set: %+v", a.Headers)
	}
	v, _, err := svc.Rotate(ctx, credTenantA, id, CredentialRotation{ExtraHeaders: map[string]string{}})
	if err != nil || len(v.ExtraHeaderNames) != 0 {
		t.Fatalf("clear = %+v err=%v", v, err)
	}
	if a = mustResolve(t, svc, id); len(a.Headers) != 1 {
		t.Fatalf("after clearing: %+v", a.Headers)
	}
}

// TestCredentialService_RotateReSealsUnderActiveKey: after the operator rotates the MASTER key, a
// credential rotation re-seals every envelope it keeps (kept extra headers, a live previous key)
// under the new active key, so key_id stays truthful for the whole row (plan D-3, D-13).
func TestCredentialService_RotateReSealsUnderActiveKey(t *testing.T) {
	store, spy := newFakeCredentialStore(), &spyAuditor{}
	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	for _, k := range [][]byte{oldKey, newKey} {
		if _, err := rand.Read(k); err != nil {
			t.Fatal(err)
		}
	}
	oldRing, err := crypto.NewKeyring("v1", oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRing, err := crypto.NewKeyring("v2", newKey, map[string][]byte{"v1": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	before := NewCredentialService(store, oldRing, false).WithAuditor(spy)
	id := mustCreate(t, before, CredentialInput{Name: "signed", BaseURL: "https://hook.example.vn", AuthType: "hmac",
		Secret: strPtr(strongKey), ExtraHeaders: map[string]string{"X-Doi-Tac": "p"}})
	if _, _, err := before.Rotate(context.Background(), credTenantA, id, CredentialRotation{GenerateSecret: true}); err != nil {
		t.Fatal(err)
	}
	if *store.rows[id].KeyID != oldRing.ActiveID() {
		t.Fatal("fixture: row not sealed under the old master key")
	}

	after := NewCredentialService(store, newRing, false).WithAuditor(spy)
	if _, _, err := after.Rotate(context.Background(), credTenantA, id, CredentialRotation{ExtraHeaders: map[string]string{"X-Doi-Tac": "q"}}); err != nil {
		t.Fatal(err)
	}
	row := store.rows[id]
	if *row.KeyID != newRing.ActiveID() {
		t.Fatalf("key_id = %s; want %s", *row.KeyID, newRing.ActiveID())
	}
	for slot, env := range map[credential.Slot]*string{credential.SlotSecret: row.SecretEnc, credential.SlotPrevSecret: row.PrevSecretEnc, credential.SlotExtraHeaders: row.ExtraHeadersEnc} {
		if env == nil {
			t.Fatalf("slot %s lost its envelope", slot)
		}
		if kid, _ := crypto.KeyIDOf(*env); kid != newRing.ActiveID() {
			t.Fatalf("slot %s still sealed under %s", slot, kid)
		}
	}
	if a := mustResolve(t, after, id); a.PrevSigningSecret != strongKey || a.Headers["X-Doi-Tac"] != "q" {
		t.Fatalf("kept previous key / new header lost: %+v", a)
	}
}

// TestCredentialService_RotateValidates: every refusal is a coded 422/404 with the field named, and
// a refused rotation changes nothing.
func TestCredentialService_RotateValidates(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	hm := mustCreate(t, svc, CredentialInput{Name: "signed", BaseURL: "https://hook.example.vn", AuthType: "hmac", Secret: strPtr(strongKey)})
	hd := mustCreate(t, svc, CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn", AuthType: "header", Secret: strPtr("tok")})
	bs := mustCreate(t, svc, CredentialInput{Name: "shop", BaseURL: "https://shop.example.vn", AuthType: "basic", Username: strPtr("u"), Secret: strPtr("pw")})
	ut := mustCreate(t, svc, CredentialInput{Name: "tele", BaseURL: "https://api.telegram.org/bot{secret}", AuthType: "url_token", Secret: strPtr("123456:ABC-def_GHIjklMNO")})
	snapshot := func() string { b, _ := json.Marshal(store.rows); return string(b) + *store.rows[hm].SecretEnc }
	before := snapshot()

	cases := []struct {
		name  string
		id    string
		in    CredentialRotation
		code  string
		field string
	}{
		{"nothing to change", hm, CredentialRotation{}, "CREDENTIAL_SECRET_REQUIRED", "secret"},
		{"grace without a new secret is refused, not dropped", hm, CredentialRotation{ExtraHeaders: map[string]string{}, GraceHours: intPtr(5)}, "CREDENTIAL_GRACE_INVALID", "graceHours"},
		{"empty secret is nothing", hm, CredentialRotation{Secret: strPtr("")}, "CREDENTIAL_SECRET_REQUIRED", "secret"},
		{"grace above 168", hm, CredentialRotation{GenerateSecret: true, GraceHours: intPtr(169)}, "CREDENTIAL_GRACE_INVALID", "graceHours"},
		{"grace below 0", hm, CredentialRotation{GenerateSecret: true, GraceHours: intPtr(-1)}, "CREDENTIAL_GRACE_INVALID", "graceHours"},
		{"grace on a header key", hd, CredentialRotation{Secret: strPtr("t2"), GraceHours: intPtr(1)}, "CREDENTIAL_GRACE_INVALID", "graceHours"},
		{"generate on a header key", hd, CredentialRotation{GenerateSecret: true}, "CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret"},
		{"generate with a typed secret", hm, CredentialRotation{GenerateSecret: true, Secret: strPtr(strongKey)}, "CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret"},
		{"weak hmac key", hm, CredentialRotation{Secret: strPtr("short")}, "CREDENTIAL_SECRET_WEAK", "secret"},
		{"control char in a header secret", hd, CredentialRotation{Secret: strPtr("a\r\nX: y")}, "CREDENTIAL_SECRET_INVALID", "secret"},
		{"reserved extra header", hd, CredentialRotation{ExtraHeaders: map[string]string{"Host": "evil"}}, "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"extra header collides with the auth header", hd, CredentialRotation{ExtraHeaders: map[string]string{"authorization": "x"}}, "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"plain http host", hm, CredentialRotation{BaseURL: strPtr("http://evil.example.vn"), Secret: strPtr(strongKey)}, "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		{"username on a header key", hd, CredentialRotation{Secret: strPtr("t2"), Username: strPtr("u")}, "CREDENTIAL_AUTH_TYPE_INVALID", "username"},
		{"basic username with a colon", bs, CredentialRotation{Secret: strPtr("pw2"), Username: strPtr("a:b")}, "CREDENTIAL_HEADER_INVALID", "username"},
		{"url_token base loses its placeholder", ut, CredentialRotation{BaseURL: strPtr("https://api.telegram.org/bot"), Secret: strPtr("654321:new")}, "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		{"url_token base gets two placeholders", ut, CredentialRotation{BaseURL: strPtr("https://api.telegram.org/{secret}/{secret}"), Secret: strPtr("654321:new")}, "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		{"url_token secret is a dot segment", ut, CredentialRotation{Secret: strPtr("..")}, "CREDENTIAL_SECRET_INVALID", "secret"},
		{"unknown key", "no-such-id", CredentialRotation{Secret: strPtr(strongKey)}, "CREDENTIAL_NOT_FOUND", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.Rotate(ctx, credTenantA, tc.id, tc.in)
			wantCredCode(t, err, tc.code, tc.field)
		})
	}
	if snapshot() != before {
		t.Fatal("a refused rotation changed the stored row")
	}

	t.Run("another tenant's key is not found", func(t *testing.T) {
		_, _, err := svc.Rotate(ctx, credTenantB, hm, CredentialRotation{Secret: strPtr(strongKey)})
		wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
	})
	t.Run("vault not configured", func(t *testing.T) {
		_, _, err := NewCredentialService(store, nil, false).Rotate(ctx, credTenantA, hm, CredentialRotation{Secret: strPtr(strongKey)})
		wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	})
	t.Run("a key sealed under a master key this server lacks cannot be rotated into a grace", func(t *testing.T) {
		other := NewCredentialService(store, testKeyring(t), false)
		_, _, err := other.Rotate(ctx, credTenantA, hm, CredentialRotation{GenerateSecret: true})
		wantCredCode(t, err, "CREDENTIAL_KEY_UNAVAILABLE", "")
		// ... but a fresh secret with no grace, no kept header and no previous key needs nothing opened.
		if _, _, err := other.Rotate(ctx, credTenantA, hm, CredentialRotation{Secret: strPtr(strongKey + "z"), GraceHours: intPtr(0), ExtraHeaders: map[string]string{}}); err != nil {
			t.Fatalf("re-entering the secret on the new server: %v", err)
		}
		if a, err := other.ResolveAuth(ctx, credTenantA, hm); err != nil || a.SigningSecret != strongKey+"z" {
			t.Fatalf("re-entered key does not resolve: %+v err=%v", a, err)
		}
	})
}

// TestCredentialService_RotateAuditHasNoUsageCount: usageCount is a derived count, not a stored
// column, and a rotation does not change it — the audit diff of a key used by rules must not say
// "usageCount 0 -> N" — while the returned view still reports it.
func TestCredentialService_RotateAuditHasNoUsageCount(t *testing.T) {
	svc, store, _, spy := newCredSvc(t)
	id := mustCreate(t, svc, CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn", AuthType: "header", Secret: strPtr("tok-OLD-0123456789")})
	store.usages[id] = []credential.Usage{{WorkflowID: "wf-1", WorkflowName: "A"}, {WorkflowID: "wf-2", WorkflowName: "B"}}

	v, _, err := svc.Rotate(context.Background(), credTenantA, id, CredentialRotation{Secret: strPtr("tok-NEW-9876543210")})
	if err != nil || v.UsageCount != 2 {
		t.Fatalf("rotate = %+v, %v; want the view to report usageCount 2", v, err)
	}
	for _, e := range spy.events {
		if e.Action != audit.ActionRotate {
			continue
		}
		raw, _ := json.Marshal(e.Diff)
		if strings.Contains(string(raw), "usageCount") {
			t.Fatalf("rotate audit diff names usageCount: %s", raw)
		}
	}
}
