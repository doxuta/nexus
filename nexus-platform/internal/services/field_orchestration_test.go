package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeFieldSchema records the dynamic-schema (ALTER) calls AddField makes, so tests can
// assert that ordinary fields touch no schema and unique fields ALTER once — plus the
// compensation drop when metadata persistence fails after a successful ALTER. It also
// answers ColumnExists so the re-add precheck (B-1) can be exercised without a real DB.
type fakeFieldSchema struct {
	added   []string // field names passed to AddUniqueGencolNonUnique
	dropped []string // field names passed to DropUniqueGencolNonUnique (compensation)
	addErr  error

	relAdded   []string // field names passed to AddRelationColumn
	relDropped []string // field names passed to DropRelationColumn (compensation)
	relAddErr  error

	// ENG-REL2 #5: 1-1 relation tracking, mirroring the 1-N (rel*) fields above in separate
	// slices so a test can assert which pair (1-N vs 1-1) AddField actually used.
	oneToOneAdded   []string // field names passed to AddOneToOneRelationColumn
	oneToOneDropped []string // field names passed to DropOneToOneRelationColumn (compensation)
	oneToOneAddErr  error

	colExists    bool     // value ColumnExists returns (does the physical column already exist?)
	colExistsErr error    // error ColumnExists returns
	colChecked   []string // field names passed to ColumnExists (asserts the precheck ran)

	// D1 indexed-column tracking.
	idxAdded   []string // field names passed to AddIndexedColumn
	idxDropped []string // field names passed to DropIndexedColumn (compensation)
	idxAddErr  error

	// ACTIVITY-CORE #3a polymorphic-column tracking (physical cols `<field>_type` + `<field>_id`).
	polyAdded   []string // field names passed to AddPolymorphicColumns
	polyDropped []string // field names passed to DropPolymorphicColumns (compensation)
	polyAddErr  error

	// RM-28 A2: backfill tracking. backfillCalled records field names passed to
	// BackfillUniqueField; backfillErr/backfillConflicts let a test simulate a hard failure or
	// an intra-tenant dupe report. Zero-value (nil/nil) is success-with-nothing-to-backfill, so
	// every pre-existing unique-field test keeps passing unchanged.
	backfillCalled    []string
	backfillErr       error
	backfillConflicts []dynrepo.BackfillConflict
	// pivotDeleted records field names passed to DeletePivotByField (the compensation path).
	pivotDeleted []string

	// queue #157: CountCurrencyCodeRows answers from currencyRows (stored code → live rows) and
	// currencyTrashed (stored code → recycle-bin rows), and records each call as
	// "entity.field=code@tenant".
	currencyRows    map[string]int64
	currencyTrashed map[string]int64
	currencyErr     error
	currencyCalls   []string
}

func (f *fakeFieldSchema) CountCurrencyCodeRows(_ context.Context, entity, tenantID, field, code string) (int64, int64, error) {
	f.currencyCalls = append(f.currencyCalls, entity+"."+field+"="+code+"@"+tenantID)
	if f.currencyErr != nil {
		return 0, 0, f.currencyErr
	}
	return f.currencyRows[code], f.currencyTrashed[code], nil
}

func (f *fakeFieldSchema) DeletePivotByField(_ context.Context, _, _, field string) error {
	f.pivotDeleted = append(f.pivotDeleted, field)
	return nil
}

func (f *fakeFieldSchema) BackfillUniqueField(_ context.Context, _, _, field string) (*dynrepo.BackfillResult, error) {
	f.backfillCalled = append(f.backfillCalled, field)
	if f.backfillErr != nil {
		return nil, f.backfillErr
	}
	return &dynrepo.BackfillResult{Conflicts: f.backfillConflicts}, nil
}

func (f *fakeFieldSchema) AddUniqueGencolNonUnique(_ context.Context, _, field string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, field)
	return nil
}

func (f *fakeFieldSchema) DropUniqueGencolNonUnique(_ context.Context, _, field string) error {
	f.dropped = append(f.dropped, field)
	return nil
}

func (f *fakeFieldSchema) AddRelationColumn(_ context.Context, _, field string) error {
	if f.relAddErr != nil {
		return f.relAddErr
	}
	f.relAdded = append(f.relAdded, field)
	return nil
}

func (f *fakeFieldSchema) DropRelationColumn(_ context.Context, _, field string) error {
	f.relDropped = append(f.relDropped, field)
	return nil
}

// AddOneToOneRelationColumn / DropOneToOneRelationColumn satisfy the extended
// FieldSchemaRepo interface (ENG-REL2 #5).
func (f *fakeFieldSchema) AddOneToOneRelationColumn(_ context.Context, _, field string) error {
	if f.oneToOneAddErr != nil {
		return f.oneToOneAddErr
	}
	f.oneToOneAdded = append(f.oneToOneAdded, field)
	return nil
}

func (f *fakeFieldSchema) DropOneToOneRelationColumn(_ context.Context, _, field string) error {
	f.oneToOneDropped = append(f.oneToOneDropped, field)
	return nil
}

func (f *fakeFieldSchema) ColumnExists(_ context.Context, _, field string) (bool, error) {
	f.colChecked = append(f.colChecked, field)
	if f.colExistsErr != nil {
		return false, f.colExistsErr
	}
	return f.colExists, nil
}

func (f *fakeFieldSchema) AddIndexedColumn(_ context.Context, _, field, _ string) error {
	if f.idxAddErr != nil {
		return f.idxAddErr
	}
	f.idxAdded = append(f.idxAdded, field)
	return nil
}

func (f *fakeFieldSchema) DropIndexedColumn(_ context.Context, _, field string) error {
	f.idxDropped = append(f.idxDropped, field)
	return nil
}

// AddPolymorphicColumns / DropPolymorphicColumns satisfy the extended FieldSchemaRepo interface
// (ACTIVITY-CORE #3a).
func (f *fakeFieldSchema) AddPolymorphicColumns(_ context.Context, _, field string) error {
	if f.polyAddErr != nil {
		return f.polyAddErr
	}
	f.polyAdded = append(f.polyAdded, field)
	return nil
}

func (f *fakeFieldSchema) DropPolymorphicColumns(_ context.Context, _, field string) error {
	f.polyDropped = append(f.polyDropped, field)
	return nil
}

// fakeFieldMeta stubs the metadata reads/writes AddField depends on: entity lookup
// (tenant-scoped), the entity's existing fields, and the field-create write.
//
// existing holds ACTIVE fields (returned by ListFields, GORM hides soft-deleted rows).
// includingDeleted holds ALL fields incl. soft-deleted ones (returned by
// ListFieldsIncludingDeleted, the Unscoped read the dup-check uses for B-1). When
// includingDeleted is nil it falls back to existing, so older tests that only set
// existing keep their meaning.
type fakeFieldMeta struct {
	entity           *metadata.EntityDefinition
	entityErr        error
	existing         []metadata.FieldDefinition
	includingDeleted []metadata.FieldDefinition
	created          []metadata.FieldDefinition
	createErr        error
	// entitiesByName, when set, resolves GetEntityByName per name (used by relation tests
	// where the host entity and the FK target entity are different lookups). A name absent
	// from the map resolves to metadata.ErrEntityNotFound. When nil, every lookup returns
	// `entity`/`entityErr` (older single-entity tests keep their meaning).
	entitiesByName map[string]*metadata.EntityDefinition

	// fieldsByEntityID backs ListFields for the ENG-COMPUTED #3 (rollup) cross-entity checks —
	// keyed by the target *metadata.EntityDefinition.ID (as set in entitiesByName fixtures).
	fieldsByEntityID map[string][]metadata.FieldDefinition
	// relationshipsByName backs GetRelationshipByName for the N-N rollup cross-entity check. A
	// name absent from the map returns metadata.ErrRelationshipNotFound.
	relationshipsByName map[string]*metadata.RelationshipDefinition

	// claimedInverse backs GetRelationshipByInverseField (ENG-REL2 #4), keyed by
	// "<parentEntityName>|<inverseField>". A key absent from the map means the name is
	// available (metadata.ErrRelationshipNotFound). Tests seed this to exercise collisions.
	claimedInverse map[string]*metadata.RelationshipDefinition
	// createdWithRel records the (field, relationship) pairs passed to
	// CreateFieldWithRelationship — the 1-N registry write path for a `relation` field.
	createdWithRel []struct {
		field metadata.FieldDefinition
		rel   metadata.RelationshipDefinition
	}
	// deletedRelChildFK records "<childEntityName>|<childFKColumn>" pairs passed to
	// SoftDeleteRelationshipByChildFK (the DeleteField registry-cleanup path).
	deletedRelChildFK []string
	deleteRelErr      error
}

func (f *fakeFieldMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if f.entitiesByName != nil {
		e, ok := f.entitiesByName[name]
		if !ok {
			return nil, metadata.ErrEntityNotFound
		}
		return e, nil
	}
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.entity, nil
}

func (f *fakeFieldMeta) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	// fieldsByEntityID, when set, resolves per target entity (needed by the ENG-COMPUTED #3
	// rollup cross-entity check, which reads a DIFFERENT — child — entity's fields, not the
	// host entity's `existing`). Falls back to `existing` so pre-existing AddField tests (which
	// never set fieldsByEntityID) keep their prior meaning.
	if f.fieldsByEntityID != nil {
		return f.fieldsByEntityID[entityID], nil
	}
	return f.existing, nil
}

func (f *fakeFieldMeta) GetRelationshipByName(_ context.Context, _, name string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.relationshipsByName[name]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

// GetRelationshipByInverseField satisfies the extended FieldMetadataRepo interface (ENG-REL2
// #4). Looks up claimedInverse by "<parent>|<inverseField>"; a miss means the name is free.
func (f *fakeFieldMeta) GetRelationshipByInverseField(_ context.Context, _, parentEntityName, inverseField string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.claimedInverse[parentEntityName+"|"+inverseField]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

// CreateFieldWithRelationship satisfies the extended FieldMetadataRepo interface (ENG-REL2
// #4): the atomic field+registry write for a `relation`-type field. Reuses createErr (the
// SAME knob CreateField honors) so existing tests that simulate "the metadata write fails" via
// meta.createErr keep exercising the relation-field compensation path unchanged. Records both
// halves on success so a test can assert the built RelationshipDefinition
// (name/parent/child/inverse/fk).
func (f *fakeFieldMeta) CreateFieldWithRelationship(_ context.Context, fd *metadata.FieldDefinition, rel *metadata.RelationshipDefinition) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, *fd)
	f.createdWithRel = append(f.createdWithRel, struct {
		field metadata.FieldDefinition
		rel   metadata.RelationshipDefinition
	}{*fd, *rel})
	return nil
}

// SoftDeleteRelationshipByChildFK satisfies the extended FieldMetadataRepo interface
// (ENG-REL2 #4). The AddField/EditField tests never call it; DeleteField tests use their own
// fakeDeleteMeta, so this is inert here.
func (f *fakeFieldMeta) SoftDeleteRelationshipByChildFK(_ context.Context, tenantID, childEntityName, childFKColumn string) error {
	if f.deleteRelErr != nil {
		return f.deleteRelErr
	}
	f.deletedRelChildFK = append(f.deletedRelChildFK, childEntityName+"|"+childFKColumn)
	return nil
}

func (f *fakeFieldMeta) ListFieldsIncludingDeleted(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	if f.includingDeleted != nil {
		return f.includingDeleted, nil
	}
	return f.existing, nil
}

func (f *fakeFieldMeta) CreateField(_ context.Context, fd *metadata.FieldDefinition) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, *fd)
	return nil
}

// GetFieldByID / UpdateFieldMutable satisfy the extended FieldMetadataRepo interface for
// the AddField tests; the AddField path never calls them, so they are inert here.
func (f *fakeFieldMeta) GetFieldByID(_ context.Context, _, _ string) (*metadata.FieldDefinition, error) {
	return nil, metadata.ErrFieldNotFound
}

func (f *fakeFieldMeta) UpdateFieldMutable(_ context.Context, _ string, _ *metadata.FieldDefinition) error {
	return nil
}

func (f *fakeFieldMeta) SoftDeleteField(_ context.Context, _, _ string) error {
	return nil
}

// GetEntityNameByID satisfies the extended FieldMetadataRepo interface for EditField (D1
// indexed toggle). Returns the entity's Name from the stored entity fixture, or
// ErrEntityNotFound when no entity is configured.
func (f *fakeFieldMeta) GetEntityNameByID(_ context.Context, _, _ string) (string, error) {
	if f.entity != nil {
		return f.entity.Name, nil
	}
	return "", metadata.ErrEntityNotFound
}

func entityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{
		ID:        "ent-1",
		TenantID:  "tenant-1",
		Name:      "courses",
		DataTable: "obj_courses",
	}
}

func ordinaryField() AddFieldInput {
	return AddFieldInput{
		EntityName: "courses",
		Field:      FieldInput{Name: "summary", Label: "Tóm tắt", FieldType: "text"},
	}
}

func uniqueField() AddFieldInput {
	return AddFieldInput{
		EntityName: "courses",
		Field:      FieldInput{Name: "sku", Label: "SKU", FieldType: "text", IsUnique: true},
	}
}

func TestAddField_OrdinaryFieldNoSchemaChange(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", ordinaryField())
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if len(schema.added) != 0 {
		t.Errorf("ordinary field must NOT alter schema; added=%v", schema.added)
	}
	if len(meta.created) != 1 || meta.created[0].Name != "summary" {
		t.Errorf("metadata field not created correctly; created=%v", meta.created)
	}
	if fd.EntityID != "ent-1" {
		t.Errorf("EntityID = %q; want ent-1", fd.EntityID)
	}
}

func TestAddField_UniqueFieldAltersThenPersists(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", uniqueField()); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if len(schema.added) != 1 || schema.added[0] != "sku" {
		t.Errorf("unique field should ALTER once for sku; added=%v", schema.added)
	}
	if len(meta.created) != 1 || !meta.created[0].IsUnique {
		t.Errorf("unique field metadata not persisted; created=%v", meta.created)
	}
	if len(schema.dropped) != 0 {
		t.Errorf("no compensation on happy path; dropped=%v", schema.dropped)
	}
}

func TestAddField_CompensatesWhenMetaFailsAfterAlter(t *testing.T) {
	// Unique field: ALTER auto-commits, then metadata write fails -> must DROP the column
	// so no orphan generated column/index is left behind.
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture(), createErr: errors.New("meta boom")}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", uniqueField()); err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.added) != 1 {
		t.Errorf("ALTER should have run before metadata; added=%v", schema.added)
	}
	if len(schema.dropped) != 1 || schema.dropped[0] != "sku" {
		t.Errorf("metadata failure must DROP the added column; dropped=%v", schema.dropped)
	}
}

func TestAddField_OrdinaryFieldNoCompensationNeeded(t *testing.T) {
	// Ordinary field: no DDL ran, so a metadata failure has nothing to compensate.
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture(), createErr: errors.New("meta boom")}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", ordinaryField()); err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.dropped) != 0 {
		t.Errorf("no DDL ran for ordinary field; nothing to drop. dropped=%v", schema.dropped)
	}
}

func TestAddField_UnknownEntityIsNotFound(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entityErr: metadata.ErrEntityNotFound}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", ordinaryField())
	if codeOf(err) != "ENTITY_NOT_FOUND" {
		t.Fatalf("code = %q; want ENTITY_NOT_FOUND", codeOf(err))
	}
	if len(schema.added) != 0 {
		t.Errorf("no DDL for unknown entity; added=%v", schema.added)
	}
}

func TestAddField_DuplicateFieldBlocksBeforeDDL(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{
		entity:   entityFixture(),
		existing: []metadata.FieldDefinition{{Name: "sku"}},
	}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", uniqueField())
	if codeOf(err) != "FIELD_ALREADY_EXISTS" {
		t.Fatalf("code = %q; want FIELD_ALREADY_EXISTS", codeOf(err))
	}
	if len(schema.added) != 0 {
		t.Errorf("duplicate field must not run DDL; added=%v", schema.added)
	}
	if len(meta.created) != 0 {
		t.Errorf("duplicate field must not persist metadata; created=%v", meta.created)
	}
}

// TestAddField_FieldLimitExceeded: AddField must enforce maxFieldsPerEntity against the
// entity's LIVE field count — CreateEntity only caps a single declared list, so repeated
// AddField calls could otherwise grow an entity past the cap without bound. Soft-deleted
// rows do not count (their metadata slot is gone; only the physical column lingers).
func TestAddField_FieldLimitExceeded(t *testing.T) {
	full := make([]metadata.FieldDefinition, 0, maxFieldsPerEntity+1)
	for i := 0; i < maxFieldsPerEntity; i++ {
		full = append(full, metadata.FieldDefinition{Name: fmt.Sprintf("f_%03d", i)})
	}
	full = append(full, metadata.FieldDefinition{
		Name:      "ghost",
		DeletedAt: gorm.DeletedAt{Time: time.Now(), Valid: true},
	})

	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture(), includingDeleted: full}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", ordinaryField())
	if codeOf(err) != "FIELD_LIMIT_EXCEEDED" {
		t.Fatalf("code = %q; want FIELD_LIMIT_EXCEEDED", codeOf(err))
	}
	if len(schema.added) != 0 || len(meta.created) != 0 {
		t.Errorf("over-limit add must not touch schema/metadata; added=%v created=%v", schema.added, meta.created)
	}

	// 199 live + 1 soft-deleted ghost = under the cap; the add must still be allowed.
	underMeta := &fakeFieldMeta{entity: entityFixture(), includingDeleted: full[1:]}
	if _, err := NewFieldService(&fakeFieldSchema{}, underMeta).AddField(context.Background(), "tenant-1", ordinaryField()); err != nil {
		t.Fatalf("add at maxFieldsPerEntity-1 live fields must succeed: %v", err)
	}
}

func TestAddField_ValidationBlocksBeforeLookup(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := ordinaryField()
	in.Field.Name = "Bad Name" // invalid identifier
	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_INVALID_NAME" {
		t.Fatalf("code = %q; want FIELD_INVALID_NAME", codeOf(err))
	}
	if len(schema.added) != 0 || len(meta.created) != 0 {
		t.Errorf("invalid field must not touch schema/metadata; added=%v created=%v", schema.added, meta.created)
	}
}

// --- B-1: re-add a field after soft-delete (spec §7.2) -----------------------------------

// TestAddField_SoftDeletedDuplicateBlocked is the core B-1 acceptance. A field 'sku' was
// added then soft-deleted: it no longer appears in the ACTIVE list (existing is empty), but
// IS present in the include-deleted list. Re-adding it must be rejected with
// FIELD_ALREADY_EXISTS (409 upstream) — NOT silently create a second sys_fields row, and NOT
// run any DDL (which on a unique field would ALTER an already-existing column -> 500).
func TestAddField_SoftDeletedDuplicateBlocked(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{
		entity:   entityFixture(),
		existing: nil, // active list hides the soft-deleted field (GORM behavior)
		includingDeleted: []metadata.FieldDefinition{
			{Name: "sku"}, // soft-deleted row, only visible via the Unscoped read
		},
	}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", uniqueField())
	if codeOf(err) != "FIELD_ALREADY_EXISTS" {
		t.Fatalf("re-add of soft-deleted field: code = %q; want FIELD_ALREADY_EXISTS", codeOf(err))
	}
	if len(schema.added) != 0 {
		t.Errorf("soft-deleted duplicate must not run DDL; added=%v", schema.added)
	}
	if len(meta.created) != 0 {
		t.Errorf("soft-deleted duplicate must not persist a second row; created=%v", meta.created)
	}
}

// TestAddField_UniqueChecksColumnBeforeAlter asserts the physical-column precheck (B-1) runs
// before the ALTER on the normal unique-add path: the column does not yet exist, so the
// precheck is consulted and then AddUniqueGencolNonUnique runs exactly once.
func TestAddField_UniqueChecksColumnBeforeAlter(t *testing.T) {
	schema := &fakeFieldSchema{colExists: false}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", uniqueField()); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if len(schema.colChecked) != 1 || schema.colChecked[0] != "sku" {
		t.Errorf("unique add must check the physical column before ALTER; checked=%v", schema.colChecked)
	}
	if len(schema.added) != 1 || schema.added[0] != "sku" {
		t.Errorf("unique add should ALTER once when the column is absent; added=%v", schema.added)
	}
}

// TestAddField_UniqueReAddSkipsAlterWhenColumnExists is the defense-in-depth half of B-1:
// even if a unique field's metadata row is absent (so the dup-check passes) but its physical
// generated column still exists from a prior life (soft-delete keeps the column), AddField
// must NOT issue ALTER ADD COLUMN on the existing column (which would 500). It detects the
// column via the precheck, skips the ALTER, and still persists the metadata row. No
// compensation drop must run — the column was pre-existing and is reused, not owned by us.
func TestAddField_UniqueReAddSkipsAlterWhenColumnExists(t *testing.T) {
	schema := &fakeFieldSchema{colExists: true}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", uniqueField())
	if err != nil {
		t.Fatalf("AddField with pre-existing column: %v", err)
	}
	if len(schema.colChecked) != 1 || schema.colChecked[0] != "sku" {
		t.Errorf("precheck must consult the physical column; checked=%v", schema.colChecked)
	}
	if len(schema.added) != 0 {
		t.Errorf("must NOT ALTER an already-existing column; added=%v", schema.added)
	}
	if len(meta.created) != 1 || !meta.created[0].IsUnique {
		t.Errorf("metadata row must still be persisted; created=%v", meta.created)
	}
	if fd == nil || !fd.IsUnique {
		t.Errorf("returned field def should be the persisted unique field; got %v", fd)
	}
}

// TestAddField_UniqueColumnPrecheckErrorSurfaces: a failure of the physical-column precheck
// is surfaced (wrapped), not swallowed, and no ALTER runs.
func TestAddField_UniqueColumnPrecheckErrorSurfaces(t *testing.T) {
	schema := &fakeFieldSchema{colExistsErr: errors.New("information_schema boom")}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", uniqueField()); err == nil {
		t.Fatal("expected error when the column precheck fails")
	}
	if len(schema.added) != 0 {
		t.Errorf("precheck failure must not ALTER; added=%v", schema.added)
	}
}

// TestAddField_AutoNumberFieldNoRelationshipRegistry locks the "registry/metadata untouched"
// contract from the ENG #6 spec: auto_number is NOT a relation, so AddField must persist it via
// the plain CreateField path (mirrors TestAddField_OrdinaryFieldNoSchemaChange) and must NEVER
// call CreateFieldWithRelationship / write a sys_relationships row.
func TestAddField_AutoNumberFieldNoRelationshipRegistry(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number",
			Options: map[string]any{"prefix": "INV-", "padding": float64(5)}},
	}
	if _, err := svc.AddField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if len(schema.added) != 0 {
		t.Errorf("auto_number (non-unique, non-indexed) must NOT alter schema; added=%v", schema.added)
	}
	if len(meta.created) != 1 || meta.created[0].Name != "invoice_no" {
		t.Errorf("metadata field not created correctly via plain CreateField; created=%v", meta.created)
	}
	if len(meta.createdWithRel) != 0 {
		t.Errorf("auto_number must never write a sys_relationships registry row; createdWithRel=%v", meta.createdWithRel)
	}
}

// Adding a UNIQUE auto_number field must take the UNIQUE DDL branch — the shape check, not just
// the "no error" check.
//
// The old rule (see validateAutoNumberFieldFlags) forbade this outright; RM-28's per-tenant pivot
// retired its reason. What must be true now is that the field materializes like a unique `text`
// field — a generated column over data.<field> plus the pivot — because auto_number's value IS a
// string living there. This is deliberately UNLIKE relation/user, which stay non-unique forever:
// those need a CHAR(36) <field>_id FK column, so the unique branch would build the wrong shape.
func TestAddField_AutoNumberUniqueTakesUniqueBranch(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := AddFieldInput{
		EntityName: "courses",
		Field:      FieldInput{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number", IsUnique: true},
	}
	if _, err := svc.AddField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("unique auto_number must be accepted since RM-28; got %v", err)
	}
	if len(meta.created) != 1 {
		t.Fatalf("metadata row not written: created=%v", meta.created)
	}
	// `added` records AddUniqueGencolNonUnique calls — i.e. the UNIQUE branch. Asserting on the
	// DDL actually issued (not merely on the absence of an error) is what pins the SHAPE.
	if len(schema.added) != 1 || schema.added[0] != "invoice_no" {
		t.Errorf("expected the UNIQUE gencol branch for invoice_no; added=%v rel=%v", schema.added, schema.relAdded)
	}
}
