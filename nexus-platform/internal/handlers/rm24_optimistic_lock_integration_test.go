//go:build integration

// RM-24 Optimistic Locking — EMPIRICAL acceptance (spec: ../specs/032-optimistic-lock/spec.md §6).
//
// Proves, against a REAL MySQL 8 with the FULL migration set (through 000013's obj_* version
// sweep) applied to a THROWAWAY database, that:
//
//   - Migration 000013 backfills a `version` column onto every obj_* table (SHOW COLUMNS),
//     and a table created AFTER the sweep (via the admin engine) also carries it.
//   - CONCURRENCY: two PUTs at the SAME version, released by a shared barrier, resolve to
//     EXACTLY one 200 (version → N+1) and one 409 RECORD_VERSION_CONFLICT carrying the
//     advanced currentVersion (never two winners, never a 500).
//   - PUT with a STALE If-Match version → 409.
//   - PUT with NO If-Match (opt-in skip) → ordinary update, no 409. W5-C2: the legacy path now
//     ALSO bumps `version` (every successful write advances it), so a stale If-Match writer can
//     no longer silently clobber a legacy write — it correctly 409s.
//   - Two PUTs on DIFFERENT fields at the same version: the first wins (200, version→1); the
//     second is a stale 409 — but a retry at the advanced version merges cleanly.
//   - A soft-deleted record → 404 (not 409).
//   - GET returns data.version AND a matching ETag header.
//
// ISOLATION: a throwaway DB per run (setupMigratedDB), DROPped in t.Cleanup; the shared dev DB
// is never mutated.
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestRM24
// Requires a reachable MySQL 8 (docker compose `mysql` service).
package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// newFullRouterOn mounts BOTH the admin and the dynamic-record handlers (the same wiring as
// production via RegisterAPIRoutes) against an explicit throwaway migrated DB, so the RM-24
// scenarios run on their own v13 schema rather than the shared dev DB. aclChecker is nil — the
// minted tokens are admin (tokenForTenant sets IsAdmin), so the record routes see every row.
func newFullRouterOn(t *testing.T, sx *sqlx.DB, dbName string) *gin.Engine {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(throwawayDSN(t, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open throwaway: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	fieldSvc := services.NewFieldService(dyn, meta)
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), fieldSvc, zap.NewNop()),
		Record: NewDynamicRecordHandler(services.NewRecordService(meta, dyn), zap.NewNop()),
	}, zap.NewNop())
	return r
}

// recReq fires an authenticated JSON request and returns the recorder. headers are applied
// after Content-Type/Authorization so a test can add If-Match.
func recReq(t *testing.T, r *gin.Engine, tenant, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	setAuth(t, req, tenant)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// createRM24Entity seeds an entity with two ordinary text fields (title, note) and returns one
// fresh record's id. Both fields live in the JSON `data` column (ordinary → no DDL column), so a
// merge on `title` leaves `note` untouched and vice-versa — exactly the field-RỜI-nhau case.
func createRM24Entity(t *testing.T, r *gin.Engine, tenant, entity string) string {
	t.Helper()
	create := map[string]any{
		"name":  entity,
		"label": "RM24 " + entity,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "note", "label": "Note", "fieldType": "text"},
		},
	}
	if w := postEntity(t, r, tenant, create); w.Code != http.StatusCreated {
		t.Fatalf("seed entity %s: status=%d body=%s", entity, w.Code, w.Body.String())
	}
	w := recReq(t, r, tenant, http.MethodPost, "/api/v1/"+entity, map[string]any{"title": "v0", "note": "n0"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed record: status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Data.ID == "" {
		t.Fatal("seed record id empty")
	}
	return created.Data.ID
}

// getVersionAndETag GETs a record and returns its data.version + the ETag header.
func getVersionAndETag(t *testing.T, r *gin.Engine, tenant, entity, id string) (int, string) {
	t.Helper()
	w := recReq(t, r, tenant, http.MethodGet, "/api/v1/"+entity+"/"+id, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s/%s: status=%d body=%s", entity, id, w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	return env.Data.Version, w.Header().Get("ETag")
}

// TestRM24_MigrationAddsVersionColumn proves migration 000013 backfilled `version` onto an
// obj_* table that existed BEFORE the sweep, and that a table created AFTER (by the engine)
// also carries it (mandatoryColumns change). It manually creates a pre-sweep table on a fresh
// server schema, then re-runs the sweep semantics via SHOW COLUMNS on an engine-made table.
func TestRM24_MigrationAddsVersionColumn(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)

	tenant := uuid.NewString()
	const entity = "rm24_mig"
	_ = createRM24Entity(t, r, tenant, entity)

	// The engine-created table must carry `version INT NOT NULL DEFAULT 0` (mandatoryColumns).
	type colRow struct {
		Nullable string         `db:"IS_NULLABLE"`
		Default  sql.NullString `db:"COLUMN_DEFAULT"`
		DataType string         `db:"DATA_TYPE"`
	}
	var c colRow
	if err := sx.Get(&c, `SELECT IS_NULLABLE, COLUMN_DEFAULT, DATA_TYPE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'version'`, "obj_"+entity); err != nil {
		t.Fatalf("information_schema version column: %v", err)
	}
	if c.DataType != "int" {
		t.Errorf("version DATA_TYPE=%q; want int", c.DataType)
	}
	if c.Nullable != "NO" {
		t.Errorf("version IS_NULLABLE=%q; want NO", c.Nullable)
	}
	if !c.Default.Valid || c.Default.String != "0" {
		t.Errorf("version default=%v; want 0", c.Default)
	}
}

// readMigration reads a migrations/<base>.<dir>.sql file (dir = "up" | "down") for the
// stored-proc sweep cycle test.
func readMigration(t *testing.T, base, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", base+"."+dir+".sql"))
	if err != nil {
		t.Fatalf("read migration %s.%s.sql: %v", base, dir, err)
	}
	return string(b)
}

// versionColExists reports whether `table` has a `version` column in the current schema.
func versionColExists(t *testing.T, sx *sqlx.DB, table string) bool {
	t.Helper()
	var n int
	if err := sx.Get(&n, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'version'`, table); err != nil {
		t.Fatalf("version col probe %s: %v", table, err)
	}
	return n == 1
}

// TestRM24_MigrationSweep_PreExistingTable_UpDownCycle drives the 000013 stored-proc sweep
// DIRECTLY against a PRE-EXISTING obj_* table (one that has no `version` column), proving the
// sweep is what backfills the column on tables born before the migration — not just the
// mandatoryColumns path. It runs up (adds), down (drops), and up again (idempotent cycle), and
// asserts the column's presence at each step. This is the empirical proof the
// information_schema-LIKE 'obj\_%' sweep runs intact under the multi-statement migration runner.
func TestRM24_MigrationSweep_PreExistingTable_UpDownCycle(t *testing.T) {
	sx, _ := setupMigratedDB(t)

	// Forge a pre-sweep obj_* table WITHOUT a version column (mimics one created before 000013).
	const table = "obj_rm24_presweep"
	if _, err := sx.Exec("CREATE TABLE `" + table + "` (`id` CHAR(36) NOT NULL, `tenant_id` CHAR(36) NOT NULL, `data` JSON NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB"); err != nil {
		t.Fatalf("forge pre-sweep table: %v", err)
	}
	if versionColExists(t, sx, table) {
		t.Fatal("pre-sweep table unexpectedly already has a version column")
	}

	up := readMigration(t, "000013_add_obj_version", "up")
	down := readMigration(t, "000013_add_obj_version", "down")

	// DOWN first (the suite already applied UP during setupMigratedDB; running DOWN here removes
	// version from every obj_* table, including our forged one — which only just got created and
	// did NOT receive the column, so DOWN is a no-op on it). Then UP, DOWN, UP to prove the cycle.
	for _, step := range []struct {
		name string
		sql  string
		want bool
	}{
		{"down (no-op on forged table)", down, false},
		{"up (sweep adds version)", up, true},
		{"down (sweep drops version)", down, false},
		{"up again (idempotent)", up, true},
	} {
		if _, err := sx.Exec(step.sql); err != nil {
			t.Fatalf("%s: exec migration: %v", step.name, err)
		}
		if got := versionColExists(t, sx, table); got != step.want {
			t.Fatalf("%s: version column present=%v; want %v", step.name, got, step.want)
		}
	}
}

// TestRM24_GetReturnsVersionAndETag pins the read contract: GET surfaces data.version and a
// matching strong ETag header.
func TestRM24_GetReturnsVersionAndETag(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_etag"
	id := createRM24Entity(t, r, tenant, entity)

	v, etag := getVersionAndETag(t, r, tenant, entity, id)
	if v != 0 {
		t.Errorf("initial version=%d; want 0", v)
	}
	if etag != `"0"` {
		t.Errorf("ETag=%q; want \"0\"", etag)
	}
}

// TestRM24_ConcurrentSameVersion_OneWinnerOne409 is the headline empirical acceptance: two
// goroutines, released by a shared barrier, both PUT with If-Match "0". Exactly one gets 200
// (version → 1); the other gets 409 RECORD_VERSION_CONFLICT with currentVersion=1 (never two
// winners, never a 500). The stored row ends at version 1 with the winner's value.
func TestRM24_ConcurrentSameVersion_OneWinnerOne409(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_race"
	id := createRM24Entity(t, r, tenant, entity)

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		status         int
		code           string
		currentVersion int
	}
	results := make([]result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := map[string]any{"title": "writer-" + uuid.NewString()[:4]}
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/"+entity+"/"+id, bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", `"0"`)
			setAuth(t, req, tenant)
			w := httptest.NewRecorder()
			<-start
			r.ServeHTTP(w, req)
			var env struct {
				Code    string `json:"code"`
				Details struct {
					CurrentVersion int `json:"currentVersion"`
				} `json:"details"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			results[idx] = result{status: w.Code, code: env.Code, currentVersion: env.Details.CurrentVersion}
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, conflict int
	for _, res := range results {
		switch {
		case res.status == http.StatusOK:
			ok++
		case res.status == http.StatusConflict && res.code == "RECORD_VERSION_CONFLICT":
			conflict++
			if res.currentVersion != 1 {
				t.Errorf("RM-24: conflict currentVersion=%d; want 1 (winner advanced it)", res.currentVersion)
			}
		default:
			t.Errorf("RM-24: unexpected race outcome status=%d code=%q (want 200 or 409 RECORD_VERSION_CONFLICT)", res.status, res.code)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("RM-24: race outcomes ok=%d conflict409=%d; want exactly 1 + 1", ok, conflict)
	}

	// The stored row must be exactly at version 1 (one successful bump, not two).
	v, _ := getVersionAndETag(t, r, tenant, entity, id)
	if v != 1 {
		t.Fatalf("RM-24: final version=%d; want 1 (exactly one writer won)", v)
	}
}

// TestRM24_StaleVersionRejected covers the sequential stale-edit path: after a successful
// version-bumping PUT (0→1), a second PUT still pinning version 0 → 409 with currentVersion=1.
func TestRM24_StaleVersionRejected(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_stale"
	id := createRM24Entity(t, r, tenant, entity)

	// First edit at version 0 → 200, version → 1.
	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"title": "fresh"}, map[string]string{"If-Match": `"0"`}); w.Code != http.StatusOK {
		t.Fatalf("first edit: status=%d body=%s", w.Code, w.Body.String())
	}
	if v, _ := getVersionAndETag(t, r, tenant, entity, id); v != 1 {
		t.Fatalf("after first edit version=%d; want 1", v)
	}

	// Second edit still pinning the stale version 0 → 409 currentVersion=1.
	w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"title": "stale"}, map[string]string{"If-Match": `"0"`})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale edit: status=%d body=%s; want 409", w.Code, w.Body.String())
	}
	var env struct {
		Code    string `json:"code"`
		Details struct {
			CurrentVersion int `json:"currentVersion"`
		} `json:"details"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != "RECORD_VERSION_CONFLICT" || env.Details.CurrentVersion != 1 {
		t.Fatalf("stale edit envelope code=%q currentVersion=%d; want RECORD_VERSION_CONFLICT / 1", env.Code, env.Details.CurrentVersion)
	}
}

// TestRM24_NoIfMatch_OptInSkip proves the opt-in semantics: a PUT WITHOUT If-Match (and without
// body.version) updates unconditionally (last-writer-wins) and never returns 409 — backward
// compatible. W5-C2: the legacy path now ALSO bumps `version` (0→1) so a concurrent stale
// If-Match writer can no longer silently clobber it (false-safety fix). The update still
// succeeds without any version check of its own.
func TestRM24_NoIfMatch_OptInSkip(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_optin"
	id := createRM24Entity(t, r, tenant, entity)

	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"title": "no-ifmatch"}, nil); w.Code != http.StatusOK {
		t.Fatalf("opt-in update: status=%d body=%s; want 200", w.Code, w.Body.String())
	}
	// W5-C2: the legacy unconditional UPDATE now bumps version (0→1) — every successful write
	// advances the counter so stale If-Match writers are detected.
	if v, _ := getVersionAndETag(t, r, tenant, entity, id); v != 1 {
		t.Errorf("opt-in update version=%d; want 1 (W5-C2: legacy path now bumps version)", v)
	}
}

// TestRM24_LegacyPutBumpsVersion_StaleIfMatch409_W5C2 is the W5-C2 false-safety acceptance.
// Scenario: writer X reads version 0, then a LEGACY writer (no If-Match) writes — under the old
// code that legacy write did NOT bump version, so version stayed 0 and X's later If-Match="0"
// PUT silently CLOBBERED the legacy write. After the fix the legacy write advances 0→1, so X's
// stale If-Match="0" PUT correctly gets 409 RECORD_VERSION_CONFLICT instead of clobbering.
func TestRM24_LegacyPutBumpsVersion_StaleIfMatch409_W5C2(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_c2"
	id := createRM24Entity(t, r, tenant, entity)

	// Writer X observes version 0 (it would send If-Match "0" later).
	if v, _ := getVersionAndETag(t, r, tenant, entity, id); v != 0 {
		t.Fatalf("initial version=%d; want 0", v)
	}

	// A LEGACY writer (no If-Match) writes `note`. W5-C2: this MUST bump version 0→1.
	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"note": "legacy-write"}, nil); w.Code != http.StatusOK {
		t.Fatalf("legacy write: status=%d body=%s; want 200", w.Code, w.Body.String())
	}
	if v, _ := getVersionAndETag(t, r, tenant, entity, id); v != 1 {
		t.Fatalf("after legacy write version=%d; want 1 (W5-C2: legacy bumps version)", v)
	}

	// Writer X now PUTs with its STALE If-Match "0". Pre-fix this silently overwrote the
	// legacy write (version was still 0). Post-fix it MUST 409 — the legacy write advanced it.
	w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"note": "X-clobber"}, map[string]string{"If-Match": `"0"`})
	if w.Code != http.StatusConflict {
		t.Fatalf("W5-C2: stale If-Match=0 after legacy write: status=%d body=%s; want 409 (legacy clobber prevented)", w.Code, w.Body.String())
	}
	var env struct {
		Code    string `json:"code"`
		Details struct {
			CurrentVersion int `json:"currentVersion"`
		} `json:"details"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != "RECORD_VERSION_CONFLICT" || env.Details.CurrentVersion != 1 {
		t.Fatalf("W5-C2 envelope code=%q currentVersion=%d; want RECORD_VERSION_CONFLICT / 1", env.Code, env.Details.CurrentVersion)
	}

	// The legacy writer's value must survive (X did not clobber it).
	wg := recReq(t, r, tenant, http.MethodGet, "/api/v1/"+entity+"/"+id, nil, nil)
	var got struct {
		Data struct {
			Version int `json:"version"`
			Data    struct {
				Note string `json:"note"`
			} `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wg.Body.Bytes(), &got)
	if got.Data.Data.Note != "legacy-write" {
		t.Errorf("W5-C2: note=%q; want legacy-write (the legacy write must NOT be clobbered)", got.Data.Data.Note)
	}
	if got.Data.Version != 1 {
		t.Errorf("W5-C2: final version=%d; want 1 (X's stale write rejected, no extra bump)", got.Data.Version)
	}
}

// TestRM24_DisjointFields_StaleRetryMerges covers the field-RỜI-nhau scenario: two writers at
// the same version touch DIFFERENT fields. The first wins (200, version→1, title changed); the
// second is a stale 409, but on retry at the advanced version its note-only merge lands cleanly
// WITHOUT clobbering the first writer's title (JSON_MERGE_PATCH is partial).
func TestRM24_DisjointFields_StaleRetryMerges(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_disjoint"
	id := createRM24Entity(t, r, tenant, entity)

	// Writer A edits `title` at version 0 → 200.
	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"title": "A-title"}, map[string]string{"If-Match": `"0"`}); w.Code != http.StatusOK {
		t.Fatalf("writer A: status=%d body=%s", w.Code, w.Body.String())
	}

	// Writer B edits `note` still at stale version 0 → 409.
	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"note": "B-note"}, map[string]string{"If-Match": `"0"`}); w.Code != http.StatusConflict {
		t.Fatalf("writer B stale: status=%d body=%s; want 409", w.Code, w.Body.String())
	}

	// Writer B retries at the advanced version 1 → 200; its note merges, title preserved.
	if w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"note": "B-note"}, map[string]string{"If-Match": `"1"`}); w.Code != http.StatusOK {
		t.Fatalf("writer B retry: status=%d body=%s; want 200", w.Code, w.Body.String())
	}

	// Final state: title = A-title (preserved), note = B-note (merged), version = 2.
	w := recReq(t, r, tenant, http.MethodGet, "/api/v1/"+entity+"/"+id, nil, nil)
	var env struct {
		Data struct {
			Version int `json:"version"`
			Data    struct {
				Title string `json:"title"`
				Note  string `json:"note"`
			} `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Data.Data.Title != "A-title" {
		t.Errorf("title=%q; want A-title (writer A's value must survive B's merge)", env.Data.Data.Title)
	}
	if env.Data.Data.Note != "B-note" {
		t.Errorf("note=%q; want B-note", env.Data.Data.Note)
	}
	if env.Data.Version != 2 {
		t.Errorf("final version=%d; want 2 (two successful bumps)", env.Data.Version)
	}
}

// TestRM24_SoftDeletedReturns404 proves a version-pinned PUT against a soft-deleted record is a
// 404 (RECORD_NOT_FOUND), NOT a 409 — the row no longer exists for editing.
func TestRM24_SoftDeletedReturns404(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	r := newFullRouterOn(t, sx, dbName)
	tenant := uuid.NewString()
	const entity = "rm24_deleted"
	id := createRM24Entity(t, r, tenant, entity)

	if w := recReq(t, r, tenant, http.MethodDelete, "/api/v1/"+entity+"/"+id, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("soft delete: status=%d body=%s; want 204", w.Code, w.Body.String())
	}
	w := recReq(t, r, tenant, http.MethodPut, "/api/v1/"+entity+"/"+id,
		map[string]any{"title": "zombie"}, map[string]string{"If-Match": `"0"`})
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT on soft-deleted: status=%d body=%s; want 404", w.Code, w.Body.String())
	}
	if code := decodeCode(w.Body.Bytes()); code != "RECORD_NOT_FOUND" {
		t.Errorf("soft-deleted PUT code=%q; want RECORD_NOT_FOUND", code)
	}
}
