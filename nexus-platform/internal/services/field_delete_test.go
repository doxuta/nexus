package services

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"nexus/platform/internal/metadata"
)

// fakeDeleteMeta stubs the metadata reads/writes DeleteField depends on: a tenant-scoped
// field lookup by id, and the soft-delete write. It records the id passed to
// SoftDeleteField so tests can assert exactly which field was deleted, and never reports a
// schema (DDL) call — soft delete must touch no obj_* column.
type fakeDeleteMeta struct {
	field   *metadata.FieldDefinition
	getErr  error
	deleted []string // field ids passed to SoftDeleteField
	// siblings backs ListFields: the entity's active fields, read by the formula-dependants guard (#233).
	siblings []metadata.FieldDefinition
	delErr   error

	gotTenant       string // tenant id forwarded to GetFieldByID (asserts scoping)
	gotDeleteTenant string // tenant id forwarded to SoftDeleteField (asserts re-pin)

	// ENG-REL2 #4: registry soft-delete tracking (relation field delete cleanup).
	deletedRelChildFK []string // "<childEntityName>|<childFKColumn>" pairs
	deleteRelErr      error
	// entityName is returned by GetEntityNameByID (used to resolve the owning entity name for
	// the registry cleanup lookup key). Defaults to "" (tests that exercise the relation-delete
	// path must set it).
	entityName string
}

// The delete path only needs the field getter + the soft-delete writer; the entity lookup,
// list, create and update methods are unused here but required to satisfy the interface.
func (f *fakeDeleteMeta) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	return nil, errors.New("unused")
}

func (f *fakeDeleteMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return f.siblings, nil
}

func (f *fakeDeleteMeta) ListFieldsIncludingDeleted(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, errors.New("unused")
}

func (f *fakeDeleteMeta) CreateField(_ context.Context, _ *metadata.FieldDefinition) error {
	return errors.New("unused")
}

func (f *fakeDeleteMeta) GetFieldByID(_ context.Context, tenantID, _ string) (*metadata.FieldDefinition, error) {
	f.gotTenant = tenantID
	if f.getErr != nil {
		return nil, f.getErr
	}
	cp := *f.field
	return &cp, nil
}

func (f *fakeDeleteMeta) UpdateFieldMutable(_ context.Context, _ string, _ *metadata.FieldDefinition) error {
	return errors.New("unused")
}

func (f *fakeDeleteMeta) SoftDeleteField(_ context.Context, tenantID, fieldID string) error {
	f.gotDeleteTenant = tenantID
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, fieldID)
	return nil
}

// GetEntityNameByID satisfies the extended FieldMetadataRepo interface (D1 / ENG-REL2 #4).
// DeleteField's `relation`-field registry cleanup resolves the owning entity's name via this
// call; returns the fixture's entityName (empty string for tests that don't set it).
func (f *fakeDeleteMeta) GetEntityNameByID(_ context.Context, _, _ string) (string, error) {
	return f.entityName, nil
}

// GetRelationshipByName satisfies the extended FieldMetadataRepo interface (ENG-COMPUTED #3).
// The delete path never calls it, so it is inert here.
func (f *fakeDeleteMeta) GetRelationshipByName(_ context.Context, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

// GetRelationshipByInverseField / CreateFieldWithRelationship satisfy the extended
// FieldMetadataRepo interface (ENG-REL2 #4). DeleteField never adds a field, so both are
// inert here; SoftDeleteRelationshipByChildFK is the one the delete path DOES call for a
// `relation`-type field — see the dedicated fields/tracking below.
func (f *fakeDeleteMeta) GetRelationshipByInverseField(_ context.Context, _, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

func (f *fakeDeleteMeta) CreateFieldWithRelationship(_ context.Context, _ *metadata.FieldDefinition, _ *metadata.RelationshipDefinition) error {
	return errors.New("unused")
}

// SoftDeleteRelationshipByChildFK records the (childEntityName, childFKColumn) pair passed by
// DeleteField's registry-cleanup step (ENG-REL2 #4) so a test can assert it ran (or did not)
// for a `relation`-type field.
func (f *fakeDeleteMeta) SoftDeleteRelationshipByChildFK(_ context.Context, tenantID, childEntityName, childFKColumn string) error {
	f.deletedRelChildFK = append(f.deletedRelChildFK, childEntityName+"|"+childFKColumn)
	if f.deleteRelErr != nil {
		return f.deleteRelErr
	}
	return nil
}

func deletableField() *metadata.FieldDefinition {
	return &metadata.FieldDefinition{ID: "fld-1", EntityID: "ent-1", Name: "summary", FieldType: "text"}
}

// deleteService wires a FieldService whose schema repo must never be touched: a soft delete
// performs NO DDL, so any AddUniqueColumn/DropUniqueColumn call is a bug.
func deleteService(meta FieldMetadataRepo) (*FieldService, *fakeFieldSchema) {
	schema := &fakeFieldSchema{}
	return NewFieldService(schema, meta), schema
}

// TestDeleteField_SoftDeletesAndRunsNoDDL is the core acceptance: a soft delete marks the
// sys_fields row (via SoftDeleteField, by id) and performs NO ALTER on the obj_* table.
func TestDeleteField_SoftDeletesAndRunsNoDDL(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField()}
	svc, schema := deleteService(meta)

	if err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-1"}); err != nil {
		t.Fatalf("DeleteField: %v", err)
	}
	if len(meta.deleted) != 1 || meta.deleted[0] != "fld-1" {
		t.Errorf("field must be soft-deleted by id; deleted=%v", meta.deleted)
	}
	// No physical schema change of any kind: the column / data is preserved.
	if len(schema.added) != 0 || len(schema.dropped) != 0 {
		t.Errorf("soft delete must run NO DDL; added=%v dropped=%v", schema.added, schema.dropped)
	}
}

// TestDeleteField_TenantScoped asserts the tenant id flows into the field lookup, so a
// caller can never soft-delete a field belonging to another tenant's entity.
func TestDeleteField_TenantScoped(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField()}
	svc, _ := deleteService(meta)

	if err := svc.DeleteField(context.Background(), "tenant-XYZ", DeleteFieldInput{FieldID: "fld-1"}); err != nil {
		t.Fatalf("DeleteField: %v", err)
	}
	if meta.gotTenant != "tenant-XYZ" {
		t.Errorf("field lookup must be tenant-scoped; gotTenant=%q", meta.gotTenant)
	}
	// Defense-in-depth: the same tenant is also re-pinned at the soft-delete write.
	if meta.gotDeleteTenant != "tenant-XYZ" {
		t.Errorf("soft-delete must re-pin tenant; gotDeleteTenant=%q", meta.gotDeleteTenant)
	}
}

// TestDeleteField_TenantIsolation: tenant-B asking to delete tenant-A's field id resolves
// to FIELD_NOT_FOUND (the tenant-scoped getter hides it) and deletes nothing.
func TestDeleteField_TenantIsolation(t *testing.T) {
	meta := &fakeDeleteMeta{getErr: metadata.ErrFieldNotFound}
	svc, _ := deleteService(meta)

	err := svc.DeleteField(context.Background(), "tenant-B", DeleteFieldInput{FieldID: "fld-of-tenant-A"})
	if codeOf(err) != "FIELD_NOT_FOUND" {
		t.Fatalf("cross-tenant delete code = %q; want FIELD_NOT_FOUND", codeOf(err))
	}
	if len(meta.deleted) != 0 {
		t.Errorf("cross-tenant delete must soft-delete nothing; deleted=%v", meta.deleted)
	}
}

// TestDeleteField_UnknownFieldIsNotFound: a missing/already-deleted field resolves to a
// stable FIELD_NOT_FOUND code and runs no write.
func TestDeleteField_UnknownFieldIsNotFound(t *testing.T) {
	meta := &fakeDeleteMeta{getErr: metadata.ErrFieldNotFound}
	svc, _ := deleteService(meta)

	err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "nope"})
	if codeOf(err) != "FIELD_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_NOT_FOUND", codeOf(err))
	}
	if len(meta.deleted) != 0 {
		t.Errorf("missing field must not soft-delete; deleted=%v", meta.deleted)
	}
}

// TestDeleteField_EmptyIDRejected: an empty field id is rejected before any repository call.
func TestDeleteField_EmptyIDRejected(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField()}
	svc, _ := deleteService(meta)

	err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "  "})
	if codeOf(err) != "FIELD_ID_REQUIRED" {
		t.Fatalf("code = %q; want FIELD_ID_REQUIRED", codeOf(err))
	}
	if len(meta.deleted) != 0 {
		t.Errorf("empty id must not reach the repository; deleted=%v", meta.deleted)
	}
}

// TestDeleteField_SoftDeleteRace: the field was resolved but the soft-delete write reports
// it gone (deleted by a concurrent request) -> FIELD_NOT_FOUND, surfaced not swallowed.
func TestDeleteField_SoftDeleteRace(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField(), delErr: metadata.ErrFieldNotFound}
	svc, _ := deleteService(meta)

	err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-1"})
	if codeOf(err) != "FIELD_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_NOT_FOUND", codeOf(err))
	}
}

// TestDeleteField_PropagatesSoftDeleteError surfaces a generic repository failure wrapped.
func TestDeleteField_PropagatesSoftDeleteError(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField(), delErr: errors.New("db boom")}
	svc, _ := deleteService(meta)

	if err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-1"}); err == nil {
		t.Fatal("expected error when soft-delete write fails")
	}
}

// --- ENG-REL2 #4: relation field delete soft-deletes its registry row -------------------

func deletableRelationField() *metadata.FieldDefinition {
	return &metadata.FieldDefinition{ID: "fld-2", EntityID: "ent-1", Name: "owner", FieldType: "relation"}
}

// TestDeleteField_RelationField_SoftDeletesRegistryRow: deleting a `relation`-type field ALSO
// soft-deletes its companion 1-N sys_relationships row, keyed by
// (tenant, owning-entity-name, "<field>_id"). The owning entity name is resolved via
// GetEntityNameByID (the field row carries only entity_id).
func TestDeleteField_RelationField_SoftDeletesRegistryRow(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableRelationField(), entityName: "courses"}
	svc, schema := deleteService(meta)

	if err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-2"}); err != nil {
		t.Fatalf("DeleteField(relation): %v", err)
	}
	if len(meta.deleted) != 1 || meta.deleted[0] != "fld-2" {
		t.Errorf("field must still be soft-deleted; deleted=%v", meta.deleted)
	}
	if len(meta.deletedRelChildFK) != 1 || meta.deletedRelChildFK[0] != "courses|owner_id" {
		t.Errorf("registry row must be soft-deleted for courses|owner_id; deletedRelChildFK=%v", meta.deletedRelChildFK)
	}
	// Still no DDL — the registry cleanup is a pure metadata operation, like the field
	// soft-delete itself.
	if len(schema.added) != 0 || len(schema.dropped) != 0 || len(schema.relAdded) != 0 || len(schema.relDropped) != 0 {
		t.Errorf("relation field soft-delete must run NO DDL")
	}
}

// TestDeleteField_NonRelationField_SkipsRegistryCleanup: an ordinary (non-relation) field
// delete never calls the registry cleanup — there is no registry row to clean up.
func TestDeleteField_NonRelationField_SkipsRegistryCleanup(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableField(), entityName: "courses"} // FieldType: "text"
	svc, _ := deleteService(meta)

	if err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-1"}); err != nil {
		t.Fatalf("DeleteField(ordinary): %v", err)
	}
	if len(meta.deletedRelChildFK) != 0 {
		t.Errorf("ordinary field delete must not touch the relationship registry; deletedRelChildFK=%v", meta.deletedRelChildFK)
	}
}

// TestDeleteField_RelationField_RegistryCleanupErrorLeavesTheFieldRetryable: a failure while
// soft-deleting the registry row is surfaced (wrapped), never swallowed — AND the field itself is
// left live, so the whole call can simply be retried.
//
// The contract CHANGED here, deliberately. It used to tombstone the field first and clean up
// after, which its own comment conceded was unrecoverable ("a retry of DeleteField would 404 …
// an admin-cleanup case"): the field row was gone, so the retry resolved nothing and the residue
// could only be removed by hand. Both cleanups are idempotent deletes, so running them BEFORE the
// tombstone costs nothing and turns an unrecoverable state into a retryable one — the same
// reordering applied to entity-delete and file-delete in this sweep.
func TestDeleteField_RelationField_RegistryCleanupErrorLeavesTheFieldRetryable(t *testing.T) {
	meta := &fakeDeleteMeta{field: deletableRelationField(), entityName: "courses", deleteRelErr: errors.New("registry boom")}
	svc, _ := deleteService(meta)

	err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-2"})
	if err == nil {
		t.Fatal("expected error when registry cleanup fails")
	}
	if len(meta.deleted) != 0 {
		t.Errorf("the field was tombstoned despite the cleanup failing (deleted=%v) — a retry would 404 and the residue would need hand-cleanup",
			meta.deleted)
	}
}

func formulaSibling(id, name, expr string) metadata.FieldDefinition {
	b, _ := json.Marshal(map[string]any{"formula": expr, "resultType": "number"})
	opts := string(b)
	return metadata.FieldDefinition{ID: id, EntityID: "ent-1", Name: name, Label: name, FieldType: "formula", Options: &opts}
}

// #233: deleting a field a sibling formula still reads is refused (422), nothing is deleted.
func TestDeleteField_RefusedWhileFormulaReadsIt(t *testing.T) {
	amount := metadata.FieldDefinition{ID: "fld-amount", EntityID: "ent-1", Name: "amount", FieldType: "number"}
	tests := []struct {
		name     string
		siblings []metadata.FieldDefinition
		wantDeps []string // nil = delete must go through
	}{
		{"formula reads it", []metadata.FieldDefinition{amount, formulaSibling("f1", "total", "IF(amount > 1, amount, 0)")}, []string{"total"}},
		{"formula does not read it", []metadata.FieldDefinition{amount, formulaSibling("f1", "total", "qty * 2")}, nil},
		{"unparseable formula is skipped", []metadata.FieldDefinition{amount, formulaSibling("f1", "bad", "amount +")}, nil},
		{"no siblings", []metadata.FieldDefinition{amount}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := amount
			meta := &fakeDeleteMeta{field: &f, siblings: tc.siblings}
			svc, _ := deleteService(meta)
			err := svc.DeleteField(context.Background(), "tenant-1", DeleteFieldInput{FieldID: "fld-amount"})
			if tc.wantDeps == nil {
				if err != nil || len(meta.deleted) != 1 {
					t.Fatalf("err=%v deleted=%v; want clean delete", err, meta.deleted)
				}
				return
			}
			if codeOf(err) != "FIELD_HAS_FORMULA_DEPENDANTS" {
				t.Fatalf("code = %q; want FIELD_HAS_FORMULA_DEPENDANTS (err=%v)", codeOf(err), err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !reflect.DeepEqual(ve.Details["formulas"], tc.wantDeps) {
				t.Fatalf("details = %+v; want formulas %v", ve, tc.wantDeps)
			}
			if len(meta.deleted) != 0 {
				t.Errorf("refused delete must write nothing; deleted=%v", meta.deleted)
			}
		})
	}
}
