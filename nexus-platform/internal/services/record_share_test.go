package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// shareOnlyStore models a record the caller's own scope cannot reach but that is shared with them
// (specs/051-record-manual-share): a READ with IncludeShares finds it, anything else — every write,
// and every read without the flag — is not-found. It records the scopes it was handed so the tests
// can prove no write path ever carried the share flag.
type shareOnlyStore struct {
	bulkTestStore
	shared      bool // is the record shared with the caller?
	writeScopes []dynrepo.RowScope
	readScopes  []dynrepo.RowScope
}

func (s *shareOnlyStore) GetRecord(_ context.Context, _, _, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	s.readScopes = append(s.readScopes, scope)
	if s.shared && scope.IncludeShares {
		return &dynrepo.Record{ID: id, Data: map[string]any{"name": "shared"}}, nil
	}
	return nil, dynrepo.ErrRecordNotFound
}
func (s *shareOnlyStore) UpdateRecord(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.writeScopes = append(s.writeScopes, scope)
	return dynrepo.ErrRecordNotFound
}
func (s *shareOnlyStore) SoftDeleteRecord(_ context.Context, _, _, _ string, scope dynrepo.RowScope) error {
	s.writeScopes = append(s.writeScopes, scope)
	return dynrepo.ErrRecordNotFound
}

// levelAuthz answers every Authorize with allow + a fixed level and remembers the actions asked.
type levelAuthz struct {
	level   string
	deny    map[string]bool
	actions []string
}

func (a *levelAuthz) Authorize(_ context.Context, _, _, _, action string) (bool, string, error) {
	a.actions = append(a.actions, action)
	return !a.deny[action], a.level, nil
}

func shareTestService(store RecordStore) *RecordService {
	cat := &bulkCatalog{entity: bulkEntityFixture(), fields: nil}
	return NewRecordService(cat, store)
}

// TestRecordShare_WriteOnSharedRecordIs403 proves Q4(b) "chia sẻ = CHỈ ĐỌC" at the service seam: a
// PUT/DELETE on a record the caller sees ONLY through a share answers RECORD_SHARED_READ_ONLY (403)
// instead of the bare 404, while a record that is not shared keeps the 404 — and the write itself
// never ran with the share flag.
func TestRecordShare_WriteOnSharedRecordIs403(t *testing.T) {
	scope := RecordScope{UserID: "u-b", AccessLevel: "owner", RoleID: "r1"}
	cases := []struct {
		name   string
		shared bool
		call   func(s *RecordService) error
		want   error
	}{
		{"PUT shared → 403", true, func(s *RecordService) error {
			return s.Update(context.Background(), scope, "tenant-1", "widgets", "rec-1", map[string]any{}, nil)
		}, ErrRecordSharedReadOnly},
		{"DELETE shared → 403", true, func(s *RecordService) error {
			return s.Delete(context.Background(), scope, "tenant-1", "widgets", "rec-1")
		}, ErrRecordSharedReadOnly},
		{"PUT not shared → 404", false, func(s *RecordService) error {
			return s.Update(context.Background(), scope, "tenant-1", "widgets", "rec-1", map[string]any{}, nil)
		}, ErrRecordNotFound},
		{"DELETE not shared → 404", false, func(s *RecordService) error {
			return s.Delete(context.Background(), scope, "tenant-1", "widgets", "rec-1")
		}, ErrRecordNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &shareOnlyStore{shared: tc.shared}
			err := tc.call(shareTestService(store))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
			for _, ws := range store.writeScopes {
				if ws.IncludeShares {
					t.Fatalf("write ran with IncludeShares=true: %+v", ws)
				}
			}
		})
	}

	t.Run("admin never gets the share probe", func(t *testing.T) {
		store := &shareOnlyStore{shared: true}
		admin := RecordScope{UserID: "u-a", AccessLevel: "all", IsAdmin: true}
		if err := shareTestService(store).Delete(context.Background(), admin, "tenant-1", "widgets", "rec-1"); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("admin delete err = %v; want ErrRecordNotFound", err)
		}
	})
}

// TestRecordScope_ToRowScopeCarriesIncludeShares — the flag the handler sets for a VIEW request
// reaches the repository unchanged.
func TestRecordScope_ToRowScopeCarriesIncludeShares(t *testing.T) {
	if !(RecordScope{UserID: "u", AccessLevel: "owner", IncludeShares: true}).toRowScope().IncludeShares {
		t.Fatal("toRowScope dropped IncludeShares")
	}
	if (RecordScope{UserID: "u", AccessLevel: "owner"}).toRowScope().IncludeShares {
		t.Fatal("toRowScope invented IncludeShares")
	}
}

// TestResolveShareTarget covers the manage-shares gate (Q4a "ai có quyền SỬA hồ sơ đó được chia
// sẻ"): the EDIT grant is re-resolved whatever the route mounted, the record is read under that
// edit scope WITHOUT shares, and a caller who only sees it through a share gets 403.
func TestResolveShareTarget(t *testing.T) {
	ctx := context.Background()
	viewScope := RecordScope{UserID: "u-b", AccessLevel: "all", RoleID: "r1", IncludeShares: true}

	t.Run("editor in scope → canonical entity name", func(t *testing.T) {
		store := &editableStore{}
		authz := &levelAuthz{level: "owner"}
		name, err := shareTestService(store).WithACLAuthorizer(authz).ResolveShareTarget(ctx, viewScope, "tenant-1", "widgets", "rec-1")
		if err != nil || name != "widgets" {
			t.Fatalf("got (%q, %v); want (widgets, nil)", name, err)
		}
		if len(authz.actions) != 1 || authz.actions[0] != "edit" {
			t.Fatalf("authorized actions = %v; want [edit]", authz.actions)
		}
		if len(store.readScopes) != 1 || store.readScopes[0].IncludeShares || store.readScopes[0].Level != dynrepo.AccessOwner {
			t.Fatalf("record read with %+v; want the EDIT level, no shares", store.readScopes)
		}
	})

	t.Run("no edit grant → FORBIDDEN before any read", func(t *testing.T) {
		store := &editableStore{}
		authz := &levelAuthz{level: "owner", deny: map[string]bool{"edit": true}}
		_, err := shareTestService(store).WithACLAuthorizer(authz).ResolveShareTarget(ctx, viewScope, "tenant-1", "widgets", "rec-1")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "FORBIDDEN" {
			t.Fatalf("err = %v; want FORBIDDEN", err)
		}
		if len(store.readScopes) != 0 {
			t.Fatalf("record read before the grant check: %v", store.readScopes)
		}
	})

	t.Run("sees it only through a share → RECORD_SHARED_READ_ONLY", func(t *testing.T) {
		store := &shareOnlyStore{shared: true}
		_, err := shareTestService(store).WithACLAuthorizer(&levelAuthz{level: "owner"}).ResolveShareTarget(ctx, viewScope, "tenant-1", "widgets", "rec-1")
		if !errors.Is(err, ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want ErrRecordSharedReadOnly", err)
		}
	})

	t.Run("out of scope and not shared → 404", func(t *testing.T) {
		store := &shareOnlyStore{shared: false}
		_, err := shareTestService(store).WithACLAuthorizer(&levelAuthz{level: "owner"}).ResolveShareTarget(ctx, viewScope, "tenant-1", "widgets", "rec-1")
		if !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
	})
}

// editableStore finds the record under any scope (the caller may edit it).
type editableStore struct {
	bulkTestStore
	readScopes []dynrepo.RowScope
}

func (s *editableStore) GetRecord(_ context.Context, _, _, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	s.readScopes = append(s.readScopes, scope)
	return &dynrepo.Record{ID: id, Data: map[string]any{}}, nil
}

// TestRecordShare_UpdateOfSharedRecordIsReadOnlyBeforeFieldChecks pins DAILY-QUEUE #226 point 3: the
// share probe must run when the first read under the caller's edit scope misses the record, BEFORE
// the field-ACL/validation stages that need the record's current values (nil for a share-only
// caller ⇒ strict mode ⇒ FIELD_FORBIDDEN for a locked field). The caller must get the 403 FR-005
// promises, not a field error that hides it. A record that is NOT shared keeps its old answer.
func TestRecordShare_UpdateOfSharedRecordIsReadOnlyBeforeFieldChecks(t *testing.T) {
	scope := RecordScope{UserID: "u-b", AccessLevel: "owner", RoleID: "r1"}
	for _, tc := range []struct {
		name   string
		shared bool
		want   error
	}{
		{"shared + locked field in payload → 403 read-only", true, ErrRecordSharedReadOnly},
		{"not shared + locked field → field error as before", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &shareOnlyStore{shared: tc.shared}
			svc := shareTestService(store).WithFieldACL(&fakeFieldACL{deniedField: "locked"})
			err := svc.Update(context.Background(), scope, "tenant-1", "widgets", "rec-1", map[string]any{"locked": "x"}, nil)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v; want %v", err, tc.want)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "FIELD_FORBIDDEN" {
				t.Fatalf("err = %v; want the unchanged FIELD_FORBIDDEN", err)
			}
		})
	}
}
