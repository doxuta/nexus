package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/workqueue"
)

// stubQueueIDs / stubRoleIDs are the tenant's existing work queues / roles under the real AppService.
type stubQueueIDs map[string]bool

func (f stubQueueIDs) ListAll(context.Context, string) ([]workqueue.Queue, error) {
	var out []workqueue.Queue
	for id := range f {
		out = append(out, workqueue.Queue{ID: id})
	}
	return out, nil
}

type stubRoleIDs map[string]bool

func (f stubRoleIDs) RoleExists(_ context.Context, _, id string) (bool, error) { return f[id], nil }

// specs/074 lát f2-app-home-default-ui / queue #483: the builder picks the home queues and the
// per-role default app explicitly.
func TestAppHandler_ExplicitHomeQueuesAndRoleDefault(t *testing.T) {
	put := func(body string) (int, string) {
		w := appServe(t, appAdmin, nil, seedSalesApp, http.MethodPut, "/api/v1/admin/apps/app-1", body)
		return w.Code, w.Body.String()
	}
	t.Run("hàng đợi có thật → 200 và ghi vào app", func(t *testing.T) {
		code, body := put(`{"label":"Sales","homeQueueIds":["q-1","q-2"]}`)
		if code != http.StatusOK || !strings.Contains(body, `"homeQueueIds":["q-1","q-2"]`) {
			t.Fatalf("status = %d (%s); want 200 with homeQueueIds", code, body)
		}
	})
	t.Run("hàng đợi không tồn tại → 422", func(t *testing.T) {
		code, body := put(`{"label":"Sales","homeQueueIds":["q-ghost"]}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "APP_HOME_QUEUE_INVALID") {
			t.Fatalf("status = %d (%s); want 422 APP_HOME_QUEUE_INVALID", code, body)
		}
	})
	t.Run("vai không tồn tại → 422", func(t *testing.T) {
		code, body := put(`{"label":"Sales","defaultRoleIds":["role-ghost"]}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "APP_DEFAULT_ROLE_INVALID") {
			t.Fatalf("status = %d (%s); want 422 APP_DEFAULT_ROLE_INVALID", code, body)
		}
	})
	t.Run("vai có thật → 200", func(t *testing.T) {
		code, body := put(`{"label":"Sales","defaultRoleIds":["role-1"]}`)
		if code != http.StatusOK || !strings.Contains(body, `"defaultRoleIds":["role-1"]`) {
			t.Fatalf("status = %d (%s); want 200 with defaultRoleIds", code, body)
		}
	})
	t.Run("không gửi khoá → giữ nguyên", func(t *testing.T) {
		seed := func(st *stubAppStore) {
			seedSalesApp(st)
			st.apps["app-1"].HomeQueueIDs = []string{"q-1"}
		}
		w := appServe(t, appAdmin, nil, seed, http.MethodPut, "/api/v1/admin/apps/app-1", `{"label":"Sales"}`)
		if !strings.Contains(w.Body.String(), `"homeQueueIds":["q-1"]`) {
			t.Fatalf("homeQueueIds phải được giữ: %s", w.Body.String())
		}
	})
}

func TestAppHandler_CatalogRoleDefaultAndHomeOnly(t *testing.T) {
	seed := func(st *stubAppStore) {
		seedCatalogApps(st)
		st.apps["app-1"].DefaultRoleIDs = []string{"role-1"}
		st.apps["app-1"].HomeQueueIDs = []string{"q-1"}
		// app chỉ có trang chủ dashboard, chưa có mục menu nào
		st.apps["app-2"].HomeDashboardID = "dash-1"
	}
	w := appServe(t, appUser, nil, seed, http.MethodGet, "/api/v1/apps", "")
	var resp struct {
		Data struct {
			Apps []map[string]any `json:"apps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	by := map[string]map[string]any{}
	for _, a := range resp.Data.Apps {
		by[a["name"].(string)] = a
	}
	if by["sales"]["defaultForCaller"] != true {
		t.Errorf("sales.defaultForCaller = %v; want true cho vai role-1", by["sales"]["defaultForCaller"])
	}
	if by["board"]["defaultForCaller"] == true {
		t.Errorf("board không phải mặc định của vai này")
	}
	if _, leaked := by["sales"]["defaultRoleIds"]; leaked {
		t.Errorf("catalog không được lộ defaultRoleIds của vai khác")
	}
	if q, _ := by["sales"]["homeQueueIds"].([]any); len(q) != 1 {
		t.Errorf("catalog thiếu homeQueueIds: %v", by["sales"])
	}
	if _, ok := by["empty"]; !ok {
		t.Errorf("app chỉ có trang chủ dashboard phải hiện với nhân viên, got %v", w.Body.String())
	}
}

func TestAppHandler_CatalogSurvivesAppDeletedMidLoop(t *testing.T) {
	seed := func(st *stubAppStore) {
		seedCatalogApps(st)
		st.ghost = map[string]bool{"hidden": true}
	}
	w := appServe(t, appUser, nil, seed, http.MethodGet, "/api/v1/apps", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); app bị xoá giữa vòng lặp không được làm hỏng cả catalog", w.Code, w.Body.String())
	}
}

func TestAppHandler_CreateRejectsHomeKeys(t *testing.T) {
	for _, key := range []string{`"homeDashboardId":"dash-1"`, `"homeQueueIds":["q-1"]`, `"defaultRoleIds":["role-1"]`} {
		w := appServe(t, appAdmin, nil, nil, http.MethodPost, "/api/v1/admin/apps", `{"name":"x","label":"X",`+key+`}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "APP_HOME_ON_CREATE") {
			t.Errorf("%s: status = %d (%s); want 422 APP_HOME_ON_CREATE", key, w.Code, w.Body.String())
		}
	}
	_ = appmeta.App{}
}
