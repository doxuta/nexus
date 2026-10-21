//go:build integration

// Issue #43 over the full production chain (Auth → Tenant → ACL → handler → service → dynrepo) on a
// freshly migrated MySQL, with REAL logins of a non-admin rep: the write surfaces that act on an
// EXISTING record — file upload, "Gửi duyệt" (submit + the startable list), note create, relation
// link/unlink — answer a record the rep can read but not write the way PUT does since #18/#41: 403
// FORBIDDEN with the ACL stage's own bytes (RECORD_SHARED_READ_ONLY when they read it only through a
// share). A record they cannot read — another team's, a deleted one — answers byte for byte what an
// unknown id answers, so the check is no existence oracle. The rep is view=team / edit=own, which pins
// the scope the follow-up read runs at: any wider and another team's record would turn into a 403.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/files"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/services"
	"nexus/platform/internal/storage"
)

const (
	wsTenantID   = "ed175c00-0000-0000-0000-0000000000d4"
	wsTenantSlug = "write-surfaces"
	wsEntity     = "it_ws_cases"
	wsTagEntity  = "it_ws_tags"
	wsRel        = "it_ws_cases_tags"
)

func TestWriteSurfaces_ViewableNotWritable_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	backend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocalBackend: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	relSvc := services.NewRelationService(dyn, meta)
	// Wired like main.go: one ACL service and one record service behind every surface.
	rt := newA4Router(t, gdb, sx, func(h *APIHandlers) {
		acl := services.NewACLService(auth.NewRoleRepository(gdb))
		records := services.NewRecordService(meta, dyn).WithACLAuthorizer(acl)
		relSvc.WithRelationACLAuthorizer(acl)
		h.File = NewFileHandler(files.NewService(files.NewRepository(sx), backend, records,
			files.Limits{MaxBytes: 1 << 20, MimeOK: func(string) bool { return true }}, nil).WithACL(acl), 1<<20, zap.NewNop())
		h.Note = NewNoteHandler(notes.NewService(notes.NewRepository(sx), records, acl), zap.NewNop())
		h.Approval = NewApprovalHandler(services.NewApprovalService(nil, nil, nil).WithRequestFlow(&sendRecorder{}, records), zap.NewNop())
		h.Relation = NewRelationHandler(relSvc, dyn, acl, zap.NewNop())
	})

	a4SeedTenant(t, gdb, wsTenantID, wsTenantSlug, "Write surfaces")
	teamX, teamY := uuid.NewString(), uuid.NewString()
	a5SeedTeam(t, gdb, teamX, wsTenantID, "Team X")
	a5SeedTeam(t, gdb, teamY, wsTenantID, "Team Y")
	// The rep reads the team's cases and changes only their own; no delete grant (the ACL stage's 403 below).
	rep := a4SeedRole(t, gdb, wsTenantID, "teamrep", []auth.RoleAction{
		grantLevel(wsEntity, "create", "all"), grantLevel(wsEntity, "view", "team"), grantLevel(wsEntity, "edit", "owner"),
		grantLevel(wsTagEntity, "view", "all"),
	})
	mail := func(who string) string { return who + "@" + wsTenantSlug }
	x1 := a4SeedUser(t, gdb, wsTenantID, mail("x1"), false, &rep)
	x2 := a4SeedUser(t, gdb, wsTenantID, mail("x2"), false, &rep)
	y1 := a4SeedUser(t, gdb, wsTenantID, mail("y1"), false, &rep)
	a4SeedUser(t, gdb, wsTenantID, mail("admin"), true, nil)
	a5SeedMembership(t, gdb, x1, teamX) // memberships before any record: a record's team set is fixed at insert
	a5SeedMembership(t, gdb, x2, teamX)
	a5SeedMembership(t, gdb, y1, teamY)

	joinTable, _, _, err := ddl.JoinParts(wsEntity, wsTagEntity)
	if err != nil {
		t.Fatalf("ddl.JoinParts: %v", err)
	}
	cleanup := func() {
		a4DropEntity(sx, []string{wsTenantID}, wsEntity)
		a4DropEntity(sx, []string{wsTenantID}, wsTagEntity)
		sx.Exec("DROP TABLE IF EXISTS `" + joinTable + "`")
		for _, table := range []string{"sys_relationships", "sys_record_shares", "sys_record_notes", "sys_files"} {
			sx.Exec("DELETE FROM "+table+" WHERE tenant_id = ?", wsTenantID)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	tok := func(who string) string { return rt.login(t, wsTenantSlug, mail(who)).Data.AccessToken }
	adminTok := tok("admin")
	if w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": wsEntity, "label": wsEntity,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "doc", "label": "Document", "fieldType": "file"},
		},
	}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", wsEntity, w.Code, w.Body.String())
	}
	rt.createEntity(t, adminTok, wsTagEntity)
	if _, err := relSvc.RegisterRelationship(context.Background(), wsTenantID, services.RegisterRelationshipInput{
		Name: wsRel, EntityA: wsEntity, EntityB: wsTagEntity,
	}); err != nil {
		t.Fatalf("register N-N: %v", err)
	}
	x1Tok, x2Tok, y1Tok := tok("x1"), tok("x2"), tok("y1")

	base := "/api/v1/" + wsEntity
	call := func(name, method, path, token string, body any, wantStatus int, wantCode string) []byte {
		t.Helper()
		w := doAuthed(t, rt.engine, method, path, token, body, nil)
		if w.Code != wantStatus || shareCode(t, w.Body.Bytes()) != wantCode {
			t.Fatalf("%s: %s %s → %d %s; want %d %s", name, method, path, w.Code, w.Body.String(), wantStatus, wantCode)
		}
		return w.Body.Bytes()
	}
	create := func(path, token, title string) string {
		t.Helper()
		var env recordCreateEnvelope
		_ = json.Unmarshal(call("create "+title, http.MethodPost, path, token, map[string]any{"title": title}, http.StatusCreated, ""), &env)
		return env.Data.ID
	}
	recs := map[string]string{
		"mine":   create(base, x1Tok, "x1's case"),
		"mate":   create(base, x2Tok, "x2's case"),    // a teammate's: x1 reads it, may not change it
		"other":  create(base, y1Tok, "y1's case"),    // another team's: hidden from x1
		"shared": create(base, y1Tok, "y1's shared"),  // another team's, shared read-only with x1
		"gone":   create(base, x2Tok, "x2's deleted"), // readable once, deleted before x1 writes
		"none":   uuid.NewString(),                    // an id that never existed
	}
	tag := create("/api/v1/"+wsTagEntity, adminTok, "tag")
	for _, k := range []string{"mine", "mate", "other", "shared", "gone"} { // admin links every case to the tag
		call("admin links "+k, http.MethodPost, base+"/"+recs[k]+"/relationships/"+wsRel, adminTok, map[string]any{"targetId": tag}, http.StatusCreated, "")
	}
	call("y1 shares with x1", http.MethodPost, base+"/"+recs["shared"]+"/shares", y1Tok, map[string]any{"userId": x1}, http.StatusCreated, "")
	call("x1 reads mate's", http.MethodGet, base+"/"+recs["mate"], x1Tok, nil, http.StatusOK, "")
	call("x1 reads gone before delete", http.MethodGet, base+"/"+recs["gone"], x1Tok, nil, http.StatusOK, "")
	call("x1 cannot read other's", http.MethodGet, base+"/"+recs["other"], x1Tok, nil, http.StatusNotFound, "RECORD_NOT_FOUND")
	call("admin deletes gone", http.MethodDelete, base+"/"+recs["gone"], adminTok, nil, http.StatusNoContent, "")
	// The ACL stage's own 403 (x1 holds no delete grant): every 403 FORBIDDEN below must be these bytes.
	acl403 := call("x1 DELETE (ACL stage)", http.MethodDelete, base+"/"+recs["mine"], x1Tok, nil, http.StatusForbidden, "FORBIDDEN")

	upload := func(rec string) (int, []byte) {
		w := (&fileTestEnv{engine: rt.engine}).uploadFile(t, x1Tok, wsEntity, rec, "doc", "a.txt", "text/plain", []byte("hello"))
		return w.Code, w.Body.Bytes()
	}
	surface := func(method, suffix string, body any) func(rec string) (int, []byte) {
		return func(rec string) (int, []byte) {
			w := doAuthed(t, rt.engine, method, base+"/"+rec+suffix, x1Tok, body, nil)
			return w.Code, w.Body.Bytes()
		}
	}
	type want struct {
		status int
		code   string
	}
	surfaces := []struct {
		name   string
		do     func(rec string) (int, []byte)
		mine   want
		mate   want // readable, not writable
		absent want // what an unknown id answers — and so must another team's or a deleted record
	}{
		{"file upload", upload, want{201, ""}, want{403, "FORBIDDEN"}, want{404, "RECORD_NOT_FOUND"}},
		{"approval submit", surface(http.MethodPost, "/approvals/submit", nil), want{201, ""}, want{403, "FORBIDDEN"}, want{404, "RECORD_NOT_FOUND"}},
		{"approval startable", surface(http.MethodGet, "/approvals/startable", nil), want{200, ""}, want{403, "FORBIDDEN"}, want{404, "RECORD_NOT_FOUND"}},
		// The note floor is "view" (specs/060 #349): a teammate's record is commentable.
		{"note create", surface(http.MethodPost, "/notes", map[string]any{"body": "ghi chú"}), want{201, ""}, want{201, ""}, want{404, "RECORD_NOT_FOUND"}},
		{"relation link", surface(http.MethodPost, "/relationships/"+wsRel, map[string]any{"targetId": tag}), want{200, ""}, want{403, "FORBIDDEN"}, want{422, "RECORD_RELATION_INVALID"}},
		{"relation unlink", surface(http.MethodDelete, "/relationships/"+wsRel+"/"+tag, nil), want{204, ""}, want{403, "FORBIDDEN"}, want{422, "RECORD_RELATION_INVALID"}},
	}
	for _, s := range surfaces {
		check := func(label, rec string, w want) []byte {
			t.Helper()
			status, raw := s.do(recs[rec])
			if status != w.status || (w.code != "" && shareCode(t, raw) != w.code) {
				t.Fatalf("%s on %s's record: %d %s; want %d %s", s.name, label, status, raw, w.status, w.code)
			}
			return raw
		}
		check("x1's own", "mine", s.mine)
		if mate := check("a teammate", "mate", s.mate); s.mate.status == http.StatusForbidden && string(mate) != string(acl403) {
			t.Fatalf("%s 403 body differs from the ACL stage's:\n got %s\n ACL %s", s.name, mate, acl403)
		}
		check("a share-only", "shared", want{403, "RECORD_SHARED_READ_ONLY"})
		// Hidden and deleted records answer what an unknown id answers, byte for byte (the relation
		// routes echo the id in their sentence, so it is normalised before the comparison).
		none := strings.ReplaceAll(string(check("an unknown", "none", s.absent)), recs["none"], "<id>")
		for _, label := range []string{"other", "gone"} {
			if got := strings.ReplaceAll(string(check(label, label, s.absent)), recs[label], "<id>"); got != none {
				t.Fatalf("%s on %s differs from an unknown id:\n %s\n %s", s.name, label, got, none)
			}
		}
	}

	// Nothing landed on a record x1 may not change: no file, no note on the share, the links stand.
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_files WHERE tenant_id = ? AND record_id <> ? AND deleted_at IS NULL", wsTenantID, recs["mine"]); err != nil || n != 0 {
		t.Fatalf("files on records x1 may not change = %d (%v); want 0", n, err)
	}
	if err := sx.Get(&n, "SELECT COUNT(*) FROM sys_record_notes WHERE tenant_id = ? AND record_id NOT IN (?, ?)", wsTenantID, recs["mine"], recs["mate"]); err != nil || n != 0 {
		t.Fatalf("notes on records x1 may not note = %d (%v); want 0", n, err)
	}
	var linked struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	for _, k := range []string{"mate", "shared", "other"} {
		_ = json.Unmarshal(call("admin lists "+k+"'s links", http.MethodGet, base+"/"+recs[k]+"/relationships/"+wsRel, adminTok, nil, http.StatusOK, ""), &linked)
		if linked.Pagination.Total != 1 {
			t.Fatalf("%s's links after x1's unlink = %d; want the 1 admin made", k, linked.Pagination.Total)
		}
	}
}
