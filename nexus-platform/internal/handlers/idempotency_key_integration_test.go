//go:build integration

// Idempotency-Key on POST /:entity — EMPIRICAL acceptance against real MySQL 8
// (specs/047-idempotency-key-on-create, DAILY-QUEUE #29/#30).
//
// The full production stack runs: RegisterAPIRoutes (Auth → Tenant) → DynamicRecordHandler →
// RecordService.CreateIdempotent → idempotency.SQLRepository (sys_idempotency_keys, mig 000069) +
// dynrepo (obj_*). The requests authenticate as ONE fixed principal — the key is scoped to
// (tenant, principal, key), so the package's tokenForTenant (a random user per call) would make
// every request a different key space.
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestIdempotencyKey
//
// The concurrency half (#30) lives in idempotency_key_concurrency_integration_test.go.
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/idempotency"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

type idemFixture struct {
	r         *gin.Engine
	sx        *sqlx.DB
	tenant    string
	entity    string
	token     string
	principal string
}

// newIdemFixture builds the router over the handlers suite's throwaway DB, creates a fresh entity
// in a fresh tenant, and mints ONE token for a fixed principal. wrap (optional) replaces the record
// store — the in-flight test (idempotency_key_concurrency_integration_test.go) passes a gated store.
func newIdemFixture(t *testing.T, wrap func(*dynrepo.Repository) services.RecordStore) *idemFixture {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = sx.Close() })
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	var store services.RecordStore = dyn
	if wrap != nil {
		store = wrap(dyn)
	}
	idem := idempotency.NewSQLRepository(sx)
	recordSvc := services.NewRecordService(meta, store).WithIdempotencyStore(idem).WithBulkInserter(dyn)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Import: NewImportHandler(services.NewImportService(meta, dyn, zap.NewNop()).WithIdempotencyStore(idem), zap.NewNop()),
	}, zap.NewNop())

	f := &idemFixture{r: r, sx: sx, tenant: uuid.NewString(), entity: "it_idem_" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""), principal: uuid.NewString()}
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{UserID: f.principal, TenantID: f.tenant, IsAdmin: true})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	f.token = tok
	t.Cleanup(func() {
		sx.Exec(`DELETE FROM sys_idempotency_keys WHERE tenant_id = ?`, f.tenant)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id WHERE e.tenant_id = ? AND e.name = ?`, f.tenant, f.entity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, f.tenant, f.entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + f.entity + "`")
	})

	w := f.post(t, "/api/v1/admin/entities", "", map[string]any{
		"name": f.entity, "label": "Phiếu thu",
		"fields": []map[string]any{
			{"name": "memo", "label": "Nội dung", "fieldType": "text", "isRequired": true},
			{"name": "amount", "label": "Số tiền", "fieldType": "number"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("seed entity: %d %s", w.Code, w.Body.String())
	}
	return f
}

func (f *idemFixture) post(t *testing.T, path, idemKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	switch b := body.(type) {
	case string:
		buf = []byte(b)
	default:
		buf, _ = json.Marshal(b)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func (f *idemFixture) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.sx.Get(&n, "SELECT COUNT(*) FROM `obj_"+f.entity+"` WHERE tenant_id = ? AND deleted_at IS NULL", f.tenant); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func createdID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("no data.id in %d %s", w.Code, w.Body.String())
	}
	return env.Data.ID
}

// #29 — a resend with the same key + body is a replay of the first record; a changed body is 422.
func TestIdempotencyKey_ResendReplaysTheFirstRecord(t *testing.T) {
	f := newIdemFixture(t, nil)
	path := "/api/v1/" + f.entity

	first := f.post(t, path, "abc-1", map[string]any{"memo": "Thu tiền đơn 1001", "amount": 250000})
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST = %d %s, want 201", first.Code, first.Body.String())
	}
	if first.Header().Get("Idempotent-Replay") != "" {
		t.Fatal("a first execution must not carry Idempotent-Replay")
	}
	id := createdID(t, first)

	// Same JSON, different key order and spacing — still the same request.
	second := f.post(t, path, "abc-1", `{ "amount": 250000.0,  "memo": "Thu tiền đơn 1001" }`)
	if second.Code != http.StatusOK {
		t.Fatalf("resend = %d %s, want 200 replay", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Idempotent-Replay"); got != "true" {
		t.Fatalf("Idempotent-Replay = %q, want \"true\"", got)
	}
	if got := createdID(t, second); got != id {
		t.Fatalf("replay id = %s, want the first record %s", got, id)
	}
	if n := f.rows(t); n != 1 {
		t.Fatalf("obj rows = %d after a resend, want 1", n)
	}

	reused := f.post(t, path, "abc-1", map[string]any{"memo": "Thu tiền đơn 1002", "amount": 250000})
	if reused.Code != http.StatusUnprocessableEntity || decodeCode(reused.Body.Bytes()) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("same key, other body = %d %s, want 422 IDEMPOTENCY_KEY_REUSED", reused.Code, reused.Body.String())
	}
	// Keys are case-sensitive tokens: "ABC-1" is a different key.
	if w := f.post(t, path, "ABC-1", map[string]any{"memo": "Thu tiền đơn 1002"}); w.Code != http.StatusCreated {
		t.Fatalf("ABC-1 = %d %s, want 201 (keys are case-sensitive)", w.Code, w.Body.String())
	}
	if n := f.rows(t); n != 2 {
		t.Fatalf("obj rows = %d, want 2", n)
	}

	// A failed create releases the key: the corrected resend runs.
	bad := f.post(t, path, "fix-me", map[string]any{"amount": 1})
	if bad.Code != http.StatusUnprocessableEntity || decodeCode(bad.Body.Bytes()) != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("invalid body = %d %s, want 422 RECORD_REQUIRED_MISSING", bad.Code, bad.Body.String())
	}
	if w := f.post(t, path, "fix-me", map[string]any{"memo": "đã sửa", "amount": 1}); w.Code != http.StatusCreated {
		t.Fatalf("corrected resend = %d %s, want 201", w.Code, w.Body.String())
	}

	// No header: the old behaviour, every POST creates.
	for i := 0; i < 2; i++ {
		if w := f.post(t, path, "", map[string]any{"memo": "không khoá"}); w.Code != http.StatusCreated {
			t.Fatalf("no-key POST %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	if n := f.rows(t); n != 5 {
		t.Fatalf("obj rows = %d, want 5", n)
	}
}

// A malformed key is refused before anything is created.
func TestIdempotencyKey_MalformedHeaderIs400(t *testing.T) {
	f := newIdemFixture(t, nil)
	path := "/api/v1/" + f.entity
	for name, key := range map[string]string{
		"space":      "abc 1",
		"too long":   strings.Repeat("k", 256),
		"non-ascii":  "khóa-1",
		"whitespace": "   ",
	} {
		w := f.post(t, path, key, map[string]any{"memo": "x"})
		if w.Code != http.StatusBadRequest || decodeCode(w.Body.Bytes()) != "INVALID_IDEMPOTENCY_KEY" {
			t.Fatalf("%s: %d %s, want 400 INVALID_IDEMPOTENCY_KEY", name, w.Code, w.Body.String())
		}
	}
	if w := f.post(t, path, strings.Repeat("k", 255), map[string]any{"memo": "x"}); w.Code != http.StatusCreated {
		t.Fatalf("255-char key = %d %s, want 201", w.Code, w.Body.String())
	}
	if n := f.rows(t); n != 1 {
		t.Fatalf("obj rows = %d, want 1 (only the valid key created)", n)
	}
}

// specs/047 FR-11 — a claim stuck in processing (the process died between claim and complete) is
// reconciled once its lease is over, against the record id the claim chose BEFORE the real dynrepo
// insert: the record exists → replay; it never committed → the resend takes the claim over. Inside
// the lease the resend still gets 409.
func TestIdempotencyKey_StaleProcessingClaimIsReconciled(t *testing.T) {
	f := newIdemFixture(t, nil)
	path := "/api/v1/" + f.entity
	body := map[string]any{"memo": "Thu tiền đơn 2001", "amount": 100}
	claimRecordID := func(key string) (status, recordID string) {
		var row struct {
			Status   string  `db:"status"`
			RecordID *string `db:"record_id"`
		}
		if err := f.sx.Get(&row, `SELECT status, record_id FROM sys_idempotency_keys WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`,
			f.tenant, f.principal, key); err != nil {
			t.Fatalf("read claim %s: %v", key, err)
		}
		if row.RecordID == nil {
			return row.Status, ""
		}
		return row.Status, *row.RecordID
	}
	// rewind puts the claim back to processing as if it was written `ago` before now, optionally
	// pointing at another record id (expires_at is claim time + 24 h, on the application clock).
	rewind := func(key string, ago time.Duration, recordID string) {
		q := `UPDATE sys_idempotency_keys SET status = 'processing', expires_at = ? WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`
		args := []any{time.Now().Add(-ago).Add(24 * time.Hour).UTC(), f.tenant, f.principal, key}
		if recordID != "" {
			q = `UPDATE sys_idempotency_keys SET status = 'processing', expires_at = ?, record_id = ? WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`
			args = []any{args[0], recordID, f.tenant, f.principal, key}
		}
		if _, err := f.sx.Exec(q, args...); err != nil {
			t.Fatalf("rewind claim: %v", err)
		}
	}

	first := f.post(t, path, "crash-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST = %d %s", first.Code, first.Body.String())
	}
	id := createdID(t, first)
	// The claim names the record id BEFORE the insert, and dynrepo inserted under exactly that id.
	if status, rid := claimRecordID("crash-1"); status != "completed" || rid != id {
		t.Fatalf("claim = (%s, %s), want completed with the created record %s", status, rid, id)
	}

	// Inside the lease: still the first request's — 409.
	rewind("crash-1", 30*time.Second, "")
	if w := f.post(t, path, "crash-1", body); w.Code != http.StatusConflict || decodeCode(w.Body.Bytes()) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("resend inside the lease = %d %s, want 409 IDEMPOTENCY_IN_PROGRESS", w.Code, w.Body.String())
	}

	// Past the lease, the create had committed (only its Complete was lost): replay, no second row.
	rewind("crash-1", 3*time.Minute, "")
	w := f.post(t, path, "crash-1", body)
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" || createdID(t, w) != id {
		t.Fatalf("resend past the lease = %d %s, want 200 replay of %s", w.Code, w.Body.String(), id)
	}
	if status, _ := claimRecordID("crash-1"); status != "completed" {
		t.Fatalf("reconciled claim status = %s, want completed", status)
	}
	if n := f.rows(t); n != 1 {
		t.Fatalf("obj rows = %d, want 1", n)
	}

	// Past the lease, nothing ever committed under the claim's id: the resend takes over and creates.
	ghost := uuid.NewString()
	rewind("crash-1", 3*time.Minute, ghost)
	w = f.post(t, path, "crash-1", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("resend on a claim whose create never committed = %d %s, want 201", w.Code, w.Body.String())
	}
	fresh := createdID(t, w)
	if fresh == ghost || fresh == id {
		t.Fatalf("take-over created %s, want a fresh id", fresh)
	}
	if status, rid := claimRecordID("crash-1"); status != "completed" || rid != fresh {
		t.Fatalf("taken-over claim = (%s, %s), want completed with %s", status, rid, fresh)
	}
	if n := f.rows(t); n != 2 {
		t.Fatalf("obj rows = %d, want 2", n)
	}
}
