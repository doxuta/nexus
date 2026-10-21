package services

import (
	"context"
	"errors"
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
		{"shared record, view=own edit=team: the team edit scope reaches it → absent", RecordScope{UserID: "u-rep", AccessLevel: "owner", RoleID: "r1", IncludeShares: true},
			map[string]string{"view": "owner", "edit": "team"}, false, true, false, nil, 2},
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

// viewNotEditStore models a colleague's record for "view=all, edit/delete=own" (issue #18): every
// owner-narrowed scope — reads AND writes — misses it, "all" reaches it, "none" never does. gone = the
// record was deleted (e.g. between the write and the follow-up check): nothing reaches it any more.
type viewNotEditStore struct {
	bulkTestStore
	gone        bool
	inMyTeam    bool // the record's team set holds a team of the caller: a "team" scope reaches it
	reads       []dynrepo.RowScope
	writeScopes []dynrepo.RowScope
}

func (s *viewNotEditStore) reaches(scope dynrepo.RowScope) bool {
	return !s.gone && (scope.IsAdmin || scope.Level == dynrepo.AccessAll || (s.inMyTeam && scope.Level == dynrepo.AccessTeam))
}

func (s *viewNotEditStore) GetRecord(_ context.Context, _, _, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	s.reads = append(s.reads, scope)
	if !s.reaches(scope) {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, Data: map[string]any{}}, nil
}

func (s *viewNotEditStore) UpdateRecord(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.writeScopes = append(s.writeScopes, scope)
	if !s.reaches(scope) {
		return dynrepo.ErrRecordNotFound
	}
	return nil
}

func (s *viewNotEditStore) SoftDeleteRecord(_ context.Context, _, _, _ string, scope dynrepo.RowScope) error {
	s.writeScopes = append(s.writeScopes, scope)
	if !s.reaches(scope) {
		return dynrepo.ErrRecordNotFound
	}
	return nil
}

// faultAuthz fails every Authorize: an ACL fault on the follow-up check must surface, never be
// guessed into a 403 or a 404.
type faultAuthz struct{}

func (faultAuthz) Authorize(context.Context, string, string, string, string) (bool, string, error) {
	return false, "", errors.New("acl down")
}

// TestRecordWrite_ViewableButOutOfWriteScopeIs403 — issue #18: when the caller's edit/delete scope
// misses a record their VIEW scope reaches, PUT/DELETE answer ErrRecordEditForbidden (403 FORBIDDEN)
// instead of the "does not exist" 404. A record they cannot view — or one that is gone — stays 404, so
// the check never reveals a record outside the view scope.
func TestRecordWrite_ViewableButOutOfWriteScopeIs403(t *testing.T) {
	ctx := context.Background()
	own := RecordScope{UserID: "u-rep", AccessLevel: "owner", RoleID: "r1"}
	put := func(s *RecordService, scope RecordScope) error {
		return s.Update(ctx, scope, "tenant-1", "widgets", "rec-1", map[string]any{}, nil)
	}
	del := func(s *RecordService, scope RecordScope) error {
		return s.Delete(ctx, scope, "tenant-1", "widgets", "rec-1")
	}
	// A body the field checks would refuse (422): the 403 comes first, as for a share-only record (#226).
	putField := func(s *RecordService, scope RecordScope) error {
		return s.Update(ctx, scope, "tenant-1", "widgets", "rec-1", map[string]any{"name": "x"}, nil)
	}
	cases := []struct {
		name     string
		scope    RecordScope
		authz    entityAuthorizer // nil = none wired
		gone     bool
		inMyTeam bool
		call     func(*RecordService, RecordScope) error
		want     error
		probed   bool // a read ran under the VIEW level (with the share clause)
	}{
		{"PUT view=all edit=own → 403", own, &perActionAuthz{levels: map[string]string{"view": "all", "edit": "owner"}}, false, false, put, ErrRecordEditForbidden, true},
		{"PUT a field, view=all edit=own → 403 before the field checks", own, &perActionAuthz{levels: map[string]string{"view": "all", "edit": "owner"}}, false, false, putField, ErrRecordEditForbidden, true},
		{"DELETE view=all delete=own → 403", own, &perActionAuthz{levels: map[string]string{"view": "all", "delete": "owner"}}, false, false, del, ErrRecordEditForbidden, true},
		{"PUT edit=none view=all → 403", RecordScope{UserID: "u-rep", AccessLevel: "none", RoleID: "r1"},
			&perActionAuthz{levels: map[string]string{"view": "all", "edit": "none"}}, false, false, put, ErrRecordEditForbidden, true},
		{"PUT view=own (cannot view) → 404", own, &perActionAuthz{levels: map[string]string{"view": "owner", "edit": "owner"}}, false, false, put, ErrRecordNotFound, false},
		{"DELETE no view grant → 404", own, &perActionAuthz{levels: map[string]string{"delete": "owner"}}, false, false, del, ErrRecordNotFound, false},
		{"PUT view=none → 404", own, &perActionAuthz{levels: map[string]string{"view": "none", "edit": "owner"}}, false, false, put, ErrRecordNotFound, false},
		{"PUT view=all, record gone → 404", own, &perActionAuthz{levels: map[string]string{"view": "all", "edit": "owner"}}, true, false, put, ErrRecordNotFound, true},
		{"DELETE view=all, record gone → 404", own, &perActionAuthz{levels: map[string]string{"view": "all", "delete": "owner"}}, true, false, del, ErrRecordNotFound, true},
		{"PUT no authorizer wired → 404", own, nil, false, false, put, ErrRecordNotFound, false},
		// view=team: the check reads under the TEAM scope — another team's record stays hidden (404, like an
		// unknown id), a teammate's record is readable so its write is 403.
		{"PUT view=team edit=own, another team's record → 404", own, &perActionAuthz{levels: map[string]string{"view": "team", "edit": "owner"}}, false, false, put, ErrRecordNotFound, true},
		{"DELETE view=team delete=own, another team's record → 404", own, &perActionAuthz{levels: map[string]string{"view": "team", "delete": "owner"}}, false, false, del, ErrRecordNotFound, true},
		{"PUT view=team edit=own, a teammate's record → 403", own, &perActionAuthz{levels: map[string]string{"view": "team", "edit": "owner"}}, false, true, put, ErrRecordEditForbidden, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &viewNotEditStore{gone: tc.gone, inMyTeam: tc.inMyTeam}
			svc := shareTestService(store)
			if tc.authz != nil {
				svc = svc.WithACLAuthorizer(tc.authz)
			}
			if err := tc.call(svc, tc.scope); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
			for _, ws := range store.writeScopes { // the write itself is never widened
				if ws.Level != dynrepo.AccessLevel(tc.scope.AccessLevel) || ws.IncludeShares {
					t.Fatalf("write ran with %+v; want the caller's own %q level, no shares", ws, tc.scope.AccessLevel)
				}
			}
			// The view probe: any read at a level other than the caller's write level. It must run at
			// exactly their VIEW level, as them, with the share clause a view read carries — never wider.
			probed := false
			for _, rs := range store.reads {
				if rs.Level == dynrepo.AccessLevel(tc.scope.AccessLevel) {
					continue
				}
				probed = true
				pa, _ := tc.authz.(*perActionAuthz)
				if pa == nil || rs.Level != dynrepo.AccessLevel(pa.levels["view"]) || rs.IsAdmin || !rs.IncludeShares || rs.UserID != tc.scope.UserID {
					t.Fatalf("view probe ran with %+v; want the caller's own VIEW level, identity and share clause", rs)
				}
			}
			if probed != tc.probed {
				t.Fatalf("view probe ran = %v; want %v (reads %+v)", probed, tc.probed, store.reads)
			}
		})
	}

	t.Run("admin → 404, ACL never consulted", func(t *testing.T) {
		store := &viewNotEditStore{gone: true}
		authz := &perActionAuthz{levels: map[string]string{"view": "all"}}
		admin := RecordScope{UserID: "u-a", AccessLevel: "all", IsAdmin: true}
		if err := put(shareTestService(store).WithACLAuthorizer(authz), admin); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
		if len(authz.actions) != 0 {
			t.Fatalf("ACL consulted for an admin: %v", authz.actions)
		}
	})

	t.Run("ACL fault on the check → wrapped error, neither 403 nor 404", func(t *testing.T) {
		err := del(shareTestService(&viewNotEditStore{}).WithACLAuthorizer(faultAuthz{}), own)
		if err == nil || errors.Is(err, ErrRecordNotFound) || errors.Is(err, ErrRecordEditForbidden) {
			t.Fatalf("err = %v; want the ACL fault", err)
		}
	})

	t.Run("bulk update: the row says FORBIDDEN", func(t *testing.T) {
		svc := shareTestService(&viewNotEditStore{}).WithACLAuthorizer(&perActionAuthz{levels: map[string]string{"view": "all", "edit": "owner"}})
		res, err := svc.UpdateBulk(ctx, own, "tenant-1", "widgets", []string{"rec-1"}, map[string]any{"name": "x"})
		if err != nil {
			t.Fatalf("UpdateBulk: %v", err)
		}
		if len(res.Results) != 1 || res.Results[0].OK || res.Results[0].Code != "FORBIDDEN" {
			t.Fatalf("results = %+v; want one FORBIDDEN row", res.Results)
		}
	})
}
