//go:build integration

package appmeta_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/appmeta"
)

// specs/063-draft-publish T077: the *Exec statements run on a caller transaction (the publish tx)
// and write exactly what the repository does.
func TestAppExec_OnTx_WritesAsRepo(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := appmeta.NewSQLRepository(db)
	ctx := context.Background()

	id1, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales", IsDefault: true})
	if err != nil {
		t.Fatalf("create sales: %v", err)
	}
	id2, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "support", Label: "Support"})
	if err != nil {
		t.Fatalf("create support: %v", err)
	}

	home := "dash-1"
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := appmeta.ClearDefaultExec(ctx, tx, tenantA, id2); err != nil {
		t.Fatalf("ClearDefaultExec: %v", err)
	}
	if err := appmeta.UpdateExec(ctx, tx, tenantA, id2, appmeta.UpdateInput{Label: "Hỗ trợ", Description: "d", Icon: "i", Color: "#fff",
		IsDefault: true, SortOrder: 3, UpdatedBy: "u1", HomeDashboardID: &home}); err != nil {
		t.Fatalf("UpdateExec: %v", err)
	}
	if err := appmeta.ReplaceNavExec(ctx, tx, tenantA, id2, []appmeta.NavItemInput{
		{ItemType: appmeta.ItemTypeEntity, RefID: "e1", Label: "Khách"},
		{ItemType: appmeta.ItemTypeDashboard, RefID: "dash-1"},
	}, time.Now()); err != nil {
		t.Fatalf("ReplaceNavExec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	a1, err := repo.GetByID(ctx, tenantA, id1)
	if err != nil || a1.IsDefault {
		t.Fatalf("old default = %+v (%v); want cleared", a1, err)
	}
	a2, err := repo.GetByID(ctx, tenantA, id2)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a2.Label != "Hỗ trợ" || !a2.IsDefault || a2.SortOrder != 3 || a2.HomeDashboardID != "dash-1" || a2.NavCount != 2 {
		t.Fatalf("app2 = %+v; want label/default/sort/home/nav applied", a2)
	}
	nav, err := repo.ListNav(ctx, tenantA, id2)
	if err != nil || len(nav) != 2 || nav[0].RefID != "e1" || nav[0].Label == nil || *nav[0].Label != "Khách" || nav[1].Label != nil {
		t.Fatalf("nav = %+v (%v); want order + NULL label for blank", nav, err)
	}

	// A rolled-back tx leaves nothing (the publish tx guarantee).
	tx, err = db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	if err := appmeta.UpdateExec(ctx, tx, tenantA, id2, appmeta.UpdateInput{Label: "ROLLED"}); err != nil {
		t.Fatalf("UpdateExec: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if a2, err = repo.GetByID(ctx, tenantA, id2); err != nil || a2.Label != "Hỗ trợ" {
		t.Fatalf("label after rollback = %+v (%v)", a2, err)
	}

	// Tenant pin: another tenant cannot touch the row.
	if err := appmeta.UpdateExec(ctx, db, tenantB, id2, appmeta.UpdateInput{Label: "x"}); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("cross-tenant UpdateExec err = %v; want ErrAppNotFound", err)
	}
}

func TestAppExec_RestoreExec(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := appmeta.NewSQLRepository(db)
	ctx := context.Background()

	id, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := appmeta.RestoreExec(ctx, db, tenantA, id, "sales", appmeta.UpdateInput{Label: "x"}); err != nil || ok {
		t.Fatalf("restore of a live app = (%v, %v); want (false, nil)", ok, err)
	}
	if err := repo.SoftDelete(ctx, tenantA, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if ok, _ := appmeta.RestoreExec(ctx, db, tenantB, id, "sales", appmeta.UpdateInput{Label: "x"}); ok {
		t.Fatal("cross-tenant restore must not match")
	}
	ok, err := appmeta.RestoreExec(ctx, db, tenantA, id, "sales", appmeta.UpdateInput{Label: "Back", SortOrder: 2})
	if err != nil || !ok {
		t.Fatalf("restore = (%v, %v); want (true, nil)", ok, err)
	}
	a, err := repo.GetByName(ctx, tenantA, "sales")
	if err != nil || a.ID != id || a.Label != "Back" || a.SortOrder != 2 {
		t.Fatalf("restored app = %+v (%v); want original id, slug and content", a, err)
	}
}
