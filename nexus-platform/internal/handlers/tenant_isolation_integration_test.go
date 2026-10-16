//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

const isoEntity = "it_iso"
const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// cleanupAllNamed removes an entity (rows for ALL tenants) and drops its table.
func cleanupAllNamed(db *sqlx.DB, name string) {
	db.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id WHERE e.name = ?`, name)
	db.Exec(`DELETE FROM sys_entities WHERE name = ?`, name)
	db.Exec("DROP TABLE IF EXISTS `obj_" + name + "`")
}

func doAs(t *testing.T, r *gin.Engine, method, path, tenant string, body any) *httptest.ResponseRecorder {
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
	// Authenticate as `tenant` via a verified Bearer access token — the tenant lives in the
	// signed claim, never in a client header. This is the production trust boundary the
	// isolation test exercises (an empty tenant would send no token).
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestTenantIsolation proves acceptance 9.1(2): with the SAME entity registered for two
// tenants (sharing one obj_* table, separated by tenant_id), tenant B cannot read tenant
// A's records — not by list, not by direct id.
func TestTenantIsolation(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupAllNamed(sx, isoEntity)
	t.Cleanup(func() { cleanupAllNamed(sx, isoEntity) })

	entityBody := map[string]any{
		"name":  isoEntity,
		"label": "Cô lập",
		"fields": []map[string]any{
			{"name": "name", "label": "Tên", "fieldType": "text", "isRequired": true},
		},
	}
	// Both tenants register the same entity (second create reuses the existing table).
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", tenantA, entityBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity for A: %d %s", w.Code, w.Body.String())
	}
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", tenantB, entityBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity for B: %d %s", w.Code, w.Body.String())
	}

	// Tenant A inserts a record.
	w := doAs(t, r, http.MethodPost, "/api/v1/"+isoEntity, tenantA, map[string]any{"name": "Bí mật của A"})
	if w.Code != http.StatusCreated {
		t.Fatalf("A insert: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	idA := created.Data.ID

	// Tenant B lists -> sees nothing (A's record is invisible).
	w = doAs(t, r, http.MethodGet, "/api/v1/"+isoEntity, tenantB, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("B list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Pagination.Total != 0 {
		t.Errorf("tenant B sees %d records; want 0 (isolation breached)", list.Pagination.Total)
	}

	// Tenant B tries to read A's record by id -> 404.
	if w := doAs(t, r, http.MethodGet, "/api/v1/"+isoEntity+"/"+idA, tenantB, nil); w.Code != http.StatusNotFound {
		t.Errorf("tenant B reading A's record: %d; want 404 (isolation breached)", w.Code)
	}

	// Sanity: tenant A still sees its own record.
	w = doAs(t, r, http.MethodGet, "/api/v1/"+isoEntity, tenantA, nil)
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Pagination.Total != 1 {
		t.Errorf("tenant A sees %d records; want 1", list.Pagination.Total)
	}
}
