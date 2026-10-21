//go:build integration

// Issue #18 over the full production chain (Auth → Tenant → ACL → DynamicRecordHandler →
// RecordService → dynrepo) on a freshly migrated MySQL, with REAL logins of non-admin reps: a write
// whose edit/delete scope misses a record the caller CAN view answers 403 FORBIDDEN — the ACL
// stage's own body — while a record the caller cannot view (another rep's, another tenant's, a
// deleted one, an unknown id) keeps the 404 that hides it.
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
)

const (
	editScopeTenantAID   = "ed175c00-0000-0000-0000-0000000000a1"
	editScopeTenantBID   = "ed175c00-0000-0000-0000-0000000000b2"
	editScopeTenantASlug = "editscope-acme"
	editScopeEntity      = "it_edit_scope_tickets"
)

func TestRecordWrite_EditScope403_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, editScopeTenantAID, editScopeTenantASlug, "Acme edit scope")
	a4SeedTenant(t, gdb, editScopeTenantBID, "editscope-globex", "Globex edit scope")

	// "rep" is the demo's crm_sales_rep shape: sees every ticket, edits and deletes only their own.
	rep := a4SeedRole(t, gdb, editScopeTenantAID, "rep", []auth.RoleAction{
		grantLevel(editScopeEntity, "create", "all"), grantLevel(editScopeEntity, "view", "all"),
		grantLevel(editScopeEntity, "edit", "owner"), grantLevel(editScopeEntity, "delete", "owner"),
	})
	// "narrow" sees only their own tickets: someone else's is not there for them.
	narrow := a4SeedRole(t, gdb, editScopeTenantAID, "narrow", []auth.RoleAction{
		grantLevel(editScopeEntity, "view", "owner"), grantLevel(editScopeEntity, "edit", "owner"),
		grantLevel(editScopeEntity, "delete", "owner"),
	})
	// "reader" has no delete grant at all: its DELETE is refused by the ACL stage itself.
	reader := a4SeedRole(t, gdb, editScopeTenantAID, "reader", []auth.RoleAction{grantLevel(editScopeEntity, "view", "all")})

	a4SeedUser(t, gdb, editScopeTenantAID, "a@"+editScopeTenantASlug, false, &rep)
	a4SeedUser(t, gdb, editScopeTenantAID, "b@"+editScopeTenantASlug, false, &rep)
	a4SeedUser(t, gdb, editScopeTenantAID, "n@"+editScopeTenantASlug, false, &narrow)
	a4SeedUser(t, gdb, editScopeTenantAID, "r@"+editScopeTenantASlug, false, &reader)
	a4SeedUser(t, gdb, editScopeTenantAID, "admin@"+editScopeTenantASlug, true, nil)

	a4DropEntity(sx, []string{editScopeTenantAID}, editScopeEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{editScopeTenantAID}, editScopeEntity) })
	rt.createEntity(t, rt.login(t, editScopeTenantASlug, "admin@"+editScopeTenantASlug).Data.AccessToken, editScopeEntity)

	tok := func(email string) string {
		return rt.login(t, editScopeTenantASlug, email+"@"+editScopeTenantASlug).Data.AccessToken
	}
	aTok, bTok, nTok, rTok := tok("a"), tok("b"), tok("n"), tok("r")

	base := "/api/v1/" + editScopeEntity
	do := func(method, path, token string, body any) (int, string, []byte) {
		t.Helper()
		w := doAuthed(t, rt.engine, method, path, token, body, nil)
		return w.Code, shareCode(t, w.Body.Bytes()), w.Body.Bytes()
	}
	expect := func(name, method, path, token string, body any, wantStatus int, wantCode string) []byte {
		t.Helper()
		status, code, raw := do(method, path, token, body)
		if status != wantStatus || code != wantCode {
			t.Fatalf("%s: %s %s → %d %s; want %d %s", name, method, path, status, raw, wantStatus, wantCode)
		}
		return raw
	}
	var created recordCreateEnvelope
	_ = json.Unmarshal(expect("A creates", http.MethodPost, base, aTok, map[string]any{"title": "A's ticket"}, http.StatusCreated, ""), &created)
	rec := base + "/" + created.Data.ID
	title := func() string {
		t.Helper()
		var v string
		if err := sx.Get(&v, "SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.title')) FROM `obj_"+editScopeEntity+"` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL",
			editScopeTenantAID, created.Data.ID); err != nil {
			t.Fatalf("read A's ticket: %v", err)
		}
		return v
	}
	edit := map[string]any{"title": "hacked"}

	// B (same role, not the owner) reads A's ticket — and is refused every write with the ACL stage's 403.
	expect("B GET", http.MethodGet, rec, bTok, nil, http.StatusOK, "")
	put403 := expect("B PUT", http.MethodPut, rec, bTok, edit, http.StatusForbidden, "FORBIDDEN")
	del403 := expect("B DELETE", http.MethodDelete, rec, bTok, nil, http.StatusForbidden, "FORBIDDEN")
	acl403 := expect("reader DELETE (no delete grant: the ACL stage)", http.MethodDelete, rec, rTok, nil, http.StatusForbidden, "FORBIDDEN")
	if string(put403) != string(acl403) || string(del403) != string(acl403) {
		t.Fatalf("403 bodies differ from the ACL stage's:\n PUT    %s\n DELETE %s\n ACL    %s", put403, del403, acl403)
	}
	// The bulk doors share the single-record path: the row says FORBIDDEN, not RECORD_NOT_FOUND.
	for _, door := range []struct{ path, name string }{{"/bulk-update", "bulk-update"}, {"/bulk-delete", "bulk-delete"}} {
		body := map[string]any{"ids": []string{created.Data.ID}}
		if door.name == "bulk-update" {
			body["values"] = edit
		}
		var env struct {
			Data struct {
				Results []struct {
					OK   bool   `json:"ok"`
					Code string `json:"code"`
				} `json:"results"`
			} `json:"data"`
		}
		_ = json.Unmarshal(expect("B "+door.name, http.MethodPost, base+door.path, bTok, body, http.StatusOK, ""), &env)
		if len(env.Data.Results) != 1 || env.Data.Results[0].OK || env.Data.Results[0].Code != "FORBIDDEN" {
			t.Fatalf("B %s rows = %+v; want one FORBIDDEN row", door.name, env.Data.Results)
		}
	}
	if got := title(); got != "A's ticket" {
		t.Fatalf("A's ticket after B's writes: %q", got)
	}

	// N cannot view A's ticket: every answer is the 404 of a ticket that is not there.
	expect("N GET", http.MethodGet, rec, nTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("N PUT", http.MethodPut, rec, nTok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("N DELETE", http.MethodDelete, rec, nTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	// Nor do view=all callers learn of an id that is not theirs to see: unknown, or another tenant's.
	other := uuid.NewString()
	if _, err := sx.Exec("INSERT INTO `obj_"+editScopeEntity+"` (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)",
		other, editScopeTenantBID, uuid.NewString(), `{"title":"Globex"}`); err != nil {
		t.Fatalf("seed tenant B's ticket: %v", err)
	}
	expect("B PUT unknown id", http.MethodPut, base+"/"+uuid.NewString(), bTok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("B PUT another tenant's ticket", http.MethodPut, base+"/"+other, bTok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("B DELETE another tenant's ticket", http.MethodDelete, base+"/"+other, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")

	// The owner still edits it — then deletes it; once gone, B's writes are 404 again, not 403.
	expect("A PUT own", http.MethodPut, rec, aTok, map[string]any{"title": "A's ticket v2"}, http.StatusOK, "")
	expect("A DELETE own", http.MethodDelete, rec, aTok, nil, http.StatusNoContent, "")
	expect("B PUT deleted", http.MethodPut, rec, bTok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("B DELETE deleted", http.MethodDelete, rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
}
