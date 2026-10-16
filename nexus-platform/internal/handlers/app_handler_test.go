package handlers

// httptest coverage for AdminAppHandler (../specs/010-app-container/spec.md §6): admin CRUD happy path +
// 409 duplicate slug + 422 invalid slug/nav-ref, the any-user GET /apps catalog, and the INV-3
// ACL filter on GET /apps/:name/nav (denied entity dropped via a fake ACL). The auth chain is
// simulated the way AuthMiddleware would populate context (tenantID + authClaims), same pattern
// as the other handler suites.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// ---- fakes wired under the real AppService --------------------------------------------------

type stubAppStore struct {
	apps map[string]*appmeta.App
	// ghost: names List still returns but GetByName no longer finds (app deleted mid-loop).
	ghost map[string]bool
	nav   map[string][]appmeta.NavItem
	seq   int
}

func newStubAppStore() *stubAppStore {
	return &stubAppStore{apps: map[string]*appmeta.App{}, nav: map[string][]appmeta.NavItem{}}
}

func (f *stubAppStore) Create(_ context.Context, in appmeta.CreateInput) (string, error) {
	for _, a := range f.apps {
		if a.TenantID == in.TenantID && a.Name == in.Name {
			return "", appmeta.ErrAppDuplicateName
		}
	}
	f.seq++
	id := fmt.Sprintf("app-%d", f.seq)
	f.apps[id] = &appmeta.App{ID: id, TenantID: in.TenantID, Name: in.Name, Label: in.Label,
		IsDefault: in.IsDefault, SortOrder: in.SortOrder}
	return id, nil
}

func (f *stubAppStore) GetByID(_ context.Context, tenantID, id string) (*appmeta.App, error) {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return nil, appmeta.ErrAppNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *stubAppStore) GetByName(_ context.Context, tenantID, name string) (*appmeta.App, error) {
	if f.ghost[name] {
		return nil, appmeta.ErrAppNotFound
	}
	for _, a := range f.apps {
		if a.TenantID == tenantID && a.Name == name {
			cp := *a
			return &cp, nil
		}
	}
	return nil, appmeta.ErrAppNotFound
}

func (f *stubAppStore) List(_ context.Context, tenantID string) ([]appmeta.App, error) {
	var out []appmeta.App
	for _, a := range f.apps {
		if a.TenantID == tenantID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *stubAppStore) Update(_ context.Context, tenantID, id string, in appmeta.UpdateInput) error {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return appmeta.ErrAppNotFound
	}
	a.Label = in.Label
	if in.HomeDashboardID != nil {
		a.HomeDashboardID = *in.HomeDashboardID
	}
	if in.HomeQueueIDs != nil {
		a.HomeQueueIDs = *in.HomeQueueIDs
	}
	if in.DefaultRoleIDs != nil {
		a.DefaultRoleIDs = *in.DefaultRoleIDs
	}
	return nil
}

func (f *stubAppStore) SoftDelete(_ context.Context, tenantID, id string) error {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return appmeta.ErrAppNotFound
	}
	delete(f.apps, id)
	delete(f.nav, id)
	return nil
}

func (f *stubAppStore) ReplaceNav(_ context.Context, tenantID, appID string, items []appmeta.NavItemInput) error {
	rows := make([]appmeta.NavItem, 0, len(items))
	for i, it := range items {
		rows = append(rows, appmeta.NavItem{ID: fmt.Sprintf("nav-%d", i), TenantID: tenantID,
			AppID: appID, ItemType: it.ItemType, RefID: it.RefID, SortOrder: i})
	}
	f.nav[appID] = rows
	return nil
}

func (f *stubAppStore) ListNav(_ context.Context, tenantID, appID string) ([]appmeta.NavItem, error) {
	var out []appmeta.NavItem
	for _, it := range f.nav[appID] {
		if it.TenantID == tenantID {
			out = append(out, it)
		}
	}
	return out, nil
}

type stubAppEntities struct{ ents []metadata.EntityDefinition }

func (f *stubAppEntities) ListEntities(string) ([]metadata.EntityDefinition, error) {
	return f.ents, nil
}

type stubAppDashboards struct{ dashes []dashboard.Record }

func (f *stubAppDashboards) List(context.Context, string) ([]dashboard.Record, error) {
	return f.dashes, nil
}

type stubAppACL struct{ allow map[string]bool }

func (f *stubAppACL) CanViewEntity(_ context.Context, _, _, entityName string) (bool, error) {
	return f.allow[entityName], nil
}

// appServe runs one request through a fresh engine wearing a principal shim in front of the
// handler routes — the shim injects the verified tenant + claims the way the Auth → Tenant
// chain would (admin gating itself is router wiring, exercised by the router suite; this suite
// pins the handler+service behaviour).
func appServe(t *testing.T, sub auth.Subject, acl *stubAppACL, seed func(*stubAppStore), method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := newStubAppStore()
	if seed != nil {
		seed(st)
	}
	ents := &stubAppEntities{ents: []metadata.EntityDefinition{
		{ID: "ent-accounts", TenantID: "tenant-a", Name: "accounts", Label: "Khách hàng"},
		{ID: "ent-contacts", TenantID: "tenant-a", Name: "contacts", Label: "Liên hệ"},
	}}
	dashes := &stubAppDashboards{dashes: []dashboard.Record{{ID: "dash-1", TenantID: "tenant-a", Name: "KPI"}}}
	if acl == nil {
		acl = &stubAppACL{allow: map[string]bool{"accounts": true, "contacts": true}}
	}
	svc := services.NewAppService(st, ents, dashes, acl, nil).
		WithHomeRefs(stubQueueIDs{"q-1": true, "q-2": true}, stubRoleIDs{"role-1": true, "role-2": true})
	h := NewAdminAppHandler(svc, zap.NewNop())

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", sub.TenantID)
		c.Set("authClaims", &auth.Claims{Subject: sub})
		c.Next()
	})
	api := r.Group("/api/v1")
	h.RegisterAdminRoutes(api)
	h.RegisterReadRoutes(api)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var appAdmin = auth.Subject{UserID: "u-admin", TenantID: "tenant-a", IsAdmin: true}
var appUser = auth.Subject{UserID: "u-1", TenantID: "tenant-a", RoleID: "role-1"}

// seedSalesApp preloads one app ("sales") with an accounts+contacts+dashboard nav.
func seedSalesApp(st *stubAppStore) {
	st.apps["app-1"] = &appmeta.App{ID: "app-1", TenantID: "tenant-a", Name: "sales", Label: "Sales"}
	st.nav["app-1"] = []appmeta.NavItem{
		{ID: "n1", TenantID: "tenant-a", AppID: "app-1", ItemType: "entity", RefID: "ent-accounts", SortOrder: 0},
		{ID: "n2", TenantID: "tenant-a", AppID: "app-1", ItemType: "entity", RefID: "ent-contacts", SortOrder: 1},
		{ID: "n3", TenantID: "tenant-a", AppID: "app-1", ItemType: "dashboard", RefID: "dash-1", SortOrder: 2},
	}
}

func TestAppHandler_AdminCRUD(t *testing.T) {
	t.Run("POST /admin/apps → 201 với app đã lưu", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, nil, http.MethodPost, "/api/v1/admin/apps",
			`{"name":"Sales","label":"Bán hàng","icon":"briefcase"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s); want 201", w.Code, w.Body.String())
		}
		var resp struct {
			Data struct {
				App struct {
					ID    string `json:"id"`
					Name  string `json:"name"`
					Label string `json:"label"`
				} `json:"app"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Data.App.Name != "sales" || resp.Data.App.Label != "Bán hàng" {
			t.Errorf("app = %+v; want lowercased slug + label", resp.Data.App)
		}
	})

	t.Run("POST slug trùng → 409 APP_ALREADY_EXISTS", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, seedSalesApp, http.MethodPost, "/api/v1/admin/apps",
			`{"name":"sales","label":"Sales 2"}`)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "APP_ALREADY_EXISTS") {
			t.Fatalf("status = %d (%s); want 409 APP_ALREADY_EXISTS", w.Code, w.Body.String())
		}
	})

	t.Run("POST slug sai → 422 APP_NAME_INVALID", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, nil, http.MethodPost, "/api/v1/admin/apps",
			`{"name":"Sales App!","label":"x"}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "APP_NAME_INVALID") {
			t.Fatalf("status = %d (%s); want 422 APP_NAME_INVALID", w.Code, w.Body.String())
		}
	})

	t.Run("GET /admin/apps/:id → 200 app + items; id lạ → 404", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, seedSalesApp, http.MethodGet, "/api/v1/admin/apps/app-1", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ent-accounts"`) {
			t.Fatalf("status = %d (%s); want 200 with nav items", w.Code, w.Body.String())
		}
		w = appServe(t, appAdmin, nil, seedSalesApp, http.MethodGet, "/api/v1/admin/apps/ghost", "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "APP_NOT_FOUND") {
			t.Fatalf("status = %d (%s); want 404 APP_NOT_FOUND", w.Code, w.Body.String())
		}
	})

	t.Run("PUT /admin/apps/:id → 200; DELETE → 204", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, seedSalesApp, http.MethodPut, "/api/v1/admin/apps/app-1",
			`{"label":"Sales VN"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sales VN") {
			t.Fatalf("PUT status = %d (%s); want 200 updated", w.Code, w.Body.String())
		}
		w = appServe(t, appAdmin, nil, seedSalesApp, http.MethodDelete, "/api/v1/admin/apps/app-1", "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE status = %d (%s); want 204", w.Code, w.Body.String())
		}
	})

	t.Run("PUT nav ref lạ → 422 APP_NAV_REF_INVALID; ref hợp lệ → 200", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, seedSalesApp, http.MethodPut, "/api/v1/admin/apps/app-1/nav",
			`[{"itemType":"entity","refId":"ent-ghost"}]`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "APP_NAV_REF_INVALID") {
			t.Fatalf("status = %d (%s); want 422 APP_NAV_REF_INVALID", w.Code, w.Body.String())
		}
		w = appServe(t, appAdmin, nil, seedSalesApp, http.MethodPut, "/api/v1/admin/apps/app-1/nav",
			`[{"itemType":"dashboard","refId":"dash-1"},{"itemType":"entity","refId":"ent-accounts"}]`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sortOrder":1`) {
			t.Fatalf("status = %d (%s); want 200 with reordered items", w.Code, w.Body.String())
		}
	})
}

func TestAppHandler_ReadSurface(t *testing.T) {
	t.Run("GET /apps — user thường (không admin) đọc được catalog", func(t *testing.T) {
		w := appServe(t, appUser, nil, seedSalesApp, http.MethodGet, "/api/v1/apps", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s); want 200", w.Code, w.Body.String())
		}
		var resp struct {
			Data struct {
				Apps []map[string]any `json:"apps"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Data.Apps) != 1 || resp.Data.Apps[0]["name"] != "sales" {
			t.Fatalf("apps = %+v; want the seeded app", resp.Data.Apps)
		}
		if _, leaked := resp.Data.Apps[0]["createdBy"]; leaked {
			t.Errorf("catalog leaked audit fields: %+v", resp.Data.Apps[0])
		}
	})

	t.Run("GET /apps/:name/nav ẨN entity bị deny (INV-3), không lộ tồn tại", func(t *testing.T) {
		acl := &stubAppACL{allow: map[string]bool{"accounts": true}} // contacts denied
		w := appServe(t, appUser, acl, seedSalesApp, http.MethodGet, "/api/v1/apps/sales/nav", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s); want 200", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "contacts") || strings.Contains(body, "Liên hệ") {
			t.Errorf("denied entity leaked into nav: %s", body)
		}
		if !strings.Contains(body, "/accounts") || !strings.Contains(body, "/dashboards/dash-1") {
			t.Errorf("allowed items missing: %s", body)
		}
	})

	t.Run("GET /apps/:name/nav — admin thấy đủ", func(t *testing.T) {
		acl := &stubAppACL{allow: map[string]bool{}} // deny-all: admin must bypass
		w := appServe(t, appAdmin, acl, seedSalesApp, http.MethodGet, "/api/v1/apps/sales/nav", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "contacts") {
			t.Fatalf("status = %d (%s); want 200 with every item", w.Code, w.Body.String())
		}
	})

	t.Run("GET /apps/:name/nav app lạ → 404", func(t *testing.T) {
		w := appServe(t, appUser, nil, seedSalesApp, http.MethodGet, "/api/v1/apps/ghost/nav", "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "APP_NOT_FOUND") {
			t.Fatalf("status = %d (%s); want 404 APP_NOT_FOUND", w.Code, w.Body.String())
		}
	})
}

// seedCatalogApps: "sales" (accounts+contacts) and "board" (dashboard only), plus "empty" (no nav)
// and "hidden" (only a denied entity).
func seedCatalogApps(st *stubAppStore) {
	seedSalesApp(st)
	st.apps["app-2"] = &appmeta.App{ID: "app-2", TenantID: "tenant-a", Name: "empty", Label: "Trống"}
	st.apps["app-3"] = &appmeta.App{ID: "app-3", TenantID: "tenant-a", Name: "board", Label: "Bảng", HomeDashboardID: "dash-1"}
	st.nav["app-3"] = []appmeta.NavItem{{ID: "n9", TenantID: "tenant-a", AppID: "app-3", ItemType: "dashboard", RefID: "dash-1"}}
	st.apps["app-4"] = &appmeta.App{ID: "app-4", TenantID: "tenant-a", Name: "hidden", Label: "Ẩn"}
	st.nav["app-4"] = []appmeta.NavItem{{ID: "n10", TenantID: "tenant-a", AppID: "app-4", ItemType: "entity", RefID: "ent-accounts"}}
}

func catalogNames(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Data struct {
			Apps []map[string]any `json:"apps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	var names []string
	for _, a := range resp.Data.Apps {
		names = append(names, a["name"].(string))
	}
	sort.Strings(names)
	return names
}

func TestAppHandler_CatalogScopedToCaller(t *testing.T) {
	t.Run("nhân viên chỉ thấy app có ít nhất một mục nhìn được", func(t *testing.T) {
		acl := &stubAppACL{allow: map[string]bool{}} // mọi entity bị chặn
		w := appServe(t, appUser, acl, seedCatalogApps, http.MethodGet, "/api/v1/apps", "")
		got := catalogNames(t, w)
		if len(got) != 2 || got[0] != "board" || got[1] != "sales" {
			t.Fatalf("apps = %v; want [board sales] (app rỗng và app chỉ có entity bị chặn phải ẩn)", got)
		}
	})
	t.Run("admin thấy đủ mọi app", func(t *testing.T) {
		acl := &stubAppACL{allow: map[string]bool{}}
		w := appServe(t, appAdmin, acl, seedCatalogApps, http.MethodGet, "/api/v1/apps", "")
		if got := catalogNames(t, w); len(got) != 4 {
			t.Fatalf("apps = %v; want 4", got)
		}
	})
	t.Run("catalog mang homeDashboardId", func(t *testing.T) {
		w := appServe(t, appUser, nil, seedCatalogApps, http.MethodGet, "/api/v1/apps", "")
		if !strings.Contains(w.Body.String(), `"homeDashboardId":"dash-1"`) {
			t.Fatalf("catalog thiếu homeDashboardId: %s", w.Body.String())
		}
	})
}

func TestAppHandler_UpdateHomeDashboard(t *testing.T) {
	put := func(body string) *httptest.ResponseRecorder {
		return appServe(t, appAdmin, nil, seedSalesApp, http.MethodPut, "/api/v1/admin/apps/app-1", body)
	}
	t.Run("dashboard không tồn tại → 422", func(t *testing.T) {
		w := put(`{"label":"Sales","homeDashboardId":"dash-ghost"}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "APP_HOME_DASHBOARD_INVALID") {
			t.Fatalf("status = %d (%s); want 422 APP_HOME_DASHBOARD_INVALID", w.Code, w.Body.String())
		}
	})
	t.Run("dashboard hợp lệ → 200 và ghi vào app", func(t *testing.T) {
		w := put(`{"label":"Sales","homeDashboardId":"dash-1"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"homeDashboardId":"dash-1"`) {
			t.Fatalf("status = %d (%s); want 200 with homeDashboardId", w.Code, w.Body.String())
		}
	})
	t.Run("không gửi khoá → giữ nguyên (không reset)", func(t *testing.T) {
		w := appServe(t, appAdmin, nil, seedCatalogApps, http.MethodPut, "/api/v1/admin/apps/app-3", `{"label":"Bảng"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"homeDashboardId":"dash-1"`) {
			t.Fatalf("status = %d (%s); homeDashboardId phải được giữ", w.Code, w.Body.String())
		}
	})
}
