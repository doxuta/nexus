package services

// ENG #6 auto_number — record-time behaviour: readonly-from-client (mirrors
// computed_readonly_test.go's lookup/rollup coverage exactly), formatAutoNumber rendering, and
// assignSequences (the Create-only seam that claims + writes the value).

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func autoNumberFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "customer", FieldType: "text", IsRequired: true},
		{Name: "invoice_no", FieldType: "auto_number", Options: map[string]any{"prefix": "INV-", "padding": float64(5)}},
	}
}

// --- POST (ValidateRecord) ---

func TestValidateRecord_AutoNumberFieldInPayloadIsRejected(t *testing.T) {
	specs := autoNumberFieldSpecs()
	payload := map[string]any{
		"customer":   "Acme",
		"invoice_no": "INV-99999", // client trying to set a server-assigned field
	}
	_, err := ValidateRecord(specs, payload)
	if codeOf(err) != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestValidateRecord_AutoNumberAbsentIsOK(t *testing.T) {
	specs := autoNumberFieldSpecs()
	payload := map[string]any{"customer": "Acme"}
	clean, err := ValidateRecord(specs, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := clean["invoice_no"]; present {
		t.Errorf("auto_number field must not be in clean map from ValidateRecord (assigned later by assignSequences)")
	}
}

// --- PUT (ValidatePartialRecord) ---

func TestValidatePartialRecord_AutoNumberFieldInPayloadIsRejected(t *testing.T) {
	specs := autoNumberFieldSpecs()
	payload := map[string]any{"invoice_no": "INV-99999"}
	_, err := ValidatePartialRecord(specs, payload)
	if codeOf(err) != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_READONLY (err=%v)", codeOf(err), err)
	}
}

// --- CSV import parity (record_import.go buildMapping) ---

func TestBuildMapping_AutoNumberFieldRejected(t *testing.T) {
	specs := autoNumberFieldSpecs()
	_, err := buildMapping([]string{"Invoice"}, map[string]string{"Invoice": "invoice_no"}, specs, nil)
	if codeOf(err) != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_READONLY (err=%v)", codeOf(err), err)
	}
}

// --- formatAutoNumber ---

func TestFormatAutoNumber(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
		n       int64
		want    string
	}{
		{"no options", nil, 42, "42"},
		{"prefix only", map[string]any{"prefix": "INV-"}, 42, "INV-42"},
		{"padding only", map[string]any{"padding": float64(5)}, 42, "00042"},
		{"prefix and padding", map[string]any{"prefix": "INV-", "padding": float64(5)}, 42, "INV-00042"},
		{"padding zero is no-op", map[string]any{"padding": float64(0)}, 42, "42"},
		{"value wider than padding is never truncated", map[string]any{"padding": float64(3)}, 123456, "123456"},
		{"value exactly at padding width", map[string]any{"padding": float64(3)}, 123, "123"},
		{"first claim value one", map[string]any{"prefix": "INV-", "padding": float64(5)}, 1, "INV-00001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAutoNumber(tc.options, tc.n)
			if got != tc.want {
				t.Errorf("formatAutoNumber(%v, %d) = %q; want %q", tc.options, tc.n, got, tc.want)
			}
		})
	}
}

// --- assignSequences ---

// fakeSequenceClaimer is a scriptable SequenceClaimer: claims returns sequential integers
// starting at 1 per DISTINCT (tenantID, entityName, fieldName) key, unless err is set (then every
// claim fails). Records every call so tests can assert claimer usage (or non-usage).
type fakeSequenceClaimer struct {
	err      error
	calls    int
	lastArgs []string // tenantID, entityName, fieldName of the most recent call
	next     map[string]int64
}

func (f *fakeSequenceClaimer) ClaimSequence(_ context.Context, tenantID, entityName, fieldName string) (int64, error) {
	f.calls++
	f.lastArgs = []string{tenantID, entityName, fieldName}
	if f.err != nil {
		return 0, f.err
	}
	if f.next == nil {
		f.next = make(map[string]int64)
	}
	key := tenantID + "|" + entityName + "|" + fieldName
	f.next[key]++
	return f.next[key], nil
}

func TestAssignSequences_ClaimsAndFormatsIntoClean(t *testing.T) {
	svc := &RecordService{sequences: &fakeSequenceClaimer{}}
	specs := autoNumberFieldSpecs()
	clean := map[string]any{"customer": "Acme"}

	if err := svc.assignSequences(context.Background(), "tenant-1", "invoices", specs, clean); err != nil {
		t.Fatalf("assignSequences: %v", err)
	}
	if got := clean["invoice_no"]; got != "INV-00001" {
		t.Fatalf("clean[invoice_no] = %v; want INV-00001", got)
	}
	if got := clean["customer"]; got != "Acme" {
		t.Errorf("assignSequences must not disturb non-auto_number fields; customer = %v", got)
	}
}

func TestAssignSequences_ClaimsCorrectTenantEntityField(t *testing.T) {
	claimer := &fakeSequenceClaimer{}
	svc := &RecordService{sequences: claimer}
	specs := autoNumberFieldSpecs()
	clean := map[string]any{}

	if err := svc.assignSequences(context.Background(), "tenant-xyz", "invoices", specs, clean); err != nil {
		t.Fatalf("assignSequences: %v", err)
	}
	want := []string{"tenant-xyz", "invoices", "invoice_no"}
	if len(claimer.lastArgs) != 3 || claimer.lastArgs[0] != want[0] || claimer.lastArgs[1] != want[1] || claimer.lastArgs[2] != want[2] {
		t.Errorf("ClaimSequence called with %v; want %v", claimer.lastArgs, want)
	}
}

func TestAssignSequences_NoAutoNumberFieldsIsNoOp(t *testing.T) {
	claimer := &fakeSequenceClaimer{}
	svc := &RecordService{sequences: claimer}
	specs := []FieldSpec{{Name: "name", FieldType: "text"}}
	clean := map[string]any{"name": "Acme"}

	if err := svc.assignSequences(context.Background(), "tenant-1", "accounts", specs, clean); err != nil {
		t.Fatalf("assignSequences: %v", err)
	}
	if claimer.calls != 0 {
		t.Errorf("claimer must never be called when the entity has no auto_number field; calls=%d", claimer.calls)
	}
}

func TestAssignSequences_ClaimErrorPropagates(t *testing.T) {
	svc := &RecordService{sequences: &fakeSequenceClaimer{err: errors.New("db down")}}
	specs := autoNumberFieldSpecs()
	clean := map[string]any{}

	err := svc.assignSequences(context.Background(), "tenant-1", "invoices", specs, clean)
	if err == nil {
		t.Fatal("expected error when ClaimSequence fails")
	}
	if _, present := clean["invoice_no"]; present {
		t.Errorf("clean must not carry a value when the claim failed")
	}
}

func TestAssignSequences_NilClaimerFailsClosedWhenNeeded(t *testing.T) {
	svc := &RecordService{sequences: nil}
	specs := autoNumberFieldSpecs()
	clean := map[string]any{}

	err := svc.assignSequences(context.Background(), "tenant-1", "invoices", specs, clean)
	if codeOf(err) != "RECORD_AUTONUMBER_UNAVAILABLE" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_UNAVAILABLE (err=%v)", codeOf(err), err)
	}
}

func TestAssignSequences_NilClaimerNoOpWithoutAutoNumberField(t *testing.T) {
	svc := &RecordService{sequences: nil}
	specs := []FieldSpec{{Name: "name", FieldType: "text"}}
	clean := map[string]any{"name": "Acme"}

	if err := svc.assignSequences(context.Background(), "tenant-1", "accounts", specs, clean); err != nil {
		t.Fatalf("nil claimer must be harmless when no auto_number field exists: %v", err)
	}
}

// --- Full Create()/Update() round trip via the public RecordService surface ---

// autoNumberCatalog resolves a fixed entity+fields — enough for Create/Update's resolve() step.
type autoNumberCatalog struct {
	entity *metadata.EntityDefinition
	fields []metadata.FieldDefinition
}

func (c *autoNumberCatalog) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	return c.entity, nil
}
func (c *autoNumberCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return c.fields, nil
}

// capturingRecordStore embeds fakeRecordStore and records the data map passed to
// InsertRecord/UpdateRecord so a test can assert the persisted auto_number value.
type capturingRecordStore struct {
	fakeRecordStore
	insertedData []map[string]any
	updatedData  []map[string]any
	existing     *dynrepo.Record // returned by GetRecord (Update's before-image read)
}

func (s *capturingRecordStore) InsertRecord(_ context.Context, _, _, _ string, data map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	s.insertedData = append(s.insertedData, cloneData(data))
	return "rec-" + string(rune('0'+len(s.insertedData))), nil
}
func (s *capturingRecordStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return s.existing, nil
}
func (s *capturingRecordStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.updatedData = append(s.updatedData, cloneData(data))
	return nil
}

func autoNumberEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-invoices", TenantID: "tenant-1", Name: "invoices", DataTable: "obj_invoices"}
}

func autoNumberFieldDefinitions(t *testing.T) []metadata.FieldDefinition {
	t.Helper()
	return []metadata.FieldDefinition{
		{Name: "customer", FieldType: "text", IsRequired: true},
		{Name: "invoice_no", FieldType: "auto_number", Options: jsonOptsPtr(t, map[string]any{"prefix": "INV-", "padding": float64(5)})},
		{Name: "amount", FieldType: "number"},
	}
}

func TestRecordService_Create_AssignsSequentialAutoNumbers(t *testing.T) {
	catalog := &autoNumberCatalog{entity: autoNumberEntityFixture(), fields: autoNumberFieldDefinitions(t)}
	store := &capturingRecordStore{}
	claimer := &fakeSequenceClaimer{}
	svc := NewRecordService(catalog, store).WithSequenceClaimer(claimer)

	admin := RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
	if _, err := svc.Create(context.Background(), admin, "tenant-1", "u1", "invoices", map[string]any{"customer": "Acme"}); err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	if _, err := svc.Create(context.Background(), admin, "tenant-1", "u1", "invoices", map[string]any{"customer": "Beta"}); err != nil {
		t.Fatalf("Create #2: %v", err)
	}

	if len(store.insertedData) != 2 {
		t.Fatalf("expected 2 inserted records, got %d", len(store.insertedData))
	}
	if got := store.insertedData[0]["invoice_no"]; got != "INV-00001" {
		t.Errorf("first record invoice_no = %v; want INV-00001", got)
	}
	if got := store.insertedData[1]["invoice_no"]; got != "INV-00002" {
		t.Errorf("second record invoice_no = %v; want INV-00002 (sequential increment)", got)
	}
}

func TestRecordService_Create_ClientSuppliedAutoNumberRejectedBeforeClaim(t *testing.T) {
	catalog := &autoNumberCatalog{entity: autoNumberEntityFixture(), fields: autoNumberFieldDefinitions(t)}
	store := &capturingRecordStore{}
	claimer := &fakeSequenceClaimer{}
	svc := NewRecordService(catalog, store).WithSequenceClaimer(claimer)

	admin := RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
	_, err := svc.Create(context.Background(), admin, "tenant-1", "u1", "invoices",
		map[string]any{"customer": "Acme", "invoice_no": "INV-99999"})
	if codeOf(err) != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_READONLY (err=%v)", codeOf(err), err)
	}
	if claimer.calls != 0 {
		t.Errorf("a rejected payload must never reach assignSequences/ClaimSequence; calls=%d", claimer.calls)
	}
	if len(store.insertedData) != 0 {
		t.Errorf("a rejected payload must never reach InsertRecord")
	}
}

func TestRecordService_Update_DoesNotReassignAutoNumber(t *testing.T) {
	catalog := &autoNumberCatalog{entity: autoNumberEntityFixture(), fields: autoNumberFieldDefinitions(t)}
	claimer := &fakeSequenceClaimer{}
	store := &capturingRecordStore{}
	svc := NewRecordService(catalog, store).WithSequenceClaimer(claimer)

	admin := RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
	id, err := svc.Create(context.Background(), admin, "tenant-1", "u1", "invoices", map[string]any{"customer": "Acme"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if claimer.calls != 1 {
		t.Fatalf("Create should claim exactly once; calls=%d", claimer.calls)
	}
	// Simulate the stored before-image (what GetRecord would return) so Update's formula-merge
	// step has something to read.
	store.existing = &dynrepo.Record{ID: id, Data: map[string]any{"customer": "Acme", "invoice_no": "INV-00001", "amount": float64(0)}}

	if err := svc.Update(context.Background(), admin, "tenant-1", "invoices", id, map[string]any{"amount": float64(500)}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if claimer.calls != 1 {
		t.Errorf("Update must NEVER call ClaimSequence again; calls=%d (want 1, unchanged since Create)", claimer.calls)
	}
	if len(store.updatedData) != 1 {
		t.Fatalf("expected 1 UpdateRecord call, got %d", len(store.updatedData))
	}
	if _, present := store.updatedData[0]["invoice_no"]; present {
		t.Errorf("Update's persisted patch must not touch invoice_no at all; got %v", store.updatedData[0])
	}
}

func TestRecordService_Update_ClientSuppliedAutoNumberRejected(t *testing.T) {
	catalog := &autoNumberCatalog{entity: autoNumberEntityFixture(), fields: autoNumberFieldDefinitions(t)}
	store := &capturingRecordStore{existing: &dynrepo.Record{ID: "rec-1", Data: map[string]any{"customer": "Acme", "invoice_no": "INV-00001"}}}
	claimer := &fakeSequenceClaimer{}
	svc := NewRecordService(catalog, store).WithSequenceClaimer(claimer)

	admin := RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
	err := svc.Update(context.Background(), admin, "tenant-1", "invoices", "rec-1", map[string]any{"invoice_no": "INV-99999"}, nil)
	if codeOf(err) != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("code = %q; want RECORD_AUTONUMBER_READONLY (err=%v)", codeOf(err), err)
	}
}
