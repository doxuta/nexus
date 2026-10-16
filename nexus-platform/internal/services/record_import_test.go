package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// --- Fakes for ImportService end-to-end tests ----------------------------------

// fakeImportCatalog stubs EntityCatalog for ImportService tests.
type fakeImportCatalog struct {
	entity *metadata.EntityDefinition
	fields []metadata.FieldDefinition
}

func (f *fakeImportCatalog) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	if f.entity == nil {
		return nil, metadata.ErrEntityNotFound
	}
	return f.entity, nil
}

func (f *fakeImportCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return f.fields, nil
}

// fakeImportStore stubs ImportStore + RelationBatchChecker and records whether InsertRecords
// was called and how many times BatchExistingValuesForField was invoked (to assert O(1) calls).
type fakeImportStore struct {
	insertCalled bool
	insertErr    error
	// existingRelIDs: "entity:id" → true means the id exists (for BatchRecordExists).
	// nil map means all ids exist (default "all-exist" behaviour for tests that do not care about FK).
	existingRelIDs map[string]bool
	// existingDedupVals is the set of values BatchExistingValuesForField reports as existing.
	// nil means no values exist in DB (default for tests that do not care about dedup).
	existingDedupVals map[string]struct{}
	// batchDedupCalls counts how many times BatchExistingValuesForField was called.
	batchDedupCalls int
	// insertedRows is what the last InsertRecords call would have persisted — lets a test
	// assert a value reached the store verbatim (Q7: a currency code is never rewritten).
	insertedRows []map[string]any
}

func (f *fakeImportStore) InsertRecords(_ context.Context, _, _, _ string, rows []map[string]any, _ []dynrepo.UniqueFieldSpec) ([]string, error) {
	f.insertCalled = true
	f.insertedRows = rows
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = fmt.Sprintf("rec-%d", i+1)
	}
	return ids, nil
}

func (f *fakeImportStore) BatchExistingValuesForField(_ context.Context, _, _, _ string, _ dynrepo.DedupFieldSpec, values []string) (map[string]struct{}, error) {
	f.batchDedupCalls++
	result := make(map[string]struct{}, len(values))
	for _, v := range values {
		if _, exists := f.existingDedupVals[v]; exists {
			result[v] = struct{}{}
		}
	}
	return result, nil
}

// BatchRecordExists satisfies RelationBatchChecker. When existingRelIDs is nil every id is
// considered present (backward-compat default for tests that don't exercise FK validation).
func (f *fakeImportStore) BatchRecordExists(_ context.Context, entity, _ string, ids []string) (map[string]bool, error) {
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		if f.existingRelIDs == nil {
			result[id] = true
		} else {
			result[id] = f.existingRelIDs[entity+":"+id]
		}
	}
	return result, nil
}

// fakeFieldACL is a test-double for fieldACLEnforcer. It denies writes on deniedField
// unless isAdmin is true.
type fakeFieldACL struct {
	deniedField string
}

func (f *fakeFieldACL) FilterReadFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}

func (f *fakeFieldACL) FilterReadFieldsBatch(_ context.Context, _, _, _ string, _ bool, _ []map[string]any) error {
	return nil
}

func (f *fakeFieldACL) CheckWriteFields(_ context.Context, _, _, _ string, isAdmin bool, payload map[string]any) error {
	if isAdmin {
		return nil
	}
	if f.deniedField != "" {
		if _, ok := payload[f.deniedField]; ok {
			return invalidField("FIELD_FORBIDDEN", f.deniedField, "bạn không có quyền sửa field %q", f.deniedField)
		}
	}
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (f *fakeFieldACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}

func (f *fakeFieldACL) AssertFieldVisible(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}

func (f *fakeFieldACL) AssertFieldsVisible(_ context.Context, _, _, _ string, _ bool, _ []string) error {
	return nil
}

func (f *fakeFieldACL) HiddenFields(_ context.Context, _, _, _ string, _ bool) (map[string]struct{}, error) {
	return nil, nil
}

// newImportSvcForTest builds a minimal ImportService with a fakeImportCatalog that
// exposes fields "name" (required text), "secret" (optional text), and "account_id"
// (optional relation → accounts). The returned fakeImportStore lets callers assert
// whether InsertRecords was invoked and control relation-FK existence answers.
func newImportSvcForTest() (*ImportService, *fakeImportStore) {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{
			ID:   "ent-1",
			Name: "contacts",
		},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "secret", FieldType: "text", IsRequired: false},
			{
				Name:       "account_id",
				FieldType:  "relation",
				IsRequired: false,
				Options:    strPtr(`{"relatedEntity":"accounts"}`),
			},
		},
	}
	store := &fakeImportStore{}
	svc := NewImportService(cat, store, nil)
	svc.WithRelationChecker(store)
	return svc, store
}

// --- A6 field-write ACL bypass regression tests --------------------------------

// TestImportFieldACL_Denied verifies that a CSV import whose column mapping includes a
// field the caller's role may not write is REJECTED before any insert (A6 bypass fix).
func TestImportFieldACL_Denied(t *testing.T) {
	svc, store := newImportSvcForTest()
	// Wire a field-ACL enforcer that forbids writes to "secret".
	svc.WithFieldACL(&fakeFieldACL{deniedField: "secret"})

	csv := "col_name,col_secret\nAlice,topsecret\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		RoleID:     "role-restricted",
		IsAdmin:    false,
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_secret": "secret"},
		Reader:     strings.NewReader(csv),
		FileSize:   int64(len(csv)),
	}

	_, err := svc.Import(context.Background(), in)
	if err == nil {
		t.Fatal("expected FIELD_FORBIDDEN error, got nil")
	}
	// The error must be a *ValidationError with code FIELD_FORBIDDEN.
	checkCode(t, err, "FIELD_FORBIDDEN")
	// InsertRecords must NOT have been called — the bypass must be caught before insert.
	if store.insertCalled {
		t.Error("InsertRecords was called despite FIELD_FORBIDDEN: the A6 bypass is still open")
	}
}

// TestImportFieldACL_AdminBypasses verifies that an admin import (IsAdmin=true) succeeds
// even when the same field-ACL enforcer would deny a non-admin.
func TestImportFieldACL_AdminBypasses(t *testing.T) {
	svc, store := newImportSvcForTest()
	svc.WithFieldACL(&fakeFieldACL{deniedField: "secret"})

	csv := "col_name,col_secret\nAlice,topsecret\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		RoleID:     "role-restricted",
		IsAdmin:    true, // admin bypasses field-ACL
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_secret": "secret"},
		Reader:     strings.NewReader(csv),
		FileSize:   int64(len(csv)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("admin import should succeed, got error: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("inserted: got %d, want 1", result.Inserted)
	}
	if !store.insertCalled {
		t.Error("InsertRecords was not called for a valid admin import")
	}
}

// TestImportFieldACL_AllowedRole verifies that a role that is not restricted on any mapped
// field succeeds (no false positives from the new guard).
func TestImportFieldACL_AllowedRole(t *testing.T) {
	svc, store := newImportSvcForTest()
	// Enforcer denies "secret" but the mapping only covers "name" — should be allowed.
	svc.WithFieldACL(&fakeFieldACL{deniedField: "secret"})

	csv := "col_name\nAlice\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		RoleID:     "role-restricted",
		IsAdmin:    false,
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name"},
		Reader:     strings.NewReader(csv),
		FileSize:   int64(len(csv)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("role with allowed mapping should succeed, got error: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("inserted: got %d, want 1", result.Inserted)
	}
	if !store.insertCalled {
		t.Error("InsertRecords was not called for a valid allowed-role import")
	}
}

// TestImportFieldACL_NilEnforcer verifies that when no field-ACL enforcer is wired
// (nil — unit-test default), import proceeds as before (nil-safe convention).
func TestImportFieldACL_NilEnforcer(t *testing.T) {
	svc, store := newImportSvcForTest()
	// No WithFieldACL call — svc.fieldACL is nil.

	csv := "col_name,col_secret\nAlice,topsecret\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		RoleID:     "role-anything",
		IsAdmin:    false,
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_secret": "secret"},
		Reader:     strings.NewReader(csv),
		FileSize:   int64(len(csv)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("nil enforcer should be a no-op, got error: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("inserted: got %d, want 1", result.Inserted)
	}
	if !store.insertCalled {
		t.Error("InsertRecords was not called with nil enforcer")
	}
}

// TestParseCSV verifies the CSV parser handles standard cases:
// header row + data rows, empty body, trailing newlines, and quoted fields.
func TestParseCSV(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantRows int
		wantCols []string
		wantErr  bool
	}{
		{
			name:     "two data rows",
			input:    "name,email\nAlice,alice@example.com\nBob,bob@example.com\n",
			wantRows: 2,
			wantCols: []string{"name", "email"},
		},
		{
			name:     "quoted fields",
			input:    "name,email\n\"Alice, Jr.\",alice@example.com\n",
			wantRows: 1,
			wantCols: []string{"name", "email"},
		},
		{
			name:    "empty body — no header",
			input:   "",
			wantErr: true,
		},
		{
			name:     "header only — zero data rows",
			input:    "name,email\n",
			wantRows: 0,
			wantCols: []string{"name", "email"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers, rows, err := parseCSV(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(rows) != tt.wantRows {
				t.Errorf("row count: got %d, want %d", len(rows), tt.wantRows)
			}
			for i, col := range tt.wantCols {
				if i >= len(headers) || headers[i] != col {
					t.Errorf("header[%d]: got %q, want %q", i, headers[i], col)
				}
			}
		})
	}
}

// TestBuildMapping verifies that buildMapping resolves valid mappings, rejects mappings
// that reference undeclared fields, and detects required fields absent from the mapping.
func TestBuildMapping(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "email", FieldType: "email", IsRequired: true},
		{Name: "age", FieldType: "number", IsRequired: false},
	}

	t.Run("valid mapping — all required covered", func(t *testing.T) {
		raw := map[string]string{"CSV_name": "name", "CSV_email": "email"}
		headers := []string{"CSV_name", "CSV_email"}
		_, err := buildMapping(headers, raw, specs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("mapping targets unknown field", func(t *testing.T) {
		raw := map[string]string{"CSV_name": "name", "CSV_email": "ghost_field"}
		headers := []string{"CSV_name", "CSV_email"}
		_, err := buildMapping(headers, raw, specs, nil)
		if err == nil {
			t.Fatal("expected error for unknown field target, got nil")
		}
		checkCode(t, err, "RECORD_UNKNOWN_FIELD")
	})

	t.Run("required field absent from mapping", func(t *testing.T) {
		raw := map[string]string{"CSV_name": "name"} // email is required but not mapped
		headers := []string{"CSV_name"}
		_, err := buildMapping(headers, raw, specs, nil)
		if err == nil {
			t.Fatal("expected error for missing required field, got nil")
		}
		checkCode(t, err, "IMPORT_REQUIRED_FIELD_NOT_MAPPED")
	})
}

// TestCoerceCell verifies per-type coercion of CSV string values to Go types
// that ValidateRecord expects (float64 for number, bool for boolean, etc.).
func TestCoerceCell(t *testing.T) {
	tests := []struct {
		name      string
		fieldType string
		cell      string
		wantVal   any
		wantErr   bool
	}{
		{name: "number ok", fieldType: "number", cell: "42.5", wantVal: float64(42.5)},
		{name: "number bad", fieldType: "number", cell: "abc", wantErr: true},
		{name: "boolean true", fieldType: "boolean", cell: "true", wantVal: true},
		{name: "boolean false", fieldType: "boolean", cell: "false", wantVal: false},
		{name: "boolean bad", fieldType: "boolean", cell: "maybe", wantErr: true},
		{name: "text passthrough", fieldType: "text", cell: "hello", wantVal: "hello"},
		{name: "email passthrough", fieldType: "email", cell: "a@b.com", wantVal: "a@b.com"},
		{name: "date passthrough", fieldType: "date", cell: "2026-01-01", wantVal: "2026-01-01"},
		{name: "picklist passthrough", fieldType: "picklist", cell: "active", wantVal: "active"},
		{name: "empty cell is nil", fieldType: "text", cell: "", wantVal: nil},
		{
			name:      "rating integer",
			fieldType: "rating",
			cell:      "3",
			wantVal:   float64(3),
		},
		{
			name:      "multi_select JSON array",
			fieldType: "multi_select",
			cell:      `["a","b"]`,
			wantVal:   []any{"a", "b"},
		},
		{
			name:      "multi_select bad JSON",
			fieldType: "multi_select",
			cell:      `["a",notjson`, // a JSON array that does not parse (a bare list is now read as choices — specs/061 FR-002)
			wantErr:   true,
		},
		{
			name:      "currency JSON object",
			fieldType: "currency",
			cell:      `{"amount":100,"currencyCode":"VND"}`,
			wantVal:   map[string]any{"amount": float64(100), "currencyCode": "VND"},
		},
		{
			name:      "currency bad JSON",
			fieldType: "currency",
			cell:      `notjson`,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := FieldSpec{Name: "f", FieldType: tt.fieldType}
			got, err := coerceCell(spec, tt.cell)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (val=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// For nil expected value, ensure we get nil back.
			if tt.wantVal == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			// Basic equality for primitives; for slices/maps do string comparison.
			switch want := tt.wantVal.(type) {
			case float64:
				f, ok := got.(float64)
				if !ok || f != want {
					t.Errorf("got %v (%T), want %v", got, got, want)
				}
			case bool:
				b, ok := got.(bool)
				if !ok || b != want {
					t.Errorf("got %v, want %v", got, want)
				}
			case string:
				s, ok := got.(string)
				if !ok || s != want {
					t.Errorf("got %v, want %v", got, want)
				}
			default:
				// For complex types we just check non-nil and type match.
				if got == nil {
					t.Errorf("got nil, want %v", want)
				}
			}
		})
	}
}

// TestMapRow verifies that a CSV row is correctly mapped and coerced into a payload
// map using the resolved columnMapping, skipping unmapped columns.
func TestMapRow(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text"},
		{Name: "age", FieldType: "number"},
	}
	// Only "name" is mapped; "extra" CSV col is ignored.
	mapping := columnMapping{
		"col_name": specs[0],
		"col_age":  specs[1],
	}
	headers := []string{"col_name", "col_age", "unmapped_col"}
	row := []string{"Alice", "30", "ignored"}

	payload, err := mapRow(headers, row, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["name"] != "Alice" {
		t.Errorf("name: got %v, want Alice", payload["name"])
	}
	if payload["age"] != float64(30) {
		t.Errorf("age: got %v, want 30", payload["age"])
	}
	if _, ok := payload["unmapped_col"]; ok {
		t.Error("unmapped column should not appear in payload")
	}
}

// TestProcessRows_PartialSuccess verifies that valid rows are collected and invalid
// rows are captured in errors — without calling any DB. This is the pure logic test.
func TestProcessRows_PartialSuccess(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "email", FieldType: "email", IsRequired: true},
	}
	mapping := columnMapping{
		"name":  specs[0],
		"email": specs[1],
	}
	headers := []string{"name", "email"}
	rows := [][]string{
		{"Alice", "alice@example.com"}, // valid
		{"", "notanemail"},             // invalid: required name missing, bad email
		{"Bob", "bob@example.com"},     // valid
	}

	valid, _, errs := processRows(headers, rows, mapping, specs)
	if len(valid) != 2 {
		t.Errorf("valid rows: got %d, want 2", len(valid))
	}
	if len(errs) == 0 {
		t.Error("expected at least 1 import error for the bad row, got 0")
	}
	// Verify original row numbers are preserved in the valid set.
	// Row 2 (index 1) is invalid, so valid[0].rowNum==1, valid[1].rowNum==3.
	if valid[0].rowNum != 1 {
		t.Errorf("valid[0].rowNum: got %d, want 1", valid[0].rowNum)
	}
	if valid[1].rowNum != 3 {
		t.Errorf("valid[1].rowNum: got %d, want 3", valid[1].rowNum)
	}
}

// TestProcessRows_CurrencyCodeMismatch (queue #7, Q7) — a CSV currency cell whose code differs
// from the field's configured options.currencyCode is REJECTED with RECORD_CURRENCY_MISMATCH,
// the row error names the field (the FE error table renders err.field), and the matching row
// still imports with its code untouched. Never rewrite: relabelling USD 100 as VND 100 is the
// exact wrong total this guard exists to prevent.
func TestProcessRows_CurrencyCodeMismatch(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "price", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
	}
	mapping := columnMapping{"name": specs[0], "price": specs[1]}
	headers := []string{"name", "price"}
	rows := [][]string{
		{"Alice", `{"amount":100,"currencyCode":"USD"}`}, // row 1: wrong code → rejected
		{"Bob", `{"amount":200,"currencyCode":"VND"}`},   // row 2: matches → imported
	}

	valid, _, errs := processRows(headers, rows, mapping, specs)
	if len(valid) != 1 || valid[0].rowNum != 2 {
		t.Fatalf("valid rows = %+v; want exactly CSV row 2", valid)
	}
	got, _ := valid[0].payload["price"].(map[string]any)
	if got["currencyCode"] != "VND" || got["amount"] != float64(200) {
		t.Fatalf("surviving row payload = %v; want VND 200 stored verbatim", got)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %+v; want exactly 1", errs)
	}
	e := errs[0]
	if e.Row != 1 || e.Code != "RECORD_CURRENCY_MISMATCH" || e.Field != "price" {
		t.Fatalf("row error = %+v; want {Row:1 Field:price Code:RECORD_CURRENCY_MISMATCH}", e)
	}
}

// TestImport_CurrencyCodeMismatch_RejectsRowNeverRewrites runs the whole ImportService path with
// the field's options coming from the sys_fields JSON string (toFieldSpecs), as in production:
// total 2 · inserted 1 · failed 1, the error names the field, and the store received only the
// VND row — still labelled VND, not a "normalised" USD row.
func TestImport_CurrencyCodeMismatch_RejectsRowNeverRewrites(t *testing.T) {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", Name: "products"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "list_price", FieldType: "currency", Options: strPtr(`{"currencyCode":"VND"}`)},
		},
	}
	store := &fakeImportStore{}
	svc := NewImportService(cat, store, nil)

	csv := "col_name,col_price\n" +
		`Alice,"{""amount"":100,""currencyCode"":""USD""}"` + "\n" +
		`Bob,"{""amount"":200,""currencyCode"":""VND""}"` + "\n"
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "products",
		Mapping:    map[string]string{"col_name": "name", "col_price": "list_price"},
		Reader:     strings.NewReader(csv),
		FileSize:   int64(len(csv)),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Total != 2 || res.Inserted != 1 || res.Failed != 1 {
		t.Fatalf("result = total %d inserted %d failed %d; want 2/1/1", res.Total, res.Inserted, res.Failed)
	}
	if len(res.Errors) != 1 || res.Errors[0].Row != 1 || res.Errors[0].Code != "RECORD_CURRENCY_MISMATCH" || res.Errors[0].Field != "list_price" {
		t.Fatalf("errors = %+v; want one {Row:1 Field:list_price Code:RECORD_CURRENCY_MISMATCH}", res.Errors)
	}
	if len(store.insertedRows) != 1 {
		t.Fatalf("store received %d rows; want 1 (the VND row only)", len(store.insertedRows))
	}
	stored, _ := store.insertedRows[0]["list_price"].(map[string]any)
	if stored["currencyCode"] != "VND" || stored["amount"] != float64(200) {
		t.Fatalf("stored row = %v; want VND 200 verbatim", stored)
	}
}

// TestDedupFilter verifies that dedup removes rows whose dedupField value already
// exists in the "existing" set, and reports them as RECORD_DUPLICATE errors with the
// original CSV row number (not the slice index).
func TestDedupFilter(t *testing.T) {
	existing := map[string]struct{}{"alice@example.com": {}}
	dedupField := "email"

	// Simulate: CSV row 1 was invalid (validation-fail), so valid rows start at rowNum 2.
	rows := []validRow{
		{rowNum: 2, payload: map[string]any{"name": "Alice", "email": "alice@example.com"}}, // DB dup
		{rowNum: 3, payload: map[string]any{"name": "Bob", "email": "bob@example.com"}},     // new
	}

	kept, errs := applyDedup(rows, dedupField, existing)
	if len(kept) != 1 {
		t.Errorf("kept: got %d, want 1", len(kept))
	}
	if len(errs) != 1 {
		t.Errorf("errs: got %d, want 1", len(errs))
	}
	if errs[0].Code != "RECORD_DUPLICATE" {
		t.Errorf("error code: got %q, want RECORD_DUPLICATE", errs[0].Code)
	}
	// Crucially: error row number must be the original CSV row number (2), not slice index (1).
	if errs[0].Row != 2 {
		t.Errorf("error row: got %d, want 2 (original CSV line)", errs[0].Row)
	}
}

// TestInFileDedup verifies that duplicate values WITHIN the CSV file itself
// (before any DB check) are also deduplicated correctly, and that the error row number
// reflects the original CSV position even when earlier rows failed validation.
func TestInFileDedup(t *testing.T) {
	// Simulate: CSV row 1 failed validation; valid rows are 2, 3, 4.
	rows := []validRow{
		{rowNum: 2, payload: map[string]any{"name": "Alice", "email": "same@example.com"}},
		{rowNum: 3, payload: map[string]any{"name": "Alice2", "email": "same@example.com"}}, // in-file dup
		{rowNum: 4, payload: map[string]any{"name": "Bob", "email": "bob@example.com"}},
	}

	existing := map[string]struct{}{} // nothing in DB
	kept, errs := applyDedup(rows, "email", existing)
	if len(kept) != 2 {
		t.Errorf("kept: got %d, want 2", len(kept))
	}
	if len(errs) != 1 {
		t.Errorf("errs: got %d, want 1 (in-file dup)", len(errs))
	}
	// Row 3 (CSV original line) is the duplicate — not index 2.
	if errs[0].Row != 3 {
		t.Errorf("dedup error row: got %d, want 3 (original CSV line)", errs[0].Row)
	}
}

// checkCode asserts that err is a *ValidationError with the given code.
func checkCode(t *testing.T, err error, code string) {
	t.Helper()
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Code != code {
		t.Errorf("error code: got %q, want %q", ve.Code, code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// B3 regression: relation-field FK validation on the import path (close B3 gap)
// ─────────────────────────────────────────────────────────────────────────────

// TestImportRelationFK_InvalidRef verifies that a CSV row whose relation field
// references a non-existent same-tenant id is rejected as RECORD_RELATION_INVALID
// (captured in ImportResult.Errors, not an overall Import() error) while a row with
// a valid FK still inserts. This closes the B3 dangling-FK gap.
func TestImportRelationFK_InvalidRef(t *testing.T) {
	svc, store := newImportSvcForTest()

	// Only "accounts:good-acct" exists; "accounts:bad-acct" does not.
	store.existingRelIDs = map[string]bool{
		"accounts:good-acct": true,
	}

	// CSV: two rows — one valid FK, one dangling FK.
	csvData := "col_name,col_account_id\nAlice,good-acct\nBob,bad-acct\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_account_id": "account_id"},
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("Import returned unexpected error: %v", err)
	}

	// Row with good FK must insert; row with bad FK must appear in Errors.
	if result.Inserted != 1 {
		t.Errorf("Inserted: got %d, want 1 (only valid-FK row should insert)", result.Inserted)
	}
	if len(result.Errors) == 0 {
		t.Fatal("Errors: got 0, want 1 (dangling-FK row must be captured)")
	}
	got := result.Errors[0]
	if got.Code != "RECORD_RELATION_INVALID" {
		t.Errorf("Errors[0].Code = %q; want RECORD_RELATION_INVALID", got.Code)
	}
	// The error must point to the original CSV row number (2 = Bob's row).
	if got.Row != 2 {
		t.Errorf("Errors[0].Row = %d; want 2 (Bob's CSV row)", got.Row)
	}
}

// TestImportRelationFK_NilChecker verifies that when no RelationBatchChecker is wired
// (nil — backward-compat unit-test default), import skips FK validation and inserts all
// structurally valid rows. This preserves the pre-fix nil-safe convention.
func TestImportRelationFK_NilChecker(t *testing.T) {
	svc, _ := newImportSvcForTest()
	// Override: remove the checker wired by newImportSvcForTest to test nil path.
	svc.relChecker = nil

	csvData := "col_name,col_account_id\nAlice,nonexistent-id\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_account_id": "account_id"},
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("nil relChecker should be a no-op, got error: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("Inserted: got %d, want 1 (nil checker = no FK validation = row inserted)", result.Inserted)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors: got %v, want none with nil checker", result.Errors)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// B3-perf: batched dedup existence check — close the "one query per value" gap
// ─────────────────────────────────────────────────────────────────────────────

// newImportSvcWithEmail builds an ImportService whose "contacts" entity has an
// extra unique "email" field (IsUnique=true), used by the dedup perf tests.
func newImportSvcWithEmail() (*ImportService, *fakeImportStore) {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{
			ID:   "ent-email",
			Name: "contacts",
		},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "email", FieldType: "email", IsRequired: false, IsUnique: true},
		},
	}
	store := &fakeImportStore{}
	svc := NewImportService(cat, store, nil)
	svc.WithRelationChecker(store)
	return svc, store
}

// TestDedup_BatchQueryCount verifies that importing N rows with distinct dedup values
// issues exactly ONE call to BatchExistingValuesForField (not one per value).
// This is the O(1)-queries-per-field assertion that proves the perf fix.
func TestDedup_BatchQueryCount(t *testing.T) {
	svc, store := newImportSvcWithEmail()
	// No values exist in DB — every row will be inserted.
	store.existingDedupVals = map[string]struct{}{}

	// Five rows with distinct email values.
	csvData := "col_name,col_email\n" +
		"Alice,a@x.com\n" +
		"Bob,b@x.com\n" +
		"Carol,c@x.com\n" +
		"Dave,d@x.com\n" +
		"Eve,e@x.com\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_email": "email"},
		DedupField: "email",
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inserted != 5 {
		t.Errorf("Inserted: got %d, want 5", result.Inserted)
	}
	// KEY assertion: exactly ONE call to BatchExistingValuesForField regardless of row count.
	if store.batchDedupCalls != 1 {
		t.Errorf("BatchExistingValuesForField calls: got %d, want 1 (O(1) per dedup field, not O(N) per value)", store.batchDedupCalls)
	}
}

// TestDedup_DetectsDBDuplicates verifies that rows whose dedup-field value is already in
// the DB are rejected as RECORD_DUPLICATE, while new values are inserted — identical
// outcome to the old per-value path (behavior preserved, only query count differs).
func TestDedup_DetectsDBDuplicates(t *testing.T) {
	svc, store := newImportSvcWithEmail()
	// "alice@x.com" already exists in DB; "bob@x.com" is new.
	store.existingDedupVals = map[string]struct{}{
		"alice@x.com": {},
	}

	csvData := "col_name,col_email\nAlice,alice@x.com\nBob,bob@x.com\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_email": "email"},
		DedupField: "email",
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only Bob inserts; Alice is a DB duplicate.
	if result.Inserted != 1 {
		t.Errorf("Inserted: got %d, want 1", result.Inserted)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors: got %d, want 1", len(result.Errors))
	}
	if result.Errors[0].Code != "RECORD_DUPLICATE" {
		t.Errorf("Errors[0].Code = %q; want RECORD_DUPLICATE", result.Errors[0].Code)
	}
	if result.Errors[0].Row != 1 {
		t.Errorf("Errors[0].Row = %d; want 1 (Alice's CSV row)", result.Errors[0].Row)
	}
	// Exactly one batched call, not two.
	if store.batchDedupCalls != 1 {
		t.Errorf("BatchExistingValuesForField calls: got %d, want 1", store.batchDedupCalls)
	}
}

// TestDedup_NoDedupField verifies that when DedupField is empty, BatchExistingValuesForField
// is never called (zero overhead for imports that skip dedup).
func TestDedup_NoDedupField(t *testing.T) {
	svc, store := newImportSvcWithEmail()

	csvData := "col_name\nAlice\nBob\n"
	in := ImportInput{
		TenantID:   "t1",
		UserID:     "u1",
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name"},
		DedupField: "", // no dedup
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inserted != 2 {
		t.Errorf("Inserted: got %d, want 2", result.Inserted)
	}
	if store.batchDedupCalls != 0 {
		t.Errorf("BatchExistingValuesForField calls: got %d, want 0 (no dedup field)", store.batchDedupCalls)
	}
}
