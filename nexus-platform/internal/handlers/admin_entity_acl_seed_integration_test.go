//go:build integration

// TRỤ 2 mục 4 RED-first proof (docs/NEXUS-MOT-XUONG-2026-07-29.md, "quyền không còn vô hình"):
// before this slice, an entity created BY HAND was invisible (403) to every non-admin — only a
// vertical-pack entity worked, because the pack itself ships its own Roles. This test proves the
// gap is closed for entities created under an app's scope: creating "it_aclseed_widgets" with
// appId set auto-seeds the app's two default roles and grants the entity into them, so a
// non-admin holding the "Nhân viên" role reads 200 (owner-scoped — only their own record) and a
// non-admin holding NO role still reads 403 (default-deny untouched). Runs over a REAL migrated
// MySQL, the full Auth → Tenant → ACL → handler chain, real bcrypt logins — mirrors the a4 RBAC
// harness gold pattern (same throwaway per-test DB via a4SetupMigratedDB).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	aclSeedTenantID = "acls0000-0000-0000-0000-0000000000a1"
	aclSeedSlug     = "acls-acme"
	aclSeedEntity   = "it_aclseed_widgets"
	aclSeedAppName  = "aclseed_app"
)

// newACLSeedRouter mounts Auth + Admin(entities, WithACLSeeder) + App + Record behind the REAL
// Auth → Tenant → ACL chain (a non-nil services.ACLService as aclChecker — unlike the plain
// newFullRouter fixture elsewhere in this package, which passes nil and so never enforces A4).
func newACLSeedRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *gin.Engine {
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

	appRepo := appmeta.NewSQLRepository(sx)
	appSvc := services.NewAppService(appRepo, meta, dashboard.NewSQLRepository(sx), aclSvc, zap.NewNop())
	appACLSeedSvc := services.NewAppACLSeedService(appRepo, roleRepo)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth: NewAuthHandler(authSvc, zap.NewNop()),
		Admin: NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()).
			WithACLSeeder(appACLSeedSvc),
		App:    NewAdminAppHandler(appSvc, zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		// METADATA-ACL-FILTER: the three metadata reads must not name tables the caller cannot
		// view. WithEntityACL is what arms that; a harness omitting it silently tests the
		// pre-fix world (the filter is nil-safe by design, for the older harnesses).
		Metadata: NewMetadataHandler(gdb, zap.NewNop()).WithEntityACL(aclSvc),
	}, zap.NewNop())
	return r
}

type acsAppResp struct {
	Data struct {
		App struct {
			ID string `json:"id"`
		} `json:"app"`
	} `json:"data"`
}

type acsEntityResp struct {
	Data struct {
		Entity struct {
			Name string `json:"name"`
		} `json:"entity"`
		AclSeed struct {
			AppScoped bool   `json:"appScoped"`
			Seeded    bool   `json:"seeded"`
			Warning   string `json:"warning"`
		} `json:"aclSeed"`
	} `json:"data"`
}

type acsListResp struct {
	Data []map[string]any `json:"data"`
}

type acsRecordResp struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

func TestACLSeed_AppScopedEntity_StaffSeesOwnRows_NoRoleGets403(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newACLSeedRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, aclSeedTenantID, aclSeedSlug, "Acme ACL Seed")
	a4SeedUser(t, gdb, aclSeedTenantID, "admin@"+aclSeedSlug, true, nil)
	a4SeedUser(t, gdb, aclSeedTenantID, "norole@"+aclSeedSlug, false, nil)

	t.Cleanup(func() {
		a4DropEntity(sx, []string{aclSeedTenantID}, aclSeedEntity)
		sx.Exec(`DELETE FROM sys_app_nav_items WHERE tenant_id = ?`, aclSeedTenantID)
		sx.Exec(`DELETE FROM sys_apps WHERE tenant_id = ?`, aclSeedTenantID)
	})

	adminTok := itLogin(t, r, aclSeedSlug, "admin@"+aclSeedSlug, authPassword).Data.AccessToken

	// 1) Dựng dự án: tạo app.
	aw := doAuthed(t, r, http.MethodPost, "/api/v1/admin/apps", adminTok, map[string]any{
		"name": aclSeedAppName, "label": "App ACL Seed",
	}, nil)
	if aw.Code != http.StatusCreated {
		t.Fatalf("create app: %d %s", aw.Code, aw.Body.String())
	}
	var appResp acsAppResp
	if err := json.Unmarshal(aw.Body.Bytes(), &appResp); err != nil {
		t.Fatalf("decode app: %v; body=%s", err, aw.Body.String())
	}
	appID := appResp.Data.App.ID
	if appID == "" {
		t.Fatalf("empty app id: %s", aw.Body.String())
	}

	// 2) RED: tạo entity DƯỚI app scope (appId trong body) — phải tự seed vai + grant mặc định.
	ew := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": aclSeedEntity, "label": "Widgets", "appId": appID,
		"fields": []map[string]any{{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true}},
	}, nil)
	if ew.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", ew.Code, ew.Body.String())
	}
	var entResp acsEntityResp
	if err := json.Unmarshal(ew.Body.Bytes(), &entResp); err != nil {
		t.Fatalf("decode entity: %v; body=%s", err, ew.Body.String())
	}
	if !entResp.Data.AclSeed.AppScoped {
		t.Fatal("aclSeed.appScoped = false; want true (appId was sent)")
	}
	if !entResp.Data.AclSeed.Seeded {
		t.Fatalf("aclSeed.seeded = false, warning=%q; want true", entResp.Data.AclSeed.Warning)
	}
	entityName := entResp.Data.Entity.Name
	if entityName == "" {
		t.Fatal("empty entity name in response")
	}

	// 3) Đọc thẳng id vai "Nhân viên" vừa được auto-seed (khớp appDefaultRoleName trong
	// internal/services/app_acl_seed.go) rồi gán CHO staff — gán người là hành động chủ đích của
	// admin (spec item 4: KHÔNG tự gán), nên test tự làm việc đó, KHÔNG phải seeder.
	var staffRoleID string
	if err := sx.Get(&staffRoleID, `SELECT id FROM acl_roles WHERE tenant_id = ? AND name = ?`,
		aclSeedTenantID, "app_"+aclSeedAppName+"_staff"); err != nil {
		t.Fatalf("tìm vai Nhân viên vừa seed: %v (seeding có thật sự chạy không?)", err)
	}
	a4SeedUser(t, gdb, aclSeedTenantID, "staff@"+aclSeedSlug, false, &staffRoleID)

	staffTok := itLogin(t, r, aclSeedSlug, "staff@"+aclSeedSlug, authPassword).Data.AccessToken
	noRoleTok := itLogin(t, r, aclSeedSlug, "norole@"+aclSeedSlug, authPassword).Data.AccessToken

	// 4) Staff tự tạo MỘT bản ghi (sở hữu bản ghi đó).
	cw := doAuthed(t, r, http.MethodPost, "/api/v1/"+entityName, staffTok, map[string]any{"title": "của nhân viên"}, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("staff create record: %d %s", cw.Code, cw.Body.String())
	}
	var staffRec acsRecordResp
	if err := json.Unmarshal(cw.Body.Bytes(), &staffRec); err != nil || staffRec.Data.ID == "" {
		t.Fatalf("decode staff record: %v; body=%s", err, cw.Body.String())
	}

	// 5) Admin tạo bản ghi THỨ HAI (không thuộc staff) — nếu row-filter bị hỏng (vô tình thành
	// "all"), staff sẽ thấy 2 thay vì 1.
	aw2 := doAuthed(t, r, http.MethodPost, "/api/v1/"+entityName, adminTok, map[string]any{"title": "của admin"}, nil)
	if aw2.Code != http.StatusCreated {
		t.Fatalf("admin create record: %d %s", aw2.Code, aw2.Body.String())
	}
	var adminRec acsRecordResp
	if err := json.Unmarshal(aw2.Body.Bytes(), &adminRec); err != nil || adminRec.Data.ID == "" {
		t.Fatalf("decode admin record: %v; body=%s", err, aw2.Body.String())
	}

	// 5b) owner-scope PHẢI áp cả trên GHI, không chỉ ĐỌC: staff PUT bản ghi CỦA MÌNH -> 200; staff
	// PUT bản ghi CỦA ADMIN (ngoài phạm vi sửa owner) -> KHÔNG được 200. Staff XEM được bản ghi đó
	// (view:all, bước 6), nên lời từ chối là 403 FORBIDDEN (#18) — 404 chỉ dành cho bản ghi người gọi
	// không xem được (không tiết lộ tồn tại).
	puw := doAuthed(t, r, http.MethodPut, "/api/v1/"+entityName+"/"+staffRec.Data.ID, staffTok, map[string]any{"title": "đã sửa"}, nil)
	if puw.Code != http.StatusOK {
		t.Fatalf("staff PUT bản ghi của mình: %d %s; want 200", puw.Code, puw.Body.String())
	}
	pow := doAuthed(t, r, http.MethodPut, "/api/v1/"+entityName+"/"+adminRec.Data.ID, staffTok, map[string]any{"title": "chiếm quyền"}, nil)
	if pow.Code == http.StatusOK {
		t.Fatalf("staff PUT bản ghi của admin trả 200 — owner-scope KHÔNG áp trên ghi (lỗ bảo mật): %s", pow.Body.String())
	}
	if pow.Code != http.StatusForbidden {
		t.Fatalf("staff PUT bản ghi của admin: %d %s; want 403 (xem được nhưng không sửa được, #18)", pow.Code, pow.Body.String())
	}

	// 6) Staff GET list -> 200, thấy CẢ HAI bản ghi.
	//
	// ĐÃ ĐỔI 2026-07-29 theo quyết định của Founder: vai nhân viên mặc định là **"thấy hết, chỉ sửa
	// được của mình"** (`view:all` + `create/edit:owner`, xem staffGrants trong
	// internal/services/app_acl_seed.go). Trước đó `view` cũng là `owner` và test này khoá đúng con
	// số 1 — nên nó ĐỎ ngay sau khi default đổi, và đỏ ở một chỗ chỉ chạy với `-tags=integration`,
	// tức `go test ./...` thường không hề thấy. Đây là bằng chứng phải chạy CẢ suite integration khi
	// đụng ngữ nghĩa quyền, không chỉ package mình vừa sửa.
	//
	// Vế "chỉ sửa của mình" KHÔNG bị nới: bước 5 ngay trên vẫn đòi PUT lên bản ghi của admin bị từ chối (403).
	lw := doAuthed(t, r, http.MethodGet, "/api/v1/"+entityName, staffTok, nil, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("staff list: %d %s", lw.Code, lw.Body.String())
	}
	var listResp acsListResp
	if err := json.Unmarshal(lw.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, lw.Body.String())
	}
	if len(listResp.Data) != 2 {
		t.Fatalf("staff thấy %d bản ghi; muốn 2 (view:all — Founder chốt \"thấy hết, chỉ sửa được của mình\")", len(listResp.Data))
	}

	// 7) User không mang vai nào -> vẫn 403 (default-deny không đổi, spec item 6).
	nw := doAuthed(t, r, http.MethodGet, "/api/v1/"+entityName, noRoleTok, nil, nil)
	if nw.Code != http.StatusForbidden {
		t.Fatalf("no-role list: %d %s; want 403", nw.Code, nw.Body.String())
	}
}

// METADATA-ACL-FILTER — a staff member must not learn the NAME of a table they cannot open.
//
// Measured on the live northwind tenant 2026-07-30, BEFORE this fix: an account that could read 8 of 19
// tables got all 19 names from GET /metadata/entities, 7 of the hidden ones from
// GET /metadata/relationships, and the FULL schema of a hidden table from
// GET /metadata/fields/<name> with a 200. Both a metadata leak and a dead end — the names showed up
// in "Tất cả dữ liệu", the user clicked, and the data call answered 403.
//
// This pins all three reads at once, because closing one of them is how the leak moves next door:
// the acceptance gate (scripts/acceptance-acl.py) only compares /metadata/entities against the data
// reads, so it went quiet after the first endpoint was filtered while two others still leaked.
func TestMetadataACLFilter_StaffNeverLearnsNamesItCannotView(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newACLSeedRouter(t, gdb, sx)

	const hiddenEntity = "it_meta_secret"

	a4SeedTenant(t, gdb, aclSeedTenantID, aclSeedSlug, "Acme Metadata ACL")
	a4SeedUser(t, gdb, aclSeedTenantID, "admin@"+aclSeedSlug, true, nil)
	a4SeedUser(t, gdb, aclSeedTenantID, "norole@"+aclSeedSlug, false, nil)

	t.Cleanup(func() {
		a4DropEntity(sx, []string{aclSeedTenantID}, aclSeedEntity)
		a4DropEntity(sx, []string{aclSeedTenantID}, hiddenEntity)
		sx.Exec(`DELETE FROM sys_app_nav_items WHERE tenant_id = ?`, aclSeedTenantID)
		sx.Exec(`DELETE FROM sys_apps WHERE tenant_id = ?`, aclSeedTenantID)
	})

	adminTok := itLogin(t, r, aclSeedSlug, "admin@"+aclSeedSlug, authPassword).Data.AccessToken

	// GRANTED table: created under an app scope, which auto-seeds the staff role + view grant.
	aw := doAuthed(t, r, http.MethodPost, "/api/v1/admin/apps", adminTok, map[string]any{
		"name": aclSeedAppName, "label": "App Metadata ACL",
	}, nil)
	if aw.Code != http.StatusCreated {
		t.Fatalf("create app: %d %s", aw.Code, aw.Body.String())
	}
	var appResp acsAppResp
	if err := json.Unmarshal(aw.Body.Bytes(), &appResp); err != nil {
		t.Fatalf("decode app: %v; body=%s", err, aw.Body.String())
	}
	ew := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": aclSeedEntity, "label": "Widgets", "appId": appResp.Data.App.ID,
		"fields": []map[string]any{{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true}},
	}, nil)
	if ew.Code != http.StatusCreated {
		t.Fatalf("create granted entity: %d %s", ew.Code, ew.Body.String())
	}

	// HIDDEN table: no appId ⇒ no role, no grant. This is the ordinary shape of a hand-built
	// entity, which is exactly why the leak mattered.
	hw := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": hiddenEntity, "label": "Bí mật",
		"fields": []map[string]any{{"name": "code", "label": "Mã", "fieldType": "text"}},
	}, nil)
	if hw.Code != http.StatusCreated {
		t.Fatalf("create hidden entity: %d %s", hw.Code, hw.Body.String())
	}

	var staffRoleID string
	if err := sx.Get(&staffRoleID, `SELECT id FROM acl_roles WHERE tenant_id = ? AND name = ?`,
		aclSeedTenantID, "app_"+aclSeedAppName+"_staff"); err != nil {
		t.Fatalf("find auto-seeded staff role: %v", err)
	}
	a4SeedUser(t, gdb, aclSeedTenantID, "staff@"+aclSeedSlug, false, &staffRoleID)
	staffTok := itLogin(t, r, aclSeedSlug, "staff@"+aclSeedSlug, authPassword).Data.AccessToken
	noRoleTok := itLogin(t, r, aclSeedSlug, "norole@"+aclSeedSlug, authPassword).Data.AccessToken

	entityNames := func(tok string) map[string]bool {
		w := doAuthed(t, r, http.MethodGet, "/api/v1/metadata/entities", tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /metadata/entities = %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data []struct{ Name string } `json:"data"`
			Meta struct{ Total int }     `json:"meta"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode entities: %v; body=%s", err, w.Body.String())
		}
		out := map[string]bool{}
		for _, e := range resp.Data {
			out[e.Name] = true
		}
		// meta.total must agree with the FILTERED list, or the count itself leaks the number
		// of tables the caller cannot see.
		if resp.Meta.Total != len(resp.Data) {
			t.Errorf("meta.total = %d but data has %d rows — the count leaks hidden tables", resp.Meta.Total, len(resp.Data))
		}
		return out
	}

	// Admin keeps the whole model (D3 super-gate) — Studio and the AI builder depend on it.
	admin := entityNames(adminTok)
	if !admin[aclSeedEntity] || !admin[hiddenEntity] {
		t.Fatalf("admin must see BOTH tables; got %v", admin)
	}

	staff := entityNames(staffTok)
	if !staff[aclSeedEntity] {
		t.Errorf("staff lost the table they DO have a grant on (%s) — filter is too aggressive", aclSeedEntity)
	}
	if staff[hiddenEntity] {
		t.Errorf("staff still sees the name %q with no grant — METADATA-ACL-FILTER regressed", hiddenEntity)
	}

	// A user with no role at all sees nothing (deny-all is the fail-closed default).
	if noRole := entityNames(noRoleTok); noRole[aclSeedEntity] || noRole[hiddenEntity] {
		t.Errorf("a user with NO role must see no table names; got %v", noRole)
	}

	// The schema read must refuse with the SAME 404 body as a table that does not exist —
	// 403 would confirm existence (api-conventions anti-enumeration rule).
	fw := doAuthed(t, r, http.MethodGet, "/api/v1/metadata/fields/"+hiddenEntity, staffTok, nil, nil)
	if fw.Code != http.StatusNotFound {
		t.Errorf("GET /metadata/fields/%s as staff = %d; want 404 (was 200: full schema leaked)", hiddenEntity, fw.Code)
	}
	var ferr struct{ Code string }
	_ = json.Unmarshal(fw.Body.Bytes(), &ferr)
	if ferr.Code != "ENTITY_NOT_FOUND" {
		t.Errorf("refusal code = %q; want ENTITY_NOT_FOUND (indistinguishable from absent)", ferr.Code)
	}
	// ...and the granted table still reads normally, so the guard is not a blanket denial.
	if okw := doAuthed(t, r, http.MethodGet, "/api/v1/metadata/fields/"+aclSeedEntity, staffTok, nil, nil); okw.Code != http.StatusOK {
		t.Errorf("GET /metadata/fields/%s as staff = %d; want 200", aclSeedEntity, okw.Code)
	}
}
