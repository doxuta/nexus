//go:build integration

// RM-25 + RM-27 TOCTOU concurrency hardening — EMPIRICAL acceptance (SOUL §SOI SÂU).
//
// These tests prove the two admin-path races are closed against a REAL MySQL 8 with the full
// migration set (including 000012's sys_fields unique key) applied:
//
//	RM-25 — two concurrent POST /admin/fields for the SAME (tenant, entity, unique field)
//	         resolve to EXACTLY one 201 and one 409 FIELD_ALREADY_EXISTS (never a 500, never
//	         two winners). Drives the full HTTP → service → DB stack with a sync.WaitGroup
//	         barrier so both requests pass the application dup-check before either ALTER/INSERT.
//
//	RM-27 — two concurrent POST /admin/entities for the SAME (tenant, name) resolve to one 201
//	         and one 409, and the winner's physical obj_* table is NEVER dropped by the loser's
//	         compensation path.
//
// ISOLATION: each test run provisions a THROWAWAY database (name carries the PID + a nonce),
// applies every migrations/*.up.sql in lexical order against it (a fresh DB has no orphan
// rows, so the 000011 / 000012 orphan guards pass cleanly), runs the scenario, and DROPs the
// database in t.Cleanup. The shared dev DB is never mutated (gate-isolation lesson).
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestRM2
// Requires a reachable MySQL 8 (docker compose `mysql` service).
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// baseDSNNoDB returns the server DSN with the database name stripped, plus the original
// database name. We connect to the server (no default schema) to CREATE/DROP the throwaway DB.
// The DSN comes from TEST_DATABASE_URL (or the dsn() default); we replace the schema segment.
func baseDSNNoDB(t *testing.T) (serverDSN string) {
	t.Helper()
	full := dsn()
	// DSN shape: user:pass@tcp(host:port)/dbname?params
	slash := strings.LastIndex(full, "/")
	q := strings.Index(full[slash:], "?")
	prefix := full[:slash+1] // up to and including the '/'
	params := ""
	if q >= 0 {
		params = full[slash+q:]
	}
	// multiStatements is required to exec a whole migration file (stored-proc bodies) in one call.
	if params == "" {
		params = "?multiStatements=true&parseTime=true"
	} else if !strings.Contains(params, "multiStatements") {
		params += "&multiStatements=true"
	}
	return prefix + params // no db name -> connects to server
}

// throwawayDSN builds a DSN pointing at a named throwaway database, preserving params.
func throwawayDSN(t *testing.T, dbName string) string {
	t.Helper()
	server := baseDSNNoDB(t)
	slash := strings.LastIndex(server, "/")
	return server[:slash+1] + dbName + server[slash+1:]
}

// setupMigratedDB provisions a fresh throwaway database, applies every migrations/*.up.sql in
// lexical order, and returns a sqlx handle to it. It registers a t.Cleanup that DROPs the DB.
// A fresh DB guarantees the 000011/000012 orphan guards have nothing to trip on.
func setupMigratedDB(t *testing.T) (*sqlx.DB, string) {
	t.Helper()
	server, err := sqlx.Connect("mysql", baseDSNNoDB(t))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	defer server.Close()

	dbName := fmt.Sprintf("nexus_rm2527_%d_%s", os.Getpid(), strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	if _, err := server.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create throwaway db %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		s2, err := sqlx.Connect("mysql", baseDSNNoDB(t))
		if err == nil {
			_, _ = s2.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = s2.Close()
		}
	})

	db, err := sqlx.Connect("mysql", throwawayDSN(t, dbName))
	if err != nil {
		t.Fatalf("connect throwaway db %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applyMigrations(t, db)
	return db, dbName
}

// applyMigrations reads migrations/*.up.sql (relative to this test package, which lives at
// internal/handlers; the migrations dir is at repo-root/migrations) and execs each in lexical
// order. Each file is run as a single multi-statement Exec so stored-procedure bodies (the
// orphan guards) execute intact.
func applyMigrations(t *testing.T, db *sqlx.DB) {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir %s: %v", dir, err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}

// newAdminRouterOn builds the full A2-protected admin router against an explicit DSN/handle so
// the concurrency tests run on their own migrated throwaway DB (not the shared dev DB).
func newAdminRouterOn(t *testing.T, sx *sqlx.DB, dbName string) *gin.Engine {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(throwawayDSN(t, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open throwaway: %v", err)
	}
	svc := services.NewEntityService(dynrepo.New(sx), metadata.NewRepository(gdb))
	fieldSvc := services.NewFieldService(dynrepo.New(sx), metadata.NewRepository(gdb))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin: NewAdminEntityHandler(svc, fieldSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// raceResult captures one concurrent request's HTTP status + decoded error code.
type raceResult struct {
	status int
	code   string
}

// decodeCode extracts the flat error envelope's "code" field from a response body, "" if none.
// The handler emits {"error": <msg>, "code": <code>, "details": ...} (a FLAT shape, not nested).
func decodeCode(body []byte) string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Code
}

// TestRM25_ConcurrentAddField_UniqueField_OneWinnerOne409 is the RM-25 empirical acceptance:
// two goroutines, released by a shared barrier, both POST /admin/fields with the SAME unique
// field on the SAME entity. Exactly one must get 201; the other must get 409 FIELD_ALREADY_EXISTS
// (NOT 500). After both finish, the entity must have exactly one 'email' field row and the
// physical generated column must exist exactly once.
func TestRM25_ConcurrentAddField_UniqueField_OneWinnerOne409(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newAdminRouterOn(t, sx, dbName)

	tenant := uuid.NewString()
	const entity = "rm25_widgets"

	// Create the entity first (sequential — not under test).
	createBody := map[string]any{
		"name":  entity,
		"label": "RM25 Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		},
	}
	if w := postEntity(t, r, tenant, createBody); w.Code != http.StatusCreated {
		t.Fatalf("seed entity: status=%d body=%s", w.Code, w.Body.String())
	}

	// Two concurrent AddField calls for the SAME unique field 'email'.
	fieldBody := map[string]any{
		"entityName": entity,
		"name":       "email", "label": "Email", "fieldType": "text", "isUnique": true,
	}

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{}) // barrier: release all goroutines simultaneously
	results := make([]raceResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			buf, _ := json.Marshal(fieldBody)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/fields", bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			setAuth(t, req, tenant)
			w := httptest.NewRecorder()
			<-start // wait at the barrier
			r.ServeHTTP(w, req)
			results[idx] = raceResult{status: w.Code, code: decodeCode(w.Body.Bytes())}
		}(i)
	}
	close(start) // fire
	wg.Wait()

	// Exactly one 201, exactly one 409 FIELD_ALREADY_EXISTS, never a 500.
	var created, conflict, other int
	for _, res := range results {
		switch {
		case res.status == http.StatusCreated:
			created++
		case res.status == http.StatusConflict && res.code == "FIELD_ALREADY_EXISTS":
			conflict++
		default:
			other++
			t.Errorf("RM-25: unexpected race outcome status=%d code=%q (want 201 or 409 FIELD_ALREADY_EXISTS)", res.status, res.code)
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("RM-25: race outcomes created=%d conflict409=%d other=%d; want exactly 1 + 1", created, conflict, other)
	}

	// Exactly ONE 'email' field row (no phantom second row) and exactly one physical column.
	var rows int
	if err := sx.Get(&rows, `SELECT COUNT(*) FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id
		WHERE e.tenant_id=? AND e.name=? AND f.name=?`, tenant, entity, "email"); err != nil {
		t.Fatalf("count email field rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("RM-25: sys_fields has %d 'email' rows after the race; want exactly 1", rows)
	}
	var cols int
	if err := sx.Get(&cols, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`, "obj_"+entity, "email"); err != nil {
		t.Fatalf("count email column: %v", err)
	}
	if cols != 1 {
		t.Errorf("RM-25: obj_%s has %d 'email' physical columns; want exactly 1", entity, cols)
	}
}

// TestRM25_ConcurrentAddField_OrdinaryField_OneWinnerOne409 covers the ORDINARY (non-unique,
// non-relation, non-indexed) field path, where NO DDL runs — the ONLY race serializer is the
// 000012 sys_fields unique key (metadata.ErrDuplicateField → 409). Without 000012 this race
// would persist two phantom sys_fields rows.
func TestRM25_ConcurrentAddField_OrdinaryField_OneWinnerOne409(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newAdminRouterOn(t, sx, dbName)

	tenant := uuid.NewString()
	const entity = "rm25_plain"

	if w := postEntity(t, r, tenant, map[string]any{
		"name": entity, "label": "RM25 Plain",
		"fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed entity: status=%d body=%s", w.Code, w.Body.String())
	}

	fieldBody := map[string]any{
		"entityName": entity,
		"name":       "note", "label": "Note", "fieldType": "text",
	}

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]raceResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			buf, _ := json.Marshal(fieldBody)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/fields", bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			setAuth(t, req, tenant)
			w := httptest.NewRecorder()
			<-start
			r.ServeHTTP(w, req)
			results[idx] = raceResult{status: w.Code, code: decodeCode(w.Body.Bytes())}
		}(i)
	}
	close(start)
	wg.Wait()

	var created, conflict int
	for _, res := range results {
		switch {
		case res.status == http.StatusCreated:
			created++
		case res.status == http.StatusConflict && res.code == "FIELD_ALREADY_EXISTS":
			conflict++
		default:
			t.Errorf("RM-25 ordinary: unexpected status=%d code=%q", res.status, res.code)
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("RM-25 ordinary: created=%d conflict409=%d; want 1 + 1", created, conflict)
	}
	var rows int
	if err := sx.Get(&rows, `SELECT COUNT(*) FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id
		WHERE e.tenant_id=? AND e.name=? AND f.name=?`, tenant, entity, "note"); err != nil {
		t.Fatalf("count note rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("RM-25 ordinary: %d 'note' rows after the race; want exactly 1 (no phantom row)", rows)
	}
}

// TestRM27_ConcurrentCreateEntity_OneWinnerOne409_TableSurvives is the RM-27 empirical
// acceptance: two goroutines, released by a shared barrier, both POST /admin/entities with the
// SAME (tenant, name). Exactly one must get 201; the other 409 ENTITY_ALREADY_EXISTS. The
// winner's physical obj_* table MUST survive — the loser must NOT DropTable it.
func TestRM27_ConcurrentCreateEntity_OneWinnerOne409_TableSurvives(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newAdminRouterOn(t, sx, dbName)

	tenant := uuid.NewString()
	const entity = "rm27_race"

	body := map[string]any{
		"name":  entity,
		"label": "RM27 Race",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "sku", "label": "SKU", "fieldType": "text", "isUnique": true},
		},
	}

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]raceResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/entities", bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			setAuth(t, req, tenant)
			w := httptest.NewRecorder()
			<-start
			r.ServeHTTP(w, req)
			results[idx] = raceResult{status: w.Code, code: decodeCode(w.Body.Bytes())}
		}(i)
	}
	close(start)
	wg.Wait()

	var created, conflict int
	for _, res := range results {
		switch {
		case res.status == http.StatusCreated:
			created++
		case res.status == http.StatusConflict && res.code == "ENTITY_ALREADY_EXISTS":
			conflict++
		default:
			t.Errorf("RM-27: unexpected status=%d code=%q (want 201 or 409 ENTITY_ALREADY_EXISTS)", res.status, res.code)
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("RM-27: created=%d conflict409=%d; want exactly 1 + 1", created, conflict)
	}

	// THE RM-27 ASSERTION: the winner's physical table MUST still exist (loser did not drop it).
	var tbl int
	if err := sx.Get(&tbl, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?`, "obj_"+entity); err != nil {
		t.Fatalf("check table survives: %v", err)
	}
	if tbl != 1 {
		t.Fatalf("RM-27: obj_%s does NOT exist after the race — the loser dropped the winner's table", entity)
	}
	// The winner's unique 'sku' physical column must also survive intact.
	var skuCol int
	if err := sx.Get(&skuCol, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`, "obj_"+entity, "sku"); err != nil {
		t.Fatalf("check sku column: %v", err)
	}
	if skuCol != 1 {
		t.Errorf("RM-27: winner's unique 'sku' column missing on obj_%s after the race", entity)
	}
	// Exactly one sys_entities row.
	var ent int
	if err := sx.Get(&ent, `SELECT COUNT(*) FROM sys_entities WHERE tenant_id=? AND name=?`, tenant, entity); err != nil {
		t.Fatalf("count entity rows: %v", err)
	}
	if ent != 1 {
		t.Errorf("RM-27: %d sys_entities rows after the race; want exactly 1", ent)
	}
}
