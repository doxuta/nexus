//go:build integration

// Integration test for Repository.ListRollupsAggregating (queue #234) on the REAL schema: the
// JSON_EXTRACT lookup of rollups whose options aggregate <childEntity>.<aggField>. Pins tenant
// isolation (tenant B's identically-named rollup never shows for A), the field/child filters, and
// that a soft-deleted rollup no longer counts as a dependant.
package metadata

import (
	"context"
	"testing"
	"time"
)

func TestRepo_ListRollupsAggregating_IT(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	const entName = "it_lra_orders"
	cleanup := func() {
		gdb.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		          WHERE e.name = ? AND e.tenant_id IN (?, ?)`, entName, repinTenantA, repinTenantB)
		gdb.Exec(`DELETE FROM sys_entities WHERE name = ? AND tenant_id IN (?, ?)`, entName, repinTenantA, repinTenantB)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now()
	opts := func(child, agg string) *string {
		s := `{"childEntity":"` + child + `","metric":"sum","aggField":"` + agg + `","viaRelation":"order"}`
		return &s
	}
	seed := func(tag, tenant string) {
		entID := "lra-ent-" + tag
		if err := gdb.Create(&EntityDefinition{ID: entID, TenantID: tenant, Name: entName, Label: "Orders",
			DataTable: "obj_" + entName, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatalf("seed entity %s: %v", tag, err)
		}
		for i, f := range []FieldDefinition{
			{ID: "lra-f1-" + tag, Name: "total", FieldType: "rollup", Options: opts("it_lra_lines", "line_total")},
			{ID: "lra-f2-" + tag, Name: "other", FieldType: "rollup", Options: opts("it_lra_lines", "qty")},
			{ID: "lra-f3-" + tag, Name: "elsewhere", FieldType: "rollup", Options: opts("it_lra_other", "line_total")},
			{ID: "lra-f4-" + tag, Name: "plain", FieldType: "number"},
		} {
			f.EntityID, f.TenantID, f.FieldOrder, f.CreatedAt, f.UpdatedAt = entID, tenant, i, now, now
			if err := gdb.Create(&f).Error; err != nil {
				t.Fatalf("seed field %s: %v", f.Name, err)
			}
		}
	}
	seed("a", repinTenantA)
	seed("b", repinTenantB)
	ctx := context.Background()

	got, err := repo.ListRollupsAggregating(ctx, repinTenantA, "it_lra_lines", "line_total")
	if err != nil {
		t.Fatalf("ListRollupsAggregating: %v", err)
	}
	if len(got) != 1 || got[0] != entName+".total" {
		t.Fatalf("got %v; want only [%s.total] (other aggField / other child / other tenant excluded)", got, entName)
	}

	if none, err := repo.ListRollupsAggregating(ctx, repinTenantA, "it_lra_lines", "nope"); err != nil || len(none) != 0 {
		t.Fatalf("unknown aggField: got %v err %v; want empty", none, err)
	}

	if err := gdb.Exec(`UPDATE sys_fields SET deleted_at = NOW() WHERE id = ?`, "lra-f1-a").Error; err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if after, err := repo.ListRollupsAggregating(ctx, repinTenantA, "it_lra_lines", "line_total"); err != nil || len(after) != 0 {
		t.Fatalf("after soft-delete: got %v err %v; want empty", after, err)
	}
}
