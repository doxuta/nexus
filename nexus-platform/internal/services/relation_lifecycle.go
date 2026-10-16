package services

// W2 (UX revolution — Relationship Canvas) relationship LIFECYCLE operations, addressed by
// sys_relationships row id: impact pre-check, patch (immutability contract), and delete
// ("gỡ nối"). These deliberately REUSE the existing primitives instead of duplicating them:
//
//   - 1-N / 1-1 delete rides FieldService.DeleteField — the SAME code path as
//     DELETE /admin/fields/:id — so the relation field is soft-deleted, its companion
//     registry row is cleaned up (SoftDeleteRelationshipByChildFK), the metadata cache is
//     invalidated, and the RM-19 field audit row is emitted, all by the one existing flow.
//   - N-N delete soft-deletes the registry row with tombstone-renamed unique columns
//     (metadata.SoftDeleteRelationshipByID) and does NOT drop the rel_* join table
//     (grace window — re-registering the same pair reuses the table and its links).
//
// Child-record FK values on 1-N/1-1 delete: per the field soft-delete convention (§4.4),
// NO DDL runs and the physical `<field>_id` column KEEPS its values — they simply become
// inert/invisible because the field is no longer in metadata. Reversible; nothing is wiped.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// relationLifecycleMeta is the metadata-registry read/write surface the lifecycle service
// needs. Satisfied by *metadata.Repository; declared consumer-side for unit-testability.
type relationLifecycleMeta interface {
	GetRelationshipByID(ctx context.Context, tenantID, relID string) (*metadata.RelationshipDefinition, error)
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	SoftDeleteRelationshipByID(ctx context.Context, tenantID, relID string) error
	UpdateRelationshipViewConfig(ctx context.Context, tenantID, relID string, viewConfig *string) error
}

// relationImpactCounts is the obj_*/rel_* counting surface behind the impact pre-check.
// Satisfied by *dynrepo.Repository; declared consumer-side for unit-testability.
type relationImpactCounts interface {
	// CountRelationFKRefs counts live child records whose <field>_id FK is non-NULL.
	CountRelationFKRefs(ctx context.Context, entity, tenantID, field string) (int, error)
	// CountJoinRows counts the tenant's link rows in rel_<a>_<b>.
	CountJoinRows(ctx context.Context, entityA, entityB, tenantID string) (int, error)
	// JoinTableExists reports whether rel_<a>_<b> physically exists (guards CountJoinRows).
	JoinTableExists(ctx context.Context, entityA, entityB string) (bool, error)
}

// relationFieldDeleter is the single primitive the 1-N/1-1 delete path rides: the SAME
// DeleteField flow DELETE /admin/fields/:id uses (field soft-delete + registry-row cleanup
// + cache invalidation + RM-19 audit). Satisfied by *FieldService.
type relationFieldDeleter interface {
	DeleteField(ctx context.Context, tenantID string, in DeleteFieldInput) error
}

// RelationDependent identifies a computed field that depends on a relationship —
// a rollup (blocking: its read path breaks or silently freezes without the relation) or a
// lookup (informational: applyLookups skips a missing via-field defensively, fail-soft).
type RelationDependent struct {
	EntityName string `json:"entityName"`
	FieldName  string `json:"fieldName"`
}

// RelationImpact is the GET /admin/relationships/:id/impact payload: everything the Studio
// canvas needs to render the guarded-delete manifest BEFORE the destructive confirm.
type RelationImpact struct {
	// Relationship echoes the full registry row (same shape as GET /metadata/relationships).
	Relationship *metadata.RelationshipDefinition `json:"relationship"`
	// AffectedRecords: 1-N/1-1 → live child records holding a non-NULL FK; N-N → the
	// tenant's join-table link rows (links to soft-deleted endpoints included — every link
	// row stops being reachable once the registration is removed).
	AffectedRecords int `json:"affectedRecords"`
	// DependentRollups lists rollup fields that reference this relationship
	// (options.viaRelation for 1-N/1-1, options.relationship for N-N). Non-empty ⇒ delete
	// is refused with 409 RELATIONSHIP_HAS_DEPENDENTS until they are removed.
	DependentRollups []RelationDependent `json:"dependentRollups"`
	// DependentLookups lists lookup fields on the child entity riding this relation field
	// (options.via). Informational only: lookups fail soft (resolve to nothing) after the
	// delete, so they do not block it — but the canvas should show them in the manifest.
	DependentLookups []RelationDependent `json:"dependentLookups"`
	// Deletable is false when blocking dependents exist (== len(DependentRollups) > 0).
	Deletable bool `json:"deletable"`
}

// PatchRelationshipInput is the PATCH /admin/relationships/:id body. EVERY key of a
// relationship's identity is structurally immutable (see Patch's doc comment) — the input
// exists so a change ATTEMPT is rejected with the canonical code instead of silently
// ignored. Pointer semantics: nil = key absent = untouched; a supplied value equal to the
// stored one is a no-op.
type PatchRelationshipInput struct {
	Name             *string
	ParentEntityName *string
	ChildEntityName  *string
	RelationshipType *string
	InverseField     *string
	ChildFKColumn    *string
	JoinTableName    *string
}

// RelationLifecycleService orchestrates the W2 relationship lifecycle (impact/patch/delete).
type RelationLifecycleService struct {
	meta   relationLifecycleMeta
	counts relationImpactCounts
	fields relationFieldDeleter
	logger *zap.Logger
}

// NewRelationLifecycleService wires the lifecycle service over the metadata repo, the
// dynamic repo (counts), and the field service (the reused 1-N/1-1 delete primitive).
func NewRelationLifecycleService(meta relationLifecycleMeta, counts relationImpactCounts, fields relationFieldDeleter, opts ...func(*RelationLifecycleService)) *RelationLifecycleService {
	s := &RelationLifecycleService{meta: meta, counts: counts, fields: fields, logger: zap.NewNop()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithRelationLifecycleLogger injects a real logger.
func WithRelationLifecycleLogger(l *zap.Logger) func(*RelationLifecycleService) {
	return func(s *RelationLifecycleService) {
		if l != nil {
			s.logger = l
		}
	}
}

// relationshipNotFound is the canonical 404 for an unknown / cross-tenant / already-deleted
// relationship id — all three resolve identically through the tenant-pinned GetRelationshipByID,
// so no information leaks about other tenants' rows (mirrors FIELD_NOT_FOUND semantics).
func relationshipNotFound(relID string) error {
	return invalid("RELATIONSHIP_NOT_FOUND", "quan hệ %q không tồn tại", relID)
}

// fkFieldName derives the owning relation FIELD name from a 1-N/1-1 registry row's
// child_fk_column ("customer_id" → "customer"). Registry rows written since migration
// 000028 always carry the column; empty (defensive) yields "".
func fkFieldName(rel *metadata.RelationshipDefinition) string {
	return strings.TrimSuffix(rel.ChildFKColumn, "_id")
}

// isFKShape reports whether the relationship is one of the FK-on-child shapes (owned by a
// relation field on the child entity), as opposed to a join-table N-N registration.
func isFKShape(rel *metadata.RelationshipDefinition) bool {
	return rel.RelationshipType == "1-N" || rel.RelationshipType == "1-1"
}

// Impact resolves the delete-impact manifest for a relationship (tenant-scoped by id).
// Unknown/cross-tenant/deleted id → RELATIONSHIP_NOT_FOUND (404). Never mutates anything.
func (s *RelationLifecycleService) Impact(ctx context.Context, tenantID, relID string) (*RelationImpact, error) {
	rel, err := s.meta.GetRelationshipByID(ctx, tenantID, relID)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return nil, relationshipNotFound(relID)
		}
		return nil, fmt.Errorf("services.RelationLifecycle.Impact: resolve: %w", err)
	}

	affected := 0
	if isFKShape(rel) {
		if field := fkFieldName(rel); field != "" {
			affected, err = s.counts.CountRelationFKRefs(ctx, rel.ChildEntityName, tenantID, field)
			if err != nil {
				return nil, fmt.Errorf("services.RelationLifecycle.Impact: count FK refs: %w", err)
			}
		}
	} else {
		exists, err := s.counts.JoinTableExists(ctx, rel.ParentEntityName, rel.ChildEntityName)
		if err != nil {
			return nil, fmt.Errorf("services.RelationLifecycle.Impact: join table exists: %w", err)
		}
		if exists {
			affected, err = s.counts.CountJoinRows(ctx, rel.ParentEntityName, rel.ChildEntityName, tenantID)
			if err != nil {
				return nil, fmt.Errorf("services.RelationLifecycle.Impact: count join rows: %w", err)
			}
		}
	}

	rollups, lookups, err := s.dependents(ctx, tenantID, rel)
	if err != nil {
		return nil, err
	}

	return &RelationImpact{
		Relationship:     rel,
		AffectedRecords:  affected,
		DependentRollups: rollups,
		DependentLookups: lookups,
		Deletable:        len(rollups) == 0,
	}, nil
}

// Patch is the EDIT surface for a relationship — and it deliberately allows NOTHING
// structural to change:
//
//   - relatedEntity (parent/child), inverseField, and cardinality (relationshipType) are
//     pinned to the physical schema and the registry contract — the SAME immutability
//     EditField already enforces per-field via validateRelationOptionsImmutable
//     (FIELD_RELATION_OPTIONS_IMMUTABLE). Letting a relationship-level PATCH mutate them
//     would desync sys_relationships from sys_fields.options and from the obj_* DDL shape.
//   - name is referenced by N-N rollups (options.relationship) and, for 1-N rows, derived
//     from <childEntity>_<fieldName>; renaming would break both.
//   - joinTableName / childFkColumn name physical objects.
//
// sys_relationships has no cosmetic columns (label/description) today, so there is nothing
// mutable at all: a 1-N relation's display label lives on the relation FIELD and is edited
// via the existing PUT /admin/fields/:id. This endpoint therefore validates any supplied
// key against the stored row — a differing value is rejected 422
// FIELD_RELATION_OPTIONS_IMMUTABLE (details.field = offending key); equal/absent keys
// no-op and the current row is echoed back (200). "Đổi cardinality/target = xoá và tạo lại."
func (s *RelationLifecycleService) Patch(ctx context.Context, tenantID, relID string, in PatchRelationshipInput) (*metadata.RelationshipDefinition, error) {
	rel, err := s.meta.GetRelationshipByID(ctx, tenantID, relID)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return nil, relationshipNotFound(relID)
		}
		return nil, fmt.Errorf("services.RelationLifecycle.Patch: resolve: %w", err)
	}

	storedJoin := ""
	if rel.JoinTableName != nil {
		storedJoin = *rel.JoinTableName
	}
	checks := []struct {
		key      string
		supplied *string
		stored   string
	}{
		{"name", in.Name, rel.Name},
		{"parentEntityName", in.ParentEntityName, rel.ParentEntityName},
		{"childEntityName", in.ChildEntityName, rel.ChildEntityName},
		{"relationshipType", in.RelationshipType, rel.RelationshipType},
		{"inverseField", in.InverseField, rel.InverseField},
		{"childFkColumn", in.ChildFKColumn, rel.ChildFKColumn},
		{"joinTableName", in.JoinTableName, storedJoin},
	}
	for _, c := range checks {
		if c.supplied != nil && strings.TrimSpace(*c.supplied) != strings.TrimSpace(c.stored) {
			return nil, &ValidationError{
				Code: "FIELD_RELATION_OPTIONS_IMMUTABLE",
				Msg: fmt.Sprintf("không thể đổi %s của quan hệ %q — xoá quan hệ và tạo lại nếu cần cấu trúc khác",
					c.key, rel.Name),
				Field:   c.key,
				Details: map[string]any{"field": c.key},
			}
		}
	}
	return rel, nil
}

// Delete removes a relationship ("gỡ nối") — tenant-scoped by id, guarded by the dependent-
// rollup pre-check (409 RELATIONSHIP_HAS_DEPENDENTS before ANY mutation):
//
//   - 1-N / 1-1: resolves the owning relation field on the child entity (from
//     child_fk_column) and REUSES FieldService.DeleteField — identical semantics to
//     DELETE /admin/fields/:id (field soft-deleted, registry row soft-deleted via
//     SoftDeleteRelationshipByChildFK, cache invalidated, RM-19 audit emitted). Child
//     records' FK values are KEPT (no DDL, §4.4) — inert once the field is gone.
//     An orphaned registry row (owning field already gone — e.g. the rare DeleteField
//     step-3 failure documented in field_orchestration.go) falls back to soft-deleting
//     the row itself, making this endpoint the in-product repair path.
//   - N-N: soft-deletes the registry row with tombstoned unique columns; the rel_* join
//     table and its link rows are preserved (grace window — re-registering the same pair
//     reuses them).
//
// A second Delete of the same id returns RELATIONSHIP_NOT_FOUND (404): the row is already
// soft-deleted, indistinguishable from never-existed (same convention as field delete).
//
// TOCTOU (accepted, not fixed): the dependents check (s.dependents, below) and the actual
// mutation (deleteFKShape / SoftDeleteRelationshipByID) run as SEPARATE reads/transactions —
// there is no single transaction spanning "list rollups referencing this relationship" and
// "delete the relationship". A concurrent POST /admin/fields adding a rollup field whose
// options.viaRelation/options.relationship names THIS relationship can race between the
// dependents scan (sees zero) and the delete committing, leaving that brand-new rollup
// orphaned (its relation now gone — reads it as RECORD_ROLLUP_UNAVAILABLE / frozen, same as
// the state this gate exists to prevent). Accepted rather than fixed: both sides of the race
// are same-tenant admin-gated config-time operations (not a cross-tenant or record-data
// integrity issue), the window is a single AddField call, and the outcome is recoverable —
// an admin deletes the orphaned rollup field, exactly the manual step this gate normally
// forces BEFORE the relationship delete. Closing it properly would need either a DB-level
// advisory lock or moving the whole delete (dependents scan + field/registry mutation) into
// one transaction spanning metadata AND (for the 1-N path) the reused DeleteField call —
// deferred until this race is observed in practice rather than built defensively.
func (s *RelationLifecycleService) Delete(ctx context.Context, tenantID, relID string) error {
	rel, err := s.meta.GetRelationshipByID(ctx, tenantID, relID)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return relationshipNotFound(relID)
		}
		return fmt.Errorf("services.RelationLifecycle.Delete: resolve: %w", err)
	}

	// Dependent-rollup gate BEFORE any mutation: an N-N rollup's read path returns
	// RECORD_ROLLUP_UNAVAILABLE on every record once the registry row is gone, and a 1-N
	// rollup silently freezes on the surviving physical column — both are broken states an
	// admin must resolve explicitly (delete the rollup fields first). See the TOCTOU note
	// above the func signature: this check and the mutation below are not atomic together.
	rollups, _, err := s.dependents(ctx, tenantID, rel)
	if err != nil {
		return err
	}
	if len(rollups) > 0 {
		deps := make([]map[string]any, 0, len(rollups))
		for _, d := range rollups {
			deps = append(deps, map[string]any{"entityName": d.EntityName, "fieldName": d.FieldName})
		}
		return &ValidationError{
			Code: "RELATIONSHIP_HAS_DEPENDENTS",
			Msg: fmt.Sprintf("không thể xoá quan hệ %q: %d field rollup đang phụ thuộc — xoá các field đó trước",
				rel.Name, len(rollups)),
			Details: map[string]any{"dependentRollups": deps},
		}
	}

	if isFKShape(rel) {
		if err := s.deleteFKShape(ctx, tenantID, rel); err != nil {
			return err
		}
	} else {
		if err := s.meta.SoftDeleteRelationshipByID(ctx, tenantID, rel.ID); err != nil {
			if errors.Is(err, metadata.ErrRelationshipNotFound) {
				return relationshipNotFound(relID)
			}
			return fmt.Errorf("services.RelationLifecycle.Delete: soft delete N-N: %w", err)
		}
	}

	s.logger.Info("relationship deleted",
		zap.String("tenant_id", tenantID),
		zap.String("relationship_id", rel.ID),
		zap.String("name", rel.Name),
		zap.String("relationship_type", rel.RelationshipType))
	return nil
}

// deleteFKShape deletes a 1-N/1-1 relationship by soft-deleting its owning relation field
// through the reused DeleteField primitive, falling back to a direct registry-row cleanup
// when the owning field (or its entity) is already gone.
func (s *RelationLifecycleService) deleteFKShape(ctx context.Context, tenantID string, rel *metadata.RelationshipDefinition) error {
	fieldName := fkFieldName(rel)
	if fieldName != "" {
		entity, err := s.meta.GetEntityByName(ctx, tenantID, rel.ChildEntityName)
		switch {
		case err == nil:
			fields, lerr := s.meta.ListFields(ctx, tenantID, entity.ID)
			if lerr != nil {
				return fmt.Errorf("services.RelationLifecycle.Delete: list child fields: %w", lerr)
			}
			for _, f := range fields {
				if f.Name == fieldName && f.FieldType == "relation" {
					// Reuse: the SAME flow as DELETE /admin/fields/:id — soft-deletes the
					// field AND this registry row (child-FK match), invalidates cache, audits.
					// specs/063 FR-070: a relationship is "Áp ngay", not drafted. The field delete would
					// otherwise fork into a "sẽ xoá" draft on a delivered app while this method reports
					// success; the apply marker (server-side only, never from HTTP) keeps it live.
					if derr := s.fields.DeleteField(metaversion.WithPublishApply(ctx), tenantID, DeleteFieldInput{FieldID: f.ID}); derr != nil {
						return fmt.Errorf("services.RelationLifecycle.Delete: delete relation field: %w", derr)
					}
					return nil
				}
			}
			// Owning field not found among active fields → orphaned registry row; fall through.
		case errors.Is(err, metadata.ErrEntityNotFound):
			// Child entity already gone → orphaned registry row; fall through to direct cleanup.
		default:
			return fmt.Errorf("services.RelationLifecycle.Delete: resolve child entity: %w", err)
		}
	}
	// Orphan cleanup: the registry row outlived its owning field (or carries no FK column).
	// Soft-delete the row directly (tombstoned) so the registry stops advertising it.
	if err := s.meta.SoftDeleteRelationshipByID(ctx, tenantID, rel.ID); err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return relationshipNotFound(rel.ID)
		}
		return fmt.Errorf("services.RelationLifecycle.Delete: soft delete orphan registry row: %w", err)
	}
	return nil
}

// dependents scans for computed fields that reference the relationship:
//
//   - 1-N/1-1: rollups live on the PARENT entity (config-time validation pins
//     options.viaRelation's relatedEntity to the parent) and match on
//     viaRelation == <owning field> && childEntity == <child entity>; lookups live on the
//     CHILD entity (sibling rule) and match on via == <owning field>.
//   - N-N: rollups may live on EITHER side and match on options.relationship == rel.Name;
//     lookups never ride N-N (options.via requires a to-one relation field) → always empty.
//
// The scan is bounded metadata work (ListFields on at most two entities, options parsed in
// Go) on an admin-time endpoint — no JSON_EXTRACT table scans, no per-record loops (B5-safe).
// A side entity that no longer resolves (soft-deleted) simply contributes no dependents.
func (s *RelationLifecycleService) dependents(ctx context.Context, tenantID string, rel *metadata.RelationshipDefinition) (rollups, lookups []RelationDependent, err error) {
	rollups = []RelationDependent{}
	lookups = []RelationDependent{}

	scan := func(entityName string, visit func(f metadata.FieldDefinition, opts map[string]any)) error {
		entity, gerr := s.meta.GetEntityByName(ctx, tenantID, entityName)
		if gerr != nil {
			if errors.Is(gerr, metadata.ErrEntityNotFound) {
				return nil // side entity already gone → no dependents on that side
			}
			return fmt.Errorf("services.RelationLifecycle.dependents: resolve entity %q: %w", entityName, gerr)
		}
		fields, lerr := s.meta.ListFields(ctx, tenantID, entity.ID)
		if lerr != nil {
			return fmt.Errorf("services.RelationLifecycle.dependents: list fields of %q: %w", entityName, lerr)
		}
		for _, f := range fields {
			if f.FieldType != "rollup" && f.FieldType != "lookup" {
				continue
			}
			opts, perr := parseOptionsJSON(f.Options)
			if perr != nil {
				return fmt.Errorf("services.RelationLifecycle.dependents: parse options of %s.%s: %w", entityName, f.Name, perr)
			}
			visit(f, opts)
		}
		return nil
	}

	strOpt := func(opts map[string]any, key string) string {
		v, _ := opts[key].(string)
		return strings.TrimSpace(v)
	}

	if isFKShape(rel) {
		fieldName := fkFieldName(rel)
		if fieldName == "" {
			return rollups, lookups, nil // defensive: no FK column → nothing can reference it
		}
		// Parent side: rollups in FK mode (options.viaRelation names the child's relation field).
		if err := scan(rel.ParentEntityName, func(f metadata.FieldDefinition, opts map[string]any) {
			if f.FieldType == "rollup" &&
				strOpt(opts, "viaRelation") == fieldName &&
				strOpt(opts, "childEntity") == rel.ChildEntityName {
				rollups = append(rollups, RelationDependent{EntityName: rel.ParentEntityName, FieldName: f.Name})
			}
		}); err != nil {
			return nil, nil, err
		}
		// Child side: sibling lookups riding this relation field (options.via).
		if err := scan(rel.ChildEntityName, func(f metadata.FieldDefinition, opts map[string]any) {
			if f.FieldType == "lookup" && strOpt(opts, "via") == fieldName {
				lookups = append(lookups, RelationDependent{EntityName: rel.ChildEntityName, FieldName: f.Name})
			}
		}); err != nil {
			return nil, nil, err
		}
		return rollups, lookups, nil
	}

	// N-N: rollups referencing options.relationship may live on either side.
	sides := []string{rel.ParentEntityName}
	if rel.ChildEntityName != rel.ParentEntityName {
		sides = append(sides, rel.ChildEntityName)
	}
	for _, side := range sides {
		sideName := side
		if err := scan(sideName, func(f metadata.FieldDefinition, opts map[string]any) {
			if f.FieldType == "rollup" && strOpt(opts, "relationship") == rel.Name {
				rollups = append(rollups, RelationDependent{EntityName: sideName, FieldName: f.Name})
			}
		}); err != nil {
			return nil, nil, err
		}
	}
	return rollups, lookups, nil
}

// ─────────────────────────────────────────────────────────────────────────────────────────
// ENG-RELVIEW — per-relationship related-list ("subpanel") view config (migration 000044).
// ─────────────────────────────────────────────────────────────────────────────────────────

// relViewLabelMaxRunes caps view_config.label — same budget as every other display label
// column in the metadata registry (sys_entities.label / sys_fields.label VARCHAR(100)).
const relViewLabelMaxRunes = 100

// relViewMaxColumns caps view_config.columns. A subpanel is a PREVIEW list on a detail
// view, not a full list screen; more than a dozen columns is a config smell, and the cap
// keeps the stored document trivially bounded.
const relViewMaxColumns = 12

// relViewSystemSortFields are the always-sortable system columns, mirroring the fixed part
// of buildSortAllowlist (record_orchestration.go) so config-time acceptance == run-time
// acceptance of GET /:entity?sort= (validator-parity, B6).
var relViewSystemSortFields = map[string]struct{}{
	"created_at": {}, "updated_at": {}, "id": {},
}

// SetViewConfig validates and persists the related-list view config of one relationship
// (PUT /admin/relationships/:id/view-config). raw is the request body verbatim:
//
//   - JSON `null` (or an empty body slot) CLEARS the config → sys_relationships.view_config
//     = SQL NULL → the FE falls back to today's auto-derived rendering. Returns (nil, nil).
//   - a JSON object is validated against the shape
//     {label?, order?, columns?, sort?, allowCreate?, allowLink?} and against the CHILD
//     entity's live field set, then stored re-marshaled (canonical, insignificant
//     whitespace dropped). Returns the parsed config for the handler to echo.
//
// Validation rules (all 422 via the ValidationError codes below):
//
//	RELVIEW_INVALID           — not an object, unknown key, wrong value type, label >100
//	                            runes, order not an integer, >relViewMaxColumns columns,
//	                            duplicate column, malformed sort object, dir ∉ {asc, desc}.
//	RELVIEW_COLUMN_UNKNOWN    — a columns entry (or sort.field) that is not a live field of
//	                            the CHILD entity (details.field names the offender).
//	RELVIEW_SORT_NOT_SORTABLE — sort.field exists but has no physical sort column (not
//	                            unique/relation/indexed and not a system column). Config-time
//	                            parity with the run-time sort allowlist (RM-09): a stored
//	                            config must never 422 later when the FE renders it.
//
// Tenant-pinned end to end: the relationship resolves through the tenant-scoped
// GetRelationshipByID (unknown / cross-tenant / soft-deleted → 404 RELATIONSHIP_NOT_FOUND),
// the child entity + fields through the same tenant, and the UPDATE re-pins tenant_id.
func (s *RelationLifecycleService) SetViewConfig(ctx context.Context, tenantID, relID string, raw json.RawMessage) (map[string]any, error) {
	rel, err := s.meta.GetRelationshipByID(ctx, tenantID, relID)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return nil, relationshipNotFound(relID)
		}
		return nil, fmt.Errorf("services.RelationLifecycle.SetViewConfig: resolve: %w", err)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if err := s.meta.UpdateRelationshipViewConfig(ctx, tenantID, rel.ID, nil); err != nil {
			return nil, fmt.Errorf("services.RelationLifecycle.SetViewConfig: clear: %w", err)
		}
		s.logger.Info("relationship view config cleared",
			zap.String("tenant_id", tenantID), zap.String("relationship_id", rel.ID), zap.String("name", rel.Name))
		return nil, nil
	}

	var cfg map[string]any
	if err := json.Unmarshal(trimmed, &cfg); err != nil {
		return nil, invalid("RELVIEW_INVALID", "view_config phải là JSON object hoặc null")
	}
	if err := s.validateViewConfig(ctx, tenantID, rel, cfg); err != nil {
		return nil, err
	}

	canonical, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("services.RelationLifecycle.SetViewConfig: marshal: %w", err)
	}
	stored := string(canonical)
	if err := s.meta.UpdateRelationshipViewConfig(ctx, tenantID, rel.ID, &stored); err != nil {
		return nil, fmt.Errorf("services.RelationLifecycle.SetViewConfig: persist: %w", err)
	}
	s.logger.Info("relationship view config updated",
		zap.String("tenant_id", tenantID), zap.String("relationship_id", rel.ID), zap.String("name", rel.Name))
	return cfg, nil
}

// validateViewConfig enforces the view_config shape + child-field allowlist for SetViewConfig.
// The child field set is loaded lazily — only when columns/sort actually reference fields —
// so a labels-only config never pays the metadata round-trip.
func (s *RelationLifecycleService) validateViewConfig(ctx context.Context, tenantID string, rel *metadata.RelationshipDefinition, cfg map[string]any) error {
	for k := range cfg {
		switch k {
		case "label", "order", "columns", "sort", "allowCreate", "allowLink":
		default:
			return invalidField("RELVIEW_INVALID", k, "view_config có key không hợp lệ: %q", k)
		}
	}

	if v, ok := cfg["label"]; ok {
		str, isStr := v.(string)
		if !isStr {
			return invalidField("RELVIEW_INVALID", "label", "label phải là chuỗi")
		}
		if utf8.RuneCountInString(str) > relViewLabelMaxRunes {
			return invalidField("RELVIEW_INVALID", "label", "label vượt quá %d ký tự", relViewLabelMaxRunes)
		}
	}
	if v, ok := cfg["order"]; ok {
		num, isNum := v.(float64)
		if !isNum || num != math.Trunc(num) {
			return invalidField("RELVIEW_INVALID", "order", "order phải là số nguyên")
		}
	}
	for _, key := range []string{"allowCreate", "allowLink"} {
		if v, ok := cfg[key]; ok {
			if _, isBool := v.(bool); !isBool {
				return invalidField("RELVIEW_INVALID", key, "%s phải là true/false", key)
			}
		}
	}

	// Lazy child-field allowlists — only built when columns/sort reference fields.
	var fieldNames map[string]struct{}    // every live child field name
	var sortableNames map[string]struct{} // subset with a physical sort column (RM-09 parity)
	loadChildFields := func() error {
		if fieldNames != nil {
			return nil
		}
		entity, err := s.meta.GetEntityByName(ctx, tenantID, rel.ChildEntityName)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				// The child side is gone (soft-deleted) — a field-referencing config is
				// unresolvable. Same code/status the metadata surface uses for a dead entity.
				return invalid("ENTITY_NOT_FOUND", "entity con %q của quan hệ không tồn tại", rel.ChildEntityName)
			}
			return fmt.Errorf("services.RelationLifecycle.validateViewConfig: resolve child entity: %w", err)
		}
		fields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
		if err != nil {
			return fmt.Errorf("services.RelationLifecycle.validateViewConfig: list child fields: %w", err)
		}
		fieldNames = make(map[string]struct{}, len(fields))
		sortableNames = make(map[string]struct{}, len(fields))
		for _, f := range fields {
			fieldNames[f.Name] = struct{}{}
			opts, perr := parseOptionsJSON(f.Options)
			if perr != nil {
				return fmt.Errorf("services.RelationLifecycle.validateViewConfig: parse options of %s.%s: %w", rel.ChildEntityName, f.Name, perr)
			}
			// Mirrors buildSortAllowlist (record_orchestration.go): unique → own generated
			// column, relation/user → <field>_id, D1 indexed → own generated column.
			if f.IsUnique || f.FieldType == "relation" || f.FieldType == "user" || boolOpt(opts, "indexed") {
				sortableNames[f.Name] = struct{}{}
			}
		}
		return nil
	}

	if v, ok := cfg["columns"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return invalidField("RELVIEW_INVALID", "columns", "columns phải là mảng tên field")
		}
		if len(arr) > relViewMaxColumns {
			return invalidField("RELVIEW_INVALID", "columns", "columns tối đa %d field", relViewMaxColumns)
		}
		seen := make(map[string]struct{}, len(arr))
		for _, item := range arr {
			col, isStr := item.(string)
			if !isStr || strings.TrimSpace(col) == "" {
				return invalidField("RELVIEW_INVALID", "columns", "columns phải là mảng tên field (chuỗi khác rỗng)")
			}
			if _, dup := seen[col]; dup {
				return invalidField("RELVIEW_INVALID", "columns", "columns có field trùng lặp: %q", col)
			}
			seen[col] = struct{}{}
			if err := loadChildFields(); err != nil {
				return err
			}
			if _, exists := fieldNames[col]; !exists {
				return invalidField("RELVIEW_COLUMN_UNKNOWN", col,
					"field %q không tồn tại trên entity con %q", col, rel.ChildEntityName)
			}
		}
	}

	if v, ok := cfg["sort"]; ok {
		obj, isObj := v.(map[string]any)
		if !isObj {
			return invalidField("RELVIEW_INVALID", "sort", "sort phải là object {field, dir}")
		}
		for k := range obj {
			if k != "field" && k != "dir" {
				return invalidField("RELVIEW_INVALID", "sort", "sort có key không hợp lệ: %q", k)
			}
		}
		field, isStr := obj["field"].(string)
		if !isStr || strings.TrimSpace(field) == "" {
			return invalidField("RELVIEW_INVALID", "sort", "sort.field là bắt buộc")
		}
		if dirRaw, has := obj["dir"]; has {
			dir, dirStr := dirRaw.(string)
			if !dirStr || (dir != "asc" && dir != "desc") {
				return invalidField("RELVIEW_INVALID", "sort", "sort.dir phải là 'asc' hoặc 'desc'")
			}
		}
		if _, isSystem := relViewSystemSortFields[field]; !isSystem {
			if err := loadChildFields(); err != nil {
				return err
			}
			if _, exists := fieldNames[field]; !exists {
				return invalidField("RELVIEW_COLUMN_UNKNOWN", "sort.field",
					"field %q không tồn tại trên entity con %q", field, rel.ChildEntityName)
			}
			if _, sortable := sortableNames[field]; !sortable {
				return invalidField("RELVIEW_SORT_NOT_SORTABLE", "sort.field",
					"field %q không sort được (cần unique/relation/indexed hoặc cột hệ thống)", field)
			}
		}
	}
	return nil
}
