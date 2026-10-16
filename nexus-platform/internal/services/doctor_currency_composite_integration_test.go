//go:build integration

// Queue #440 — D2 and the #157 row count over a currency SUB-field of a composite, real MySQL 8:
// the nested JSON path ($.box.unit.currencyCode), tenant scoping and the soft-delete split.
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

func TestDoctorCurrencyCodeMismatch_CompositeSubField(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()
	const tenantA = "d4a4a4a4-0000-0000-0000-00000000000a"
	const tenantB = "d4b4b4b4-0000-0000-0000-00000000000b"
	const box = `{"subFields":[{"name":"unit","fieldType":"currency","options":{"currencyCode":"VND"}},{"name":"note","fieldType":"text"}]}`

	entA := dsSeedEntity(t, db, tenantA, "d4_deals")
	dsSeedField(t, db, tenantA, entA, "box", "composite", false, box)
	entB := dsSeedEntity(t, db, tenantB, "d4_deals")
	dsSeedField(t, db, tenantB, entB, "box", "composite", false, box)
	if err := dyn.CreateTable(ctx, "d4_deals", []ddl.FieldDef{{Name: "box", FieldType: "composite"}}); err != nil {
		t.Fatalf("create obj_d4_deals: %v", err)
	}
	insert := func(tenant, code string) string {
		t.Helper()
		id, err := dyn.InsertRecord(ctx, "d4_deals", tenant, "u-"+tenant[:4], map[string]any{
			"box": map[string]any{"unit": map[string]any{"amount": 10.0, "currencyCode": code}}}, nil)
		if err != nil {
			t.Fatalf("insert %s %s: %v", tenant, code, err)
		}
		return id
	}
	idUSD := insert(tenantA, "USD")
	insert(tenantA, "VND")
	idTrashed := insert(tenantA, "USD")
	dsExec(t, db, "UPDATE `obj_d4_deals` SET `deleted_at` = NOW() WHERE `tenant_id` = ? AND `id` = ?", tenantA, idTrashed)
	insert(tenantB, "USD") // co-tenant row on the same table

	got := drScanCounts(t, svc, tenantA)[DoctorCheckCurrencyCodeMismatch]
	if got.Count != 1 || got.Items[0].Key != "d4_deals.box.unit:"+idUSD {
		t.Fatalf("D2 tenant A = %+v; want only the live USD row %s at box.unit", got, idUSD)
	}

	live, trashed, err := dyn.CountCurrencyCodeRows(ctx, "d4_deals", tenantA, "box.unit", "USD")
	if err != nil || live != 1 || trashed != 1 {
		t.Fatalf("CountCurrencyCodeRows(box.unit, USD) = %d live, %d trashed, %v; want 1, 1, nil", live, trashed, err)
	}
	if _, _, err := dyn.CountCurrencyCodeRows(ctx, "d4_deals", tenantA, "box.unit.x", "USD"); err == nil {
		t.Fatal("a path deeper than composite.sub must be refused")
	}
}
