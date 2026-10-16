package services

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeRecordCatalog stubs the EntityCatalog the RecordService resolves an entity and its
// fields from. List uses the field specs to validate ?field[op]=value filters and to pick
// the columns ?q searches.
type fakeRecordCatalog struct {
	entity    *metadata.EntityDefinition
	entityErr error
	fields    []metadata.FieldDefinition
}

func (f *fakeRecordCatalog) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.entity, nil
}

func (f *fakeRecordCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return f.fields, nil
}

// fakeRecordStore captures the ListParams and RowScope the service hands the repo, so tests can
// assert the resolved (validated, sanitized) filters/search and the A5 row scope without a database.
type fakeRecordStore struct {
	gotParams dynrepo.ListParams
	gotScope  dynrepo.RowScope
	// gotTenantID is the tenant the service hands the repo on List — captured so a test can assert
	// a new predicate did not cost the query its mandatory tenant scoping (OWNER-DIMENSION).
	gotTenantID  string
	gotAggParams dynrepo.AggregateParams
	// cursorErr, when set, is returned by ListRecordsCursor — lets an export test prove what the
	// service has (not) written to its io.Writer before a first-page store failure.
	cursorErr error
	// cursorRecords is the single page ListRecordsCursor serves when cursorErr is nil.
	cursorRecords []dynrepo.Record
	// listRecords is the page ListRecords serves (offset mode) — lets a report-run test feed rows
	// through the whole List pipeline (strip → lookups → expand) without a database.
	listRecords []dynrepo.Record
	// cursorCalls / gotAfter record whether (and with which After) the keyset path ran —
	// CURSOR-BOOTSTRAP #42 asserts a ?paginate=cursor first page reaches it with After=="".
	cursorCalls int
	gotAfter    string
}

func (f *fakeRecordStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	return "", nil
}
func (f *fakeRecordStore) GetRecord(_ context.Context, _, _, _ string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	f.gotScope = scope
	return nil, nil
}
func (f *fakeRecordStore) ListRecords(_ context.Context, _, tenantID string, scope dynrepo.RowScope, p dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	f.gotParams = p
	f.gotScope = scope
	f.gotTenantID = tenantID
	return append([]dynrepo.Record{}, f.listRecords...), len(f.listRecords), nil
}
func (f *fakeRecordStore) ListRecordsCursor(_ context.Context, _, _ string, scope dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	// Capture filters/search into gotParams so cursor-mode tests can inspect them too.
	f.cursorCalls++
	f.gotAfter = p.After
	f.gotScope = scope
	f.gotParams = dynrepo.ListParams{
		Filters: p.Filters,
		Search:  p.Search,
		Sort:    p.Sort,
		Order:   p.Order,
		Limit:   p.Limit,
	}
	if f.cursorErr != nil {
		return nil, "", f.cursorErr
	}
	return f.cursorRecords, "", nil
}
func (f *fakeRecordStore) UpdateRecord(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.gotScope = scope
	return nil
}
func (f *fakeRecordStore) SoftDeleteRecord(_ context.Context, _, _, _ string, scope dynrepo.RowScope) error {
	f.gotScope = scope
	return nil
}
func (f *fakeRecordStore) RestoreRecord(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	f.gotScope = scope
	return nil
}
func (f *fakeRecordStore) RecordExists(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}
func (f *fakeRecordStore) BatchRecordExists(_ context.Context, _, _ string, ids []string) (map[string]bool, error) {
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func (f *fakeRecordStore) AggregateScalar(_ context.Context, _, _ string, scope dynrepo.RowScope, p dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	f.gotScope = scope
	f.gotAggParams = p
	return dynrepo.AggregateScalar{}, nil
}
func (f *fakeRecordStore) AggregateGrouped(_ context.Context, _, _ string, scope dynrepo.RowScope, p dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	f.gotScope = scope
	f.gotAggParams = p
	return nil, nil
}
func (f *fakeRecordStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}
func (f *fakeRecordStore) SetOwner(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, _ string) error {
	f.gotScope = scope
	return nil
}

func courseEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-1", TenantID: "tenant-1", Name: "courses", DataTable: "obj_courses"}
}

func courseFieldDefs() []metadata.FieldDefinition {
	return []metadata.FieldDefinition{
		{Name: "title", FieldType: "text"},
		{Name: "seats", FieldType: "number"},
		{Name: "level", FieldType: "picklist"},
	}
}

func newListService() (*RecordService, *fakeRecordStore) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: courseFieldDefs()}
	return NewRecordService(cat, store), store
}

// --- RM-08: filter operators ---

// A bare ?field=value (no operator bracket) resolves to eq — backward-compatible.
func TestList_FieldFilterEqDefault(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: "", Values: []string{"Go 101"}}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Fatalf("Filters len = %d; want 1", len(store.gotParams.Filters))
	}
	got := store.gotParams.Filters[0]
	if got.Column != "title" {
		t.Errorf("Column = %q; want title", got.Column)
	}
	if got.Operator != dynrepo.FilterEq {
		t.Errorf("Operator = %q; want eq", got.Operator)
	}
	// text field → case-insensitive
	if !got.CaseInsensitive {
		t.Errorf("CaseInsensitive = false; want true for text field eq")
	}
}

// gt/gte/lt/lte on a number field → CastNumeric=true, no case-insensitive.
func TestList_NumericOrderedOperator(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "seats", Operator: dynrepo.FilterGte, Values: []string{"10"}}},
	})
	if err != nil {
		t.Fatalf("List gte: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Fatalf("Filters len = %d; want 1", len(store.gotParams.Filters))
	}
	f := store.gotParams.Filters[0]
	if f.Operator != dynrepo.FilterGte {
		t.Errorf("Operator = %q; want gte", f.Operator)
	}
	if !f.CastNumeric {
		t.Errorf("CastNumeric = false; want true for number field")
	}
	if f.CaseInsensitive {
		t.Errorf("CaseInsensitive = true; want false for numeric comparison")
	}
}

// gt on a text field must be rejected (operator-vs-type mismatch → 422 FILTER_OPERATOR_NOT_ALLOWED).
func TestList_OrderedOperatorRejectedForText(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: dynrepo.FilterGt, Values: []string{"foo"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want FILTER_OPERATOR_NOT_ALLOWED", got, err)
	}
}

// between on a number field with 2 values → valid.
func TestList_BetweenNumberValid(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "seats", Operator: dynrepo.FilterBetween, Values: []string{"10", "50"}}},
	})
	if err != nil {
		t.Fatalf("List between: %v", err)
	}
	f := store.gotParams.Filters[0]
	if f.Operator != dynrepo.FilterBetween {
		t.Errorf("Operator = %q; want between", f.Operator)
	}
	if !f.CastNumeric {
		t.Errorf("CastNumeric = false; want true")
	}
}

// between on a number field with 1 value → 422 FILTER_INVALID_ARITY.
func TestList_BetweenArityError(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "seats", Operator: dynrepo.FilterBetween, Values: []string{"10"}}},
	})
	if got := codeOf(err); got != "FILTER_INVALID_ARITY" {
		t.Fatalf("code = %q (err=%v); want FILTER_INVALID_ARITY", got, err)
	}
}

// in on a picklist field → valid, case-insensitive (RM-14: picklist is in caseInsensitiveTypes).
func TestList_InOperatorPicklist(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "level", Operator: dynrepo.FilterIn, Values: []string{"beginner", "intermediate"}}},
	})
	if err != nil {
		t.Fatalf("List in: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Fatalf("Filters len = %d; want 1", len(store.gotParams.Filters))
	}
	f := store.gotParams.Filters[0]
	if f.Operator != dynrepo.FilterIn {
		t.Errorf("Operator = %q; want in", f.Operator)
	}
	if len(f.Values) != 2 {
		t.Errorf("Values len = %d; want 2", len(f.Values))
	}
	if !f.CaseInsensitive {
		t.Errorf("CaseInsensitive = false; want true for picklist in (RM-14)")
	}
}

// contains on a text field → valid.
func TestList_ContainsText(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: dynrepo.FilterContains, Values: []string{"golang"}}},
	})
	if err != nil {
		t.Fatalf("List contains: %v", err)
	}
	f := store.gotParams.Filters[0]
	if f.Operator != dynrepo.FilterContains {
		t.Errorf("Operator = %q; want contains", f.Operator)
	}
}

// contains on a number field must be rejected → 422 FILTER_OPERATOR_NOT_ALLOWED.
func TestList_ContainsRejectedForNumber(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "seats", Operator: dynrepo.FilterContains, Values: []string{"5"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want FILTER_OPERATOR_NOT_ALLOWED", got, err)
	}
}

// An unknown operator is rejected → 422 FILTER_UNKNOWN_OPERATOR.
func TestList_UnknownOperatorRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: dynrepo.FilterOperator("like"), Values: []string{"foo"}}},
	})
	if got := codeOf(err); got != "FILTER_UNKNOWN_OPERATOR" {
		t.Fatalf("code = %q (err=%v); want FILTER_UNKNOWN_OPERATOR", got, err)
	}
}

// A filter naming a field not declared in sys_fields is rejected → 422 LIST_UNKNOWN_FILTER.
func TestList_UnknownFilterRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "hacker", Operator: "", Values: []string{"1"}}},
	})
	if got := codeOf(err); got != "LIST_UNKNOWN_FILTER" {
		t.Fatalf("code = %q (err=%v); want LIST_UNKNOWN_FILTER", got, err)
	}
}

// --- RM-09: sort allowlist ---

// Sort on a system column (created_at) → allowed, translates to physical col.
func TestList_SortCreatedAt(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20, Sort: "created_at",
	})
	if err != nil {
		t.Fatalf("List sort created_at: %v", err)
	}
	if store.gotParams.Sort != "created_at" {
		t.Errorf("Sort = %q; want created_at", store.gotParams.Sort)
	}
}

// Sort on a non-unique, non-relation, non-system field (seats: number, no generated col)
// → 422 SORT_NOT_ALLOWED (no silent fallback, RM-09).
func TestList_SortUnsortableFieldRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20, Sort: "seats",
	})
	if got := codeOf(err); got != "SORT_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want SORT_NOT_ALLOWED", got, err)
	}
}

// Sort on an unknown field → 422 SORT_NOT_ALLOWED (no silent fallback).
func TestList_SortUnknownFieldRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20, Sort: "hacker; DROP TABLE obj_courses",
	})
	if got := codeOf(err); got != "SORT_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want SORT_NOT_ALLOWED", got, err)
	}
}

// Sort on a unique field (has physical generated column) → allowed.
func TestList_SortUniqueField(t *testing.T) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: []metadata.FieldDefinition{
			{Name: "code", FieldType: "text", IsUnique: true},
			{Name: "title", FieldType: "text"},
		},
	}
	svc := NewRecordService(cat, store)
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20, Sort: "code",
	})
	if err != nil {
		t.Fatalf("List sort unique field: %v", err)
	}
	// physical col for unique field is the field name itself
	if store.gotParams.Sort != "code" {
		t.Errorf("Sort = %q; want code (physical col for unique field)", store.gotParams.Sort)
	}
}

// Sort on a relation field → the offset store resolves it to the <field>_id physical column
// THROUGH the allowlist it is handed. dynrepo.ListParams.normalize() looks `Sort` up in
// `SortAllowlist` (keyed by wire field name) and rejects a miss with ErrUnsortableField; this
// test used to pin Sort == "account_id" — the already-translated name, which is NOT a key — so
// every `?sort=<relation>` answered 500 RECORD_OP_FAILED while the allowlist said "sortable"
// (found live by DAILY-QUEUE #4 acceptance: `?sort=account` on crm_opportunities). The contract
// the store actually needs: Sort must be resolvable by the allowlist it receives.
func TestList_SortRelationField(t *testing.T) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: []metadata.FieldDefinition{
			{Name: "account", FieldType: "relation"},
			{Name: "title", FieldType: "text"},
		},
	}
	svc := NewRecordService(cat, store)
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20, Sort: "account",
	})
	if err != nil {
		t.Fatalf("List sort relation field: %v", err)
	}
	got := store.gotParams
	if phys, ok := got.SortAllowlist[got.Sort]; !ok || phys != "account_id" {
		t.Errorf("Sort %q handed to the offset store resolves to %q (present=%v) via its SortAllowlist; want a key that resolves to account_id — normalize() would reject it with ErrUnsortableField", got.Sort, phys, ok)
	}
}

// --- RM-14: case-insensitive eq for text types ---

// eq on a text field sets CaseInsensitive=true.
func TestList_EqTextCaseInsensitive(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: dynrepo.FilterEq, Values: []string{"Go 101"}}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	f := store.gotParams.Filters[0]
	if !f.CaseInsensitive {
		t.Errorf("CaseInsensitive = false; want true for text eq (RM-14)")
	}
}

// eq on a picklist field IS case-insensitive (RM-14 fix-direction explicitly includes picklist).
// This allows ?status=Open to match a record stored as "open".
func TestList_EqPicklistCaseInsensitive(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "level", Operator: dynrepo.FilterEq, Values: []string{"beginner"}}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	f := store.gotParams.Filters[0]
	if !f.CaseInsensitive {
		t.Errorf("CaseInsensitive = false; want true for picklist eq (RM-14: ?status=Open matches 'open')")
	}
}

// --- Search ---

// ?q resolves to a search across only the string-typed declared columns (text/picklist),
// never number (LIKE on number is nonsense).
func TestList_SearchResolvesStringColumns(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Search: "go",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.gotParams.Search == nil {
		t.Fatalf("Search not set on params: %+v", store.gotParams)
	}
	if store.gotParams.Search.Term != "go" {
		t.Errorf("Search.Term = %q; want go", store.gotParams.Search.Term)
	}
	cols := store.gotParams.Search.Columns
	// title (text) and level (picklist) are searchable; seats (number) is not.
	if len(cols) != 2 {
		t.Fatalf("Search.Columns = %v; want [title level]", cols)
	}
	want := map[string]bool{"title": true, "level": true}
	for _, c := range cols {
		if !want[c] {
			t.Errorf("unexpected searchable column %q", c)
		}
	}
}

// An empty/blank ?q must NOT produce a search clause.
func TestList_BlankSearchIgnored(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Search: "   ",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.gotParams.Search != nil {
		t.Errorf("blank ?q must not set Search; got %+v", store.gotParams.Search)
	}
}

// codeOf is defined in entity_service_test.go (same package); shared across test files.

// --- helpers for S2/S3/B1 tests ---

func extendedEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-ext", TenantID: "tenant-1", Name: "orders", DataTable: "obj_orders"}
}

func extendedFieldDefs() []metadata.FieldDefinition {
	subFields := `{"subFields":[{"name":"street","fieldType":"text"}]}`
	return []metadata.FieldDefinition{
		{Name: "title", FieldType: "text"},
		{Name: "price", FieldType: "currency"},
		{Name: "tags", FieldType: "multi_select"},
		{Name: "address", FieldType: "composite", Options: &subFields},
		{Name: "due_date", FieldType: "date"},
		{Name: "scheduled_at", FieldType: "datetime"},
	}
}

func newExtendedListService() (*RecordService, *fakeRecordStore) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: extendedEntityFixture(), fields: extendedFieldDefs()}
	return NewRecordService(cat, store), store
}

// --- B1: currency filters target .amount sub-path ---

// currency gt → CastNumeric=true, JSONSubPath="amount".
func TestList_CurrencyGtTargetsAmountSubPath(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "price", Operator: dynrepo.FilterGt, Values: []string{"100"}}},
	})
	if err != nil {
		t.Fatalf("List currency gt: %v", err)
	}
	f := store.gotParams.Filters[0]
	if f.JSONSubPath != "amount" {
		t.Errorf("JSONSubPath = %q; want amount (B1: currency gt must target .amount)", f.JSONSubPath)
	}
	if !f.CastNumeric {
		t.Errorf("CastNumeric = false; want true for currency ordered op")
	}
}

// currency eq → JSONSubPath="amount", not case-insensitive.
func TestList_CurrencyEqTargetsAmountSubPath(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "price", Operator: dynrepo.FilterEq, Values: []string{"99.99"}}},
	})
	if err != nil {
		t.Fatalf("List currency eq: %v", err)
	}
	f := store.gotParams.Filters[0]
	if f.JSONSubPath != "amount" {
		t.Errorf("JSONSubPath = %q; want amount (B1: currency eq must target .amount)", f.JSONSubPath)
	}
	if f.CaseInsensitive {
		t.Errorf("CaseInsensitive = true; want false for currency eq")
	}
}

// currency between → JSONSubPath="amount", CastNumeric=true.
func TestList_CurrencyBetweenTargetsAmountSubPath(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "price", Operator: dynrepo.FilterBetween, Values: []string{"50", "200"}}},
	})
	if err != nil {
		t.Fatalf("List currency between: %v", err)
	}
	f := store.gotParams.Filters[0]
	if f.JSONSubPath != "amount" {
		t.Errorf("JSONSubPath = %q; want amount", f.JSONSubPath)
	}
	if !f.CastNumeric {
		t.Errorf("CastNumeric = false; want true")
	}
}

// --- S2: composite and multi_select guards ---

// composite eq → 422 FILTER_OPERATOR_NOT_ALLOWED (composite cannot be filtered by value ops).
func TestList_CompositeEqRejected(t *testing.T) {
	svc, _ := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "address", Operator: dynrepo.FilterEq, Values: []string{"foo"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q; want FILTER_OPERATOR_NOT_ALLOWED for composite eq", got)
	}
}

// composite gt → 422 FILTER_OPERATOR_NOT_ALLOWED.
func TestList_CompositeGtRejected(t *testing.T) {
	svc, _ := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "address", Operator: dynrepo.FilterGt, Values: []string{"foo"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q; want FILTER_OPERATOR_NOT_ALLOWED for composite gt", got)
	}
}

// composite isnull → allowed (null check on the whole composite column is fine).
func TestList_CompositeIsNullAllowed(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "address", Operator: dynrepo.FilterIsNull, Values: nil}},
	})
	if err != nil {
		t.Fatalf("composite isnull should be allowed: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Errorf("expected 1 filter to reach store; got %d", len(store.gotParams.Filters))
	}
}

// multi_select eq → 422 FILTER_OPERATOR_NOT_ALLOWED (no scalar eq on a JSON array).
func TestList_MultiSelectEqRejected(t *testing.T) {
	svc, _ := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "tags", Operator: dynrepo.FilterEq, Values: []string{"urgent"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q; want FILTER_OPERATOR_NOT_ALLOWED for multi_select eq", got)
	}
}

// multi_select in → 422 FILTER_OPERATOR_NOT_ALLOWED (no "has element" semantics this wave).
func TestList_MultiSelectInRejected(t *testing.T) {
	svc, _ := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "tags", Operator: dynrepo.FilterIn, Values: []string{"urgent", "low"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q; want FILTER_OPERATOR_NOT_ALLOWED for multi_select in", got)
	}
}

// multi_select contains → allowed (substring match within the serialized array is cheap).
func TestList_MultiSelectContainsAllowed(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "tags", Operator: dynrepo.FilterContains, Values: []string{"urgent"}}},
	})
	if err != nil {
		t.Fatalf("multi_select contains should be allowed: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Errorf("expected 1 filter to reach store; got %d", len(store.gotParams.Filters))
	}
}

// multi_select isnull → allowed.
func TestList_MultiSelectIsNullAllowed(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "tags", Operator: dynrepo.FilterIsNull, Values: nil}},
	})
	if err != nil {
		t.Fatalf("multi_select isnull should be allowed: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Errorf("expected 1 filter to reach store; got %d", len(store.gotParams.Filters))
	}
}

// --- S3: date/datetime ordered ops use CastDatetime ---

// date gte → CastDatetime=true, CastNumeric=false.
func TestList_DateGteCastDatetime(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "due_date", Operator: dynrepo.FilterGte, Values: []string{"2026-01-01"}}},
	})
	if err != nil {
		t.Fatalf("List date gte: %v", err)
	}
	f := store.gotParams.Filters[0]
	if !f.CastDatetime {
		t.Errorf("CastDatetime = false; want true for date gte (S3)")
	}
	if f.CastNumeric {
		t.Errorf("CastNumeric = true; want false for date field")
	}
}

// datetime between → CastDatetime=true.
func TestList_DatetimeBetweenCastDatetime(t *testing.T) {
	svc, store := newExtendedListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "orders", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "scheduled_at", Operator: dynrepo.FilterBetween, Values: []string{"2026-01-01T00:00:00Z", "2026-12-31T23:59:59Z"}}},
	})
	if err != nil {
		t.Fatalf("List datetime between: %v", err)
	}
	f := store.gotParams.Filters[0]
	if !f.CastDatetime {
		t.Errorf("CastDatetime = false; want true for datetime between (S3)")
	}
}

// --- S4: case-insensitive eq now passes value as-is (no Go ToLower), COLLATE handles it ---
// This is exercised at the repo layer in the integration test; here we verify the flag
// is still set correctly (so the repo knows to apply COLLATE).
func TestList_EqTextCaseInsensitiveFlag(t *testing.T) {
	svc, store := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "title", Operator: dynrepo.FilterEq, Values: []string{"Go 101"}}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	f := store.gotParams.Filters[0]
	if !f.CaseInsensitive {
		t.Errorf("CaseInsensitive = false; want true for text eq (S4 COLLATE path)")
	}
	// Value must arrive unmodified (no Go-side ToLower — the DB does it via COLLATE).
	if len(f.Values) == 0 || f.Values[0] != "Go 101" {
		t.Errorf("Values[0] = %q; want unmodified %q (S4: no Go-side ToLower)", f.Values[0], "Go 101")
	}
}

// --- D2: cursor pagination (keyset) ---

// Offset mode page > MaxOffsetPage → 422 PAGE_TOO_DEEP.
func TestList_PageTooDeep(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: dynrepo.MaxOffsetPage + 1, Limit: 20,
	})
	if got := codeOf(err); got != "PAGE_TOO_DEEP" {
		t.Fatalf("code = %q (err=%v); want PAGE_TOO_DEEP", got, err)
	}
}

// Offset mode at exactly MaxOffsetPage → allowed (boundary should not error).
func TestList_PageAtMaxAllowed(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: dynrepo.MaxOffsetPage, Limit: 20,
	})
	if err != nil {
		t.Fatalf("page=%d should be allowed; got err: %v", dynrepo.MaxOffsetPage, err)
	}
}

// Cursor mode: After non-empty → CursorMode=true in result, store receives CursorParams
// (exercised via fakeRecordStore.ListRecordsCursor capturing into gotParams).
func TestList_CursorMode_RoutesToCursorStore(t *testing.T) {
	svc, store := newListService()
	// Build a valid cursor by encoding a sort_value + id.
	cursor := dynrepo.EncodeCursor("created_at", "DESC", false, "2026-06-24 10:00:00", "00000000-0000-0000-0000-000000000001")
	result, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 10,
		Sort:  "created_at",
		Order: "desc",
		After: cursor,
	})
	if err != nil {
		t.Fatalf("cursor mode: %v", err)
	}
	if !result.CursorMode {
		t.Error("CursorMode = false; want true when After is non-empty")
	}
	// Store must have received Order="DESC" (normalized).
	if store.gotParams.Order != "DESC" {
		t.Errorf("Order = %q; want DESC", store.gotParams.Order)
	}
}

// Cursor mode on a non-indexed sort field → 422 CURSOR_REQUIRES_INDEXED_SORT.
// "title" is a plain text field in the courseFieldDefs fixture (no unique/indexed flag) — not sortable.
func TestList_CursorMode_NonIndexedSortRejected(t *testing.T) {
	svc, _ := newListService()
	cursor := dynrepo.EncodeCursor("title", "DESC", false, "go", "00000000-0000-0000-0000-000000000001")
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 10, Sort: "title", After: cursor,
	})
	// "title" is not in the sort allowlist at all → SORT_NOT_ALLOWED fires before cursor check.
	code := codeOf(err)
	if code != "SORT_NOT_ALLOWED" && code != "CURSOR_REQUIRES_INDEXED_SORT" {
		t.Fatalf("code = %q; want SORT_NOT_ALLOWED or CURSOR_REQUIRES_INDEXED_SORT", code)
	}
}

// Cursor mode: malformed After → 400 INVALID_CURSOR (service maps ErrInvalidCursor).
func TestList_CursorMode_MalformedCursor(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 10, After: "not-a-valid-cursor!!!",
	})
	if got := codeOf(err); got != "INVALID_CURSOR" {
		t.Fatalf("code = %q (err=%v); want INVALID_CURSOR for malformed cursor", got, err)
	}
}

// specs/039 §2.1: a cursor is bound to the PHYSICAL sort column + direction the service resolves.
// A token from another chain is 400 INVALID_CURSOR before the store runs — never a keyset on the
// wrong column (a wrong page, or an empty last page after MySQL's type coercion). The no-sort
// default and a unique field's physical column still continue their own chain.
func TestList_CursorMode_BoundToSortAndOrder(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name     string
		minted   string // cursor minted under (physical col, order)
		sort     string
		order    string
		wantCode string
		wantSort string // physical column handed to the store on success
	}{
		{name: "sort A cursor replayed with sort B", minted: "code|ASC", sort: "created_at", order: "asc", wantCode: "INVALID_CURSOR"},
		{name: "desc cursor replayed with order=asc", minted: "created_at|DESC", sort: "created_at", order: "asc", wantCode: "INVALID_CURSOR"},
		{name: "no-sort cursor replayed with order=asc", minted: "created_at|DESC", order: "asc", wantCode: "INVALID_CURSOR"},
		{name: "no-sort chain continues", minted: "created_at|DESC", wantSort: "created_at"},
		{name: "unique field chain continues", minted: "code|ASC", sort: "code", order: "asc", wantSort: "code"},
		// The binding is on the PHYSICAL column: relation `account` sorts on `account_id`.
		{name: "relation field chain continues", minted: "account_id|DESC", sort: "account", wantSort: "account_id"},
		{name: "relation cursor replayed on the unique field", minted: "account_id|DESC", sort: "code", wantCode: "INVALID_CURSOR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col, ord, _ := strings.Cut(tc.minted, "|")
			store := &fakeRecordStore{}
			svc := NewRecordService(&fakeRecordCatalog{
				entity: courseEntityFixture(),
				fields: []metadata.FieldDefinition{
					{Name: "code", FieldType: "text", IsUnique: true},
					{Name: "account", FieldType: "relation"},
					{Name: "title", FieldType: "text"},
				},
			}, store)
			_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
				Page: 1, Limit: 10, Sort: tc.sort, Order: tc.order,
				After: dynrepo.EncodeCursor(col, ord, false, "x", id),
			})
			if tc.wantCode != "" {
				if got := codeOf(err); got != tc.wantCode {
					t.Fatalf("code = %q (err=%v); want %q", got, err, tc.wantCode)
				}
				if store.cursorCalls != 0 {
					t.Errorf("keyset path ran with a cursor from another sort/order")
				}
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if store.cursorCalls != 1 || store.gotParams.Sort != tc.wantSort {
				t.Errorf("keyset calls=%d sort=%q; want 1 call on %q", store.cursorCalls, store.gotParams.Sort, tc.wantSort)
			}
		})
	}
}

// CURSOR-BOOTSTRAP #42: the pagination-mode decision, table-driven. A ?paginate=cursor first
// page (After=="") must reach the keyset path WITHOUT being decoded as a cursor (DecodeCursor
// rejects "" → it would 400 INVALID_CURSOR), and every combination the engine cannot serve
// honestly is a named 422 that never touches the store.
func TestList_PaginationMode(t *testing.T) {
	// No ?sort=/?order= resolves to the keyset default created_at DESC — the binding the token needs.
	valid := dynrepo.EncodeCursor("created_at", "DESC", false, "2026-09-01 00:00:00", "00000000-0000-0000-0000-000000000009")
	cases := []struct {
		name       string
		q          ListQuery
		wantCode   string // "" → success
		wantCursor bool
		wantAfter  string
	}{
		{name: "paginate=cursor opens keyset from the first page",
			q: ListQuery{Page: 1, Limit: 100, Sort: "created_at", Order: "desc", Paginate: "cursor"}, wantCursor: true},
		{name: "paginate=cursor follows an after token",
			q: ListQuery{Page: 1, Limit: 100, Paginate: "cursor", After: valid}, wantCursor: true, wantAfter: valid},
		{name: "legacy after alone is still keyset",
			q: ListQuery{Page: 1, Limit: 20, After: valid}, wantCursor: true, wantAfter: valid},
		{name: "no paginate stays offset",
			q: ListQuery{Page: 3, Limit: 20, PageExplicit: true}},
		{name: "paginate=offset is the explicit default",
			q: ListQuery{Page: 3, Limit: 20, Paginate: "offset", PageExplicit: true}},
		{name: "unknown mode", q: ListQuery{Page: 1, Limit: 20, Paginate: "keyset"}, wantCode: "INVALID_PAGINATION_MODE"},
		{name: "cursor with explicit page", q: ListQuery{Page: 5, Limit: 20, Paginate: "cursor", PageExplicit: true}, wantCode: "PAGINATION_MODE_CONFLICT"},
		{name: "after with explicit page", q: ListQuery{Page: 2, Limit: 20, After: valid, PageExplicit: true}, wantCode: "PAGINATION_MODE_CONFLICT"},
		{name: "offset with after", q: ListQuery{Page: 1, Limit: 20, Paginate: "offset", After: valid}, wantCode: "PAGINATION_MODE_CONFLICT"},
		{name: "recycle bin refuses cursor", q: ListQuery{Page: 1, Limit: 20, Paginate: "cursor", OnlyDeleted: true}, wantCode: "PAGINATION_MODE_CONFLICT"},
		{name: "recycle bin refuses after", q: ListQuery{Page: 1, Limit: 20, After: valid, OnlyDeleted: true}, wantCode: "PAGINATION_MODE_CONFLICT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := newListService()
			res, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", tc.q)
			if tc.wantCode != "" {
				if got := codeOf(err); got != tc.wantCode {
					t.Fatalf("code = %q (err=%v); want %q", got, err, tc.wantCode)
				}
				if store.cursorCalls != 0 {
					t.Errorf("keyset path ran for a rejected request")
				}
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if res.CursorMode != tc.wantCursor {
				t.Errorf("CursorMode = %v; want %v", res.CursorMode, tc.wantCursor)
			}
			if tc.wantCursor && (store.cursorCalls != 1 || store.gotAfter != tc.wantAfter) {
				t.Errorf("keyset calls=%d after=%q; want 1 call with after=%q", store.cursorCalls, store.gotAfter, tc.wantAfter)
			}
			if !tc.wantCursor && store.cursorCalls != 0 {
				t.Errorf("offset request hit the keyset path")
			}
		})
	}
}

// Offset mode (After="") → CursorMode=false in result.
func TestList_OffsetMode_CursorModeFalse(t *testing.T) {
	svc, _ := newListService()
	result, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
	})
	if err != nil {
		t.Fatalf("offset mode: %v", err)
	}
	if result.CursorMode {
		t.Error("CursorMode = true; want false for offset mode (no After)")
	}
}
