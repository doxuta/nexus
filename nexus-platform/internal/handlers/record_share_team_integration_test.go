//go:build integration

// Sharing a record with a whole TEAM over the FULL production chain (DAILY-QUEUE #224
// record-manual-share-team, specs/051-record-manual-share): real logins of non-admin users, the real
// ACL stage, shares.Service → RecordService → dynrepo on a freshly migrated MySQL. The dynrepo suite
// proves the SQL; this proves what a user actually hits: the team's members open the record, the rest
// of the tenant still gets 404, the share is read-only, and leaving the team revokes it at once.
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
)

const (
	teamShareTenantID   = "7e4a0000-0000-0000-0000-0000000000a1"
	teamShareForeignID  = "7e4a0000-0000-0000-0000-0000000000b2"
	teamShareTenantSlug = "teamshare-acme"
	teamShareForeignSlg = "teamshare-globex"
	teamShareEntity     = "it_team_share_deals"
)

func TestRecordShare_TeamOverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, teamShareTenantID, teamShareTenantSlug, "Acme team share")
	a4SeedTenant(t, gdb, teamShareForeignID, teamShareForeignSlg, "Globex team share")

	rep := a4SeedRole(t, gdb, teamShareTenantID, "rep", []auth.RoleAction{
		grantLevel(teamShareEntity, "create", "all"), grantLevel(teamShareEntity, "view", "owner"),
		grantLevel(teamShareEntity, "edit", "owner"), grantLevel(teamShareEntity, "delete", "owner"),
	})
	uA := a4SeedUser(t, gdb, teamShareTenantID, "a@"+teamShareTenantSlug, false, &rep)
	uB := a4SeedUser(t, gdb, teamShareTenantID, "b@"+teamShareTenantSlug, false, &rep) // member of team X
	a4SeedUser(t, gdb, teamShareTenantID, "d@"+teamShareTenantSlug, false, &rep)       // on no team
	a4SeedUser(t, gdb, teamShareTenantID, "admin@"+teamShareTenantSlug, true, nil)

	teamX, teamForeign := uuid.NewString(), uuid.NewString()
	for _, row := range [][]string{{teamX, teamShareTenantID, "Phòng Kinh doanh"}, {teamForeign, teamShareForeignID, "Nhóm nước ngoài"}} {
		if _, err := sx.Exec(`INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed team: %v", err)
		}
	}
	if _, err := sx.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), uB, teamX); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	a4DropEntity(sx, []string{teamShareTenantID}, teamShareEntity)
	t.Cleanup(func() {
		a4DropEntity(sx, []string{teamShareTenantID}, teamShareEntity)
		sx.Exec("DELETE FROM sys_record_shares WHERE tenant_id IN (?, ?)", teamShareTenantID, teamShareForeignID)
		sx.Exec("DELETE FROM team_memberships WHERE team_id IN (?, ?)", teamX, teamForeign)
		sx.Exec("DELETE FROM teams WHERE id IN (?, ?)", teamX, teamForeign)
	})
	rt.createEntity(t, rt.login(t, teamShareTenantSlug, "admin@"+teamShareTenantSlug).Data.AccessToken, teamShareEntity)

	tok := func(email string) string { return rt.login(t, teamShareTenantSlug, email).Data.AccessToken }
	aTok, bTok, dTok := tok("a@"+teamShareTenantSlug), tok("b@"+teamShareTenantSlug), tok("d@"+teamShareTenantSlug)

	base := "/api/v1/" + teamShareEntity
	w := doAuthed(t, rt.engine, http.MethodPost, base, aTok, map[string]any{"title": "Hợp đồng nhóm"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("A create: %d %s", w.Code, w.Body.String())
	}
	var created recordCreateEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	rec := created.Data.ID
	shares := base + "/" + rec + "/shares"

	expect := func(name, method, path, token string, body any, wantStatus int, wantCode string) []byte {
		t.Helper()
		w := doAuthed(t, rt.engine, method, path, token, body, nil)
		if w.Code != wantStatus || (wantCode != "" && shareCode(t, w.Body.Bytes()) != wantCode) {
			t.Fatalf("%s: %s %s -> %d %s; want %d %s", name, method, path, w.Code, w.Body.String(), wantStatus, wantCode)
		}
		return w.Body.Bytes()
	}
	listTotal := func(token string) int {
		t.Helper()
		var env struct {
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		_ = json.Unmarshal(expect("list", http.MethodGet, base, token, nil, http.StatusOK, ""), &env)
		return env.Pagination.Total
	}

	expect("B before share", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")

	// Grantee validation: one of userId / teamId, and the team must be a team of THIS tenant.
	expect("both grantees", http.MethodPost, shares, aTok, map[string]any{"userId": uB, "teamId": teamX}, http.StatusUnprocessableEntity, "RECORD_SHARE_GRANTEE_AMBIGUOUS")
	expect("unknown team", http.MethodPost, shares, aTok, map[string]any{"teamId": uuid.NewString()}, http.StatusUnprocessableEntity, "RECORD_SHARE_TEAM_INVALID")
	expect("another tenant's team", http.MethodPost, shares, aTok, map[string]any{"teamId": teamForeign}, http.StatusUnprocessableEntity, "RECORD_SHARE_TEAM_INVALID")

	// A shares with team X (twice: one live row).
	body := expect("A shares with the team", http.MethodPost, shares, aTok, map[string]any{"teamId": teamX}, http.StatusCreated, "")
	var one struct {
		Data struct{ UserID, TeamID, TeamName, SharedBy string } `json:"data"`
	}
	_ = json.Unmarshal(body, &one)
	if one.Data.TeamID != teamX || one.Data.UserID != "" || one.Data.SharedBy != uA || one.Data.TeamName != "Phòng Kinh doanh" {
		t.Fatalf("share body = %+v", one.Data)
	}
	expect("A shares with the team again", http.MethodPost, shares, aTok, map[string]any{"teamId": teamX}, http.StatusCreated, "")
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_record_shares WHERE tenant_id = ? AND record_id = ?", teamShareTenantID, rec); err != nil || n != 1 {
		t.Fatalf("share rows = %d (%v); want exactly 1", n, err)
	}

	// B (member) reads it, flagged share-only; D (no team) still cannot.
	var got struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(expect("B after share", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusOK, ""), &got)
	if v, _ := got.Data["sharedWithMe"].(bool); !v {
		t.Fatalf("B's GET of a team-shared record: sharedWithMe = %v; want true", got.Data["sharedWithMe"])
	}
	if listTotal(bTok) != 1 {
		t.Fatal("B does not list the team-shared record")
	}
	expect("D after share", http.MethodGet, base+"/"+rec, dTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	if listTotal(dTok) != 0 {
		t.Fatal("D lists a record shared with a team they are not in")
	}

	// Read-only for the team, and the team cannot pass it on.
	expect("B PUT", http.MethodPut, base+"/"+rec, bTok, map[string]any{"title": "hacked"}, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	expect("B re-shares to the team", http.MethodPost, shares, bTok, map[string]any{"teamId": teamX}, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")

	// A lists the shares (the team row carries its name) and the team picker works for a non-admin.
	var list struct {
		Data []struct{ UserID, TeamID, TeamName string } `json:"data"`
	}
	_ = json.Unmarshal(expect("A lists shares", http.MethodGet, shares, aTok, nil, http.StatusOK, ""), &list)
	if len(list.Data) != 1 || list.Data[0].TeamID != teamX || list.Data[0].TeamName != "Phòng Kinh doanh" || list.Data[0].UserID != "" {
		t.Fatalf("shares list = %+v; want [team X]", list.Data)
	}
	var dir struct {
		Data []struct{ ID, Name string } `json:"data"`
	}
	_ = json.Unmarshal(expect("team directory (non-admin)", http.MethodGet, "/api/v1/teams/directory", aTok, nil, http.StatusOK, ""), &dir)
	if len(dir.Data) != 1 || dir.Data[0].ID != teamX {
		t.Fatalf("team directory = %+v; want only this tenant's team X", dir.Data)
	}

	// Leaving the team revokes the team's reach at the very next read; joining restores it.
	if _, err := sx.Exec("DELETE FROM team_memberships WHERE user_id = ? AND team_id = ?", uB, teamX); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	expect("B after leaving the team", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	if _, err := sx.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), uB, teamX); err != nil {
		t.Fatalf("re-add membership: %v", err)
	}
	expect("B after rejoining", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusOK, "")

	// Revoking the team share (the path segment is the TEAM id) takes the whole team's access away.
	expect("A revokes the team share", http.MethodDelete, shares+"/"+teamX, aTok, nil, http.StatusNoContent, "")
	expect("B after revoke", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	expect("revoke twice", http.MethodDelete, shares+"/"+teamX, aTok, nil, http.StatusNotFound, "RECORD_SHARE_NOT_FOUND")
}
