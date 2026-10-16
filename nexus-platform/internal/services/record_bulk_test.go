package services

// ENG-BATCH #1 unit tests (fake store/catalog/sequenceClaimer — no DB). Covers the two-phase
// contract from ../specs/009-bulk-create/spec.md: cap/empty guards, atomic-abort (zero writes, zero
// sequence claims), partial best-effort, intra-batch unique/1-1 pre-check, RM-11 batched
// relation-FK probing, per-row field-ACL, and index-mapping. Integration coverage (real MySQL —
// atomic rollback, race, tenant isolation, auto_number) lives in
// internal/services/eng_batch_bulk_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// --- fixtures -----------------------------------------------------------------------------

// bulkEntityFixture returns the fixed "widgets" entity used by every CreateBulk unit test.
func bulkEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-widgets", TenantID: "tenant-1", Name: "widgets", DataTable: "obj_widgets"}
}

// bulkFieldDefinitions declares: name (required text), email (unique text — intra-batch
// RECORD_DUPLICATE candidate), owner (1-N relation → accounts), profile (1-1 relation →
// profiles — intra-batch RECORD_RELATION_ALREADY_BOUND candidate), code (auto_number).
func bulkFieldDefinitions(t *testing.T) []metadata.FieldDefinition {
	t.Helper()
	return []metadata.FieldDefinition{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "email", FieldType: "text", IsUnique: true},
		{Name: "owner", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
		{Name: "profile", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "profiles", "cardinality": "1-1"})},
		{Name: "code", FieldType: "auto_number", Options: jsonOptsPtr(t, map[string]any{"prefix": "W-", "padding": float64(3)})},
		{Name: "assignee", FieldType: "user"},
		{Name: "related_to", FieldType: "polymorphic_relation", Options: jsonOptsPtr(t, map[string]any{"targetEntities": []any{"accounts"}})},
	}
}

// bulkCatalog resolves the fixed widgets entity/fields — enough for CreateBulk's resolve() step.
type bulkCatalog struct {
	entity    *metadata.EntityDefinition
	entityErr error
	fields    []metadata.FieldDefinition
}

func (c *bulkCatalog) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	if c.entityErr != nil {
		return nil, c.entityErr
	}
	return c.entity, nil
}
func (c *bulkCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return c.fields, nil
}

func bulkCatalogFixture(t *testing.T) *bulkCatalog {
	t.Helper()
	return &bulkCatalog{entity: bulkEntityFixture(), fields: bulkFieldDefinitions(t)}
}

// bulkTestStore is a RecordStore fake that: (1) captures every InsertRecord call (the path
// mode=partial exercises via Create()), (2) answers BatchRecordExists from a configurable `live`
// set while counting invocations (RM-11 anti-N+1 proof), (3) hands out sequential ids.
type bulkTestStore struct {
	inserted   []map[string]any
	live       map[string]bool // key: "entity|tenant|id"
	batchCalls int
	nextID     int
}

func (s *bulkTestStore) InsertRecord(_ context.Context, _, _, _ string, data map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	s.nextID++
	s.inserted = append(s.inserted, cloneData(data))
	return fmt.Sprintf("rec-%d", s.nextID), nil
}
func (s *bulkTestStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return nil, dynrepo.ErrRecordNotFound
}
func (s *bulkTestStore) ListRecords(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return nil, 0, nil
}
func (s *bulkTestStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return nil, "", nil
}
func (s *bulkTestStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *bulkTestStore) SoftDeleteRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) error {
	return nil
}
func (s *bulkTestStore) RestoreRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *bulkTestStore) RecordExists(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}
func (s *bulkTestStore) BatchRecordExists(_ context.Context, entity, tenantID string, ids []string) (map[string]bool, error) {
	s.batchCalls++
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = s.live[entity+"|"+tenantID+"|"+id]
	}
	return result, nil
}
func (s *bulkTestStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	return dynrepo.AggregateScalar{}, nil
}
func (s *bulkTestStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	return nil, nil
}
func (s *bulkTestStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}
func (s *bulkTestStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}

// fakeBulkInserter is a scriptable BulkInserter (mode=atomic Phase 2). By default every call
// succeeds and hands out sequential ids; set err+failedIdx to simulate a mid-batch DB failure
// (the atomic transaction rolling back).
type fakeBulkInserter struct {
	calls     int
	lastRows  []map[string]any
	err       error
	failedIdx int
	failTimes int
	failErr   error
	nextID    int
}

// failTimes/failErr script TRANSIENT failures: the first failTimes calls return failErr
// (e.g. dynrepo.ErrWriteConflict) and then the fake succeeds — exercising the atomic
// deadlock-retry loop (dual-review 2026-09-01 MEDIUM: that loop had zero coverage).
func (f *fakeBulkInserter) InsertRecordsAtomic(_ context.Context, _, _, _ string, rows []map[string]any, _ []dynrepo.UniqueFieldSpec) ([]string, int, error) {
	f.calls++
	f.lastRows = rows
	if f.failTimes > 0 {
		f.failTimes--
		return nil, -1, f.failErr
	}
	if f.err != nil {
		return nil, f.failedIdx, f.err
	}
	ids := make([]string, len(rows))
	for i := range rows {
		f.nextID++
		ids[i] = fmt.Sprintf("atomic-%d", f.nextID)
	}
	return ids, -1, nil
}

func bulkAdminScope() RecordScope {
	return RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
}

func bulkRoleScope() RecordScope {
	return RecordScope{UserID: "u1", AccessLevel: "all", RoleID: "role-1"}
}

func newBulkSvc(store *bulkTestStore, claimer SequenceClaimer, inserter BulkInserter, acl fieldACLEnforcer, t *testing.T) *RecordService {
	svc := NewRecordService(bulkCatalogFixture(t), store).WithSequenceClaimer(claimer).WithBulkInserter(inserter)
	if acl != nil {
		svc = svc.WithFieldACL(acl)
	}
	return svc
}

// --- cap / empty / mode guards --------------------------------------------------------------

func TestCreateBulk_Empty_ReturnsBulkEmpty(t *testing.T) {
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", nil, BulkModeAtomic)
	if codeOf(err) != "BULK_EMPTY" {
		t.Fatalf("code = %q; want BULK_EMPTY (err=%v)", codeOf(err), err)
	}
}

func TestCreateBulk_TooLarge_ReturnsBulkTooLargeWithMaxDetail(t *testing.T) {
	t.Setenv("BULK_MAX_ROWS", "2")
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{{"name": "a"}, {"name": "b"}, {"name": "c"}}
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if codeOf(err) != "BULK_TOO_LARGE" {
		t.Fatalf("code = %q; want BULK_TOO_LARGE (err=%v)", codeOf(err), err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err is not *ValidationError: %v", err)
	}
	if ve.Details["max"] != 2 {
		t.Errorf("Details[max] = %v; want 2", ve.Details["max"])
	}
}

func TestCreateBulk_InvalidMode_ReturnsBulkInvalidMode(t *testing.T) {
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{{"name": "a"}}
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkMode("bogus"))
	if codeOf(err) != "BULK_INVALID_MODE" {
		t.Fatalf("code = %q; want BULK_INVALID_MODE (err=%v)", codeOf(err), err)
	}
}

func TestCreateBulk_EntityNotFound(t *testing.T) {
	catalog := &bulkCatalog{entityErr: metadata.ErrEntityNotFound}
	svc := NewRecordService(catalog, &bulkTestStore{}).WithSequenceClaimer(&fakeSequenceClaimer{}).WithBulkInserter(&fakeBulkInserter{})
	payloads := []map[string]any{{"name": "a"}}
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "ghost", payloads, BulkModeAtomic)
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("err = %v; want ErrEntityNotFound", err)
	}
}

// --- parity: partial-mode single row == what a standalone Create() would persist -----------

func TestCreateBulk_Partial_SingleRow_ParityWithCreate(t *testing.T) {
	store := &bulkTestStore{}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{{"name": "Acme", "email": "acme@example.com"}}

	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 1 || result.Failed != 0 {
		t.Fatalf("Created=%d Failed=%d; want 1/0", result.Created, result.Failed)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("store.inserted len = %d; want 1", len(store.inserted))
	}
	if got := store.inserted[0]["name"]; got != "Acme" {
		t.Errorf("inserted name = %v; want Acme", got)
	}
	if got := store.inserted[0]["email"]; got != "acme@example.com" {
		t.Errorf("inserted email = %v; want acme@example.com", got)
	}
	if result.Results[0].Status != "created" || result.Results[0].ID == "" {
		t.Errorf("Results[0] = %+v; want status=created with non-empty id", result.Results[0])
	}
}

// --- crown jewel #1: atomic Phase-1 failure aborts with ZERO writes and ZERO sequence claims -

func TestCreateBulk_Atomic_PhaseOneFailure_AbortsWithZeroWritesAndZeroClaims(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{}
	inserter := &fakeBulkInserter{}
	svc := newBulkSvc(store, claimer, inserter, nil, t)

	payloads := []map[string]any{
		{"name": "Ok1"},
		{"name": "Ok2"},
		{"email": "missing-name@example.com"}, // fails RECORD_REQUIRED_MISSING (name absent)
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk top-level error: %v (want nil — failure must be embedded in the envelope)", err)
	}
	if result.Mode != "atomic" || result.Created != 0 || result.Failed != 1 || result.Skipped != 2 {
		t.Fatalf("result = %+v; want atomic/created=0/failed=1/skipped=2 (only row2 bad; rows0-1 skipped)", result)
	}
	if len(result.Results) != 3 {
		t.Fatalf("Results len = %d; want 3", len(result.Results))
	}
	if result.Results[0].Status != "skipped" || result.Results[0].Code != "BULK_ATOMIC_ABORTED" {
		t.Errorf("Results[0] = %+v; want status=skipped code=BULK_ATOMIC_ABORTED", result.Results[0])
	}
	if result.Results[1].Status != "skipped" || result.Results[1].Code != "BULK_ATOMIC_ABORTED" {
		t.Errorf("Results[1] = %+v; want status=skipped code=BULK_ATOMIC_ABORTED", result.Results[1])
	}
	if result.Results[2].Status != "failed" || result.Results[2].Code != "RECORD_REQUIRED_MISSING" {
		t.Errorf("Results[2] = %+v; want status=failed code=RECORD_REQUIRED_MISSING", result.Results[2])
	}
	// The crown-jewel assertions: Phase 2 must NEVER run when Phase 1 found any failure.
	if len(store.inserted) != 0 {
		t.Errorf("store.inserted len = %d; want 0 (atomic abort must write nothing)", len(store.inserted))
	}
	if inserter.calls != 0 {
		t.Errorf("fakeBulkInserter.calls = %d; want 0 (Phase 2 insert must never be reached)", inserter.calls)
	}
	if claimer.calls != 0 {
		t.Errorf("fakeSequenceClaimer.calls = %d; want 0 (no auto_number claimed for a row that never reached Phase 2)", claimer.calls)
	}
}

// --- crown jewel #2: atomic success claims exactly one sequence PER ROW, one insert call ----

func TestCreateBulk_Atomic_AllValid_ClaimsSequencePerRow_SingleInsertCall(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{}
	inserter := &fakeBulkInserter{}
	svc := newBulkSvc(store, claimer, inserter, nil, t)

	payloads := []map[string]any{
		{"name": "Row0"},
		{"name": "Row1"},
		{"name": "Row2"},
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 3 || result.Failed != 0 {
		t.Fatalf("Created=%d Failed=%d; want 3/0", result.Created, result.Failed)
	}
	if inserter.calls != 1 {
		t.Fatalf("fakeBulkInserter.calls = %d; want 1 (ONE transaction for the whole batch)", inserter.calls)
	}
	if len(inserter.lastRows) != 3 {
		t.Fatalf("lastRows len = %d; want 3", len(inserter.lastRows))
	}
	if claimer.calls != 3 {
		t.Fatalf("claimer.calls = %d; want 3 (one auto_number claim per row)", claimer.calls)
	}
	wantCodes := []string{"W-001", "W-002", "W-003"}
	for i, want := range wantCodes {
		if got := inserter.lastRows[i]["code"]; got != want {
			t.Errorf("lastRows[%d][code] = %v; want %v (sequential per-row claim BEFORE insert)", i, got, want)
		}
	}
	for i, r := range result.Results {
		if r.Status != "created" || r.Index != i || r.ID == "" {
			t.Errorf("Results[%d] = %+v; want status=created index=%d non-empty id", i, r, i)
		}
	}
}

// --- atomic DB-level failure: rollback + gap-burn (documented gaps-allowed behaviour) --------

func TestCreateBulk_Atomic_InsertFails_AbortsAndMapsFailingRow(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{}
	inserter := &fakeBulkInserter{err: &dynrepo.DuplicateUniqueError{IndexName: "uq_obj_widgets_email"}, failedIdx: 1}
	svc := newBulkSvc(store, claimer, inserter, nil, t)

	payloads := []map[string]any{
		{"name": "Row0", "email": "a@example.com"},
		{"name": "Row1", "email": "b@example.com"},
		{"name": "Row2", "email": "c@example.com"},
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk top-level error: %v (want nil)", err)
	}
	if result.Created != 0 || result.Failed != 1 || result.Skipped != 2 {
		t.Fatalf("Created=%d Failed=%d Skipped=%d; want 0/1/2 (insert fails at row1; rows0,2 skipped)", result.Created, result.Failed, result.Skipped)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_DUPLICATE" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_DUPLICATE", result.Results[1])
	}
	if result.Results[1].Details["field"] != "email" {
		t.Errorf("Results[1].Details[field] = %v; want email", result.Results[1].Details["field"])
	}
	if result.Results[0].Status != "skipped" || result.Results[2].Status != "skipped" {
		t.Errorf("Results[0]/[2] must be skipped/BULK_ATOMIC_ABORTED; got %+v / %+v", result.Results[0], result.Results[2])
	}
	// Documented gap-mode (../specs/006-auto-number/spec.md §"#6"): the claim happens BEFORE the insert attempt,
	// so a rollback still burns the numbers already claimed for THIS batch.
	if claimer.calls != 3 {
		t.Errorf("claimer.calls = %d; want 3 (sequence claim happens before the insert attempt — gap-burn on rollback)", claimer.calls)
	}
}

// --- atomic: bulkStore not wired fails closed -------------------------------------------------

func TestCreateBulk_Atomic_NilBulkStore_FailsClosed(t *testing.T) {
	store := &bulkTestStore{}
	svc := NewRecordService(bulkCatalogFixture(t), store).WithSequenceClaimer(&fakeSequenceClaimer{}) // no WithBulkInserter
	payloads := []map[string]any{{"name": "Row0"}}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk top-level error: %v (want nil — embedded in envelope)", err)
	}
	if result.Created != 0 || result.Failed != 1 {
		t.Fatalf("Created=%d Failed=%d; want 0/1", result.Created, result.Failed)
	}
	if result.Results[0].Code != "BULK_ATOMIC_UNAVAILABLE" {
		t.Errorf("Results[0].Code = %q; want BULK_ATOMIC_UNAVAILABLE", result.Results[0].Code)
	}
}

// --- infra-error classification: a non-ValidationError (e.g. a DB timeout) must NEVER be ------
// --- masked as a 422-shaped per-row code — it must bubble as a top-level error so respondError --
// --- can classify it (503 DB_TIMEOUT / 500), exactly like a single Create() would. -------------

func TestCreateBulk_Atomic_InsertInfraError_BubblesAsTopLevelError(t *testing.T) {
	store := &bulkTestStore{}
	inserter := &fakeBulkInserter{err: errors.New("dial tcp 10.0.0.5:3306: i/o timeout"), failedIdx: -1}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, inserter, nil, t)

	payloads := []map[string]any{{"name": "Row0"}, {"name": "Row1"}}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err == nil {
		t.Fatalf("CreateBulk top-level error = nil; want a bubbled infra error (result=%+v)", result)
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("bubbled error must NOT be a *ValidationError (that would map to 422, not 503/500): %v", err)
	}
	if result.Results != nil {
		t.Errorf("result.Results = %+v; want nil/empty on a top-level infra error", result.Results)
	}
}

func TestCreateBulk_Atomic_SequenceClaimInfraError_BubblesAsTopLevelError(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{err: errors.New("dial tcp 10.0.0.5:3306: i/o timeout")}
	svc := newBulkSvc(store, claimer, &fakeBulkInserter{}, nil, t)

	payloads := []map[string]any{{"name": "Row0"}}
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err == nil {
		t.Fatal("CreateBulk top-level error = nil; want a bubbled infra error from the sequence claim")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("bubbled error must NOT be a *ValidationError: %v", err)
	}
}

func TestCreateBulk_Partial_InfraErrorOnOneRow_BubblesAsTopLevelError(t *testing.T) {
	store := &bulkTestStore{}
	// GetRecord/InsertRecord succeed normally for Create(); force the infra failure at the
	// sequence-claim step so it surfaces mid-partial-loop on the second row.
	claimer := &countingFailAfterNClaimer{failAfter: 1, err: errors.New("dial tcp 10.0.0.5:3306: i/o timeout")}
	svc := newBulkSvc(store, claimer, &fakeBulkInserter{}, nil, t)

	payloads := []map[string]any{{"name": "Row0"}, {"name": "Row1"}}
	_, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err == nil {
		t.Fatal("CreateBulk top-level error = nil; want a bubbled infra error from row 1's Create() call")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("bubbled error must NOT be a *ValidationError: %v", err)
	}
	// Row 0 (before the infra failure) must have been durably inserted regardless — a top-level
	// error on THIS response must never imply the earlier row's Create() call was rolled back.
	if len(store.inserted) != 1 {
		t.Errorf("store.inserted len = %d; want 1 (row 0's independent Create() call already committed)", len(store.inserted))
	}
}

// countingFailAfterNClaimer succeeds on its first `failAfter` calls, then fails every call after
// that with err — used to simulate an infra hiccup partway through a partial-mode loop.
type countingFailAfterNClaimer struct {
	calls     int
	failAfter int
	err       error
}

func (c *countingFailAfterNClaimer) ClaimSequence(_ context.Context, _, _, _ string) (int64, error) {
	c.calls++
	if c.calls > c.failAfter {
		return 0, c.err
	}
	return int64(c.calls), nil
}

// --- partial mode: mixed valid/invalid, index mapping, sequence claims == valid-row count ---

func TestCreateBulk_Partial_MixedRows_IndexMappingAndSequenceClaims(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{}
	svc := newBulkSvc(store, claimer, &fakeBulkInserter{}, nil, t)

	payloads := []map[string]any{
		{"name": "Row0"},                        // 0: valid
		{"email": "no-name@example.com"},        // 1: invalid — RECORD_REQUIRED_MISSING
		{"name": "Row2"},                        // 2: valid
		{"name": "Row3", "bogus_field": "nope"}, // 3: invalid — RECORD_UNKNOWN_FIELD
		{"name": "Row4"},                        // 4: valid
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Mode != "partial" || result.Created != 3 || result.Failed != 2 {
		t.Fatalf("result = %+v; want partial/created=3/failed=2", result)
	}
	wantStatus := []string{"created", "failed", "created", "failed", "created"}
	wantCode := []string{"", "RECORD_REQUIRED_MISSING", "", "RECORD_UNKNOWN_FIELD", ""}
	for i, r := range result.Results {
		if r.Index != i {
			t.Errorf("Results[%d].Index = %d; want %d", i, r.Index, i)
		}
		if r.Status != wantStatus[i] {
			t.Errorf("Results[%d].Status = %q; want %q", i, r.Status, wantStatus[i])
		}
		if r.Code != wantCode[i] {
			t.Errorf("Results[%d].Code = %q; want %q", i, r.Code, wantCode[i])
		}
	}
	if len(store.inserted) != 3 {
		t.Fatalf("store.inserted len = %d; want 3", len(store.inserted))
	}
	if claimer.calls != 3 {
		t.Errorf("claimer.calls = %d; want 3 (only the 3 VALID rows reach Create()'s assignSequences)", claimer.calls)
	}
}

// --- intra-batch unique pre-check ------------------------------------------------------------

func TestCreateBulk_IntraBatchUniqueDuplicate_Atomic_AbortsWholeBatch(t *testing.T) {
	store := &bulkTestStore{}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0", "email": "dup@example.com"},
		{"name": "Row1", "email": "dup@example.com"}, // same email — must fail as the LATER row
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 0 || result.Failed != 1 || result.Skipped != 1 {
		t.Fatalf("Created=%d Failed=%d Skipped=%d; want 0/1/1 (row1 dup fails, row0 skipped)", result.Created, result.Failed, result.Skipped)
	}
	if result.Results[0].Status != "skipped" {
		t.Errorf("Results[0].Status = %q; want skipped (row0 itself is valid)", result.Results[0].Status)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_DUPLICATE" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_DUPLICATE", result.Results[1])
	}
	if len(store.inserted) != 0 {
		t.Errorf("store.inserted len = %d; want 0", len(store.inserted))
	}
}

func TestCreateBulk_IntraBatchUniqueDuplicate_Partial_FirstWinsSecondFails(t *testing.T) {
	store := &bulkTestStore{}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0", "email": "dup@example.com"},
		{"name": "Row1", "email": "dup@example.com"},
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 1 || result.Failed != 1 {
		t.Fatalf("Created=%d Failed=%d; want 1/1", result.Created, result.Failed)
	}
	if result.Results[0].Status != "created" {
		t.Errorf("Results[0].Status = %q; want created", result.Results[0].Status)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_DUPLICATE" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_DUPLICATE", result.Results[1])
	}
	if len(store.inserted) != 1 {
		t.Fatalf("store.inserted len = %d; want 1", len(store.inserted))
	}
}

// --- intra-batch 1-1 relation pre-check -------------------------------------------------------

func TestCreateBulk_IntraBatchOneToOneRelationDuplicate(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{"profiles|tenant-1|prof-1": true}}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0", "profile": "prof-1"},
		{"name": "Row1", "profile": "prof-1"}, // same 1-1 target — must fail as the LATER row
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Results[0].Status != "created" {
		t.Errorf("Results[0].Status = %q; want created", result.Results[0].Status)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_RELATION_ALREADY_BOUND" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_RELATION_ALREADY_BOUND", result.Results[1])
	}
	if result.Results[1].Details["field"] != "profile" {
		t.Errorf("Results[1].Details[field] = %v; want profile", result.Results[1].Details["field"])
	}
}

// --- relation-FK validation: per-row error + RM-11 single batched probe ---------------------

// TestCreateBulk_RelationInvalid_Atomic_SingleBatchedProbe proves the RM-11 property in
// isolation: mode=atomic never re-validates relations in Phase 2 (it goes straight from
// Phase-1's already-clean data to assignSequences/InsertRecordsAtomic), so Phase-1's ONE
// BatchRecordExists call per unique target entity — covering EVERY row's ids in the WHOLE
// batch — is the ONLY relation probe issued, regardless of batch size.
func TestCreateBulk_RelationInvalid_Atomic_SingleBatchedProbe(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{"accounts|tenant-1|acct-live": true}}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0", "owner": "acct-live"},
		{"name": "Row1", "owner": "acct-dead"}, // does not exist → RECORD_RELATION_INVALID
		{"name": "Row2", "owner": "acct-live"}, // same target as row0
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 0 || result.Failed != 1 || result.Skipped != 2 {
		t.Fatalf("Created=%d Failed=%d Skipped=%d; want 0/1/2 (row1 bad FK fails; rows0,2 skipped)", result.Created, result.Failed, result.Skipped)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_RELATION_INVALID" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_RELATION_INVALID", result.Results[1])
	}
	// RM-11: ids for the SAME target entity ("accounts") across the WHOLE batch are probed in
	// ONE BatchRecordExists call, not once per row/field — and atomic mode never re-probes in
	// Phase 2, so this is the total for the entire CreateBulk call.
	if store.batchCalls != 1 {
		t.Errorf("store.batchCalls = %d; want 1 (RM-11 — one probe per unique target entity for the WHOLE batch)", store.batchCalls)
	}
}

// TestCreateBulk_RelationInvalid_Partial_ReusesCreatePerSurvivingRow documents the (bounded,
// intentional) cost of mode=partial's full reuse of Create() for each surviving row (spec's
// "TÁI DÙNG, KHÔNG copy logic" mandate): Phase-1's ONE batched probe filters out the bad row
// BEFORE Phase 2, but each row that reaches Phase 2 calls Create() — which independently
// re-validates relations via its own single-row check. Total cost here is IDENTICAL to the
// client sending N separate POST /:entity requests (the semantic definition of "partial")
// PLUS Phase-1's one cheap early-reject call; it is never worse than that.
func TestCreateBulk_RelationInvalid_Partial_ReusesCreatePerSurvivingRow(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{"accounts|tenant-1|acct-live": true}}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0", "owner": "acct-live"},
		{"name": "Row1", "owner": "acct-dead"}, // does not exist → RECORD_RELATION_INVALID
		{"name": "Row2", "owner": "acct-live"}, // same target as row0
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 2 || result.Failed != 1 {
		t.Fatalf("Created=%d Failed=%d; want 2/1", result.Created, result.Failed)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_RELATION_INVALID" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_RELATION_INVALID", result.Results[1])
	}
	// 1 (Phase-1 batched pre-check, whole batch) + 2 (one per surviving row's own Create() call).
	if store.batchCalls != 3 {
		t.Errorf("store.batchCalls = %d; want 3 (1 phase-1 batched probe + 1 per surviving row's Create() call)", store.batchCalls)
	}
}

// --- field-ACL per row -------------------------------------------------------------------------

func TestCreateBulk_FieldACL_ForbiddenFieldPerRow(t *testing.T) {
	store := &bulkTestStore{}
	acl := &fakeFieldACL{deniedField: "owner"}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, acl, t)
	payloads := []map[string]any{
		{"name": "Row0"},                       // no owner — allowed
		{"name": "Row1", "owner": "acct-live"}, // touches forbidden field
	}
	result, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Results[0].Status != "created" {
		t.Errorf("Results[0].Status = %q; want created", result.Results[0].Status)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "FIELD_FORBIDDEN" {
		t.Errorf("Results[1] = %+v; want status=failed code=FIELD_FORBIDDEN", result.Results[1])
	}
}

// --- readonly computed / auto_number rejection (per row, via ValidateRecord reuse) ----------

func TestCreateBulk_ReadonlyAutoNumberFieldRejectedPerRow(t *testing.T) {
	store := &bulkTestStore{}
	claimer := &fakeSequenceClaimer{}
	svc := newBulkSvc(store, claimer, &fakeBulkInserter{}, nil, t)
	payloads := []map[string]any{
		{"name": "Row0"},
		{"name": "Row1", "code": "W-999"}, // client trying to set a server-assigned field
	}
	result, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_AUTONUMBER_READONLY" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_AUTONUMBER_READONLY", result.Results[1])
	}
	if claimer.calls != 0 {
		t.Errorf("claimer.calls = %d; want 0 (a rejected payload must never reach assignSequences)", claimer.calls)
	}
}

// --- pure-function coverage: BulkMaxRows / dedupKey / applyIntraBatchUniqueCheck / errParts --

func TestBulkMaxRows_DefaultAndOverride(t *testing.T) {
	if got := BulkMaxRows(); got != defaultBulkMaxRows {
		t.Errorf("BulkMaxRows() default = %d; want %d", got, defaultBulkMaxRows)
	}
	t.Setenv("BULK_MAX_ROWS", "7")
	if got := BulkMaxRows(); got != 7 {
		t.Errorf("BulkMaxRows() with env=7 = %d; want 7", got)
	}
	t.Setenv("BULK_MAX_ROWS", "not-a-number")
	if got := BulkMaxRows(); got != defaultBulkMaxRows {
		t.Errorf("BulkMaxRows() with invalid env = %d; want fallback %d", got, defaultBulkMaxRows)
	}
	t.Setenv("BULK_MAX_ROWS", "-5")
	if got := BulkMaxRows(); got != defaultBulkMaxRows {
		t.Errorf("BulkMaxRows() with non-positive env = %d; want fallback %d", got, defaultBulkMaxRows)
	}
}

func TestDedupKey_StringsAreByteExact_NoCaseFolding(t *testing.T) {
	kFoo, err := dedupKey("Foo")
	if err != nil {
		t.Fatalf("dedupKey(Foo): %v", err)
	}
	kfoo, err := dedupKey("foo")
	if err != nil {
		t.Fatalf("dedupKey(foo): %v", err)
	}
	if kFoo == kfoo {
		t.Fatalf("dedupKey must be byte-exact (no case-folding): dedupKey(%q) == dedupKey(%q) == %q", "Foo", "foo", kFoo)
	}
	kFoo2, _ := dedupKey("Foo")
	if kFoo != kFoo2 {
		t.Errorf("dedupKey must be stable for the same input: %q != %q", kFoo, kFoo2)
	}
}

func TestDedupKey_ScalarsAndMaps(t *testing.T) {
	if k, err := dedupKey(float64(42)); err != nil || k != "42" {
		t.Errorf("dedupKey(42.0) = %q, %v; want \"42\", nil", k, err)
	}
	if k, err := dedupKey(true); err != nil || k != "true" {
		t.Errorf("dedupKey(true) = %q, %v; want \"true\", nil", k, err)
	}
	k1, err1 := dedupKey(map[string]any{"amount": float64(10), "currencyCode": "USD"})
	k2, err2 := dedupKey(map[string]any{"currencyCode": "USD", "amount": float64(10)})
	if err1 != nil || err2 != nil {
		t.Fatalf("dedupKey(map): errs %v / %v", err1, err2)
	}
	if k1 != k2 {
		t.Errorf("dedupKey must be deterministic regardless of Go map iteration order: %q != %q", k1, k2)
	}
}

func TestApplyIntraBatchUniqueCheck_SkipsAlreadyFailedAndEmptyValues(t *testing.T) {
	specs := []FieldSpec{{Name: "email", FieldType: "text", IsUnique: true}}
	rows := []*bulkRowState{
		{index: 0, err: errors.New("already failed")},  // excluded — never claims
		{index: 1, clean: map[string]any{"email": ""}}, // empty — never claims (NULL-ish)
		{index: 2, clean: map[string]any{"email": "a@example.com"}},
		{index: 3, clean: map[string]any{"email": "a@example.com"}}, // duplicates row 2
	}
	applyIntraBatchUniqueCheck(specs, rows)
	if rows[0].err == nil || rows[0].err.Error() != "already failed" {
		t.Errorf("rows[0].err must be left untouched")
	}
	if rows[1].err != nil {
		t.Errorf("rows[1].err = %v; want nil (empty value never collides)", rows[1].err)
	}
	if rows[2].err != nil {
		t.Errorf("rows[2].err = %v; want nil (first claimant)", rows[2].err)
	}
	if codeOf(rows[3].err) != "RECORD_DUPLICATE" {
		t.Errorf("rows[3].err code = %q; want RECORD_DUPLICATE", codeOf(rows[3].err))
	}
}

func TestErrParts_ValidationErrorPassthroughAndGenericFallback(t *testing.T) {
	ve := &ValidationError{Code: "RECORD_DUPLICATE", Msg: "trùng rồi", Details: map[string]any{"field": "email"}}
	code, msg, details := errParts(ve)
	if code != "RECORD_DUPLICATE" || msg != "trùng rồi" || details["field"] != "email" {
		t.Errorf("errParts(ValidationError) = %q, %q, %v; want passthrough", code, msg, details)
	}

	code2, msg2, _ := errParts(errors.New("connection refused: 10.0.0.5:3306"))
	if code2 != "RECORD_OP_FAILED" {
		t.Errorf("errParts(generic) code = %q; want RECORD_OP_FAILED", code2)
	}
	if msg2 == "connection refused: 10.0.0.5:3306" {
		t.Fatalf("errParts must NEVER leak a raw infra error string (B12): got %q", msg2)
	}
}

func TestAbortAtomicBulk_MarksNonFailedRowsSkipped(t *testing.T) {
	rows := []*bulkRowState{
		{index: 0},
		{index: 1, err: invalid("RECORD_REQUIRED_MISSING", "field %q là bắt buộc", "name")},
		{index: 2},
	}
	result := abortAtomicBulk(rows)
	if result.Created != 0 || result.Failed != 1 || result.Skipped != 2 {
		t.Fatalf("Created=%d Failed=%d Skipped=%d; want 0/1/2 (insert fails at row1; rows0,2 skipped)", result.Created, result.Failed, result.Skipped)
	}
	if result.Results[0].Status != "skipped" || result.Results[0].Code != "BULK_ATOMIC_ABORTED" {
		t.Errorf("Results[0] = %+v; want skipped/BULK_ATOMIC_ABORTED", result.Results[0])
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_REQUIRED_MISSING" {
		t.Errorf("Results[1] = %+v; want failed/RECORD_REQUIRED_MISSING", result.Results[1])
	}
	if result.Results[2].Status != "skipped" {
		t.Errorf("Results[2].Status = %q; want skipped", result.Results[2].Status)
	}
}

// --- ENG-SAMPLE-REF: "@ref" resolution across SEPARATE CreateBulk calls -------------------

// refCatalogFixture resolves a fixed "cong_viec" ("Công việc") entity with a SELF-referencing
// relation field "parent_id" — this is the "Dự án → Công việc" shape from the task in miniature,
// collapsed to one entity so the fake single-entity catalog can express "child references a
// record of the SAME entity created by an earlier, separate CreateBulk call". Cross-ENTITY
// resolution (the actual Dự án/Công việc case) is proven over real MySQL by the -tags=integration
// test in record_bulk_ref_integration_test.go — this file only has fakes, so it cannot construct
// two distinct obj_* tables; the resolution MECHANISM (registry survives across calls, strips
// "$ref", never lets an unresolved token slip past Phase 1) is identical either way.
func refCatalogFixture(t *testing.T) *bulkCatalog {
	t.Helper()
	return &bulkCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-cv", TenantID: "tenant-1", Name: "cong_viec", DataTable: "obj_cong_viec"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "parent_id", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "cong_viec"})},
		},
	}
}

func newRefSvc(t *testing.T, store *bulkTestStore) *RecordService {
	t.Helper()
	return NewRecordService(refCatalogFixture(t), store).WithSequenceClaimer(&fakeSequenceClaimer{})
}

// TestCreateBulk_Ref_ParentThenChild_ResolvesRealID is the "cha-con trong cùng lô resolve đúng id
// thật" case: two SEPARATE CreateBulk (mode=partial — the mode propose_records' Apply actually
// uses) calls, mirroring exactly how the AI Builder FE applies two "records" draft cards in
// sequence (AiBuilder.tsx applyAll). The FIRST row declares itself via "$ref"; the SECOND row's
// relation field references it by that same token and must persist the FIRST row's REAL id.
func TestCreateBulk_Ref_ParentThenChild_ResolvesRealID(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{}}
	svc := newRefSvc(t, store)
	ctx := context.Background()

	parent := []map[string]any{{"name": "Xây nhà phố Quận 9", "$ref": "@du_an_1"}}
	res, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec", parent, BulkModePartial)
	if err != nil || res.Created != 1 {
		t.Fatalf("parent CreateBulk: res=%+v err=%v", res, err)
	}
	parentID := res.Results[0].ID
	if parentID == "" {
		t.Fatalf("parent id empty: %+v", res)
	}
	// The registry only matches an EXISTING record — the child's relation-FK probe must find it,
	// exactly like a real record it points to.
	store.live["cong_viec|tenant-1|"+parentID] = true

	child := []map[string]any{{"name": "Đổ móng", "parent_id": "@du_an_1"}}
	res2, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec", child, BulkModePartial)
	if err != nil {
		t.Fatalf("child CreateBulk: %v", err)
	}
	if res2.Failed != 0 || res2.Created != 1 {
		t.Fatalf("child result = %+v; want 1 created, 0 failed", res2)
	}
	if len(store.inserted) != 2 {
		t.Fatalf("store.inserted len = %d; want 2", len(store.inserted))
	}
	if got := store.inserted[1]["parent_id"]; got != parentID {
		t.Fatalf("child.parent_id = %v; want parent's real id %q", got, parentID)
	}
}

// TestCreateBulk_Ref_DollarRefIsStrippedNeverPersisted proves the reserved "$ref" marker key
// never reaches ValidateRecord (RECORD_UNKNOWN_FIELD would otherwise fire) and never leaks into
// the persisted row.
func TestCreateBulk_Ref_DollarRefIsStrippedNeverPersisted(t *testing.T) {
	store := &bulkTestStore{}
	svc := newRefSvc(t, store)
	payloads := []map[string]any{{"name": "Xây nhà phố Quận 9", "$ref": "@du_an_1"}}

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "cong_viec", payloads, BulkModePartial)
	if err != nil || res.Failed != 0 {
		t.Fatalf("res=%+v err=%v; want a clean create ($ref must not be RECORD_UNKNOWN_FIELD)", res, err)
	}
	if _, has := store.inserted[0]["$ref"]; has {
		t.Fatalf("persisted row still carries \"$ref\": %+v", store.inserted[0])
	}
}

// TestCreateBulk_Ref_Dangling_RejectedPerRowWithFieldDetail is the "tham chiếu treo bị từ chối
// kèm số dòng" case: a relation field referencing a token nobody ever declared must fail with a
// DEDICATED code — not the generic RECORD_RELATION_INVALID a raw "@foo" string would otherwise
// hit at the DB-existence probe — and carry details.field (RM-06 convention) so the model/FE can
// point at the exact field. mode=partial reports it per-row (index-aligned), matching how a
// dangling ref inside a larger sample-data batch must not block its siblings.
func TestCreateBulk_Ref_Dangling_RejectedPerRowWithFieldDetail(t *testing.T) {
	store := &bulkTestStore{}
	svc := newRefSvc(t, store)
	payloads := []map[string]any{
		{"name": "Đổ móng", "parent_id": "@ma-chua-ton-tai"},
	}

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "cong_viec", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Created != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v; want 0 created, 1 failed", res)
	}
	row := res.Results[0]
	if row.Code != "RECORD_REF_UNRESOLVED" {
		t.Fatalf("code = %q; want RECORD_REF_UNRESOLVED", row.Code)
	}
	if row.Details["field"] != "parent_id" {
		t.Fatalf("details.field = %v; want %q", row.Details["field"], "parent_id")
	}
}

// TestCreateBulk_Ref_EntityMismatch_RejectedNotSilentlyLinked: a token that DOES resolve, but to
// a record of a DIFFERENT entity than the referencing field targets, must not silently link
// cross-entity — the whole point of tenant+entity-scoped resolution.
func TestCreateBulk_Ref_EntityMismatch_RejectedNotSilentlyLinked(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{}}
	svc := newRefSvc(t, store)
	ctx := context.Background()

	// Register a ref under a DIFFERENT tenant than the child's lookup — same effect (never
	// resolvable) as registering it under a different entity, exercised via the public surface
	// (CreateBulk), not by reaching into the registry internals.
	other := []map[string]any{{"name": "Việc của tenant khác", "$ref": "@shared_token"}}
	if _, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-OTHER", "u1", "cong_viec", other, BulkModePartial); err != nil {
		t.Fatalf("setup CreateBulk (tenant-OTHER): %v", err)
	}

	child := []map[string]any{{"name": "Đổ móng", "parent_id": "@shared_token"}}
	res, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec", child, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Failed != 1 || res.Results[0].Code != "RECORD_REF_UNRESOLVED" {
		t.Fatalf("result = %+v; want 1 failed / RECORD_REF_UNRESOLVED (tenant-scoped, never cross-tenant)", res)
	}
}

// TestCreateBulk_Ref_ValidationStillRunsFully is the "validate vẫn chạy đầy đủ (row sai kiểu vẫn
// 422 như cũ)" case: a row carrying BOTH a "$ref"/"@ref" pair AND an ordinary validation defect
// (missing required field) must still be rejected for the ORDINARY reason — resolving refs must
// never loosen services.ValidateRecord (the SAME body POST /:entity runs).
func TestCreateBulk_Ref_ValidationStillRunsFully(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{}}
	svc := newRefSvc(t, store)
	// "name" is required and absent — must fail RECORD_REQUIRED_MISSING regardless of the ref pair.
	payloads := []map[string]any{{"$ref": "@du_an_1", "parent_id": "@du_an_1"}}

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "tenant-1", "u1", "cong_viec", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Failed != 1 || res.Results[0].Code != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("result = %+v; want 1 failed / RECORD_REQUIRED_MISSING (ref must not bypass required-field validation)", res)
	}
}

// TestCreateBulk_Ref_AtomicMode_AlsoResolves: mode=atomic (the manual "Nhập nhiều dòng" batch
// form's other mode — BatchCreateForm.tsx) shares the SAME registry, so it is not a silent gap
// even though propose_records' own Apply path only ever uses mode=partial.
func TestCreateBulk_Ref_AtomicMode_AlsoResolves(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{}}
	inserter := &fakeBulkInserter{}
	svc := newRefSvc(t, store).WithBulkInserter(inserter)
	ctx := context.Background()

	parent := []map[string]any{{"name": "Xây nhà phố Quận 9", "$ref": "@du_an_1"}}
	res, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec", parent, BulkModeAtomic)
	if err != nil || res.Created != 1 {
		t.Fatalf("parent CreateBulk (atomic): res=%+v err=%v", res, err)
	}
	parentID := res.Results[0].ID
	store.live["cong_viec|tenant-1|"+parentID] = true

	child := []map[string]any{{"name": "Đổ móng", "parent_id": "@du_an_1"}}
	res2, err := svc.CreateBulk(ctx, bulkAdminScope(), "tenant-1", "u1", "cong_viec", child, BulkModeAtomic)
	if err != nil || res2.Created != 1 {
		t.Fatalf("child CreateBulk (atomic): res=%+v err=%v", res2, err)
	}
	// Atomic mode inserts via InsertRecordsAtomic (fakeBulkInserter), not store.InsertRecord — its
	// captured rows are the ones to check for the resolved id.
	if got := inserter.lastRows[0]["parent_id"]; got != parentID {
		t.Fatalf("child.parent_id = %v; want parent's real id %q", got, parentID)
	}
}

// --- USER-FIELD parity on the atomic path (dual-review 2026-09-01 HIGH) ---------------------

// bulkUserStore embeds bulkTestStore and adds BatchActiveUserExists, so NewRecordService's
// userExister auto-wire kicks in — same wiring shape production gets from *dynrepo.Repository.
type bulkUserStore struct {
	bulkTestStore
	activeUsers map[string]bool // key: "tenant|id"
	userCalls   int
}

func (s *bulkUserStore) BatchActiveUserExists(_ context.Context, tenantID string, ids []string) (map[string]bool, error) {
	s.userCalls++
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = s.activeUsers[tenantID+"|"+id]
	}
	return out, nil
}

func TestCreateBulk_Atomic_UserFieldDangling_RowFailsAndBatchAborts(t *testing.T) {
	store := &bulkUserStore{activeUsers: map[string]bool{"t1|u-alive": true}}
	claimer := &fakeSequenceClaimer{}
	inserter := &fakeBulkInserter{}
	svc := NewRecordService(bulkCatalogFixture(t), store).WithSequenceClaimer(claimer).WithBulkInserter(inserter)

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "ok", "assignee": "u-alive"},
		{"name": "bad", "assignee": "u-ghost"}, // dangling — single POST rejects this with 422
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v", err)
	}
	if res.Created != 0 || inserter.calls != 0 || claimer.calls != 0 {
		t.Fatalf("atomic abort must write/claim nothing: created=%d inserts=%d claims=%d", res.Created, inserter.calls, claimer.calls)
	}
	if got := res.Results[1].Code; got != "RECORD_USER_INVALID" {
		t.Fatalf("row1 code = %q; want RECORD_USER_INVALID", got)
	}
	if got := res.Results[0].Code; got != "BULK_ATOMIC_ABORTED" {
		t.Fatalf("row0 (valid) code = %q; want BULK_ATOMIC_ABORTED", got)
	}
	if store.userCalls != 1 {
		t.Fatalf("BatchActiveUserExists calls = %d; want 1 (one probe per batch)", store.userCalls)
	}
}

func TestCreateBulk_Atomic_UserFieldValid_Passes(t *testing.T) {
	store := &bulkUserStore{activeUsers: map[string]bool{"t1|u-alive": true}}
	svc := NewRecordService(bulkCatalogFixture(t), store).WithSequenceClaimer(&fakeSequenceClaimer{}).WithBulkInserter(&fakeBulkInserter{})

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "ok", "assignee": "u-alive"},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v", err)
	}
	if res.Created != 1 || res.Failed != 0 {
		t.Fatalf("created=%d failed=%d; want 1/0", res.Created, res.Failed)
	}
}

func TestCreateBulk_Atomic_UserStoreNil_FailsClosedPerRow(t *testing.T) {
	// bulkTestStore does NOT implement userExister -> s.userStore stays nil. A row carrying a
	// user value must fail CLOSED (RECORD_USER_UNAVAILABLE), never persist unverified.
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "x", "assignee": "u-any"},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v", err)
	}
	if res.Created != 0 || res.Results[0].Code != "RECORD_USER_UNAVAILABLE" {
		t.Fatalf("created=%d code=%q; want 0/RECORD_USER_UNAVAILABLE", res.Created, res.Results[0].Code)
	}
}

// --- polymorphic bulk probe coverage (dual-review 2026-09-01 MEDIUM: had zero tests) --------

func TestCreateBulk_Atomic_PolymorphicDangling_RowFails(t *testing.T) {
	store := &bulkTestStore{live: map[string]bool{"accounts|t1|acc-1": true}}
	inserter := &fakeBulkInserter{}
	svc := newBulkSvc(store, &fakeSequenceClaimer{}, inserter, nil, t)

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "ok", "related_to": map[string]any{"type": "accounts", "id": "acc-1"}},
		{"name": "bad", "related_to": map[string]any{"type": "accounts", "id": "acc-ghost"}},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v", err)
	}
	if res.Created != 0 || inserter.calls != 0 {
		t.Fatalf("atomic abort expected: created=%d inserts=%d", res.Created, inserter.calls)
	}
	if got := res.Results[1].Code; got != "RECORD_POLYMORPHIC_TARGET_INVALID" {
		t.Fatalf("row1 code = %q; want RECORD_POLYMORPHIC_TARGET_INVALID", got)
	}
}

// --- deadlock-retry loop coverage (dual-review 2026-09-01 MEDIUM) ---------------------------

func TestCreateBulk_Atomic_TransientDeadlock_RetriesThenSucceeds(t *testing.T) {
	inserter := &fakeBulkInserter{failTimes: 1, failErr: dynrepo.ErrWriteConflict}
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, inserter, nil, t)

	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "a"}, {"name": "b"},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v (transient conflict must be retried, not surfaced)", err)
	}
	if inserter.calls != 2 {
		t.Fatalf("insert calls = %d; want 2 (fail once, retry once)", inserter.calls)
	}
	if res.Created != 2 || res.Failed != 0 {
		t.Fatalf("created=%d failed=%d; want 2/0", res.Created, res.Failed)
	}
}

// --- "$ref" non-string is a per-row error, not a silent swallow (dual-review LOW) -----------

func TestCreateBulk_Atomic_RefNonString_RowFailsWithRefInvalid(t *testing.T) {
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t)
	res, err := svc.CreateBulk(context.Background(), bulkAdminScope(), "t1", "u1", "widgets", []map[string]any{
		{"name": "x", "$ref": float64(7)},
	}, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk err = %v", err)
	}
	if res.Created != 0 || res.Results[0].Code != "RECORD_REF_INVALID" {
		t.Fatalf("created=%d code=%q; want 0/RECORD_REF_INVALID", res.Created, res.Results[0].Code)
	}
}
