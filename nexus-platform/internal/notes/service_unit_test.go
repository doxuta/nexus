package notes

// Default-build unit tests for the notes Service business logic (RED-first — written before
// service.go). The RecordGuard (services.RecordService.ResolveNoteTarget) and EntityAuthorizer
// (services.ACLService.Authorize) seams are faked in-memory; the sys_record_notes Repository runs
// over the fake sqlx driver (fakedb_test.go). Covered per the task's mandatory test list:
//   - ACL: a caller the guard denies never sees/writes notes (List/Create/Delete all propagate the
//     guard's error verbatim — the SAME not-found the parent record GET would give).
//   - Delete permission: the note's own author may always delete it; a non-author needs the
//     entity's "delete" ACL grant (checked via EntityAuthorizer), admin always bypasses, and a nil
//     authorizer fails closed (author-only, never author-or-anyone).
//   - Validation: empty (post-trim) body → RECORD_NOTE_EMPTY; over MaxBodyRunes → RECORD_NOTE_TOO_LONG.
//   - created_by is always the VERIFIED scope.UserID, never a client-supplied value.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/services"
)

// ---- fakes -------------------------------------------------------------------------------------

// fakeGuard fakes services.RecordService.ResolveNoteTarget: it records every call and can be
// programmed to deny (simulating "caller cannot view this record").
type fakeGuard struct {
	mu         sync.Mutex
	entity     string // canonical entity name to return (defaults to the input name)
	resolveErr error
	// onlyViaShare: the record is visible to the caller ONLY through a manual share
	// (specs/051) — a scope without IncludeShares gets ErrRecordNotFound.
	onlyViaShare bool
	calls        int
	gotScope     services.RecordScope
	gotEntity    string
	gotRecordID  string
}

func (g *fakeGuard) ResolveNoteTarget(_ context.Context, scope services.RecordScope, _, entityName, recordID string) (string, services.RecordScope, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.gotScope = scope
	g.gotEntity = entityName
	g.gotRecordID = recordID
	if g.resolveErr != nil {
		return "", services.RecordScope{}, g.resolveErr
	}
	if g.onlyViaShare && !scope.IncludeShares {
		return "", services.RecordScope{}, services.ErrRecordNotFound // reachable ONLY through a manual share
	}
	canonical := entityName
	if g.entity != "" {
		canonical = g.entity
	}
	// The "view"-resolved scope: AccessLevel flipped to "all" (as AuthorizeView would for an
	// allowed non-admin grant) so fakeGuard mirrors production shape even though the fake never
	// actually inspects the returned scope's AccessLevel.
	viewScope := scope
	viewScope.AccessLevel = "all"
	return canonical, viewScope, nil
}

// fakeAuthz fakes services.ACLService.Authorize for the delete path's "OR has the entity's delete
// grant" override.
type fakeAuthz struct {
	mu      sync.Mutex
	allowed bool
	level   string
	err     error
	calls   int
	gotArgs [4]string // tenantID, roleID, entity, action of the LAST call
}

func (a *fakeAuthz) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.gotArgs = [4]string{tenantID, roleID, entity, action}
	return a.allowed, a.level, a.err
}

func newTestService(t *testing.T, guard *fakeGuard, authz EntityAuthorizer) (*Service, *fakeDB) {
	t.Helper()
	db, rec := newFakeDB(t)
	id := 0
	svc := NewService(NewRepository(db), guard, authz)
	svc.newID = func() string { id++; return "note-gen-" + itoa(id) }
	return svc, rec
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

var testScope = services.RecordScope{UserID: "user-1", RoleID: "role-1", AccessLevel: "create"}

// ---- List ---------------------------------------------------------------------------------------

func TestService_List(t *testing.T) {
	t.Run("guard từ chối → lỗi truyền nguyên vẹn (không lộ record)", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		svc, _ := newTestService(t, guard, nil)
		_, _, err := svc.List(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", 1, 20)
		if !errors.Is(err, services.ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("guard cho qua entity không tồn tại → ErrEntityNotFound truyền nguyên vẹn", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrEntityNotFound}
		svc, _ := newTestService(t, guard, nil)
		_, _, err := svc.List(context.Background(), testScope, "tenant-a", "unknown_entity", "rec-1", 1, 20)
		if !errors.Is(err, services.ErrEntityNotFound) {
			t.Fatalf("err = %v; want ErrEntityNotFound", err)
		}
	})

	t.Run("happy path trả đúng trang + tổng", func(t *testing.T) {
		guard := &fakeGuard{}
		svc, rec := newTestService(t, guard, nil)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "xin chào")}, nil)
		rec.queueQuery([]string{"count"}, [][]driver.Value{{int64(1)}}, nil)
		rows, total, err := svc.List(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", 1, 20)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 1 || len(rows) != 1 || rows[0].ID != "note-1" {
			t.Errorf("List = (%+v, %d); want 1 row, total 1", rows, total)
		}
		if guard.calls != 1 || guard.gotEntity != "crm_accounts" || guard.gotRecordID != "rec-1" {
			t.Errorf("guard called with entity=%q record=%q calls=%d; want crm_accounts/rec-1/1",
				guard.gotEntity, guard.gotRecordID, guard.calls)
		}
	})

	t.Run("limit vượt trần bị kẹp (không quét không giới hạn)", func(t *testing.T) {
		guard := &fakeGuard{}
		svc, rec := newTestService(t, guard, nil)
		rec.queueQuery(noteCols(), nil, nil)                                 // ListByRecord (SELECT)
		rec.queueQuery([]string{"count"}, [][]driver.Value{{int64(0)}}, nil) // CountByRecord
		if _, _, err := svc.List(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", 1, 100000); err != nil {
			t.Fatalf("List: %v", err)
		}
		call := rec.queryCalls[0] // the FIRST query is ListByRecord — CountByRecord ran after it
		// ClampPageLimit caps at 100 — the 5th bind arg is LIMIT.
		if len(call.args) < 4 || call.args[3] != int64(100) {
			t.Errorf("LIMIT arg = %v; want clamped to 100 (args=%v)", call.args[3], call.args)
		}
	})
}

// ---- Create -------------------------------------------------------------------------------------

func TestService_Create(t *testing.T) {
	t.Run("rỗng sau trim → RECORD_NOTE_EMPTY, không đụng guard/repo", func(t *testing.T) {
		guard := &fakeGuard{}
		svc, rec := newTestService(t, guard, nil)
		_, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "   \n\t  ")
		assertValidationCode(t, err, "RECORD_NOTE_EMPTY")
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert was called on an empty body")
		}
	})

	t.Run("vượt quá độ dài tối đa → RECORD_NOTE_TOO_LONG", func(t *testing.T) {
		guard := &fakeGuard{}
		svc, rec := newTestService(t, guard, nil)
		long := strings.Repeat("a", MaxBodyRunes+1)
		_, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", long)
		assertValidationCode(t, err, "RECORD_NOTE_TOO_LONG")
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert was called on an over-length body")
		}
	})

	t.Run("đúng bằng trần độ dài → cho qua", func(t *testing.T) {
		guard := &fakeGuard{}
		svc, _ := newTestService(t, guard, nil)
		exact := strings.Repeat("a", MaxBodyRunes)
		if _, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", exact); err != nil {
			t.Errorf("Create at exactly MaxBodyRunes: %v; want ok", err)
		}
	})

	// DAILY-QUEUE #225: the POST route does not carry the share flag, so a share-only viewer used to
	// get the same 404 as a stranger. They already read the record, so tell them it is read-only.
	t.Run("người chỉ thấy hồ sơ qua chia sẻ tạo ghi chú → 403 chỉ xem, không ghi", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts", onlyViaShare: true}
		svc, rec := newTestService(t, guard, nil)
		_, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "nội dung hợp lệ")
		if !errors.Is(err, services.ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want ErrRecordSharedReadOnly", err)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert ran for a share-only caller")
		}
	})

	t.Run("người lạ (không thấy cả khi tính chia sẻ) tạo ghi chú → vẫn 404", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		svc, _ := newTestService(t, guard, nil)
		if _, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "x"); !errors.Is(err, services.ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("guard từ chối → lỗi truyền nguyên vẹn, không ghi", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		svc, rec := newTestService(t, guard, nil)
		_, err := svc.Create(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "nội dung hợp lệ")
		if !errors.Is(err, services.ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert was called despite guard denial")
		}
	})

	t.Run("happy path: created_by LUÔN LÀ scope.UserID thật, body được trim", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		svc, rec := newTestService(t, guard, nil)
		got, err := svc.Create(context.Background(), testScope, "tenant-a", "CRM_Accounts", "rec-1", "  đã gọi, khách hẹn tuần sau  ")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.CreatedBy != testScope.UserID {
			t.Errorf("CreatedBy = %q; want the verified scope.UserID %q (never client-supplied)", got.CreatedBy, testScope.UserID)
		}
		if got.Body != "đã gọi, khách hẹn tuần sau" {
			t.Errorf("Body = %q; want trimmed", got.Body)
		}
		if got.EntityName != "crm_accounts" {
			t.Errorf("EntityName = %q; want the GUARD-resolved canonical name, not the raw input", got.EntityName)
		}
		if got.ID == "" {
			t.Errorf("Create returned an empty id")
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "INSERT INTO sys_record_notes")
	})

	// specs/060 (#349): the POST route is now pinned to the "view" floor, whose scope arrives with
	// IncludeShares=true (dynamic_record_handler scopeOf). A manual share is READ-ONLY (specs/051), so
	// a note — a write on the record's conversation — must still need the record WITHOUT the share.
	t.Run("người chỉ thấy hồ sơ qua chia sẻ ghi chú → 403 chỉ xem, không ghi", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts", onlyViaShare: true}
		shared := services.RecordScope{UserID: "user-2", RoleID: "role-2", AccessLevel: "owner", IncludeShares: true}
		svc, rec := newTestService(t, guard, nil)
		_, err := svc.Create(context.Background(), shared, "tenant-a", "crm_accounts", "rec-1", "nội dung hợp lệ")
		if !errors.Is(err, services.ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want ErrRecordSharedReadOnly", err)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert ran for a share-only caller")
		}
	})

	t.Run("hồ sơ của mình (view, có cờ chia sẻ trong scope) → ghi được, guard thấy scope KHÔNG cờ chia sẻ", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"} // visible without any share
		own := services.RecordScope{UserID: "user-1", RoleID: "role-1", AccessLevel: "owner", IncludeShares: true}
		svc, rec := newTestService(t, guard, nil)
		if _, err := svc.Create(context.Background(), own, "tenant-a", "crm_accounts", "rec-1", "ghi chú trên phiếu của tôi"); err != nil {
			t.Fatalf("Create on own record: %v", err)
		}
		if guard.gotScope.IncludeShares {
			t.Error("the write-path guard call carried IncludeShares; a share must never authorize a note")
		}
		mustContainSQL(t, rec.lastExec(t).query, "INSERT INTO sys_record_notes")
	})

	t.Run("hồ sơ không tồn tại / ngoài phạm vi, không có chia sẻ → vẫn 404 nguyên văn", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		scoped := services.RecordScope{UserID: "user-1", RoleID: "role-1", AccessLevel: "owner", IncludeShares: true}
		svc, rec := newTestService(t, guard, nil)
		_, err := svc.Create(context.Background(), scoped, "tenant-a", "crm_accounts", "rec-1", "nội dung hợp lệ")
		if !errors.Is(err, services.ErrRecordNotFound) || errors.Is(err, services.ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want plain ErrRecordNotFound (no existence oracle)", err)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("repo.Insert ran despite guard denial")
		}
	})
}

// ---- Delete ---------------------------------------------------------------------------------------

func TestService_Delete(t *testing.T) {
	t.Run("guard từ chối → lỗi truyền nguyên vẹn, không đụng note/authz", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		authz := &fakeAuthz{allowed: true}
		svc, _ := newTestService(t, guard, authz)
		err := svc.Delete(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "note-1")
		if !errors.Is(err, services.ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
		if authz.calls != 0 {
			t.Errorf("authz consulted despite guard denial (calls=%d)", authz.calls)
		}
	})

	t.Run("note không tồn tại / thuộc record khác → ErrNoteNotFound", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		svc, rec := newTestService(t, guard, nil)
		rec.queueQuery(nil, nil, nil) // GetForRecord → zero rows → sql.ErrNoRows path
		err := svc.Delete(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "note-x")
		if !errors.Is(err, ErrNoteNotFound) {
			t.Fatalf("err = %v; want ErrNoteNotFound", err)
		}
	})

	t.Run("tác giả tự xoá ghi chú của mình → cho qua, KHÔNG cần hỏi authz", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		authz := &fakeAuthz{allowed: false} // even if it WOULD deny, author bypass must win
		svc, rec := newTestService(t, guard, authz)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của tôi")}, nil) // created_by=user-1
		if err := svc.Delete(context.Background(), testScope, "tenant-a", "crm_accounts", "rec-1", "note-1"); err != nil {
			t.Fatalf("Delete (author): %v", err)
		}
		if authz.calls != 0 {
			t.Errorf("authz consulted for the note's own author (calls=%d); author bypass must short-circuit", authz.calls)
		}
		delCall := rec.lastExec(t)
		mustContainSQL(t, delCall.query, "UPDATE sys_record_notes SET deleted_at = ?")
	})

	t.Run("người khác không có quyền delete trên entity → FORBIDDEN 403, không xoá", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		authz := &fakeAuthz{allowed: false}
		other := services.RecordScope{UserID: "user-2", RoleID: "role-2", AccessLevel: "create"}
		svc, rec := newTestService(t, guard, authz)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của user-1")}, nil)
		err := svc.Delete(context.Background(), other, "tenant-a", "crm_accounts", "rec-1", "note-1")
		assertValidationCode(t, err, "FORBIDDEN")
		if authz.calls != 1 || authz.gotArgs[3] != "delete" {
			t.Errorf("authz call = %+v (calls=%d); want ONE call with action=delete", authz.gotArgs, authz.calls)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("SoftDelete exec ran despite FORBIDDEN")
		}
	})

	t.Run("người khác có quyền delete trên entity (vd Quản lý) → cho qua", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		authz := &fakeAuthz{allowed: true, level: "all"}
		other := services.RecordScope{UserID: "user-2", RoleID: "role-manager", AccessLevel: "all"}
		svc, rec := newTestService(t, guard, authz)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của user-1")}, nil)
		if err := svc.Delete(context.Background(), other, "tenant-a", "crm_accounts", "rec-1", "note-1"); err != nil {
			t.Fatalf("Delete (manager override): %v", err)
		}
		if authz.calls != 1 {
			t.Errorf("authz calls = %d; want 1", authz.calls)
		}
	})

	// specs/051: a manual share is read-only. The delete route is pinned to "view", so a share
	// recipient reaches the record — but removing SOMEONE ELSE's note is a write on the record's
	// conversation, so even a delete-grant holder must reach the record without the share.
	t.Run("người chỉ thấy hồ sơ qua chia sẻ xoá ghi chú của người khác → 403 chỉ xem, không xoá", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts", onlyViaShare: true}
		authz := &fakeAuthz{allowed: true, level: "owner"} // would allow on grant alone
		shared := services.RecordScope{UserID: "user-2", RoleID: "role-2", AccessLevel: "owner", IncludeShares: true}
		svc, rec := newTestService(t, guard, authz)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của user-1")}, nil)
		err := svc.Delete(context.Background(), shared, "tenant-a", "crm_accounts", "rec-1", "note-1")
		if !errors.Is(err, services.ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want ErrRecordSharedReadOnly", err)
		}
		if len(rec.execCalls) != 0 {
			t.Errorf("SoftDelete exec ran for a share-only caller")
		}
	})

	t.Run("người chỉ thấy qua chia sẻ vẫn xoá được ghi chú CỦA MÌNH", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts", onlyViaShare: true}
		author := services.RecordScope{UserID: "user-1", RoleID: "role-2", AccessLevel: "owner", IncludeShares: true}
		svc, rec := newTestService(t, guard, nil)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của tôi")}, nil) // created_by=user-1
		if err := svc.Delete(context.Background(), author, "tenant-a", "crm_accounts", "rec-1", "note-1"); err != nil {
			t.Fatalf("Delete (author via share): %v", err)
		}
	})

	t.Run("admin luôn xoá được, không hỏi authz", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		authz := &fakeAuthz{allowed: false}
		admin := services.RecordScope{UserID: "user-admin", IsAdmin: true}
		svc, rec := newTestService(t, guard, authz)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của user-1")}, nil)
		if err := svc.Delete(context.Background(), admin, "tenant-a", "crm_accounts", "rec-1", "note-1"); err != nil {
			t.Fatalf("Delete (admin): %v", err)
		}
		if authz.calls != 0 {
			t.Errorf("authz consulted for admin (calls=%d); admin must bypass entirely", authz.calls)
		}
	})

	t.Run("authz chưa wire (nil) + không phải tác giả → an toàn mặc định: từ chối", func(t *testing.T) {
		guard := &fakeGuard{entity: "crm_accounts"}
		other := services.RecordScope{UserID: "user-2", RoleID: "role-2"}
		svc, rec := newTestService(t, guard, nil) // authz == nil
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "của user-1")}, nil)
		err := svc.Delete(context.Background(), other, "tenant-a", "crm_accounts", "rec-1", "note-1")
		assertValidationCode(t, err, "FORBIDDEN")
	})
}

// assertValidationCode fails unless err is a *services.ValidationError carrying the given code.
func assertValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	var ve *services.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T); want *services.ValidationError code=%s", err, err, code)
	}
	if ve.Code != code {
		t.Errorf("code = %q; want %q", ve.Code, code)
	}
}
