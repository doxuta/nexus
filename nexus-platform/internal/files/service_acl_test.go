package files

// Per-attachment authorization. Download and delete live at /files/:id — a flat path with no
// :entity segment — so the route-level ACL stage cannot run on them and the resolved access level
// never reached the handler. scopeOf() correctly failed CLOSED, so EVERY non-admin got 404 on
// EVERY attachment while upload worked, because upload rides the record group.
//
// The package's fakeGuard ignores the scope entirely, so it cannot show which scope arrived. These
// tests use a guard that RECORDS the scope of every call — which is also what caught the second,
// worse half of the bug: delete authorized with the resolved scope and then wrote the record's
// reference array with the unresolved one, so the file was already gone by the time the update
// failed and the record kept listing a phantom attachment.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// scopeSpyGuard records the scope of each call and can refuse when the scope sees no rows —
// modelling the real row-scoped read rather than accepting anything.
type scopeSpyGuard struct {
	resolveScopes []services.RecordScope
	setRefsScopes []services.RecordScope
}

func (g *scopeSpyGuard) ResolveFileField(_ context.Context, sc services.RecordScope, _, entityName, _, _ string) (string, error) {
	g.resolveScopes = append(g.resolveScopes, sc)
	if !sc.IsAdmin && sc.AccessLevel == string(dynrepo.AccessNone) {
		return "", services.ErrRecordNotFound // what a row-scoped read really answers
	}
	return entityName, nil
}

func (g *scopeSpyGuard) SetFileRefs(_ context.Context, sc services.RecordScope, _, _, _, _ string, _ []map[string]any) error {
	g.setRefsScopes = append(g.setRefsScopes, sc)
	if !sc.IsAdmin && sc.AccessLevel == string(dynrepo.AccessNone) {
		return services.ErrRecordNotFound
	}
	return nil
}

// fakeACL answers per (entity, action) so a test can grant view but not delete.
type fakeACL struct {
	allow map[string]string // "entity|action" → access level
	calls []string
}

func (a *fakeACL) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	key := entity + "|" + action
	a.calls = append(a.calls, key)
	lvl, ok := a.allow[key]
	if !ok {
		return false, "", nil
	}
	return true, lvl, nil
}

func TestScopeFor_ResolvesTheParentEntitysGrant(t *testing.T) {
	acl := &fakeACL{allow: map[string]string{"crm_contracts|view": "all"}}
	s := (&Service{}).WithACL(acl)
	// The caller arrives with AccessNone because no ACL stage could run on /files/:id.
	arriving := services.RecordScope{UserID: "u1", RoleID: "r1", AccessLevel: string(dynrepo.AccessNone)}

	got := s.scopeFor(context.Background(), arriving, "t1", "crm_contracts", "view")
	if got.AccessLevel != "all" {
		t.Fatalf("AccessLevel = %q, want the grant's level — this is the whole bug", got.AccessLevel)
	}
	if got.UserID != "u1" || got.RoleID != "r1" {
		t.Errorf("identity mangled: %+v", got)
	}

	// No grant for delete → stays closed, so the parent-record check answers 404 exactly like the
	// record read. A refusal must not become a different shape here.
	if lvl := s.scopeFor(context.Background(), arriving, "t1", "crm_contracts", "edit").AccessLevel; lvl != string(dynrepo.AccessNone) {
		t.Errorf("ungranted action resolved to %q, want none", lvl)
	}
}

// TestScopeFor_SharesOnlyOnView (specs/051-record-manual-share): a download is a read of the parent,
// so a record shared with the caller must open its attachments like GET /:entity/:id does; an
// upload (edit) or a delete is a write and never rides a share, even when the caller arrived with
// the flag set.
func TestScopeFor_SharesOnlyOnView(t *testing.T) {
	acl := &fakeACL{allow: map[string]string{"c|view": "owner", "c|edit": "owner", "c|delete": "owner"}}
	s := (&Service{}).WithACL(acl)
	in := services.RecordScope{UserID: "u1", RoleID: "r1", AccessLevel: string(dynrepo.AccessNone), IncludeShares: true}
	for action, want := range map[string]bool{"view": true, "edit": false, "delete": false} {
		if got := s.scopeFor(context.Background(), in, "t1", "c", action).IncludeShares; got != want {
			t.Errorf("%s: IncludeShares = %v, want %v", action, got, want)
		}
	}
}

func TestScopeFor_AdminIsUntouchedAndCostsNoQuery(t *testing.T) {
	acl := &fakeACL{allow: map[string]string{}}
	s := (&Service{}).WithACL(acl)
	in := services.RecordScope{UserID: "root", IsAdmin: true, AccessLevel: "all"}

	if got := s.scopeFor(context.Background(), in, "t1", "anything", "view"); got != in {
		t.Errorf("admin scope changed: %+v", got)
	}
	if len(acl.calls) != 0 {
		t.Errorf("admin cost %d ACL queries, want 0 — is_admin already bypasses ACL", len(acl.calls))
	}
}

// TestScopeFor_UnwiredFailsOpenToTheCallersScope: an unwired resolver must not silently deny
// everything. It returns the scope untouched, which is the pre-existing behaviour, so a wiring
// mistake is a visible regression rather than a hidden lockout with no explanation.
func TestScopeFor_UnwiredKeepsTheCallersScope(t *testing.T) {
	s := &Service{}
	in := services.RecordScope{UserID: "u1", AccessLevel: "owner"}
	if got := s.scopeFor(context.Background(), in, "t1", "e", "view"); got != in {
		t.Errorf("scope changed with no resolver wired: %+v", got)
	}
}

// TestDelete_AuthorizesAndWritesWithTheSAMEScope is the regression guard for the half-completed
// delete: the blob and the row went away and only then did the record update fail, leaving a
// phantom attachment. Both calls must carry the resolved scope.
func TestDelete_AuthorizesAndWritesWithTheSameScope(t *testing.T) {
	guard := &scopeSpyGuard{}
	// specs/060 (#348): detaching a file is "may this person change this record" — edit, the same
	// floor as upload. The grant shape is the technician's: edit (own rows), NO delete at all.
	acl := &fakeACL{allow: map[string]string{"orders|edit": "owner"}}
	backend := &fakeBackend{}
	s, rec := newTestService(t, nil, backend, Limits{MimeOK: allowAllMime})
	s.guard = guard
	s.WithACL(acl)
	rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil) // GetByID
	rec.queueExec(1, nil)                                                                    // SoftDelete
	rec.queueQuery(fileCols(), nil, nil)                                                     // ListByRecordField

	err := s.Delete(context.Background(), services.RecordScope{
		UserID: "u1", RoleID: "r1", AccessLevel: string(dynrepo.AccessNone),
	}, "tenant-a", "file-1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(guard.resolveScopes) != 1 || len(guard.setRefsScopes) != 1 {
		t.Fatalf("calls: resolve=%d setRefs=%d, want one each", len(guard.resolveScopes), len(guard.setRefsScopes))
	}
	if guard.resolveScopes[0].AccessLevel != "owner" {
		t.Errorf("authorize saw %q, want the resolved edit level", guard.resolveScopes[0].AccessLevel)
	}
	if guard.setRefsScopes[0].AccessLevel != "owner" {
		t.Errorf("the record write saw %q — this is what left phantom attachments", guard.setRefsScopes[0].AccessLevel)
	}
}

func TestDelete_WithoutTheEditGrantRefusesBeforeTouchingAnything(t *testing.T) {
	guard := &scopeSpyGuard{}
	// view + delete but NO edit: since #348 the floor is edit, so a delete grant alone no longer
	// detaches a file (it rewrites the record's file-ref array, which is an edit).
	acl := &fakeACL{allow: map[string]string{"orders|view": "all", "orders|delete": "all"}}
	backend := &fakeBackend{}
	s, rec := newTestService(t, nil, backend, Limits{MimeOK: allowAllMime})
	s.guard = guard
	s.WithACL(acl)
	rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil) // GetByID

	err := s.Delete(context.Background(), services.RecordScope{
		UserID: "u1", RoleID: "r1", AccessLevel: string(dynrepo.AccessNone),
	}, "tenant-a", "file-1")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound (the record read's own answer, no existence leak)", err)
	}
	if len(guard.setRefsScopes) != 0 {
		t.Error("the record was written despite the refusal")
	}
	if len(backend.deletedKeys()) != 0 {
		t.Error("the blob was removed despite the refusal")
	}
}

// TestUpload_ResolvesTheEDITGrant_NotTheRouteCreateGrant is the regression guard for a privilege
// mismatch that shipped with a comment asserting the opposite.
//
// POST /:entity/:id/files was mounted on the record group, where middleware/acl.go maps POST→"create"
// (it rides the edit-pinned group since specs/060; the service must stay right whatever the mount).
// But uploading MUTATES an existing record: the parent's file-ref array is rewritten. So a role
// holding create:all and edit:own arrived with all-rows authority over a write that should have
// been own-rows only — and that is not a hypothetical grant shape, it is the one the stock CRM pack
// ships. Open and Delete already re-resolved (view / delete); upload was the single call site that
// trusted the route's action.
func TestUpload_ResolvesTheEditGrantNotTheRouteCreateGrant(t *testing.T) {
	guard := &scopeSpyGuard{}
	// The dangerous shape, stated explicitly: broad create, narrow edit.
	acl := &fakeACL{allow: map[string]string{
		"orders|create": "all",
		"orders|edit":   "owner",
	}}
	backend := &fakeBackend{}
	s, _ := newTestService(t, nil, backend, Limits{MimeOK: allowAllMime})
	s.guard = guard
	s.WithACL(acl)

	// The caller arrives the way the route hands it over: create-level authority.
	_, _ = s.Upload(context.Background(), services.RecordScope{
		UserID: "u1", RoleID: "r1", AccessLevel: "all",
	}, UploadInput{
		TenantID: "tenant-a", EntityName: "orders", RecordID: "rec-1", FieldName: "avatar",
		Mime: "image/png", Size: 3, Filename: "a.png", Content: strings.NewReader("png"),
	})

	// What matters is which QUESTION was asked of the ACL, before anything was stored.
	asked := strings.Join(acl.calls, ",")
	if !strings.Contains(asked, "orders|edit") {
		t.Fatalf("upload never resolved the edit grant (ACL asked: %v) — it is writing a record at the create grant's scope", acl.calls)
	}
	if len(guard.resolveScopes) == 0 {
		t.Fatal("guard was never called")
	}
	if got := guard.resolveScopes[0].AccessLevel; got != "owner" {
		t.Errorf("record probe ran at %q; want \"owner\" — the EDIT grant's level, not the route's create grant", got)
	}
}
