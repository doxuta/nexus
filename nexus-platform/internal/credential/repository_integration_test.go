//go:build integration

// Integration tests for the sys_named_credentials store (specs/050 S1a) against real MySQL 8:
//
//	go test -tags=integration -race ./internal/credential/
package credential

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"nexus/platform/internal/crypto"
	nexusdb "nexus/platform/internal/db"
)

func testGorm(t *testing.T) *gorm.DB {
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

// newTenant returns a fresh tenant id and deletes everything it owns when the test ends.
func newTenant(t *testing.T, db *gorm.DB) string {
	t.Helper()
	tenant := uuid.NewString()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			"DELETE FROM sys_named_credentials WHERE tenant_id = ?",
			"DELETE FROM sys_workflows WHERE tenant_id = ?",
		} {
			if err := db.WithContext(ctx).Exec(q, tenant).Error; err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	return tenant
}

func newCred(tenant, name string) *Credential {
	return &Credential{
		ID: uuid.NewString(), TenantID: tenant, Name: name, Label: "Cổng thanh toán",
		BaseURL: "https://pay.example.vn/api", AuthType: AuthHeader,
		HeaderName: "Authorization", HeaderPrefix: "Bearer ", CreatedBy: "user-1",
	}
}

func testKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// The longest label config accepts (23 chars): its "<label>.<fingerprint>" id must still fit
	// key_id VARCHAR(32) — strict mode rejects a longer value with error 1406.
	kr, err := crypto.NewKeyring(strings.Repeat("k", 23), k, nil)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

// seal fills every sealed slot of c under kr, bound to (tenant, id, slot).
func seal(t *testing.T, kr *crypto.Keyring, c *Credential, secret, headersJSON string) {
	t.Helper()
	s, err := kr.SealAAD([]byte(secret), AAD(c.TenantID, c.ID, SlotSecret))
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	h, err := kr.SealAAD([]byte(headersJSON), AAD(c.TenantID, c.ID, SlotExtraHeaders))
	if err != nil {
		t.Fatalf("seal headers: %v", err)
	}
	kid := kr.ActiveID()
	c.SecretEnc, c.ExtraHeadersEnc, c.KeyID = &s, &h, &kid
	c.SecretHint = secret[len(secret)-4:]
	c.ExtraHeaderNames = []string{"X-Doi-Tac"}
}

// TestCredentialRepository_SealedAtRest: what lands in the column is the self-describing
// envelope, never the plaintext, and it only opens under its own (tenant, id, slot).
func TestCredentialRepository_SealedAtRest(t *testing.T) {
	db := testGorm(t)
	repo, ctx, kr := NewRepository(db), context.Background(), testKeyring(t)
	tenant := newTenant(t, db)

	const secret = "partner-token-0123456789-abcdef"
	c := newCred(tenant, "pay")
	seal(t, kr, c, secret, `{"X-Doi-Tac":"doi-tac-value"}`)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var raw struct {
		SecretEnc       sql.NullString
		ExtraHeadersEnc sql.NullString
		KeyID           sql.NullString
	}
	if err := db.Raw("SELECT secret_enc, extra_headers_enc, key_id FROM sys_named_credentials WHERE tenant_id = ? AND id = ?",
		tenant, c.ID).Row().Scan(&raw.SecretEnc, &raw.ExtraHeadersEnc, &raw.KeyID); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	kid := kr.ActiveID()
	for col, v := range map[string]sql.NullString{"secret_enc": raw.SecretEnc, "extra_headers_enc": raw.ExtraHeadersEnc} {
		if !strings.HasPrefix(v.String, "v1:"+kid+":") {
			t.Fatalf("%s = %q; want a v1:<keyId>: envelope", col, v.String)
		}
		if strings.Contains(v.String, secret) || strings.Contains(v.String, "doi-tac-value") {
			t.Fatalf("%s carries plaintext", col)
		}
	}
	if raw.KeyID.String != kid || len(kid) != 32 {
		t.Fatalf("key_id = %q; want %q, 32 chars (the key sealing every envelope of the row)", raw.KeyID.String, kid)
	}

	got, err := repo.Get(ctx, tenant, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	pt, err := kr.OpenAAD(*got.SecretEnc, AAD(tenant, c.ID, SlotSecret))
	if err != nil || string(pt) != secret {
		t.Fatalf("OpenAAD(read back) = %q, %v; want the secret", pt, err)
	}
	if _, err := kr.OpenAAD(*got.SecretEnc, AAD(tenant, uuid.NewString(), SlotSecret)); err == nil {
		t.Fatal("the envelope opened under another row's AAD")
	}
	if len(got.ExtraHeaderNames) != 1 || got.ExtraHeaderNames[0] != "X-Doi-Tac" || got.SecretHint != "cdef" {
		t.Fatalf("non-secret columns read back as %+v", got)
	}
}

// TestCredentialRepository_DuplicateNameConflict: the name is unique per tenant among live rows,
// enforced by the DB (uq on the generated name_live) — and GORM never writes the generated column
// (MySQL would answer 3105 on every INSERT).
func TestCredentialRepository_DuplicateNameConflict(t *testing.T) {
	db := testGorm(t)
	repo, ctx := NewRepository(db), context.Background()
	tA, tB := newTenant(t, db), newTenant(t, db)

	if err := repo.Create(ctx, newCred(tA, "pay")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := repo.Create(ctx, newCred(tA, "pay")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second Create same tenant+name = %v; want ErrNameTaken", err)
	}
	if err := repo.Create(ctx, newCred(tB, "pay")); err != nil {
		t.Fatalf("same name in another tenant = %v; want nil", err)
	}
	list, err := repo.List(ctx, tA)
	if err != nil || len(list) != 1 {
		t.Fatalf("List(tA) = %d rows, %v; want exactly the first row", len(list), err)
	}
}

// TestCredentialRepository_DuplicateNameLogKeepsEnvelopeOut: a retyped name (409, an everyday
// path) makes GORM log the failed INSERT at Warn, and so does any INSERT slower than 200ms. Under
// the production logger (db.Logger) that line keeps its placeholders: no envelope, no master-key id.
func TestCredentialRepository_DuplicateNameLogKeepsEnvelopeOut(t *testing.T) {
	base := testGorm(t)
	var logged bytes.Buffer
	repo, ctx := NewRepository(base.Session(&gorm.Session{Logger: nexusdb.Logger(&logged)})), context.Background()
	tenant := newTenant(t, base)

	if err := repo.Create(ctx, newCred(tenant, "pay")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	tag := uuid.NewString()[:8]
	secret, prev, extra, kid := "v1:SECRETMARK"+tag, "v1:PREVMARK"+tag, "v1:EXTRAMARK"+tag, "KIDMARK"+tag
	dup := newCred(tenant, "pay")
	dup.SecretEnc, dup.PrevSecretEnc, dup.ExtraHeadersEnc, dup.KeyID = &secret, &prev, &extra, &kid
	if err := repo.Create(ctx, dup); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second Create = %v; want ErrNameTaken", err)
	}

	out := logged.String()
	if !strings.Contains(out, "1062") {
		t.Fatalf("GORM logged nothing for the failed INSERT — the leak check below would be vacuous; log: %q", out)
	}
	for _, m := range []string{secret, prev, extra, kid} {
		if strings.Contains(out, m) {
			t.Errorf("the duplicate-name log line carries sealed material %q:\n%s", m, out)
		}
	}
}

// TestCredentialRepository_NameReusableAfterDelete: a deleted key frees its name (a pack reinstall
// recreates "pay"), and deleting twice is not found.
func TestCredentialRepository_NameReusableAfterDelete(t *testing.T) {
	db := testGorm(t)
	repo, ctx := NewRepository(db), context.Background()
	tenant := newTenant(t, db)

	first := newCred(tenant, "pay")
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SoftDeleteShred(ctx, tenant, first.ID, "user-2"); err != nil {
		t.Fatalf("SoftDeleteShred: %v", err)
	}
	if err := repo.Create(ctx, newCred(tenant, "pay")); err != nil {
		t.Fatalf("re-Create after delete = %v; want the name free again", err)
	}
	if err := repo.SoftDeleteShred(ctx, tenant, first.ID, "user-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v; want ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, tenant, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(deleted) = %v; want ErrNotFound", err)
	}
}

// TestCredentialRepository_SoftDeleteShredsInOneUpdate: delete = tombstone + crypto-shred in ONE
// statement, so no crash window leaves a deleted row still holding sealed secrets (or a live row
// with its secrets gone).
func TestCredentialRepository_SoftDeleteShredsInOneUpdate(t *testing.T) {
	db := testGorm(t)
	repo, ctx, kr := NewRepository(db), context.Background(), testKeyring(t)
	tenant := newTenant(t, db)

	c := newCred(tenant, "pay")
	seal(t, kr, c, "partner-token-0123456789", `{"X-Doi-Tac":"v"}`)
	prev := *c.SecretEnc
	c.PrevSecretEnc = &prev
	graceEnds := time.Now().Add(24 * time.Hour).UTC()
	c.PrevExpiresAt = &graceEnds // a live grace window; delete must clear it too (queue #222 LOW 3)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stmts []string
	count := func(tx *gorm.DB) { stmts = append(stmts, tx.Statement.SQL.String()) }
	for _, reg := range []error{
		db.Callback().Update().After("gorm:update").Register("it:count_update", count),
		db.Callback().Raw().After("gorm:raw").Register("it:count_raw", count),
		db.Callback().Query().After("gorm:query").Register("it:count_query", count),
		db.Callback().Create().After("gorm:create").Register("it:count_create", count),
		db.Callback().Delete().After("gorm:delete").Register("it:count_delete", count),
	} {
		if reg != nil {
			t.Fatalf("register callback: %v", reg)
		}
	}
	if err := repo.SoftDeleteShred(ctx, tenant, c.ID, "user-2"); err != nil {
		t.Fatalf("SoftDeleteShred: %v", err)
	}
	if len(stmts) != 1 || !strings.HasPrefix(strings.ToUpper(stmts[0]), "UPDATE") {
		t.Fatalf("SoftDeleteShred ran %d statement(s) %q; want exactly one UPDATE", len(stmts), stmts)
	}

	var deleted, secretEnc, prevEnc, headersEnc, hint, keyID, updatedBy, prevExpires sql.NullString
	var names sql.NullString
	if err := db.Raw(`SELECT deleted_at, secret_enc, prev_secret_enc, extra_headers_enc, secret_hint, key_id,
			updated_by, extra_header_names, prev_expires_at FROM sys_named_credentials WHERE tenant_id = ? AND id = ?`, tenant, c.ID).
		Row().Scan(&deleted, &secretEnc, &prevEnc, &headersEnc, &hint, &keyID, &updatedBy, &names, &prevExpires); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !deleted.Valid {
		t.Fatal("deleted_at is NULL after delete")
	}
	for col, v := range map[string]sql.NullString{"secret_enc": secretEnc, "prev_secret_enc": prevEnc,
		"extra_headers_enc": headersEnc, "secret_hint": hint, "key_id": keyID, "prev_expires_at": prevExpires} {
		if v.Valid {
			t.Fatalf("%s = %q after delete; want NULL (crypto-shred)", col, v.String)
		}
	}
	if updatedBy.String != "user-2" || !strings.Contains(names.String, "X-Doi-Tac") {
		t.Fatalf("updated_by=%q extra_header_names=%q; want the deleter and the names kept for the audit trail", updatedBy.String, names.String)
	}
}

// TestCredentialRepository_TenantPinned: another tenant's id is not found on every path — read,
// update, delete — and never listed.
func TestCredentialRepository_TenantPinned(t *testing.T) {
	db := testGorm(t)
	repo, ctx := NewRepository(db), context.Background()
	tA, tB := newTenant(t, db), newTenant(t, db)

	c := newCred(tA, "pay")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.Get(ctx, tB, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get from tenant B = %v; want ErrNotFound", err)
	}
	hijack := *c
	hijack.BaseURL = "https://evil.example"
	if err := repo.UpdateMetadata(ctx, tB, &hijack); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateMetadata from tenant B = %v; want ErrNotFound", err)
	}
	if err := repo.SoftDeleteShred(ctx, tB, c.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SoftDeleteShred from tenant B = %v; want ErrNotFound", err)
	}
	if list, err := repo.List(ctx, tB); err != nil || len(list) != 0 {
		t.Fatalf("List(tenant B) = %d rows, %v; want none", len(list), err)
	}
	// The rotation lock is pinned too: another tenant's admin who knows the id never reaches fn,
	// so cannot overwrite the key.
	called := false
	if err := repo.WithTxForUpdate(ctx, tB, c.ID, func(*Credential) error { called = true; return nil }); !errors.Is(err, ErrNotFound) || called {
		t.Fatalf("WithTxForUpdate from tenant B = %v (fn called: %v); want ErrNotFound and no call", err, called)
	}

	// The owning tenant can update; an unchanged repeat is still found (MySQL reports 0 rows
	// changed for a same-value UPDATE).
	c.Label, c.BaseURL, c.UpdatedBy = "Cổng mới", "https://pay.example.vn/v2", "user-3"
	for i := 0; i < 2; i++ {
		if err := repo.UpdateMetadata(ctx, tA, c); err != nil {
			t.Fatalf("UpdateMetadata #%d: %v", i+1, err)
		}
	}
	got, err := repo.Get(ctx, tA, c.ID)
	if err != nil || got.Label != "Cổng mới" || got.BaseURL != "https://pay.example.vn/v2" || got.UpdatedBy != "user-3" {
		t.Fatalf("after update = %+v, %v", got, err)
	}
}

// TestCredentialRepository_UsagesJSONContains: which rules point at a key is read from the
// actions JSON, pinned to the tenant; a disabled rule still counts (it can be re-enabled), a
// soft-deleted one does not (spec edge case).
func TestCredentialRepository_UsagesJSONContains(t *testing.T) {
	db := testGorm(t)
	repo, ctx := NewRepository(db), context.Background()
	tA, tB := newTenant(t, db), newTenant(t, db)
	credX, credY := uuid.NewString(), uuid.NewString()

	rule := func(tenant, name string, active bool, deleted bool, actions string) string {
		id := uuid.NewString()
		q := `INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions, is_active, deleted_at)
			VALUES (?, ?, ?, 'crm_contacts', 'after_create', ?, ?, IF(?, NOW(3), NULL))`
		if err := db.Exec(q, id, tenant, name, actions, active, deleted).Error; err != nil {
			t.Fatalf("insert rule %s: %v", name, err)
		}
		return id
	}
	hook := func(cid string) string {
		return `{"type":"outbound_webhook","credentialId":"` + cid + `","path":"/orders"}`
	}
	email := `{"type":"email_alert","to":"a@b.vn"}`

	active := rule(tA, "b-active", true, false, "["+email+","+hook(credX)+"]")
	disabled := rule(tA, "a-disabled", false, false, "["+hook(credX)+","+hook(credX)+"]")
	rule(tA, "c-deleted", true, true, "["+hook(credX)+"]")
	rule(tA, "d-other", true, false, "["+hook(credY)+"]")
	rule(tA, "e-none", true, false, "["+email+"]")
	rule(tB, "f-foreign", true, false, "["+hook(credX)+"]") // same id string, other tenant

	got, err := repo.Usages(ctx, tA, credX)
	if err != nil {
		t.Fatalf("Usages: %v", err)
	}
	want := []Usage{
		{WorkflowID: disabled, WorkflowName: "a-disabled", ActionIndex: 0},
		{WorkflowID: disabled, WorkflowName: "a-disabled", ActionIndex: 1},
		{WorkflowID: active, WorkflowName: "b-active", ActionIndex: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("Usages(tA, X) = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Usages(tA, X)[%d] = %+v; want %+v", i, got[i], want[i])
		}
	}

	counts, err := repo.CountUsagesByTenant(ctx, tA)
	if err != nil {
		t.Fatalf("CountUsagesByTenant: %v", err)
	}
	if len(counts) != 2 || counts[credX] != 2 || counts[credY] != 1 {
		t.Fatalf("CountUsagesByTenant(tA) = %v; want {X: 2 rules, Y: 1 rule}", counts)
	}
	if counts, err := repo.CountUsagesByTenant(ctx, tB); err != nil || counts[credX] != 1 || len(counts) != 1 {
		t.Fatalf("CountUsagesByTenant(tB) = %v, %v; want only its own rule", counts, err)
	}
}
