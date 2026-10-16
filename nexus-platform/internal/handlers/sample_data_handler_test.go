package handlers

// sample_data_handler_test.go — specs/064-preview-as-role-sample-data S9-4 (T073). The real
// SampleDataService over in-memory fakes, mounted through RegisterAPIRoutes with the operator
// tier, so every gate of the production chain (RequireAdmin -> operator tier -> PreviewReadOnly)
// is in the path of both routes.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

type sdStore struct{}

func (sdStore) CountSampleRecords(_ context.Context, _, _ string) (dynrepo.SampleCounts, error) {
	return dynrepo.SampleCounts{Live: 2, InBin: 1, BySource: map[string]int{"ai": 3}}, nil
}
func (sdStore) SampleEntityNames(_ context.Context, _ string) ([]string, error) {
	return []string{"crm_accounts"}, nil
}

func (sdStore) LiveRecordIDs(_ context.Context, _, _ string, _ int) ([]string, error) {
	return nil, nil
}
func (sdStore) ActiveUserIDs(_ context.Context, _ string, _ int) ([]string, error) { return nil, nil }

func (sdStore) SampleRoots(_ context.Context, _, _ string) ([]dynrepo.SampleRootRow, error) {
	return []dynrepo.SampleRootRow{{ID: "r1", BatchID: "b1"}, {ID: "r2", BatchID: "b1"}, {ID: "r3", BatchID: "b2"}}, nil
}
func (sdStore) ResetSequenceIfEmpty(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}

// sdPurger stands in for RecordService.PurgeSample.
type sdPurger struct {
	calls  int
	scope  services.RecordScope
	tenant string
}

func (p *sdPurger) PurgeSample(_ context.Context, scope services.RecordScope, tenantID, entity, id string, _ map[string]dynrepo.PurgePin) (*services.RecordPurgeReport, error) {
	p.calls++
	p.scope, p.tenant = scope, tenantID
	return &services.RecordPurgeReport{Entity: entity, ID: id, Records: []services.PurgedRecordRef{{Entity: entity, ID: id}}}, nil
}

type sdLister struct{}

func (sdLister) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	return []metadata.EntityDefinition{{ID: "e1", TenantID: tenantID, Name: "crm_accounts", Label: "Khách hàng"}}, nil
}

type sdCatalog struct{}

func (sdCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, nil
}

func (sdCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if name == "crm_accounts" {
		return &metadata.EntityDefinition{ID: "e1", TenantID: tenantID, Name: name, Label: "Khách hàng"}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

type sdWriter struct {
	calls   int
	gotMark bool
	scope   services.RecordScope
	tenant  string
	res     services.BulkCreateResult
	err     error
}

func (w *sdWriter) CreateBulk(ctx context.Context, scope services.RecordScope, tenantID, _, _ string, rows []map[string]any, mode services.BulkMode) (services.BulkCreateResult, error) {
	w.calls++
	_, w.gotMark = dynrepo.SampleMarkFrom(ctx)
	w.scope, w.tenant = scope, tenantID
	if w.err != nil {
		return services.BulkCreateResult{}, w.err
	}
	if w.res.Mode == "" {
		return services.BulkCreateResult{Mode: string(mode), Created: len(rows)}, nil
	}
	return w.res, nil
}

func newSampleHarness(t *testing.T, w *sdWriter) *gin.Engine {
	t.Helper()
	return newWipeHarness(t, w, &sdPurger{})
}

func newWipeHarness(t *testing.T, w *sdWriter, p *sdPurger) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	access := func(uid string, admin bool) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: uid, TenantID: "t1", IsAdmin: admin, SessionID: "sid-" + uid}, TokenType: auth.TokenTypeAccess}
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-op":    access("u-op", true),
		"tok-admin": access("u-adm", true), // legacy customer admin: not an operator
		"tok-staff": access("u-staff", false),
	}}
	svc := services.NewSampleDataService(sdStore{}, sdCatalog{}, w).WithWipe(p, sdLister{})
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{SampleData: NewSampleDataHandler(svc, zap.NewNop())}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())))
	return r
}

func sdDo(r *gin.Engine, method, path, token, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func TestSampleDataHandler_Summary(t *testing.T) {
	r := newSampleHarness(t, &sdWriter{})
	code, out := sdDo(r, http.MethodGet, "/api/v1/admin/sample-data", "tok-op", "")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, out)
	}
	data := out["data"].(map[string]any)
	ents := data["entities"].([]any)
	if data["total"].(float64) != 3 || len(ents) != 1 {
		t.Fatalf("data = %v, want total 3 over 1 entity", data)
	}
	e := ents[0].(map[string]any)
	by := e["bySource"].(map[string]any)
	if e["entity"] != "crm_accounts" || e["label"] != "Khách hàng" || e["count"].(float64) != 3 || e["live"].(float64) != 2 ||
		e["inBin"].(float64) != 1 || by["ai"].(float64) != 3 || by["generator"].(float64) != 0 || by["pack"].(float64) != 0 {
		t.Fatalf("entity row (camelCase contract) = %v", e)
	}
	if code, out := sdDo(r, http.MethodGet, "/api/v1/admin/sample-data?entities=nope", "tok-op", ""); code != http.StatusUnprocessableEntity || out["code"] != "SAMPLE_ENTITY_NOT_FOUND" {
		t.Fatalf("unknown entity: %d %v, want 422 SAMPLE_ENTITY_NOT_FOUND", code, out)
	}
}

func TestSampleDataHandler_CreateRecords(t *testing.T) {
	w := &sdWriter{}
	r := newSampleHarness(t, w)
	code, out := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/records/crm_accounts", "tok-op", `{"records":[{"name":"A"},{"name":"B"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d (%v), want 201 (atomic default, same as /:entity/bulk)", code, out)
	}
	if created := out["data"].(map[string]any)["created"].(float64); created != 2 {
		t.Fatalf("created = %v, want 2", created)
	}
	if !w.gotMark || w.tenant != "t1" || w.scope.UserID != "u-op" || !w.scope.IsAdmin {
		t.Fatalf("writer saw mark=%v tenant=%q scope=%+v; want a sample mark, tenant t1, admin u-op", w.gotMark, w.tenant, w.scope)
	}

	t.Run("partial mode is 200 like /:entity/bulk", func(t *testing.T) {
		w := &sdWriter{res: services.BulkCreateResult{Mode: "partial", Created: 1, Failed: 1}}
		code, _ := sdDo(newSampleHarness(t, w), http.MethodPost, "/api/v1/admin/sample-data/records/crm_accounts?mode=partial", "tok-op", `{"records":[{},{}]}`)
		if code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
	})
	t.Run("atomic abort is 422 like /:entity/bulk", func(t *testing.T) {
		w := &sdWriter{res: services.BulkCreateResult{Mode: "atomic", Failed: 1}}
		code, _ := sdDo(newSampleHarness(t, w), http.MethodPost, "/api/v1/admin/sample-data/records/crm_accounts", "tok-op", `{"records":[{}]}`)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422", code)
		}
	})
	t.Run("unknown entity is 422 SAMPLE_ENTITY_NOT_FOUND, writer untouched", func(t *testing.T) {
		w := &sdWriter{}
		code, out := sdDo(newSampleHarness(t, w), http.MethodPost, "/api/v1/admin/sample-data/records/nope", "tok-op", `{"records":[{}]}`)
		if code != http.StatusUnprocessableEntity || out["code"] != "SAMPLE_ENTITY_NOT_FOUND" || w.calls != 0 {
			t.Fatalf("%d %v calls=%d, want 422 SAMPLE_ENTITY_NOT_FOUND and no write", code, out, w.calls)
		}
	})
	t.Run("bad body is 400", func(t *testing.T) {
		if code, _ := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/records/crm_accounts", "tok-op", `nope`); code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", code)
		}
	})
}

func TestSampleDataHandler_Generate(t *testing.T) {
	w := &sdWriter{}
	r := newSampleHarness(t, w)
	code, out := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/generate", "tok-op", `{"entities":["crm_accounts"],"perEntity":5}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d (%v), want 201", code, out)
	}
	data := out["data"].(map[string]any)
	created := data["created"].([]any)
	if data["batchId"] == "" || len(created) != 1 || created[0].(map[string]any)["entity"] != "crm_accounts" || created[0].(map[string]any)["count"].(float64) != 5 {
		t.Fatalf("data = %v, want a batchId and crm_accounts created 5 (camelCase contract)", data)
	}
	for _, k := range []string{"failed", "skipped", "addedParents"} {
		if _, ok := data[k].([]any); !ok {
			t.Fatalf("%s = %#v, want a JSON array", k, data[k])
		}
	}
	if !w.gotMark || w.tenant != "t1" || w.scope.UserID != "u-op" || !w.scope.IsAdmin {
		t.Fatalf("writer saw mark=%v tenant=%q scope=%+v; want a sample mark, tenant t1, admin u-op", w.gotMark, w.tenant, w.scope)
	}

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"perEntity 21", `{"entities":["crm_accounts"],"perEntity":21}`, "SAMPLE_PER_ENTITY_INVALID", http.StatusUnprocessableEntity},
		{"perEntity missing", `{"entities":["crm_accounts"]}`, "SAMPLE_PER_ENTITY_INVALID", http.StatusUnprocessableEntity},
		{"unknown entity", `{"entities":["nope"],"perEntity":5}`, "SAMPLE_ENTITY_NOT_FOUND", http.StatusUnprocessableEntity},
		{"pack source before S9-6", `{"entities":["crm_accounts"],"perEntity":5,"source":"pack"}`, "SAMPLE_PACK_UNAVAILABLE", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sdWriter{}
			code, out := sdDo(newSampleHarness(t, w), http.MethodPost, "/api/v1/admin/sample-data/generate", "tok-op", tc.body)
			if code != tc.status || out["code"] != tc.code || w.calls != 0 {
				t.Fatalf("%d %v calls=%d, want %d %s and no write", code, out, w.calls, tc.status, tc.code)
			}
		})
	}
	t.Run("bad body is 400", func(t *testing.T) {
		if code, _ := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/generate", "tok-op", `nope`); code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", code)
		}
	})
}

// T112: POST /admin/sample-data/wipe — 200 shape, the confirmation contract, the caller's scope.
func TestSampleDataHandler_Wipe(t *testing.T) {
	p := &sdPurger{}
	r := newWipeHarness(t, &sdWriter{}, p)
	code, out := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/wipe", "tok-op", `{"expectTotal":3}`)
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, out)
	}
	data := out["data"].(map[string]any)
	deleted := data["deleted"].([]any)
	if len(deleted) != 1 || deleted[0].(map[string]any)["entity"] != "crm_accounts" || deleted[0].(map[string]any)["count"].(float64) != 3 {
		t.Fatalf("deleted = %v, want crm_accounts x3 (camelCase contract)", deleted)
	}
	for _, k := range []string{"kept", "sequencesReset"} {
		if arr, ok := data[k].([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s = %#v, want an empty JSON array", k, data[k])
		}
	}
	if data["blobsRemoved"].(float64) != 0 {
		t.Fatalf("blobsRemoved = %v", data["blobsRemoved"])
	}
	if p.calls != 3 || p.tenant != "t1" || p.scope.UserID != "u-op" || !p.scope.IsAdmin {
		t.Fatalf("purger saw calls=%d tenant=%q scope=%+v; want 3 roots, tenant t1, admin u-op", p.calls, p.tenant, p.scope)
	}

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"expectTotal missing", `{}`, "SAMPLE_WIPE_CONFIRM_REQUIRED", http.StatusUnprocessableEntity},
		{"expectTotal stale", `{"expectTotal":2}`, "SAMPLE_WIPE_STALE", http.StatusConflict},
		{"unknown entity", `{"entities":["nope"],"expectTotal":0}`, "SAMPLE_ENTITY_NOT_FOUND", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &sdPurger{}
			code, out := sdDo(newWipeHarness(t, &sdWriter{}, p), http.MethodPost, "/api/v1/admin/sample-data/wipe", "tok-op", tc.body)
			if code != tc.status || out["code"] != tc.code || p.calls != 0 {
				t.Fatalf("%d %v calls=%d, want %d %s and nothing purged", code, out, p.calls, tc.status, tc.code)
			}
			if tc.code == "SAMPLE_WIPE_STALE" && out["details"].(map[string]any)["current"].(float64) != 3 {
				t.Fatalf("details = %v, want current=3", out["details"])
			}
		})
	}
	t.Run("bad body is 400", func(t *testing.T) {
		if code, _ := sdDo(r, http.MethodPost, "/api/v1/admin/sample-data/wipe", "tok-op", `nope`); code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", code)
		}
	})
}

// Every route refuses everyone but a platform-operator admin, before any service call.
func TestSampleDataHandler_Gate(t *testing.T) {
	for _, tok := range []string{"tok-staff", "tok-admin"} {
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/admin/sample-data", ""},
			{http.MethodPost, "/api/v1/admin/sample-data/records/crm_accounts", `{"records":[{"name":"x"}]}`},
			{http.MethodPost, "/api/v1/admin/sample-data/generate", `{"entities":["crm_accounts"],"perEntity":5}`},
			{http.MethodPost, "/api/v1/admin/sample-data/wipe", `{"expectTotal":3}`},
		} {
			w, p := &sdWriter{}, &sdPurger{}
			code, _ := sdDo(newWipeHarness(t, w, p), rt.method, rt.path, tok, rt.body)
			if code != http.StatusForbidden || w.calls != 0 || p.calls != 0 {
				t.Errorf("%s %s %s: status %d writes %d purges %d, want 403 and nothing done", tok, rt.method, rt.path, code, w.calls, p.calls)
			}
		}
	}
	// The pair of the 403s above: the operator admin gets through on the same router (B21).
	if code, _ := sdDo(newSampleHarness(t, &sdWriter{}), http.MethodPost, "/api/v1/admin/sample-data/wipe", "tok-op", `{"expectTotal":3}`); code != http.StatusOK {
		t.Errorf("operator admin on wipe: status %d, want 200", code)
	}
}

// The service answers a concurrent batch with a SAMPLE_BUSY ValidationError; the shared status map must say 409.
func TestSampleDataHandler_BusyIs409(t *testing.T) {
	for _, code := range []string{"SAMPLE_BUSY", "SAMPLE_WIPE_STALE"} {
		if got := statusForCode(code); got != http.StatusConflict {
			t.Fatalf("%s -> %d, want 409", code, got)
		}
	}
}
