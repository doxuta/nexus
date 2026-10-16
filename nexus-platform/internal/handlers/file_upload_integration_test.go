//go:build integration

// File-upload M1 integration test (ENGINE-COVERAGE-SPEC §EPIC CROSS "File / media / document
// attachment"). It exercises the WHOLE upload→download→delete chain over the REAL migrated schema
// (golang-migrate, via a4SetupMigratedDB so the hand-written sys_files migration runs exactly as
// production deploy does, NOT AutoMigrate) and the genuine Auth→Tenant→ACL→handler→service→storage
// stack wired exactly like cmd/server/main.go.
//
// Coverage (task acceptance):
//   - upload     : POST /:entity/:id/files (multipart) stores the blob on the LocalBackend, catalogs
//     it in sys_files, and syncs the record's file field [{id,name}] reference array.
//   - download   : GET /files/:id streams the stored bytes back with the original filename/mime.
//   - size-limit : an upload over MAX_UPLOAD_BYTES is rejected 413 (FILE_TOO_LARGE) and leaves no row.
//   - mime       : a content type outside the whitelist is rejected 415 (FILE_MIME_NOT_ALLOWED).
//   - tenant-iso : tenant B cannot download / delete tenant A's file — it surfaces as a 404, never a
//     cross-tenant leak (the file id is a per-tenant secret; same not-found as a miss).
//
// The suite SKIPS (not fails) when MySQL or the golang-migrate CLI is unavailable (off-CI dev box),
// mirroring the rest of the A4/A5 integration suite.
package handlers

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/files"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/storage"
)

// fileTestEnv bundles the migrated-DB-backed router with file uploads wired, plus the file config
// the test asserts against (size cap, whitelist).
type fileTestEnv struct {
	engine  *gin.Engine
	gdb     *gorm.DB
	sx      *sqlx.DB
	maxByte int64
}

// newFileRouter stands up the production /api/v1 surface (admin entities + records + the File-upload
// M1 routes) over the migrated throwaway DB, with a LocalBackend rooted at a per-test temp dir and
// the upload policy driven by env (so the size-limit / mime cases are config-true, not hardcoded).
// withACL=false leaves the ACL stage disabled (nil aclChecker) — the M1 suite targets the file
// subsystem + tenant isolation, which the service enforces against the parent record's tenant/A5
// scope regardless of ACL. withACL=true wires the real ACL service exactly like main.go (route stage
// + fileSvc.WithACL), for the specs/060 non-admin upload scenarios.
func newFileRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB, withACL bool) *fileTestEnv {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")

	// Drive the upload policy from env so the limits the handler/service enforce are the same the
	// test asserts. The cap is 512 bytes: comfortably above the happy-path file + multipart
	// envelope overhead (boundary/headers add ~200B), but well under the oversize case's payload —
	// so the size-limit assertion is deterministic and not tripped by envelope bytes. The whitelist
	// accepts text/plain (happy path) but NOT application/x-bogus (mime case).
	t.Setenv("FILE_STORAGE_DIR", t.TempDir())
	t.Setenv("MAX_UPLOAD_BYTES", "512")
	t.Setenv("UPLOAD_MIME_WHITELIST", "text/plain,image/png")

	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}

	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)
	userSvc := services.NewUserService(userRepo)
	roleSvc := services.NewRoleService(auth.NewRoleRepository(gdb))
	teamSvc := services.NewTeamService(auth.NewTeamRepository(gdb))

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	recordSvc := services.NewRecordService(meta, dyn)

	// File-upload M1 wiring — mirrors cmd/server/main.go exactly.
	fileCfg := config.LoadFileConfig()
	backend, err := storage.NewLocalBackend(fileCfg.StorageDir)
	if err != nil {
		t.Fatalf("storage.NewLocalBackend: %v", err)
	}
	fileSvc := files.NewService(
		files.NewRepository(sx),
		backend,
		recordSvc,
		files.Limits{MaxBytes: fileCfg.MaxUploadBytes, MimeOK: fileCfg.MimeAllowed},
		nil,
	)

	var aclChecker ACLChecker
	if withACL {
		aclSvc := services.NewACLService(auth.NewRoleRepository(gdb))
		fileSvc.WithACL(aclSvc)
		aclChecker = aclSvc
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclChecker, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		File:     NewFileHandler(fileSvc, fileCfg.MaxUploadBytes, zap.NewNop()),
	}, zap.NewNop())

	return &fileTestEnv{engine: r, gdb: gdb, sx: sx, maxByte: fileCfg.MaxUploadBytes}
}

// createFileEntity registers a dynamic entity carrying a `file` field (so the upload endpoint has a
// valid attachment target) plus a required text field, via the admin API as the given admin token.
func (e *fileTestEnv) createFileEntity(t *testing.T, adminToken, name string) {
	t.Helper()
	body := map[string]any{
		"name":  name,
		"label": name,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "doc", "label": "Document", "fieldType": "file", "isRequired": false},
		},
	}
	w := doAuthed(t, e.engine, http.MethodPost, "/api/v1/admin/entities", adminToken, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity %s: %d %s", name, w.Code, w.Body.String())
	}
}

// createRecord inserts a record into the entity and returns its id.
func (e *fileTestEnv) createRecord(t *testing.T, token, entity, title string) string {
	t.Helper()
	w := doAuthed(t, e.engine, http.MethodPost, "/api/v1/"+entity, token, map[string]any{"title": title}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create record in %s: %d %s", entity, w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create record: %v (%s)", err, w.Body.String())
	}
	id, _ := resp.Data["id"].(string)
	if id == "" {
		t.Fatalf("create record: empty id in %s", w.Body.String())
	}
	return id
}

// uploadFile POSTs a multipart upload to /:entity/:id/files?field=<field>, returning the recorder.
// contentType is set as the part's Content-Type so the mime whitelist is exercised through the
// real path (the handler reads fileHeader.Header.Get("Content-Type")).
func (e *fileTestEnv) uploadFile(t *testing.T, token, entity, recordID, field, filename, contentType string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)}
	hdr["Content-Type"] = []string{contentType}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart body: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/"+entity+"/"+recordID+"/files?field="+field, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// countFileRows returns the number of live (not soft-deleted) sys_files rows for a tenant.
func (e *fileTestEnv) countFileRows(t *testing.T, tenantID string) int {
	t.Helper()
	var n int
	if err := e.sx.Get(&n, "SELECT COUNT(*) FROM sys_files WHERE tenant_id = ? AND deleted_at IS NULL", tenantID); err != nil {
		t.Fatalf("count sys_files: %v", err)
	}
	return n
}

// TestFileUpload_M1_EndToEnd is the headline File-upload M1 acceptance: upload + download +
// size-limit + mime + tenant-isolation, all over the real migrated schema and full HTTP stack.
func TestFileUpload_M1_EndToEnd(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	env := newFileRouter(t, gdb, sx, false)

	const (
		entity   = "fileeng_docs"
		tenantA  = "11111111-1111-1111-1111-111111111111"
		tenantB  = "22222222-2222-2222-2222-222222222222"
		slugA    = "file-tenant-a"
		slugB    = "file-tenant-b"
		emailA   = "a-admin@file.test"
		emailB   = "b-admin@file.test"
		fieldDoc = "doc"
	)

	a4SeedTenant(t, gdb, tenantA, slugA, "File Tenant A")
	a4SeedTenant(t, gdb, tenantB, slugB, "File Tenant B")
	a4SeedUser(t, gdb, tenantA, emailA, true, nil) // admin so entity-create + record write pass
	a4SeedUser(t, gdb, tenantB, emailB, true, nil)
	t.Cleanup(func() { a4DropEntity(sx, []string{tenantA, tenantB}, entity) })
	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_files WHERE tenant_id IN (?, ?)", tenantA, tenantB)
	})

	tokenA := itLogin(t, env.engine, slugA, emailA, authPassword).Data.AccessToken
	tokenB := itLogin(t, env.engine, slugB, emailB, authPassword).Data.AccessToken

	// Each tenant gets its OWN entity + record (the entity is per-tenant in sys_entities). Tenant B
	// creating the same-named entity proves the metadata is tenant-scoped, not shared.
	env.createFileEntity(t, tokenA, entity)
	env.createFileEntity(t, tokenB, entity)
	recordA := env.createRecord(t, tokenA, entity, "Record A")

	// --- 1) UPLOAD (happy path) -----------------------------------------------------------------
	payload := []byte("hello file engine") // 17 bytes, under the 64-byte cap
	wUp := env.uploadFile(t, tokenA, entity, recordA, fieldDoc, "report.txt", "text/plain", payload)
	if wUp.Code != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d %s", wUp.Code, wUp.Body.String())
	}
	var upResp struct {
		Data files.UploadResult `json:"data"`
	}
	if err := json.Unmarshal(wUp.Body.Bytes(), &upResp); err != nil {
		t.Fatalf("decode upload: %v (%s)", err, wUp.Body.String())
	}
	fileID := upResp.Data.ID
	if fileID == "" {
		t.Fatalf("upload: empty file id in %s", wUp.Body.String())
	}
	if upResp.Data.Name != "report.txt" {
		t.Errorf("upload name = %q, want report.txt", upResp.Data.Name)
	}
	if upResp.Data.Size != int64(len(payload)) {
		t.Errorf("upload size = %d, want %d", upResp.Data.Size, len(payload))
	}
	if !strings.HasSuffix(upResp.Data.URL, "/files/"+fileID) {
		t.Errorf("upload url = %q, want suffix /files/%s", upResp.Data.URL, fileID)
	}
	// The catalog row exists for tenant A only.
	if got := env.countFileRows(t, tenantA); got != 1 {
		t.Fatalf("tenant A file rows = %d, want 1", got)
	}
	if got := env.countFileRows(t, tenantB); got != 0 {
		t.Fatalf("tenant B file rows = %d, want 0 (no cross-tenant leak)", got)
	}
	// #54: the catalog carries the sha256 of the stored bytes (migration 000068), so the restore
	// drill can compare content, not just length.
	var gotSum sql.NullString
	if err := env.sx.Get(&gotSum, "SELECT sha256 FROM sys_files WHERE tenant_id = ? AND id = ?", tenantA, fileID); err != nil {
		t.Fatalf("read sha256: %v", err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(payload)); gotSum.String != want {
		t.Errorf("sys_files.sha256 = %q (valid=%v), want %s", gotSum.String, gotSum.Valid, want)
	}
	// The record's file field now carries the [{id,name}] reference (sync after upload).
	wRec := doAuthed(t, env.engine, http.MethodGet, "/api/v1/"+entity+"/"+recordA, tokenA, nil, nil)
	if wRec.Code != http.StatusOK {
		t.Fatalf("get record after upload: %d %s", wRec.Code, wRec.Body.String())
	}
	if !strings.Contains(wRec.Body.String(), fileID) {
		t.Errorf("record field did not sync file id %s: %s", fileID, wRec.Body.String())
	}

	// --- 2) DOWNLOAD (happy path) ---------------------------------------------------------------
	wDl := doAuthed(t, env.engine, http.MethodGet, "/api/v1/files/"+fileID, tokenA, nil, nil)
	if wDl.Code != http.StatusOK {
		t.Fatalf("download: want 200, got %d %s", wDl.Code, wDl.Body.String())
	}
	if got := wDl.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Errorf("download body = %q, want %q", got, payload)
	}
	if ct := wDl.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("download Content-Type = %q, want text/plain", ct)
	}
	if cd := wDl.Header().Get("Content-Disposition"); !strings.Contains(cd, "report.txt") {
		t.Errorf("download Content-Disposition = %q, want filename report.txt", cd)
	}

	// --- 3) SIZE LIMIT (413) --------------------------------------------------------------------
	big := bytes.Repeat([]byte("X"), int(env.maxByte)+50) // over the 64-byte cap
	wBig := env.uploadFile(t, tokenA, entity, recordA, fieldDoc, "big.txt", "text/plain", big)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: want 413, got %d %s", wBig.Code, wBig.Body.String())
	}
	if got := env.countFileRows(t, tenantA); got != 1 {
		t.Fatalf("after rejected oversize upload, tenant A rows = %d, want still 1 (no orphan)", got)
	}

	// --- 4) MIME WHITELIST (415) ----------------------------------------------------------------
	wMime := env.uploadFile(t, tokenA, entity, recordA, fieldDoc, "evil.bin", "application/x-bogus", []byte("nope"))
	if wMime.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("disallowed mime upload: want 415, got %d %s", wMime.Code, wMime.Body.String())
	}
	if got := env.countFileRows(t, tenantA); got != 1 {
		t.Fatalf("after rejected mime upload, tenant A rows = %d, want still 1", got)
	}

	// --- 5) TENANT ISOLATION --------------------------------------------------------------------
	// Tenant B knows the file id but must NOT be able to download or delete tenant A's file. The
	// repo pins tenant_id, so B's GET/DELETE surface as a clean 404 (no existence oracle, no leak).
	wXdl := doAuthed(t, env.engine, http.MethodGet, "/api/v1/files/"+fileID, tokenB, nil, nil)
	if wXdl.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant download: want 404, got %d %s", wXdl.Code, wXdl.Body.String())
	}
	wXdel := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+fileID, tokenB, nil, nil)
	if wXdel.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: want 404, got %d %s", wXdel.Code, wXdel.Body.String())
	}
	// The file is still there for tenant A (B's attempts were no-ops).
	if got := env.countFileRows(t, tenantA); got != 1 {
		t.Fatalf("after cross-tenant attempts, tenant A rows = %d, want still 1", got)
	}

	// --- 6) DELETE (own file) -------------------------------------------------------------------
	wDel := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+fileID, tokenA, nil, nil)
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("delete own file: want 204, got %d %s", wDel.Code, wDel.Body.String())
	}
	if got := env.countFileRows(t, tenantA); got != 0 {
		t.Fatalf("after delete, tenant A rows = %d, want 0", got)
	}
	// Download after delete is a 404 (soft-deleted row is invisible).
	wGone := doAuthed(t, env.engine, http.MethodGet, "/api/v1/files/"+fileID, tokenA, nil, nil)
	if wGone.Code != http.StatusNotFound {
		t.Fatalf("download after delete: want 404, got %d %s", wGone.Code, wGone.Body.String())
	}
	// A double-delete is also a clean 404.
	wDel2 := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+fileID, tokenA, nil, nil)
	if wDel2.Code != http.StatusNotFound {
		t.Fatalf("double delete: want 404, got %d %s", wDel2.Code, wDel2.Body.String())
	}
}

// TestFileUpload_EditGrantWithoutCreate is the specs/060 (DAILY-QUEUE #269) acceptance over the real
// migrated schema and the REAL ACL service: a technician whose role may VIEW and EDIT their own work
// orders — and may NOT create one — attaches the acceptance photo to the order they own. Before the
// fix the route stage asked for "create" (POST on recordGroup) and the service then asked for "edit",
// so this person got 403 on every upload; granting create to unblock it also let them open orders.
func TestFileUpload_EditGrantWithoutCreate(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	env := newFileRouter(t, gdb, sx, true)

	const (
		entity      = "filacl_phieu"
		tenant      = "60606060-6060-6060-6060-606060606060"
		slug        = "file-acl"
		adminEmail  = "admin@file-acl.test"
		ktvEmail    = "ktv@file-acl.test"
		viewerEmail = "xem@file-acl.test"
		fieldDoc    = "doc"
	)
	a4SeedTenant(t, gdb, tenant, slug, "File ACL")
	a4SeedUser(t, gdb, tenant, adminEmail, true, nil)
	ktvRole := a4SeedRole(t, gdb, tenant, "ky_thuat_vien", []auth.RoleAction{
		{EntityName: entity, Action: "view", AccessLevel: "owner"},
		{EntityName: entity, Action: "edit", AccessLevel: "owner"},
	})
	viewerRole := a4SeedRole(t, gdb, tenant, "chi_xem", []auth.RoleAction{grant(entity, "view")})
	ktvID := a4SeedUser(t, gdb, tenant, ktvEmail, false, &ktvRole)
	a4SeedUser(t, gdb, tenant, viewerEmail, false, &viewerRole)
	t.Cleanup(func() { a4DropEntity(sx, []string{tenant}, entity) })
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_files WHERE tenant_id = ?", tenant) })

	adminTok := itLogin(t, env.engine, slug, adminEmail, authPassword).Data.AccessToken
	env.createFileEntity(t, adminTok, entity)
	own := env.createRecord(t, adminTok, entity, "Phiếu của KTV")
	other := env.createRecord(t, adminTok, entity, "Phiếu người khác")
	// Hand the first order to the technician — the owner an AccessOwner grant reads.
	if w := doAuthed(t, env.engine, http.MethodPut, "/api/v1/"+entity+"/"+own+"/owner", adminTok,
		map[string]any{"userId": ktvID}, nil); w.Code != http.StatusOK {
		t.Fatalf("assign owner: %d %s", w.Code, w.Body.String())
	}
	ktvTok := itLogin(t, env.engine, slug, ktvEmail, authPassword).Data.AccessToken
	viewerTok := itLogin(t, env.engine, slug, viewerEmail, authPassword).Data.AccessToken

	// 1) Edit (own) without create: the photo lands on the order the technician owns.
	wUp := env.uploadFile(t, ktvTok, entity, own, fieldDoc, "nghiem-thu.txt", "text/plain", []byte("anh nghiem thu"))
	if wUp.Code != http.StatusCreated {
		t.Fatalf("KTV upload to own order: want 201, got %d %s", wUp.Code, wUp.Body.String())
	}
	var up struct {
		Data files.UploadResult `json:"data"`
	}
	if err := json.Unmarshal(wUp.Body.Bytes(), &up); err != nil || up.Data.ID == "" {
		t.Fatalf("decode upload: %v (%s)", err, wUp.Body.String())
	}

	// 2) Someone else's order stays out of reach — the same not-found an unreadable record gives.
	if w := env.uploadFile(t, ktvTok, entity, other, fieldDoc, "x.txt", "text/plain", []byte("x")); w.Code != http.StatusNotFound {
		t.Fatalf("KTV upload to another person's order: want 404, got %d %s", w.Code, w.Body.String())
	}

	// 3) View-only is refused at the route stage.
	if w := env.uploadFile(t, viewerTok, entity, own, fieldDoc, "x.txt", "text/plain", []byte("x")); w.Code != http.StatusForbidden {
		t.Fatalf("view-only upload: want 403, got %d %s", w.Code, w.Body.String())
	}
	if got := env.countFileRows(t, tenant); got != 1 {
		t.Fatalf("file rows = %d, want 1 (the refused uploads must store nothing)", got)
	}

	// 4) The fix did not need a create grant: the technician still cannot open an order.
	if w := doAuthed(t, env.engine, http.MethodPost, "/api/v1/"+entity, ktvTok, map[string]any{"title": "Phiếu mới"}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("KTV create: want 403, got %d %s", w.Code, w.Body.String())
	}

	// 5) Saving the order afterwards WITHOUT the file key (what the record form now sends) keeps the
	// attachment: a partial PUT leaves an absent field alone.
	if w := doAuthed(t, env.engine, http.MethodPut, "/api/v1/"+entity+"/"+own, ktvTok, map[string]any{"title": "Đã xong"}, nil); w.Code != http.StatusOK {
		t.Fatalf("KTV save own order: want 200, got %d %s", w.Code, w.Body.String())
	}
	wRec := doAuthed(t, env.engine, http.MethodGet, "/api/v1/"+entity+"/"+own, ktvTok, nil, nil)
	if wRec.Code != http.StatusOK || !strings.Contains(wRec.Body.String(), up.Data.ID) {
		t.Fatalf("after save the attachment must still be on the order: %d %s", wRec.Code, wRec.Body.String())
	}
}
