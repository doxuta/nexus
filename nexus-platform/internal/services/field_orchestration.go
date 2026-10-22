package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// FieldSchemaRepo materializes (and, on compensation, removes) the physical column an
// indexed field needs. Ordinary fields live inside the JSON `data` column and require no
// schema change, so only unique/relation fields ever reach this interface. Implemented by
// the sqlx dynamic-record repository; abstracted here so orchestration is unit-testable.
type FieldSchemaRepo interface {
	// AddUniqueGencolNonUnique / DropUniqueGencolNonUnique materialize (and, on compensation,
	// remove) a NEW `is_unique` field's physical column: the byte-identical gencol
	// AddUniqueColumn used to build, but a NON-unique tenant-scoped index (RM-28 Slice B —
	// uniqueness is enforced solely by the sys_unique_index pivot, never a physical UNIQUE
	// index on the shared obj_* table).
	AddUniqueGencolNonUnique(ctx context.Context, entity, field string) error
	DropUniqueGencolNonUnique(ctx context.Context, entity, field string) error
	// AddRelationColumn / DropRelationColumn materialize (and, on compensation, remove) the
	// CHAR(36) `<field>_id` FK column + non-unique tenant-scoped index a 1-N `relation`
	// field needs (M3, FK-on-child). Mirrors the unique-column pair.
	AddRelationColumn(ctx context.Context, entity, field string) error
	DropRelationColumn(ctx context.Context, entity, field string) error
	// AddOneToOneRelationColumn / DropOneToOneRelationColumn materialize (and, on
	// compensation, remove) the CASE-guarded CHAR(36) `<field>_id` FK column + tenant-scoped
	// UNIQUE key a 1-1 `relation` field needs (ENG-REL2 #5, options.cardinality == "1-1").
	// Distinct from AddRelationColumn/DropRelationColumn because the index shape differs
	// (UNIQUE vs non-unique) and therefore so does the index NAME (uq_ vs idx_ prefix) —
	// using the wrong pair to compensate would error.
	AddOneToOneRelationColumn(ctx context.Context, entity, field string) error
	DropOneToOneRelationColumn(ctx context.Context, entity, field string) error
	// ColumnExists reports whether the physical column for a field already exists on the
	// entity's obj_* table. AddField checks this before ALTERing a unique field so a
	// re-add whose generated column survived a soft-delete does not run ADD COLUMN onto an
	// existing column (which would error -> 500). See spec §7.2 B-1.
	ColumnExists(ctx context.Context, entity, field string) (bool, error)
	// AddIndexedColumn / DropIndexedColumn materialize (and on compensation remove) the
	// typed STORED generated column + non-unique tenant-scoped index for a field whose
	// options["indexed"] == true (D1). Mirror the unique-column pair. The fieldType arg
	// selects the column type per the D1 design table.
	AddIndexedColumn(ctx context.Context, entity, field, fieldType string) error
	DropIndexedColumn(ctx context.Context, entity, field string) error
	// AddPolymorphicColumns / DropPolymorphicColumns materialize (and on compensation remove) the
	// TWO STORED generated columns (<field>_type, <field>_id) + composite tenant-scoped index a
	// `polymorphic_relation` field needs (ACTIVITY-CORE #3a). One ALTER adds both columns + index;
	// the Drop reverses it. Distinct from the relation pair because the shape (two columns + a
	// 3-column composite index) differs.
	AddPolymorphicColumns(ctx context.Context, entity, field string) error
	DropPolymorphicColumns(ctx context.Context, entity, field string) error
	// DeletePivotByField removes ALL pivot rows of ONE tenant's (entity, field) from
	// sys_unique_index (RM-28 §3.3). DeleteField calls it for a UNIQUE field: DeleteField is
	// no-DDL (the obj_* generated column survives), so without this the pivot rows linger and,
	// after the field is re-created, would 409 a record the tenant can no longer see.
	DeletePivotByField(ctx context.Context, entity, tenantID, field string) error
	// BackfillUniqueField populates sys_unique_index from THIS tenant's live obj_<entity> rows
	// for one unique field (RM-28 §2.3/§4.1), AFTER the gencol/unique-index already exists
	// (materialized above by AddUniqueGencolNonUnique, or reused from a co-tenant's earlier AddField on
	// the same shared table). Idempotent — safe to call even when there is nothing to backfill.
	// A non-empty BackfillResult.Conflicts reports an intra-tenant dupe value the pre-scan
	// found; AddField maps it to 409 RECORD_DUPLICATE instead of silently under-covering the
	// pivot.
	BackfillUniqueField(ctx context.Context, entity, tenantID, field string) (*dynrepo.BackfillResult, error)
	// CountCurrencyCodeRows counts ONE tenant's rows whose currency field stores `code`, live and
	// in the recycle bin (a restore brings a trashed row back unchanged) — the rows EditField
	// would relabel by changing the field's configured currencyCode away from it (queue #157,
	// guardCurrencyCodeChange).
	CountCurrencyCodeRows(ctx context.Context, entity, tenantID, field, code string) (live, trashed int64, err error)
}

// FieldMetadataRepo reads the target entity + its existing fields and persists a new
// field definition. Implemented by the GORM metadata repository. All reads are
// tenant-scoped at the repository layer (GetEntityByName / GetFieldByID take tenantID).
type FieldMetadataRepo interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	// ListFieldsIncludingDeleted returns all fields of an entity INCLUDING soft-deleted
	// rows. AddField's dup-check uses this (not ListFields) so re-adding a name that was
	// soft-deleted is rejected instead of creating a phantom second row / 500. See §7.2 B-1.
	// tenantID is included for defense-in-depth (RM-18).
	ListFieldsIncludingDeleted(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	CreateField(ctx context.Context, f *metadata.FieldDefinition) error
	// GetFieldByID resolves a field by id, scoped to the tenant that owns its parent
	// entity; returns metadata.ErrFieldNotFound when no such field exists for the tenant.
	GetFieldByID(ctx context.Context, tenantID, fieldID string) (*metadata.FieldDefinition, error)
	// UpdateFieldMutable persists only the editable attributes (label/required/default/
	// options) of an already-resolved field; it never rewrites field_type or name. It
	// re-pins tenantID directly (defense-in-depth) so a forged/stale id from another tenant
	// updates 0 rows -> ErrFieldNotFound.
	UpdateFieldMutable(ctx context.Context, tenantID string, f *metadata.FieldDefinition) error
	// SoftDeleteField soft-deletes a field by id (sets deleted_at); it runs NO DDL and
	// preserves the underlying obj_* data. The caller resolves the field via
	// GetFieldByID (tenant-scoped) first; the tenantID is re-pinned directly here too
	// (defense-in-depth) so a forged/stale id from another tenant deletes 0 rows.
	SoftDeleteField(ctx context.Context, tenantID, fieldID string) error
	// GetEntityNameByID resolves an entity name by its id, scoped to the given tenant.
	// Returns metadata.ErrEntityNotFound when no active entity exists. EditField uses this
	// when DDL is needed (indexed toggle): the resolved FieldDefinition carries only EntityID,
	// not the entity name needed to target the obj_* table.
	GetEntityNameByID(ctx context.Context, tenantID, entityID string) (string, error)
	// GetRelationshipByName resolves a registered relationship (any type) by tenant + name,
	// returning metadata.ErrRelationshipNotFound when none exists. ENG-COMPUTED #3 (rollup)
	// uses it to validate a rollup field's options.relationship (N-N mode) at AddField time.
	GetRelationshipByName(ctx context.Context, tenantID, name string) (*metadata.RelationshipDefinition, error)
	// GetRelationshipByInverseField resolves the active relationship (any type) claiming a
	// given (parent entity, inverse field) pair, scoped to the tenant. ENG-REL2 #4 (AddField's
	// 1-N registry write) uses it to detect an inverse-field naming collision before
	// registering a new 1-N relationship row. Returns metadata.ErrRelationshipNotFound when the
	// name is unclaimed (not an error case — "available").
	GetRelationshipByInverseField(ctx context.Context, tenantID, parentEntityName, inverseField string) (*metadata.RelationshipDefinition, error)
	// CreateFieldWithRelationship persists a field definition AND its companion 1-N
	// sys_relationships registry row in ONE transaction (ENG-REL2 #4): used ONLY for a
	// `relation`-type field, so the field row and its registry row land atomically (or neither
	// does). Every other field type continues to use CreateField (rel is always nil there).
	CreateFieldWithRelationship(ctx context.Context, f *metadata.FieldDefinition, rel *metadata.RelationshipDefinition) error
	// SoftDeleteRelationshipByChildFK soft-deletes the 1-N registry row matching (tenant, child
	// entity, child FK column), scoped to relationship_type='1-N'. DeleteField calls this for a
	// `relation`-type field so the registry row does not outlive the field it describes. A
	// missing row (pre-000028 field, or already cleaned up) is a no-op, not an error.
	SoftDeleteRelationshipByChildFK(ctx context.Context, tenantID, childEntityName, childFKColumn string) error
}

// FieldService orchestrates adding a field to an existing entity. Ordinary fields are a
// pure metadata write (no schema change). Unique fields need a generated column + index,
// so the flow is: ALTER (auto-commits) → persist metadata → compensating DROP if the
// metadata write fails (MySQL cannot roll DDL back inside the metadata transaction).
type FieldService struct {
	schema FieldSchemaRepo
	meta   FieldMetadataRepo
	// reorder is the W5 field-reorder seam (see FieldReorderRepo in field_reorder.go). It is
	// auto-wired when the metadata repo also implements it (the concrete *metadata.Repository
	// does), so every existing wiring site gets PUT /admin/fields/reorder for free; unit-test
	// fakes that don't implement it simply leave reorder nil (ReorderFields then errors).
	reorder FieldReorderRepo
	// drafts is the S7-8 draft layer of a field's editable attributes (WithDraftFields). nil = every
	// edit goes straight to the registry, the pre-slice behaviour unit-test wirings rely on.
	drafts *DraftFieldRepo
	// cache, when set, is the shared metadata cache; field writes invalidate the entity's cached
	// field list AFTER commit. Nil = no caching (Invalidate* is nil-safe).
	cache *MetaCache
	// auditor records committed field-metadata mutations (RM-19), async + fail-open.
	// Defaults to a noop so existing wiring/tests run unchanged; injected via WithAuditor.
	auditor audit.Auditor
	// restrictions backs the queue #142 guard in EditField (WithFieldRestrictions). nil = guard
	// off (unit tests that predate it); production wires *auth.FieldACLRepository.
	restrictions FieldRestrictionLister
	// formulaRows backs the stored-formula sweep AddField/EditField run on a formula field
	// (recomputeStoredFormulas). Auto-wired when the schema repo also implements it (the concrete
	// *dynrepo.Repository does — production and integration wiring); unit-test fakes that do not
	// simply leave it nil, which skips the sweep.
	formulaRows formulaRowStore
	// layouts backs the queue #259 guard in EditField (WithLayoutReader). nil = guard off.
	layouts layoutReader
	// storedRollupSweep recomputes one stored roll-up over the rows that already exist (specs/083):
	// AddField runs it for a new stored roll-up, RecomputeField for the "Tính lại" command. nil =
	// off (unit fakes); main.go wires RecordService.RecomputeStoredRollup, which needs the record
	// service and so is attached after both are built.
	storedRollupSweep func(ctx context.Context, tenantID, entityName, fieldName string) (int, error)
}

// WithStoredRollupSweep wires the stored roll-up backfill (specs/083). Chainable.
func (s *FieldService) WithStoredRollupSweep(f func(ctx context.Context, tenantID, entityName, fieldName string) (int, error)) *FieldService {
	s.storedRollupSweep = f
	return s
}

// RecomputeField is the "Tính lại" command for a stored roll-up: it recomputes the field's value on
// every row of its entity and returns how many rows changed. A field that is not a stored roll-up
// is 422 FIELD_ROLLUP_NOT_STORED; an unknown/cross-tenant id is 404 (the lookup is tenant-scoped).
func (s *FieldService) RecomputeField(ctx context.Context, tenantID, fieldID string) (int, error) {
	field, err := s.meta.GetFieldByID(ctx, tenantID, strings.TrimSpace(fieldID))
	if err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return 0, invalid("FIELD_NOT_FOUND", "field %q không tồn tại", fieldID)
		}
		return 0, fmt.Errorf("services.RecomputeField: resolve field: %w", err)
	}
	opts, err := parseOptionsJSON(field.Options)
	if err != nil {
		return 0, fmt.Errorf("services.RecomputeField: %w", err)
	}
	if field.FieldType != "rollup" || !boolOpt(opts, "stored") {
		return 0, invalid("FIELD_ROLLUP_NOT_STORED", "trường %q không phải trường tổng hợp có lưu giá trị", field.Name)
	}
	if s.storedRollupSweep == nil {
		return 0, fmt.Errorf("services.RecomputeField: no sweep wired")
	}
	entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
	if err != nil {
		return 0, fmt.Errorf("services.RecomputeField: resolve entity: %w", err)
	}
	n, err := s.storedRollupSweep(ctx, tenantID, entityName, field.Name)
	if err != nil {
		return n, fmt.Errorf("services.RecomputeField: %w", err)
	}
	return n, nil
}

// FieldRestrictionLister is the slice of *auth.FieldACLRepository the EditField lock guard reads:
// the tenant's restrictive sys_field_acl rows on one field, joined to their roles.
type FieldRestrictionLister interface {
	ListFieldRestrictions(ctx context.Context, tenantID, entity, field string) ([]auth.FieldACLRestriction, error)
}

// Pins the seam to what cmd/server/main.go wires: if the repository ever stops satisfying it, the
// build breaks instead of the guard going quiet.
var _ FieldRestrictionLister = (*auth.FieldACLRepository)(nil)

// WithFieldRestrictions wires the queue #142 guard: EditField refuses to make a field
// required-without-fill while any role has it read-only or hidden. Chainable.
func (s *FieldService) WithFieldRestrictions(l FieldRestrictionLister) *FieldService {
	s.restrictions = l
	return s
}

// WithLayoutReader wires the queue #259 guard: EditField refuses to make a field required while
// the entity's edit layout hides it behind a visibleWhen condition (specs/054 FR-6). Chainable.
func (s *FieldService) WithLayoutReader(r layoutReader) *FieldService {
	s.layouts = r
	return s
}

// NewFieldService wires the orchestration over a schema repo and a metadata repo. When meta
// also implements FieldReorderRepo (the concrete *metadata.Repository does) the W5 reorder
// seam is wired automatically.
func NewFieldService(schema FieldSchemaRepo, meta FieldMetadataRepo) *FieldService {
	s := &FieldService{schema: schema, meta: meta, auditor: audit.NewNoopAuditor()}
	if rr, ok := meta.(FieldReorderRepo); ok {
		s.reorder = rr
	}
	if fr, ok := schema.(formulaRowStore); ok {
		s.formulaRows = fr
	}
	return s
}

// WithFormulaRows overrides the stored-formula sweep's store (unit tests whose schema fake does
// not implement formulaRowStore). Chainable.
func (s *FieldService) WithFormulaRows(fr formulaRowStore) *FieldService {
	s.formulaRows = fr
	return s
}

// recomputeFormulas re-evaluates the entity's stored formula results for this tenant after a
// formula field was added or edited, reading the field list fresh (the change just committed).
// It runs detached from the request's cancellation so a closed browser tab cannot leave the table
// half recomputed. No store wired (unit fakes) = no sweep.
func (s *FieldService) recomputeFormulas(ctx context.Context, tenantID, entityID, entityName string) error {
	if s.formulaRows == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	fields, err := s.meta.ListFields(ctx, tenantID, entityID)
	if err != nil {
		return fmt.Errorf("services.recomputeFormulas: list fields: %w", err)
	}
	specs, err := toFieldSpecs(fields)
	if err != nil {
		return fmt.Errorf("services.recomputeFormulas: %w", err)
	}
	if _, err := recomputeStoredFormulas(ctx, s.formulaRows, tenantID, entityName, specs); err != nil {
		return fmt.Errorf("services.recomputeFormulas: %w", err)
	}
	return nil
}

// WithDraftFields wires the S7-8 draft layer: on a delivered workspace a builder's EditField, the
// required flag of AddField and PUT /admin/fields/reorder become drafts instead of live writes.
// Chainable.
func (s *FieldService) WithDraftFields(d *DraftFieldRepo) *FieldService {
	s.drafts = d
	return s
}

// getField resolves a field the way the caller reads it: the builder's draft overlay when one is wired.
func (s *FieldService) getField(ctx context.Context, tenantID, fieldID string) (*metadata.FieldDefinition, error) {
	if s.drafts != nil {
		return s.drafts.GetFieldByID(ctx, tenantID, fieldID)
	}
	return s.meta.GetFieldByID(ctx, tenantID, fieldID)
}

// WithReorderRepo overrides the W5 field-reorder seam (used by unit tests whose metadata
// fake does not implement FieldReorderRepo). Chainable.
func (s *FieldService) WithReorderRepo(rr FieldReorderRepo) *FieldService {
	s.reorder = rr
	return s
}

// WithMetaCache attaches the shared metadata cache so field writes invalidate the entity's cached
// field list after commit (O3). Chainable.
func (s *FieldService) WithMetaCache(c *MetaCache) *FieldService {
	s.cache = c
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so committed field mutations emit an
// append-only audit row (async, fail-open). The actor user id is read from the request
// context (audit.WithActor, set by the admin handler). Chainable; a nil auditor is ignored.
func (s *FieldService) WithAuditor(a audit.Auditor) *FieldService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// fieldAlreadyExists returns the canonical 409 FIELD_ALREADY_EXISTS ValidationError used by
// every RM-25 concurrent-add race outcome (1061 ER_DUP_KEYNAME on an ALTER, or
// metadata.ErrDuplicateField / 1062 on the sys_fields unique key). Centralizing it keeps the
// message and code identical across all four race sites — the DDL loser and the metadata
// loser are indistinguishable to the client: the field already exists.
func fieldAlreadyExists(fieldName, entityName string) error {
	return invalid("FIELD_ALREADY_EXISTS", "field %q đã tồn tại trên entity %q (tạo đồng thời)", fieldName, entityName)
}

// AddField validates the payload, resolves the (tenant-scoped) target entity, rejects a
// duplicate field name, then — for unique fields only — ALTERs the obj_* table to add a
// generated column + tenant-scoped unique index before persisting the sys_fields row. If
// the metadata write fails after a successful ALTER, the just-added column is dropped so no
// orphan column/index is left behind. Returns the persisted field definition.
func (s *FieldService) AddField(ctx context.Context, tenantID string, in AddFieldInput) (*metadata.FieldDefinition, error) {
	if err := ValidateAddField(in); err != nil {
		return nil, err
	}
	// Names are validated; re-run sanitize to get the normalized (lowercased) forms.
	entityName, err := ddl.SanitizeIdentifier(in.EntityName)
	if err != nil {
		return nil, invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}
	fieldName, err := ddl.SanitizeIdentifier(in.Field.Name)
	if err != nil {
		return nil, invalid("FIELD_INVALID_NAME", "tên field %q không hợp lệ: %v", in.Field.Name, err)
	}

	// Resolve the target entity (tenant-scoped). Unknown entity -> stable not-found code.
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return nil, fmt.Errorf("services.AddField: resolve entity: %w", err)
	}

	// specs/063 S7-8 (FR-062): on a delivered workspace a REQUIRED field is created optional and the
	// "bắt buộc" flag rides a draft, so the customer's staff are not blocked by a field they cannot
	// see yet. Asked before any DDL so a gate refusal (no caller) leaves nothing behind.
	deferRequired := false
	if in.Field.IsRequired && s.drafts != nil {
		mode, merr := s.drafts.gate.WriteMode(ctx, tenantID)
		if merr != nil {
			return nil, fmt.Errorf("services.AddField: %w", merr)
		}
		deferRequired = mode == metaversion.ModeDraft
	}

	// Reject a field name that already exists on this entity before any DDL runs. The check
	// uses ListFieldsIncludingDeleted (NOT ListFields) so a name that was added then
	// soft-deleted is still detected (spec §7.2 B-1): GORM hides soft-deleted rows from the
	// active list, but a soft-delete keeps the physical obj_* column, so re-adding a unique
	// field via ListFields would ALTER an already-existing column -> 500, and re-adding an
	// ordinary field would create a phantom second sys_fields row. Re-add is rejected (409);
	// the caller must restore the original field rather than create a clashing one.
	existing, err := s.meta.ListFieldsIncludingDeleted(ctx, tenantID, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("services.AddField: list fields: %w", err)
	}
	live := 0
	for _, f := range existing {
		if f.Name == fieldName {
			return nil, invalid("FIELD_ALREADY_EXISTS", "field %q đã tồn tại trên entity %q (có thể đã bị xoá mềm)", fieldName, entityName)
		}
		if !f.DeletedAt.Valid {
			live++
		}
	}
	// CreateEntity caps a single declared field list at maxFieldsPerEntity, but repeated
	// AddField calls could otherwise grow an entity past the cap without bound. Enforce the
	// same ceiling against the entity's LIVE field count here — soft-deleted rows do not
	// count (their metadata slot is freed; only the physical column lingers).
	if live >= maxFieldsPerEntity {
		return nil, invalid("FIELD_LIMIT_EXCEEDED",
			"entity %q đã có %d field — tối đa %d field mỗi entity", entityName, live, maxFieldsPerEntity)
	}

	// specs/052: a dependent picklist's controllingField must name a live sibling picklist on THIS
	// entity (map keys among its values, no cycle) — checked before any DDL, like formula refs.
	if hasDependentKeys(in.Field.Options) || hasRelationFilter(in.Field.Options) {
		siblings, err := liveFieldInputs(existing)
		if err != nil {
			return nil, fmt.Errorf("services.AddField: %w", err)
		}
		f := in.Field
		f.Name = fieldName
		if err := validateDependentRefs(f, siblings, nil); err != nil {
			return nil, err
		}
		if err := validateRelationFilterRefs(f, siblings, nil); err != nil {
			return nil, err
		}
	}

	// Validate formula field refs against the entity's EXISTING active fields (plus the new
	// field itself). This is the orchestration-layer ref check for AddField; CreateEntity
	// has its own equivalent call (validateFormulaRefs over the full input field list).
	// We build a combined FieldInput slice from the active existing fields + the new field
	// so formula.Validate can check every identifier against all siblings.
	if in.Field.FieldType == "formula" {
		// liveFieldInputs keeps only active (non-soft-deleted) fields — existing is from
		// ListFieldsIncludingDeleted — with their options, which a lookup operand needs.
		combined, lerr := liveFieldInputs(existing)
		if lerr != nil {
			return nil, fmt.Errorf("services.AddField: %w", lerr)
		}
		// Append the new field itself so self-ref detection works (self-name is in the list
		// but with type "formula" → Validate's NESTED guard catches it before UNKNOWN_REF).
		combined = append(combined, in.Field)
		lookupTypes, lerr := lookupValueTypes(ctx, s.meta, tenantID, entityName, combined)
		if lerr != nil {
			return nil, fmt.Errorf("services.AddField: %w", lerr)
		}
		if err := validateFormulaRefs(combined, lookupTypes); err != nil {
			return nil, err
		}
	}

	// ENG-COMPUTED #2: same active-siblings + new-field combination check for a lookup field's
	// options.via (must name a sibling to-one relation field on THIS entity). Unlike the formula
	// combine above, this needs the sibling's Options (validateLookupRefs reads
	// options.cardinality off the relation sibling), so options are parsed back from storage.
	if in.Field.FieldType == "lookup" {
		combined := make([]FieldInput, 0, len(existing)+1)
		for _, ef := range existing {
			if ef.DeletedAt.Valid {
				continue
			}
			sibOpts, perr := parseOptionsJSON(ef.Options)
			if perr != nil {
				return nil, fmt.Errorf("services.AddField: parse sibling options for lookup ref check: %w", perr)
			}
			combined = append(combined, FieldInput{
				Name:      ef.Name,
				FieldType: ef.FieldType,
				Options:   sibOpts,
			})
		}
		combined = append(combined, in.Field)
		if err := validateLookupRefs(combined); err != nil {
			return nil, err
		}

		// ENG-COMPUTED #2 (cont'd): sourceField nested-guard (mirrors CreateEntity). AddField
		// always targets an already-persisted entity, so selfFields is nil here too — a
		// self-referential lookup's target reads via the DB like any other target.
		relSpecsByName := make(map[string]FieldInput, len(combined))
		for _, cf := range combined {
			if cf.FieldType == "relation" {
				relSpecsByName[cf.Name] = cf
			}
		}
		if err := validateLookupSourceField(ctx, s.meta, tenantID, relSpecsByName, nil, entityName, in.Field); err != nil {
			return nil, err
		}
	}

	// ENG-COMPUTED #3: validate a new rollup field's cross-entity refs. AddField always targets
	// an ALREADY-PERSISTED entity, so there is no self-referential in-memory case here (unlike
	// CreateEntity) — selfFields is nil, forcing a DB read even when childEntity == entityName.
	if in.Field.FieldType == "rollup" {
		if err := validateRollupCrossEntity(ctx, s.meta, tenantID, entityName, nil, in.Field); err != nil {
			return nil, err
		}
	}

	// A 1-N `relation` field (M3, FK-on-child) needs its FK target verified before any DDL:
	// options.relatedEntity must name an entity that exists for this tenant, else we'd
	// materialize an FK column pointing at a non-existent parent table. The target is
	// resolved tenant-scoped (so a target owned by another tenant is treated as not-found).
	//
	// ENG-REL2 #4: once the target is confirmed, resolve this field's inverse-field registry
	// row here too (before any DDL) — a naming collision must reject 422 before we ALTER
	// anything, exactly like the target-existence check above.
	var pendingRel *metadata.RelationshipDefinition
	if in.Field.FieldType == "relation" {
		target, _ := relatedEntityName(in.Field.Options) // presence/identifier already validated
		targetName, err := ddl.SanitizeIdentifier(target)
		if err != nil {
			return nil, invalid("FIELD_RELATION_TARGET_INVALID",
				"field relation %q có options.relatedEntity không hợp lệ: %v", fieldName, err)
		}
		if _, err := s.meta.GetEntityByName(ctx, tenantID, targetName); err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return nil, invalid("FIELD_RELATION_TARGET_NOT_FOUND",
					"entity đích %q của field relation %q không tồn tại", targetName, fieldName)
			}
			return nil, fmt.Errorf("services.AddField: resolve relation target: %w", err)
		}
		inverse, err := resolveInverseField(ctx, s.meta, pendingInverseClaims{}, tenantID, targetName, entityName, fieldName, in.Field.Options)
		if err != nil {
			return nil, err
		}
		// ENG-REL2 #5: the registry row's type mirrors the field's resolved cardinality
		// (validateRelationCardinality already rejected anything but "1-N"/"1-1" at
		// ValidateAddField time, above).
		rel := newInverseRelationship(entity.TenantID, targetName, entityName, fieldName, inverse, relationCardinality(in.Field.Options))
		pendingRel = &rel
	}

	// ACTIVITY-CORE #3a: a polymorphic_relation field needs EACH of its targetEntities verified to
	// exist for THIS tenant before any DDL (mirrors the relation target-existence check above) —
	// otherwise the FE attach-picker would offer an entity whose records can never be selected. A
	// target that is the entity being added-to (self-reference) is allowed (its table exists). A
	// missing/foreign target → 422 FIELD_POLYMORPHIC_TARGET_NOT_FOUND.
	if in.Field.FieldType == "polymorphic_relation" {
		targets, _ := polymorphicTargetList(in.Field.Options) // non-empty guaranteed by ValidateAddField
		for _, t := range targets {
			target, err := ddl.SanitizeIdentifier(t)
			if err != nil {
				return nil, invalid("FIELD_POLYMORPHIC_TARGET_NOT_FOUND",
					"field polymorphic_relation %q có targetEntity %q không hợp lệ: %v", fieldName, t, err)
			}
			if target == entityName {
				continue // self-reference: the target is this entity, whose table already exists
			}
			if _, err := s.meta.GetEntityByName(ctx, tenantID, target); err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return nil, invalid("FIELD_POLYMORPHIC_TARGET_NOT_FOUND",
						"targetEntity %q của field polymorphic_relation %q không tồn tại", target, fieldName)
				}
				return nil, fmt.Errorf("services.AddField: resolve polymorphic target: %w", err)
			}
		}
	}

	// Unique and relation fields need a materialized, indexable column; ordinary fields stay
	// in `data`. Before ALTERing, confirm the physical column does not already exist.
	// UNIQUE (spec §7.2 B-1): a column left behind by a prior soft-delete must be reused,
	// not re-added (ALTER ADD COLUMN onto an existing column errors -> 500); when present we
	// skip the ALTER and do NOT mark `altered`, so the compensation path never drops a
	// column we did not create. RELATION: a relation's `<field>_id` column already existing
	// is instead a name collision (it belongs to another field/column) and is rejected with
	// FIELD_RELATION_COLUMN_CONFLICT — never skipped-and-persisted, which would let the
	// relation hijack a foreign column. A relation field's physical column is `<field>_id`.
	// isIndexed reads the incoming options to determine whether the new field should have a
	// non-unique generated column + index (D1). It is independent of IsUnique: a field can
	// be indexed without being unique. However when IsUnique is also true we SKIP the
	// indexed path — the unique generated column already provides a physical B-tree column;
	// adding a second column with the same name would cause a MySQL duplicate-column error.
	isIndexed := boolOpt(in.Field.Options, "indexed")

	altered := false
	relAltered := false
	// relOneToOne, when true (ENG-REL2 #5), records that relAltered's ALTER used the 1-1
	// pair (AddOneToOneRelationColumn/DropOneToOneRelationColumn) rather than the ordinary
	// 1-N pair — so compensation drops the column with the matching Drop* (the index names
	// differ: uq_obj_<entity>_<field>_id vs idx_obj_<entity>_<field>_id).
	relOneToOne := false
	idxAltered := false
	// polyAltered (ACTIVITY-CORE #3a) records that the ALTER materialized a polymorphic_relation
	// field's two gen-columns + composite index, so compensation drops them with
	// DropPolymorphicColumns on a failed metadata write.
	polyAltered := false
	switch {
	case in.Field.IsUnique:
		exists, err := s.schema.ColumnExists(ctx, entityName, fieldName)
		if err != nil {
			return nil, fmt.Errorf("services.AddField: check column exists: %w", err)
		}
		if !exists {
			// RM-28 Slice B: AddUniqueGencolNonUnique materializes a NON-unique idx_ index
			// (not a physical UNIQUE index) — uniqueness for this field is enforced SOLELY by
			// the sys_unique_index pivot, populated by the backfill just below. Because the
			// index is non-unique, this ALTER can NEVER trip ER_DUP_ENTRY (1062) — the
			// CROSS_TENANT_SHARED_UNIQUE verdict that used to fire here is retired (its role is
			// fully replaced by the pivot's intra-tenant RECORD_DUPLICATE below); a co-tenant's
			// existing duplicate values on the shared table simply do not collide with this
			// tenant's own pivot rows (tenant_id is the leading pivot key — cross-tenant
			// decoupling by construction).
			if err := s.schema.AddUniqueGencolNonUnique(ctx, entityName, fieldName); err != nil {
				// RM-25 DDL race: a concurrent AddField for the SAME unique field already
				// created the `idx_<table>_<field>` index, so our ADD INDEX trips
				// ER_DUP_KEYNAME (1061). Both callers passed the dup-check; this one lost
				// the race. Map to 409 FIELD_ALREADY_EXISTS, not an opaque 500. No column
				// was added by this call (the ALTER failed atomically), so there is nothing
				// to compensate.
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return nil, fieldAlreadyExists(fieldName, entityName)
				}
				return nil, fmt.Errorf("services.AddField: add unique column: %w", err)
			}
			altered = true
		}
		// RM-28 §4.1 (A2, now the SOLE enforcement post Slice B): backfill THIS tenant's pivot
		// rows for the field now that its gencol is guaranteed to exist (just materialized
		// above, or already present from a co-tenant's earlier AddField on the SAME shared
		// obj_* table — the shared-table reconcile case, exists==true). Runs BEFORE the
		// metadata write: an intra-tenant dupe the pre-scan finds rejects the WHOLE add with
		// 409 RECORD_DUPLICATE — this tenant's own live rows collide under the field's JSON key
		// (possible even on a freshly-altered column: the key may already hold values from
		// before this field was ever registered unique, e.g. a prior soft-deleted field of the
		// same name — spec §7.2 B-1). Since the ALTER above is now a non-unique index, it can
		// NEVER catch this itself (pre Slice-B it sometimes did, via the physical UNIQUE
		// index's own 1062) — the pre-scan is the ONLY place an intra-tenant dupe is caught.
		bf, err := s.schema.BackfillUniqueField(ctx, entityName, tenantID, fieldName)
		if err != nil {
			if altered {
				if dropErr := s.schema.DropUniqueGencolNonUnique(ctx, entityName, fieldName); dropErr != nil {
					return nil, fmt.Errorf("services.AddField: backfill pivot failed (%w) and column cleanup failed: %v", err, dropErr)
				}
			}
			return nil, fmt.Errorf("services.AddField: backfill unique pivot: %w", err)
		}
		if len(bf.Conflicts) > 0 {
			// Purge nothing beyond the column: BackfillUniqueField writes NOTHING to the pivot
			// for a conflicting value (the pre-scan excludes it via NOT IN — §2.3), so there are
			// no garbage pivot rows to clean up either way; only the just-created column (when
			// altered) needs to be rolled back.
			if altered {
				if dropErr := s.schema.DropUniqueGencolNonUnique(ctx, entityName, fieldName); dropErr != nil {
					return nil, fmt.Errorf("services.AddField: pivot conflict and column cleanup failed: %w", dropErr)
				}
			}
			return nil, invalidField("RECORD_DUPLICATE", fieldName,
				"không thể đặt field %q unique: dữ liệu hiện có trùng giá trị %q (%d bản ghi)",
				fieldName, bf.Conflicts[0].Value, bf.Conflicts[0].Count)
		}
	case in.Field.FieldType == "relation":
		// A relation field materializes a `<field>_id` FK column. If that physical column
		// already exists, it was created by something else (e.g. a prior UNIQUE field
		// literally named `<field>_id`): silently skipping the ALTER and persisting anyway
		// would let the relation hijack a foreign column and drift metadata from schema.
		// A re-add of this relation's own soft-deleted column is already rejected earlier by
		// the dup-check (ListFieldsIncludingDeleted), so reaching here with the column
		// present is always a name collision -> reject (422), never skip-and-persist (§7.2).
		if err := CheckRelationColumn(ctx, s.schema, entityName, fieldName, in.Field.FieldType); err != nil {
			return nil, err
		}
		// ENG-REL2 #5: cardinality selects which DDL pair materializes the FK column — the
		// ordinary 1-N shape (plain FK column + non-unique index, unchanged) or the 1-1
		// shape (CASE-guarded FK column + tenant-scoped UNIQUE key, enforcing exclusivity).
		if relationCardinality(in.Field.Options) == "1-1" {
			if err := s.schema.AddOneToOneRelationColumn(ctx, entityName, fieldName); err != nil {
				// RM-25 DDL race for a concurrent relation AddField: 1060/1061 → 409.
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return nil, fieldAlreadyExists(fieldName, entityName)
				}
				// 1062 on the UNIQUE index: existing rows on a SHARED obj_* table already
				// violate the 1-1 constraint (more than one live child already points at the
				// same parent for this tenant). Map to 409, not an opaque 500. NOTE: 1-1
				// relation cardinality is a SEPARATE physical UNIQUE index (uq_obj_<e>_<f>_id)
				// — it is NOT pivot-backed and NOT retired by RM-28 Slice B (which only demotes
				// plain `is_unique` fields' indexes); this is the one remaining place a
				// physical shared-table UNIQUE index can still legitimately 1062.
				if errors.Is(err, dynrepo.ErrDuplicateUnique) {
					return nil, invalid("FIELD_RELATION_CARDINALITY_CONFLICT",
						"không thể thêm field relation 1-1 %q: dữ liệu hiện có trên bảng dùng chung %q vi phạm ràng buộc 1-1 (nhiều bản ghi con cùng trỏ một bản ghi cha)", fieldName, "obj_"+entityName)
				}
				return nil, fmt.Errorf("services.AddField: add one-to-one relation column: %w", err)
			}
			relAltered = true
			relOneToOne = true
		} else {
			if err := s.schema.AddRelationColumn(ctx, entityName, fieldName); err != nil {
				// RM-25 DDL race for a concurrent relation AddField: 1061 → 409.
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return nil, fieldAlreadyExists(fieldName, entityName)
				}
				return nil, fmt.Errorf("services.AddField: add relation column: %w", err)
			}
			relAltered = true
		}
	case in.Field.FieldType == "user":
		// USER-FIELD: a `user` field materializes the SAME `<field>_id` FK column shape a
		// plain 1-N `relation` field does (reused DDL, ../specs/017-user-field-type/spec.md) — but its
		// target is the FIXED `users` system table, so there is no cardinality option, no
		// target-existence check, and no sys_relationships registry row (pendingRel stays
		// nil below). The column-collision guard mirrors relation's exactly.
		if err := CheckRelationColumn(ctx, s.schema, entityName, fieldName, in.Field.FieldType); err != nil {
			return nil, err
		}
		if err := s.schema.AddRelationColumn(ctx, entityName, fieldName); err != nil {
			// RM-25 DDL race for a concurrent user-field AddField: 1061 -> 409.
			if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
				return nil, fieldAlreadyExists(fieldName, entityName)
			}
			return nil, fmt.Errorf("services.AddField: add user column: %w", err)
		}
		relAltered = true
	case in.Field.FieldType == "polymorphic_relation":
		// A polymorphic_relation field materializes `<field>_type` + `<field>_id`. Three cases:
		//   - BOTH columns already present → a SHARED obj_* table reconcile: a co-tenant on the same
		//     physical table already added this exact field. Register the metadata for THIS tenant
		//     WITHOUT re-running the DDL (mirrors CreateEntity's shared-table reconcile for inline
		//     fields, and how a deferred polymorphic pack field re-installs into a second tenant).
		//     Safe: both sub-column names are RESERVED at entity-create so no foreign field can own
		//     them, and a re-add of this tenant's own soft-deleted field is caught by the
		//     ListFieldsIncludingDeleted dup-check above — so both-present is always a genuine
		//     same-shape reconcile, never a hijack.
		//   - Exactly ONE present → a partial/foreign collision (a stray column named `<field>_type`
		//     or `<field>_id`, or an interrupted add): reject (422) rather than drift.
		//   - Neither present → materialize both.
		typeExists, err := s.schema.ColumnExists(ctx, entityName, fieldName+"_type")
		if err != nil {
			return nil, fmt.Errorf("services.AddField: check polymorphic type column exists: %w", err)
		}
		idExists, err := s.schema.ColumnExists(ctx, entityName, fieldName+"_id")
		if err != nil {
			return nil, fmt.Errorf("services.AddField: check polymorphic id column exists: %w", err)
		}
		switch {
		case typeExists && idExists:
			// Shared-table reconcile — no DDL, nothing to compensate.
		case typeExists || idExists:
			present := fieldName + "_type"
			if idExists {
				present = fieldName + "_id"
			}
			return nil, invalid("FIELD_RELATION_COLUMN_CONFLICT",
				"không thể thêm field polymorphic_relation %q: cột %q đã tồn tại trên entity %q", fieldName, present, entityName)
		default:
			if err := s.schema.AddPolymorphicColumns(ctx, entityName, fieldName); err != nil {
				// RM-25 DDL race for a concurrent polymorphic AddField: 1060/1061 → 409.
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return nil, fieldAlreadyExists(fieldName, entityName)
				}
				return nil, fmt.Errorf("services.AddField: add polymorphic columns: %w", err)
			}
			polyAltered = true
		}
	default:
		// Non-unique, non-relation field: add an indexed generated column if requested (D1).
		// The physical column name is the bare field name; ColumnExists guards re-adds (a
		// prior soft-delete may have left the column behind — in that case we skip the ALTER
		// rather than 500 on ADD COLUMN onto an existing column).
		if isIndexed {
			exists, err := s.schema.ColumnExists(ctx, entityName, fieldName)
			if err != nil {
				return nil, fmt.Errorf("services.AddField: check indexed column exists: %w", err)
			}
			if !exists {
				if err := s.schema.AddIndexedColumn(ctx, entityName, fieldName, in.Field.FieldType); err != nil {
					// RM-25 DDL race for a concurrent indexed AddField: 1061 → 409.
					if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
						return nil, fieldAlreadyExists(fieldName, entityName)
					}
					return nil, fmt.Errorf("services.AddField: add indexed column: %w", err)
				}
				idxAltered = true
			}
		}
	}

	opts, err := optionsJSON(in.Field.Options)
	if err != nil {
		return nil, err
	}
	// W5: resolve the new field's display position. Explicit fieldOrder wins; otherwise the
	// field is APPENDED after the entity's current maximum active order (+10, matching the
	// entity-create gap). Soft-deleted rows are excluded from the max so a deleted trailing
	// field does not inflate the append position. `existing` was already loaded above for the
	// dup-check — no extra query.
	fieldOrder := 0
	if in.Field.FieldOrder != nil {
		fieldOrder = *in.Field.FieldOrder
	} else {
		maxOrder := 0
		for _, ef := range existing {
			if ef.DeletedAt.Valid {
				continue
			}
			if ef.FieldOrder > maxOrder {
				maxOrder = ef.FieldOrder
			}
		}
		fieldOrder = maxOrder + 10
	}
	fd := &metadata.FieldDefinition{
		ID:       uuid.NewString(),
		EntityID: entity.ID,
		// TenantID is taken from the resolved entity (server-derived), NEVER from the client,
		// so every new sys_fields row carries its owning tenant directly (A2 defense-in-depth).
		TenantID:    entity.TenantID,
		Name:        fieldName,
		Label:       in.Field.Label,
		FieldType:   in.Field.FieldType,
		IsRequired:  in.Field.IsRequired && !deferRequired,
		IsUnique:    in.Field.IsUnique,
		Options:     opts,
		FieldOrder:  fieldOrder,
		Description: in.Field.Description,
	}

	// ENG-REL2 #4: a `relation` field persists its sys_fields row AND its companion 1-N
	// sys_relationships registry row atomically (one transaction) via
	// CreateFieldWithRelationship — never the plain CreateField, which would leave the two
	// writes independently fallible (a field with no registry row, or vice versa). Every other
	// field type is unaffected: pendingRel is nil, and CreateField's behaviour is unchanged.
	var createErr error
	if pendingRel != nil {
		createErr = s.meta.CreateFieldWithRelationship(ctx, fd, pendingRel)
	} else {
		createErr = s.meta.CreateField(ctx, fd)
	}
	if err := createErr; err != nil {
		// RM-25 metadata-layer serializer: a concurrent AddField won the race and already
		// persisted this (tenant_id, entity_id, name), so our INSERT trips the 000012
		// unique key (metadata.ErrDuplicateField). This is the ONLY race serializer for an
		// ORDINARY (non-unique, non-relation, non-indexed) field, which runs no DDL — the
		// 1061 ALTER guards above never fire for it. We map to 409 FIELD_ALREADY_EXISTS,
		// but FIRST run the same compensation as any other metadata failure so a unique/
		// relation/indexed loser that DID auto-commit an ALTER leaves no orphan column.
		dup := errors.Is(err, metadata.ErrDuplicateField)
		// Finding A: a `relation` field's auto-registered 1-N sys_relationships row collided
		// with an EXISTING relationship row on (tenant_id, name) — an admin-registered N-N
		// literally named the same, or a 1-N row left behind by an entity recreate after
		// soft-delete (RM-21). This can only ever be true when pendingRel != nil (FieldType ==
		// "relation"), which — if we reach this point at all (no earlier FIELD_RELATION_*
		// return) — always sets relAltered = true just below, so this is only ever checked in
		// the relAltered branch.
		dupRel := errors.Is(err, metadata.ErrDuplicateRelationship)
		// Compensation: an auto-committed ALTER cannot roll back with the metadata write,
		// so drop the column we just added. Ordinary fields ran no DDL — nothing to undo.
		if altered {
			if dropErr := s.schema.DropUniqueGencolNonUnique(ctx, entityName, fieldName); dropErr != nil {
				return nil, fmt.Errorf("services.AddField: metadata failed (%w) and column cleanup failed: %v", err, dropErr)
			}
			// RM-28: the column we just created is going away — its just-backfilled pivot rows
			// (this call's BackfillUniqueField, above) would otherwise linger as garbage nobody
			// can ever legitimately claim (no sys_fields row for this tenant will ever exist for
			// a column that no longer exists). Best-effort: a failure here does not block the
			// column-rollback response — the Doctor D1 orphan check is the backstop.
			if pivotErr := s.schema.DeletePivotByField(ctx, entityName, tenantID, fieldName); pivotErr != nil {
				return nil, fmt.Errorf("services.AddField: metadata failed (%w), column rolled back, but pivot cleanup failed: %v", err, pivotErr)
			}
			if dup {
				return nil, fieldAlreadyExists(fieldName, entityName)
			}
			return nil, fmt.Errorf("services.AddField: persist metadata (column rolled back): %w", err)
		}
		if relAltered {
			// ENG-REL2 #5: drop with the SAME pair that added it — the 1-1 and 1-N shapes
			// use different index names (uq_ vs idx_), so the wrong Drop* would error.
			var dropErr error
			if relOneToOne {
				dropErr = s.schema.DropOneToOneRelationColumn(ctx, entityName, fieldName)
			} else {
				dropErr = s.schema.DropRelationColumn(ctx, entityName, fieldName)
			}
			if dropErr != nil {
				return nil, fmt.Errorf("services.AddField: metadata failed (%w) and relation column cleanup failed: %v", err, dropErr)
			}
			if dup {
				return nil, fieldAlreadyExists(fieldName, entityName)
			}
			if dupRel {
				return nil, relationshipAlreadyExists(pendingRel.Name)
			}
			return nil, fmt.Errorf("services.AddField: persist metadata (relation column rolled back): %w", err)
		}
		// D1 compensation: indexed generated column was ALTERed in but metadata write failed.
		if idxAltered {
			if dropErr := s.schema.DropIndexedColumn(ctx, entityName, fieldName); dropErr != nil {
				return nil, fmt.Errorf("services.AddField: metadata failed (%w) and indexed column cleanup failed: %v", err, dropErr)
			}
			if dup {
				return nil, fieldAlreadyExists(fieldName, entityName)
			}
			return nil, fmt.Errorf("services.AddField: persist metadata (indexed column rolled back): %w", err)
		}
		// ACTIVITY-CORE #3a compensation: polymorphic gen-columns were ALTERed in but the metadata
		// write failed — drop the pair (index + both columns) so no orphan schema is left behind.
		if polyAltered {
			if dropErr := s.schema.DropPolymorphicColumns(ctx, entityName, fieldName); dropErr != nil {
				return nil, fmt.Errorf("services.AddField: metadata failed (%w) and polymorphic column cleanup failed: %v", err, dropErr)
			}
			if dup {
				return nil, fieldAlreadyExists(fieldName, entityName)
			}
			return nil, fmt.Errorf("services.AddField: persist metadata (polymorphic columns rolled back): %w", err)
		}
		// No DDL was run (ordinary field, or the column already existed and we skipped the
		// ALTER): nothing to compensate. A duplicate is a clean 409.
		if dup {
			return nil, fieldAlreadyExists(fieldName, entityName)
		}
		return nil, fmt.Errorf("services.AddField: persist metadata: %w", err)
	}
	// Invalidate the entity's cached field list AFTER commit so cached resolve sees the new field (O3).
	s.cache.InvalidateFields(tenantID, fd.EntityID)
	// RM-19: audit the committed field add (after-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.FieldMeta, audit.ActionCreate, nil, fd)
	if deferRequired {
		want := *fd
		want.IsRequired = true
		if _, err := s.drafts.SaveEdit(ctx, tenantID, &want); err != nil {
			// Never a silent state: the field exists, optional. Say so and say how to finish.
			return nil, &ValidationError{Code: "FIELD_REQUIRED_DRAFT_FAILED",
				Msg:     fmt.Sprintf("Đã tạo trường %q nhưng chưa lưu được \"Bắt buộc\" vào bản nháp (%v). Mở trường và bật lại \"Bắt buộc\".", fieldName, err),
				Details: map[string]any{"fieldId": fd.ID, "field": fieldName}}
		}
	}
	// A formula is stored at write time: give the rows that already exist their value, or every
	// total over it counts only rows written from now on. The field is saved either way; a failed
	// sweep is reported, and saving the field again (EditField) re-runs it.
	if fd.FieldType == "formula" {
		if err := s.recomputeFormulas(ctx, tenantID, fd.EntityID, entityName); err != nil {
			return nil, fmt.Errorf("services.AddField: field saved, stored formulas not recomputed: %w", err)
		}
	}
	// A stored roll-up has no value on the rows that already exist: give them one now. The field is
	// saved either way; a failed sweep is reported and "Tính lại" (RecomputeField) retries it.
	if fd.FieldType == "rollup" && s.storedRollupSweep != nil {
		if opts, perr := parseOptionsJSON(fd.Options); perr == nil && boolOpt(opts, "stored") {
			if _, err := s.storedRollupSweep(ctx, tenantID, entityName, fd.Name); err != nil {
				return nil, fmt.Errorf("services.AddField: field saved, stored roll-up not computed: %w", err)
			}
		}
	}
	return fd, nil
}

// EditField updates the mutable attributes (label / isRequired / defaultValue / options)
// of an existing field. It performs NO DDL — editing a field never alters the obj_* table,
// which is what keeps the operation safe and reversible (see §4.4). The flow is:
//
//  1. Resolve the field tenant-scoped (GetFieldByID) — unknown/cross-tenant id -> 404.
//  2. Reject any attempt to change fieldType (422, FIELD_TYPE_IMMUTABLE): changing a
//     field's type would require migrating existing `data` values, which is out of scope.
//  3. Validate the edit (picklist options still non-empty when supplied).
//  4. Apply the mutable attributes onto the resolved field and persist them.
//
// fieldType, name, entity_id and uniqueness are preserved exactly as stored; the caller's
// FieldType is used only to detect a change attempt, never written.
func (s *FieldService) EditField(ctx context.Context, tenantID string, in EditFieldInput) (*metadata.FieldDefinition, error) {
	if strings.TrimSpace(in.FieldID) == "" {
		return nil, invalid("FIELD_ID_REQUIRED", "thiếu id field cần sửa")
	}

	// Resolve tenant-scoped: a field id belonging to another tenant resolves to not-found,
	// so this is also the tenant-isolation guard — no cross-tenant read or write is possible.
	field, err := s.getField(ctx, tenantID, in.FieldID)
	if err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return nil, invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
		}
		return nil, fmt.Errorf("services.EditField: resolve field: %w", err)
	}
	// Snapshot the before-image for the RM-19 audit diff.
	beforeField := *field
	if s.drafts != nil {
		// The audit log is the customer admin's: diff LIVE rows only, never the builder's draft overlay.
		live, lerr := s.meta.GetFieldByID(ctx, tenantID, in.FieldID)
		if lerr != nil {
			return nil, fmt.Errorf("services.EditField: resolve live field for audit: %w", lerr)
		}
		beforeField = *live
		// #56: options sent alone that differ from the LIVE row in "indexed" only are "set indexed"
		// ("Bật index ngay" outside /studio read the field live): lay the flag onto the builder's view.
		if in.Options != nil && in.Label == nil && in.IsRequired == nil && in.DefaultVal == nil && in.FieldOrder == nil && in.Description == nil {
			merged, merr := indexedOnlyOptions(in.Options, live.Options, field.Options)
			if merr != nil {
				return nil, fmt.Errorf("services.EditField: %w", merr)
			}
			if merged != nil {
				in.Options = merged
			}
		}
	}

	// ONE-body validation + apply-to-copy, shared verbatim with the AI Builder's
	// propose_edit_field (ValidateEditFieldChanges — validator-parity extraction, see its doc):
	// fieldType immutability, ValidateEditField, options config/flag guards, relation-options
	// immutability, mutable attributes applied onto a copy, effective default re-checked.
	//
	// storedOpts (the ORIGINAL options, parsed once inside the shared body) feeds the D1
	// indexed-toggle diff below: we must read the STORED indexed state BEFORE the applied
	// options, so we can compare old vs new and decide whether DDL is needed.
	//
	// Shared-table caveat (documented per design §8 / wave2b-design.md §D1):
	// obj_<name> is shared across ALL tenants that register an entity with this name, so a
	// physical ADD/DROP COLUMN affects every co-tenant of that entity-name. W5-A1: toggle-OFF
	// therefore NEVER physically drops the column (that would strip a co-tenant's sort/filter
	// column); it only flips this tenant's `indexed` metadata flag. Toggle-ON is idempotent via
	// ColumnExists so a re-index never re-ADDs a column a prior un-index left in place. ADD on
	// first index is still shared (additive, harmless); physical reclamation waits for the
	// per-tenant index/uniqueness pivot (RM-28).
	// A formula's identifiers are checked against the entity's active fields (queue #107); no
	// other type reads siblings, so only a formula edit pays for the list - and, since the
	// dependent-picklist ref check moved into ValidateEditFieldChanges (queue #230), an edit whose
	// options carry controllingField/dependentValues/filterBy.
	var siblings []metadata.FieldDefinition
	var lookupTypes map[string]string
	if field.FieldType == "formula" || (in.Options != nil && (hasDependentKeys(in.Options) || hasRelationFilter(in.Options))) {
		if siblings, err = s.meta.ListFields(ctx, tenantID, field.EntityID); err != nil {
			return nil, fmt.Errorf("services.EditField: list sibling fields: %w", err)
		}
	}
	if field.FieldType == "formula" {
		entityName, nerr := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
		if nerr != nil {
			return nil, fmt.Errorf("services.EditField: resolve entity: %w", nerr)
		}
		if lookupTypes, err = ResolveLookupTypes(ctx, s.meta, tenantID, entityName, siblings); err != nil {
			return nil, fmt.Errorf("services.EditField: %w", err)
		}
	}
	effective, storedOpts, err := ValidateEditFieldChanges(*field, in, siblings, lookupTypes)
	if err != nil {
		return nil, err
	}
	// specs/052 dependent-picklist + relation-filter ref checks run inside ValidateEditFieldChanges
	// (shared with the AI Builder preview, queue #230).
	if err := s.guardRequiredWhileLocked(ctx, tenantID, *field, *effective); err != nil {
		return nil, err
	}
	if err := s.guardRequiredHasCondition(ctx, tenantID, *field, *effective); err != nil {
		return nil, err
	}
	if err := s.guardCurrencyCodeChange(ctx, tenantID, *field, storedOpts, in.Options); err != nil {
		return nil, err
	}
	// queue #234: a formula a rollup aggregates keeps its resultType while the rollup exists.
	if field.FieldType == "formula" && in.Options != nil {
		if finder, ok := s.meta.(RollupAggFinder); ok {
			entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
			if err != nil {
				return nil, fmt.Errorf("services.EditField: rollup guard: resolve entity: %w", err)
			}
			if err := GuardFormulaResultTypeInUse(ctx, finder, tenantID, entityName, *field, storedOpts, in.Options); err != nil {
				return nil, err
			}
			// #284: nor into a formula that reads a lookup/rollup (no longer stored).
			if err := GuardFormulaBecomesDerived(ctx, finder, tenantID, entityName, *field, storedOpts, in.Options, siblings); err != nil {
				return nil, err
			}
		}
	}
	wasIndexed := boolOpt(storedOpts, "indexed")
	field = effective

	// D1: resolve new indexed state from the effective (post-apply) options.
	nowIndexed := false
	if in.Options != nil {
		nowIndexed = boolOpt(in.Options, "indexed")
	} else {
		nowIndexed = wasIndexed // options not changed → indexed state unchanged
	}

	// Resolve the entity name from the field's entity_id so we can target the right obj_*
	// table. We need it only when DDL is required; skip the lookup otherwise.
	//
	// indexed toggle is only valid for non-unique, non-relation, non-user, non-computed fields:
	// unique fields already carry a physical column; relation AND user fields have their own
	// `<field>_id` column (USER-FIELD: skipping this is load-bearing, not just tidy — without
	// it, toggling options.indexed on a `user` field would call AddIndexedColumn with
	// fieldType="user", which indexedColExpr (ddl/alter.go) does not recognize and returns a
	// plain error, surfacing as an opaque 500 instead of being silently absorbed here);
	// lookup/rollup fields (ENG-COMPUTED #2/#3) are resolved at READ time and never write to
	// `data`, so a generated column over their JSON path would be uniformly NULL
	// (validateComputedFieldFlags rejects this at create/AddField, but EditField does not run
	// that guard — this site-check is the belt-and-suspenders equivalent, mirroring the
	// datetime reject a few lines below). Silently skip DDL (not an error) in all these cases —
	// the caller's indexed flag is still persisted in options for forward-compat but has no
	// DDL effect.
	indexedDDLNeeded := nowIndexed != wasIndexed && !field.IsUnique && field.FieldType != "relation" && field.FieldType != "user" && !isComputedFieldType(field.FieldType)
	if indexedDDLNeeded {
		// GetEntityNameByID resolves the entity name from the field's entity_id so we can
		// target the correct obj_* table for DDL. The field row carries only entity_id, not
		// the entity name, so a dedicated repo method is required.
		entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
		if err != nil {
			return nil, fmt.Errorf("services.EditField: resolve entity for DDL: %w", err)
		}

		switch {
		case nowIndexed && !wasIndexed:
			// Defense-in-depth: validateFieldConfig (called above via ValidateEditField) already
			// blocks indexed=true on datetime before reaching here; this site guard prevents DDL
			// if the validation path is bypassed by a future caller.
			if field.FieldType == "datetime" {
				return nil, invalid("FIELD_INDEX_UNSUPPORTED_TYPE",
					"field datetime không thể đánh index — RFC3339 chưa được chuẩn hoá cho generated column; dùng date hoặc filter datetime không cần index")
			}
			// Toggle ON: add the generated column + non-unique index — UNLESS it already exists.
			// W5-A1 shared-table safety: toggle-OFF (below) never physically drops the column (a
			// co-tenant sharing this entity-name may still index it), so the physical column can
			// outlive an un-index. ColumnExists makes re-index idempotent and avoids ADD COLUMN
			// onto an existing column (1060). MySQL's STORED generated column auto-backfills.
			added := false
			colExists, err := s.schema.ColumnExists(ctx, entityName, field.Name)
			if err != nil {
				return nil, fmt.Errorf("services.EditField: check indexed column exists: %w", err)
			}
			if !colExists {
				if err := s.schema.AddIndexedColumn(ctx, entityName, field.Name, field.FieldType); err != nil {
					return nil, fmt.Errorf("services.EditField: add indexed column: %w", err)
				}
				added = true
			}
			// Compensation: if metadata update fails after the ALTER, drop the column ONLY if we
			// added it in THIS call — never drop a column a co-tenant relies on.
			draftOnly, err := s.persistEdit(ctx, tenantID, field)
			if err != nil {
				if added {
					if dropErr := s.schema.DropIndexedColumn(ctx, entityName, field.Name); dropErr != nil {
						return nil, fmt.Errorf("services.EditField: metadata failed (%w) and indexed column cleanup failed: %v", err, dropErr)
					}
				}
				if errors.Is(err, metadata.ErrFieldNotFound) {
					return nil, invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
				}
				return nil, fmt.Errorf("services.EditField: persist update (indexed column rolled back): %w", err)
			}
			return s.editCommitted(ctx, tenantID, in, beforeField, field, draftOnly)

		case !nowIndexed && wasIndexed:
			// Toggle OFF: do NOT physically drop the generated column/index (W5-A1).
			// obj_<entity> is shared by entity-NAME across ALL tenants; a DROP COLUMN here would
			// remove the physical sort/filter column for every co-tenant that still indexes this
			// same-named field — a cross-tenant schema mutation the un-indexing tenant must never
			// cause. Mirror DeleteField's soft-only semantics: flip the metadata `indexed` flag and
			// leave the column in place (cheap, reversible; re-index is idempotent via ColumnExists
			// above). Physical reclamation is deferred to the per-tenant index/uniqueness pivot (RM-28).
			draftOnly, err := s.persistEdit(ctx, tenantID, field)
			if err != nil {
				if errors.Is(err, metadata.ErrFieldNotFound) {
					return nil, invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
				}
				return nil, fmt.Errorf("services.EditField: persist update: %w", err)
			}
			return s.editCommitted(ctx, tenantID, in, beforeField, field, draftOnly)
		}
	}

	draftOnly, err := s.persistEdit(ctx, tenantID, field)
	if err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return nil, invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
		}
		return nil, fmt.Errorf("services.EditField: persist update: %w", err)
	}
	return s.editCommitted(ctx, tenantID, in, beforeField, field, draftOnly)
}

// persistEdit writes the editable attributes of a field: through the draft layer when one is wired
// (a builder on a delivered workspace saves a draft, options.indexed excepted), else straight to the
// registry. draftOnly=true means nothing live changed — the customer's audit log must not hear of it.
func (s *FieldService) persistEdit(ctx context.Context, tenantID string, field *metadata.FieldDefinition) (draftOnly bool, err error) {
	if s.drafts != nil {
		return s.drafts.SaveEdit(ctx, tenantID, field)
	}
	return false, s.meta.UpdateFieldMutable(ctx, tenantID, field)
}

// editCommitted is the ONE tail every successful EditField runs. Invalidate + audit + formula
// sweep are one tail, not independent habits: the index-toggle branches once returned having done
// only the audit, so a committed field edit stayed invisible to record validation for up to the
// cache TTL (60s). The cache is invalidated AFTER commit (O3); the audit diff is the changed
// metadata (RM-19, fail-open). A formula whose options were sent (Studio always sends them) is
// re-evaluated on every stored row — an expression, resultType or currencyCode change otherwise
// leaves rows in the old value or shape — and re-saving a formula is how an admin retries a sweep.
func (s *FieldService) editCommitted(ctx context.Context, tenantID string, in EditFieldInput, before metadata.FieldDefinition, field *metadata.FieldDefinition, draftOnly bool) (*metadata.FieldDefinition, error) {
	s.cache.InvalidateFields(tenantID, field.EntityID)
	if draftOnly {
		// A draft-only save changed nothing live: no audit row (it would show unpublished work to the
		// customer's admin) and no formula sweep (the live expression did not change).
		return field, nil
	}
	auditAfter := *field
	if s.drafts != nil {
		live, err := s.meta.GetFieldByID(ctx, tenantID, field.ID)
		if err != nil {
			return nil, fmt.Errorf("services.EditField: field saved, resolve live field for audit: %w", err)
		}
		auditAfter = *live
	}
	emitMetaAudit(ctx, s.auditor, tenantID, audit.FieldMeta, audit.ActionUpdate, before, auditAfter)
	if field.FieldType != "formula" || in.Options == nil || s.formulaRows == nil {
		return field, nil
	}
	entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
	if err != nil {
		return nil, fmt.Errorf("services.EditField: field saved, resolve entity for recompute: %w", err)
	}
	if err := s.recomputeFormulas(ctx, tenantID, field.EntityID, entityName); err != nil {
		return nil, fmt.Errorf("services.EditField: field saved, stored formulas not recomputed: %w", err)
	}
	return field, nil
}

// guardRequiredWhileLocked is the EditField half of the queue #62 dead end (queue #142). PutGrants
// refuses to LOCK a required field the engine cannot fill; the same state is reached the other way
// round when a field some role already has read-only or hidden is made required, or loses the
// default that was filling it. That role then cannot create anything (the create guard refuses
// every locked key, and the create path can only explain it after the fact). So the edit that
// CREATES the shape is refused, naming the roles it would strand and the exits.
//
// Only a transition is judged (the queue #62 rule): an edit that leaves an already-dead-end field
// dead-ended (rows saved before the PutGrants guard) is allowed, so the admin can still relabel it
// or turn "bắt buộc" off. A lock-read failure refuses the edit (fail closed: the field row is not
// written, nothing is lost). nil restrictions = guard off (unit tests that predate it).
func (s *FieldService) guardRequiredWhileLocked(ctx context.Context, tenantID string, stored, effective metadata.FieldDefinition) error {
	if s.restrictions == nil {
		return nil
	}
	return CheckRequiredWhileLocked(ctx, s.restrictions, tenantID, func() (string, error) {
		return s.meta.GetEntityNameByID(ctx, tenantID, stored.EntityID)
	}, stored, effective)
}

// ColumnChecker reads whether a physical column exists on an entity's obj_* table (*dynrepo.Repository).
type ColumnChecker interface {
	ColumnExists(ctx context.Context, entity, field string) (bool, error)
}

// CheckRelationColumn is AddField's column-collision guard for a relation or user field, which
// materializes a `<field>_id` FK column: a column of that name already on the table (a system column
// such as owner_id, or one an indexed field literally named so created) is a 422
// FIELD_RELATION_COLUMN_CONFLICT, never skipped-and-persisted (the relation would hijack it). Exported
// so the AI Builder's propose_field refuses at propose time what Apply refuses (issue #42). Any other
// field type derives no such column: nil. A read failure is returned as a non-validation error.
func CheckRelationColumn(ctx context.Context, cols ColumnChecker, entityName, fieldName, fieldType string) error {
	if fieldType != "relation" && fieldType != "user" {
		return nil
	}
	col := fieldName + "_id"
	exists, err := cols.ColumnExists(ctx, entityName, col)
	if err != nil {
		return fmt.Errorf("services.CheckRelationColumn: %w", err)
	}
	if exists {
		return invalid("FIELD_RELATION_COLUMN_CONFLICT",
			"không thể thêm field %s %q: cột %q đã tồn tại trên entity %q", fieldType, fieldName, col, entityName)
	}
	return nil
}

// CheckRequiredWhileLocked is the body of guardRequiredWhileLocked, exported so the AI Builder's
// propose_edit_field judges the same transition with the same predicate (queue #319) instead of a
// mirror: the proposal card then fails with FIELD_REQUIRED_WHILE_LOCKED at propose time, naming the
// stranded roles, rather than turning red only at Apply. entityName yields the entity's registry
// name (the sys_field_acl key) and is called only once a transition is confirmed, so the common
// no-op edit costs no lookup; a read failure is returned as a non-validation error (fail closed).
func CheckRequiredWhileLocked(ctx context.Context, restrictions FieldRestrictionLister, tenantID string, entityName func() (string, error), stored, effective metadata.FieldDefinition) error {
	after, err := requiredEngineCannotFill(effective)
	if err != nil {
		return fmt.Errorf("services.EditField: lock guard: %w", err)
	}
	if !after {
		return nil
	}
	before, err := requiredEngineCannotFill(stored)
	if err != nil {
		return fmt.Errorf("services.EditField: lock guard: %w", err)
	}
	if before {
		return nil
	}
	entity, err := entityName()
	if err != nil {
		return fmt.Errorf("services.EditField: lock guard: resolve entity: %w", err)
	}
	rows, err := restrictions.ListFieldRestrictions(ctx, tenantID, entity, stored.Name)
	if err != nil {
		return fmt.Errorf("services.EditField: lock guard: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, 0, len(rows))
	roles := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		name := r.RoleLabel
		if name == "" {
			name = r.RoleName
		}
		names = append(names, name)
		roles = append(roles, map[string]any{"id": r.RoleID, "name": r.RoleName, "label": r.RoleLabel, "hidden": !r.CanView})
	}
	label := effective.Label
	if label == "" {
		label = stored.Name
	}
	exit := "gỡ khoá field này cho các vai trên (Phân quyền field) rồi sửa lại"
	if studioCanSetDefault(stored.FieldType) {
		exit += ", hoặc đặt giá trị mặc định cho field trong cùng lần sửa này"
	} else {
		exit += fmt.Sprintf(" (Studio không đặt được giá trị mặc định cho kiểu %q)", stored.FieldType)
	}
	return &ValidationError{
		Code: "FIELD_REQUIRED_WHILE_LOCKED",
		Msg: fmt.Sprintf("Không thể làm trường %q thành bắt buộc mà máy không tự điền: trường đang bị khoá hoặc ẩn với %d vai (%s) — các vai đó sẽ không tạo được bản ghi %s nào. Cách gỡ: %s.",
			label, len(rows), strings.Join(names, ", "), entity, exit),
		Field:   stored.Name,
		Details: map[string]any{"field": stored.Name, "roles": roles, "defaultSupported": studioCanSetDefault(stored.FieldType)},
	}
}

// guardRequiredHasCondition refuses to turn `required` ON for a field the entity's edit layout shows
// only under a visibleWhen condition (queue #259, specs/054 FR-6: a required field must not be
// hidden). Without it the edit saved clean and the NEXT layout save failed with
// LAYOUT_VISIBLE_WHEN_INVALID, far from its cause. Judges only the flip false→true (a field already
// required keeps its edits; turning required off is the exit); the layout is read only then. A read
// failure refuses the edit (fail closed) rather than let the dead end through unverified.
func (s *FieldService) guardRequiredHasCondition(ctx context.Context, tenantID string, stored, effective metadata.FieldDefinition) error {
	if s.layouts == nil || stored.IsRequired || !effective.IsRequired {
		return nil
	}
	entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, stored.EntityID)
	if err != nil {
		return fmt.Errorf("services.EditField: visibleWhen guard: resolve entity: %w", err)
	}
	rows, err := s.layouts.GetByEntity(ctx, tenantID, entityName)
	if err != nil {
		return fmt.Errorf("services.EditField: visibleWhen guard: read layout: %w", err)
	}
	all, err := s.meta.ListFields(ctx, tenantID, stored.EntityID)
	if err != nil {
		return fmt.Errorf("services.EditField: visibleWhen guard: list fields: %w", err)
	}
	live := make(map[string]struct{}, len(all))
	for _, f := range all {
		live[f.Name] = struct{}{}
	}
	conds, err := storedEditConditions(rows, func(name string) bool { _, ok := live[name]; return ok })
	if err != nil {
		return fmt.Errorf("services.EditField: visibleWhen guard: %w", err)
	}
	vc, conditional := conds[stored.Name]
	if !conditional {
		return nil
	}
	label := effective.Label
	if label == "" {
		label = stored.Name
	}
	return &ValidationError{
		Code: "FIELD_REQUIRED_HAS_VISIBLE_WHEN",
		Msg: fmt.Sprintf("Không thể làm trường %q thành bắt buộc: bố cục sửa đang chỉ hiện trường này khi trường %q thoả điều kiện — trường bắt buộc không được ẩn theo điều kiện. Cách gỡ: bỏ điều kiện ẩn/hiện của ô này trong bố cục, hoặc để trường không bắt buộc.",
			label, vc.Field),
		Field:   stored.Name,
		Details: map[string]any{"field": stored.Name, "controllerField": vc.Field},
	}
}

// guardCurrencyCodeChange refuses to change a currency field's configured options.currencyCode
// while records of this tenant still store the OLD code (queue #157) — live ones AND ones in the
// recycle bin, since RecordService.Restore brings a trashed record back unchanged (the review of
// #157 reproduced "trash, change the code, restore" relabelling USD 10 as VND 10). The per-record value is
// {amount, currencyCode} and every money reader (aggregate, rollup, report) sums `.amount` under
// the CONFIGURED code, so the edit would silently turn each of those records into the same number
// in another currency — the very relabel Q7 (queue #7) forbids on the record path, and the vector
// that produced the rows Doctor D2_CURRENCY_CODE_MISMATCH reports. No conversion, no relabel.
//
// Only the rows the edit CHANGES are judged (the queue #62 rule): rows already in the new code, or
// in a third code (legacy mismatches, which stay mismatched either way), do not block it, so the
// edit that FIXES a field whose rows are all in the new code goes through. A stored config with no
// code (legacy row predating validateCurrencyConfig) compared nothing, so gaining one is not
// judged. A count failure refuses the edit (fail closed: the field row is not written).
func (s *FieldService) guardCurrencyCodeChange(ctx context.Context, tenantID string, stored metadata.FieldDefinition, storedOpts, newOpts map[string]any) error {
	if newOpts == nil {
		return nil
	}
	oldCodes := configuredCurrencyCodes(stored.Name, stored.FieldType, storedOpts)
	if len(oldCodes) == 0 {
		return nil
	}
	newCodes := configuredCurrencyCodes(stored.Name, stored.FieldType, newOpts)
	paths := make([]string, 0, len(oldCodes))
	for p := range oldCodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var entityName string
	for _, path := range paths {
		from, to := oldCodes[path], newCodes[path] // to: validateFieldConfig guarantees a valid 3-letter code
		if from == "" || to == "" || to == from {
			continue
		}
		if entityName == "" {
			var err error
			if entityName, err = s.meta.GetEntityNameByID(ctx, tenantID, stored.EntityID); err != nil {
				return fmt.Errorf("services.EditField: currency guard: resolve entity: %w", err)
			}
		}
		live, trashed, err := s.schema.CountCurrencyCodeRows(ctx, entityName, tenantID, path, from)
		if err != nil {
			return fmt.Errorf("services.EditField: currency guard: %w", err)
		}
		n := live + trashed
		if n == 0 {
			continue
		}
		label := stored.Label
		if label == "" {
			label = stored.Name
		}
		if path != stored.Name { // a composite's currency sub-field (#440)
			label += " (" + strings.TrimPrefix(path, stored.Name+".") + ")"
		}
		inBin := ""
		if trashed > 0 {
			inBin = fmt.Sprintf(" (trong đó %d bản ghi ở thùng rác — khôi phục lại vẫn mang mã %s)", trashed, from)
		}
		return &ValidationError{
			Code: "FIELD_CURRENCY_CODE_IN_USE",
			Msg: fmt.Sprintf("Không đổi được mã tiền tệ của trường %q từ %s sang %s: %d bản ghi%s đang lưu số tiền theo %s, và đổi mã cấu hình không quy đổi số tiền — mọi tổng và báo cáo sẽ đọc chúng thành %s cùng con số. Cách làm: giữ mã %s và thêm một trường tiền tệ mới theo %s, hoặc xoá vĩnh viễn các bản ghi đó trước khi đổi mã (xoá vào thùng rác chưa đủ).",
				label, from, to, n, inBin, from, to, from, to),
			Field:   path,
			Details: map[string]any{"field": path, "from": from, "to": to, "records": n, "recycleBin": trashed},
		}
	}
	return nil
}

// configuredCurrencyCodes lists the configured options.currencyCode of a field's money values,
// keyed by the JSON path the value lives at: the field name for a currency field, "name.sub" for
// each currency sub-field of a composite (#440; sub-fields are flat, so one level). Entries with
// no configured code are left out — nothing to compare against.
func configuredCurrencyCodes(name, fieldType string, opts map[string]any) map[string]string {
	out := map[string]string{}
	codeOf := func(o any) string {
		m, _ := o.(map[string]any)
		c, _ := m["currencyCode"].(string)
		return c
	}
	switch fieldType {
	case "currency":
		if c := codeOf(opts); c != "" {
			out[name] = c
		}
	case "composite":
		subs, _ := opts["subFields"].([]any)
		for _, raw := range subs {
			sub, _ := raw.(map[string]any)
			subName, _ := sub["name"].(string)
			if ft, _ := sub["fieldType"].(string); ft == "currency" && subName != "" {
				if c := codeOf(sub["options"]); c != "" {
					out[name+"."+subName] = c
				}
			}
		}
	}
	return out
}

func (s *FieldService) guardFormulaDependants(ctx context.Context, tenantID string, deleting metadata.FieldDefinition) error {
	siblings, err := s.meta.ListFields(ctx, tenantID, deleting.EntityID)
	if err != nil {
		return fmt.Errorf("services.DeleteField: list siblings: %w", err)
	}
	var deps []string
	for _, f := range siblings {
		if f.FieldType != "formula" || f.ID == deleting.ID {
			continue
		}
		opts, err := parseOptionsJSON(f.Options)
		if err != nil {
			return fmt.Errorf("services.DeleteField: parse formula options of %q: %w", f.Name, err)
		}
		expr, _ := opts["formula"].(string)
		// An unparseable stored formula reads nothing we can name: skip it (it is already broken and
		// evaluates to nothing). Deliberately fail-open, unlike field_acl.go (read-all): this guard only
		// protects delete, and blocking on a dead formula would make the field undeletable forever.
		idents, ierr := formula.Identifiers(expr)
		if ierr != nil {
			continue
		}
		for _, id := range idents {
			if id == deleting.Name {
				deps = append(deps, f.Name)
				break
			}
		}
	}
	if len(deps) == 0 {
		return nil
	}
	label := deleting.Label
	if label == "" {
		label = deleting.Name
	}
	return &ValidationError{
		Code:    "FIELD_HAS_FORMULA_DEPENDANTS",
		Msg:     fmt.Sprintf("Không xoá được trường %q: công thức %s đang dùng nó. Sửa hoặc xoá các công thức đó trước.", label, strings.Join(deps, ", ")),
		Field:   deleting.Name,
		Details: map[string]any{"field": deleting.Name, "formulas": deps},
	}
}

// DeleteField soft-deletes a field. Per §4.4 the field's sys_fields row is marked deleted
// (deleted_at) and the underlying obj_* column / `data` values are KEPT — no ALTER DROP is
// ever issued, so the operation is safe and reversible. The flow is:
//
//  1. Resolve the field tenant-scoped (GetFieldByID) — unknown/cross-tenant id -> 404.
//     This is also the tenant-isolation guard: a field id owned by another tenant
//     resolves to not-found, so no cross-tenant row is read or deleted.
//  2. Soft-delete the resolved field by id. No schema repo is touched -> zero DDL.
//  3. ENG-REL2 #4: if the field is a `relation` field, ALSO soft-delete its companion 1-N
//     sys_relationships registry row, so the registry does not keep advertising an inverse
//     relation for a field that no longer exists. This runs AFTER the field's own soft-delete
//     commits (step 2) — the field is already gone by the time this step can fail, so a
//     failure here is surfaced (never swallowed, per project convention) but leaves the field
//     soft-deleted with a stale-but-inert registry row rather than rolling anything back
//     (there is no multi-statement transaction spanning steps 2+3; the field delete is a
//     single-table soft-delete and this is a best-effort, go-forward cleanup — a retry of
//     DeleteField on the same id would 404 since the field is already gone, so an admin would
//     need to clean up the orphaned registry row directly in that rare failure case).
func (s *FieldService) DeleteField(ctx context.Context, tenantID string, in DeleteFieldInput) error {
	if strings.TrimSpace(in.FieldID) == "" {
		return invalid("FIELD_ID_REQUIRED", "thiếu id field cần xoá")
	}

	// Tenant-scoped resolve. A field id from another tenant -> ErrFieldNotFound, never a
	// cross-tenant hit; an already-deleted field is also excluded (GORM DeletedAt).
	field, err := s.meta.GetFieldByID(ctx, tenantID, in.FieldID)
	if err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
		}
		return fmt.Errorf("services.DeleteField: resolve field: %w", err)
	}

	// #233: refuse while a sibling formula still reads this field — it would silently evaluate to 0
	// on every later write. Identifiers() shares the formula package's AST walk (no second parser).
	if err := s.guardFormulaDependants(ctx, tenantID, *field); err != nil {
		return err
	}

	// specs/063 S7-9: on a delivered workspace the builder's delete is a "sẽ xoá khi xuất bản" draft;
	// the customer keeps the field and its data, and Publish later calls this method again with
	// WithPublishApply, which falls through to the real delete below. Nothing is audited for a draft
	// (the customer's /audit-log must not show unpublished work).
	if s.drafts != nil {
		drafted, derr := s.drafts.SaveFieldDelete(ctx, tenantID, field)
		if derr != nil {
			return fmt.Errorf("services.DeleteField: draft delete: %w", derr)
		}
		if drafted {
			return nil
		}
	}

	// CLEANUPS FIRST, tombstone last. Both cleanups below are idempotent deletes, so a failure
	// here now leaves the field LIVE and the whole call retryable. The other order — tombstone,
	// then clean — made a rare failure unrecoverable and said so in its own comment ("a retry of
	// DeleteField would 404 … an admin-cleanup case"): the field row was already soft-deleted, so
	// the retry resolves nothing and the residue could only be removed by hand. Same shape as the
	// entity-delete and file-delete orderings fixed alongside this.

	// RM-28: a UNIQUE field keeps its obj_* generated column after this soft-delete (no DDL), so its
	// pivot rows would linger and 409 a record the tenant can no longer see once the field is
	// re-created. Purge them (tenant+entity+field). Surfaced (never swallowed) — a retry of
	// DeleteField would 404, so a rare failure here is an admin-cleanup case, not silent data-bleed.
	if field.IsUnique {
		entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
		if err != nil {
			return fmt.Errorf("services.DeleteField: resolve entity for pivot cleanup: %w", err)
		}
		if err := s.schema.DeletePivotByField(ctx, entityName, tenantID, field.Name); err != nil {
			return fmt.Errorf("services.DeleteField: delete pivot rows: %w", err)
		}
	}

	// ENG-REL2 #4: a `relation` field owns a 1-N registry row keyed by (tenant, child entity,
	// child_fk_column). Resolve the owning entity's NAME (the field row carries only entity_id)
	// then soft-delete the matching row. Missing row is a no-op (pre-000028 field, or already
	// cleaned up) — see SoftDeleteRelationshipByChildFK's doc comment.
	if field.FieldType == "relation" {
		entityName, err := s.meta.GetEntityNameByID(ctx, tenantID, field.EntityID)
		if err != nil {
			return fmt.Errorf("services.DeleteField: resolve entity for relationship cleanup: %w", err)
		}
		if err := s.meta.SoftDeleteRelationshipByChildFK(ctx, tenantID, entityName, field.Name+"_id"); err != nil {
			return fmt.Errorf("services.DeleteField: soft delete relationship registry: %w", err)
		}
	}

	// A pending draft of this field would be orphaned by the delete (Publish conflict / ErrFieldNotFound).
	if s.drafts != nil {
		if err := s.drafts.DropDraft(ctx, tenantID, field.ID); err != nil {
			return fmt.Errorf("services.DeleteField: drop field draft: %w", err)
		}
	}

	// Soft-delete LAST. No FieldSchemaRepo call -> no ALTER, the obj_* column survives.
	// tenantID is re-pinned at the repo (defense-in-depth) so even a forged id cannot
	// cross tenants here.
	if err := s.meta.SoftDeleteField(ctx, tenantID, field.ID); err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return invalid("FIELD_NOT_FOUND", "field %q không tồn tại", in.FieldID)
		}
		return fmt.Errorf("services.DeleteField: soft delete: %w", err)
	}

	// Invalidate the entity's cached field list AFTER commit so cached resolve drops the field (O3).
	s.cache.InvalidateFields(tenantID, field.EntityID)
	// RM-19: audit the committed field delete (before-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.FieldMeta, audit.ActionDelete, *field, nil)
	return nil
}
