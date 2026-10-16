//go:build integration

// "Ẩn" must mean NOT SERVED. GET /metadata/layouts/:entity and GET /metadata/fields/:entity used to
// hand a non-admin the very field its role cannot view (can_view=0): the record read strips the
// VALUE, but the layout still placed the field and the field list still described it, so the edit
// form rendered it empty and resent it as null on save — the FIELD_FORBIDDEN-on-every-save bug
// that TestFLS_Integration_HiddenFieldFormResendSaves (services) pins from the write side. This
// suite drives the PRODUCTION router (RegisterAPIRoutes → AuthMiddleware → RequireTenant → handler)
// over a freshly-migrated throwaway DB with the REAL FieldACLService over auth.FieldACLRepository,
// so it proves the wiring, the tenant-pinned grant JOIN and the strip together.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/handlers/ -run TestFLSHiddenMetadata -v
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const flsMetaEntity = "fls_accounts"

// newFLSMetaRouter mounts the two metadata reads under test over the given DB with the given
// field-ACL resolver: the REAL enforcer for the strip proof, an erroring fake for fail-closed.
func newFLSMetaRouter(t *testing.T, gdb *gorm.DB, resolver fieldViewChecker) *gin.Engine {
	t.Helper()
	metaRepo := metadata.NewRepository(gdb)
	layoutSvc := services.NewLayoutService(metaRepo, metadata.NewLayoutRepository(gdb), zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	mh := NewMetadataHandler(gdb, zap.NewNop()).WithFieldACL(resolver)
	// field-lock-signal: the locked-set seam is a separate, narrower optional interface (nil = no
	// readOnly key), so a resolver that only knows the hidden set keeps the pre-fix behaviour.
	if locks, ok := resolver.(fieldLockChecker); ok {
		mh = mh.WithFieldLocks(locks)
	}
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Metadata: mh,
		Layout:   NewAdminLayoutHandler(layoutSvc, zap.NewNop()).WithFieldACL(resolver),
	}, zap.NewNop())
	return r
}

// flsMetaSeed stands up tenant + two roles + an entity (name/revenue/notes) + a hidden grant on
// 'revenue' for the first role + a STORED edit layout that references 'revenue' both as an object
// cell and as the only cell of its own section (so the strip must drop a cell AND an emptied
// section). detail and list stay default-generated (string cells / list columns), so both cell
// shapes are covered.
func flsMetaSeed(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) (tenantID, hiddenRole, freeRole string) {
	t.Helper()
	tenantID = uuid.NewString()
	a4SeedTenant(t, gdb, tenantID, "fls-meta", "FLS Meta")
	hiddenRole = a4SeedRole(t, gdb, tenantID, "fls-sales", nil)
	freeRole = a4SeedRole(t, gdb, tenantID, "fls-free", nil)

	metaRepo := metadata.NewRepository(gdb)
	if _, _, err := services.NewEntityService(dynrepo.New(sx), metaRepo).CreateEntity(context.Background(), tenantID, services.CreateEntityInput{
		Name:  flsMetaEntity,
		Label: "FLS Accounts",
		Fields: []services.FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "revenue", Label: "Revenue", FieldType: "number"},
			{Name: "notes", Label: "Notes", FieldType: "rich_text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if _, err := sx.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'revenue', 0, 0, NOW(3), NOW(3))`,
		uuid.NewString(), tenantID, hiddenRole, flsMetaEntity); err != nil {
		t.Fatalf("seed sys_field_acl: %v", err)
	}
	// field-lock-signal: 'notes' is visible but read-only (can_view=1, can_edit=0) for the same role.
	if _, err := sx.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'notes', 1, 0, NOW(3), NOW(3))`,
		uuid.NewString(), tenantID, hiddenRole, flsMetaEntity); err != nil {
		t.Fatalf("seed sys_field_acl (locked): %v", err)
	}
	if _, err := sx.Exec(
		`INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, layout_json) VALUES (?, ?, ?, 'edit', ?)`,
		uuid.NewString(), tenantID, flsMetaEntity,
		`{"version":1,"sections":[{"title":"Thông tin","columns":2,"fields":["name",{"field":"revenue","fullWidth":true},"notes"]},{"title":"Chỉ số","columns":1,"fields":[{"field":"revenue"}]}]}`); err != nil {
		t.Fatalf("seed sys_layouts: %v", err)
	}
	return tenantID, hiddenRole, freeRole
}

func flsMetaToken(t *testing.T, tenantID, roleID string, isAdmin bool) string {
	t.Helper()
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{UserID: uuid.NewString(), TenantID: tenantID, RoleID: roleID, IsAdmin: isAdmin})
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	return tok
}

// flsFieldNames decodes GET /metadata/fields/:entity into the served field names.
func flsFieldNames(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var resp struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode fields: %v (%s)", err, body)
	}
	out := make(map[string]bool, len(resp.Data))
	for _, f := range resp.Data {
		out[f.Name] = true
	}
	return out
}

func TestFLSHiddenMetadata_StrippedForRoleServedForAdmin(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID, hiddenRole, freeRole := flsMetaSeed(t, gdb, sx)
	r := newFLSMetaRouter(t, gdb, services.NewFieldACLService(auth.NewFieldACLRepository(gdb)))

	layoutsPath := "/api/v1/metadata/layouts/" + flsMetaEntity
	fieldsPath := "/api/v1/metadata/fields/" + flsMetaEntity

	// (1) The role with 'revenue' hidden: neither read names the field, anywhere.
	member := flsMetaToken(t, tenantID, hiddenRole, false)
	w := doAuthed(t, r, "GET", layoutsPath, member, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("member GET layouts: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "revenue") {
		t.Fatalf("hidden 'revenue' leaked through GET /metadata/layouts for the role: %s", body)
	} else if strings.Contains(body, "Chỉ số") {
		t.Fatalf("a section left with no visible field must be dropped, got: %s", body)
	} else if !strings.Contains(body, `"name"`) || !strings.Contains(body, `"notes"`) {
		t.Fatalf("visible fields must still be placed: %s", body)
	}
	w = doAuthed(t, r, "GET", fieldsPath, member, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("member GET fields: %d %s", w.Code, w.Body.String())
	}
	if names := flsFieldNames(t, w.Body.Bytes()); names["revenue"] || !names["name"] || !names["notes"] {
		t.Fatalf("member GET fields = %v; want name+notes without revenue", names)
	}

	// (2) A role with NO restriction keeps seeing everything (default-allow, no over-strip).
	free := flsMetaToken(t, tenantID, freeRole, false)
	if w = doAuthed(t, r, "GET", layoutsPath, free, nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "revenue") {
		t.Fatalf("unrestricted role GET layouts = %d %s; want 200 with revenue", w.Code, w.Body.String())
	}
	if w = doAuthed(t, r, "GET", fieldsPath, free, nil, nil); w.Code != http.StatusOK || !flsFieldNames(t, w.Body.Bytes())["revenue"] {
		t.Fatalf("unrestricted role GET fields = %d %s; want 200 with revenue", w.Code, w.Body.String())
	}

	// (3) Admin bypasses field-level security entirely.
	admin := flsMetaToken(t, tenantID, "", true)
	if w = doAuthed(t, r, "GET", layoutsPath, admin, nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "revenue") || !strings.Contains(w.Body.String(), "Chỉ số") {
		t.Fatalf("admin GET layouts = %d %s; want 200 with revenue + its section", w.Code, w.Body.String())
	}
	if w = doAuthed(t, r, "GET", fieldsPath, admin, nil, nil); w.Code != http.StatusOK || !flsFieldNames(t, w.Body.Bytes())["revenue"] {
		t.Fatalf("admin GET fields = %d %s; want 200 with revenue", w.Code, w.Body.String())
	}
}

// flsErrResolver models a field-grant load failure (sys_field_acl unreachable).
type flsErrResolver struct{}

func (flsErrResolver) HiddenFields(context.Context, string, string, string, bool) (map[string]struct{}, error) {
	return nil, errors.New("sys_field_acl unreachable")
}

// A grant-load failure must never widen what a non-admin sees: both reads fail CLOSED with the
// same refusal their entity-ACL neighbour gives (404 for the layout, 500 for the field list), and
// the admin — who bypasses field-level security — is unaffected.
func TestFLSHiddenMetadata_LoadErrorFailsClosed(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID, hiddenRole, _ := flsMetaSeed(t, gdb, sx)
	r := newFLSMetaRouter(t, gdb, flsErrResolver{})

	member := flsMetaToken(t, tenantID, hiddenRole, false)
	if w := doAuthed(t, r, "GET", "/api/v1/metadata/layouts/"+flsMetaEntity, member, nil, nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "ENTITY_NOT_FOUND") {
		t.Fatalf("member GET layouts on grant-load error = %d %s; want 404 ENTITY_NOT_FOUND (fail closed)", w.Code, w.Body.String())
	}
	if w := doAuthed(t, r, "GET", "/api/v1/metadata/fields/"+flsMetaEntity, member, nil, nil); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "METADATA_QUERY_FAILED") {
		t.Fatalf("member GET fields on grant-load error = %d %s; want 500 METADATA_QUERY_FAILED (fail closed)", w.Code, w.Body.String())
	}

	admin := flsMetaToken(t, tenantID, "", true)
	for _, p := range []string{"/api/v1/metadata/layouts/" + flsMetaEntity, "/api/v1/metadata/fields/" + flsMetaEntity} {
		if w := doAuthed(t, r, "GET", p, admin, nil, nil); w.Code != http.StatusOK {
			t.Fatalf("admin GET %s must bypass the field-ACL load = %d %s", p, w.Code, w.Body.String())
		}
	}
}

// ── field-lock-signal (DAILY-QUEUE #12) ─────────────────────────────────────────────────────────
//
// "Khoá" (can_edit=0, can_view=1) used to be invisible to the form: the field list described the
// field like any other, the form drew an input, and the 403 FIELD_FORBIDDEN only arrived on save.
// GET /metadata/fields/:entity now carries a per-CALLER `readOnly: true` on such a field. The key is
// ADDITIVE and only ever present when true — an admin or an unrestricted role gets the byte-identical
// list of before — and a hidden field is still not served at all (strip hidden BEFORE marking
// locked: loadRestrictions puts every hidden field in the locked set too).

// flsFieldReadOnly decodes GET /metadata/fields/:entity into name → whether the readOnly KEY is
// present (a `readOnly:false` on the wire would be a contract break, so presence is what is pinned).
func flsFieldReadOnly(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var resp struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode fields: %v (%s)", err, body)
	}
	out := make(map[string]bool, len(resp.Data))
	for _, f := range resp.Data {
		var name string
		if err := json.Unmarshal(f["name"], &name); err != nil {
			t.Fatalf("decode field name: %v", err)
		}
		raw, present := f["readOnly"]
		if present && string(raw) != "true" {
			t.Fatalf("field %q carries readOnly=%s; the key must be absent unless true", name, raw)
		}
		out[name] = present
	}
	return out
}

func TestFLSLockedMetadata_ReadOnlyKeyPerCaller(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID, lockedRole, freeRole := flsMetaSeed(t, gdb, sx)
	r := newFLSMetaRouter(t, gdb, services.NewFieldACLService(auth.NewFieldACLRepository(gdb)))
	fieldsPath := "/api/v1/metadata/fields/" + flsMetaEntity

	// (1) The role with 'notes' locked (and 'revenue' hidden): notes flagged, name not, revenue absent.
	member := flsMetaToken(t, tenantID, lockedRole, false)
	w := doAuthed(t, r, "GET", fieldsPath, member, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("member GET fields: %d %s", w.Code, w.Body.String())
	}
	ro := flsFieldReadOnly(t, w.Body.Bytes())
	if !ro["notes"] {
		t.Fatalf("locked 'notes' must carry readOnly:true for the role: %s", w.Body.String())
	}
	if ro["name"] {
		t.Fatalf("unrestricted 'name' must NOT carry readOnly: %s", w.Body.String())
	}
	if _, served := ro["revenue"]; served {
		t.Fatalf("hidden 'revenue' must still not be served at all (strip hidden BEFORE marking locked): %s", w.Body.String())
	}

	// (2) A role with no restriction: no readOnly key anywhere (default-allow, byte-identical list).
	free := flsMetaToken(t, tenantID, freeRole, false)
	w = doAuthed(t, r, "GET", fieldsPath, free, nil, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "readOnly") {
		t.Fatalf("unrestricted role GET fields = %d %s; want 200 with no readOnly key", w.Code, w.Body.String())
	}

	// (3) Admin bypasses: sees every field, none flagged.
	admin := flsMetaToken(t, tenantID, "", true)
	w = doAuthed(t, r, "GET", fieldsPath, admin, nil, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "readOnly") || !flsFieldNames(t, w.Body.Bytes())["revenue"] {
		t.Fatalf("admin GET fields = %d %s; want 200, all fields, no readOnly key", w.Code, w.Body.String())
	}
}

// flsLockErrResolver knows the hidden set (nothing hidden) but fails to load the locked set — the
// locked-signal path must fail CLOSED on its own, not only when the hidden load fails first.
type flsLockErrResolver struct{}

func (flsLockErrResolver) HiddenFields(context.Context, string, string, string, bool) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (flsLockErrResolver) LockedFields(context.Context, string, string, string, bool) (map[string]struct{}, error) {
	return nil, errors.New("sys_field_acl unreachable")
}

// A field list that cannot say which fields are locked must not claim they are all editable: the
// form would draw inputs and the user would meet the 403 on save — the very bug this key removes.
// Same refusal as the hidden load (500 METADATA_QUERY_FAILED); admin unaffected.
func TestFLSLockedMetadata_LoadErrorFailsClosed(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID, lockedRole, _ := flsMetaSeed(t, gdb, sx)
	r := newFLSMetaRouter(t, gdb, flsLockErrResolver{})
	fieldsPath := "/api/v1/metadata/fields/" + flsMetaEntity

	member := flsMetaToken(t, tenantID, lockedRole, false)
	if w := doAuthed(t, r, "GET", fieldsPath, member, nil, nil); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "METADATA_QUERY_FAILED") {
		t.Fatalf("member GET fields on locked-load error = %d %s; want 500 METADATA_QUERY_FAILED (fail closed)", w.Code, w.Body.String())
	}
	admin := flsMetaToken(t, tenantID, "", true)
	if w := doAuthed(t, r, "GET", fieldsPath, admin, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("admin GET fields must bypass the locked-set load = %d %s", w.Code, w.Body.String())
	}
}
