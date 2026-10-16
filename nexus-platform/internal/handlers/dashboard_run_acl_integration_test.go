//go:build integration

// DASHBOARD-RUN-ACL integration test — the /dashboards/:id/run endpoint is mounted OUTSIDE the
// /:entity RequireACL middleware, so each widget's entity ACL must be resolved in the run path
// (RecordService.AuthorizeView, per-widget). Proven here end-to-end over real MySQL:
//   - a non-admin with view on ONE widget's entity but NOT the other gets a RESULT for the granted
//     widget and a per-widget FORBIDDEN for the denied one (a denied widget must NOT kill siblings,
//     and the dashboard HTTP status stays 200 — per-widget errors are body-level);
//   - an admin gets a result for BOTH widgets.
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	dashACLTenantID = "dash1000-0000-0000-0000-0000000000a1"
	dashACLSlug     = "dash-acme"
	dashGranted     = "it_dashacl_orders" // owner can view
	dashDenied      = "it_dashacl_secret" // owner has NO grant
)

func newDashboardACLRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *gin.Engine {
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
	recordSvc := services.NewRecordService(meta, dyn).WithACLAuthorizer(aclSvc)
	dashSvc := services.NewDashboardService(dashboard.NewSQLRepository(sx), recordSvc, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:      NewAuthHandler(authSvc, zap.NewNop()),
		Admin:     NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record:    NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Dashboard: NewAdminDashboardHandler(dashSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

func TestDashboardRunACL_PerWidget(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newDashboardACLRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, dashACLTenantID, dashACLSlug, "Acme DashACL")
	// owner: view@all on the granted entity (+ create so it can seed rows) — NO grant on the denied one.
	ownerRole := a4SeedRole(t, gdb, dashACLTenantID, "dash-owner", []auth.RoleAction{
		{EntityName: dashGranted, Action: "view", AccessLevel: "all"},
		{EntityName: dashGranted, Action: "create", AccessLevel: "all"},
	})
	a4SeedUser(t, gdb, dashACLTenantID, "admin@"+dashACLSlug, true, nil)
	a4SeedUser(t, gdb, dashACLTenantID, "owner@"+dashACLSlug, false, &ownerRole)

	a4DropEntity(sx, []string{dashACLTenantID}, dashGranted)
	a4DropEntity(sx, []string{dashACLTenantID}, dashDenied)
	t.Cleanup(func() {
		sx.Exec(`DELETE FROM sys_dashboards WHERE tenant_id = ?`, dashACLTenantID)
		a4DropEntity(sx, []string{dashACLTenantID}, dashGranted)
		a4DropEntity(sx, []string{dashACLTenantID}, dashDenied)
	})

	adminTok := itLogin(t, r, dashACLSlug, "admin@"+dashACLSlug, authPassword).Data.AccessToken
	ownerTok := itLogin(t, r, dashACLSlug, "owner@"+dashACLSlug, authPassword).Data.AccessToken

	for _, e := range []string{dashGranted, dashDenied} {
		if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
			"name": e, "label": e,
			"fields": []map[string]any{{"name": "title", "label": "T", "fieldType": "text", "isRequired": true}},
		}, nil); w.Code != http.StatusCreated {
			t.Fatalf("create entity %s: %d %s", e, w.Code, w.Body.String())
		}
		// One row each so a COUNT widget has data.
		if w := doAuthed(t, r, http.MethodPost, "/api/v1/"+e, adminTok, map[string]any{"title": "r1"}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed row %s: %d %s", e, w.Code, w.Body.String())
		}
	}

	// A dashboard with a COUNT widget on each entity.
	body := map[string]any{
		"name": "d1",
		"widgets": []map[string]any{
			{"id": "w-granted", "title": "Granted", "type": "kpi", "entity": dashGranted, "metric": "count"},
			{"id": "w-denied", "title": "Denied", "type": "kpi", "entity": dashDenied, "metric": "count"},
		},
	}
	cw := doAuthed(t, r, http.MethodPost, "/api/v1/admin/dashboards", adminTok, body, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", cw.Code, cw.Body.String())
	}
	var created struct {
		Data struct {
			Dashboard struct {
				ID string `json:"id"`
			} `json:"dashboard"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, cw.Body.String())
	}
	dashID := created.Data.Dashboard.ID
	if dashID == "" {
		t.Fatalf("no dashboard id in create response: %s", cw.Body.String())
	}

	widgetCodes := func(tok string) map[string]string {
		w := doAuthed(t, r, http.MethodGet, "/api/v1/dashboards/"+dashID+"/run", tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("run dashboard: %d %s", w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				Widgets []struct {
					WidgetID string          `json:"widgetId"`
					Result   json.RawMessage `json:"result"`
					Code     string          `json:"code"`
				} `json:"widgets"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode run: %v (%s)", err, w.Body.String())
		}
		out := map[string]string{}
		for _, wd := range env.Data.Widgets {
			if len(wd.Result) > 0 && string(wd.Result) != "null" {
				out[wd.WidgetID] = "result"
			} else {
				out[wd.WidgetID] = wd.Code
			}
		}
		return out
	}

	// Owner: granted widget has a result, denied widget is a per-widget FORBIDDEN (200 overall).
	own := widgetCodes(ownerTok)
	if own["w-granted"] != "result" {
		t.Fatalf("owner granted widget = %q; want a result", own["w-granted"])
	}
	if own["w-denied"] != "FORBIDDEN" {
		t.Fatalf("owner denied widget = %q; want FORBIDDEN (per-widget ACL)", own["w-denied"])
	}
	// Admin: both widgets return a result.
	adm := widgetCodes(adminTok)
	if adm["w-granted"] != "result" || adm["w-denied"] != "result" {
		t.Fatalf("admin widgets = %v; want both result", adm)
	}
}
