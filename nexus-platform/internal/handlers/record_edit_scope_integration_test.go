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
	uN := a4SeedUser(t, gdb, editScopeTenantAID, "n@"+editScopeTenantASlug, false, &narrow)
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
	// Share management (edit-gated) refuses B the same way, with the same bytes.
	shareList := expect("B lists A's shares", http.MethodGet, rec+"/shares", bTok, nil, http.StatusForbidden, "FORBIDDEN")
	shareAdd := expect("B shares A's ticket", http.MethodPost, rec+"/shares", bTok, map[string]any{"userId": uN}, http.StatusForbidden, "FORBIDDEN")
	for name, got := range map[string][]byte{"PUT": put403, "DELETE": del403, "GET shares": shareList, "POST shares": shareAdd} {
		if string(got) != string(acl403) {
			t.Fatalf("%s 403 body differs from the ACL stage's:\n got %s\n ACL %s", name, got, acl403)
		}
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

const (
	teamScopeTenantID   = "ed175c00-0000-0000-0000-0000000000c3"
	teamScopeTenantSlug = "editscope-team"
	teamScopeEntity     = "it_edit_scope_team_tickets"
)

// TestRecordWrite_EditScopeTeamView_OverHTTP pins the scope the #18 check reads under: a view=team,
// edit=own rep gets 403 on a teammate's record (they can read it) but the 404 of an unknown id —
// byte for byte — on another team's record, which their GET hides. A check that read any wider than
// the caller's view scope would turn that 404 into an existence oracle.
func TestRecordWrite_EditScopeTeamView_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, teamScopeTenantID, teamScopeTenantSlug, "Acme team scope")
	teamX, teamY := uuid.NewString(), uuid.NewString()
	a5SeedTeam(t, gdb, teamX, teamScopeTenantID, "Team X")
	a5SeedTeam(t, gdb, teamY, teamScopeTenantID, "Team Y")

	rep := a4SeedRole(t, gdb, teamScopeTenantID, "teamrep", []auth.RoleAction{
		grantLevel(teamScopeEntity, "create", "all"), grantLevel(teamScopeEntity, "view", "team"),
		grantLevel(teamScopeEntity, "edit", "owner"), grantLevel(teamScopeEntity, "delete", "owner"),
	})
	mail := func(who string) string { return who + "@" + teamScopeTenantSlug }
	x1 := a4SeedUser(t, gdb, teamScopeTenantID, mail("x1"), false, &rep)
	x2 := a4SeedUser(t, gdb, teamScopeTenantID, mail("x2"), false, &rep)
	y1 := a4SeedUser(t, gdb, teamScopeTenantID, mail("y1"), false, &rep)
	a4SeedUser(t, gdb, teamScopeTenantID, mail("admin"), true, nil)
	// Memberships before any record: a record's team set is its creator's teams at insert time.
	a5SeedMembership(t, gdb, x1, teamX)
	a5SeedMembership(t, gdb, x2, teamX)
	a5SeedMembership(t, gdb, y1, teamY)

	a4DropEntity(sx, []string{teamScopeTenantID}, teamScopeEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{teamScopeTenantID}, teamScopeEntity) })
	rt.createEntity(t, rt.login(t, teamScopeTenantSlug, mail("admin")).Data.AccessToken, teamScopeEntity)
	tok := func(who string) string { return rt.login(t, teamScopeTenantSlug, mail(who)).Data.AccessToken }
	x1Tok, x2Tok, y1Tok := tok("x1"), tok("x2"), tok("y1")

	base := "/api/v1/" + teamScopeEntity
	call := func(name, method, path, token string, body any, wantStatus int, wantCode string) []byte {
		t.Helper()
		w := doAuthed(t, rt.engine, method, path, token, body, nil)
		if w.Code != wantStatus || shareCode(t, w.Body.Bytes()) != wantCode {
			t.Fatalf("%s: %s %s → %d %s; want %d %s", name, method, path, w.Code, w.Body.String(), wantStatus, wantCode)
		}
		return w.Body.Bytes()
	}
	create := func(token, title string) string {
		t.Helper()
		var env recordCreateEnvelope
		_ = json.Unmarshal(call("create "+title, http.MethodPost, base, token, map[string]any{"title": title}, http.StatusCreated, ""), &env)
		return base + "/" + env.Data.ID
	}
	yRec, xRec := create(y1Tok, "Team Y's"), create(x2Tok, "Team X's")
	edit := map[string]any{"title": "hacked"}

	// Another team's record: hidden from x1's GET, so every write is the unknown id's 404, byte for byte.
	call("x1 GET Y's", http.MethodGet, yRec, x1Tok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	unknown := base + "/" + uuid.NewString()
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		hidden := call("x1 "+m+" Y's", m, yRec, x1Tok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
		none := call("x1 "+m+" unknown id", m, unknown, x1Tok, edit, http.StatusNotFound, "RECORD_NOT_FOUND")
		if string(hidden) != string(none) {
			t.Fatalf("x1 %s on another team's record differs from an unknown id:\n %s\n %s", m, hidden, none)
		}
	}
	// A teammate's record: readable, not theirs to change → 403.
	call("x1 GET X's", http.MethodGet, xRec, x1Tok, nil, http.StatusOK, "")
	call("x1 PUT X's", http.MethodPut, xRec, x1Tok, edit, http.StatusForbidden, "FORBIDDEN")
	call("x1 DELETE X's", http.MethodDelete, xRec, x1Tok, nil, http.StatusForbidden, "FORBIDDEN")

}
