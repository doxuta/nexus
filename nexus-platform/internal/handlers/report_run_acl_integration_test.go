//go:build integration

// REPORT-RUN-ACL integration test — proves the report RUN + CSV-export surface enforces the report
// ENTITY's "view" ACL for a caller who is NOT the /:entity RequireACL middleware's subject (the
// report's entity is stored on the record, not the URL, so RequireACL cannot apply). The report
// service resolves it itself via RecordService.AuthorizeView (mirroring the ⌘K assistant), so:
//   - a non-admin WITH an "owner"-level view grant sees ONLY the rows it created (NOT 0, NOT all);
//   - a non-admin WITH an "all"-level view grant sees every tenant row;
//   - a non-admin WITHOUT any grant is refused 403 FORBIDDEN before any data query (and before the
//     first CSV byte on export);
//   - an admin sees every row.
//
// This drives the genuine Auth → Tenant → report-service-ACL → aggregate/list chain against a REAL
// migrated MySQL 8 (the a4 golang-migrate harness), so the SQL row-scoping is what the assertions
// observe — a unit test cannot see it. Run with: go test -tags=integration ./internal/handlers/
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
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

const (
	repACLTenantID = "reac1000-0000-0000-0000-0000000000a1"
	repACLSlug     = "reac-acme"
	repACLEntity   = "it_repacl_orders"
)

// newReportACLRouter mirrors newA4Router's production wiring but ADDS the report RUN/export surface
// over a RecordService with WithACLAuthorizer wired — so the report path resolves the report
// entity's "view" ACL itself (REPORT-RUN-ACL). A real ACLService is also the RegisterAPIRoutes
// aclChecker, so /:entity record creation and the report run/export all authorize through the same
// tenant-pinned grant matrix a production login would carry.
func newReportACLRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *gin.Engine {
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
	aclSvc := services.NewACLService(roleRepo)

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	// The record service carries the ACL authorizer so AuthorizeView (the report entity "view"
	// stage) can resolve the caller's row-level access level — exactly what production wires. The
	// real field-ACL enforcer too (cmd/server/main.go wires it the same way), so a hidden report
	// axis is refused by the genuine sys_field_acl grants (specs/049 report_grid_integration_test).
	recordSvc := services.NewRecordService(meta, dyn).WithACLAuthorizer(aclSvc).
		WithFieldACL(services.NewFieldACLService(auth.NewFieldACLRepository(gdb))).
		// The label fetcher main.go wires: a report's relation column / axis names the record
		// through it (REPORT-RELATION-LABELS, specs/049 S6 axis labels).
		WithLabelFetcher(dyn).
		// The relationship resolver main.go wires: a joined report axis ("<rel>.<field>") resolves
		// the relationship through it (#240 joined-axis integration).
		WithRollupMeta(meta)
	reportSvc := services.NewReportService(report.NewSQLRepository(sx), recordSvc).WithEntityChecker(meta)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:   NewAuthHandler(authSvc, zap.NewNop()),
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Report: NewAdminReportHandler(reportSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// viewGrant builds an acl_role_actions "view" grant on repACLEntity at a specific access_level
// (owner/team/all) — a4SeedRole's grant() helper always defaults to "all", but this test needs an
// OWNER-scoped grant to make row-scoping observable.
func viewGrant(level string) auth.RoleAction {
	return auth.RoleAction{EntityName: repACLEntity, Action: "view", AccessLevel: level}
}

func TestReportRunACL_RowScopedAndDenied(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newReportACLRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, repACLTenantID, repACLSlug, "Acme ReportACL")

	// Roles: owner (view@owner + create@all so it can seed its OWN rows), viewer-all (view@all),
	// viewer-none (no grant at all).
	ownerRole := a4SeedRole(t, gdb, repACLTenantID, "rep-owner", []auth.RoleAction{
		viewGrant("owner"), grant(repACLEntity, "create"),
	})
	allRole := a4SeedRole(t, gdb, repACLTenantID, "rep-viewall", []auth.RoleAction{viewGrant("all")})
	noneRole := a4SeedRole(t, gdb, repACLTenantID, "rep-none", nil)

	a4SeedUser(t, gdb, repACLTenantID, "admin@"+repACLSlug, true, nil)
	a4SeedUser(t, gdb, repACLTenantID, "owner@"+repACLSlug, false, &ownerRole)
	a4SeedUser(t, gdb, repACLTenantID, "viewall@"+repACLSlug, false, &allRole)
	a4SeedUser(t, gdb, repACLTenantID, "viewnone@"+repACLSlug, false, &noneRole)

	a4DropEntity(sx, []string{repACLTenantID}, repACLEntity)
	t.Cleanup(func() {
		sx.Exec(`DELETE FROM sys_reports WHERE tenant_id = ? AND entity_name = ?`, repACLTenantID, repACLEntity)
		a4DropEntity(sx, []string{repACLTenantID}, repACLEntity)
	})

	adminTok := itLogin(t, r, repACLSlug, "admin@"+repACLSlug, authPassword).Data.AccessToken
	ownerTok := itLogin(t, r, repACLSlug, "owner@"+repACLSlug, authPassword).Data.AccessToken
	viewAllTok := itLogin(t, r, repACLSlug, "viewall@"+repACLSlug, authPassword).Data.AccessToken
	viewNoneTok := itLogin(t, r, repACLSlug, "viewnone@"+repACLSlug, authPassword).Data.AccessToken

	// Admin registers the entity (title text, required).
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": repACLEntity, "label": "Đơn hàng",
		"fields": []map[string]any{{"name": "title", "label": "Tiêu đề", "fieldType": "text", "isRequired": true}},
	}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	// Seed rows so OWNER-scoping is OBSERVABLE: 2 rows created_by=admin + 3 rows created_by=owner.
	// (If every row were admin-owned, an owner-scoped run would see 0 — indistinguishable from the
	// bug being fixed. The 3 owner rows are what discriminate the fix from the bug.)
	seed := func(tok, title string) {
		if w := doAuthed(t, r, http.MethodPost, "/api/v1/"+repACLEntity, tok, map[string]any{"title": title}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed record %q: %d %s", title, w.Code, w.Body.String())
		}
	}
	seed(adminTok, "admin-a")
	seed(adminTok, "admin-b")
	seed(ownerTok, "owner-1")
	seed(ownerTok, "owner-2")
	seed(ownerTok, "owner-3")

	// Admin creates a TABULAR report over the entity (row count reflects the scope directly — no
	// indexed-groupBy setup needed).
	wCreate := doAuthed(t, r, http.MethodPost, "/api/v1/admin/reports", adminTok, map[string]any{
		"name": "Tất cả đơn", "entityName": repACLEntity, "type": "tabular",
		"config": map[string]any{"columns": []string{"title"}},
	}, nil)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("create report: %d %s", wCreate.Code, wCreate.Body.String())
	}
	var created struct {
		Data struct {
			Report struct {
				ID string `json:"id"`
			} `json:"report"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wCreate.Body.Bytes(), &created)
	reportID := created.Data.Report.ID
	if reportID == "" {
		t.Fatalf("empty report id (%s)", wCreate.Body.String())
	}

	// --- RUN: total reflects the caller's A5 scope. -------------------------------------------
	runURL := "/api/v1/reports/" + reportID + "/run"
	type runEnvelope struct {
		Data struct {
			Type  string `json:"type"`
			Total int    `json:"total"`
			Rows  []struct {
				ID string `json:"id"`
			} `json:"rows"`
		} `json:"data"`
		Code string `json:"code"`
	}
	runAs := func(who, tok string) runEnvelope {
		w := doAuthed(t, r, http.MethodGet, runURL, tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s run: got %d, want 200 (body=%s)", who, w.Code, w.Body.String())
		}
		var env runEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return env
	}

	// Owner (A5=owner): sees ONLY its 3 rows — NOT 0 (the bug), NOT 5 (all).
	if env := runAs("owner", ownerTok); env.Data.Total != 3 || len(env.Data.Rows) != 3 {
		t.Fatalf("owner run total=%d rows=%d; want 3/3 (owner sees only its own; not 0=bug, not 5=all)",
			env.Data.Total, len(env.Data.Rows))
	}
	// Viewer-all: sees every tenant row (5).
	if env := runAs("viewall", viewAllTok); env.Data.Total != 5 {
		t.Fatalf("viewall run total=%d; want 5 (all-level grant sees every row)", env.Data.Total)
	}
	// Admin: sees every tenant row (5), unchanged.
	if env := runAs("admin", adminTok); env.Data.Total != 5 {
		t.Fatalf("admin run total=%d; want 5 (admin sees all, unchanged)", env.Data.Total)
	}
	// Viewer-none: 403 FORBIDDEN — no grant, no rows leaked.
	if w := doAuthed(t, r, http.MethodGet, runURL, viewNoneTok, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("no-grant run: got %d, want 403 (body=%s)", w.Code, w.Body.String())
	} else {
		var env runEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != "FORBIDDEN" {
			t.Errorf("no-grant run code = %q; want FORBIDDEN", env.Code)
		}
	}

	// --- EXPORT: same enforcement; a denied caller gets 403 before ANY CSV byte. --------------
	exportURL := "/api/v1/reports/" + reportID + "/export.csv"
	dataRows := func(who, tok string) int {
		w := doAuthed(t, r, http.MethodGet, exportURL, tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s export: got %d, want 200 (body=%s)", who, w.Code, w.Body.String())
		}
		rows := parseDownloadedCSV(t, w.Body.String()) // also pins the UTF-8 BOM the download must open with
		return len(rows) - 1                           // minus the header row
	}
	if n := dataRows("owner", ownerTok); n != 3 {
		t.Errorf("owner export data rows = %d; want 3", n)
	}
	if n := dataRows("admin", adminTok); n != 5 {
		t.Errorf("admin export data rows = %d; want 5", n)
	}
	// Viewer-none export → 403 FORBIDDEN with a JSON envelope (NOT a CSV body): deny before write.
	wNone := doAuthed(t, r, http.MethodGet, exportURL, viewNoneTok, nil, nil)
	if wNone.Code != http.StatusForbidden {
		t.Fatalf("no-grant export: got %d, want 403 (body=%s)", wNone.Code, wNone.Body.String())
	}
	if ct := wNone.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("no-grant export Content-Type = %q; want application/json (deny before any CSV byte)", ct)
	}
	var noneEnv struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(wNone.Body.Bytes(), &noneEnv)
	if noneEnv.Code != "FORBIDDEN" {
		t.Errorf("no-grant export code = %q; want FORBIDDEN", noneEnv.Code)
	}
}
