package services

// sample_generate_test.go — specs/064-preview-as-role-sample-data S9-5 (T090 drift guard, T092).
// SampleDataService.Generate over in-memory fakes: the generator reads field metadata at run time,
// feeds RecordService.CreateBulk (here a recording fake) per table in parent-first order under a
// generator-sourced sample mark, and reports created / failed / skipped / addedParents.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/sampledata"
)

// genCatalog serves the live entities of a tenant with their fields.
type genCatalog struct {
	entities map[string]string                     // tenant|name -> label
	fields   map[string][]metadata.FieldDefinition // tenant|name -> fields
}

func (c genCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if label, ok := c.entities[tenantID+"|"+name]; ok {
		return &metadata.EntityDefinition{ID: "id-" + name, TenantID: tenantID, Name: name, Label: label}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (c genCatalog) ListFields(_ context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return c.fields[tenantID+"|"+entityID[len("id-"):]], nil
}

// genStore answers the candidate probes; it records the tenant of every call.
type genStore struct {
	fakeSampleStore
	ids     map[string][]string // tenant|entity -> live ids
	users   map[string][]string // tenant -> active user ids
	tenants []string
}

func (s *genStore) LiveRecordIDs(_ context.Context, tenantID, entity string, _ int) ([]string, error) {
	s.tenants = append(s.tenants, tenantID)
	return s.ids[tenantID+"|"+entity], nil
}

func (s *genStore) ActiveUserIDs(_ context.Context, tenantID string, _ int) ([]string, error) {
	s.tenants = append(s.tenants, tenantID)
	return s.users[tenantID], nil
}

// genWriter records every CreateBulk call and answers with per-row results.
type genWriter struct {
	calls    []genCall
	failRow  map[string]int // entity -> row index answered "failed"
	onCreate func()
}

type genCall struct {
	entity string
	rows   []map[string]any
	mark   dynrepo.SampleMark
	marked bool
	mode   BulkMode
}

func (w *genWriter) CreateBulk(ctx context.Context, _ RecordScope, _, _, entity string, rows []map[string]any, mode BulkMode) (BulkCreateResult, error) {
	mark, ok := dynrepo.SampleMarkFrom(ctx)
	w.calls = append(w.calls, genCall{entity: entity, rows: rows, mark: mark, marked: ok, mode: mode})
	if w.onCreate != nil {
		w.onCreate()
	}
	res := BulkCreateResult{Mode: string(mode)}
	for i := range rows {
		if bad, has := w.failRow[entity]; has && bad == i {
			res.Failed++
			res.Results = append(res.Results, BulkRowResult{Index: i, Status: "failed", Code: "RECORD_DUPLICATE_UNIQUE", Error: "trùng"})
			continue
		}
		res.Created++
		res.Results = append(res.Results, BulkRowResult{Index: i, Status: "created", ID: fmt.Sprintf("%s-%d", entity, i)})
	}
	return res, nil
}

func fdef(name, ft string, required bool, opts map[string]any) metadata.FieldDefinition {
	f := metadata.FieldDefinition{Name: name, Label: name, FieldType: ft, IsRequired: required}
	if opts != nil {
		b, _ := json.Marshal(opts)
		s := string(b)
		f.Options = &s
	}
	return f
}

// threeTableApp is the S9-5 acceptance app: customers, appointments (required customer), services.
func threeTableApp() genCatalog {
	return genCatalog{
		entities: map[string]string{"t1|customers": "Khách hàng", "t1|appointments": "Lịch hẹn", "t1|services": "Dịch vụ", "t1|docs": "Hồ sơ", "t2|customers": "KH"},
		fields: map[string][]metadata.FieldDefinition{
			"t1|customers": {fdef("name", "text", true, nil), fdef("email", "email", true, nil), fdef("phone", "text", false, nil)},
			"t1|appointments": {
				fdef("customer", "relation", true, map[string]any{"relatedEntity": "customers"}),
				fdef("status", "picklist", true, map[string]any{"values": []string{"Mới", "Xong"}}),
				fdef("day", "date", true, nil),
			},
			"t1|services":  {fdef("price", "currency", true, nil), fdef("code", "auto_number", false, map[string]any{"prefix": "DV-"})},
			"t1|docs":      {fdef("file", "file", true, nil)},
			"t2|customers": {fdef("name", "text", true, nil)},
		},
	}
}

func newGenSvc(cat genCatalog, store *genStore, w *genWriter) (*SampleDataService, *spyAuditor) {
	aud := &spyAuditor{}
	return NewSampleDataService(store, cat, w).WithAuditor(aud), aud
}

var genScope = RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}

func TestSampleDataService_Generate_Validation(t *testing.T) {
	many := make([]string, maxSampleEntities+1)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	tests := []struct {
		name   string
		tenant string
		req    GenerateRequest
		want   string
	}{
		{"perEntity 0", "t1", GenerateRequest{Entities: []string{"customers"}, PerEntity: 0}, "SAMPLE_PER_ENTITY_INVALID"},
		{"perEntity 21", "t1", GenerateRequest{Entities: []string{"customers"}, PerEntity: 21}, "SAMPLE_PER_ENTITY_INVALID"},
		{"unknown entity", "t1", GenerateRequest{Entities: []string{"nope"}, PerEntity: 5}, "SAMPLE_ENTITY_NOT_FOUND"},
		{"no entity chosen", "t1", GenerateRequest{PerEntity: 5}, "SAMPLE_ENTITY_NOT_FOUND"},
		{"another tenant's entity is not found", "t2", GenerateRequest{Entities: []string{"services"}, PerEntity: 5}, "SAMPLE_ENTITY_NOT_FOUND"},
		{"too many entities", "t1", GenerateRequest{Entities: many, PerEntity: 5}, "SAMPLE_TOO_MANY_ENTITIES"},
		{"pack source without a pack seeder wired", "t1", GenerateRequest{Entities: []string{"customers"}, PerEntity: 5, Source: "pack"}, "SAMPLE_PACK_UNAVAILABLE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &genWriter{}
			svc, _ := newGenSvc(threeTableApp(), &genStore{}, w)
			_, err := svc.Generate(context.Background(), genScope, tc.tenant, "u1", tc.req)
			if !codeIs(err, tc.want) || len(w.calls) != 0 {
				t.Fatalf("err = %v calls=%d, want %s and no write", err, len(w.calls), tc.want)
			}
		})
	}
}

func TestSampleDataService_Generate_Busy(t *testing.T) {
	w := &genWriter{}
	svc, _ := newGenSvc(threeTableApp(), &genStore{}, w)
	req := GenerateRequest{Entities: []string{"customers"}, PerEntity: 2}
	w.onCreate = func() {
		w.onCreate = nil
		if _, err := svc.Generate(context.Background(), genScope, "t1", "u1", req); !codeIs(err, "SAMPLE_BUSY") {
			t.Errorf("second Generate while one runs: %v, want SAMPLE_BUSY", err)
		}
		if _, err := svc.CreateRecords(context.Background(), genScope, "t1", "u1", "customers", nil, BulkModeAtomic); !codeIs(err, "SAMPLE_BUSY") {
			t.Errorf("CreateRecords while Generate runs: %v, want SAMPLE_BUSY (one lock for every sample write)", err)
		}
	}
	if _, err := svc.Generate(context.Background(), genScope, "t1", "u1", req); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if _, err := svc.Generate(context.Background(), genScope, "t1", "u1", req); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
}

// Appointments chosen while customers is empty: customers is generated first and added to the report.
func TestSampleDataService_Generate_AddsEmptyRequiredParent(t *testing.T) {
	store := &genStore{}
	w := &genWriter{}
	svc, aud := newGenSvc(threeTableApp(), store, w)
	res, err := svc.Generate(context.Background(), genScope, "t1", "u1", GenerateRequest{Entities: []string{"appointments"}, PerEntity: 5})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(w.calls) != 2 || w.calls[0].entity != "customers" || w.calls[1].entity != "appointments" {
		t.Fatalf("calls = %+v, want customers then appointments", w.calls)
	}
	for _, c := range w.calls {
		if !c.marked || c.mark.Source != dynrepo.SampleSourceGenerator || c.mark.BatchID != res.BatchID || c.mode != BulkModePartial || len(c.rows) != 5 {
			t.Fatalf("call %s: mark=%+v marked=%v mode=%s rows=%d; want generator mark of batch %s, partial, 5 rows", c.entity, c.mark, c.marked, c.mode, len(c.rows), res.BatchID)
		}
	}
	for _, r := range w.calls[1].rows {
		if id, _ := r["customer"].(string); len(id) < len("customers-0") || id[:9] != "customers" {
			t.Fatalf("appointment row %v: customer must be an id just created in this batch", r)
		}
	}
	if !reflect.DeepEqual(res.AddedParents, []string{"customers"}) || len(res.Created) != 2 || res.Created[0] != (SampleCreated{"customers", 5}) {
		t.Fatalf("result = %+v, want customers added, created customers=5 then appointments=5", res)
	}
	if len(aud.events) != 1 || aud.events[0].EntityName != audit.SampleDataMeta || aud.events[0].RecordID != res.BatchID || aud.events[0].Action != audit.ActionCreate {
		t.Fatalf("audit = %+v, want one __sample_data__ create for the batch", aud.events)
	}
}

// Customers already have rows: appointments point at them, nothing is added or written for customers.
func TestSampleDataService_Generate_UsesExistingParentRows(t *testing.T) {
	store := &genStore{ids: map[string][]string{"t1|customers": {"c1", "c2"}}}
	w := &genWriter{}
	svc, _ := newGenSvc(threeTableApp(), store, w)
	res, err := svc.Generate(context.Background(), genScope, "t1", "u1", GenerateRequest{Entities: []string{"appointments"}, PerEntity: 4})
	if err != nil || len(res.AddedParents) != 0 || len(w.calls) != 1 {
		t.Fatalf("res=%+v err=%v calls=%d, want one write and no added parent", res, err, len(w.calls))
	}
	for _, r := range w.calls[0].rows {
		if r["customer"] != "c1" && r["customer"] != "c2" {
			t.Fatalf("customer = %v, want an existing id", r["customer"])
		}
	}
	for _, tn := range store.tenants {
		if tn != "t1" {
			t.Fatalf("store probed tenant %q, want only t1", tn)
		}
	}
}

func TestSampleDataService_Generate_ReportsFailedAndSkipped(t *testing.T) {
	w := &genWriter{failRow: map[string]int{"customers": 1}}
	svc, _ := newGenSvc(threeTableApp(), &genStore{}, w)
	res, err := svc.Generate(context.Background(), genScope, "t1", "u1", GenerateRequest{Entities: []string{"customers", "docs", "services"}, PerEntity: 3})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if want := []SampleFailed{{Entity: "customers", Index: 1, Code: "RECORD_DUPLICATE_UNIQUE", Error: "trùng"}}; !reflect.DeepEqual(res.Failed, want) {
		t.Fatalf("failed = %+v, want %+v (the code of specs/009-bulk-create, kept)", res.Failed, want)
	}
	if want := []SampleSkipped{{Entity: "docs", Code: "SAMPLE_FILE_REQUIRED"}}; !reflect.DeepEqual(res.Skipped, want) {
		t.Fatalf("skipped = %+v, want %+v", res.Skipped, want)
	}
	got := map[string]int{}
	for _, c := range res.Created {
		got[c.Entity] = c.Count
	}
	if got["customers"] != 2 || got["services"] != 3 || got["docs"] != 0 {
		t.Fatalf("created = %+v, want customers 2, services 3", res.Created)
	}
	for _, c := range w.calls {
		if c.entity == "docs" {
			t.Fatal("a skipped table must not be written")
		}
		if c.entity == "services" { // server-computed auto_number is never sent
			if _, has := c.rows[0]["code"]; has {
				t.Fatalf("services row %v carries the auto_number", c.rows[0])
			}
		}
	}
}

func TestSampleDataService_Generate_ResultArraysAreNeverNull(t *testing.T) {
	svc, _ := newGenSvc(threeTableApp(), &genStore{}, &genWriter{})
	res, err := svc.Generate(context.Background(), genScope, "t1", "u1", GenerateRequest{Entities: []string{"customers"}, PerEntity: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"created", "failed", "skipped", "addedParents"} {
		if _, ok := m[k].([]any); !ok {
			t.Fatalf("%s = %#v in %s, want a JSON array", k, m[k], b)
		}
	}
}

// ---- drift guard + validators (T090) ----

// Every engine field type is known to the generator: a new type must be handled (or left to the server).
func TestSampleGenerator_HandlesEveryEngineFieldType(t *testing.T) {
	for _, ft := range AllowedFieldTypes() {
		if !sampledata.Handles(ft) {
			t.Errorf("sampledata.Handles(%q) = false: teach the generator this field type", ft)
		}
	}
}

// What the generator makes passes the real record validators for every kind of field.
func TestSampleGenerator_RowsPassTheRecordValidators(t *testing.T) {
	provinces := []any{"Hà Nội", "Đà Nẵng"}
	e := sampledata.Entity{Name: "customers", Label: "Khách hàng", Fields: []sampledata.Field{
		{Name: "ho_ten", Label: "Họ tên", Type: "text", Required: true, Unique: true, Options: map[string]any{"maxLength": float64(60)}},
		{Name: "dien_thoai", Label: "Điện thoại", Type: "text", Options: map[string]any{"pattern": `0\d{9}`}},
		{Name: "mail", Type: "email", Required: true, Unique: true},
		{Name: "so_luong", Type: "number", Required: true, Options: map[string]any{"min": float64(10), "max": float64(90), "step": float64(10)}},
		{Name: "ma_so", Type: "number", Unique: true, Options: map[string]any{"min": float64(1), "max": float64(1e9)}},
		{Name: "gia", Type: "currency", Required: true, Options: map[string]any{"currencyCode": "VND", "precision": float64(0)}},
		{Name: "sao", Type: "rating", Required: true, Options: map[string]any{"max": float64(5)}},
		{Name: "vip", Type: "boolean", Required: true},
		{Name: "ngay", Type: "date", Required: true},
		{Name: "gio", Type: "datetime", Required: true},
		{Name: "tinh", Type: "picklist", Options: map[string]any{"values": provinces}},
		{Name: "huyen", Type: "picklist", Required: true, Options: map[string]any{
			"values": []any{"Ba Đình", "Hải Châu"}, "controllingField": "tinh",
			"dependentValues": map[string]any{"Hà Nội": []any{"Ba Đình"}, "Đà Nẵng": []any{"Hải Châu"}}}},
		{Name: "nhom", Type: "multi_select", Required: true, Options: map[string]any{"values": []any{"a", "b", "c"}}},
		{Name: "gioi_thieu", Type: "rich_text", Required: true, Options: map[string]any{"maxLength": float64(120)}},
		{Name: "lien_he", Type: "composite", Required: true, Options: map[string]any{"subFields": []any{
			map[string]any{"name": "dia_chi", "fieldType": "text", "required": true},
			map[string]any{"name": "email", "fieldType": "email"},
			map[string]any{"name": "diem", "fieldType": "rating", "options": map[string]any{"max": float64(10)}},
		}}},
		{Name: "cong_ty", Type: "relation", Required: true, Options: map[string]any{"relatedEntity": "accounts"}},
		{Name: "lien_quan", Type: "polymorphic_relation", Required: true, Options: map[string]any{"targetEntities": []any{"accounts"}}},
		{Name: "nguoi_phu_trach", Type: "user", Required: true},
	}}
	specs := make([]FieldSpec, len(e.Fields))
	for i, f := range e.Fields {
		specs[i] = FieldSpec{Name: f.Name, Label: f.Label, FieldType: f.Type, IsRequired: f.Required, IsUnique: f.Unique, Options: f.Options}
	}
	rows, skip := sampledata.Rows(e, 20, "batch-validators", time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
		sampledata.Candidates{Relation: map[string][]string{"accounts": {"a1", "a2"}}, Users: []string{"u1"}})
	if skip != "" {
		t.Fatalf("skipped: %s", skip)
	}
	for i, row := range rows {
		clean, err := ValidateRecord(specs, row)
		if err != nil {
			t.Fatalf("row %d %v: %v", i, row, err)
		}
		if err := checkDependentPicklists(specs, clean, nil); err != nil {
			t.Fatalf("row %d %v: %v", i, row, err)
		}
	}
	if !slices.Contains(AllowedFieldTypes(), "composite") {
		t.Fatal("test fixture drifted from the engine")
	}
}
