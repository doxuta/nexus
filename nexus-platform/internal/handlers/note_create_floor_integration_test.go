//go:build integration

package handlers

// specs/060 (DAILY-QUEUE #349) acceptance over the real migrated schema and the REAL ACL service:
// the note floor is "view" ("whoever can read the record can comment on it"), not "create". Before,
// POST /:entity/:id/notes sat on the method-mapped record group (POST→create), so a technician with
// view/edit (own) and no create grant got 403 on every note on their own work order. The service still
// re-resolves view + row scope, so another person's order stays a 404; and because a "view" scope
// carries IncludeShares, a person who sees the record ONLY through a manual share (read-only,
// specs/051) must be refused, not allowed to write.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nexus/platform/internal/auth"
)

func TestRecordNotes_ViewFloorWithoutCreate(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newNoteRouter(t, gdb, sx)

	const (
		tenantID = "n1000000-0000-0000-0000-00000000000b"
		slug     = "notes-floor"
		entity   = "it_notes_floor"
	)
	a4SeedTenant(t, gdb, tenantID, slug, "Notes floor IT")
	// The technician shape: sees and edits only their own rows, NO create, NO delete.
	roleKtv := a4SeedRole(t, gdb, tenantID, "ktv", []auth.RoleAction{
		grantLevel(entity, "view", "owner"),
		grantLevel(entity, "edit", "owner"),
	})
	roleNoView := a4SeedRole(t, gdb, tenantID, "chi-tao", []auth.RoleAction{grantLevel(entity, "create", "all")})
	a4SeedUser(t, gdb, tenantID, "admin@"+slug, true, nil)
	ktvID := a4SeedUser(t, gdb, tenantID, "ktv@"+slug, false, &roleKtv)
	peerID := a4SeedUser(t, gdb, tenantID, "peer@"+slug, false, &roleKtv)
	a4SeedUser(t, gdb, tenantID, "tao@"+slug, false, &roleNoView)

	t.Cleanup(func() { a4DropEntity(sx, []string{tenantID}, entity) })
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_record_notes WHERE tenant_id = ?", tenantID) })
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_record_shares WHERE tenant_id = ?", tenantID) })

	adminTok := rt.login(t, slug, "admin@"+slug).Data.AccessToken
	rt.createEntity(t, adminTok, entity)
	own := noteCreateRecord(t, rt, adminTok, entity, "Phiếu của KTV")
	other := noteCreateRecord(t, rt, adminTok, entity, "Phiếu của người khác")
	shared := noteCreateRecord(t, rt, adminTok, entity, "Phiếu chia sẻ cho KTV")
	for rec, uid := range map[string]string{own: ktvID, other: peerID, shared: peerID} {
		if w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/"+entity+"/"+rec+"/owner", adminTok,
			map[string]any{"userId": uid}, nil); w.Code != http.StatusOK {
			t.Fatalf("assign owner: %d %s", w.Code, w.Body.String())
		}
	}
	// Peer's order shared READ-ONLY with the technician (what the share dialog writes).
	if _, err := sx.Exec(`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, shared_by, created_by)
		VALUES (UUID(), ?, ?, ?, ?, ?, ?)`, tenantID, entity, shared, ktvID, peerID, peerID); err != nil {
		t.Fatalf("seed share: %v", err)
	}
	ktvTok := rt.login(t, slug, "ktv@"+slug).Data.AccessToken
	taoTok := rt.login(t, slug, "tao@"+slug).Data.AccessToken

	post := func(tok, rec string) *httptest.ResponseRecorder {
		return doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+entity+"/"+rec+"/notes", tok, map[string]any{"body": "đã kiểm tra"}, nil)
	}

	if w := post(ktvTok, own); w.Code != http.StatusCreated {
		t.Fatalf("KTV (view/edit own, no create) note on own order: want 201, got %d %s", w.Code, w.Body.String())
	}
	if w := post(ktvTok, other); w.Code != http.StatusNotFound {
		t.Fatalf("KTV note on another person's order: want 404, got %d %s", w.Code, w.Body.String())
	}
	// Seen only through a read-only share: readable, but not writable.
	if w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+entity+"/"+shared+"/notes", ktvTok, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("KTV list notes on a shared order: want 200, got %d %s", w.Code, w.Body.String())
	}
	if w := post(ktvTok, shared); w.Code != http.StatusForbidden {
		t.Fatalf("KTV note on a share-only order: want 403 RECORD_SHARED_READ_ONLY, got %d %s", w.Code, w.Body.String())
	}
	// Create alone is not a view grant.
	if w := post(taoTok, own); w.Code != http.StatusForbidden {
		t.Fatalf("create-only role note: want 403, got %d %s", w.Code, w.Body.String())
	}
}
