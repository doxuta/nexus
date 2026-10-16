//go:build integration

// Manual record shares over the FULL production chain (DAILY-QUEUE #34 record-manual-share-a,
// specs/051-record-manual-share): Auth → Tenant → ACL → DynamicRecordHandler / RecordShareHandler →
// shares.Service → RecordService → dynrepo, on a freshly migrated MySQL, with REAL logins of
// non-admin users. The dynrepo suite proves the SQL; this proves the wiring a user actually hits —
// the share widens a VIEW, is refused on every write with 403 RECORD_SHARED_READ_ONLY, and is managed
// only by someone who may edit the record.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/shares"
)

const (
	shareTenantAID   = "5a5e0000-0000-0000-0000-0000000000a1"
	shareTenantBID   = "5a5e0000-0000-0000-0000-0000000000b2"
	shareTenantASlug = "share-acme"
	shareTenantBSlug = "share-globex"
	shareEntity      = "it_share_deals"
)

func shareCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Code
}

func TestRecordShare_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, shareTenantAID, shareTenantASlug, "Acme share")
	a4SeedTenant(t, gdb, shareTenantBID, shareTenantBSlug, "Globex share")

	// "rep" mirrors crm_sales_rep on crm_opportunities: sees and edits only their own rows.
	rep := a4SeedRole(t, gdb, shareTenantAID, "rep", []auth.RoleAction{
		grantLevel(shareEntity, "create", "all"), grantLevel(shareEntity, "view", "owner"),
		grantLevel(shareEntity, "edit", "owner"), grantLevel(shareEntity, "delete", "owner"),
	})
	viewOnly := a4SeedRole(t, gdb, shareTenantAID, "viewer", []auth.RoleAction{
		grantLevel(shareEntity, "view", "owner"),
	})
	manager := a4SeedRole(t, gdb, shareTenantAID, "manager", []auth.RoleAction{
		grantLevel(shareEntity, "view", "all"), grantLevel(shareEntity, "edit", "all"),
	})
	repB := a4SeedRole(t, gdb, shareTenantBID, "rep", []auth.RoleAction{grantLevel(shareEntity, "view", "owner")})

	uA := a4SeedUser(t, gdb, shareTenantAID, "a@"+shareTenantASlug, false, &rep)
	uB := a4SeedUser(t, gdb, shareTenantAID, "b@"+shareTenantASlug, false, &rep)
	uC := a4SeedUser(t, gdb, shareTenantAID, "c@"+shareTenantASlug, false, &viewOnly)
	a4SeedUser(t, gdb, shareTenantAID, "m@"+shareTenantASlug, false, &manager)
	a4SeedUser(t, gdb, shareTenantAID, "admin@"+shareTenantASlug, true, nil)
	uX := a4SeedUser(t, gdb, shareTenantBID, "x@"+shareTenantBSlug, false, &repB)

	a4DropEntity(sx, []string{shareTenantAID}, shareEntity)
	t.Cleanup(func() {
		a4DropEntity(sx, []string{shareTenantAID}, shareEntity)
		sx.Exec("DELETE FROM sys_record_shares WHERE tenant_id IN (?, ?)", shareTenantAID, shareTenantBID)
	})
	rt.createEntity(t, rt.login(t, shareTenantASlug, "admin@"+shareTenantASlug).Data.AccessToken, shareEntity)

	tok := func(email string) string { return rt.login(t, shareTenantASlug, email).Data.AccessToken }
	aTok, bTok, cTok, mTok := tok("a@"+shareTenantASlug), tok("b@"+shareTenantASlug), tok("c@"+shareTenantASlug), tok("m@"+shareTenantASlug)

	base := "/api/v1/" + shareEntity
	w := doAuthed(t, rt.engine, http.MethodPost, base, aTok, map[string]any{"title": "Hợp đồng A"}, nil)
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
			t.Fatalf("%s: %s %s → %d %s; want %d %s", name, method, path, w.Code, w.Body.String(), wantStatus, wantCode)
		}
		return w.Body.Bytes()
	}
	listHas := func(token string) bool {
		t.Helper()
		var env struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		_ = json.Unmarshal(expect("list", http.MethodGet, base, token, nil, http.StatusOK, ""), &env)
		for _, r := range env.Data {
			if r.ID == rec {
				if env.Pagination.Total != len(env.Data) {
					t.Fatalf("list total %d != rows %d", env.Pagination.Total, len(env.Data))
				}
				return true
			}
		}
		return false
	}

	// Before any share: B (same role, different person) cannot see A's record.
	expect("B get before share", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	if listHas(bTok) {
		t.Fatal("B lists A's record before any share")
	}

	// Validation of the grantee.
	expect("share with self", http.MethodPost, shares, aTok, map[string]any{"userId": uA}, http.StatusUnprocessableEntity, "RECORD_SHARE_SELF")
	expect("share with another tenant's user", http.MethodPost, shares, aTok, map[string]any{"userId": uX}, http.StatusUnprocessableEntity, "RECORD_SHARE_USER_INVALID")
	expect("share with nobody", http.MethodPost, shares, aTok, map[string]any{"userId": uuid.NewString()}, http.StatusUnprocessableEntity, "RECORD_SHARE_USER_INVALID")

	// A shares with B — twice: one live row, not two.
	body := expect("A shares with B", http.MethodPost, shares, aTok, map[string]any{"userId": uB}, http.StatusCreated, "")
	var one struct {
		Data struct {
			UserID, SharedBy, UserEmail string
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &one)
	if one.Data.UserID != uB || one.Data.SharedBy != uA || one.Data.UserEmail != "b@"+shareTenantASlug {
		t.Fatalf("share body = %+v", one.Data)
	}
	expect("A shares with B again", http.MethodPost, shares, aTok, map[string]any{"userId": uB}, http.StatusCreated, "")
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_record_shares WHERE tenant_id = ? AND record_id = ?", shareTenantAID, rec); err != nil || n != 1 {
		t.Fatalf("share rows = %d (%v); want exactly 1", n, err)
	}

	// B now READS it: GET + list. The GET says it is share-only (DAILY-QUEUE #223) so the record page
	// hides "Sửa" on open; A's own read of the same record carries no marker.
	sharedWithMe := func(body []byte) (bool, bool) {
		var env struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(body, &env)
		v, present := env.Data["sharedWithMe"]
		b, _ := v.(bool)
		return b, present
	}
	if got, _ := sharedWithMe(expect("B get after share", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusOK, "")); !got {
		t.Fatal("B's GET of a share-only record: sharedWithMe != true")
	}
	if _, present := sharedWithMe(expect("A get own", http.MethodGet, base+"/"+rec, aTok, nil, http.StatusOK, "")); present {
		t.Fatal("A's GET of their own record carries sharedWithMe")
	}
	if !listHas(bTok) {
		t.Fatal("B does not list the shared record")
	}

	// ... and every WRITE is refused as read-only (403, not 404), leaving the record untouched.
	expect("B PUT shared", http.MethodPut, base+"/"+rec, bTok, map[string]any{"title": "hacked"}, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	expect("B DELETE shared", http.MethodDelete, base+"/"+rec, bTok, nil, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	var title string
	if err := sx.Get(&title, "SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.title')) FROM `obj_"+shareEntity+"` WHERE id = ? AND deleted_at IS NULL", rec); err != nil || title != "Hợp đồng A" {
		t.Fatalf("record after B's writes: %q (%v)", title, err)
	}

	// B cannot re-share, list or revoke: no edit scope on the record.
	expect("B re-shares", http.MethodPost, shares, bTok, map[string]any{"userId": uC}, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	expect("B lists shares", http.MethodGet, shares, bTok, nil, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	expect("B revokes own share", http.MethodDelete, shares+"/"+uB, bTok, nil, http.StatusForbidden, "RECORD_SHARED_READ_ONLY")
	// C has no edit grant at all → the ACL stage refuses before anything else.
	expect("C (no edit grant) shares", http.MethodPost, shares, cTok, map[string]any{"userId": uC}, http.StatusForbidden, "FORBIDDEN")

	// A lists the shares; a manager (edit=all, not the sharer) revokes — Q4a.
	var list struct {
		Data []struct{ UserID string } `json:"data"`
	}
	_ = json.Unmarshal(expect("A lists shares", http.MethodGet, shares, aTok, nil, http.StatusOK, ""), &list)
	if len(list.Data) != 1 || list.Data[0].UserID != uB {
		t.Fatalf("shares list = %+v; want [B]", list.Data)
	}
	expect("manager revokes", http.MethodDelete, shares+"/"+uB, mTok, nil, http.StatusNoContent, "")
	expect("B get after revoke", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	if listHas(bTok) {
		t.Fatal("B still lists the record after the revoke")
	}
	expect("revoke twice", http.MethodDelete, shares+"/"+uB, aTok, nil, http.StatusNotFound, "RECORD_SHARE_NOT_FOUND")

	// Re-sharing after a revoke revives the same row (UNIQUE key never collides with the tombstone).
	expect("A shares with B after revoke", http.MethodPost, shares, aTok, map[string]any{"userId": uB}, http.StatusCreated, "")
	expect("B get after re-share", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusOK, "")

	// Owner change keeps the share (specs/051 §6, DAILY-QUEUE #226 point 4): shares belong to the
	// record, not to its owner (Q4a — whoever may edit manages them), and an automatic `assign` must
	// not silently strip someone's access. The manager reassigns A's record to C; B still reads it.
	expect("manager changes the owner", http.MethodPut, base+"/"+rec+"/owner", mTok, map[string]any{"userId": uC}, http.StatusOK, "")
	expect("B still reads after the owner changed", http.MethodGet, base+"/"+rec, bTok, nil, http.StatusOK, "")
	var live int
	if err := sx.Get(&live, "SELECT COUNT(*) FROM sys_record_shares WHERE tenant_id = ? AND record_id = ? AND user_id = ? AND deleted_at IS NULL", shareTenantAID, rec, uB); err != nil || live != 1 {
		t.Fatalf("live share of B after the owner change = %d (%v); want 1", live, err)
	}
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_record_shares WHERE tenant_id = ? AND record_id = ?", shareTenantAID, rec); err != nil || n != 1 {
		t.Fatalf("share rows after revive = %d (%v); want 1", n, err)
	}
}

// TestShareRepository_UpsertReportsChange pins DAILY-QUEUE #226 point 1 on a real MySQL: Upsert says
// whether the call changed the table — true for a fresh insert and for reviving a revoked share, false
// when the grantee already held a live one (the caller then writes no second audit row). The signal
// must not depend on the DSN's clientFoundRows flag (production forces it on, and under it an
// ON DUPLICATE KEY no-op reports 1, indistinguishable from an insert).
func TestShareRepository_UpsertReportsChange(t *testing.T) {
	_, sx := a4SetupMigratedDB(t)
	repo := shares.NewRepository(sx)
	ctx := context.Background()
	const tenant, entity, rec, user = "5a5e0000-0000-0000-0000-0000000000c3", "it_share_deals", "5a5e0000-0000-0000-0000-0000000000d4", "5a5e0000-0000-0000-0000-0000000000e5"
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_record_shares WHERE tenant_id = ?", tenant) })
	sx.Exec("DELETE FROM sys_record_shares WHERE tenant_id = ?", tenant)

	step := func(name, sharer string, wantChanged bool) *shares.Share {
		t.Helper()
		sh, changed, err := repo.Upsert(ctx, uuid.NewString(), tenant, entity, rec, user, "", sharer)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changed != wantChanged {
			t.Fatalf("%s: changed = %v; want %v", name, changed, wantChanged)
		}
		return sh
	}
	first := step("first share inserts", "s-1", true)
	again := step("share again with a live share is a no-op", "s-2", false)
	if again.SharedBy != "s-1" || again.ID != first.ID {
		t.Fatalf("no-op re-share changed the row: %+v vs %+v", again, first)
	}
	if _, err := repo.Revoke(ctx, tenant, entity, rec, user); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revived := step("share after revoke revives the row", "s-3", true)
	if revived.ID != first.ID || revived.SharedBy != "s-3" {
		t.Fatalf("revive = %+v; want the same row, new sharer", revived)
	}
	step("and again is a no-op", "s-4", false)
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_record_shares WHERE tenant_id = ?", tenant); err != nil || n != 1 {
		t.Fatalf("rows = %d (%v); want 1", n, err)
	}
}
