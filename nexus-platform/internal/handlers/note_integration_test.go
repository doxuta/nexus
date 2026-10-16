//go:build integration

// RECORD-NOTES integration test (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 1 "Trao đổi trên hồ
// sơ"). Drives the WHOLE chain — Auth → Tenant → ACL → NoteHandler → notes.Service →
// services.RecordService.ResolveNoteTarget → sys_record_notes — over the REAL migrated schema
// (a4SetupMigratedDB, golang-migrate up through 000060), exactly like the file-upload and A5 team
// RLS integration suites this mirrors. Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Coverage (task acceptance):
//   - ACL row-scope (A5): a caller who may only view THEIR OWN records (access_level=owner) gets
//     404 — never a note — for a record another user created, on BOTH the read (GET notes) and
//     write (POST note) path. This is the exact "create:all + view:owner" shape the advisor
//     flagged: a role with broad CREATE but narrow VIEW must not be able to attach notes to a
//     record it cannot see.
//   - Delete rule: a non-author with no entity delete-grant gets 403; the note's own author always
//     succeeds; a caller holding the entity's delete-grant (e.g. a "Quản lý" role) may remove
//     anyone's note (manager override).
//   - Validation: empty body → 422 RECORD_NOTE_EMPTY; over MaxBodyRunes → 422 RECORD_NOTE_TOO_LONG.
//   - Author display name resolves via the batched user-label fetch (createdByName).
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/services"
)

// noteRouter bundles the migrated-DB-backed production router with ACL ENABLED (like a4Router)
// plus the RECORD-NOTES surface wired — a self-contained builder (mirrors newFileRouter) rather
// than extending the shared a4Router/newA4Router harness, so this suite cannot perturb the ~40
// other integration files that already depend on that harness's exact APIHandlers shape.
type noteRouter struct {
	engine *gin.Engine
	gdb    *gorm.DB
	sx     *sqlx.DB
}

func newNoteRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *noteRouter {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
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
	roleRepo := auth.NewRoleRepository(gdb)

	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)
	userSvc := services.NewUserService(userRepo)
	roleSvc := services.NewRoleService(roleRepo)
	aclSvc := services.NewACLService(roleRepo)
	teamSvc := services.NewTeamService(auth.NewTeamRepository(gdb))

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	recordSvc := services.NewRecordService(meta, dyn).WithACLAuthorizer(aclSvc)

	noteSvc := notes.NewService(notes.NewRepository(sx), recordSvc, aclSvc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Note:     NewNoteHandler(noteSvc, zap.NewNop()).WithUserLabels(dyn),
	}, zap.NewNop())

	return &noteRouter{engine: r, gdb: gdb, sx: sx}
}

func (rt *noteRouter) login(t *testing.T, tenantSlug, email string) loginResponse {
	t.Helper()
	return itLogin(t, rt.engine, tenantSlug, email, authPassword)
}

func (rt *noteRouter) createEntity(t *testing.T, adminToken, name string) {
	t.Helper()
	body := map[string]any{
		"name":  name,
		"label": name,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		},
	}
	w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/entities", adminToken, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity %s: %d %s", name, w.Code, w.Body.String())
	}
}

// noteCreateRecord posts a record as the given token and returns its id — created_by is therefore
// the REAL authenticated user, exactly like the A5 suite's postRecordAs.
func noteCreateRecord(t *testing.T, rt *noteRouter, token, entity, title string) string {
	t.Helper()
	w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity, token, map[string]any{"title": title}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST %s: got %d, want 201 (body=%s)", entity, w.Code, w.Body.String())
	}
	var env recordCreateEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode create envelope: %v (body=%s)", err, w.Body.String())
	}
	if env.Data.ID == "" {
		t.Fatalf("POST %s returned empty id (body=%s)", entity, w.Body.String())
	}
	return env.Data.ID
}

type noteListEnvelope struct {
	Data []struct {
		ID            string `json:"id"`
		Body          string `json:"body"`
		CreatedBy     string `json:"createdBy"`
		CreatedByName string `json:"createdByName"`
	} `json:"data"`
	Pagination struct {
		Total int `json:"total"`
	} `json:"pagination"`
}

type noteCreateEnvelope struct {
	Data struct {
		ID            string `json:"id"`
		Body          string `json:"body"`
		CreatedBy     string `json:"createdBy"`
		CreatedByName string `json:"createdByName"`
	} `json:"data"`
}

type noteErrEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// TestRecordNotes_M1_EndToEnd is the headline RECORD-NOTES acceptance: create/list/validate/delete,
// ACL row-scope on BOTH read and write, and the author-or-delete-grant rule, all over the real
// migrated schema + full HTTP stack.
func TestRecordNotes_M1_EndToEnd(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newNoteRouter(t, gdb, sx)

	const (
		tenantID = "n0000000-0000-0000-0000-00000000000a"
		slug     = "notes-it"
		entity   = "it_notes_demo"
	)
	a4SeedTenant(t, gdb, tenantID, slug, "Notes IT")

	// Three role shapes, mirroring the ones the advisor flagged / the task's own ACL requirements:
	//   owner        — view:owner, create:all, edit:all (NO delete)  — alice, betty
	//   viewAllNoDel — view:all,   create:all, edit:all (NO delete)  — carol
	//   manager      — view:all,   create:all, edit:all, delete:all — dave
	roleOwner := a4SeedRole(t, gdb, tenantID, "notes-owner", []auth.RoleAction{
		grantLevel(entity, "view", "owner"),
		grantLevel(entity, "create", "all"),
		grantLevel(entity, "edit", "all"),
	})
	roleViewAllNoDelete := a4SeedRole(t, gdb, tenantID, "notes-viewall-nodelete", []auth.RoleAction{
		grantLevel(entity, "view", "all"),
		grantLevel(entity, "create", "all"),
		grantLevel(entity, "edit", "all"),
	})
	roleManager := a4SeedRole(t, gdb, tenantID, "notes-manager", []auth.RoleAction{
		grantLevel(entity, "view", "all"),
		grantLevel(entity, "create", "all"),
		grantLevel(entity, "edit", "all"),
		grantLevel(entity, "delete", "all"),
	})

	a4SeedUser(t, gdb, tenantID, "admin@"+slug, true, nil)
	a4SeedUser(t, gdb, tenantID, "alice@"+slug, false, &roleOwner)
	a4SeedUser(t, gdb, tenantID, "betty@"+slug, false, &roleOwner)
	a4SeedUser(t, gdb, tenantID, "carol@"+slug, false, &roleViewAllNoDelete)
	a4SeedUser(t, gdb, tenantID, "dave@"+slug, false, &roleManager)

	t.Cleanup(func() { a4DropEntity(sx, []string{tenantID}, entity) })
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_record_notes WHERE tenant_id = ?", tenantID) })

	adminTok := rt.login(t, slug, "admin@"+slug).Data.AccessToken
	aliceTok := rt.login(t, slug, "alice@"+slug).Data.AccessToken
	bettyTok := rt.login(t, slug, "betty@"+slug).Data.AccessToken
	carolTok := rt.login(t, slug, "carol@"+slug).Data.AccessToken
	daveTok := rt.login(t, slug, "dave@"+slug).Data.AccessToken

	rt.createEntity(t, adminTok, entity)
	recA := noteCreateRecord(t, rt, aliceTok, entity, "Alice's record")

	// --- CREATE (happy path) + author name enrichment -------------------------------------------
	var noteA1 string
	t.Run("alice tạo ghi chú trên bản ghi của chính mình", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+recA+"/notes", aliceTok,
			map[string]any{"body": "đã gọi, khách hẹn tuần sau"}, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST note: want 201, got %d %s", w.Code, w.Body.String())
		}
		var env noteCreateEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		if env.Data.Body != "đã gọi, khách hẹn tuần sau" || env.Data.ID == "" {
			t.Fatalf("unexpected note shape: %+v", env.Data)
		}
		if env.Data.CreatedByName != "alice@"+slug {
			t.Errorf("createdByName = %q; want alice's display name (a4SeedUser sets FullName=email)", env.Data.CreatedByName)
		}
		noteA1 = env.Data.ID
	})

	t.Run("alice tự xem lại thấy ghi chú vừa tạo", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+entity+"/"+recA+"/notes", aliceTok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET notes: want 200, got %d %s", w.Code, w.Body.String())
		}
		var env noteListEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		if env.Pagination.Total != 1 || len(env.Data) != 1 || env.Data[0].ID != noteA1 {
			t.Fatalf("list = %+v; want exactly noteA1", env)
		}
	})

	// --- ACL ROW-SCOPE (A5): create:all + view:owner must NOT reach a record it cannot view -----
	t.Run("betty (view:owner) không xem được record của alice -> 404, không phải ghi chú rỗng", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+entity+"/"+recA+"/notes", bettyTok, nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET notes as betty: want 404, got %d %s", w.Code, w.Body.String())
		}
		var env noteErrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "RECORD_NOT_FOUND" {
			t.Errorf("code = %q; want RECORD_NOT_FOUND", env.Code)
		}
	})

	t.Run("betty (create:all nhưng view:owner) KHÔNG viết được ghi chú vào record của alice -> 404 không phải 201", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+recA+"/notes", bettyTok,
			map[string]any{"body": "betty cố ghi vào record không phải của mình"}, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST note as betty (create:all/view:owner) on alice's record: want 404, got %d %s — "+
				"a broad CREATE grant must never let a caller attach a note to a record its VIEW grant hides",
				w.Code, w.Body.String())
		}
	})

	// --- Same gate, wider grant: carol (view:all) CAN see + write ---------------------------------
	var noteA2 string
	t.Run("carol (view:all) xem được và ghi được ghi chú trên record của alice", func(t *testing.T) {
		wGet := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+entity+"/"+recA+"/notes", carolTok, nil, nil)
		if wGet.Code != http.StatusOK {
			t.Fatalf("GET notes as carol: want 200, got %d %s", wGet.Code, wGet.Body.String())
		}
		wPost := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+recA+"/notes", carolTok,
			map[string]any{"body": "carol's note"}, nil)
		if wPost.Code != http.StatusCreated {
			t.Fatalf("POST note as carol: want 201, got %d %s", wPost.Code, wPost.Body.String())
		}
		var env noteCreateEnvelope
		_ = json.Unmarshal(wPost.Body.Bytes(), &env)
		noteA2 = env.Data.ID
	})

	// --- VALIDATION ---------------------------------------------------------------------------
	t.Run("rỗng -> 422 RECORD_NOTE_EMPTY", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+recA+"/notes", aliceTok,
			map[string]any{"body": "   "}, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("empty body: want 422, got %d %s", w.Code, w.Body.String())
		}
		var env noteErrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "RECORD_NOTE_EMPTY" {
			t.Errorf("code = %q; want RECORD_NOTE_EMPTY", env.Code)
		}
	})

	t.Run("quá dài -> 422 RECORD_NOTE_TOO_LONG", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+recA+"/notes", aliceTok,
			map[string]any{"body": strings.Repeat("a", notes.MaxBodyRunes+1)}, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("over-length body: want 422, got %d %s", w.Code, w.Body.String())
		}
		var env noteErrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "RECORD_NOTE_TOO_LONG" {
			t.Errorf("code = %q; want RECORD_NOTE_TOO_LONG", env.Code)
		}
	})

	// --- DELETE: author OR entity delete-grant, everyone else 403 --------------------------------
	t.Run("carol (không phải tác giả, không có quyền delete) xoá ghi chú của alice -> 403", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+entity+"/"+recA+"/notes/"+noteA1, carolTok, nil, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("carol delete alice's note: want 403, got %d %s", w.Code, w.Body.String())
		}
		var env noteErrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "FORBIDDEN" {
			t.Errorf("code = %q; want FORBIDDEN", env.Code)
		}
	})

	t.Run("alice (tác giả) tự xoá ghi chú của mình -> 204", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+entity+"/"+recA+"/notes/"+noteA1, aliceTok, nil, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("alice delete own note: want 204, got %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("dave (Quản lý, delete:all, không phải tác giả) xoá được ghi chú của carol -> 204", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+entity+"/"+recA+"/notes/"+noteA2, daveTok, nil, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("manager override delete: want 204, got %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("sau khi xoá cả hai, danh sách rỗng", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+entity+"/"+recA+"/notes", aliceTok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET notes: want 200, got %d %s", w.Code, w.Body.String())
		}
		var env noteListEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Pagination.Total != 0 || len(env.Data) != 0 {
			t.Fatalf("list after both deletes = %+v; want empty", env)
		}
	})

	// --- Entity-level not-found: an unknown entity name never leaks past the guard ---------------
	// Uses adminTok (D3 admin bypass skips RequireACL entirely) so the request actually reaches
	// notes.Service → ResolveNoteTarget → s.resolve() and exercises ITS ErrEntityNotFound branch.
	// A non-admin caller with no ACL grant for the unknown name never even reaches the service —
	// RequireACL 403s first (the SAME 403 a normal GET /:entity/:id would give for any entity name
	// the role holds no grant for, existing or not); that is proven separately above (betty's 404
	// cases prove the SERVICE-level A5/existence branch once ACL has already let the request in).
	t.Run("entity không tồn tại (admin, bỏ qua ACL) -> 404 ENTITY_NOT_FOUND", func(t *testing.T) {
		w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/it_notes_ghost/"+recA+"/notes", adminTok, nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unknown entity: want 404, got %d %s", w.Code, w.Body.String())
		}
		var env noteErrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "ENTITY_NOT_FOUND" {
			t.Errorf("code = %q; want ENTITY_NOT_FOUND", env.Code)
		}
	})
}
