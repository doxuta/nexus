package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// polyRefSvc: "cong_viec" with a polymorphic_relation "lien_quan" ("liên quan tới") that may point
// at a cong_viec or a du_an (the fake catalog serves one entity; the ref registry and the
// existence probe are what is under test).
func polyRefSvc(t *testing.T, store *bulkTestStore) *RecordService {
	t.Helper()
	cat := &bulkCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-cv", TenantID: "tenant-1", Name: "cong_viec", DataTable: "obj_cong_viec"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "lien_quan", FieldType: "polymorphic_relation",
				Options: jsonOptsPtr(t, map[string]any{"targetEntities": []any{"cong_viec", "du_an"}})},
		},
	}
	return NewRecordService(cat, store).WithSequenceClaimer(&fakeSequenceClaimer{})
}

// TestCreateBulk_Ref_Polymorphic_ResolvesNestedID (queue #140): the id inside a polymorphic
// {type, id} value may be an "@ref" registered by an EARLIER CreateBulk call — resolved to the real
// id before the existence probe, in both modes (partial re-submits the payload via Create(), atomic
// inserts the clean row). Before, the nested id was never inspected: the row failed the existence
// probe (RECORD_POLYMORPHIC_TARGET_INVALID) because "@p1" is not a record id.
func TestCreateBulk_Ref_Polymorphic_ResolvesNestedID(t *testing.T) {
	for _, mode := range []BulkMode{BulkModePartial, BulkModeAtomic} {
		t.Run(string(mode), func(t *testing.T) {
			store := &bulkTestStore{live: map[string]bool{}}
			inserter := &fakeBulkInserter{}
			svc := polyRefSvc(t, store).WithBulkInserter(inserter)
			ctx := context.Background()

			res, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec",
				[]map[string]any{{"name": "Dự án A", "$ref": "@p1"}}, mode)
			if err != nil || res.Created != 1 {
				t.Fatalf("parent: res=%+v err=%v", res, err)
			}
			parentID := res.Results[0].ID
			store.live["cong_viec|tenant-1|"+parentID] = true

			res2, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec",
				[]map[string]any{{"name": "Gọi lại", "lien_quan": map[string]any{"type": "cong_viec", "id": "@p1"}}}, mode)
			if err != nil || res2.Created != 1 || res2.Failed != 0 {
				t.Fatalf("child: res=%+v err=%v; want 1 created", res2, err)
			}
			persisted := inserter.lastRows
			if mode == BulkModePartial {
				persisted = store.inserted[len(store.inserted)-1:]
			}
			got := fmt.Sprint(persisted[0]["lien_quan"])
			if want := fmt.Sprint(map[string]any{"type": "cong_viec", "id": parentID}); got != want {
				t.Fatalf("lien_quan = %s; want %s", got, want)
			}
		})
	}
}

// TestCreateBulk_Ref_Polymorphic_Refused: a nested "@ref" nobody registered, or one registered by
// a record of another entity than {type}, fails the row with RECORD_REF_UNRESOLVED on the field —
// never a silent link to the wrong record.
func TestCreateBulk_Ref_Polymorphic_Refused(t *testing.T) {
	tests := []struct {
		name, typ, id, wantMsg string
	}{
		{name: "dangling", typ: "cong_viec", id: "@khong_co", wantMsg: "@khong_co"},
		{name: "ref of another entity than type", typ: "du_an", id: "@p1", wantMsg: `"cong_viec"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &bulkTestStore{live: map[string]bool{}}
			svc := polyRefSvc(t, store)
			ctx := context.Background()
			res, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec",
				[]map[string]any{{"name": "Dự án A", "$ref": "@p1"}}, BulkModePartial)
			if err != nil || res.Created != 1 {
				t.Fatalf("parent: res=%+v err=%v", res, err)
			}
			store.live["cong_viec|tenant-1|"+res.Results[0].ID] = true

			res2, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec",
				[]map[string]any{{"name": "Gọi lại", "lien_quan": map[string]any{"type": tc.typ, "id": tc.id}}}, BulkModePartial)
			if err != nil {
				t.Fatalf("CreateBulk: %v", err)
			}
			row := res2.Results[0]
			if res2.Failed != 1 || row.Code != "RECORD_REF_UNRESOLVED" || row.Details["field"] != "lien_quan" || !strings.Contains(row.Error, tc.wantMsg) {
				t.Fatalf("result = %+v; want 1 failed RECORD_REF_UNRESOLVED on lien_quan mentioning %s", res2, tc.wantMsg)
			}
		})
	}
}
