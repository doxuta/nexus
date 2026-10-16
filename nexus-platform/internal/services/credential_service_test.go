package services

// Unit tests for CredentialService (../specs/050-named-credentials S1b, tasks T019) over an
// in-memory store and a REAL keyring: the vault's contract is that a secret goes in sealed under
// AAD(tenant, id, slot) and never comes back out through any read, audit row or error.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
)

// fakeCredentialStore mirrors credential.Repository: tenant-pinned, live rows only, unique live
// name, soft delete that shreds every envelope in one step.
type fakeCredentialStore struct {
	rows   map[string]*credential.Credential
	usages map[string][]credential.Usage // credential id → rules pointing at it
}

func newFakeCredentialStore() *fakeCredentialStore {
	return &fakeCredentialStore{rows: map[string]*credential.Credential{}, usages: map[string][]credential.Usage{}}
}

func (f *fakeCredentialStore) live(tenantID, id string) (*credential.Credential, bool) {
	c, ok := f.rows[id]
	if !ok || c.TenantID != tenantID || c.DeletedAt != nil {
		return nil, false
	}
	return c, true
}

func (f *fakeCredentialStore) Create(_ context.Context, c *credential.Credential) error {
	for _, r := range f.rows {
		if r.TenantID == c.TenantID && r.Name == c.Name && r.DeletedAt == nil {
			return credential.ErrNameTaken
		}
	}
	now := time.Now()
	c.CreatedAt, c.UpdatedAt = now, now
	cp := *c
	f.rows[c.ID] = &cp
	return nil
}

func (f *fakeCredentialStore) Get(_ context.Context, tenantID, id string) (*credential.Credential, error) {
	c, ok := f.live(tenantID, id)
	if !ok {
		return nil, credential.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeCredentialStore) List(_ context.Context, tenantID string) ([]credential.Credential, error) {
	out := []credential.Credential{}
	for _, c := range f.rows {
		if c.TenantID == tenantID && c.DeletedAt == nil {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeCredentialStore) UpdateMetadata(_ context.Context, tenantID string, c *credential.Credential) error {
	cur, ok := f.live(tenantID, c.ID)
	if !ok {
		return credential.ErrNotFound
	}
	cur.Label, cur.BaseURL, cur.HeaderName, cur.HeaderPrefix, cur.Username, cur.UpdatedBy =
		c.Label, c.BaseURL, c.HeaderName, c.HeaderPrefix, c.Username, c.UpdatedBy
	cur.UpdatedAt = time.Now()
	return nil
}

// WithTxForUpdate mirrors the repository: fn edits a copy of the live row; only when it returns nil
// do the rotation columns (and nothing else) land on the stored row.
func (f *fakeCredentialStore) WithTxForUpdate(_ context.Context, tenantID, id string, fn func(*credential.Credential) error) error {
	cur, ok := f.live(tenantID, id)
	if !ok {
		return credential.ErrNotFound
	}
	cp := *cur
	if err := fn(&cp); err != nil {
		return err
	}
	cur.SecretEnc, cur.SecretHint, cur.PrevSecretEnc, cur.PrevExpiresAt = cp.SecretEnc, cp.SecretHint, cp.PrevSecretEnc, cp.PrevExpiresAt
	cur.ExtraHeadersEnc, cur.ExtraHeaderNames, cur.KeyID = cp.ExtraHeadersEnc, cp.ExtraHeaderNames, cp.KeyID
	cur.BaseURL, cur.Username, cur.RotatedAt, cur.UpdatedBy = cp.BaseURL, cp.Username, cp.RotatedAt, cp.UpdatedBy
	cur.UpdatedAt = time.Now()
	return nil
}

func (f *fakeCredentialStore) SoftDeleteShred(_ context.Context, tenantID, id, deletedBy string) error {
	c, ok := f.live(tenantID, id)
	if !ok {
		return credential.ErrNotFound
	}
	now := time.Now()
	c.DeletedAt, c.SecretEnc, c.SecretHint, c.PrevSecretEnc, c.PrevExpiresAt, c.ExtraHeadersEnc, c.KeyID, c.UpdatedBy =
		&now, nil, "", nil, nil, nil, nil, deletedBy
	return nil
}

func (f *fakeCredentialStore) Usages(_ context.Context, tenantID, id string) ([]credential.Usage, error) {
	if _, ok := f.live(tenantID, id); !ok {
		return nil, nil
	}
	return f.usages[id], nil
}

func (f *fakeCredentialStore) CountUsagesByTenant(_ context.Context, tenantID string) (map[string]int, error) {
	out := map[string]int{}
	for id, us := range f.usages {
		if c, ok := f.rows[id]; ok && c.TenantID == tenantID {
			seen := map[string]bool{}
			for _, u := range us {
				seen[u.WorkflowID] = true
			}
			out[id] = len(seen)
		}
	}
	return out, nil
}

func testKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	k, err := crypto.NewKeyring("v1", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const (
	credTenantA = "tenant-a"
	credTenantB = "tenant-b"
	strongKey   = "0123456789abcdef0123456789abcdef-strong" // ≥ 32 chars
)

func newCredSvc(t *testing.T) (*CredentialService, *fakeCredentialStore, *crypto.Keyring, *spyAuditor) {
	t.Helper()
	store, keys, spy := newFakeCredentialStore(), testKeyring(t), &spyAuditor{}
	return NewCredentialService(store, keys, false).WithAuditor(spy), store, keys, spy
}

func wantCredCode(t *testing.T, err error, code, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v; want ValidationError %s", err, code)
	}
	if ve.Code != code {
		t.Fatalf("code = %s (%s); want %s", ve.Code, ve.Msg, code)
	}
	if field != "" && ve.Details["field"] != field {
		t.Fatalf("%s details.field = %v; want %q", code, ve.Details["field"], field)
	}
}

func TestCredentialService_CreateValidates(t *testing.T) {
	base := func(mut func(*CredentialInput)) CredentialInput {
		in := CredentialInput{Name: "pay", Label: "Cổng thanh toán", BaseURL: "https://pay.example.vn/api", AuthType: "header", Secret: strPtr("tok-123")}
		if mut != nil {
			mut(&in)
		}
		return in
	}
	for _, tc := range []struct {
		name  string
		in    CredentialInput
		code  string
		field string
	}{
		{"name not a slug", base(func(in *CredentialInput) { in.Name = "Pay VN" }), "CREDENTIAL_NAME_INVALID", "name"},
		{"label too long", base(func(in *CredentialInput) { in.Label = strings.Repeat("n", 151) }), "CREDENTIAL_NAME_INVALID", "label"},
		{"plain http off loopback", base(func(in *CredentialInput) { in.BaseURL = "http://pay.example.vn" }), "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		{"userinfo in base url", base(func(in *CredentialInput) { in.BaseURL = "https://u:p@pay.example.vn" }), "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		{"unknown auth type", base(func(in *CredentialInput) { in.AuthType = "oauth" }), "CREDENTIAL_AUTH_TYPE_INVALID", "authType"},
		{"reserved header name", base(func(in *CredentialInput) { in.HeaderName = strPtr("Host") }), "CREDENTIAL_HEADER_INVALID", "headerName"},
		{"x-nexus header name", base(func(in *CredentialInput) { in.HeaderName = strPtr("X-Nexus-Signature") }), "CREDENTIAL_HEADER_INVALID", "headerName"},
		{"CR/LF in header prefix", base(func(in *CredentialInput) { in.HeaderPrefix = strPtr("Bearer\r\nX-Evil: 1") }), "CREDENTIAL_HEADER_INVALID", "headerPrefix"},
		{"CR/LF in header secret", base(func(in *CredentialInput) { in.Secret = strPtr("tok\r\nX-Evil: 1") }), "CREDENTIAL_SECRET_INVALID", "secret"},
		{"secret over 4KB", base(func(in *CredentialInput) { in.Secret = strPtr(strings.Repeat("s", 4097)) }), "CREDENTIAL_SECRET_INVALID", "secret"},
		{"extra header reserved", base(func(in *CredentialInput) { in.ExtraHeaders = map[string]string{"Content-Type": "x"} }), "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"extra header CR/LF value", base(func(in *CredentialInput) { in.ExtraHeaders = map[string]string{"X-Doi-Tac": "a\nb"} }), "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"extra header collides with auth header", base(func(in *CredentialInput) { in.ExtraHeaders = map[string]string{"authorization": "x"} }), "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"hmac secret shorter than 32", base(func(in *CredentialInput) { in.AuthType, in.Secret = "hmac", strPtr("short") }), "CREDENTIAL_SECRET_WEAK", "secret"},
		{"generateSecret on header", base(func(in *CredentialInput) { in.Secret, in.GenerateSecret = nil, true }), "CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret"},
		{"generateSecret plus secret", base(func(in *CredentialInput) { in.AuthType, in.Secret, in.GenerateSecret = "hmac", strPtr(strongKey), true }), "CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret"},
		{"basic secret without username", base(func(in *CredentialInput) { in.AuthType = "basic" }), "CREDENTIAL_SECRET_REQUIRED", "username"},
		{"basic username with colon", base(func(in *CredentialInput) { in.AuthType, in.Username = "basic", strPtr("a:b") }), "CREDENTIAL_HEADER_INVALID", "username"},
		{"basic extra Authorization collides", base(func(in *CredentialInput) {
			in.AuthType, in.Username, in.ExtraHeaders = "basic", strPtr("shop"), map[string]string{"Authorization": "x"}
		}), "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		{"headerName on hmac", base(func(in *CredentialInput) {
			in.AuthType, in.Secret, in.HeaderName = "hmac", strPtr(strongKey), strPtr("X-Key")
		}), "CREDENTIAL_AUTH_TYPE_INVALID", "headerName"},
		{"username on header", base(func(in *CredentialInput) { in.Username = strPtr("shop") }), "CREDENTIAL_AUTH_TYPE_INVALID", "username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _, spy := newCredSvc(t)
			_, _, err := svc.Create(context.Background(), credTenantA, tc.in)
			wantCredCode(t, err, tc.code, tc.field)
			if len(store.rows) != 0 || len(spy.events) != 0 {
				t.Fatalf("rejected create persisted %d rows / %d audit rows; want 0", len(store.rows), len(spy.events))
			}
		})
	}

	// Happy paths: every accepted type seals what it was given and derives the right status.
	svc, store, keys, _ := newCredSvc(t)
	ctx := context.Background()
	v, secret, err := svc.Create(ctx, credTenantA, CredentialInput{
		Name: "pay", BaseURL: "https://pay.example.vn/api", AuthType: "header", Secret: strPtr("tok-1234567890abcdef"),
		ExtraHeaders: map[string]string{"X-Doi-Tac": "partner-9"}, ActorID: "u-1",
	})
	if err != nil || secret != "" {
		t.Fatalf("header create: err=%v generated=%q", err, secret)
	}
	if v.Label != "pay" || v.HeaderName != "Authorization" || v.HeaderPrefix != "Bearer " || v.Status != credential.StatusReady {
		t.Fatalf("header defaults/status wrong: %+v", v)
	}
	if v.SecretHint != "cdef" || len(v.ExtraHeaderNames) != 1 || v.ExtraHeaderNames[0] != "X-Doi-Tac" {
		t.Fatalf("hint/extra names wrong: %+v", v)
	}
	row := store.rows[v.ID]
	if row.KeyID == nil || *row.KeyID != keys.ActiveID() || row.CreatedBy != "u-1" {
		t.Fatalf("row key id / created_by wrong: %+v", row)
	}
	pt, err := keys.OpenAAD(*row.SecretEnc, credential.AAD(credTenantA, v.ID, credential.SlotSecret))
	if err != nil || string(pt) != "tok-1234567890abcdef" {
		t.Fatalf("secret not sealed under AAD(tenant,id,secret): %v", err)
	}
	pt, err = keys.OpenAAD(*row.ExtraHeadersEnc, credential.AAD(credTenantA, v.ID, credential.SlotExtraHeaders))
	if err != nil || string(pt) != `{"X-Doi-Tac":"partner-9"}` {
		t.Fatalf("extra headers not sealed under their slot: %v %s", err, pt)
	}

	v, _, err = svc.Create(ctx, credTenantA, CredentialInput{
		Name: "apikey", Label: "Key", BaseURL: "https://api.example.vn", AuthType: "header",
		HeaderName: strPtr("X-Api-Key"), Secret: strPtr("k"),
	})
	if err != nil || v.HeaderPrefix != "" || v.SecretHint != "" {
		t.Fatalf("custom header: prefix must default empty, short secret gets no hint: %+v err=%v", v, err)
	}

	v, _, err = svc.Create(ctx, credTenantA, CredentialInput{
		Name: "shop", Label: "Shop", BaseURL: "https://shop.example.vn", AuthType: "basic",
		Username: strPtr("shop-user"), Secret: strPtr("p@ss"),
	})
	if err != nil || v.Username != "shop-user" || v.Status != credential.StatusReady {
		t.Fatalf("basic: %+v err=%v", v, err)
	}

	v, secret, err = svc.Create(ctx, credTenantA, CredentialInput{
		Name: "signed", Label: "Signed", BaseURL: "https://hook.example.vn", AuthType: "hmac", GenerateSecret: true,
	})
	if err != nil || !strings.HasPrefix(secret, "nxw_") || len(secret) != 47 {
		t.Fatalf("generated hmac secret = %q (len %d) err=%v; want nxw_ + 43 base64url chars", secret, len(secret), err)
	}
	if v.SecretHint != secret[len(secret)-4:] {
		t.Fatalf("generated secret hint = %q; want last 4 of the secret", v.SecretHint)
	}
	pt, err = keys.OpenAAD(*store.rows[v.ID].SecretEnc, credential.AAD(credTenantA, v.ID, credential.SlotSecret))
	if err != nil || string(pt) != secret {
		t.Fatalf("generated secret not the sealed one: %v", err)
	}

	_, _, err = svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", Label: "x", BaseURL: "https://pay.example.vn", AuthType: "header"})
	wantCredCode(t, err, "CREDENTIAL_NAME_TAKEN", "name")
}

func TestCredentialService_CreateWithoutSecretIsSlot(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	v, secret, err := svc.Create(context.Background(), credTenantA, CredentialInput{
		Name: "zalo", Label: "Zalo OA", BaseURL: "https://openapi.zalo.me", AuthType: "hmac",
	})
	if err != nil || secret != "" {
		t.Fatalf("slot create: err=%v secret=%q", err, secret)
	}
	if v.Status != credential.StatusNeedsSecret || v.SecretHint != "" {
		t.Fatalf("slot status = %s hint=%q; want needs_secret, no hint", v.Status, v.SecretHint)
	}
	row := store.rows[v.ID]
	if row.SecretEnc != nil || row.ExtraHeadersEnc != nil || row.KeyID != nil {
		t.Fatalf("slot carries sealed material or a key id: %+v", row)
	}
}

func TestCredentialService_CrossTenantNotFound(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "header", Secret: strPtr("t")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.Get(ctx, credTenantB, v.ID)
	wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
	_, err = svc.Update(ctx, credTenantB, v.ID, CredentialUpdate{Label: strPtr("hijack")})
	wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
	err = svc.Delete(ctx, credTenantB, v.ID, "u-b")
	wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
	list, err := svc.List(ctx, credTenantB)
	if err != nil || len(list) != 0 {
		t.Fatalf("tenant B list = %d rows (err %v); want 0", len(list), err)
	}
	if got, _, err := svc.Get(ctx, credTenantA, v.ID); err != nil || got.Label != "Pay" {
		t.Fatalf("tenant A lost its credential: %+v %v", got, err)
	}
}

func TestCredentialService_UpdateRejectsOriginChange(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn/api", AuthType: "header", Secret: strPtr("t")})
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"https://evil.example.vn/api", "https://pay.example.vn:8443/api", "http://127.0.0.1/api"} {
		_, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{BaseURL: strPtr(other)})
		wantCredCode(t, err, "CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE", "baseUrl")
	}
	if store.rows[v.ID].BaseURL != "https://pay.example.vn/api" {
		t.Fatalf("rejected origin change was persisted: %s", store.rows[v.ID].BaseURL)
	}
	// Same origin, new path prefix: allowed; unspecified fields are kept (load-then-apply).
	got, err := svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{BaseURL: strPtr("https://PAY.example.vn:443/v2"), ActorID: "u-2"})
	if err != nil || got.BaseURL != "https://PAY.example.vn:443/v2" || got.Label != "Pay" || got.HeaderPrefix != "Bearer " {
		t.Fatalf("same-origin update: %+v err=%v", got, err)
	}
	if store.rows[v.ID].UpdatedBy != "u-2" {
		t.Fatalf("updated_by = %q; want u-2", store.rows[v.ID].UpdatedBy)
	}
}

func TestCredentialService_UpdateRejectsImmutable(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "header", Secret: strPtr("t")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		up    CredentialUpdate
		field string
	}{
		{CredentialUpdate{Name: strPtr("pay2")}, "name"},
		{CredentialUpdate{AuthType: strPtr("basic")}, "authType"},
		{CredentialUpdate{Secret: strPtr("new-secret")}, "secret"},
		{CredentialUpdate{GenerateSecret: true}, "generateSecret"},
		{CredentialUpdate{ExtraHeaders: map[string]string{"X-A": "b"}}, "extraHeaders"},
	} {
		_, err = svc.Update(ctx, credTenantA, v.ID, tc.up)
		wantCredCode(t, err, "CREDENTIAL_FIELD_IMMUTABLE", tc.field)
	}
	// Echoing the unchanged immutable values (a full-object PUT) is not a change.
	if _, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{Name: strPtr("pay"), AuthType: strPtr("header"), Label: strPtr("Pay 2")}); err != nil {
		t.Fatalf("echoed immutable values rejected: %v", err)
	}
}

func TestCredentialService_UpdateValidatesHeaderName(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{
		Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "header", Secret: strPtr("t"),
		ExtraHeaders: map[string]string{"X-Doi-Tac": "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Host", "X-Nexus-Signature", "Content-Length", "bad header", "x-doi-tac"} {
		_, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{HeaderName: strPtr(name)})
		wantCredCode(t, err, "CREDENTIAL_HEADER_INVALID", "headerName")
	}
	_, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{HeaderPrefix: strPtr("Bearer\n")})
	wantCredCode(t, err, "CREDENTIAL_HEADER_INVALID", "headerPrefix")
	_, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{Username: strPtr("shop")})
	wantCredCode(t, err, "CREDENTIAL_AUTH_TYPE_INVALID", "username")
	if store.rows[v.ID].HeaderName != "Authorization" {
		t.Fatalf("rejected header name persisted: %q", store.rows[v.ID].HeaderName)
	}
	got, err := svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{HeaderName: strPtr("X-Api-Key"), HeaderPrefix: strPtr("")})
	if err != nil || got.HeaderName != "X-Api-Key" || got.HeaderPrefix != "" {
		t.Fatalf("valid header rename: %+v err=%v", got, err)
	}
}

func TestCredentialService_DeleteShredsSecrets(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{
		Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "hmac", Secret: strPtr(strongKey),
		ExtraHeaders: map[string]string{"Authorization": "Bearer x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.usages[v.ID] = []credential.Usage{{WorkflowID: "wf-1", WorkflowName: "Báo đơn", ActionIndex: 1}}
	err = svc.Delete(ctx, credTenantA, v.ID, "u-1")
	wantCredCode(t, err, "CREDENTIAL_IN_USE", "")
	var ve *ValidationError
	errors.As(err, &ve)
	if us, _ := ve.Details["usages"].([]credential.Usage); len(us) != 1 || us[0].WorkflowID != "wf-1" {
		t.Fatalf("409 details.usages = %#v; want the rule using the key", ve.Details["usages"])
	}
	if store.rows[v.ID].DeletedAt != nil || store.rows[v.ID].SecretEnc == nil {
		t.Fatal("an in-use credential was deleted")
	}

	delete(store.usages, v.ID)
	if err := svc.Delete(ctx, credTenantA, v.ID, "u-1"); err != nil {
		t.Fatalf("delete unused: %v", err)
	}
	row := store.rows[v.ID]
	if row.DeletedAt == nil || row.SecretEnc != nil || row.ExtraHeadersEnc != nil || row.KeyID != nil || row.SecretHint != "" {
		t.Fatalf("delete did not shred: %+v", row)
	}
	_, _, err = svc.Get(ctx, credTenantA, v.ID)
	wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
}

func TestCredentialService_AuditHasNoSealedValue(t *testing.T) {
	const (
		secret = "partner-signing-key-0123456789-abcdef"
		bearer = "Bearer extra-header-value-must-not-leak"
	)
	svc, _, _, spy := newCredSvc(t)
	ctx := audit.WithActor(context.Background(), "u-1")
	v, generated, err := svc.Create(ctx, credTenantA, CredentialInput{
		Name: "gen", Label: "Gen", BaseURL: "https://gen.example.vn", AuthType: "hmac", GenerateSecret: true,
		ExtraHeaders: map[string]string{"Authorization": bearer},
	})
	if err != nil {
		t.Fatal(err)
	}
	v2, _, err := svc.Create(ctx, credTenantA, CredentialInput{
		Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "hmac", Secret: strPtr(secret),
		ExtraHeaders: map[string]string{"X-Doi-Tac": bearer},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, credTenantA, v2.ID, CredentialUpdate{Label: strPtr("Pay 2")}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, credTenantA, v.ID, "u-1"); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"create": false, "update": false, "delete": false}
	for _, evt := range spy.events {
		if evt.EntityName != "__named_credential__" || evt.ActorUserID != "u-1" || evt.RecordID == "" {
			t.Fatalf("audit row not attributed to the key: %+v", evt)
		}
		want[evt.Action] = true
		raw, _ := json.Marshal(evt.Diff)
		for label, v := range map[string]string{"typed secret": secret, "generated key": generated, "extra header value": bearer, "envelope": "v1:", "key id field": "keyId"} {
			if strings.Contains(string(raw), v) {
				t.Errorf("%s audit diff leaks the %s: %s", evt.Action, label, raw)
			}
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("no %s audit row", action)
		}
	}
}

func TestCredentialService_NilKeyringNotConfigured(t *testing.T) {
	store := newFakeCredentialStore()
	svc := NewCredentialService(store, nil, false)
	ctx := context.Background()
	_, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "header"})
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	_, err = svc.List(ctx, credTenantA)
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	_, _, err = svc.Get(ctx, credTenantA, "x")
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	_, err = svc.Update(ctx, credTenantA, "x", CredentialUpdate{Label: strPtr("y")})
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	err = svc.Delete(ctx, credTenantA, "x", "u")
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
	if len(store.rows) != 0 {
		t.Fatal("inert vault wrote a row")
	}
}

func TestCredentialService_ListCountsUsagesAndDerivesStatus(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	used, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "a-used", Label: "A", BaseURL: "https://a.example.vn", AuthType: "header", Secret: strPtr("t")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "b-slot", Label: "B", BaseURL: "https://b.example.vn", AuthType: "hmac"}); err != nil {
		t.Fatal(err)
	}
	store.usages[used.ID] = []credential.Usage{{WorkflowID: "wf-1", ActionIndex: 0}, {WorkflowID: "wf-1", ActionIndex: 2}, {WorkflowID: "wf-2"}}
	// A row sealed under a master key this server does not hold reads key_unavailable.
	foreign := "v0.deadbeef"
	store.rows[used.ID].KeyID = &foreign

	list, err := svc.List(ctx, credTenantA)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d err=%v", len(list), err)
	}
	if list[0].Name != "a-used" || list[0].UsageCount != 2 || list[0].Status != credential.StatusKeyUnavailable {
		t.Fatalf("used row = %+v; want usageCount 2 (distinct rules), key_unavailable", list[0])
	}
	if list[1].Status != credential.StatusNeedsSecret || list[1].UsageCount != 0 || list[1].ExtraHeaderNames == nil {
		t.Fatalf("slot row = %+v; want needs_secret, 0 usages, [] extra names", list[1])
	}
	_, usages, err := svc.Get(ctx, credTenantA, used.ID)
	if err != nil || len(usages) != 3 {
		t.Fatalf("get usages = %d err=%v", len(usages), err)
	}
}

// TestCredentialService_URLTokenPlaceholderValidation (specs/050 S5, T085): a url_token base holds
// exactly one {secret} and it sits in the PATH; the type is accepted only then. Same body on
// Create, Update and Rotate (validateMetadata), so a base cannot lose its placeholder later.
func TestCredentialService_URLTokenPlaceholderValidation(t *testing.T) {
	mk := func(base string, secret *string) CredentialInput {
		return CredentialInput{Name: "tele", BaseURL: base, AuthType: "url_token", Secret: secret}
	}
	tok := strPtr("123456:ABC-def_GHIjklMNO")
	for _, tc := range []struct {
		name string
		in   CredentialInput
		code string
	}{
		{"no placeholder", mk("https://api.telegram.org/bot", tok), "CREDENTIAL_BASE_URL_INVALID"},
		{"two placeholders", mk("https://api.telegram.org/{secret}/{secret}", tok), "CREDENTIAL_BASE_URL_INVALID"},
		{"placeholder in host", mk("https://{secret}.example.vn/bot", tok), "CREDENTIAL_BASE_URL_INVALID"},
		{"placeholder in query", mk("https://api.example.vn/bot?k={secret}", tok), "CREDENTIAL_BASE_URL_INVALID"},
		{"encoded placeholder besides literal", mk("https://api.example.vn/%7Bsecret%7D/{secret}", tok), "CREDENTIAL_BASE_URL_INVALID"},
		{"dot-segment secret", mk("https://api.example.vn/bot{secret}/", strPtr("..")), "CREDENTIAL_SECRET_INVALID"},
		{"control char in secret", mk("https://api.example.vn/bot{secret}", strPtr("ab\ncd")), "CREDENTIAL_SECRET_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _, _ := newCredSvc(t)
			_, _, err := svc.Create(context.Background(), credTenantA, tc.in)
			wantCredCode(t, err, tc.code, "")
			if len(store.rows) != 0 {
				t.Fatalf("a refused url_token credential was stored: %d rows", len(store.rows))
			}
		})
	}

	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, mk("https://api.telegram.org/bot{secret}", tok))
	if err != nil || v.AuthType != "url_token" || v.BaseURL != "https://api.telegram.org/bot{secret}" || v.Status != "ready" {
		t.Fatalf("one {secret} in the path: view=%+v err=%v; want a ready url_token credential keeping the placeholder", v, err)
	}
	if v.SecretHint != "lMNO" { // 24-char token: last 4 characters only
		t.Fatalf("hint = %q; want the last 4 characters", v.SecretHint)
	}
	// The placeholder cannot be edited away afterwards.
	_, err = svc.Update(ctx, credTenantA, v.ID, CredentialUpdate{BaseURL: strPtr("https://api.telegram.org/bot")})
	wantCredCode(t, err, "CREDENTIAL_BASE_URL_INVALID", "baseUrl")
	// A short token keeps no hint (FR-004).
	short, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "slack", BaseURL: "https://hooks.slack.com/services/T0/B0/{secret}", AuthType: "url_token", Secret: strPtr("short")})
	if err != nil || short.SecretHint != "" {
		t.Fatalf("short url_token: view=%+v err=%v; want no hint", short, err)
	}
}
