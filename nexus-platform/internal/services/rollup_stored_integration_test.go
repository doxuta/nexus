//go:build integration

// specs/083-rollup-stored-value against real MySQL: a stored roll-up follows child create / move /
// delete / restore, backfills rows that already exist, filters and sums like a number field, and
// never mixes tenants.
//
//	TEST_DATABASE_URL=... go test -tags=integration ./internal/services/ -run TestStoredRollup -v
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

const storedUser = "11111111-1111-1111-1111-111111111183"

// storedRollupWorld: classes(name) <- enrollments(class, fee currency VND). Returns services.
func storedRollupWorld(t *testing.T, tenant string, entitySvc *EntityService) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "classes", Label: "Lớp",
		Fields: []FieldInput{{Name: "name", Label: "Tên", FieldType: "text", IsRequired: true}}}); err != nil {
		t.Fatalf("CreateEntity classes: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "enrollments", Label: "Ghi danh",
		Fields: []FieldInput{
			{Name: "student", Label: "Học viên", FieldType: "text"},
			{Name: "class", Label: "Lớp", FieldType: "relation", Options: map[string]any{"relatedEntity": "classes"}},
			{Name: "fee", Label: "Học phí", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
		}}); err != nil {
		t.Fatalf("CreateEntity enrollments: %v", err)
	}
}

func addStoredRollups(t *testing.T, fieldSvc *FieldService) {
	t.Helper()
	ctx := context.Background()
	for _, f := range []FieldInput{
		{Name: "si_so", Label: "Sĩ số", FieldType: "rollup", Options: map[string]any{
			"childEntity": "enrollments", "viaRelation": "class", "metric": "count", "stored": true}},
		{Name: "thu", Label: "Đã thu", FieldType: "rollup", Options: map[string]any{
			"childEntity": "enrollments", "viaRelation": "class", "metric": "sum", "aggField": "fee", "stored": true}},
	} {
		if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{EntityName: "classes", Field: f}); err != nil {
			t.Fatalf("AddField %s: %v", f.Name, err)
		}
	}
}

func storedSiSo(t *testing.T, svc *RecordService, tenant, id string) any {
	t.Helper()
	rec, err := svc.Get(context.Background(), engAdmin(storedUser), tenant, "classes", id, nil, nil)
	if err != nil {
		t.Fatalf("Get class %s: %v", id, err)
	}
	return rec.Data["si_so"]
}

func TestStoredRollup_FollowsChildWrites(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	storedRollupWorld(t, engTenantA, entitySvc)
	fieldSvc.WithStoredRollupSweep(recordSvc.RecomputeStoredRollup)
	addStoredRollups(t, fieldSvc)
	admin := engAdmin(storedUser)

	c1, err := recordSvc.Create(ctx, admin, engTenantA, storedUser, "classes", map[string]any{"name": "Lớp 1"})
	if err != nil {
		t.Fatalf("create c1: %v", err)
	}
	c2, err := recordSvc.Create(ctx, admin, engTenantA, storedUser, "classes", map[string]any{"name": "Lớp 2"})
	if err != nil {
		t.Fatalf("create c2: %v", err)
	}
	var e1 string
	for i, fee := range []float64{100, 200, 300} {
		id, err := recordSvc.Create(ctx, admin, engTenantA, storedUser, "enrollments", map[string]any{
			"student": "HV", "class": c1, "fee": map[string]any{"amount": fee, "currencyCode": "VND"}})
		if err != nil {
			t.Fatalf("create enrollment %d: %v", i, err)
		}
		if i == 0 {
			e1 = id
		}
	}
	if got := storedSiSo(t, recordSvc, engTenantA, c1); got != float64(3) {
		t.Fatalf("after 3 enrollments si_so = %#v; want 3", got)
	}
	rec, _ := recordSvc.Get(ctx, admin, engTenantA, "classes", c1, nil, nil)
	if thu, _ := rec.Data["thu"].(map[string]any); thu["amount"] != float64(600) || thu["currencyCode"] != "VND" {
		t.Fatalf("thu = %#v; want {600 VND}", rec.Data["thu"])
	}

	// Move one enrollment to the other class: both totals change.
	if err := recordSvc.Update(ctx, admin, engTenantA, "enrollments", e1, map[string]any{"class": c2}, nil); err != nil {
		t.Fatalf("move enrollment: %v", err)
	}
	if a, b := storedSiSo(t, recordSvc, engTenantA, c1), storedSiSo(t, recordSvc, engTenantA, c2); a != float64(2) || b != float64(1) {
		t.Fatalf("after move si_so = %v / %v; want 2 / 1", a, b)
	}

	// Delete and restore.
	if err := recordSvc.Delete(ctx, admin, engTenantA, "enrollments", e1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := storedSiSo(t, recordSvc, engTenantA, c2); got != float64(0) {
		t.Fatalf("after delete c2 si_so = %#v; want 0/absent", got)
	}
	if err := recordSvc.Restore(ctx, admin, engTenantA, "enrollments", e1); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := storedSiSo(t, recordSvc, engTenantA, c2); got != float64(1) {
		t.Fatalf("after restore c2 si_so = %#v; want 1", got)
	}

	// Filter and report SUM work on the stored value.
	list, err := recordSvc.List(ctx, admin, engTenantA, "classes", ListQuery{Page: 1, Limit: 50,
		Filters: []FilterInput{{Field: "si_so", Operator: dynrepo.FilterGte, Values: []string{"2"}}}})
	if err != nil {
		t.Fatalf("filter si_so >= 2: %v", err)
	}
	if len(list.Records) != 1 || list.Records[0].ID != c1 {
		t.Fatalf("filter si_so>=2 returned %d rows; want only c1", len(list.Records))
	}
	// A brand-new class (no enrollment yet) starts at 0 and is found by "still has room".
	cNew, err := recordSvc.Create(ctx, admin, engTenantA, storedUser, "classes", map[string]any{"name": "Lớp mới"})
	if err != nil {
		t.Fatalf("create new class: %v", err)
	}
	room, err := recordSvc.List(ctx, admin, engTenantA, "classes", ListQuery{Page: 1, Limit: 50,
		Filters: []FilterInput{{Field: "si_so", Operator: dynrepo.FilterLt, Values: []string{"1"}}}})
	if err != nil {
		t.Fatalf("filter si_so < 1: %v", err)
	}
	foundNew := false
	for _, r := range room.Records {
		foundNew = foundNew || r.ID == cNew
	}
	if !foundNew {
		t.Fatalf("filter si_so<1 missed the new empty class %s", cNew)
	}
	agg, err := recordSvc.Aggregate(ctx, admin, engTenantA, "classes", AggregateQuery{Metric: AggMetricSum, Field: "si_so"})
	if err != nil {
		t.Fatalf("SUM(si_so): %v", err)
	}
	if agg.Scalar.Value == nil || *agg.Scalar.Value != "3.0000000000" && *agg.Scalar.Value != "3" && *agg.Scalar.Value != "3.00" {
		t.Fatalf("SUM(si_so) = %v; want 3", agg.Scalar.Value)
	}
	money, err := recordSvc.Aggregate(ctx, admin, engTenantA, "classes", AggregateQuery{Metric: AggMetricSum, Field: "thu"})
	if err != nil || money.CurrencyCode != "VND" || money.Scalar.Value == nil {
		t.Fatalf("SUM(thu) = %+v err=%v; want a VND total", money, err)
	}
}

func TestStoredRollup_BackfillsExistingRowsAndRecomputeRepairs(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, dynRepo, _ := engWire(t)
	ctx := context.Background()
	storedRollupWorld(t, engTenantA, entitySvc)
	fieldSvc.WithStoredRollupSweep(recordSvc.RecomputeStoredRollup)
	admin := engAdmin(storedUser)

	c1, _ := recordSvc.Create(ctx, admin, engTenantA, storedUser, "classes", map[string]any{"name": "Có sẵn"})
	for i := 0; i < 2; i++ {
		if _, err := recordSvc.Create(ctx, admin, engTenantA, storedUser, "enrollments", map[string]any{"class": c1}); err != nil {
			t.Fatalf("create enrollment: %v", err)
		}
	}
	addStoredRollups(t, fieldSvc) // the sweep runs here
	if got := storedSiSo(t, recordSvc, engTenantA, c1); got != float64(2) {
		t.Fatalf("backfill si_so = %#v; want 2", got)
	}

	// Corrupt the stored number, then repair it with the recompute command.
	rec, _ := recordSvc.Get(ctx, admin, engTenantA, "classes", c1, nil, nil)
	if ok, err := dynRepo.SetComputedValues(ctx, "classes", engTenantA, c1, rec.Version, map[string]any{"si_so": 99}); err != nil || !ok {
		t.Fatalf("corrupt: ok=%v err=%v", ok, err)
	}
	fields, _ := fieldSvc.meta.ListFields(ctx, engTenantA, storedEntityID(t, fieldSvc, "classes"))
	var fid string
	for _, f := range fields {
		if f.Name == "si_so" {
			fid = f.ID
		}
	}
	n, err := fieldSvc.RecomputeField(ctx, engTenantA, fid)
	if err != nil || n != 1 {
		t.Fatalf("RecomputeField = %d, %v; want 1 row repaired", n, err)
	}
	if got := storedSiSo(t, recordSvc, engTenantA, c1); got != float64(2) {
		t.Fatalf("after recompute si_so = %#v; want 2", got)
	}
	// Another tenant cannot recompute this field.
	if _, err := fieldSvc.RecomputeField(ctx, engTenantB, fid); err == nil {
		t.Fatal("a field of tenant A must be 404 for tenant B")
	}
}

func storedEntityID(t *testing.T, fieldSvc *FieldService, name string) string {
	t.Helper()
	e, err := fieldSvc.meta.GetEntityByName(context.Background(), engTenantA, name)
	if err != nil {
		t.Fatalf("GetEntityByName %s: %v", name, err)
	}
	return e.ID
}

func TestStoredRollup_TenantsDoNotMix(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	fieldSvc.WithStoredRollupSweep(recordSvc.RecomputeStoredRollup)
	storedRollupWorld(t, engTenantA, entitySvc)
	storedRollupWorld(t, engTenantB, entitySvc)
	addStoredRollups(t, fieldSvc)
	for _, f := range []FieldInput{{Name: "si_so", Label: "Sĩ số", FieldType: "rollup", Options: map[string]any{
		"childEntity": "enrollments", "viaRelation": "class", "metric": "count", "stored": true}}} {
		if _, err := fieldSvc.AddField(ctx, engTenantB, AddFieldInput{EntityName: "classes", Field: f}); err != nil {
			t.Fatalf("AddField B: %v", err)
		}
	}
	a := engAdmin(storedUser)
	cA, _ := recordSvc.Create(ctx, a, engTenantA, storedUser, "classes", map[string]any{"name": "A"})
	cB, _ := recordSvc.Create(ctx, a, engTenantB, storedUser, "classes", map[string]any{"name": "B"})
	for i := 0; i < 3; i++ {
		if _, err := recordSvc.Create(ctx, a, engTenantB, storedUser, "enrollments", map[string]any{"class": cB}); err != nil {
			t.Fatalf("create B enrollment: %v", err)
		}
	}
	if got := storedSiSo(t, recordSvc, engTenantB, cB); got != float64(3) {
		t.Fatalf("tenant B si_so = %#v; want 3", got)
	}
	if got := storedSiSo(t, recordSvc, engTenantA, cA); got != float64(0) {
		t.Fatalf("tenant A si_so = %#v; B's enrollments leaked into it", got)
	}
}
