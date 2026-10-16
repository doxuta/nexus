//go:build integration

// W7 Governance pack — integration suite over a freshly-migrated throwaway MySQL DB (the A4
// harness: real `migrate up` from 0, production router via RegisterAPIRoutes, real services).
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/handlers/ -run TestW7
//
// WHAT IS PROVEN (end to end, through the production HTTP surface):
//
//  1. FIELD-ACL ADMIN CRUD (the headline): PUT /admin/field-acl hides a field for a role and
//     the A6 enforcement picks it up on the VERY NEXT record read — no restart, no cache
//     invalidation step (the enforcement loader reads MySQL per request). Full-access saves
//     DELETE the restriction row (asserted at the DB level). GET returns the matrix rows;
//     DELETE clears by filter; validation rejects contradiction / unknown names / cross-tenant
//     roles; the whole surface sits behind RequireAdmin.
//
//  2. AUDIT V2: the NULL-diff scan regression — a page containing a diff=NULL audit row used
//     to fail the whole SELECT (live "limit>10 → 500 AUDIT_QUERY_FAILED"); now it serves 200
//     with diff:null. Plus GET /audit-log/export.csv: text/csv streaming, the SAME filters as
//     the list, csvGuard on hostile cells.
//
//  3. INVITE FLOW: POST /admin/users/:id/reset-password-link mints a token through the SAME
//     forgot-password machinery, and the returned token is consumed by the PUBLIC
//     POST /auth/reset-password — after which the user logs in with the password THEY set.
//
//  4. TEAM-SETS READ API: a record created by a team member derives a team_sets row; GET
//     /admin/team-sets[/:id] serves it tenant-scoped; another tenant sees nothing / 404.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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

// w7Router is the production /api/v1 surface wired the way cmd/server/main.go wires W7: ACL
// stage enabled, A6 field-ACL enforcement on the record service, field-ACL admin CRUD, audit
// reader + CSV exporter, reset-link issuer on the user handler, team-set reads on the team
// handler.
type w7Router struct {
	engine    *gin.Engine
	gdb       *gorm.DB
	sx        *sqlx.DB
	auditRepo *audit.SQLRepository
}

func newW7Router(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *w7Router {
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
	teamRepo := auth.NewTeamRepository(gdb)
	fieldACLRepo := auth.NewFieldACLRepository(gdb)
	resetRepo := auth.NewPasswordResetTokenRepository(gdb)

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	auditRepo := audit.NewSQLRepository(sx)

	// AuthService with the reset-token repo (NO mailer — the invite flow must be fully
	// functional link-only; emailQueued=false is asserted below).
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).WithResetRepo(resetRepo)

	// A6 enforcement on the record surface — the production wiring the field-ACL admin
	// mutations must be visible to immediately.
	recordSvc := services.NewRecordService(meta, dyn).WithFieldACL(services.NewFieldACLService(fieldACLRepo))

	entitySvc := services.NewEntityService(dyn, meta)
	fieldSvc := services.NewFieldService(dyn, meta)
	userSvc := services.NewUserService(userRepo).WithTokenRevoker(refreshRepo)
	roleSvc := services.NewRoleService(roleRepo)
	teamSvc := services.NewTeamService(teamRepo)
	aclSvc := services.NewACLService(roleRepo)
	fieldACLAdminSvc := services.NewFieldACLAdminService(fieldACLRepo, roleRepo, meta)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()).WithResetLinkIssuer(authSvc),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Audit:    NewAuditHandler(auditRepo, zap.NewNop()).WithExporter(services.NewAuditExportService(auditRepo)),
		FieldACL: NewAdminFieldACLHandler(fieldACLAdminSvc, zap.NewNop()),
	}, zap.NewNop())

	return &w7Router{engine: r, gdb: gdb, sx: sx, auditRepo: auditRepo}
}

// w7CreateEntity registers a dynamic entity with a text `title` and a number `revenue`.
func w7CreateEntity(t *testing.T, rt *w7Router, adminToken, name string) {
	t.Helper()
	body := map[string]any{
		"name":  name,
		"label": name,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "revenue", "label": "Revenue", "fieldType": "number"},
		},
	}
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/entities", adminToken, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity %s: %d %s", name, w.Code, w.Body.String())
	}
}

// w7RecordData GETs one record as the given token and returns the record's data map.
func w7RecordData(t *testing.T, rt *w7Router, token, entity, id string) (map[string]any, int) {
	t.Helper()
	w := doAuthed(t, rt.engine, "GET", "/api/v1/"+entity+"/"+id, token, nil, nil)
	if w.Code != http.StatusOK {
		return nil, w.Code
	}
	var resp struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode record: %v; body=%s", err, w.Body.String())
	}
	return resp.Data.Data, w.Code
}

// --- 1. Field-ACL admin CRUD + enforcement pickup ---------------------------------------------

func TestW7FieldACLAdminLifecycle(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newW7Router(t, gdb, sx)

	const entity = "w7_deals"
	a4SeedTenant(t, gdb, "w7-tenant-a", "w7a", "W7 A")
	a4SeedTenant(t, gdb, "w7-tenant-b", "w7b", "W7 B")
	a4SeedUser(t, gdb, "w7-tenant-a", "admin@w7a.test", true, nil)
	roleA := a4SeedRole(t, gdb, "w7-tenant-a", "w7-viewer", []auth.RoleAction{grant(entity, "view"), grant(entity, "edit")})
	a4SeedUser(t, gdb, "w7-tenant-a", "user@w7a.test", false, &roleA)
	roleB := a4SeedRole(t, gdb, "w7-tenant-b", "w7-foreign", []auth.RoleAction{grant(entity, "view")})
	t.Cleanup(func() { a4DropEntity(sx, []string{"w7-tenant-a"}, entity) })

	adminTok := rt.login(t, "w7a", "admin@w7a.test")
	userTok := rt.login(t, "w7a", "user@w7a.test")

	w7CreateEntity(t, rt, adminTok, entity)

	// A record with the sensitive field populated.
	w := doAuthed(t, rt.engine, "POST", "/api/v1/"+entity, adminTok,
		map[string]any{"title": "Deal 1", "revenue": 5000}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create record: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("decode created record (%v): %s", err, w.Body.String())
	}
	recID := created.Data.ID

	// Baseline: no restriction → the role sees revenue.
	data, code := w7RecordData(t, rt, userTok, entity, recID)
	if code != http.StatusOK || data["revenue"] == nil {
		t.Fatalf("baseline read: code=%d data=%v; want revenue visible", code, data)
	}

	// PUT a hidden grant → the enforcement must pick it up on the NEXT request (no cache to
	// invalidate: the loader reads sys_field_acl per request).
	w = doAuthed(t, rt.engine, "PUT", "/api/v1/admin/field-acl", adminTok, map[string]any{
		"grants": []map[string]any{
			{"roleId": roleA, "entityName": entity, "fieldName": "revenue", "canView": false, "canEdit": false},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT field-acl: %d %s", w.Code, w.Body.String())
	}
	var putResp struct {
		Data services.FieldACLPutResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &putResp); err != nil || putResp.Data.Applied != 1 || putResp.Data.Cleared != 0 {
		t.Fatalf("PUT result = %s (err %v); want applied=1 cleared=0", w.Body.String(), err)
	}

	data, code = w7RecordData(t, rt, userTok, entity, recID)
	if code != http.StatusOK {
		t.Fatalf("read after hide: HTTP %d", code)
	}
	if _, leaked := data["revenue"]; leaked {
		t.Fatalf("revenue still visible after PUT hidden grant — enforcement did not pick up the change: %v", data)
	}
	if data["title"] == nil {
		t.Fatalf("title must stay visible; data=%v", data)
	}
	// Admin bypass is intact.
	if adminData, _ := w7RecordData(t, rt, adminTok, entity, recID); adminData["revenue"] == nil {
		t.Fatalf("admin must bypass field ACL; data=%v", adminData)
	}

	// GET matrix data (filter by entity) → exactly our row, plus the default-allow marker.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/field-acl?entity="+entity, adminTok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET field-acl: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data struct {
			DefaultPolicy string                       `json:"defaultPolicy"`
			Grants        []services.FieldACLGrantView `json:"grants"`
			DeadEnd       []string                     `json:"deadEndIfLocked"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode GET: %v; body=%s", err, w.Body.String())
	}
	// queue #306: the server's dead-end predicate rides along — `title` is required text with no
	// default (a lock would strand a create); the optional `revenue` is not.
	if len(listResp.Data.DeadEnd) != 1 || listResp.Data.DeadEnd[0] != "title" {
		t.Fatalf("deadEndIfLocked = %v; want [title] — body=%s", listResp.Data.DeadEnd, w.Body.String())
	}
	if listResp.Data.DefaultPolicy != "allow" || len(listResp.Data.Grants) != 1 ||
		listResp.Data.Grants[0].FieldName != "revenue" || listResp.Data.Grants[0].CanView {
		t.Fatalf("GET matrix = %s; want defaultPolicy=allow + one hidden revenue grant", w.Body.String())
	}

	// Full-access save DELETES the row (the no-restriction-row decision) — assert in the DB.
	w = doAuthed(t, rt.engine, "PUT", "/api/v1/admin/field-acl", adminTok, map[string]any{
		"grants": []map[string]any{
			{"roleId": roleA, "entityName": entity, "fieldName": "revenue", "canView": true, "canEdit": true},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT full-access: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &putResp); err != nil || putResp.Data.Applied != 0 || putResp.Data.Cleared != 1 {
		t.Fatalf("PUT full-access result = %s; want applied=0 cleared=1", w.Body.String())
	}
	var rowCount int
	if err := sx.Get(&rowCount, "SELECT COUNT(*) FROM sys_field_acl WHERE tenant_id = ?", "w7-tenant-a"); err != nil {
		t.Fatalf("count sys_field_acl: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("sys_field_acl rows = %d after full-access save; want 0 (row deleted, not stored)", rowCount)
	}
	if data, _ = w7RecordData(t, rt, userTok, entity, recID); data["revenue"] == nil {
		t.Fatalf("revenue must be visible again after clearing the restriction; data=%v", data)
	}

	// DELETE by filter: re-add, then clear via DELETE /admin/field-acl.
	w = doAuthed(t, rt.engine, "PUT", "/api/v1/admin/field-acl", adminTok, map[string]any{
		"grants": []map[string]any{
			{"roleId": roleA, "entityName": entity, "fieldName": "revenue", "canView": false, "canEdit": false},
		},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("re-add grant: %d %s", w.Code, w.Body.String())
	}
	w = doAuthed(t, rt.engine, "DELETE",
		"/api/v1/admin/field-acl?roleId="+roleA+"&entity="+entity+"&field=revenue", adminTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatalf("DELETE field-acl: %d %s; want deleted:1", w.Code, w.Body.String())
	}
	if data, _ = w7RecordData(t, rt, userTok, entity, recID); data["revenue"] == nil {
		t.Fatalf("revenue must be visible after DELETE; data=%v", data)
	}

	// Validation + authz corner cases.
	cases := []struct {
		name     string
		grants   []map[string]any
		wantCode string
	}{
		{"contradiction", []map[string]any{{"roleId": roleA, "entityName": entity, "fieldName": "revenue", "canView": false, "canEdit": true}}, "FIELD_ACL_CONTRADICTORY"},
		{"unknown field", []map[string]any{{"roleId": roleA, "entityName": entity, "fieldName": "salary", "canView": false, "canEdit": false}}, "FIELD_ACL_FIELD_UNKNOWN"},
		{"unknown entity", []map[string]any{{"roleId": roleA, "entityName": "ghosts", "fieldName": "revenue", "canView": false, "canEdit": false}}, "FIELD_ACL_ENTITY_NOT_FOUND"},
		{"cross-tenant role", []map[string]any{{"roleId": roleB, "entityName": entity, "fieldName": "revenue", "canView": false, "canEdit": false}}, "FIELD_ACL_ROLE_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run("reject "+tc.name, func(t *testing.T) {
			w := doAuthed(t, rt.engine, "PUT", "/api/v1/admin/field-acl", adminTok, map[string]any{"grants": tc.grants}, nil)
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("HTTP %d %s; want 422 %s", w.Code, w.Body.String(), tc.wantCode)
			}
		})
	}

	// Non-admin: the whole surface is behind RequireAdmin.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/field-acl", userTok, nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET field-acl: %d; want 403", w.Code)
	}
}

// --- 2. Audit v2: NULL-diff regression + CSV export --------------------------------------------

func TestW7AuditNullDiffAndExport(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newW7Router(t, gdb, sx)

	a4SeedTenant(t, gdb, "w7-audit-a", "w7audita", "W7 Audit A")
	a4SeedUser(t, gdb, "w7-audit-a", "admin@w7audit.test", true, nil)
	adminTok := rt.login(t, "w7audita", "admin@w7audit.test")

	// Seed a trail through the production writer: a NULL-diff row in the MIDDLE of the page
	// (the pre-fix scanner failed the whole SELECT as soon as the page included it), plus a
	// hostile entity name for the csvGuard assertion.
	events := []audit.Event{
		{TenantID: "w7-audit-a", ActorUserID: "actor-1", EntityName: "accounts", RecordID: "r-1", Action: "update",
			Diff: &audit.Diff{After: map[string]any{"x": 1}}},
		{TenantID: "w7-audit-a", ActorUserID: "actor-1", EntityName: "accounts", RecordID: "r-2", Action: "create",
			Diff: nil}, // ← the regression row (diff stored as SQL NULL)
		{TenantID: "w7-audit-a", ActorUserID: "actor-2", EntityName: `=HYPERLINK("http://evil")`, RecordID: "", Action: "create",
			Diff: &audit.Diff{After: map[string]any{"y": 2}}},
	}
	for i, evt := range events {
		if err := rt.auditRepo.InsertEvent(context.Background(), evt); err != nil {
			t.Fatalf("seed audit event %d: %v", i, err)
		}
	}

	// REGRESSION: a page that includes the NULL-diff row must serve 200 (pre-fix: 500
	// AUDIT_QUERY_FAILED because json.RawMessage cannot scan SQL NULL).
	w := doAuthed(t, rt.engine, "GET", "/api/v1/audit-log?limit=200", adminTok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit-log?limit=200 over a NULL-diff row: HTTP %d %s (the limit bug)", w.Code, w.Body.String())
	}
	var page struct {
		Data []struct {
			RecordID *string         `json:"recordId"`
			Diff     json.RawMessage `json:"diff"`
		} `json:"data"`
		Pagination struct {
			Limit int `json:"limit"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit page: %v; body=%s", err, w.Body.String())
	}
	if len(page.Data) != 3 {
		t.Fatalf("audit rows = %d; want 3 (NULL-diff row included)", len(page.Data))
	}
	if page.Pagination.Limit != 200 {
		t.Fatalf("pagination.limit = %d; want the clamped 200", page.Pagination.Limit)
	}
	nullSeen := false
	for _, row := range page.Data {
		if string(row.Diff) == "null" {
			nullSeen = true
		}
	}
	if !nullSeen {
		t.Fatalf("expected the NULL-diff row to serialize as diff:null; body=%s", w.Body.String())
	}

	// EXPORT: same filters as the list, text/csv, csvGuard on the hostile entity cell.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/audit-log/export.csv", adminTok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export.csv: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q; want text/csv", ct)
	}
	recs := parseDownloadedCSV(t, w.Body.String()) // also pins the UTF-8 BOM the download must open with
	if len(recs) != 4 {                            // header + 3 rows
		t.Fatalf("export rows = %d; want 4 (header + 3)", len(recs))
	}
	guarded := false
	for _, rec := range recs[1:] {
		if strings.HasPrefix(rec[3], "'=") {
			guarded = true
		}
		if strings.HasPrefix(rec[3], "=") {
			t.Fatalf("unguarded formula cell in export: %q", rec[3])
		}
	}
	if !guarded {
		t.Fatalf("expected the hostile entity cell to be csvGuard-prefixed; got %v", recs)
	}

	// Filters flow through: actor filter narrows to 1 row; a dateTo entirely in the past
	// (whole-day exclusive bound) excludes everything written just now.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/audit-log/export.csv?actorUserId=actor-2", adminTok, nil, nil)
	if recs = parseDownloadedCSV(t, w.Body.String()); len(recs) != 2 {
		t.Fatalf("actor-filtered export rows = %d; want 2 (header + 1)", len(recs))
	}
	w = doAuthed(t, rt.engine, "GET", "/api/v1/audit-log/export.csv?dateTo=2020-01-01", adminTok, nil, nil)
	if recs = parseDownloadedCSV(t, w.Body.String()); len(recs) != 1 {
		t.Fatalf("past-dateTo export rows = %d; want 1 (header only)", len(recs))
	}

	// Tenant isolation on the export: another tenant's admin gets an empty file.
	a4SeedTenant(t, gdb, "w7-audit-b", "w7auditb", "W7 Audit B")
	a4SeedUser(t, gdb, "w7-audit-b", "admin@w7auditb.test", true, nil)
	adminBTok := rt.login(t, "w7auditb", "admin@w7auditb.test")
	w = doAuthed(t, rt.engine, "GET", "/api/v1/audit-log/export.csv", adminBTok, nil, nil)
	if recs = parseDownloadedCSV(t, w.Body.String()); len(recs) != 1 {
		t.Fatalf("cross-tenant export rows = %d; want 1 (header only)", len(recs))
	}
}

// --- 3. Invite flow: admin-issued reset link consumed via the public reset endpoint -------------

func TestW7InviteResetLinkFlow(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newW7Router(t, gdb, sx)

	a4SeedTenant(t, gdb, "w7-invite", "w7invite", "W7 Invite")
	a4SeedUser(t, gdb, "w7-invite", "admin@w7invite.test", true, nil)
	nonAdminID := a4SeedUser(t, gdb, "w7-invite", "plain@w7invite.test", false, nil)
	adminTok := rt.login(t, "w7invite", "admin@w7invite.test")
	plainTok := rt.login(t, "w7invite", "plain@w7invite.test")

	// "Invite": create the user with a throwaway password the admin never shares…
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/users", adminTok, map[string]any{
		"email": "newbie@w7invite.test", "password": "Thr0waway-Pw!", "fullName": "New Bie",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	var createdUser struct {
		Data struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createdUser); err != nil || createdUser.Data.User.ID == "" {
		t.Fatalf("decode created user (%v): %s", err, w.Body.String())
	}

	// …then issues a set-password link (same machinery + TTL as forgot-password).
	w = doAuthed(t, rt.engine, "POST", "/api/v1/admin/users/"+createdUser.Data.User.ID+"/reset-password-link", adminTok, nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue reset link: %d %s", w.Code, w.Body.String())
	}
	var issued struct {
		Data services.ResetLinkIssue `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode issue: %v; body=%s", err, w.Body.String())
	}
	if issued.Data.Token == "" || issued.Data.TTLMinutes != 30 || issued.Data.EmailQueued {
		t.Fatalf("issue = %+v; want token set, ttlMinutes=30 (forgot-password TTL), emailQueued=false (no SMTP wired)", issued.Data)
	}

	// The user consumes the token on the PUBLIC endpoint and sets their own password.
	const newPassword = "My-0wn-Pass-42"
	w = doAuthed(t, rt.engine, "POST", "/api/v1/auth/reset-password", "", map[string]any{
		"token": issued.Data.Token, "newPassword": newPassword,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("consume reset token: %d %s", w.Code, w.Body.String())
	}
	itLogin(t, rt.engine, "w7invite", "newbie@w7invite.test", newPassword)

	// Single-use: the same token is dead now.
	w = doAuthed(t, rt.engine, "POST", "/api/v1/auth/reset-password", "", map[string]any{
		"token": issued.Data.Token, "newPassword": "Another-Pw-43",
	}, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("token must be single-use; second consume got %d %s", w.Code, w.Body.String())
	}

	// Guardrails: non-admin caller 403 (RequireAdmin); unknown target 404.
	w = doAuthed(t, rt.engine, "POST", "/api/v1/admin/users/"+nonAdminID+"/reset-password-link", plainTok, nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin issue: %d; want 403", w.Code)
	}
	w = doAuthed(t, rt.engine, "POST", "/api/v1/admin/users/no-such-user/reset-password-link", adminTok, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d %s; want 404", w.Code, w.Body.String())
	}
}

// --- 4. Team-sets read API ----------------------------------------------------------------------

func TestW7TeamSetsReadAPI(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newW7Router(t, gdb, sx)

	const entity = "w7_notes"
	a4SeedTenant(t, gdb, "w7-teams-a", "w7teamsa", "W7 Teams A")
	a4SeedTenant(t, gdb, "w7-teams-b", "w7teamsb", "W7 Teams B")
	a4SeedUser(t, gdb, "w7-teams-a", "admin@w7teams.test", true, nil)
	roleID := a4SeedRole(t, gdb, "w7-teams-a", "w7-writer", []auth.RoleAction{grant(entity, "create"), grant(entity, "view")})
	memberID := a4SeedUser(t, gdb, "w7-teams-a", "member@w7teams.test", false, &roleID)
	a4SeedUser(t, gdb, "w7-teams-b", "admin@w7teamsb.test", true, nil)
	t.Cleanup(func() { a4DropEntity(sx, []string{"w7-teams-a"}, entity) })

	adminTok := rt.login(t, "w7teamsa", "admin@w7teams.test")
	adminBTok := rt.login(t, "w7teamsb", "admin@w7teamsb.test")

	// A team with one member; the member's record write derives the team_sets row (A5 engine).
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/teams", adminTok, map[string]any{"name": "Kinh doanh"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", w.Code, w.Body.String())
	}
	var teamResp struct {
		Data struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &teamResp); err != nil || teamResp.Data.Team.ID == "" {
		t.Fatalf("decode team (%v): %s", err, w.Body.String())
	}
	teamID := teamResp.Data.Team.ID
	w = doAuthed(t, rt.engine, "POST", "/api/v1/admin/teams/"+teamID+"/members", adminTok, map[string]any{"userId": memberID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", w.Code, w.Body.String())
	}

	w7CreateEntity(t, rt, adminTok, entity)
	memberTok := rt.login(t, "w7teamsa", "member@w7teams.test")
	w = doAuthed(t, rt.engine, "POST", "/api/v1/"+entity, memberTok, map[string]any{"title": "note"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("member create record: %d %s", w.Code, w.Body.String())
	}

	// List: exactly one set, containing our team (id + name).
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/team-sets", adminTok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET team-sets: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data struct {
			TeamSets []services.TeamSetView `json:"teamSets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode team-sets: %v; body=%s", err, w.Body.String())
	}
	if len(listResp.Data.TeamSets) != 1 {
		t.Fatalf("team sets = %d; want 1 (derived by the member's record write); body=%s", len(listResp.Data.TeamSets), w.Body.String())
	}
	set := listResp.Data.TeamSets[0]
	if len(set.Teams) != 1 || set.Teams[0].ID != teamID || set.Teams[0].Name != "Kinh doanh" {
		t.Fatalf("set composition = %+v; want the single team %s (Kinh doanh)", set.Teams, teamID)
	}

	// Get by id.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/team-sets/"+set.ID, adminTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), teamID) {
		t.Fatalf("GET team-set by id: %d %s", w.Code, w.Body.String())
	}

	// Tenant isolation: tenant B sees nothing, and A's set id is a 404 for B.
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/team-sets", adminBTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"teamSets":[]`) {
		t.Fatalf("tenant B team-sets: %d %s; want empty list", w.Code, w.Body.String())
	}
	w = doAuthed(t, rt.engine, "GET", "/api/v1/admin/team-sets/"+set.ID, adminBTok, nil, nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "TEAM_SET_NOT_FOUND") {
		t.Fatalf("cross-tenant team-set get: %d %s; want 404 TEAM_SET_NOT_FOUND", w.Code, w.Body.String())
	}
}

// login mints a real token pair through the public login endpoint and returns the access token.
func (rt *w7Router) login(t *testing.T, slug, email string) string {
	t.Helper()
	return itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken
}
