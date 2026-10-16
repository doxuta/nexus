package handlers

// massop_handler_test.go — ENG-MASSOP HTTP-surface suite (no DB, no build tag). Complements
// massop_acl_wiring_test.go (which locks the fixed ACL actions): here a REAL RecordService over
// handler-local fakes runs behind the REAL RegisterAPIRoutes chain, proving the envelope contract
// of POST /:entity/bulk-update and /bulk-delete:
//   - happy path with MIXED ok/failed rows → 200 {data:{results[],updated|deleted,failed}},
//     per-row codes (RECORD_NOT_FOUND for an unknown/cross-tenant id, RECORD_DUPLICATE for a
//     unique collision) while siblings still land;
//   - cap exceeded → 422 BULK_TOO_MANY (+details.max); empty ids → 422 BULK_EMPTY;
//   - malformed JSON → 400 INVALID_BODY;
//   - a field-ACL-locked values key → per-row FIELD_FORBIDDEN (200), not a whole-batch reject.
// The stubVerifier / claimsFor fixtures come from acl_wiring_test.go; hiddenMetaCatalog /
// hiddenMetaStore from list_hidden_meta_test.go (same package).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// massopRowStore fails UpdateRecord/SoftDeleteRecord per id (scriptable) so the HTTP tests can
// produce mixed per-row outcomes without MySQL. Embeds hiddenMetaStore for the rest of the
// RecordStore surface (GetRecord → not-found keeps Update's before-image read best-effort-empty).
type massopRowStore struct {
	hiddenMetaStore
	failUpdate map[string]error
	failDelete map[string]error
}

func (s *massopRowStore) UpdateRecord(_ context.Context, _, _, id, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	return s.failUpdate[id]
}

func (s *massopRowStore) SoftDeleteRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) error {
	return s.failDelete[id]
}

// massopBothChecker grants role-1@t1 both edit and delete on contacts (row level "all") so one
// engine serves the bulk-update AND bulk-delete happy paths.
type massopBothChecker struct{}

func (c *massopBothChecker) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	if tenantID == "t1" && roleID == "role-1" && entity == "contacts" && (action == "edit" || action == "delete") {
		return true, "all", nil
	}
	return false, "", nil
}

// massopDenyPhoneACL is a field-ACL enforcer whose write guard locks the "phone" field for every
// non-admin — the minimal fake to prove a values-level FIELD_FORBIDDEN fails PER ROW.
type massopDenyPhoneACL struct{}

func (massopDenyPhoneACL) FilterReadFields(context.Context, string, string, string, bool, map[string]any) error {
	return nil
}
func (massopDenyPhoneACL) FilterReadFieldsBatch(context.Context, string, string, string, bool, []map[string]any) error {
	return nil
}
func (massopDenyPhoneACL) CheckWriteFields(_ context.Context, _, _, _ string, isAdmin bool, payload map[string]any) error {
	if isAdmin {
		return nil
	}
	if _, has := payload["phone"]; has {
		return &services.ValidationError{
			Code:    "FIELD_FORBIDDEN",
			Msg:     "bạn không có quyền sửa field \"phone\"",
			Field:   "phone",
			Details: map[string]any{"field": "phone"},
		}
	}
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (massopDenyPhoneACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return massopDenyPhoneACL{}.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}
func (massopDenyPhoneACL) AssertFieldVisible(context.Context, string, string, string, bool, string) error {
	return nil
}
func (massopDenyPhoneACL) AssertFieldsVisible(context.Context, string, string, string, bool, []string) error {
	return nil
}
func (massopDenyPhoneACL) HiddenFields(context.Context, string, string, string, bool) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// massopEnvelope decodes the {data:{...}} body of a mass-op response.
type massopEnvelope struct {
	Data struct {
		Results []struct {
			ID      string         `json:"id"`
			OK      bool           `json:"ok"`
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"results"`
		Updated int `json:"updated"`
		Deleted int `json:"deleted"`
		Failed  int `json:"failed"`
	} `json:"data"`
}

// buildMassOpEngine mounts the production chain (RegisterAPIRoutes: Auth → Tenant →
// RequireACLAction(edit|delete) → handler) with a REAL RecordService over the given store.
// A non-nil fieldACL is threaded via WithFieldACL.
func buildMassOpEngine(store services.RecordStore, withDenyPhoneACL bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "phone", FieldType: "text"},
		},
	}
	svc := services.NewRecordService(cat, store)
	if withDenyPhoneACL {
		svc = svc.WithFieldACL(massopDenyPhoneACL{})
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"member": claimsFor("t1", "role-1", false),
	}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, &massopBothChecker{}, APIHandlers{
		Record: NewDynamicRecordHandler(svc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

func doMassOpJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer member")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// Mixed batch: one id hits a unique collision (RECORD_DUPLICATE + details.field), one is
// unknown (RECORD_NOT_FOUND), the rest land — 200 with per-row outcomes.
func TestMassOpHTTP_BulkUpdate_MixedRows(t *testing.T) {
	store := &massopRowStore{
		failUpdate: map[string]error{
			"dup":  &dynrepo.DuplicateUniqueError{IndexName: "uq_obj_contacts_phone"},
			"gone": dynrepo.ErrRecordNotFound,
		},
	}
	r := buildMassOpEngine(store, false)

	rec := doMassOpJSON(t, r, "/api/v1/contacts/bulk-update",
		`{"ids":["a","dup","gone","b"],"values":{"name":"Mass"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env massopEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if env.Data.Updated != 2 || env.Data.Failed != 2 || len(env.Data.Results) != 4 {
		t.Fatalf("envelope = %+v; want updated=2 failed=2 len=4", env.Data)
	}
	codes := map[string]string{}
	oks := map[string]bool{}
	var dupDetails map[string]any
	for _, row := range env.Data.Results {
		codes[row.ID] = row.Code
		oks[row.ID] = row.OK
		if row.ID == "dup" {
			dupDetails = row.Details
		}
	}
	if !oks["a"] || !oks["b"] {
		t.Fatalf("rows a/b should be ok: %+v", oks)
	}
	if codes["dup"] != "RECORD_DUPLICATE" {
		t.Fatalf("dup row code = %q, want RECORD_DUPLICATE", codes["dup"])
	}
	if codes["gone"] != "RECORD_NOT_FOUND" {
		t.Fatalf("gone row code = %q, want RECORD_NOT_FOUND", codes["gone"])
	}
	// details.field must surface for the duplicate row (FE highlights the column).
	if f, _ := dupDetails["field"].(string); f != "phone" {
		t.Fatalf("dup details.field = %v, want phone", dupDetails)
	}
}

// Bulk-delete: one row not-found, siblings soft-delete — 200 {deleted, failed}.
func TestMassOpHTTP_BulkDelete_MixedRows(t *testing.T) {
	store := &massopRowStore{failDelete: map[string]error{"gone": dynrepo.ErrRecordNotFound}}
	r := buildMassOpEngine(store, false)

	rec := doMassOpJSON(t, r, "/api/v1/contacts/bulk-delete", `{"ids":["a","gone","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env massopEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if env.Data.Deleted != 2 || env.Data.Failed != 1 || len(env.Data.Results) != 3 {
		t.Fatalf("envelope = %+v; want deleted=2 failed=1 len=3", env.Data)
	}
}

// Request-level guards surface as clean 422 JSON: over-cap → BULK_TOO_MANY (+details.max),
// empty ids → BULK_EMPTY, empty values → BULK_VALUES_EMPTY; malformed JSON → 400 INVALID_BODY.
func TestMassOpHTTP_RequestLevelGuards(t *testing.T) {
	ids := make([]string, 501)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	over, err := json.Marshal(map[string]any{"ids": ids, "values": map[string]any{"name": "X"}})
	if err != nil {
		t.Fatalf("marshal oversized body: %v", err)
	}

	tests := []struct {
		name, path, body string
		wantStatus       int
		wantCode         string
	}{
		{"quá cap", "/api/v1/contacts/bulk-update", string(over), http.StatusUnprocessableEntity, "BULK_TOO_MANY"},
		{"ids rỗng", "/api/v1/contacts/bulk-update", `{"ids":[],"values":{"name":"X"}}`, http.StatusUnprocessableEntity, "BULK_EMPTY"},
		{"values rỗng", "/api/v1/contacts/bulk-update", `{"ids":["a"],"values":{}}`, http.StatusUnprocessableEntity, "BULK_VALUES_EMPTY"},
		{"delete ids rỗng", "/api/v1/contacts/bulk-delete", `{"ids":[]}`, http.StatusUnprocessableEntity, "BULK_EMPTY"},
		{"JSON hỏng", "/api/v1/contacts/bulk-update", `{"ids":`, http.StatusBadRequest, "INVALID_BODY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := buildMassOpEngine(&massopRowStore{}, false)
			rec := doMassOpJSON(t, r, tt.path, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var errResp struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
			}
			if errResp.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q (body=%s)", errResp.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == "BULK_TOO_MANY" {
				if max, _ := errResp.Details["max"].(float64); int(max) != 500 {
					t.Fatalf("details.max = %v, want 500", errResp.Details)
				}
			}
		})
	}
}

// A values key the role may not edit (field-ACL write guard) fails per-row — 200 with
// FIELD_FORBIDDEN on every row, not a whole-batch 403/422 (per-row parity with a single PUT).
func TestMassOpHTTP_FieldACLDefectFailsPerRow(t *testing.T) {
	r := buildMassOpEngine(&massopRowStore{}, true)

	rec := doMassOpJSON(t, r, "/api/v1/contacts/bulk-update", `{"ids":["a","b"],"values":{"phone":"0900"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 per-row (body=%s)", rec.Code, rec.Body.String())
	}
	var env massopEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Updated != 0 || env.Data.Failed != 2 || len(env.Data.Results) != 2 {
		t.Fatalf("envelope = %+v; want 0 updated / 2 failed / 2 rows", env.Data)
	}
	for _, row := range env.Data.Results {
		if row.OK || row.Code != "FIELD_FORBIDDEN" {
			t.Fatalf("row %+v; want failed FIELD_FORBIDDEN", row)
		}
	}
}
