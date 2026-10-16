package services

// Unit tests for the W2 relationship lifecycle service (impact / patch / delete) over
// in-memory fakes — the deep DB-backed assertions (real soft-deletes, tombstones, FK
// counts, join tables) live in relation_lifecycle_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// ---------------------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------------------

type fakeLifecycleMeta struct {
	rels     map[string]*metadata.RelationshipDefinition // key: tenantID + "/" + relID
	entities map[string]*metadata.EntityDefinition       // key: tenantID + "/" + name
	fields   map[string][]metadata.FieldDefinition       // key: tenantID + "/" + entityID

	softDeletedRelIDs []string
	// viewConfigWrites records ENG-RELVIEW UpdateRelationshipViewConfig calls (the method +
	// write type live in relation_viewconfig_test.go, next to the tests that assert them).
	viewConfigWrites []viewConfigWrite
}

func (f *fakeLifecycleMeta) GetRelationshipByID(_ context.Context, tenantID, relID string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.rels[tenantID+"/"+relID]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

func (f *fakeLifecycleMeta) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if e, ok := f.entities[tenantID+"/"+name]; ok {
		return e, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (f *fakeLifecycleMeta) ListFields(_ context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return f.fields[tenantID+"/"+entityID], nil
}

func (f *fakeLifecycleMeta) SoftDeleteRelationshipByID(_ context.Context, tenantID, relID string) error {
	if _, ok := f.rels[tenantID+"/"+relID]; !ok {
		return metadata.ErrRelationshipNotFound
	}
	f.softDeletedRelIDs = append(f.softDeletedRelIDs, relID)
	delete(f.rels, tenantID+"/"+relID)
	return nil
}

type fakeImpactCounts struct {
	fkRefs    int
	joinRows  int
	joinExist bool
}

func (f *fakeImpactCounts) CountRelationFKRefs(_ context.Context, _, _, _ string) (int, error) {
	return f.fkRefs, nil
}

func (f *fakeImpactCounts) CountJoinRows(_ context.Context, _, _, _ string) (int, error) {
	return f.joinRows, nil
}

func (f *fakeImpactCounts) JoinTableExists(_ context.Context, _, _ string) (bool, error) {
	return f.joinExist, nil
}

type fakeFieldDeleter struct {
	deletedFieldIDs []string
	appliedLive     []bool // per call: was ctx marked "apply now" (specs/063 FR-070: relationships are not drafted)
	err             error
}

func (f *fakeFieldDeleter) DeleteField(ctx context.Context, _ string, in DeleteFieldInput) error {
	if f.err != nil {
		return f.err
	}
	f.appliedLive = append(f.appliedLive, metaversion.IsPublishApply(ctx))
	f.deletedFieldIDs = append(f.deletedFieldIDs, in.FieldID)
	return nil
}

// ---------------------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------------------

const (
	lcTenant      = "aaaaaaaa-0a0a-0a0a-0a0a-aaaaaaaaaaaa"
	lcOtherTenant = "bbbbbbbb-0b0b-0b0b-0b0b-bbbbbbbbbbbb"
	lcRelID       = "11111111-1111-1111-1111-111111111111"
	lcFieldID     = "22222222-2222-2222-2222-222222222222"
	lcParentEntID = "33333333-3333-3333-3333-333333333333"
	lcChildEntID  = "44444444-4444-4444-4444-444444444444"
)

// strPtr is declared in field_edit_test.go (shared test helper in this package).

// lcFixture builds a fake meta with a 1-N registry row (accounts ← contacts.account) plus
// both side entities; the child owns the relation field "account".
func lcFixture() *fakeLifecycleMeta {
	relOpts := `{"relatedEntity":"accounts"}`
	return &fakeLifecycleMeta{
		rels: map[string]*metadata.RelationshipDefinition{
			lcTenant + "/" + lcRelID: {
				ID: lcRelID, TenantID: lcTenant, Name: "contacts_account",
				ParentEntityName: "accounts", ChildEntityName: "contacts",
				RelationshipType: "1-N", InverseField: "contacts", ChildFKColumn: "account_id",
			},
		},
		entities: map[string]*metadata.EntityDefinition{
			lcTenant + "/accounts": {ID: lcParentEntID, TenantID: lcTenant, Name: "accounts"},
			lcTenant + "/contacts": {ID: lcChildEntID, TenantID: lcTenant, Name: "contacts"},
		},
		fields: map[string][]metadata.FieldDefinition{
			lcTenant + "/" + lcChildEntID: {
				{ID: lcFieldID, EntityID: lcChildEntID, TenantID: lcTenant,
					Name: "account", FieldType: "relation", Options: &relOpts},
			},
		},
	}
}

func lcService(meta *fakeLifecycleMeta, counts *fakeImpactCounts, fields *fakeFieldDeleter) *RelationLifecycleService {
	if counts == nil {
		counts = &fakeImpactCounts{}
	}
	if fields == nil {
		fields = &fakeFieldDeleter{}
	}
	return NewRelationLifecycleService(meta, counts, fields)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v; want *ValidationError %s", err, code)
	}
	if ve.Code != code {
		t.Fatalf("code = %s; want %s", ve.Code, code)
	}
}

// ---------------------------------------------------------------------------------------
// Patch — immutability contract
// ---------------------------------------------------------------------------------------

func TestRelationLifecycle_Patch(t *testing.T) {
	tests := []struct {
		name     string
		tenant   string
		relID    string
		in       PatchRelationshipInput
		wantErr  string // ValidationError code; "" = success
		wantName string // returned row name on success
	}{
		{
			name: "no-op empty body echoes stored row", tenant: lcTenant, relID: lcRelID,
			in: PatchRelationshipInput{}, wantName: "contacts_account",
		},
		{
			name: "echo-equal values are a no-op", tenant: lcTenant, relID: lcRelID,
			in: PatchRelationshipInput{
				Name:             strPtr("contacts_account"),
				InverseField:     strPtr("contacts"),
				RelationshipType: strPtr("1-N"),
			},
			wantName: "contacts_account",
		},
		{
			name: "changing inverseField rejected", tenant: lcTenant, relID: lcRelID,
			in:      PatchRelationshipInput{InverseField: strPtr("renamed_side")},
			wantErr: "FIELD_RELATION_OPTIONS_IMMUTABLE",
		},
		{
			name: "changing cardinality (relationshipType) rejected", tenant: lcTenant, relID: lcRelID,
			in:      PatchRelationshipInput{RelationshipType: strPtr("1-1")},
			wantErr: "FIELD_RELATION_OPTIONS_IMMUTABLE",
		},
		{
			name: "changing relatedEntity (parent side) rejected", tenant: lcTenant, relID: lcRelID,
			in:      PatchRelationshipInput{ParentEntityName: strPtr("companies")},
			wantErr: "FIELD_RELATION_OPTIONS_IMMUTABLE",
		},
		{
			name: "changing name rejected", tenant: lcTenant, relID: lcRelID,
			in:      PatchRelationshipInput{Name: strPtr("renamed")},
			wantErr: "FIELD_RELATION_OPTIONS_IMMUTABLE",
		},
		{
			name: "unknown id → RELATIONSHIP_NOT_FOUND", tenant: lcTenant, relID: "no-such-id",
			in: PatchRelationshipInput{}, wantErr: "RELATIONSHIP_NOT_FOUND",
		},
		{
			name: "cross-tenant id resolves to not-found", tenant: lcOtherTenant, relID: lcRelID,
			in: PatchRelationshipInput{}, wantErr: "RELATIONSHIP_NOT_FOUND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := lcService(lcFixture(), nil, nil)
			rel, err := svc.Patch(context.Background(), tt.tenant, tt.relID, tt.in)
			if tt.wantErr != "" {
				wantCode(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Patch: %v", err)
			}
			if rel.Name != tt.wantName {
				t.Errorf("Name = %q; want %q", rel.Name, tt.wantName)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// Delete — routing, dependents gate, orphan fallback, tenant isolation
// ---------------------------------------------------------------------------------------

func TestRelationLifecycle_Delete_OneToN_ReusesFieldDelete(t *testing.T) {
	meta := lcFixture()
	fields := &fakeFieldDeleter{}
	svc := lcService(meta, nil, fields)

	if err := svc.Delete(context.Background(), lcTenant, lcRelID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fields.deletedFieldIDs) != 1 || fields.deletedFieldIDs[0] != lcFieldID {
		t.Errorf("deleted field ids = %v; want [%s] (must ride the DeleteField primitive)", fields.deletedFieldIDs, lcFieldID)
	}
	// FR-070: a relationship applies at once even on a delivered app, so the field delete underneath
	// must not fork into a "sẽ xoá" draft.
	if len(fields.appliedLive) != 1 || !fields.appliedLive[0] {
		t.Errorf("DeleteField ctx live marks = %v; want [true] (relationship delete must bypass the draft fork)", fields.appliedLive)
	}
	// The registry row is cleaned up INSIDE DeleteField (SoftDeleteRelationshipByChildFK) —
	// the lifecycle service must NOT double-delete it directly.
	if len(meta.softDeletedRelIDs) != 0 {
		t.Errorf("SoftDeleteRelationshipByID called %v; want none on the field-owned path", meta.softDeletedRelIDs)
	}
}

func TestRelationLifecycle_Delete_OrphanRegistryRow_FallsBackToDirectSoftDelete(t *testing.T) {
	meta := lcFixture()
	// Simulate the orphan: the owning field is already gone from the child's field list.
	meta.fields[lcTenant+"/"+lcChildEntID] = nil
	fields := &fakeFieldDeleter{}
	svc := lcService(meta, nil, fields)

	if err := svc.Delete(context.Background(), lcTenant, lcRelID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fields.deletedFieldIDs) != 0 {
		t.Errorf("DeleteField called %v; want none (no owning field)", fields.deletedFieldIDs)
	}
	if len(meta.softDeletedRelIDs) != 1 || meta.softDeletedRelIDs[0] != lcRelID {
		t.Errorf("softDeletedRelIDs = %v; want [%s]", meta.softDeletedRelIDs, lcRelID)
	}
}

func TestRelationLifecycle_Delete_NN_SoftDeletesRegistryRowOnly(t *testing.T) {
	meta := lcFixture()
	join := "rel_accounts_contacts"
	meta.rels[lcTenant+"/"+lcRelID] = &metadata.RelationshipDefinition{
		ID: lcRelID, TenantID: lcTenant, Name: "accounts_contacts",
		ParentEntityName: "accounts", ChildEntityName: "contacts",
		RelationshipType: "N-N", JoinTableName: &join,
	}
	fields := &fakeFieldDeleter{}
	svc := lcService(meta, nil, fields)

	if err := svc.Delete(context.Background(), lcTenant, lcRelID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fields.deletedFieldIDs) != 0 {
		t.Errorf("DeleteField called %v; want none for N-N", fields.deletedFieldIDs)
	}
	if len(meta.softDeletedRelIDs) != 1 || meta.softDeletedRelIDs[0] != lcRelID {
		t.Errorf("softDeletedRelIDs = %v; want [%s]", meta.softDeletedRelIDs, lcRelID)
	}
}

func TestRelationLifecycle_Delete_DependentRollup_Returns409BeforeAnyMutation(t *testing.T) {
	meta := lcFixture()
	rollupOpts := `{"childEntity":"contacts","viaRelation":"account","metric":"count"}`
	meta.fields[lcTenant+"/"+lcParentEntID] = []metadata.FieldDefinition{
		{ID: "rollup-1", EntityID: lcParentEntID, TenantID: lcTenant,
			Name: "contact_count", FieldType: "rollup", Options: &rollupOpts},
	}
	fields := &fakeFieldDeleter{}
	svc := lcService(meta, nil, fields)

	err := svc.Delete(context.Background(), lcTenant, lcRelID)
	wantCode(t, err, "RELATIONSHIP_HAS_DEPENDENTS")

	// Guard must fire BEFORE any mutation: nothing deleted anywhere.
	if len(fields.deletedFieldIDs) != 0 || len(meta.softDeletedRelIDs) != 0 {
		t.Errorf("mutations ran despite dependents gate: fields=%v rels=%v",
			fields.deletedFieldIDs, meta.softDeletedRelIDs)
	}
	var ve *ValidationError
	errors.As(err, &ve)
	if _, ok := ve.Details["dependentRollups"]; !ok {
		t.Errorf("details = %v; want dependentRollups list", ve.Details)
	}
}

func TestRelationLifecycle_Delete_NotFoundAndCrossTenant(t *testing.T) {
	tests := []struct {
		name   string
		tenant string
		relID  string
	}{
		{"unknown id", lcTenant, "no-such-id"},
		{"cross-tenant id (tenant B cannot delete tenant A's relationship)", lcOtherTenant, lcRelID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := lcFixture()
			fields := &fakeFieldDeleter{}
			svc := lcService(meta, nil, fields)
			err := svc.Delete(context.Background(), tt.tenant, tt.relID)
			wantCode(t, err, "RELATIONSHIP_NOT_FOUND")
			if len(fields.deletedFieldIDs) != 0 || len(meta.softDeletedRelIDs) != 0 {
				t.Errorf("mutations ran on a refused delete: fields=%v rels=%v",
					fields.deletedFieldIDs, meta.softDeletedRelIDs)
			}
			// The tenant-A row must still resolve for tenant A.
			if _, gerr := meta.GetRelationshipByID(context.Background(), lcTenant, lcRelID); gerr != nil {
				t.Errorf("tenant A's relationship must be untouched; got %v", gerr)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// Impact — counts + dependents + deletable verdict
// ---------------------------------------------------------------------------------------

func TestRelationLifecycle_Impact(t *testing.T) {
	lookupOpts := `{"via":"account","sourceField":"name"}`
	rollupNNOpts := `{"childEntity":"contacts","relationship":"accounts_contacts","metric":"count"}`
	join := "rel_accounts_contacts"

	tests := []struct {
		name          string
		mutate        func(m *fakeLifecycleMeta)
		counts        fakeImpactCounts
		wantAffected  int
		wantRollups   int
		wantLookups   int
		wantDeletable bool
	}{
		{
			name:          "1-N counts non-null child FKs",
			counts:        fakeImpactCounts{fkRefs: 7},
			wantAffected:  7,
			wantDeletable: true,
		},
		{
			name: "1-N reports sibling lookups informationally (still deletable)",
			mutate: func(m *fakeLifecycleMeta) {
				m.fields[lcTenant+"/"+lcChildEntID] = append(m.fields[lcTenant+"/"+lcChildEntID],
					metadata.FieldDefinition{ID: "lk-1", EntityID: lcChildEntID, TenantID: lcTenant,
						Name: "account_name", FieldType: "lookup", Options: &lookupOpts})
			},
			counts:        fakeImpactCounts{fkRefs: 1},
			wantAffected:  1,
			wantLookups:   1,
			wantDeletable: true,
		},
		{
			name: "N-N counts join rows and flags dependent N-N rollups",
			mutate: func(m *fakeLifecycleMeta) {
				m.rels[lcTenant+"/"+lcRelID] = &metadata.RelationshipDefinition{
					ID: lcRelID, TenantID: lcTenant, Name: "accounts_contacts",
					ParentEntityName: "accounts", ChildEntityName: "contacts",
					RelationshipType: "N-N", JoinTableName: &join,
				}
				m.fields[lcTenant+"/"+lcParentEntID] = []metadata.FieldDefinition{
					{ID: "rl-1", EntityID: lcParentEntID, TenantID: lcTenant,
						Name: "linked_count", FieldType: "rollup", Options: &rollupNNOpts},
				}
			},
			counts:        fakeImpactCounts{joinRows: 5, joinExist: true},
			wantAffected:  5,
			wantRollups:   1,
			wantDeletable: false,
		},
		{
			name: "N-N with reclaimed join table counts zero",
			mutate: func(m *fakeLifecycleMeta) {
				m.rels[lcTenant+"/"+lcRelID] = &metadata.RelationshipDefinition{
					ID: lcRelID, TenantID: lcTenant, Name: "accounts_contacts",
					ParentEntityName: "accounts", ChildEntityName: "contacts",
					RelationshipType: "N-N", JoinTableName: &join,
				}
			},
			counts:        fakeImpactCounts{joinRows: 99, joinExist: false},
			wantAffected:  0,
			wantDeletable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := lcFixture()
			if tt.mutate != nil {
				tt.mutate(meta)
			}
			svc := lcService(meta, &tt.counts, nil)
			imp, err := svc.Impact(context.Background(), lcTenant, lcRelID)
			if err != nil {
				t.Fatalf("Impact: %v", err)
			}
			if imp.AffectedRecords != tt.wantAffected {
				t.Errorf("AffectedRecords = %d; want %d", imp.AffectedRecords, tt.wantAffected)
			}
			if len(imp.DependentRollups) != tt.wantRollups {
				t.Errorf("DependentRollups = %v; want %d entries", imp.DependentRollups, tt.wantRollups)
			}
			if len(imp.DependentLookups) != tt.wantLookups {
				t.Errorf("DependentLookups = %v; want %d entries", imp.DependentLookups, tt.wantLookups)
			}
			if imp.Deletable != tt.wantDeletable {
				t.Errorf("Deletable = %v; want %v", imp.Deletable, tt.wantDeletable)
			}
			if imp.Relationship == nil || imp.Relationship.ID != lcRelID {
				t.Errorf("Relationship echo missing/wrong: %+v", imp.Relationship)
			}
		})
	}
}

func TestRelationLifecycle_Impact_NotFound(t *testing.T) {
	svc := lcService(lcFixture(), nil, nil)
	_, err := svc.Impact(context.Background(), lcOtherTenant, lcRelID)
	wantCode(t, err, "RELATIONSHIP_NOT_FOUND")
}
