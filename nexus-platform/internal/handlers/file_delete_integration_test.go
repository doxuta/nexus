//go:build integration

package handlers

// TestFileDelete_EditGrantWithoutDelete is the specs/060 (DAILY-QUEUE #348) acceptance over the real
// migrated schema and the REAL ACL service: the technician — edit (own), NO delete grant — takes back
// the photo they attached to their own order. Before, files.Service.Delete asked for "delete", so the
// person who could upload a wrong photo could not remove it. Someone else's order and a view-only role
// stay out of reach, and the record's reference array is re-synced so no phantom attachment is left.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/files"
)

func TestFileDelete_EditGrantWithoutDelete(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	env := newFileRouter(t, gdb, sx, true)

	const (
		entity      = "fildel_phieu"
		tenant      = "61616161-6161-6161-6161-616161616161"
		slug        = "file-del"
		adminEmail  = "admin@file-del.test"
		ktvEmail    = "ktv@file-del.test"
		viewerEmail = "xem@file-del.test"
		fieldDoc    = "doc"
	)
	a4SeedTenant(t, gdb, tenant, slug, "File Delete ACL")
	a4SeedUser(t, gdb, tenant, adminEmail, true, nil)
	ktvRole := a4SeedRole(t, gdb, tenant, "ky_thuat_vien", []auth.RoleAction{
		{EntityName: entity, Action: "view", AccessLevel: "owner"},
		{EntityName: entity, Action: "edit", AccessLevel: "owner"},
	})
	viewerRole := a4SeedRole(t, gdb, tenant, "chi_xem", []auth.RoleAction{grant(entity, "view")})
	ktvID := a4SeedUser(t, gdb, tenant, ktvEmail, false, &ktvRole)
	a4SeedUser(t, gdb, tenant, viewerEmail, false, &viewerRole)
	t.Cleanup(func() { a4DropEntity(sx, []string{tenant}, entity) })
	t.Cleanup(func() { sx.Exec("DELETE FROM sys_files WHERE tenant_id = ?", tenant) })

	adminTok := itLogin(t, env.engine, slug, adminEmail, authPassword).Data.AccessToken
	env.createFileEntity(t, adminTok, entity)
	own := env.createRecord(t, adminTok, entity, "Phiếu của KTV")
	other := env.createRecord(t, adminTok, entity, "Phiếu người khác")
	if w := doAuthed(t, env.engine, http.MethodPut, "/api/v1/"+entity+"/"+own+"/owner", adminTok,
		map[string]any{"userId": ktvID}, nil); w.Code != http.StatusOK {
		t.Fatalf("assign owner: %d %s", w.Code, w.Body.String())
	}
	ktvTok := itLogin(t, env.engine, slug, ktvEmail, authPassword).Data.AccessToken
	viewerTok := itLogin(t, env.engine, slug, viewerEmail, authPassword).Data.AccessToken

	upload := func(tok, recordID string) string {
		t.Helper()
		w := env.uploadFile(t, tok, entity, recordID, fieldDoc, "anh.txt", "text/plain", []byte("anh"))
		if w.Code != http.StatusCreated {
			t.Fatalf("upload: want 201, got %d %s", w.Code, w.Body.String())
		}
		var up struct {
			Data files.UploadResult `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil || up.Data.ID == "" {
			t.Fatalf("decode upload: %v (%s)", err, w.Body.String())
		}
		return up.Data.ID
	}
	ownFile := upload(ktvTok, own)
	otherFile := upload(adminTok, other)

	// 1) Another person's order: the file is invisible to this role — the same 404 as a missing id.
	if w := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+otherFile, ktvTok, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("KTV delete on another person's order: want 404, got %d %s", w.Code, w.Body.String())
	}
	// 2) View-only on the technician's own order: reading is not changing — 404, not 204.
	if w := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+ownFile, viewerTok, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("view-only delete: want 404, got %d %s", w.Code, w.Body.String())
	}
	if got := env.countFileRows(t, tenant); got != 2 {
		t.Fatalf("file rows = %d, want 2 (refused deletes must remove nothing)", got)
	}
	// 3) Edit (own), no delete grant: takes back their own photo.
	if w := doAuthed(t, env.engine, http.MethodDelete, "/api/v1/files/"+ownFile, ktvTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("KTV delete on own order: want 204, got %d %s", w.Code, w.Body.String())
	}
	if got := env.countFileRows(t, tenant); got != 1 {
		t.Fatalf("file rows after own delete = %d, want 1", got)
	}
	// 4) The record's reference array no longer lists the removed file.
	wRec := doAuthed(t, env.engine, http.MethodGet, "/api/v1/"+entity+"/"+own, ktvTok, nil, nil)
	if wRec.Code != http.StatusOK || strings.Contains(wRec.Body.String(), ownFile) {
		t.Fatalf("record must not list the removed attachment: %d %s", wRec.Code, wRec.Body.String())
	}
}
