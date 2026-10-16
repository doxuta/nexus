package services

// Unit tests for RecordService.SetOwner (TRỤ 2 mục 2 "Giao việc & đổi chủ",
// docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 2). Reuses newUserFieldService/fakeUserStore
// (record_user_target_test.go) — the SAME fixture the `user` field's BatchActiveUserExists
// probe is unit-tested with, so "who counts as a valid new owner" is exercised through the
// IDENTICAL fake as checkUserTargets. The row-visibility claim itself ("chủ cũ hết thấy, chủ
// mới thấy được" — a real SQL-predicate behavior) is proven against MySQL by
// internal/dynrepo/record_owner_integration_test.go; this file locks the AUTHORIZATION decision
// and the userId validation, which are pure Go logic.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func ownerScope(userID, accessLevel string, isAdmin bool) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: accessLevel, IsAdmin: isAdmin}
}

// TestRecordService_SetOwner_RequiresManagerOrAdmin locks the spec's authorization line: "chỉ
// người có quyền edit ở mức all... Người thường -> 403" — a regular staff member is refused
// EVEN when access_level is "owner"/"team" (i.e., even over a record they themselves can
// currently edit), because reassignment is a management action. Only access_level "all" or an
// admin gets past this gate.
func TestRecordService_SetOwner_RequiresManagerOrAdmin(t *testing.T) {
	tests := []struct {
		name     string
		scope    RecordScope
		wantCode string // "" = no ACL-gate error (request proceeds past this check)
	}{
		{"regular staff owner-level -> 403", ownerScope("u-staff", "owner", false), "FORBIDDEN"},
		{"regular staff team-level -> 403", ownerScope("u-staff", "team", false), "FORBIDDEN"},
		{"regular staff no grant -> 403", ownerScope("u-staff", "", false), "FORBIDDEN"},
		{"manager all-level -> passes gate", ownerScope("u-mgr", "all", false), ""},
		{"admin (any access level) -> passes gate", ownerScope("u-admin", "owner", true), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeUserStore{activeUsers: map[string]bool{userKey("tenant-1", "new-owner"): true}}
			svc := newUserFieldService(store)
			err := svc.SetOwner(context.Background(), tc.scope, "tenant-1", "tickets", "rec-1", "new-owner")
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if tc.wantCode == "FORBIDDEN" && store.setOwnerCalls != 0 {
				t.Fatalf("store.SetOwner called %d times; a 403 must never reach the store", store.setOwnerCalls)
			}
		})
	}
}

// TestRecordService_SetOwner_UserIdValidation locks the 422 RECORD_OWNER_INVALID contract:
// empty userId and an id BatchActiveUserExists does not resolve (unknown / cross-tenant /
// disabled — collapsed into one "doesn't count" outcome, mirroring RECORD_USER_INVALID) are
// BOTH rejected with the same code + details.field="userId", and neither reaches the store.
func TestRecordService_SetOwner_UserIdValidation(t *testing.T) {
	tests := []struct {
		name        string
		newOwnerID  string
		activeUsers map[string]bool
		wantCode    string
	}{
		{"empty userId", "", nil, "RECORD_OWNER_INVALID"},
		{"whitespace-only userId", "   ", nil, "RECORD_OWNER_INVALID"},
		{"unknown userId", "ghost", map[string]bool{}, "RECORD_OWNER_INVALID"},
		{"cross-tenant/disabled userId (fake reports false)", "u-other", map[string]bool{userKey("tenant-1", "u-other"): false}, "RECORD_OWNER_INVALID"},
		{"valid active userId", "u-valid", map[string]bool{userKey("tenant-1", "u-valid"): true}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeUserStore{activeUsers: tc.activeUsers}
			svc := newUserFieldService(store)
			err := svc.SetOwner(context.Background(), ownerScope("u-mgr", "all", false), "tenant-1", "tickets", "rec-1", tc.newOwnerID)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if ve := new(ValidationError); tc.wantCode != "" && errors.As(err, &ve) && ve.Field != "userId" {
				t.Fatalf("Field = %q; want userId", ve.Field)
			}
		})
	}
}

// TestRecordService_SetOwner_Success proves a valid manager-initiated reassignment reaches the
// store with the RIGHT args (entity, id, actor, new owner) and never touches created_by (the
// store call carries newOwnerID as a SEPARATE argument from the actor/modifier id).
func TestRecordService_SetOwner_Success(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{userKey("tenant-1", "new-owner"): true}}
	svc := newUserFieldService(store)
	err := svc.SetOwner(context.Background(), ownerScope("u-mgr", "all", false), "tenant-1", "tickets", "rec-1", "new-owner")
	if err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	if store.setOwnerCalls != 1 {
		t.Fatalf("setOwnerCalls = %d; want 1", store.setOwnerCalls)
	}
	if store.gotNewOwnerID != "new-owner" {
		t.Fatalf("gotNewOwnerID = %q; want new-owner", store.gotNewOwnerID)
	}
}

// TestRecordService_SetOwner_RecordNotFound proves the store's ErrRecordNotFound (no active
// record matched — including a record the row-scope may not see) surfaces as the SAME
// services.ErrRecordNotFound sentinel every other record op maps to 404.
func TestRecordService_SetOwner_RecordNotFound(t *testing.T) {
	store := &fakeUserStore{
		activeUsers: map[string]bool{userKey("tenant-1", "new-owner"): true},
		setOwnerErr: dynrepo.ErrRecordNotFound,
	}
	svc := newUserFieldService(store)
	err := svc.SetOwner(context.Background(), ownerScope("u-mgr", "all", false), "tenant-1", "tickets", "rec-missing", "new-owner")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("err = %v; want ErrRecordNotFound", err)
	}
}

// TestRecordService_SetOwner_UnknownEntity proves resolve() runs before anything else — an
// unknown entity is ErrEntityNotFound regardless of the caller's access level (a 403-worthy
// scope must not leak entity existence, and an existence check must not run before resolve()).
func TestRecordService_SetOwner_UnknownEntity(t *testing.T) {
	cat := &fakeRecordCatalog{entityErr: metadata.ErrEntityNotFound}
	store := &fakeUserStore{}
	svc := NewRecordService(cat, store)
	err := svc.SetOwner(context.Background(), ownerScope("u-mgr", "all", false), "tenant-1", "no_such_entity", "rec-1", "new-owner")
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("err = %v; want ErrEntityNotFound", err)
	}
}
