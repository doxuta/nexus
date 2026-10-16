//go:build integration

// SEARCH-UNIFIED (Wave 3.5) integration on real MySQL: the fan-out finds records across MANY
// entities (not just a client-side top-5), gates each entity through the ACL seam (a denied
// entity contributes nothing, no existence leak), and a short/empty term yields no groups.
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeSearchACL allows a fixed allow-set at access level "all"; everything else is denied.
type fakeSearchACL struct{ allow map[string]bool }

func (f fakeSearchACL) Authorize(_ context.Context, _, _, entity, _ string) (bool, string, error) {
	if f.allow[entity] {
		return true, "all", nil
	}
	return false, "", nil
}

func TestSearchService_FanOutAndACL(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	recordSvc := NewRecordService(metaRepo, dynrepo.New(sdb))
	entitySvc := NewEntityService(dynrepo.New(sdb), metaRepo)
	const tenant = "cccccccc-3c3c-3c3c-3c3c-cccccccccccc"
	const user = "00000000-0000-0000-0000-000000000003"
	ctx := context.Background()
	adminScope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	// Six entities so the match sits well past any top-5 client cut. Each gets a text field
	// and one record whose name contains the shared term "acme".
	names := []string{"su_a", "su_b", "su_c", "su_d", "su_e", "su_f"}
	for _, n := range names {
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
			Name: n, Label: n, Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}},
		}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
		if _, err := recordSvc.Create(ctx, adminScope, tenant, user, n, map[string]any{"name": "Acme " + n}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	// Admin scope (acl bypassed): the term is found in ALL six entities incl. the 6th.
	svc := NewSearchService(metaRepo, fakeSearchACL{allow: map[string]bool{}}, recordSvc)
	groups, err := svc.Search(ctx, adminScope, tenant, "acme")
	if err != nil {
		t.Fatalf("admin search: %v", err)
	}
	if len(groups) != 6 {
		t.Fatalf("admin found %d entities, want 6 (fan-out must not cap at 5)", len(groups))
	}
	found := map[string]bool{}
	for _, g := range groups {
		if len(g.Hits) != 1 {
			t.Fatalf("entity %s: %d hits, want 1", g.Entity, len(g.Hits))
		}
		found[g.Entity] = true
	}
	if !found["su_f"] {
		t.Fatal("the 6th entity's record was not found — this is the exact truncation bug")
	}

	// Role scope (non-admin): only su_a, su_c allowed → exactly those two groups, no leak.
	roleScope := RecordScope{UserID: user, RoleID: "r1", AccessLevel: "all"}
	svc2 := NewSearchService(metaRepo, fakeSearchACL{allow: map[string]bool{"su_a": true, "su_c": true}}, recordSvc)
	g2, err := svc2.Search(ctx, roleScope, tenant, "acme")
	if err != nil {
		t.Fatalf("role search: %v", err)
	}
	if len(g2) != 2 {
		t.Fatalf("role-scoped found %d entities, want 2 (denied entities must be invisible)", len(g2))
	}
	for _, g := range g2 {
		if g.Entity != "su_a" && g.Entity != "su_c" {
			t.Fatalf("leaked a denied entity: %s", g.Entity)
		}
	}

	// Short term → no groups.
	if g3, _ := svc.Search(ctx, adminScope, tenant, "a"); len(g3) != 0 {
		t.Fatalf("1-char term returned %d groups, want 0", len(g3))
	}
}
