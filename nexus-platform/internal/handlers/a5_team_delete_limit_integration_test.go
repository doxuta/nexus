//go:build integration

// Pins the ONE documented limitation of the team hierarchy (#20/#21), recorded in
// ../specs/001-auth-security/spec.md §5 under "Giới hạn đã biết (xoá nhóm giữa cây)".
//
// It began life as a throwaway repro from the pre-merge review. It is kept, with its assertions
// turned the right way round, because the property it guards is a SECURITY property: when an
// admin deletes a team in the middle of the tree, `fk_tst_team ... ON DELETE CASCADE`
// (migrations/000006_create_team_rls_a5.up.sql) removes that team's team_sets_teams rows, and the
// record made by a member of ONLY that team drops out of every ancestor's view. That is a loss of
// visibility, never a leak — and THAT is the part worth locking down. If someone later widens the
// behaviour so the record survives the delete (the queued root fix re-homes the composition rows
// onto the parent inside DeleteTeam), this test must be updated together with the spec line, on
// purpose. If someone instead breaks it the other way and the record becomes visible to a
// SIBLING or another tenant, this test fails, which is the whole point.
package handlers

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
)

// TestA5_TeamDelete_DropsRecordFromAncestors_FailClosed: tree region > branch > office; a branch
// member creates a record (team_set {branch}); the region member (team scope, member of region
// only) sees it through the closure. Admin then deletes the middle team. Asserted afterwards:
// the region member LOSES the record (documented limit), the creator KEEPS it (fail-closed, no
// data destroyed), and the surviving child team is re-homed onto the region (#21's actual promise).
func TestA5_TeamDelete_DropsRecordFromAncestors_FailClosed(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	const tenantID = "a5a50000-0000-0000-0000-0000000000d4"
	const slug = "a5-repro"
	a4SeedTenant(t, gdb, tenantID, slug, "Repro A5")

	region, branch, office := uuid.NewString(), uuid.NewString(), uuid.NewString()
	a5SeedTeam(t, gdb, region, tenantID, "Vùng Bắc")
	a5SeedTeam(t, gdb, branch, tenantID, "Chi nhánh Hà Nội")
	a5SeedTeam(t, gdb, office, tenantID, "PGD Cầu Giấy")
	for child, parent := range map[string]string{branch: region, office: branch} {
		if err := gdb.Exec("UPDATE teams SET parent_id = ? WHERE id = ?", parent, child).Error; err != nil {
			t.Fatalf("set parent: %v", err)
		}
	}

	roleTeam := a4SeedRole(t, gdb, tenantID, "branch-rep", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "team"),
	})
	uRegion := a4SeedUser(t, gdb, tenantID, "region@"+slug, false, &roleTeam)
	uBranch := a4SeedUser(t, gdb, tenantID, "branch@"+slug, false, &roleTeam)
	a4SeedUser(t, gdb, tenantID, "admin@"+slug, true, nil)
	a5SeedMembership(t, gdb, uRegion, region)
	a5SeedMembership(t, gdb, uBranch, branch)

	a4DropEntity(sx, []string{tenantID}, rlsEntity)
	t.Cleanup(func() {
		a4DropEntity(sx, []string{tenantID}, rlsEntity)
		sx.Exec("DELETE FROM team_memberships WHERE team_id IN (?, ?, ?)", region, branch, office)
		sx.Exec("DELETE st FROM team_sets_teams st JOIN team_sets s ON s.id = st.team_set_id WHERE s.tenant_id = ?", tenantID)
		sx.Exec("DELETE FROM team_sets WHERE tenant_id = ?", tenantID)
		sx.Exec("DELETE FROM teams WHERE tenant_id = ?", tenantID)
	})

	adminTok := rt.login(t, slug, "admin@"+slug).Data.AccessToken
	rt.createEntity(t, adminTok, rlsEntity)
	regionTok := rt.login(t, slug, "region@"+slug).Data.AccessToken
	branchTok := rt.login(t, slug, "branch@"+slug).Data.AccessToken

	recBranch := postRecordAs(t, rt, branchTok, "khách của Chi nhánh Hà Nội")

	// BEFORE: region member sees the branch record through the closure.
	ids, _ := listIDsAs(t, rt, regionTok)
	if !ids[recBranch] {
		t.Fatalf("PRECONDITION FAILED: region member does not see branch record before delete: %v", ids)
	}
	var setID string
	if err := sx.Get(&setID, "SELECT team_set_id FROM `obj_"+rlsEntity+"` WHERE id = ?", recBranch); err != nil {
		t.Fatalf("read team_set_id: %v", err)
	}
	var compBefore int
	_ = sx.Get(&compBefore, "SELECT COUNT(*) FROM team_sets_teams WHERE team_set_id = ?", setID)
	t.Logf("BEFORE delete: record team_set_id=%s composition rows=%d; region sees record=%v", setID, compBefore, ids[recBranch])

	// Admin "gộp chi nhánh": DELETE the middle team.
	if w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+branch, adminTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete branch: %d %s", w.Code, w.Body.String())
	}

	// AFTER: composition rows of the record's set + what each principal sees.
	var compAfter int
	_ = sx.Get(&compAfter, "SELECT COUNT(*) FROM team_sets_teams WHERE team_set_id = ?", setID)
	var officeParent *string
	_ = sx.Get(&officeParent, "SELECT parent_id FROM teams WHERE id = ?", office)
	idsAfter, totalAfter := listIDsAs(t, rt, regionTok)
	regionGet := getStatusAs(t, rt, regionTok, recBranch)
	branchGet := getStatusAs(t, rt, branchTok, recBranch)
	t.Logf("AFTER delete: composition rows=%d (was %d); office.parent_id=%v (region=%s); region list total=%d sees record=%v; region GET=%d; creator GET=%d",
		compAfter, compBefore, deref(officeParent), region, totalAfter, idsAfter[recBranch], regionGet, branchGet)

	// Documented limit: the ancestor loses the record. Asserted so the limit cannot change by
	// accident — only together with the spec line it points at.
	if idsAfter[recBranch] || regionGet != http.StatusNotFound {
		t.Errorf("ancestor visibility after deleting the middle team = (inList=%v, GET=%d); want (false, 404) per spec §5 known limit. "+
			"If the root fix landed (re-home team_sets_teams onto the parent inside DeleteTeam), update this test AND the spec line together.",
			idsAfter[recBranch], regionGet)
	}
	// The security half, and the reason this is a limitation rather than a bug: fail-CLOSED.
	// Nothing was destroyed and nothing leaked — the creator still reaches their own record.
	if branchGet != http.StatusOK {
		t.Errorf("creator lost their own record after an unrelated team was deleted (GET=%d); want 200 — "+
			"the cascade must cost visibility, never data", branchGet)
	}
	// #21's actual promise: surviving CHILD teams are re-homed onto the deleted team's parent.
	if deref(officeParent) != region {
		t.Errorf("child team parent after delete = %q; want the grandparent %q (re-home, not orphan)", deref(officeParent), region)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
