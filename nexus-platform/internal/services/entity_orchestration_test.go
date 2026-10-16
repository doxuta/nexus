package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeSchema records DDL calls so tests can assert ordering and compensation without a DB.
type fakeSchema struct {
	created   []string
	dropped   []string
	createErr error
	existed   bool // reported by TableExists (table pre-exists)

	// Reconcile path (bug3). existingCols is the set of physical columns the SHARED obj_*
	// table already has; ColumnExists consults it. addedUnique / addedRelation record the
	// reconcile ALTERs so a test can assert a missing column was added on a pre-existing
	// table. Keys/elements are the field name (relation records the bare field name, as
	// AddRelationColumn takes it; its physical column is `<field>_id`).
	existingCols  map[string]bool
	addedUnique   []string
	addedRelation []string
	addedOneToOne []string // ENG-REL2 #5: field names passed to AddOneToOneRelationColumn

	// addUniqueErr, when set for a field name, makes AddUniqueGencolNonUnique return that
	// error for that field — RM-28 Slice B: the non-unique index can only ever return
	// ErrDuplicateKeyName (1061 DDL race), never ErrDuplicateUnique (1062) — kept for that case.
	addUniqueErr map[string]error
	// addOneToOneErr, when set for a field name, makes AddOneToOneRelationColumn return that
	// error for that field — used to simulate ER_DUP_ENTRY (ErrDuplicateUnique, a 1-1
	// cardinality violation on a shared table) or ER_DUP_KEYNAME (RM-25 DDL race).
	addOneToOneErr map[string]error
	// droppedUnique / droppedRelation / droppedOneToOne record compensation Drop* calls so a
	// test can assert the reconcile ALTERs this call made were rolled back (and only those).
	droppedUnique   []string
	droppedRelation []string
	droppedOneToOne []string
	// ACTIVITY-CORE #3a: field names passed to Add/DropPolymorphicColumns (physical columns are
	// `<field>_type` + `<field>_id`).
	addedPolymorphic   []string
	droppedPolymorphic []string
}

func (f *fakeSchema) TableExists(_ context.Context, _ string) (bool, error) {
	return f.existed, nil
}

func (f *fakeSchema) CreateTable(_ context.Context, entity string, _ []ddl.FieldDef) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, entity)
	return nil
}

func (f *fakeSchema) DropTable(_ context.Context, entity string) error {
	f.dropped = append(f.dropped, entity)
	return nil
}

// SoftDeleteRecordsAt satisfies the SchemaRepo interface (RM-03 data-bleed guard). The
// orchestration tests here don't exercise entity-delete, so this is an inert stub; the real
// behavior is covered by the integration test TestRM03_RecreateSameName.
func (f *fakeSchema) SoftDeleteRecordsAt(_ context.Context, _, _ string, _ time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeSchema) ColumnExists(_ context.Context, _, field string) (bool, error) {
	return f.existingCols[field], nil
}

func (f *fakeSchema) DeletePivotByField(_ context.Context, _, _, _ string) error { return nil }

func (f *fakeSchema) AddUniqueGencolNonUnique(_ context.Context, _, field string) error {
	if f.addUniqueErr != nil {
		if err, ok := f.addUniqueErr[field]; ok {
			return err
		}
	}
	f.addedUnique = append(f.addedUnique, field)
	if f.existingCols == nil {
		f.existingCols = map[string]bool{}
	}
	f.existingCols[field] = true
	return nil
}

func (f *fakeSchema) AddRelationColumn(_ context.Context, _, field string) error {
	f.addedRelation = append(f.addedRelation, field)
	if f.existingCols == nil {
		f.existingCols = map[string]bool{}
	}
	f.existingCols[field+"_id"] = true
	return nil
}

func (f *fakeSchema) DropUniqueGencolNonUnique(_ context.Context, _, field string) error {
	f.droppedUnique = append(f.droppedUnique, field)
	return nil
}

func (f *fakeSchema) DropRelationColumn(_ context.Context, _, field string) error {
	f.droppedRelation = append(f.droppedRelation, field)
	return nil
}

// AddOneToOneRelationColumn / DropOneToOneRelationColumn satisfy the extended SchemaRepo
// interface (ENG-REL2 #5). Mirror AddRelationColumn/DropRelationColumn's bookkeeping in a
// separate set of slices so a test can assert the 1-1 pair (not the 1-N pair) was used.
func (f *fakeSchema) AddOneToOneRelationColumn(_ context.Context, _, field string) error {
	if f.addOneToOneErr != nil {
		if err, ok := f.addOneToOneErr[field]; ok {
			return err
		}
	}
	f.addedOneToOne = append(f.addedOneToOne, field)
	if f.existingCols == nil {
		f.existingCols = map[string]bool{}
	}
	f.existingCols[field+"_id"] = true
	return nil
}

func (f *fakeSchema) DropOneToOneRelationColumn(_ context.Context, _, field string) error {
	f.droppedOneToOne = append(f.droppedOneToOne, field)
	return nil
}

// AddIndexedColumn / DropIndexedColumn satisfy the extended SchemaRepo interface (D1).
// They are inert stubs in the existing entity-orchestration tests; dedicated D1 tests
// exercise them directly.
func (f *fakeSchema) AddIndexedColumn(_ context.Context, _, field, _ string) error {
	return nil
}

// AddPolymorphicColumns / DropPolymorphicColumns satisfy the extended SchemaRepo interface
// (ACTIVITY-CORE #3a). They track the field so reconcile/compensation tests can assert the pair
// was used, and register BOTH physical columns so ColumnExists sees them thereafter.
func (f *fakeSchema) AddPolymorphicColumns(_ context.Context, _, field string) error {
	f.addedPolymorphic = append(f.addedPolymorphic, field)
	if f.existingCols == nil {
		f.existingCols = map[string]bool{}
	}
	f.existingCols[field+"_type"] = true
	f.existingCols[field+"_id"] = true
	return nil
}

func (f *fakeSchema) DropPolymorphicColumns(_ context.Context, _, field string) error {
	f.droppedPolymorphic = append(f.droppedPolymorphic, field)
	return nil
}

func (f *fakeSchema) DropIndexedColumn(_ context.Context, _, field string) error {
	return nil
}

// fakeMeta lets a test force metadata persistence to fail, triggering compensation,
// or report an existing entity name.
type fakeMeta struct {
	claimedByOther bool  // another tenant owns obj_<name> by the time compensation runs
	claimErr       error // the ownership lookup itself failed
	called         bool
	err            error
	exists         bool

	// knownEntities is the set of entity names GetEntityByName resolves successfully (the
	// relation-target check uses it). A name not in the set returns metadata.ErrEntityNotFound.
	knownEntities map[string]bool
	// getEntityErr, when non-nil, is returned by GetEntityByName regardless of name (used to
	// exercise the non-not-found error branch).
	getEntityErr error

	// fieldsByEntityName backs ListFields for the ENG-COMPUTED #3 (rollup) cross-entity checks:
	// GetEntityByName resolves a known entity to &EntityDefinition{Name:name, ID:name} (ID
	// mirrors Name for a stable, predictable lookup key in tests), and ListFields keys off that
	// same ID. A name absent from the map returns an empty field list (not an error).
	fieldsByEntityName map[string][]metadata.FieldDefinition
	// relationshipsByName backs GetRelationshipByName for the N-N rollup cross-entity check. A
	// name absent from the map returns metadata.ErrRelationshipNotFound.
	relationshipsByName map[string]*metadata.RelationshipDefinition
	// claimedInverse backs GetRelationshipByInverseField (ENG-REL2 #4): keyed by
	// "<parentEntityName>|<inverseField>". A key absent from the map returns
	// metadata.ErrRelationshipNotFound (name available). Tests that don't exercise inverse-field
	// collisions leave this nil (every lookup resolves "available").
	claimedInverse map[string]*metadata.RelationshipDefinition
	// createdRels records the relationship rows passed to CreateEntityWithFields so a test can
	// assert the 1-N registry row was built with the expected name/parent/child/inverse/fk.
	createdRels []metadata.RelationshipDefinition
}

func (f *fakeMeta) EntityExists(_ context.Context, _, _ string) (bool, error) {
	return f.exists, nil
}

// claimedByOther models the TOCTOU: another tenant registered this name AFTER our TableExists probe.
func (f *fakeMeta) NameClaimedByAnyTenant(_ context.Context, _ string) (bool, error) {
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimedByOther, nil
}

func (f *fakeMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if f.getEntityErr != nil {
		return nil, f.getEntityErr
	}
	if f.knownEntities[name] {
		return &metadata.EntityDefinition{Name: name, ID: name}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (f *fakeMeta) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return f.fieldsByEntityName[entityID], nil
}

func (f *fakeMeta) GetRelationshipByName(_ context.Context, _, name string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.relationshipsByName[name]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

// GetRelationshipByInverseField satisfies the extended MetadataWriter interface (ENG-REL2
// #4). Looks up claimedInverse by "<parent>|<inverseField>"; a miss means the name is free.
func (f *fakeMeta) GetRelationshipByInverseField(_ context.Context, _, parentEntityName, inverseField string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.claimedInverse[parentEntityName+"|"+inverseField]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

func (f *fakeMeta) CreateEntityWithFields(_ context.Context, _ *metadata.EntityDefinition, _ []metadata.FieldDefinition, rels []metadata.RelationshipDefinition) error {
	f.called = true
	f.createdRels = rels
	return f.err
}

func TestCreateEntity_HappyPath(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	ent, fields, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity())
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if ent.DataTable != "obj_courses" {
		t.Errorf("DataTable = %q; want obj_courses", ent.DataTable)
	}
	if ent.TenantID != "tenant-1" {
		t.Errorf("TenantID = %q; want tenant-1", ent.TenantID)
	}
	if len(fields) != 3 {
		t.Errorf("len(fields) = %d; want 3", len(fields))
	}
	if len(schema.created) != 1 || schema.created[0] != "courses" {
		t.Errorf("created = %v; want [courses]", schema.created)
	}
	if !meta.called {
		t.Error("metadata writer was not called")
	}
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; want none on happy path", schema.dropped)
	}
}

func TestCreateEntity_CompensationOnMetaFailure(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{err: errors.New("metadata write boom")}
	svc := NewEntityService(schema, meta)

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity())
	if err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.created) != 1 {
		t.Errorf("table should have been created before metadata; created=%v", schema.created)
	}
	// The whole point: a failed metadata write must DROP the orphan table.
	if len(schema.dropped) != 1 || schema.dropped[0] != "courses" {
		t.Errorf("dropped = %v; want [courses] (compensation)", schema.dropped)
	}
}

func TestCreateEntity_PreexistingTableNotDropped(t *testing.T) {
	// If the obj_* table already existed (e.g. a concurrent creator made it), a metadata
	// failure must NOT drop it — that would destroy the other creator's table/data.
	schema := &fakeSchema{existed: true}
	meta := &fakeMeta{err: errors.New("metadata write boom")}
	svc := NewEntityService(schema, meta)

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity())
	if err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; a pre-existing table must NOT be dropped", schema.dropped)
	}
}

func TestCreateEntity_DuplicateBlocksBeforeDDL(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{exists: true}
	svc := NewEntityService(schema, meta)

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity())
	if codeOf(err) != "ENTITY_ALREADY_EXISTS" {
		t.Fatalf("code = %q; want ENTITY_ALREADY_EXISTS", codeOf(err))
	}
	if len(schema.created) != 0 {
		t.Errorf("no DDL should run for a duplicate; created=%v", schema.created)
	}
	if meta.called {
		t.Error("metadata write must not run for a duplicate")
	}
}

func TestCreateEntity_ValidationBlocksBeforeDDL(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Name = "select" // reserved -> must fail validation
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "ENTITY_INVALID_NAME" {
		t.Fatalf("code = %q; want ENTITY_INVALID_NAME", codeOf(err))
	}
	if len(schema.created) != 0 {
		t.Errorf("no DDL should run when validation fails; created=%v", schema.created)
	}
	if meta.called {
		t.Error("metadata writer must not be called when validation fails")
	}
}

// TestCreateEntity_ReconcileSharedTable is bug3: obj_<name> is shared across tenants (keyed
// by entity name, not tenant). When the table already exists (another tenant created the
// entity first), CreateTable's IF NOT EXISTS is a no-op. A unique/relation field this
// creator declares that the first creator lacked must still get its physical column ALTERed
// in, else uniqueness is never enforced / the FK column is missing.
func TestCreateEntity_ReconcileSharedTable(t *testing.T) {
	// Pre-existing shared table already has the `title` (ordinary) column but NOT the
	// `code` unique column nor the `mentor_id` relation column this creator adds.
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"title": true}}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	in := validEntity() // fields: title(text), code(text,unique), level(picklist)
	in.Fields = append(in.Fields, FieldInput{
		Name: "mentor", Label: "Mentor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users"},
	})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", in)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	// CreateTable still runs (idempotent no-op against the shared table).
	if len(schema.created) != 1 {
		t.Errorf("created = %v; want CreateTable still invoked", schema.created)
	}
	// The missing unique column must be ALTERed in.
	if len(schema.addedUnique) != 1 || schema.addedUnique[0] != "code" {
		t.Errorf("addedUnique = %v; want [code]", schema.addedUnique)
	}
	// The missing relation column must be ALTERed in.
	if len(schema.addedRelation) != 1 || schema.addedRelation[0] != "mentor" {
		t.Errorf("addedRelation = %v; want [mentor]", schema.addedRelation)
	}
}

// TestCreateEntity_ReconcileSkipsExistingColumns: on a pre-existing shared table whose
// unique/relation columns are ALREADY present, reconcile must NOT re-ALTER them (ADD COLUMN
// onto an existing column errors -> 500).
func TestCreateEntity_ReconcileSkipsExistingColumns(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{
		"title": true, "code": true, "mentor_id": true,
	}}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Fields = append(in.Fields, FieldInput{
		Name: "mentor", Label: "Mentor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users"},
	})

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-2", in); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedUnique) != 0 {
		t.Errorf("addedUnique = %v; want none (column already present)", schema.addedUnique)
	}
	if len(schema.addedRelation) != 0 {
		t.Errorf("addedRelation = %v; want none (column already present)", schema.addedRelation)
	}
}

// TestCreateEntity_FreshTableSkipsReconcile: when the table did NOT pre-exist, CreateTable
// built every column inline, so reconcile must be skipped entirely (no ColumnExists-driven
// ALTERs).
func TestCreateEntity_FreshTableSkipsReconcile(t *testing.T) {
	schema := &fakeSchema{existed: false}
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity()); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedUnique) != 0 || len(schema.addedRelation) != 0 {
		t.Errorf("fresh table must not reconcile; addedUnique=%v addedRelation=%v",
			schema.addedUnique, schema.addedRelation)
	}
}

// TestCreateEntity_ReconcileDuplicateCardinalityReturns409 is the core of this fix (bug
// #1/#8/#10), retargeted for RM-28 Slice B: a plain unique field's reconcile ALTER
// (AddUniqueGencolNonUnique) is now a non-unique index and can never 1062, so the ONE reconcile
// path that can still hit ER_DUP_ENTRY (surfaced as dynrepo.ErrDuplicateUnique) is a 1-1
// relation's cardinality index (AddOneToOneRelationColumn) — a co-tenant's rows on the SHARED
// obj_* table already violate 1-1 exclusivity. CreateEntity must return a
// FIELD_RELATION_CARDINALITY_CONFLICT ValidationError (handler maps -> 409), NOT a generic error
// (-> 500). It must also roll back any reconcile ALTER it already applied to the shared table
// (here: the unique `code` column added before the failing 1-1 relation column).
func TestCreateEntity_ReconcileDuplicateCardinalityReturns409(t *testing.T) {
	// Pre-existing shared table; `code` and `mentor_id` are absent so both get reconciled.
	// AddOneToOneRelationColumn("mentor") fails with the duplicate sentinel (co-tenant violates
	// 1-1 exclusivity).
	schema := &fakeSchema{
		existed:        true,
		existingCols:   map[string]bool{"title": true},
		addOneToOneErr: map[string]error{"mentor": dynrepo.ErrDuplicateUnique},
	}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	in := validEntity() // title(text), code(text,unique), level(picklist)
	// The unique field `code` reconciles FIRST (declared order), then the failing 1-1 relation
	// — exercises rollback of the prior reconcile ALTER.
	in.Fields = append(in.Fields, FieldInput{
		Name: "mentor", Label: "Mentor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users", "cardinality": "1-1"},
	})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", in)
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_CONFLICT", codeOf(err))
	}
	// Metadata write must NOT have run (we failed before persisting).
	if meta.called {
		t.Error("metadata write must not run when reconcile fails")
	}
	// The unique column ALTERed in before the failure must be rolled back.
	if len(schema.droppedUnique) != 1 || schema.droppedUnique[0] != "code" {
		t.Errorf("droppedUnique = %v; want [code] (rollback of prior reconcile ALTER)", schema.droppedUnique)
	}
	// The shared table itself must NEVER be dropped (a co-tenant owns it).
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; the shared table must never be dropped", schema.dropped)
	}
}

// TestCreateEntity_ReconcileCompensatesOnMetaFailure: on a pre-existing SHARED table, if the
// metadata write fails AFTER reconcile ALTERed columns in, those exact columns must be dropped
// (no orphan generated column/index), but the shared table itself must NOT be dropped.
func TestCreateEntity_ReconcileCompensatesOnMetaFailure(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"title": true}}
	meta := &fakeMeta{
		err:           errors.New("metadata write boom"),
		knownEntities: map[string]bool{"users": true},
	}
	svc := NewEntityService(schema, meta)

	in := validEntity() // adds `code` unique
	in.Fields = append(in.Fields, FieldInput{
		Name: "mentor", Label: "Mentor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users"},
	})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", in)
	if err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	// Reconcile added `code` (unique) and `mentor` (relation); both must be rolled back.
	if len(schema.droppedUnique) != 1 || schema.droppedUnique[0] != "code" {
		t.Errorf("droppedUnique = %v; want [code]", schema.droppedUnique)
	}
	if len(schema.droppedRelation) != 1 || schema.droppedRelation[0] != "mentor" {
		t.Errorf("droppedRelation = %v; want [mentor]", schema.droppedRelation)
	}
	// The shared table must NOT be dropped — a co-tenant depends on it.
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; a pre-existing shared table must NOT be dropped", schema.dropped)
	}
}

// TestCreateEntity_RelationTargetNotFound (bug #6): a relation field whose target entity was
// never provisioned must be rejected 422 (FIELD_RELATION_TARGET_NOT_FOUND) at CREATE time,
// before any DDL runs — not silently created then 500 on every later record insert.
func TestCreateEntity_RelationTargetNotFound(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{} // knownEntities empty -> any target resolves to not-found
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Fields = append(in.Fields, FieldInput{
		Name: "mentor", Label: "Mentor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "ghosts"},
	})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_TARGET_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_RELATION_TARGET_NOT_FOUND", codeOf(err))
	}
	// The check is BEFORE any DDL: no table created, no metadata write.
	if len(schema.created) != 0 {
		t.Errorf("no DDL should run when relation target is missing; created=%v", schema.created)
	}
	if meta.called {
		t.Error("metadata write must not run when relation target is missing")
	}
}

// TestCreateEntity_RelationSelfRefSkipsTargetCheck (bug #6 caution): a relation pointing at
// the entity being created (self-reference, e.g. employees.manager -> employees) must NOT be
// rejected even though the target is not yet in metadata — it is materialized by CreateTable.
func TestCreateEntity_RelationSelfRefSkipsTargetCheck(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{} // knownEntities empty: a naive check would reject the self-target
	svc := NewEntityService(schema, meta)

	in := CreateEntityInput{
		Name:  "employees",
		Label: "Nhân viên",
		Fields: []FieldInput{
			{Name: "name", Label: "Tên", FieldType: "text", IsRequired: true},
			{Name: "manager", Label: "Quản lý", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "employees"}},
		},
	}

	ent, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("self-referencing relation must be allowed: %v", err)
	}
	if ent.Name != "employees" {
		t.Errorf("entity name = %q; want employees", ent.Name)
	}
	if !meta.called {
		t.Error("metadata write should run for a valid self-referencing entity")
	}
	// ENG-REL2 #4 (review LOW-2): the self-ref path still registers its 1-N registry row —
	// parent == child == the entity itself, default inverse = the (child) entity name.
	if len(meta.createdRels) != 1 {
		t.Fatalf("self-ref relation must register exactly 1 registry row; got %d", len(meta.createdRels))
	}
	rel := meta.createdRels[0]
	if rel.Name != "employees_manager" || rel.ParentEntityName != "employees" ||
		rel.ChildEntityName != "employees" || rel.RelationshipType != "1-N" ||
		rel.ChildFKColumn != "manager_id" || rel.InverseField != "employees" {
		t.Errorf("self-ref registry row = %+v; want employees_manager employees/employees 1-N manager_id inverse=employees", rel)
	}
}

// TestCreateEntity_RelationTargetCrossTenantIsNotFound: a target that exists for ANOTHER
// tenant is resolved tenant-scoped, so GetEntityByName returns ErrEntityNotFound here and the
// create is rejected 422 (no cross-tenant target borrowing).
func TestCreateEntity_RelationTargetCrossTenantIsNotFound(t *testing.T) {
	schema := &fakeSchema{}
	// fakeMeta.GetEntityByName ignores tenant and only knows names, so to model "owned by
	// another tenant" we simply leave the target out of knownEntities (tenant-scoped miss).
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Fields = append(in.Fields, FieldInput{
		Name: "supervisor", Label: "Supervisor", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "users"},
	})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_TARGET_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_RELATION_TARGET_NOT_FOUND", codeOf(err))
	}
}

// TestCreateEntity_CompensationAsksOwnershipNotHistory pins the cross-tenant TOCTOU.
//
// obj_<name> is keyed by ENTITY NAME, not by tenant, so several tenants share one table. The
// compensation path used to decide by `!existedBefore` — a TableExists snapshot taken many
// statements earlier, before the CREATE, the reconcile ALTERs and the metadata write. If another
// tenant registers the same name inside that window, the losing request "rolls back" by DROPPING
// the winner's live table and every row in it, while the winner's sys_entities row survives — so
// every later request against that entity 500s and nothing explains why.
//
// The rule is now ownership, not history: drop only if nobody claims the name.
func TestCreateEntity_CompensationAsksOwnershipNotHistory(t *testing.T) {
	t.Run("nobody claims the name → the orphan table is still dropped", func(t *testing.T) {
		schema := &fakeSchema{}
		meta := &fakeMeta{err: errors.New("metadata write boom")}
		svc := NewEntityService(schema, meta)

		if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity()); err == nil {
			t.Fatal("expected the metadata failure")
		}
		if len(schema.dropped) != 1 {
			t.Errorf("dropped = %v; want the orphan cleaned up", schema.dropped)
		}
	})

	t.Run("another tenant claimed it mid-flight → NEVER drop", func(t *testing.T) {
		schema := &fakeSchema{} // existedBefore=false: we DID create it, as far as we knew
		meta := &fakeMeta{err: errors.New("metadata write boom"), claimedByOther: true}
		svc := NewEntityService(schema, meta)

		if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity()); err == nil {
			t.Fatal("expected the metadata failure")
		}
		if len(schema.dropped) != 0 {
			t.Fatalf("dropped %v — that is another tenant's live table and its rows", schema.dropped)
		}
	})

	t.Run("ownership lookup itself fails → refuse to drop", func(t *testing.T) {
		// An empty orphan table is recoverable (the next create adopts it via IF NOT EXISTS);
		// a destroyed live one is not. On doubt, keep.
		schema := &fakeSchema{}
		meta := &fakeMeta{err: errors.New("metadata write boom"), claimErr: errors.New("db down")}
		svc := NewEntityService(schema, meta)

		if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", validEntity()); err == nil {
			t.Fatal("expected the metadata failure")
		}
		if len(schema.dropped) != 0 {
			t.Errorf("dropped %v despite not knowing who owns the table", schema.dropped)
		}
	})
}
