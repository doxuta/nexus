package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// editScopeStore models "view=all, edit=own": the record belongs to someone else, so it is reachable
// under any scope EXCEPT an owner-narrowed one (the caller's EDIT scope). It records every read scope.
type editScopeStore struct {
	bulkTestStore
	sharedOnly bool // the record is reachable only through a share (flagged on the view read)
	mine       bool // the caller owns the record: every scope reaches it
	reads      []dynrepo.RowScope
}

func (s *editScopeStore) GetRecord(_ context.Context, _, _, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	s.reads = append(s.reads, scope)
	if !s.mine && scope.Level == dynrepo.AccessOwner && !scope.IsAdmin && !(s.sharedOnly && scope.IncludeShares) {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, Data: map[string]any{}, SharedWithMe: s.sharedOnly && scope.IncludeShares}, nil
}

// perActionAuthz answers Authorize with a level (or a denial) per RBAC action.
type perActionAuthz struct {
	levels  map[string]string // action → level; absent action = denied
	actions []string
}

func (a *perActionAuthz) Authorize(_ context.Context, _, _, _, action string) (bool, string, error) {
	a.actions = append(a.actions, action)
	lvl, ok := a.levels[action]
	return ok, lvl, nil
}

// TestRecordGet_CanEditFollowsEditScope — DAILY-QUEUE #334: GET /:entity/:id says whether the caller's
// EDIT scope reaches this record, so the page hides Sửa instead of letting a save die as 404.
func TestRecordGet_CanEditFollowsEditScope(t *testing.T) {
	ctx := context.Background()
	viewScope := RecordScope{UserID: "u-rep", AccessLevel: "all", RoleID: "r1", IncludeShares: true}
	f := false
	cases := []struct {
		name      string
		scope     RecordScope
		levels    map[string]string
		noAuthz   bool
		sharedRow bool
		mine      bool
		want      *bool // nil = canEdit absent
		wantReads int   // GetRecord calls (view read + edit probe)
	}{
		{"view=all edit=own, colleague's record → canEdit false", viewScope, map[string]string{"view": "all", "edit": "owner"}, false, false, false, &f, 2},
		{"edit=all → absent, no probe", viewScope, map[string]string{"view": "all", "edit": "all"}, false, false, false, nil, 1},
		{"edit=none → canEdit false", viewScope, map[string]string{"view": "all", "edit": "none"}, false, false, false, &f, 1},
		{"no edit grant → canEdit false", viewScope, map[string]string{"view": "all"}, false, false, false, &f, 1},
		{"shared-only record → canEdit false, no probe", RecordScope{UserID: "u-rep", AccessLevel: "owner", RoleID: "r1", IncludeShares: true},
			map[string]string{"view": "owner", "edit": "owner"}, false, true, false, &f, 1},
		{"same level, own record → absent, no probe", RecordScope{UserID: "u-rep", AccessLevel: "owner", RoleID: "r1"},
			map[string]string{"view": "owner", "edit": "owner"}, false, false, true, nil, 1},
		{"admin → absent, ACL never consulted", RecordScope{UserID: "u-a", AccessLevel: "all", IsAdmin: true}, nil, false, false, false, nil, 1},
		{"no authorizer wired → absent", viewScope, nil, true, false, false, nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &editScopeStore{sharedOnly: tc.sharedRow, mine: tc.mine}
			svc := shareTestService(store)
			authz := &perActionAuthz{levels: tc.levels}
			if !tc.noAuthz {
				svc = svc.WithACLAuthorizer(authz)
			}
			rec, err := svc.Get(ctx, tc.scope, "tenant-1", "widgets", "rec-1", nil, nil)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			switch {
			case tc.want == nil && rec.CanEdit != nil:
				t.Fatalf("canEdit = %v; want absent", *rec.CanEdit)
			case tc.want != nil && (rec.CanEdit == nil || *rec.CanEdit != *tc.want):
				t.Fatalf("canEdit = %v; want %v", rec.CanEdit, *tc.want)
			}
			if len(store.reads) != tc.wantReads {
				t.Fatalf("GetRecord calls = %d; want %d (%+v)", len(store.reads), tc.wantReads, store.reads)
			}
			for _, rs := range store.reads[1:] { // the probe: EDIT level, never the share clause
				if rs.IncludeShares || rs.Level != dynrepo.AccessLevel(tc.levels["edit"]) {
					t.Fatalf("edit probe ran with %+v; want level %q, no shares", rs, tc.levels["edit"])
				}
			}
		})
	}
}
