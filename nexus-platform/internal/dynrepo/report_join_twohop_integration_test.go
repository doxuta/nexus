//go:build integration

// Integration test for the SECOND hop of a report join (queue #293): contacts → accounts → regions,
// hop 2 chained off hop 1's alias, with the per-hop tenant predicate on the far table.
//
// Run with:  go test -tags=integration ./internal/dynrepo/ -run TwoHop
package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

func TestReportJoin_TwoHopChainAndHop2Tenant(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	for _, e := range []string{"it_rh_contacts", "it_rh_accounts", "it_rh_regions"} {
		_ = repo.DropTable(ctx, e)
	}
	defer func() {
		for _, e := range []string{"it_rh_contacts", "it_rh_accounts", "it_rh_regions"} {
			_ = repo.DropTable(context.Background(), e)
		}
	}()
	if err := repo.CreateTable(ctx, "it_rh_regions", []ddl.FieldDef{{Name: "zone", FieldType: "picklist", Indexed: true}}); err != nil {
		t.Fatalf("regions: %v", err)
	}
	if err := repo.CreateTable(ctx, "it_rh_accounts", []ddl.FieldDef{{Name: "region", FieldType: "relation", RelatedEntity: "it_rh_regions"}}); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if err := repo.CreateTable(ctx, "it_rh_contacts", []ddl.FieldDef{{Name: "account", FieldType: "relation", RelatedEntity: "it_rh_accounts"}}); err != nil {
		t.Fatalf("contacts: %v", err)
	}

	north, _ := repo.InsertRecord(ctx, "it_rh_regions", rjTenantA, rjUser1, map[string]any{"zone": "north"}, nil)
	foreign, _ := repo.InsertRecord(ctx, "it_rh_regions", rjTenantB, rjUser1, map[string]any{"zone": "leak"}, nil)
	accN, _ := repo.InsertRecord(ctx, "it_rh_accounts", rjTenantA, rjUser1, map[string]any{"region": north}, nil)
	accF, _ := repo.InsertRecord(ctx, "it_rh_accounts", rjTenantA, rjUser1, map[string]any{"region": foreign}, nil)
	for _, acc := range []string{accN, accN, accF} {
		if _, err := repo.InsertRecord(ctx, "it_rh_contacts", rjTenantA, rjUser1, map[string]any{"account": acc}, nil); err != nil {
			t.Fatalf("contact: %v", err)
		}
	}

	accTable, _ := TableFor("it_rh_accounts")
	regTable, _ := TableFor("it_rh_regions")
	got, err := repo.AggregateGrouped(ctx, "it_rh_contacts", rjTenantA, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "zone", GroupByAlias: "j1",
		Joins: []JoinSpec{
			{Alias: "j0", RelatedTable: accTable, Scope: AdminScope(), LeftCol: "account_id", RightCol: "id"},
			{Alias: "j1", FromAlias: "j0", RelatedTable: regTable, Scope: AdminScope(), LeftCol: "region_id", RightCol: "id"},
		},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped: %v", err)
	}
	g := groupsByKey(got)
	// Two contacts reach "north" through account → region; the third reaches a tenant-B region, which
	// the hop-2 tenant predicate must refuse (its zone must never surface).
	if g["north"] != 2 || len(g) != 1 {
		t.Fatalf("groups = %v, want exactly {north:2}", g)
	}
}
