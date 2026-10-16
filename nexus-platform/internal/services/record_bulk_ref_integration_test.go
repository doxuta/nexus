//go:build integration

// ENG-SAMPLE-REF integration test — proves the "@ref" cross-entity resolution (record_bulk.go /
// record_ref_registry.go) end-to-end on REAL MySQL, over the exact "Dự án → Công việc" shape the
// task names (a construction-company project management app): two DISTINCT entities, seeded via
// TWO SEPARATE CreateBulk(mode=partial) calls — exactly how the AI Builder FE applies two
// "records" draft cards in sequence (AiBuilder.tsx applyAll; propose_records' Apply is
// mode=partial). A fake-store unit test (record_bulk_test.go) cannot exercise two distinct obj_*
// tables; this test can.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestRecordBulkRef -race -v
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const refIntegrationTenant = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeeeee"

// refWireRecordService builds an EntityService + RecordService pair over a fresh throwaway DB,
// wired for CreateBulk's mode=partial path (Create() per row) and mode=atomic (InsertRecordsAtomic,
// as main.go wires it) — mirrors engWire's shape.
func refWireRecordService(t *testing.T) (*EntityService, *RecordService) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo).WithBulkInserter(dynRepo)
	return entitySvc, recordSvc
}

// TestRecordBulkRef_ProjectThenTask_ResolvesRealID applies one "lô" (Dự án's sample records, then
// Công việc's) exactly as the AI Builder's FE would: two independent CreateBulk calls, the second
// entity's REQUIRED relation field referencing the first via "@ref" instead of a bare/blank id —
// the concrete case propose_records used to refuse (RECORD field relation BẮT BUỘC → "bỏ qua
// records").
func TestRecordBulkRef_ProjectThenTask_ResolvesRealID(t *testing.T) {
	entitySvc, recordSvc := refWireRecordService(t)
	ctx := context.Background()
	tenant := refIntegrationTenant
	scope := engAdmin("u-founder")

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "du_an",
		Label: "Dự án",
		Fields: []FieldInput{
			{Name: "ten_du_an", Label: "Tên dự án", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity du_an: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "cong_viec",
		Label: "Công việc",
		Fields: []FieldInput{
			{Name: "ten_cong_viec", Label: "Tên công việc", FieldType: "text", IsRequired: true},
			{
				Name: "du_an_id", Label: "Dự án", FieldType: "relation", IsRequired: true,
				Options: map[string]any{"relatedEntity": "du_an"},
			},
		},
	}); err != nil {
		t.Fatalf("CreateEntity cong_viec: %v", err)
	}

	// Draft card 1 (Dự án's sample records) — Apply order: Dự án BEFORE Công việc, matching
	// AiBuilder.tsx applyAll's dependency-rank / original-proposal-order tie-break.
	projectRows := []map[string]any{
		{"ten_du_an": "Xây nhà phố Quận 9", "$ref": "@du_an_1"},
	}
	projectRes, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "du_an", projectRows, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk du_an: %v", err)
	}
	if projectRes.Created != 1 || projectRes.Failed != 0 {
		t.Fatalf("du_an result = %+v; want 1 created, 0 failed", projectRes)
	}
	projectID := projectRes.Results[0].ID

	// Draft card 2 (Công việc's sample records) — the REQUIRED relation field references the
	// project by "@ref", not a real id (the model never had one to give).
	taskRows := []map[string]any{
		{"ten_cong_viec": "Đổ móng", "du_an_id": "@du_an_1"},
		{"ten_cong_viec": "Xây thô", "du_an_id": "@du_an_1"},
	}
	taskRes, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "cong_viec", taskRows, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk cong_viec: %v", err)
	}
	if taskRes.Created != 2 || taskRes.Failed != 0 {
		t.Fatalf("cong_viec result = %+v; want 2 created, 0 failed (child rows must not be empty/orphaned)", taskRes)
	}

	// Read back BOTH task rows for real — the child column must hold the PARENT'S REAL id, not
	// the literal "@du_an_1" token and not blank.
	for _, row := range taskRes.Results {
		rec, err := recordSvc.Get(ctx, scope, tenant, "cong_viec", row.ID, nil, nil)
		if err != nil {
			t.Fatalf("Get cong_viec %s: %v", row.ID, err)
		}
		if got := rec.Data["du_an_id"]; got != projectID {
			t.Fatalf("cong_viec %s .du_an_id = %v; want the real du_an id %q", row.ID, got, projectID)
		}
	}
}

// TestRecordBulkRef_AppliedOutOfOrder_FailsHonestNotHang: applying the CHILD's records BEFORE the
// PARENT's (the human clicking the wrong draft card first) must fail loudly and per-row — never
// hang, never silently create an orphaned/blank-relation row.
func TestRecordBulkRef_AppliedOutOfOrder_FailsHonestNotHang(t *testing.T) {
	entitySvc, recordSvc := refWireRecordService(t)
	ctx := context.Background()
	tenant := refIntegrationTenant
	scope := engAdmin("u-founder")

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:   "du_an",
		Label:  "Dự án",
		Fields: []FieldInput{{Name: "ten_du_an", Label: "Tên dự án", FieldType: "text", IsRequired: true}},
	}); err != nil {
		t.Fatalf("CreateEntity du_an: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "cong_viec",
		Label: "Công việc",
		Fields: []FieldInput{
			{Name: "ten_cong_viec", Label: "Tên công việc", FieldType: "text", IsRequired: true},
			{Name: "du_an_id", Label: "Dự án", FieldType: "relation", IsRequired: true,
				Options: map[string]any{"relatedEntity": "du_an"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity cong_viec: %v", err)
	}

	taskRows := []map[string]any{{"ten_cong_viec": "Đổ móng", "du_an_id": "@du_an_1"}}
	res, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "cong_viec", taskRows, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk cong_viec: %v", err)
	}
	if res.Created != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v; want 0 created, 1 failed (ref not registered yet)", res)
	}
	if res.Results[0].Code != "RECORD_REF_UNRESOLVED" {
		t.Fatalf("code = %q; want RECORD_REF_UNRESOLVED", res.Results[0].Code)
	}
}

// TestRecordBulkRef_Polymorphic_ResolvesRealID (queue #140): the "liên quan tới" shape — an
// activity's polymorphic related_to {type:"khach_hang", id:"@kh_1"} applied as its own CreateBulk
// call after the khach_hang card — persists the customer's REAL id (real obj_* tables, real
// existence probe, both bulk modes), and a ref of another entity than {type} is refused.
func TestRecordBulkRef_Polymorphic_ResolvesRealID(t *testing.T) {
	entitySvc, recordSvc := refWireRecordService(t)
	ctx := context.Background()
	tenant := refIntegrationTenant
	scope := engAdmin("u-founder")

	for _, name := range []string{"khach_hang", "lien_he"} {
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
			Name: name, Label: name,
			Fields: []FieldInput{{Name: "ten", Label: "Tên", FieldType: "text", IsRequired: true}},
		}); err != nil {
			t.Fatalf("CreateEntity %s: %v", name, err)
		}
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "hoat_dong", Label: "Hoạt động",
		Fields: []FieldInput{
			{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
			{Name: "lien_quan", Label: "Liên quan tới", FieldType: "polymorphic_relation",
				Options: map[string]any{"targetEntities": []any{"khach_hang", "lien_he"}}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity hoat_dong: %v", err)
	}

	custRes, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "khach_hang",
		[]map[string]any{{"ten": "Công ty An Phát", "$ref": "@kh_1"}}, BulkModePartial)
	if err != nil || custRes.Created != 1 {
		t.Fatalf("CreateBulk khach_hang: res=%+v err=%v", custRes, err)
	}
	custID := custRes.Results[0].ID

	for _, mode := range []BulkMode{BulkModePartial, BulkModeAtomic} {
		res, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "hoat_dong", []map[string]any{
			{"tieu_de": "Gọi chào hàng " + string(mode), "lien_quan": map[string]any{"type": "khach_hang", "id": "@kh_1"}},
		}, mode)
		if err != nil || res.Created != 1 || res.Failed != 0 {
			t.Fatalf("%s: CreateBulk hoat_dong: res=%+v err=%v; want 1 created", mode, res, err)
		}
		rec, err := recordSvc.Get(ctx, scope, tenant, "hoat_dong", res.Results[0].ID, nil, nil)
		if err != nil {
			t.Fatalf("%s: Get hoat_dong: %v", mode, err)
		}
		rel, _ := rec.Data["lien_quan"].(map[string]any)
		if rel["type"] != "khach_hang" || rel["id"] != custID {
			t.Fatalf("%s: lien_quan = %v; want {type:khach_hang, id:%q}", mode, rec.Data["lien_quan"], custID)
		}
	}

	res, err := recordSvc.CreateBulk(ctx, scope, tenant, "u-founder", "hoat_dong", []map[string]any{
		{"tieu_de": "Sai loại", "lien_quan": map[string]any{"type": "lien_he", "id": "@kh_1"}},
	}, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk hoat_dong (wrong type): %v", err)
	}
	if res.Created != 0 || res.Results[0].Code != "RECORD_REF_UNRESOLVED" {
		t.Fatalf("wrong-type ref: res=%+v; want 0 created / RECORD_REF_UNRESOLVED", res)
	}
}
