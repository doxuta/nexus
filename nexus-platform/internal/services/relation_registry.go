package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/metadata"
)

// inverseFieldLookup is the read the ENG-REL2 #4 inverse-field naming rule needs: given a
// tenant + parent entity + candidate inverse field name, report whether an ACTIVE
// sys_relationships row already claims it. Satisfied structurally by *metadata.Repository
// (used by both FieldMetadataRepo, in field_orchestration.go, and MetadataWriter, in
// entity_orchestration.go) — declared here, consumer-side, so this file stays unit-testable
// without importing either of those wider interfaces.
type inverseFieldLookup interface {
	GetRelationshipByInverseField(ctx context.Context, tenantID, parentEntityName, inverseField string) (*metadata.RelationshipDefinition, error)
}

// pendingInverseClaims tracks (parentEntity, inverseField) pairs already reserved earlier in
// the SAME in-flight batch (a single CreateEntity call can declare several relation fields
// before any of them is committed to sys_relationships). Without batch-local tracking, two
// relation fields in ONE CreateEntity call that both point at the same parent entity would
// both independently see "not taken" from the DB (neither is persisted yet) and register the
// same default inverse_field — a silent collision the plain (non-unique) idx_sys_rel_inverse
// index does not prevent at the DB level. AddField only ever registers a single relation
// field per call, so an empty pendingInverseClaims{} there is equivalent to "no batch".
type pendingInverseClaims map[string]map[string]bool

func (p pendingInverseClaims) has(parent, name string) bool {
	return p[parent][name]
}

func (p pendingInverseClaims) add(parent, name string) {
	if p[parent] == nil {
		p[parent] = map[string]bool{}
	}
	p[parent][name] = true
}

// inverseFieldTaken reports whether `name` is already claimed as the inverse_field for
// `parent` — either by an earlier field in the SAME in-flight batch (checked first, no DB
// round trip) or by an already-persisted, active sys_relationships row.
func inverseFieldTaken(ctx context.Context, repo inverseFieldLookup, claims pendingInverseClaims, tenantID, parent, name string) (bool, error) {
	if claims.has(parent, name) {
		return true, nil
	}
	_, err := repo.GetRelationshipByInverseField(ctx, tenantID, parent, name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, metadata.ErrRelationshipNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("services.inverseFieldTaken: %w", err)
}

// resolveInverseField implements the ENG-REL2 #4 inverse-field naming rule for a new 1-N
// relation field (childEntity owns the FK column fieldName+"_id", pointing at parentEntity):
//
//   - options.inverseField explicit → must pass ddl.SanitizeIdentifier (422
//     FIELD_RELATION_INVERSE_INVALID otherwise) and must be unclaimed for this parent (422
//     FIELD_RELATION_INVERSE_TAKEN otherwise).
//   - options.inverseField absent → default to the owning (child) entity name. On collision,
//     fall back to "<childEntity>_<fieldName>". If THAT also collides → 422
//     FIELD_RELATION_INVERSE_TAKEN. (Both candidates are built from already-validated
//     identifiers — childEntity and fieldName each individually pass SanitizeIdentifier's
//     26-char cap — so their concatenation is always well within the inverse_field column's
//     100-char limit; no re-validation needed for the server-derived candidates.)
//
// On success the chosen name is claimed in `claims` so a LATER relation field in the SAME
// batch (CreateEntity with several relation fields) sees it as taken too.
func resolveInverseField(
	ctx context.Context,
	repo inverseFieldLookup,
	claims pendingInverseClaims,
	tenantID, parentEntity, childEntity, fieldName string,
	options map[string]any,
) (string, error) {
	if raw, ok := options["inverseField"]; ok {
		s, _ := raw.(string)
		s = strings.TrimSpace(s)
		norm, err := ddl.SanitizeIdentifier(s)
		if err != nil {
			return "", invalid("FIELD_RELATION_INVERSE_INVALID",
				"field relation %q có options.inverseField không hợp lệ: %v", fieldName, err)
		}
		taken, err := inverseFieldTaken(ctx, repo, claims, tenantID, parentEntity, norm)
		if err != nil {
			return "", err
		}
		if taken {
			return "", invalid("FIELD_RELATION_INVERSE_TAKEN",
				"inverseField %q đã được dùng cho entity %q", norm, parentEntity)
		}
		claims.add(parentEntity, norm)
		return norm, nil
	}

	// Default: the owning (child) entity name.
	taken, err := inverseFieldTaken(ctx, repo, claims, tenantID, parentEntity, childEntity)
	if err != nil {
		return "", err
	}
	if !taken {
		claims.add(parentEntity, childEntity)
		return childEntity, nil
	}

	// Fallback: "<childEntity>_<fieldName>".
	fallback := childEntity + "_" + fieldName
	taken, err = inverseFieldTaken(ctx, repo, claims, tenantID, parentEntity, fallback)
	if err != nil {
		return "", err
	}
	if taken {
		return "", invalid("FIELD_RELATION_INVERSE_TAKEN",
			"inverseField mặc định %q và fallback %q đều đã được dùng cho entity %q", childEntity, fallback, parentEntity)
	}
	claims.add(parentEntity, fallback)
	return fallback, nil
}

// relationshipAlreadyExists returns the canonical 409 RELATIONSHIP_ALREADY_EXISTS
// ValidationError for a sys_relationships (tenant_id, name) collision (migration 000025's
// uq_sysrel_tenant_name index) surfaced via metadata.ErrDuplicateRelationship. It reuses the
// EXACT SAME code the N-N RegisterRelationship duplicate-name path already returns
// (relation_service.go) — both flow through the same handler-level statusForCode entry, so a
// name collision is indistinguishable to the client regardless of which writer (admin N-N
// registration, or a relation field's auto-registered 1-N registry row) raised it. `name` is
// the best-effort identifier for the message: the specific colliding relationship name when the
// caller has exactly one (AddField), or a comma-joined list of pending candidate names for a
// batch CreateEntity call where the specific colliding row is not individually resolved.
func relationshipAlreadyExists(name string) error {
	return invalid("RELATIONSHIP_ALREADY_EXISTS", "quan hệ %q đã được đăng ký cho tenant này", name)
}

// validateRelationOptionsImmutable rejects an EditField options payload that changes (or
// drops) a relation field's relatedEntity / inverseField (ENG-REL2 #4, review MED-2). Both
// were resolved and claimed in the 1-N registry row at AddField/CreateEntity time; EditField
// never touches the registry, so letting either drift here would leave
// GET /metadata/relationships and the inverse-field collision pre-check running against stale
// data — and relatedEntity additionally defines what the physical <field>_id column POINTS AT.
// Immutable like fieldType (FIELD_TYPE_IMMUTABLE): changing the target means delete + re-add
// the field. Comparison is by trimmed string on each key, with present-vs-absent counting as a
// change (EditField applies options as a FULL replacement, so an omitted key would silently
// erase the stored value).
func validateRelationOptionsImmutable(fieldName string, stored, incoming map[string]any) error {
	// ENG-REL2 #5: options.cardinality is likewise immutable. Flipping 1-N <-> 1-1 after
	// creation changes the physical DDL shape (plain FK column + non-unique index vs a
	// CASE-guarded FK column + tenant-scoped UNIQUE key) — EditField runs NO DDL, so
	// allowing this would silently desync sys_fields.options from the actual obj_* schema
	// (a field claiming "1-1" while the physical column is still a plain 1-N index, or vice
	// versa). Same remedy as relatedEntity/inverseField: delete the field and re-add it.
	for _, key := range []string{"relatedEntity", "inverseField", "cardinality"} {
		oldRaw, oldOK := stored[key]
		newRaw, newOK := incoming[key]
		oldS, _ := oldRaw.(string)
		newS, _ := newRaw.(string)
		if oldOK != newOK || strings.TrimSpace(oldS) != strings.TrimSpace(newS) {
			return invalid("FIELD_RELATION_OPTIONS_IMMUTABLE",
				"không thể đổi options.%s của field relation %q — xoá field và tạo lại nếu cần trỏ chỗ khác", key, fieldName)
		}
	}
	return nil
}

// newInverseRelationship builds the sys_relationships registry row for a `relation` field
// (1-N, the ordinary FK-on-child shape, or 1-1, ENG-REL2 #5's tenant-scoped-UNIQUE
// variant — selected by relType, the field's resolved options.cardinality). childEntity
// owns the FK column (fieldName + "_id"); parentEntity is options.relatedEntity. tenantID
// must be server-resolved (never client input, mirroring every other metadata write).
// JoinTableName is left nil (neither shape has a join table — see metadata.go's doc comment
// on why this MUST be nil, not an empty string, given the 000025 UNIQUE(tenant_id,
// join_table_name) index).
func newInverseRelationship(tenantID, parentEntity, childEntity, fieldName, inverseField, relType string) metadata.RelationshipDefinition {
	return metadata.RelationshipDefinition{
		ID:               uuid.NewString(),
		TenantID:         tenantID,
		Name:             childEntity + "_" + fieldName,
		ParentEntityName: parentEntity,
		ChildEntityName:  childEntity,
		RelationshipType: relType,
		InverseField:     inverseField,
		ChildFKColumn:    fieldName + "_id",
	}
}
