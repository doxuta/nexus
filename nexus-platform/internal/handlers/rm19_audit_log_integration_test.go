//go:build integration

// RM-19 Audit Log — integration suite (../specs/029-audit-log/spec.md §6). Run with:
//
//	go test -tags=integration -race ./internal/handlers/
//
// Preconditions mirror the A4/A7 suites: a reachable MySQL 8 (the docker-compose `mysql`
// service, `make docker-up`) AND the golang-migrate CLI on PATH or at $GOPATH/bin/migrate
// (A4_MIGRATE_BIN to override). It reuses the A4 migrated-DB harness (a4SetupMigratedDB),
// which brings a THROWAWAY database up with the REAL `migrate up` from version 0; because the
// audit_log table lives in migration 000014_create_audit_log, this suite additionally asserts
// the schema reached version >= 14 (so the table came from the real migration, not AutoMigrate
// — bài học integration drift: migrate-từ-0 trên DB SẠCH).
//
// WHAT IS EXERCISED (end to end, through the production HTTP surface):
//   - record create/update/delete via the dynamic /:entity routes → RecordService emits one
//     audit_log row per committed mutation (action/diff/actor/tenant/record_id).
//   - admin entity-create + field-add + role-create → EntityService/FieldService/RoleService
//     emit one __entity__/__field__/__role__ audit row each, attributed to the caller.
//   - fail-open: a Writer that always errors leaves the business mutation succeeding (201/200).
//   - tenant isolation: tenant A's audit rows never appear under tenant B (GET /audit-log).
//
// The ONLY substitution is, in the fail-open test, the audit Writer (a failing fake). Every
// other path — router middleware chain, services, dispatcher, MySQL schema, migration — is the
// genuine production code path wired exactly as cmd/server/main.go does. No production source
// is modified for this suite.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// ----------------------------------------------------------------------------------------------
// Router + dispatcher wiring (production code paths; only the audit Writer is injectable).
// ----------------------------------------------------------------------------------------------

// auditRouter is the migrated-DB-backed production router with the RM-19 audit dispatcher wired
// into RecordService + the admin services exactly as cmd/server/main.go does (WithAuditor). The
// audit Writer is injectable so the fail-open test can supply a failing fake; for the happy-path
// tests it is the real audit.SQLRepository writing to the migrated audit_log table.
type auditRouter struct {
	engine     *gin.Engine
	gdb        *gorm.DB
	sx         *sqlx.DB
	dispatcher *audit.AsyncDispatcher
	reader     *audit.SQLRepository
}

// newAuditRouter builds the router. writer is the audit Writer under test (real repo or failing
// fake). A single worker + buffer keeps audit ordering deterministic for the assertions. The
// caller MUST drain the dispatcher (via drain()) before querying the audit table so the async
// write has completed.
func newAuditRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB, writer audit.Writer) *auditRouter {
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

	// Real audit reader over the migrated audit_log table (used by the GET /audit-log API and the
	// happy-path row assertions). The dispatcher's Writer is the injected one.
	reader := audit.NewSQLRepository(sx)
	dispatcher := audit.NewAsyncDispatcher(
		audit.DispatcherConfig{Buffer: 32, Workers: 1},
		writer, zap.NewNop(),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(ctx)
	})

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)

	// WithAuditor(dispatcher) is the exact production wiring (cmd/server/main.go).
	entitySvc := services.NewEntityService(dyn, meta).WithAuditor(dispatcher)
	fieldSvc := services.NewFieldService(dyn, meta).WithAuditor(dispatcher)
	recordSvc := services.NewRecordService(meta, dyn).WithAuditor(dispatcher)
	roleSvc := services.NewRoleService(roleRepo).WithAuditor(dispatcher)
	userSvc := services.NewUserService(userRepo).WithTokenRevoker(refreshRepo).WithAuditor(dispatcher)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)
	teamSvc := services.NewTeamService(auth.NewTeamRepository(gdb)).WithAuditor(dispatcher)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// aclChecker = nil disables ACL on the dynamic surface so the admin user's record CRUD is
	// gated only by the auth chain (is_admin) — the audit hooks are what's under test here, not ACL.
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Audit:    NewAuditHandler(reader, zap.NewNop()),
	}, zap.NewNop())

	return &auditRouter{engine: r, gdb: gdb, sx: sx, dispatcher: dispatcher, reader: reader}
}

// drain shuts the dispatcher down, blocking until every buffered audit event has been written.
// After drain the audit_log table reflects every row this test emitted, so the assertions are
// race-free. It also replaces the dispatcher with a fresh one so the router stays usable after a
// drain (a test that drains mid-way can keep mutating). For simplicity the suites here drain ONCE
// at the end, so we do not rebuild; the t.Cleanup Shutdown is idempotent.
func (rt *auditRouter) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("audit dispatcher drain did not complete: %v", err)
	}
}

// auditLogRow mirrors the audit_log columns we assert on.
type auditLogRow struct {
	ID          string          `db:"id"`
	TenantID    string          `db:"tenant_id"`
	ActorUserID string          `db:"actor_user_id"`
	EntityName  string          `db:"entity_name"`
	RecordID    *string         `db:"record_id"`
	Action      string          `db:"action"`
	Diff        json.RawMessage `db:"diff"`
}

// queryAuditRows returns every audit_log row for the given tenant (tenant-scoped — the
// multi-tenant invariant), newest-first, the test-side mirror of the production repository.
func queryAuditRows(t *testing.T, sx *sqlx.DB, tenantID string) []auditLogRow {
	t.Helper()
	var rows []auditLogRow
	if err := sx.Select(&rows,
		`SELECT id, tenant_id, actor_user_id, entity_name, record_id, action, diff
		   FROM audit_log WHERE tenant_id = ? ORDER BY at DESC, id DESC`, tenantID); err != nil {
		t.Fatalf("query audit_log (tenant=%s): %v", tenantID, err)
	}
	return rows
}

// auditRequireSchemaV14 asserts the migrated schema reached version >= 14 (audit_log migration
// applied via real migrate up) and that audit_log physically exists with the expected columns.
func auditRequireSchemaV14(t *testing.T, sx *sqlx.DB) {
	t.Helper()
	var ver struct {
		Version int  `db:"version"`
		Dirty   bool `db:"dirty"`
	}
	if err := sx.Get(&ver, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if ver.Version < 14 || ver.Dirty {
		t.Fatalf("schema_migrations = version %d dirty %v; want version >= 14 dirty false (migration 000014_create_audit_log must be applied via real migrate up)", ver.Version, ver.Dirty)
	}
	var cnt int
	if err := sx.Get(&cnt,
		`SELECT COUNT(*) FROM information_schema.columns
		   WHERE table_schema = DATABASE() AND table_name = 'audit_log'
		     AND column_name IN ('id','tenant_id','actor_user_id','entity_name','record_id','action','diff','at')`); err != nil {
		t.Fatalf("inspect audit_log columns: %v", err)
	}
	if cnt != 8 {
		t.Fatalf("audit_log is missing expected columns (found %d of 8) — migration 000014 schema drift", cnt)
	}
	// The 3 indexes from spec §2 must exist.
	var idx int
	if err := sx.Get(&idx,
		`SELECT COUNT(DISTINCT index_name) FROM information_schema.statistics
		   WHERE table_schema = DATABASE() AND table_name = 'audit_log'
		     AND index_name IN ('idx_al_tenant_at','idx_al_record','idx_al_actor')`); err != nil {
		t.Fatalf("inspect audit_log indexes: %v", err)
	}
	if idx != 3 {
		t.Fatalf("audit_log is missing expected indexes (found %d of 3) — migration 000014 index drift", idx)
	}
}

// failingAuditWriter is an audit.Writer that always errors — proving the fail-open contract: a
// committed business mutation still returns success even when the audit write fails.
type failingAuditWriter struct{ calls int64 }

func (w *failingAuditWriter) InsertEvent(context.Context, audit.Event) error {
	atomic.AddInt64(&w.calls, 1)
	return errors.New("simulated audit_log write failure")
}

// auditSeedAdmin seeds a tenant + an active admin user so a real login mints an admin token.
func auditSeedAdmin(t *testing.T, gdb *gorm.DB, tenantID, slug, email string) (userID string) {
	t.Helper()
	a4SeedTenant(t, gdb, tenantID, slug, slug)
	return a4SeedUser(t, gdb, tenantID, email, true, nil)
}

// createEntity POSTs /admin/entities with one text field 'title'. Returns the entity name.
func (rt *auditRouter) createEntity(t *testing.T, token, name string) {
	t.Helper()
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/entities", token, map[string]any{
		"name":  name,
		"label": "Audit " + name,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		},
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("createEntity %s: HTTP %d; body=%s", name, w.Code, w.Body.String())
	}
}

// createRecord POSTs /:entity with the given data and returns the new record id (HTTP 201).
func (rt *auditRouter) createRecord(t *testing.T, token, entity string, data map[string]any) string {
	t.Helper()
	w := doAuthed(t, rt.engine, "POST", "/api/v1/"+entity, token, data, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("createRecord %s: HTTP %d; body=%s", entity, w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode createRecord response: %v; body=%s", err, w.Body.String())
	}
	if env.Data.ID == "" {
		t.Fatalf("createRecord returned empty id; body=%s", w.Body.String())
	}
	return env.Data.ID
}

// ==============================================================================================
// Tests
// ==============================================================================================

// TestRM19_RecordMutations_WriteOneAuditRowEach is the headline §6 case: create → update →
// delete of a dynamic record each writes EXACTLY ONE audit_log row with the right action, actor,
// tenant, record_id and a {before,after} diff of only the changed fields.
func TestRM19_RecordMutations_WriteOneAuditRowEach(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	rt := newAuditRouter(t, gdb, sx, audit.NewSQLRepository(sx))

	const (
		tenantID = "a1900000-0000-0000-0000-0000000000a1"
		slug     = "rm19-acme"
		email    = "admin@rm19-acme.test"
		entity   = "rm19_accounts"
	)
	adminID := auditSeedAdmin(t, gdb, tenantID, slug, email)
	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	// Seed the dynamic entity via the admin API (this also writes an __entity__ audit row).
	rt.createEntity(t, token, entity)

	// CREATE record.
	recID := rt.createRecord(t, token, entity, map[string]any{"title": "Initial"})

	// UPDATE record (change title).
	if w := doAuthed(t, rt.engine, "PUT", "/api/v1/"+entity+"/"+recID, token, map[string]any{"title": "Renamed"}, nil); w.Code != http.StatusOK {
		t.Fatalf("update record: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	// DELETE record (soft delete).
	if w := doAuthed(t, rt.engine, "DELETE", "/api/v1/"+entity+"/"+recID, token, nil, nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete record: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	rt.drain(t)

	rows := queryAuditRows(t, sx, tenantID)

	// Filter to this entity's record mutations (exclude the __entity__ create row).
	var create, update, del *auditLogRow
	for i := range rows {
		r := &rows[i]
		if r.EntityName != entity {
			continue
		}
		switch r.Action {
		case audit.ActionCreate:
			create = r
		case audit.ActionUpdate:
			update = r
		case audit.ActionDelete:
			del = r
		}
	}
	if create == nil || update == nil || del == nil {
		t.Fatalf("record audit rows: create=%v update=%v delete=%v (rows=%+v)", create != nil, update != nil, del != nil, rows)
	}

	// Each must carry the right actor, tenant and record_id.
	for _, r := range []*auditLogRow{create, update, del} {
		if r.ActorUserID != adminID {
			t.Errorf("%s row actor=%q; want %q", r.Action, r.ActorUserID, adminID)
		}
		if r.TenantID != tenantID {
			t.Errorf("%s row tenant=%q; want %q", r.Action, r.TenantID, tenantID)
		}
		if r.RecordID == nil || *r.RecordID != recID {
			t.Errorf("%s row record_id=%v; want %q", r.Action, r.RecordID, recID)
		}
	}

	// Diff shape: create = after-only, update = before+after of changed key, delete = before-only.
	createDiff := decodeDiff(t, create.Diff)
	if createDiff.Before != nil || createDiff.After["title"] != "Initial" {
		t.Errorf("create diff = %+v; want after-only {title:Initial}", createDiff)
	}
	updateDiff := decodeDiff(t, update.Diff)
	if updateDiff.Before["title"] != "Initial" || updateDiff.After["title"] != "Renamed" {
		t.Errorf("update diff = %+v; want before{title:Initial} after{title:Renamed}", updateDiff)
	}
	deleteDiff := decodeDiff(t, del.Diff)
	if deleteDiff.After != nil || deleteDiff.Before["title"] != "Renamed" {
		t.Errorf("delete diff = %+v; want before-only {title:Renamed}", deleteDiff)
	}

	// Exactly one row per action for this record (no duplicates).
	var c, u, d int
	for _, r := range rows {
		if r.EntityName != entity || r.RecordID == nil || *r.RecordID != recID {
			continue
		}
		switch r.Action {
		case audit.ActionCreate:
			c++
		case audit.ActionUpdate:
			u++
		case audit.ActionDelete:
			d++
		}
	}
	if c != 1 || u != 1 || d != 1 {
		t.Fatalf("audit row counts create=%d update=%d delete=%d; want exactly 1 each", c, u, d)
	}
}

// TestRM19_AdminMetaMutations_WriteAuditRows proves entity-create, field-add and role-create each
// write one admin-meta audit row (__entity__/__field__/__role__) attributed to the caller, with
// NULL record_id (admin-meta mutations have no obj_* record) — except the role's, which carries the
// role id (issue #23), and a role update, whose diff names the role even when only grants changed.
func TestRM19_AdminMetaMutations_WriteAuditRows(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	rt := newAuditRouter(t, gdb, sx, audit.NewSQLRepository(sx))

	const (
		tenantID = "a1900000-0000-0000-0000-0000000000b2"
		slug     = "rm19-meta"
		email    = "admin@rm19-meta.test"
		entity   = "rm19_meta_widgets"
	)
	adminID := auditSeedAdmin(t, gdb, tenantID, slug, email)
	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	// Entity create → __entity__ row.
	rt.createEntity(t, token, entity)

	// Field add → __field__ row.
	if w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/fields", token, map[string]any{
		"entityName": entity, "name": "amount", "label": "Amount", "fieldType": "number",
	}, nil); w.Code != http.StatusCreated {
		t.Fatalf("add field: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	// Role create → __role__ row.
	var role struct {
		Data struct {
			Role struct {
				ID string `json:"id"`
			} `json:"role"`
		} `json:"data"`
	}
	if w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/roles", token, map[string]any{
		"name": "rm19_viewer", "label": "RM19 Viewer",
		"actions": []map[string]any{{"entityName": entity, "action": "view", "accessLevel": "all"}},
	}, nil); w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &role) != nil || role.Data.Role.ID == "" {
		t.Fatalf("create role: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	// Grants-only update, as the AI's access card applies it → a __role__ update row naming the role.
	if w := doAuthed(t, rt.engine, "PUT", "/api/v1/admin/roles/"+role.Data.Role.ID, token, map[string]any{
		"name": "rm19_viewer", "label": "RM19 Viewer",
		"actions": []map[string]any{{"entityName": entity, "action": "view", "accessLevel": "all"}, {"entityName": entity, "action": "edit", "accessLevel": "owner"}},
	}, nil); w.Code != http.StatusOK {
		t.Fatalf("update role: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	rt.drain(t)

	rows := queryAuditRows(t, sx, tenantID)
	seen := map[string]*auditLogRow{}
	for i := range rows {
		r := &rows[i]
		switch r.EntityName {
		case audit.EntityMeta, audit.FieldMeta, audit.RoleMeta:
			if r.Action == audit.ActionCreate {
				seen[r.EntityName] = r
			}
		}
	}
	for _, key := range []string{audit.EntityMeta, audit.FieldMeta, audit.RoleMeta} {
		r := seen[key]
		if r == nil {
			t.Fatalf("missing create audit row for %s (rows=%+v)", key, rows)
		}
		if r.ActorUserID != adminID {
			t.Errorf("%s row actor=%q; want %q", key, r.ActorUserID, adminID)
		}
		if r.TenantID != tenantID {
			t.Errorf("%s row tenant=%q; want %q", key, r.TenantID, tenantID)
		}
		if key == audit.RoleMeta {
			if r.RecordID == nil || *r.RecordID != role.Data.Role.ID {
				t.Errorf("%s row record_id=%v; want the role id %q", key, r.RecordID, role.Data.Role.ID)
			}
			continue
		}
		if r.RecordID != nil {
			t.Errorf("%s row record_id=%v; want NULL (admin-meta)", key, *r.RecordID)
		}
	}
	var upd *auditLogRow
	for i := range rows {
		if rows[i].EntityName == audit.RoleMeta && rows[i].Action == audit.ActionUpdate {
			upd = &rows[i]
		}
	}
	var diff audit.Diff
	if upd == nil || upd.RecordID == nil || *upd.RecordID != role.Data.Role.ID || json.Unmarshal(upd.Diff, &diff) != nil ||
		diff.Before["name"] != "rm19_viewer" || diff.After["name"] != "rm19_viewer" {
		t.Fatalf("role update row = %+v; want record_id %q and the role's name on both sides of its diff", upd, role.Data.Role.ID)
	}
}

// TestRM19_TeamMetaMutations_WriteAuditRows closes the __team__ zero-emit gap
// (../specs/029-audit-log/spec.md "Nợ còn mở"): team create / member add / member remove /
// rename / delete each write one __team__ audit row attributed to the caller, with NULL
// record_id and the right one-sided or before/after diff. Membership changes are modelled as
// ActionUpdate on the team: add = after-only {teamId,userId,member:true}, remove = the mirror
// before-only diff.
func TestRM19_TeamMetaMutations_WriteAuditRows(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	rt := newAuditRouter(t, gdb, sx, audit.NewSQLRepository(sx))

	const (
		tenantID = "a1900000-0000-0000-0000-0000000000d4"
		slug     = "rm19-team"
		email    = "admin@rm19-team.test"
	)
	adminID := auditSeedAdmin(t, gdb, tenantID, slug, email)
	memberID := a4SeedUser(t, gdb, tenantID, "member@rm19-team.test", false, nil)
	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	// CREATE team.
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/teams", token, map[string]any{"name": "RM19 Team"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create team: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	var teamEnv struct {
		Data struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &teamEnv); err != nil {
		t.Fatalf("decode create team response: %v; body=%s", err, w.Body.String())
	}
	teamID := teamEnv.Data.Team.ID
	if teamID == "" {
		t.Fatalf("create team returned empty id; body=%s", w.Body.String())
	}

	// ADD member, then REMOVE member.
	if w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/teams/"+teamID+"/members", token, map[string]any{"userId": memberID}, nil); w.Code != http.StatusOK {
		t.Fatalf("add member: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, "DELETE", "/api/v1/admin/teams/"+teamID+"/members/"+memberID, token, nil, nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("remove member: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	// RENAME team, then DELETE it.
	if w := doAuthed(t, rt.engine, "PUT", "/api/v1/admin/teams/"+teamID, token, map[string]any{"name": "RM19 Team v2"}, nil); w.Code != http.StatusOK {
		t.Fatalf("rename team: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, "DELETE", "/api/v1/admin/teams/"+teamID, token, nil, nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete team: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	rt.drain(t)

	var teamRows []auditLogRow
	for _, r := range queryAuditRows(t, sx, tenantID) {
		if r.EntityName == audit.TeamMeta {
			teamRows = append(teamRows, r)
		}
	}

	// Every __team__ row carries the right actor, tenant and a NULL record_id (admin-meta).
	for _, r := range teamRows {
		if r.ActorUserID != adminID {
			t.Errorf("__team__ %s row actor=%q; want %q", r.Action, r.ActorUserID, adminID)
		}
		if r.TenantID != tenantID {
			t.Errorf("__team__ %s row tenant=%q; want %q", r.Action, r.TenantID, tenantID)
		}
		if r.RecordID != nil {
			t.Errorf("__team__ %s row record_id=%v; want NULL (admin-meta)", r.Action, *r.RecordID)
		}
	}

	// Exactly one create + one delete + three updates (member add, member remove, rename).
	var creates, deletes []auditLogRow
	var memberAdd, memberRemove, rename *auditLogRow
	for i := range teamRows {
		r := &teamRows[i]
		switch r.Action {
		case audit.ActionCreate:
			creates = append(creates, *r)
		case audit.ActionDelete:
			deletes = append(deletes, *r)
		case audit.ActionUpdate:
			d := decodeDiff(t, r.Diff)
			switch {
			case d.Before == nil && d.After["member"] == true:
				memberAdd = r
			case d.After == nil && d.Before["member"] == true:
				memberRemove = r
			case d.Before["name"] != nil || d.After["name"] != nil:
				rename = r
			}
		}
	}
	if len(creates) != 1 || len(deletes) != 1 {
		t.Fatalf("__team__ create/delete row counts = %d/%d; want 1/1 (rows=%+v)", len(creates), len(deletes), teamRows)
	}

	// create = after-only diff carrying the team name.
	createDiff := decodeDiff(t, creates[0].Diff)
	if createDiff.Before != nil || createDiff.After["name"] != "RM19 Team" {
		t.Errorf("create diff = %+v; want after-only {name:RM19 Team}", createDiff)
	}
	// member add = after-only {teamId,userId,member:true}; remove = the before-only mirror.
	if memberAdd == nil {
		t.Fatalf("missing member-add __team__ update row (rows=%+v)", teamRows)
	}
	addDiff := decodeDiff(t, memberAdd.Diff)
	if addDiff.After["teamId"] != teamID || addDiff.After["userId"] != memberID {
		t.Errorf("member-add diff = %+v; want after {teamId:%s userId:%s member:true}", addDiff, teamID, memberID)
	}
	if memberRemove == nil {
		t.Fatalf("missing member-remove __team__ update row (rows=%+v)", teamRows)
	}
	removeDiff := decodeDiff(t, memberRemove.Diff)
	if removeDiff.Before["teamId"] != teamID || removeDiff.Before["userId"] != memberID {
		t.Errorf("member-remove diff = %+v; want before {teamId:%s userId:%s member:true}", removeDiff, teamID, memberID)
	}
	// rename = before/after of only the changed name.
	if rename == nil {
		t.Fatalf("missing rename __team__ update row (rows=%+v)", teamRows)
	}
	renameDiff := decodeDiff(t, rename.Diff)
	if renameDiff.Before["name"] != "RM19 Team" || renameDiff.After["name"] != "RM19 Team v2" {
		t.Errorf("rename diff = %+v; want before{name:RM19 Team} after{name:RM19 Team v2}", renameDiff)
	}
	// delete = before-only diff carrying the final name.
	deleteDiff := decodeDiff(t, deletes[0].Diff)
	if deleteDiff.After != nil || deleteDiff.Before["name"] != "RM19 Team v2" {
		t.Errorf("delete diff = %+v; want before-only {name:RM19 Team v2}", deleteDiff)
	}
}

// TestRM19_UserMetaMutations_WriteAuditRows closes the __user__ zero-emit gap on the admin user
// CRUD (../specs/029-audit-log/spec.md §4): user create / rename / role change / disable /
// password change / delete each write one __user__ audit row attributed to the caller, with
// NULL record_id and the right one-sided or before/after diff. A password change MUST be
// visible in the trail WITHOUT any secret: the after side carries only the marker
// {password:"[redacted]"} — no plaintext and no bcrypt digest anywhere in any __user__ diff.
func TestRM19_UserMetaMutations_WriteAuditRows(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	rt := newAuditRouter(t, gdb, sx, audit.NewSQLRepository(sx))

	const (
		tenantID    = "a1900000-0000-0000-0000-0000000000e5"
		slug        = "rm19-user"
		email       = "admin@rm19-user.test"
		memberEmail = "member@rm19-user.test"
		newPass     = "Rotated-Pw-9!"
	)
	adminID := auditSeedAdmin(t, gdb, tenantID, slug, email)
	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	// A real in-tenant role for the role-change update (users.role_id is FK-backed, fk_users_role).
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/roles", token, map[string]any{
		"name": "rm19_user_role", "label": "RM19 User Role",
		"actions": []map[string]any{{"entityName": "accounts", "action": "view", "accessLevel": "all"}},
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create role: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	var roleEnv roleEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &roleEnv); err != nil {
		t.Fatalf("decode create role response: %v; body=%s", err, w.Body.String())
	}
	roleID := roleEnv.Data.Role.ID
	if roleID == "" {
		t.Fatalf("create role returned empty id; body=%s", w.Body.String())
	}

	// CREATE user.
	w = doAuthed(t, rt.engine, "POST", "/api/v1/admin/users", token, map[string]any{
		"email": memberEmail, "password": authPassword, "fullName": "RM19 Member",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	var userEnv struct {
		Data struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &userEnv); err != nil {
		t.Fatalf("decode create user response: %v; body=%s", err, w.Body.String())
	}
	userID := userEnv.Data.User.ID
	if userID == "" {
		t.Fatalf("create user returned empty id; body=%s", w.Body.String())
	}

	// UPDATE ×4 — one field per call so each row's diff is unambiguous (rename, role, disable,
	// password) — then DELETE.
	for _, step := range []struct {
		name string
		body map[string]any
	}{
		{"rename", map[string]any{"fullName": "RM19 Member v2"}},
		{"role", map[string]any{"roleId": roleID}},
		{"disable", map[string]any{"status": "disabled"}},
		{"password", map[string]any{"password": newPass}},
	} {
		if w := doAuthed(t, rt.engine, "PUT", "/api/v1/admin/users/"+userID, token, step.body, nil); w.Code != http.StatusOK {
			t.Fatalf("update user (%s): HTTP %d; body=%s", step.name, w.Code, w.Body.String())
		}
	}
	if w := doAuthed(t, rt.engine, "DELETE", "/api/v1/admin/users/"+userID, token, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete user: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	rt.drain(t)

	var userRows []auditLogRow
	for _, r := range queryAuditRows(t, sx, tenantID) {
		if r.EntityName == audit.UserMeta {
			userRows = append(userRows, r)
		}
	}

	// Every __user__ row: right actor + tenant, record_id = the TARGET user (same convention as
	// the W7 reset-link row, so GET /audit-log?entityName=__user__&recordId=<id> returns the
	// whole account history), and NO password material — neither plaintext (initial + rotated)
	// nor a bcrypt digest nor a hash key.
	for _, r := range userRows {
		if r.ActorUserID != adminID {
			t.Errorf("__user__ %s row actor=%q; want %q", r.Action, r.ActorUserID, adminID)
		}
		if r.TenantID != tenantID {
			t.Errorf("__user__ %s row tenant=%q; want %q", r.Action, r.TenantID, tenantID)
		}
		if r.RecordID == nil || *r.RecordID != userID {
			t.Errorf("__user__ %s row record_id=%v; want target user id %q", r.Action, r.RecordID, userID)
		}
		raw := string(r.Diff)
		for _, secret := range []string{authPassword, newPass, "$2a$", "$2b$", "$2y$", "passwordHash", "password_hash"} {
			if strings.Contains(raw, secret) {
				t.Errorf("__user__ %s row diff carries password material %q: %s", r.Action, secret, raw)
			}
		}
	}

	// Exactly one create + one delete + four updates (rename, role, disable, password).
	var creates, deletes []auditLogRow
	var rename, role, disable, password *auditLogRow
	for i := range userRows {
		r := &userRows[i]
		switch r.Action {
		case audit.ActionCreate:
			creates = append(creates, *r)
		case audit.ActionDelete:
			deletes = append(deletes, *r)
		case audit.ActionUpdate:
			d := decodeDiff(t, r.Diff)
			switch {
			case d.After["fullName"] != nil:
				rename = r
			case d.After["roleId"] != nil:
				role = r
			case d.After["status"] != nil:
				disable = r
			case d.After["password"] != nil:
				password = r
			}
		}
	}
	if len(creates) != 1 || len(deletes) != 1 {
		t.Fatalf("__user__ create/delete row counts = %d/%d; want 1/1 (rows=%+v)", len(creates), len(deletes), userRows)
	}

	// create = after-only safe view (email, fullName, status active, isAdmin false), no password key.
	createDiff := decodeDiff(t, creates[0].Diff)
	if createDiff.Before != nil || createDiff.After["email"] != memberEmail || createDiff.After["fullName"] != "RM19 Member" ||
		createDiff.After["status"] != "active" || createDiff.After["isAdmin"] != false {
		t.Errorf("create diff = %+v; want after-only safe view of %s", createDiff, memberEmail)
	}
	if _, ok := createDiff.After["password"]; ok {
		t.Errorf("create diff carries a password key: %+v", createDiff)
	}
	// rename = before/after of only fullName.
	if rename == nil {
		t.Fatalf("missing rename __user__ update row (rows=%+v)", userRows)
	}
	renameDiff := decodeDiff(t, rename.Diff)
	if renameDiff.Before["fullName"] != "RM19 Member" || renameDiff.After["fullName"] != "RM19 Member v2" || len(renameDiff.After) != 1 {
		t.Errorf("rename diff = %+v; want before{fullName:RM19 Member} after{fullName:RM19 Member v2}", renameDiff)
	}
	// role = nil → roleID: an after-only {roleId} (the key did not exist on the before side).
	if role == nil {
		t.Fatalf("missing role-change __user__ update row (rows=%+v)", userRows)
	}
	roleDiff := decodeDiff(t, role.Diff)
	if roleDiff.Before != nil || roleDiff.After["roleId"] != roleID || len(roleDiff.After) != 1 {
		t.Errorf("role diff = %+v; want after-only {roleId:%s}", roleDiff, roleID)
	}
	// disable = before{status:active} after{status:disabled}.
	if disable == nil {
		t.Fatalf("missing disable __user__ update row (rows=%+v)", userRows)
	}
	disableDiff := decodeDiff(t, disable.Diff)
	if disableDiff.Before["status"] != "active" || disableDiff.After["status"] != "disabled" || len(disableDiff.After) != 1 {
		t.Errorf("disable diff = %+v; want before{status:active} after{status:disabled}", disableDiff)
	}
	// password = after-only {password:"[redacted]"} — the change is visible, the secret is not.
	if password == nil {
		t.Fatalf("missing password-change __user__ update row (rows=%+v)", userRows)
	}
	passwordDiff := decodeDiff(t, password.Diff)
	if passwordDiff.Before != nil || passwordDiff.After["password"] != "[redacted]" || len(passwordDiff.After) != 1 {
		t.Errorf("password diff = %+v; want after-only {password:[redacted]}", passwordDiff)
	}
	// delete = before-only diff carrying the final safe view.
	deleteDiff := decodeDiff(t, deletes[0].Diff)
	if deleteDiff.After != nil || deleteDiff.Before["email"] != memberEmail || deleteDiff.Before["fullName"] != "RM19 Member v2" ||
		deleteDiff.Before["status"] != "disabled" || deleteDiff.Before["roleId"] != roleID {
		t.Errorf("delete diff = %+v; want before-only final safe view of %s", deleteDiff, memberEmail)
	}
}

// TestRM19_FailOpen_BusinessSucceedsWhenAuditWriteFails is the THEN-CHỐT contract: with an audit
// Writer that always errors, the business mutations (entity create + record create/update/delete)
// must STILL succeed (201/200) — the audit failure is swallowed inside the dispatcher and never
// surfaces as a 500.
func TestRM19_FailOpen_BusinessSucceedsWhenAuditWriteFails(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	failer := &failingAuditWriter{}
	rt := newAuditRouter(t, gdb, sx, failer)

	const (
		tenantID = "a1900000-0000-0000-0000-0000000000c3"
		slug     = "rm19-failopen"
		email    = "admin@rm19-failopen.test"
		entity   = "rm19_failopen"
	)
	auditSeedAdmin(t, gdb, tenantID, slug, email)
	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	// Entity create must succeed despite the audit writer erroring.
	rt.createEntity(t, token, entity)

	// Record create/update/delete must all succeed too.
	recID := rt.createRecord(t, token, entity, map[string]any{"title": "FO"})
	if w := doAuthed(t, rt.engine, "PUT", "/api/v1/"+entity+"/"+recID, token, map[string]any{"title": "FO2"}, nil); w.Code != http.StatusOK {
		t.Fatalf("fail-open update: HTTP %d; body=%s (audit failure must NOT break the flow)", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, "DELETE", "/api/v1/"+entity+"/"+recID, token, nil, nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("fail-open delete: HTTP %d; body=%s", w.Code, w.Body.String())
	}

	rt.drain(t)

	// The failing writer must have actually been invoked (so the fail-open path was exercised),
	// and NO audit rows landed (every write errored).
	if atomic.LoadInt64(&failer.calls) == 0 {
		t.Fatalf("failing audit writer was never called — fail-open path not exercised")
	}
	rows := queryAuditRows(t, sx, tenantID)
	if len(rows) != 0 {
		t.Fatalf("audit_log has %d rows despite a failing writer; want 0 (writes errored)\nrows=%+v", len(rows), rows)
	}
}

// TestRM19_TenantIsolation_AuditAPI proves the multi-tenant invariant: a record mutation in
// tenant A NEVER appears in tenant B's GET /api/v1/audit-log, and vice versa.
func TestRM19_TenantIsolation_AuditAPI(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	auditRequireSchemaV14(t, sx)

	rt := newAuditRouter(t, gdb, sx, audit.NewSQLRepository(sx))

	const (
		tenantA = "a1900000-0000-0000-0000-0000000000aa"
		slugA   = "rm19-ta"
		emailA  = "admin@rm19-ta.test"

		tenantB = "a1900000-0000-0000-0000-0000000000bb"
		slugB   = "rm19-tb"
		emailB  = "admin@rm19-tb.test"

		entity = "rm19_iso"
	)
	auditSeedAdmin(t, gdb, tenantA, slugA, emailA)
	auditSeedAdmin(t, gdb, tenantB, slugB, emailB)
	tokenA := itLogin(t, rt.engine, slugA, emailA, authPassword).Data.AccessToken
	tokenB := itLogin(t, rt.engine, slugB, emailB, authPassword).Data.AccessToken

	// Each tenant creates its own entity (same name, different tenant) + one record.
	rt.createEntity(t, tokenA, entity)
	rt.createEntity(t, tokenB, entity)
	recA := rt.createRecord(t, tokenA, entity, map[string]any{"title": "A-rec"})
	recB := rt.createRecord(t, tokenB, entity, map[string]any{"title": "B-rec"})

	rt.drain(t)

	// GET /audit-log as tenant A must return ONLY tenant A's rows (and never B's record id).
	listA := rt.listAudit(t, tokenA)
	for _, r := range listA {
		if r.RecordID != nil && *r.RecordID == recB {
			t.Errorf("tenant A audit API leaked tenant B record %q: %+v", recB, r)
		}
	}
	listB := rt.listAudit(t, tokenB)
	for _, r := range listB {
		if r.RecordID != nil && *r.RecordID == recA {
			t.Errorf("tenant B audit API leaked tenant A record %q: %+v", recA, r)
		}
	}
	// Sanity: each tenant DOES see its own record-create row via the API.
	if !containsRecord(listA, recA) {
		t.Errorf("tenant A audit API missing its own record %q", recA)
	}
	if !containsRecord(listB, recB) {
		t.Errorf("tenant B audit API missing its own record %q", recB)
	}
}

// listAudit GETs /api/v1/audit-log as the given token and returns the decoded data rows.
func (rt *auditRouter) listAudit(t *testing.T, token string) []audit.AuditRow {
	t.Helper()
	w := doAuthed(t, rt.engine, "GET", "/api/v1/audit-log?limit=200", token, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit-log: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Data []audit.AuditRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode /audit-log response: %v; body=%s", err, w.Body.String())
	}
	return env.Data
}

func containsRecord(rows []audit.AuditRow, recID string) bool {
	for _, r := range rows {
		if r.RecordID != nil && *r.RecordID == recID {
			return true
		}
	}
	return false
}

// decodeDiff unmarshals an audit_log.diff JSON column into an audit.Diff for assertions.
func decodeDiff(t *testing.T, raw json.RawMessage) audit.Diff {
	t.Helper()
	var d audit.Diff
	if len(raw) == 0 || string(raw) == "null" {
		return d
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decode diff JSON %q: %v", string(raw), err)
	}
	return d
}
