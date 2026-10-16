//go:build integration

// W9b — recycle-bin + saved-views HTTP integration tests against a REAL MySQL 8 (migrated via the
// golang-migrate chain, incl. 000035 sys_views). Run with:
//
//	go test -tags=integration ./internal/handlers/ -run TestW9b
//
// They drive the genuine production chain Auth → Tenant → ACL → handler using the a4 RBAC harness
// (real services.ACLService), so the per-action gates are exactly what production enforces:
//   - GET  /:entity/deleted      rides RequireACL (GET→"view").
//   - POST /:entity/:id/restore  rides RequireACLAction("delete") — a view-only role is refused.
//   - /:entity/views[/:id]       ride RequireACLAction("view") — plus the in-service shared/personal
//     ownership gate (403 shared-by-non-admin, 404 other-user's-personal).
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
)

const (
	w9bTenantID   = "9b9b0000-0000-0000-0000-0000000000a1"
	w9bTenantSlug = "w9b-acme"
	w9bEntity     = "it_w9b_contacts"
)

// TestW9bRestoreRequiresDeleteGrant proves the recycle-bin ACL split: a role with only "view" can
// LIST the recycle bin but CANNOT restore (that needs "delete"); a role with "delete" can restore.
func TestW9bRestoreRequiresDeleteGrant(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w9bTenantID, w9bTenantSlug, "Acme W9b")
	a4SeedUser(t, gdb, w9bTenantID, "admin@"+w9bTenantSlug, true, nil)

	viewerRole := a4SeedRole(t, gdb, w9bTenantID, "viewer", []auth.RoleAction{grant(w9bEntity, "view")})
	deleterRole := a4SeedRole(t, gdb, w9bTenantID, "deleter", []auth.RoleAction{
		grant(w9bEntity, "view"), grant(w9bEntity, "delete"),
	})
	a4SeedUser(t, gdb, w9bTenantID, "viewer@"+w9bTenantSlug, false, &viewerRole)
	a4SeedUser(t, gdb, w9bTenantID, "deleter@"+w9bTenantSlug, false, &deleterRole)

	a4DropEntity(sx, []string{w9bTenantID}, w9bEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{w9bTenantID}, w9bEntity) })

	adminTok := rt.login(t, w9bTenantSlug, "admin@"+w9bTenantSlug).Data.AccessToken
	rt.createEntity(t, adminTok, w9bEntity)
	viewerTok := rt.login(t, w9bTenantSlug, "viewer@"+w9bTenantSlug).Data.AccessToken
	deleterTok := rt.login(t, w9bTenantSlug, "deleter@"+w9bTenantSlug).Data.AccessToken

	// Create a record (admin) and soft-delete it (admin bypasses ACL) so it sits in the recycle bin.
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity, adminTok, map[string]any{"title": "gone"}, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("seed record: %d %s", wc.Code, wc.Body.String())
	}
	var rec recordCreateEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &rec)
	id := rec.Data.ID
	if wd := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+w9bEntity+"/"+id, adminTok, nil, nil); wd.Code != http.StatusNoContent {
		t.Fatalf("soft-delete: %d %s", wd.Code, wd.Body.String())
	}

	// The viewer CAN list the recycle bin (GET→view) and sees the soft-deleted record.
	wl := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+w9bEntity+"/deleted", viewerTok, nil, nil)
	if wl.Code != http.StatusOK {
		t.Fatalf("viewer GET deleted: %d %s; want 200", wl.Code, wl.Body.String())
	}
	var env aclListEnvelope
	_ = json.Unmarshal(wl.Body.Bytes(), &env)
	if env.Pagination.Total != 1 {
		t.Fatalf("recycle bin should hold 1 row, got total=%d (body=%s)", env.Pagination.Total, wl.Body.String())
	}

	// The viewer CANNOT restore (restore requires the "delete" grant) → 403.
	wr := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/"+id+"/restore", viewerTok, nil, nil)
	if wr.Code != http.StatusForbidden {
		t.Fatalf("viewer restore: %d %s; want 403 (restore needs delete grant)", wr.Code, wr.Body.String())
	}

	// The deleter CAN restore → 200; the record leaves the recycle bin.
	wr2 := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/"+id+"/restore", deleterTok, nil, nil)
	if wr2.Code != http.StatusOK {
		t.Fatalf("deleter restore: %d %s; want 200", wr2.Code, wr2.Body.String())
	}
	// Restoring an already-live record now 404s (not soft-deleted).
	wr3 := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/"+id+"/restore", deleterTok, nil, nil)
	if wr3.Code != http.StatusNotFound {
		t.Fatalf("restore of a live record: %d; want 404", wr3.Code)
	}
}

// viewCreateEnvelope decodes {"data":{"id":...}} from POST /:entity/views.
type viewCreateEnvelope struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// TestW9bViewsACLAndOwnership proves the saved-views gates: the entity-level "view" grant is
// required; a non-admin cannot create/mutate a SHARED view (403); another user's PERSONAL view is
// not-found (404); an oversized config is 422.
func TestW9bViewsACLAndOwnership(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, w9bTenantID, w9bTenantSlug, "Acme W9b")
	a4SeedUser(t, gdb, w9bTenantID, "admin@"+w9bTenantSlug, true, nil)

	viewerRole := a4SeedRole(t, gdb, w9bTenantID, "viewer", []auth.RoleAction{grant(w9bEntity, "view")})
	noneRole := a4SeedRole(t, gdb, w9bTenantID, "none", nil)
	a4SeedUser(t, gdb, w9bTenantID, "alice@"+w9bTenantSlug, false, &viewerRole)
	a4SeedUser(t, gdb, w9bTenantID, "bob@"+w9bTenantSlug, false, &viewerRole)
	a4SeedUser(t, gdb, w9bTenantID, "nobody@"+w9bTenantSlug, false, &noneRole)

	a4DropEntity(sx, []string{w9bTenantID}, w9bEntity)
	t.Cleanup(func() {
		_, _ = sx.Exec("DELETE FROM sys_views WHERE tenant_id = ?", w9bTenantID)
		a4DropEntity(sx, []string{w9bTenantID}, w9bEntity)
	})

	adminTok := rt.login(t, w9bTenantSlug, "admin@"+w9bTenantSlug).Data.AccessToken
	rt.createEntity(t, adminTok, w9bEntity)
	aliceTok := rt.login(t, w9bTenantSlug, "alice@"+w9bTenantSlug).Data.AccessToken
	bobTok := rt.login(t, w9bTenantSlug, "bob@"+w9bTenantSlug).Data.AccessToken
	nobodyTok := rt.login(t, w9bTenantSlug, "nobody@"+w9bTenantSlug).Data.AccessToken

	cfg := map[string]any{"columns": []string{"title"}}

	// A role with NO "view" grant is refused on the views surface (RequireACLAction("view")).
	if w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+w9bEntity+"/views", nobodyTok, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("no-view-grant GET views: %d; want 403", w.Code)
	}
	if w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/views", nobodyTok, map[string]any{"name": "x", "config": cfg}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("no-view-grant POST views: %d; want 403", w.Code)
	}

	// Alice (view grant) creates a PERSONAL view → 201, and sees it in her list.
	wA := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/views", aliceTok, map[string]any{"name": "Alice open", "config": cfg}, nil)
	if wA.Code != http.StatusCreated {
		t.Fatalf("alice create personal view: %d %s; want 201", wA.Code, wA.Body.String())
	}
	var av viewCreateEnvelope
	_ = json.Unmarshal(wA.Body.Bytes(), &av)
	aliceViewID := av.Data.ID

	wList := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+w9bEntity+"/views", aliceTok, nil, nil)
	if wList.Code != http.StatusOK || !strings.Contains(wList.Body.String(), aliceViewID) {
		t.Fatalf("alice list should contain her view: %d %s", wList.Code, wList.Body.String())
	}

	// A non-admin cannot create a SHARED view → 403 VIEW_FORBIDDEN.
	wShared := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/views", aliceTok, map[string]any{"name": "team", "config": cfg, "shared": true}, nil)
	if wShared.Code != http.StatusForbidden {
		t.Fatalf("non-admin create shared view: %d %s; want 403", wShared.Code, wShared.Body.String())
	}

	// Bob (also view grant, different user) cannot mutate Alice's PERSONAL view → 404 (no leak).
	if w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/"+w9bEntity+"/views/"+aliceViewID, bobTok, map[string]any{"name": "hijack", "config": cfg}, nil); w.Code != http.StatusNotFound {
		t.Fatalf("bob PUT alice's personal view: %d %s; want 404", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+w9bEntity+"/views/"+aliceViewID, bobTok, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("bob DELETE alice's personal view: %d; want 404", w.Code)
	}

	// Alice CAN update + delete her own view.
	if w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/"+w9bEntity+"/views/"+aliceViewID, aliceTok, map[string]any{"name": "renamed", "config": cfg}, nil); w.Code != http.StatusOK {
		t.Fatalf("alice PUT own view: %d %s; want 200", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/"+w9bEntity+"/views/"+aliceViewID, aliceTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("alice DELETE own view: %d; want 204", w.Code)
	}

	// Admin creates a SHARED view; a non-admin then cannot mutate it → 403 VIEW_FORBIDDEN.
	wAdminShared := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/views", adminTok, map[string]any{"name": "team shared", "config": cfg, "shared": true}, nil)
	if wAdminShared.Code != http.StatusCreated {
		t.Fatalf("admin create shared view: %d %s; want 201", wAdminShared.Code, wAdminShared.Body.String())
	}
	var sv viewCreateEnvelope
	_ = json.Unmarshal(wAdminShared.Body.Bytes(), &sv)
	if w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/"+w9bEntity+"/views/"+sv.Data.ID, aliceTok, map[string]any{"name": "steal", "config": cfg}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin mutate shared view: %d %s; want 403", w.Code, w.Body.String())
	}

	// An oversized config is rejected 422 VIEW_CONFIG_TOO_LARGE.
	big := strings.Repeat("a", 70*1024)
	wBig := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+w9bEntity+"/views", aliceTok, map[string]any{"name": "big", "config": map[string]any{"blob": big}}, nil)
	if wBig.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized config: %d %s; want 422", wBig.Code, wBig.Body.String())
	}
	var bigEnv aclListEnvelope
	_ = json.Unmarshal(wBig.Body.Bytes(), &bigEnv)
	if bigEnv.Code != "VIEW_CONFIG_TOO_LARGE" {
		t.Errorf("oversized config code = %q; want VIEW_CONFIG_TOO_LARGE", bigEnv.Code)
	}
}
