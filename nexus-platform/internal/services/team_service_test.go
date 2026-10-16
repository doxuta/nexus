package services

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeTeamRepo is an in-memory stand-in for the team repository. Teams are keyed by
// (tenantID, id) and memberships by (teamID, userID), and a separate users set lets the fake
// reproduce the cross-tenant member guard without a database. It mirrors the real repo's typed
// errors (ErrTeamNotFound/ErrTeamAlreadyExists/ErrMemberUserNotFound/ErrMembershipNotFound).
type fakeTeamRepo struct {
	teams       map[string]*auth.Team // key: tenantID\x00id
	memberships map[string]bool       // key: teamID\x00userID
	users       map[string]string     // userID -> tenantID (live users only)
}

func newFakeTeamRepo() *fakeTeamRepo {
	return &fakeTeamRepo{
		teams:       map[string]*auth.Team{},
		memberships: map[string]bool{},
		users:       map[string]string{},
	}
}

func (f *fakeTeamRepo) key(a, b string) string { return a + "\x00" + b }

func (f *fakeTeamRepo) ListTeams(_ context.Context, tenantID string) ([]auth.Team, error) {
	var out []auth.Team
	for _, t := range f.teams {
		if t.TenantID == tenantID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeTeamRepo) GetTeamByID(_ context.Context, tenantID, id string) (*auth.Team, error) {
	if t, ok := f.teams[f.key(tenantID, id)]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, auth.ErrTeamNotFound
}

func (f *fakeTeamRepo) CreateTeam(_ context.Context, team *auth.Team) error {
	for _, ex := range f.teams {
		if ex.TenantID == team.TenantID && ex.Name == team.Name {
			return auth.ErrTeamAlreadyExists
		}
	}
	if team.ParentID != nil {
		if _, ok := f.teams[f.key(team.TenantID, *team.ParentID)]; !ok {
			return auth.ErrTeamNotFound
		}
	}
	cp := *team
	f.teams[f.key(team.TenantID, team.ID)] = &cp
	return nil
}

func (f *fakeTeamRepo) UpdateTeam(_ context.Context, tenantID, id string, fields map[string]any) (*auth.Team, error) {
	t, ok := f.teams[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrTeamNotFound
	}
	if v, ok := fields["name"].(string); ok {
		for _, ex := range f.teams {
			if ex.TenantID == tenantID && ex.Name == v && ex.ID != id {
				return nil, auth.ErrTeamAlreadyExists
			}
		}
		t.Name = v
	}
	if pid, ok := fields["parent_id"]; ok {
		switch v := pid.(type) {
		case nil:
			t.ParentID = nil
		case string:
			if _, ok := f.teams[f.key(tenantID, v)]; !ok {
				return nil, auth.ErrTeamNotFound
			}
			// Mirror the real repo's ancestor walk (queue #21): neither the proposed parent nor
			// any team above it may be the team itself, else the chain would loop. Visited set
			// so a pre-existing loop in the fake data terminates too.
			seen := map[string]bool{}
			for cur := v; cur != "" && !seen[cur]; {
				if cur == id {
					return nil, auth.ErrTeamParentCycle
				}
				seen[cur] = true
				p, ok := f.teams[f.key(tenantID, cur)]
				if !ok || p.ParentID == nil {
					break
				}
				cur = *p.ParentID
			}
			cp := v
			t.ParentID = &cp
		}
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTeamRepo) DeleteTeam(_ context.Context, tenantID, id string) error {
	k := f.key(tenantID, id)
	if _, ok := f.teams[k]; !ok {
		return auth.ErrTeamNotFound
	}
	delete(f.teams, k)
	return nil
}

func (f *fakeTeamRepo) ListMembers(_ context.Context, tenantID, teamID string) ([]auth.User, error) {
	if _, ok := f.teams[f.key(tenantID, teamID)]; !ok {
		return nil, auth.ErrTeamNotFound
	}
	var out []auth.User
	for k := range f.memberships {
		// k = teamID\x00userID
		if len(k) < len(teamID)+1 || k[:len(teamID)] != teamID {
			continue
		}
		userID := k[len(teamID)+1:]
		if f.users[userID] == tenantID {
			out = append(out, auth.User{ID: userID, TenantID: tenantID, Email: userID + "@x"})
		}
	}
	return out, nil
}

func (f *fakeTeamRepo) AddMember(_ context.Context, tenantID, teamID, userID, _ string) error {
	if _, ok := f.teams[f.key(tenantID, teamID)]; !ok {
		return auth.ErrTeamNotFound
	}
	if f.users[userID] != tenantID {
		return auth.ErrMemberUserNotFound
	}
	f.memberships[f.key(teamID, userID)] = true // idempotent
	return nil
}

func (f *fakeTeamRepo) RemoveMember(_ context.Context, tenantID, teamID, userID string) error {
	if _, ok := f.teams[f.key(tenantID, teamID)]; !ok {
		return auth.ErrTeamNotFound
	}
	k := f.key(teamID, userID)
	if !f.memberships[k] {
		return auth.ErrMembershipNotFound
	}
	delete(f.memberships, k)
	return nil
}

// strptr is a local helper for *string fields.
func strptr(s string) *string { return &s }

const (
	tsTenantA = "ten-a"
	tsTenantB = "ten-b"
)

func newTeamSvc(repo *fakeTeamRepo) *TeamService {
	s := NewTeamService(repo)
	// deterministic ids for assertions
	n := 0
	s.newID = func() string { n++; return fmt.Sprintf("id-%d", n) }
	return s
}

func TestTeamService_CreateAndGet(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()

	v, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Sales"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.Name != "Sales" || v.TenantID != tsTenantA || v.ID == "" {
		t.Fatalf("unexpected view: %+v", v)
	}

	got, err := s.GetTeam(ctx, tsTenantA, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != v.ID {
		t.Fatalf("get returned %+v", got)
	}
}

func TestTeamService_CreateBlankName422(t *testing.T) {
	s := newTeamSvc(newFakeTeamRepo())
	_, err := s.CreateTeam(context.Background(), tsTenantA, CreateTeamInput{Name: "  "})
	assertCode(t, err, "TEAM_NAME_REQUIRED")
}

func TestTeamService_CreateDuplicateName409(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	if _, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Eng"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Eng"})
	assertCode(t, err, "TEAM_ALREADY_EXISTS")
}

func TestTeamService_DuplicateNameAllowedAcrossTenants(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	if _, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Eng"}); err != nil {
		t.Fatalf("A create: %v", err)
	}
	if _, err := s.CreateTeam(ctx, tsTenantB, CreateTeamInput{Name: "Eng"}); err != nil {
		t.Fatalf("B create same name must be allowed: %v", err)
	}
}

func TestTeamService_GetUnknown404(t *testing.T) {
	s := newTeamSvc(newFakeTeamRepo())
	_, err := s.GetTeam(context.Background(), tsTenantA, "nope")
	assertCode(t, err, "TEAM_NOT_FOUND")
}

func TestTeamService_GetCrossTenant404(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "A"})
	_, err := s.GetTeam(ctx, tsTenantB, v.ID) // B reading A's team id
	assertCode(t, err, "TEAM_NOT_FOUND")
}

func TestTeamService_UpdateRename(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Old"})
	got, err := s.UpdateTeam(ctx, tsTenantA, v.ID, UpdateTeamInput{Name: strptr("New")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Name != "New" {
		t.Fatalf("rename failed: %+v", got)
	}
}

func TestTeamService_UpdateRenameCollision409(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	_, _ = s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Taken"})
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Mine"})
	_, err := s.UpdateTeam(ctx, tsTenantA, v.ID, UpdateTeamInput{Name: strptr("Taken")})
	assertCode(t, err, "TEAM_ALREADY_EXISTS")
}

func TestTeamService_UpdateCrossTenant404(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "A"})
	_, err := s.UpdateTeam(ctx, tsTenantB, v.ID, UpdateTeamInput{Name: strptr("hijack")})
	assertCode(t, err, "TEAM_NOT_FOUND")
}

func TestTeamService_DeleteAndUnknown(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Doomed"})
	if err := s.DeleteTeam(ctx, tsTenantA, v.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteTeam(ctx, tsTenantA, v.ID); !isCode(err, "TEAM_NOT_FOUND") {
		t.Fatalf("re-delete should be 404, got %v", err)
	}
}

func TestTeamService_AddMemberHappyAndIdempotent(t *testing.T) {
	repo := newFakeTeamRepo()
	repo.users["u1"] = tsTenantA
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "T"})

	if err := s.AddMember(ctx, tsTenantA, v.ID, "u1"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// idempotent re-add
	if err := s.AddMember(ctx, tsTenantA, v.ID, "u1"); err != nil {
		t.Fatalf("re-add member should be idempotent: %v", err)
	}
	members, err := s.ListMembers(ctx, tsTenantA, v.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 1 || members[0].ID != "u1" {
		t.Fatalf("members = %+v", members)
	}
}

func TestTeamService_AddMemberCrossTenantUser404(t *testing.T) {
	repo := newFakeTeamRepo()
	repo.users["u-b"] = tsTenantB // user belongs to B
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "T"})
	err := s.AddMember(ctx, tsTenantA, v.ID, "u-b") // A tries to add B's user
	assertCode(t, err, "MEMBER_USER_NOT_FOUND")
}

func TestTeamService_AddMemberUnknownTeam404(t *testing.T) {
	repo := newFakeTeamRepo()
	repo.users["u1"] = tsTenantA
	s := newTeamSvc(repo)
	err := s.AddMember(context.Background(), tsTenantA, "nope", "u1")
	assertCode(t, err, "TEAM_NOT_FOUND")
}

func TestTeamService_RemoveMember(t *testing.T) {
	repo := newFakeTeamRepo()
	repo.users["u1"] = tsTenantA
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "T"})
	_ = s.AddMember(ctx, tsTenantA, v.ID, "u1")

	if err := s.RemoveMember(ctx, tsTenantA, v.ID, "u1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// removing again → not a member
	if err := s.RemoveMember(ctx, tsTenantA, v.ID, "u1"); !isCode(err, "MEMBERSHIP_NOT_FOUND") {
		t.Fatalf("re-remove should be 404 membership, got %v", err)
	}
}

func TestTeamService_AddMemberBlankUser422(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	v, _ := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "T"})
	err := s.AddMember(ctx, tsTenantA, v.ID, "  ")
	assertCode(t, err, "MEMBER_USER_REQUIRED")
}

// TestTeamService_UpdateParentCycle422 (queue #21): assigning a parent that is the team itself,
// its child, or any deeper descendant is refused with a readable TEAM_PARENT_CYCLE (422) and the
// stored parent is left untouched; a legal reparent and an explicit detach still work.
func TestTeamService_UpdateParentCycle422(t *testing.T) {
	repo := newFakeTeamRepo()
	s := newTeamSvc(repo)
	ctx := context.Background()
	region, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Vùng Bắc"})
	if err != nil {
		t.Fatalf("create region: %v", err)
	}
	branch, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "Chi nhánh Hà Nội", ParentID: strptr(region.ID)})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	office, err := s.CreateTeam(ctx, tsTenantA, CreateTeamInput{Name: "PGD Cầu Giấy", ParentID: strptr(branch.ID)})
	if err != nil {
		t.Fatalf("create office: %v", err)
	}

	for _, bad := range []struct{ who, parent string }{
		{"grandchild as parent", office.ID},
		{"child as parent", branch.ID},
		{"self as parent", region.ID},
	} {
		_, err := s.UpdateTeam(ctx, tsTenantA, region.ID, UpdateTeamInput{ParentID: strptr(bad.parent)})
		if !isCode(err, "TEAM_PARENT_CYCLE") {
			t.Fatalf("%s: want TEAM_PARENT_CYCLE, got %v", bad.who, err)
		}
		got, _ := s.GetTeam(ctx, tsTenantA, region.ID)
		if got.ParentID != nil {
			t.Fatalf("%s: region parent was written despite the refusal: %v", bad.who, *got.ParentID)
		}
	}

	// Legal reparent: office directly under region.
	got, err := s.UpdateTeam(ctx, tsTenantA, office.ID, UpdateTeamInput{ParentID: strptr(region.ID)})
	if err != nil || got.ParentID == nil || *got.ParentID != region.ID {
		t.Fatalf("legal reparent: err=%v got=%+v", err, got)
	}
	// Explicit detach.
	got, err = s.UpdateTeam(ctx, tsTenantA, office.ID, UpdateTeamInput{ParentID: strptr("")})
	if err != nil || got.ParentID != nil {
		t.Fatalf("detach: err=%v got=%+v", err, got)
	}
}

// --- test helpers -----------------------------------------------------------------------------

func isCode(err error, code string) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Code == code
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if !isCode(err, code) {
		t.Fatalf("want ValidationError code %q, got %v", code, err)
	}
}
