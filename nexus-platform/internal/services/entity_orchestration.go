package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// SchemaRepo materializes and removes the physical obj_* table for an entity. Implemented
// by the sqlx dynamic-record repository; abstracted here so orchestration is unit-testable.
type SchemaRepo interface {
	TableExists(ctx context.Context, entity string) (bool, error)
	CreateTable(ctx context.Context, entity string, fields []ddl.FieldDef) error
	DropTable(ctx context.Context, entity string) error
	// ColumnExists / AddUniqueGencolNonUnique / AddRelationColumn back the reconcile path for a
	// SHARED obj_* table (bug3): obj_<name> is keyed by entity name, NOT by tenant, so two
	// tenants creating an entity with the same name share one physical table. The second
	// creator's CREATE TABLE IF NOT EXISTS is a no-op, so a unique/relation field that the
	// first creator did NOT have would silently never get its physical column (unique never
	// enforced, relation FK column missing). After CreateTable on a pre-existing table we
	// reconcile: for each unique/relation field whose column is absent, ALTER it in.
	// Implemented by the sqlx dynamic-record repository (dynrepo.ColumnExists /
	// AddUniqueGencolNonUnique / AddRelationColumn) — the same trio FieldSchemaRepo uses.
	//
	// RM-28 Slice B: AddUniqueGencolNonUnique (not AddUniqueColumn) — the reconciled column
	// gets a NON-unique idx_ index; uniqueness for `is_unique` fields is enforced solely by
	// the sys_unique_index pivot, never a physical UNIQUE index shared across tenants.
	ColumnExists(ctx context.Context, entity, field string) (bool, error)
	AddUniqueGencolNonUnique(ctx context.Context, entity, field string) error
	AddRelationColumn(ctx context.Context, entity, field string) error
	// AddOneToOneRelationColumn / DropOneToOneRelationColumn back the reconcile path for a
	// SHARED obj_* table (ENG-REL2 #5, options.cardinality == "1-1"): the CASE-guarded FK
	// column + tenant-scoped UNIQUE key pair, distinct from AddRelationColumn/
	// DropRelationColumn's ordinary 1-N pair (different index shape/name).
	AddOneToOneRelationColumn(ctx context.Context, entity, field string) error
	DropOneToOneRelationColumn(ctx context.Context, entity, field string) error
	// DropUniqueGencolNonUnique / DropRelationColumn reverse the corresponding Add* ALTER.
	// They back the create-entity compensation path for a SHARED obj_* table: when a
	// reconcile ALTER auto-commits but the metadata write fails afterward (or a later
	// reconcile column fails), only the columns THIS call added are dropped — never the
	// shared table, never a co-tenant's columns. Implemented by
	// dynrepo.DropUniqueGencolNonUnique / DropRelationColumn.
	DropUniqueGencolNonUnique(ctx context.Context, entity, field string) error
	DropRelationColumn(ctx context.Context, entity, field string) error
	// AddIndexedColumn / DropIndexedColumn back the reconcile path for a SHARED obj_* table
	// (D1): when a second tenant creates an entity with the same name and some fields are
	// indexed, the shared table may be missing the typed generated column + non-unique index.
	// After CreateTable (no-op for pre-existing table) we reconcile missing indexed columns,
	// tracking exactly the ones THIS call added so compensation can undo only those.
	AddIndexedColumn(ctx context.Context, entity, field, fieldType string) error
	DropIndexedColumn(ctx context.Context, entity, field string) error
	// AddPolymorphicColumns / DropPolymorphicColumns back the reconcile path for a SHARED obj_*
	// table (ACTIVITY-CORE #3a): a polymorphic_relation field materializes two gen-columns
	// (<field>_type, <field>_id) + a composite index. When the shared table pre-exists (a second
	// tenant sharing the entity name), the reconcile ALTERs them in; the Drop reverses only the
	// columns THIS call added on the compensation path.
	AddPolymorphicColumns(ctx context.Context, entity, field string) error
	DropPolymorphicColumns(ctx context.Context, entity, field string) error
	// SoftDeleteRecords marks all of ONE tenant's active rows in the shared obj_<entity>
	// table as deleted. It backs the entity-delete data-bleed guard (RM-03): because obj_*
	// tables are shared by entity name across tenants, deleting + recreating an entity must
	// hide the old tenant's rows so the recreated entity starts empty. Part of the interface
	// (not a runtime capability-assert) so the security-critical sweep can never be silently
	// skipped — a missing implementation is a compile error, not a fail-open at runtime.
	//
	// S7-9: SoftDeleteRecordsAt stamps the caller-chosen instant and returns how many rows moved
	// (SoftDeleteRecords is the same with now); the interface carries the At form so the instant a
	// publish recorded can never be dropped by a caller that forgot it.
	SoftDeleteRecordsAt(ctx context.Context, entity, tenantID string, at time.Time) (int64, error)
}

// MetadataWriter persists an entity definition and its fields atomically (one transaction)
// and reports whether a name is already taken for a tenant. Implemented by the GORM
// metadata repository.
type MetadataWriter interface {
	EntityExists(ctx context.Context, tenantID, name string) (bool, error)
	// NameClaimedByAnyTenant answers the OWNERSHIP question the compensation path needs — see
	// safeToDropSharedTable. Unscoped by design: obj_<name> tables are shared across tenants.
	NameClaimedByAnyTenant(ctx context.Context, name string) (bool, error)
	// GetEntityByName resolves an entity by tenant + name, returning metadata.ErrEntityNotFound
	// when none exists. CreateEntity uses it to verify a relation field's FK target exists for
	// THIS tenant before any DDL runs (mirroring AddField), so an absent target is rejected
	// 422 at create-time rather than 500-ing on every later record insert.
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	// ListFields resolves the active field definitions of an already-persisted entity
	// (tenant-scoped). ENG-COMPUTED #3 (rollup) uses it to validate a rollup field's
	// options.viaRelation / options.aggField against the CHILD entity's field list before any
	// DDL runs (validateRollupCrossEntity).
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	// GetRelationshipByName resolves a registered relationship (any type) by tenant + name,
	// returning metadata.ErrRelationshipNotFound when none exists. ENG-COMPUTED #3 (rollup)
	// uses it to validate a rollup field's options.relationship (N-N mode) at config time.
	GetRelationshipByName(ctx context.Context, tenantID, name string) (*metadata.RelationshipDefinition, error)
	// GetRelationshipByInverseField resolves the active relationship (any type) claiming a
	// given (parent entity, inverse field) pair, scoped to the tenant. ENG-REL2 #4 uses it to
	// detect an inverse-field naming collision before registering a new 1-N relationship row
	// for one of THIS entity's relation fields. Returns metadata.ErrRelationshipNotFound when
	// the name is unclaimed (not an error case — "available").
	GetRelationshipByInverseField(ctx context.Context, tenantID, parentEntityName, inverseField string) (*metadata.RelationshipDefinition, error)
	// CreateEntityWithFields persists the entity + its fields + (ENG-REL2 #4) any 1-N
	// relationship registry rows its relation fields need, all in ONE transaction. rels is
	// nil/empty when the entity declares no relation fields.
	CreateEntityWithFields(ctx context.Context, e *metadata.EntityDefinition, fields []metadata.FieldDefinition, rels []metadata.RelationshipDefinition) error
}

// EntityService orchestrates entity creation: validate → create table → persist metadata,
// with a compensating DROP if metadata persistence fails (MySQL auto-commits DDL, so the
// table cannot be rolled back inside the metadata transaction — see the Technical Document).
type EntityService struct {
	schema SchemaRepo
	meta   MetadataWriter
	// cache, when set, is the shared metadata cache that RecordService reads through. Entity
	// writes invalidate it AFTER commit so cached reads never go stale. Nil = no caching (all
	// Invalidate* calls are nil-safe), so existing wiring/tests keep working unchanged.
	cache *MetaCache
	// auditor records committed entity-metadata mutations (RM-19), async + fail-open.
	// Defaults to a noop so existing wiring/tests run unchanged; injected via WithAuditor.
	auditor audit.Auditor
	// draftFields, when set, lets an entity delete drop its fields' pending S7-8 drafts (no orphans).
	draftFields *DraftFieldRepo
	// aclCache is the role-grants cache (O3). A table delete drops every role's grants on its name, so it drops
	// the cached grants after it commits. nil = no cache.
	aclCache *CachedACLLoader
}

// WithACLCache wires the role-grants cache that a table delete refreshes. Chainable.
func (s *EntityService) WithACLCache(c *CachedACLLoader) *EntityService {
	s.aclCache = c
	return s
}

// WithDraftFields wires the field draft layer so SoftDeleteEntity drops the drafts of the fields it removes.
func (s *EntityService) WithDraftFields(d *DraftFieldRepo) *EntityService {
	s.draftFields = d
	return s
}

// NewEntityService wires the orchestration over a schema repo and a metadata writer.
func NewEntityService(schema SchemaRepo, meta MetadataWriter) *EntityService {
	return &EntityService{schema: schema, meta: meta, auditor: audit.NewNoopAuditor()}
}

// WithMetaCache attaches the shared metadata cache so entity writes invalidate it after commit
// (O3). Chainable; returns the same service.
func (s *EntityService) WithMetaCache(c *MetaCache) *EntityService {
	s.cache = c
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so committed entity mutations emit an
// append-only audit row (async, fail-open). The actor user id is read from the request
// context (audit.WithActor, set by the admin handler). Chainable; a nil auditor is ignored.
func (s *EntityService) WithAuditor(a audit.Auditor) *EntityService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// reconciledCol records a single reconcile ALTER this CreateEntity call applied to a SHARED
// obj_* table, so compensation can undo exactly those (and only those) columns. `field` is
// the bare field name (as passed to Add*Column); `kind` selects which Drop* reverses it.
type reconciledCol struct {
	field string
	kind  reconciledKind
	// fieldType is only set for kind==reconciledIndexed so DropIndexedColumn knows which
	// column to drop (it uses the field name, not the type, so this is informational only).
	fieldType string
}

// reconciledKind identifies which DDL pair (Add*/Drop*) was used for a reconciled column.
type reconciledKind int

const (
	reconciledUnique           reconciledKind = iota // AddUniqueGencolNonUnique / DropUniqueGencolNonUnique (RM-28 Slice B)
	reconciledRelation                               // AddRelationColumn / DropRelationColumn
	reconciledIndexed                                // AddIndexedColumn / DropIndexedColumn (D1)
	reconciledOneToOneRelation                       // AddOneToOneRelationColumn / DropOneToOneRelationColumn (ENG-REL2 #5)
	reconciledPolymorphic                            // AddPolymorphicColumns / DropPolymorphicColumns (ACTIVITY-CORE #3a)
)

// compensateReconcile drops the columns a CreateEntity reconcile pass added to a shared
// obj_* table, in reverse order. It is best-effort: cleanup failures are intentionally
// swallowed (the caller is already returning the originating error, and there is no second
// channel to surface a cleanup failure on without masking it). It NEVER drops the table —
// the shared table belongs to a co-tenant — only the specific generated columns/indexes
// this call ALTERed in. A nil/empty slice is a no-op.
func compensateReconcile(ctx context.Context, schema SchemaRepo, entity string, cols []reconciledCol) {
	for i := len(cols) - 1; i >= 0; i-- {
		c := cols[i]
		switch c.kind {
		case reconciledRelation:
			_ = schema.DropRelationColumn(ctx, entity, c.field)
		case reconciledOneToOneRelation:
			_ = schema.DropOneToOneRelationColumn(ctx, entity, c.field)
		case reconciledIndexed:
			_ = schema.DropIndexedColumn(ctx, entity, c.field)
		case reconciledPolymorphic:
			_ = schema.DropPolymorphicColumns(ctx, entity, c.field)
		default: // reconciledUnique
			_ = schema.DropUniqueGencolNonUnique(ctx, entity, c.field)
		}
	}
}

// CreateEntity validates the payload, creates the obj_* table, then persists the entity +
// field metadata. On metadata failure it drops the just-created table so no orphan ("ghost")
// table is left behind. Returns the persisted entity and its fields.
// safeToDropSharedTable decides whether the compensation path may physically DROP obj_<name>.
//
// obj_<name> is keyed by ENTITY NAME, not by tenant, so several tenants share one table. The old
// rule was `!existedBefore` — a snapshot taken with TableExists many statements earlier, before the
// CREATE, the reconcile ALTERs and the metadata write. That is a time-of-check/time-of-use gap: if
// another tenant registers the same-named entity inside it, the losing request "rolls back" by
// dropping the WINNER's live table, taking their rows with it while their sys_entities row survives
// — every later request against that entity then 500s, and nothing in the trail says why.
//
// So ask ownership instead of history: drop only when we created the table AND no tenant currently
// claims the name. On a lookup error, refuse to drop — leaving an empty orphan table is recoverable
// (the next create adopts it via CREATE TABLE IF NOT EXISTS), destroying a live one is not.
func (s *EntityService) safeToDropSharedTable(ctx context.Context, name string, existedBefore bool) bool {
	if existedBefore {
		return false
	}
	claimed, err := s.meta.NameClaimedByAnyTenant(ctx, name)
	if err != nil {
		// No logger on this service; the caller already surfaces the underlying metadata error, and
		// the safe choice is silent-and-conservative rather than noisy-and-destructive.
		return false
	}
	return !claimed
}

func (s *EntityService) CreateEntity(ctx context.Context, tenantID string, in CreateEntityInput) (*metadata.EntityDefinition, []metadata.FieldDefinition, error) {
	if err := ValidateCreateEntity(in); err != nil {
		return nil, nil, err
	}
	// Names are already validated; re-run sanitize to get the normalized (lowercased) form.
	name, err := ddl.SanitizeIdentifier(in.Name)
	if err != nil {
		return nil, nil, invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}

	// Reject duplicates before any DDL runs, so we never create a table we must roll back.
	exists, err := s.meta.EntityExists(ctx, tenantID, name)
	if err != nil {
		return nil, nil, fmt.Errorf("services.CreateEntity: check existence: %w", err)
	}
	if exists {
		return nil, nil, invalid("ENTITY_ALREADY_EXISTS", "entity %q đã tồn tại", name)
	}

	ddlFields := make([]ddl.FieldDef, len(in.Fields))
	for i, f := range in.Fields {
		fname, _ := ddl.SanitizeIdentifier(f.Name)
		fd := ddl.FieldDef{Name: fname, FieldType: f.FieldType, IsUnique: f.IsUnique}
		// A 1-N `relation` field (M3, FK-on-child) carries its FK target so BuildCreateTable
		// materializes the `<f>_id` column + index inline. Presence/validity of
		// options.relatedEntity was enforced by ValidateCreateEntity; here we just pass it
		// through (empty for non-relation fields, which ignore it).
		if f.FieldType == "relation" {
			fd.RelatedEntity, _ = relatedEntityName(f.Options)
			// ENG-REL2 #5: cardinality selects the DDL shape BuildCreateTable materializes
			// (CASE-guarded FK column + tenant-scoped UNIQUE for "1-1"; the ordinary plain
			// FK column + non-unique index otherwise). Validated by validateRelationCardinality
			// in ValidateCreateEntity, above.
			fd.OneToOne = relationCardinality(f.Options) == "1-1"
		}
		// D1: pass Indexed so BuildCreateTable emits the typed generated column + non-unique
		// index at create time (rather than needing a later reconcile ALTER). Skip when
		// IsUnique is true — BuildCreateTable already guards this internally, but we match
		// the flag here for clarity.
		if !f.IsUnique {
			fd.Indexed = boolOpt(f.Options, "indexed")
		}
		// ACTIVITY-CORE #3a: a polymorphic_relation field materializes its two gen-columns +
		// composite index inline in CREATE TABLE. Config (non-empty targetEntities) was enforced
		// by ValidateCreateEntity; per-target existence is checked in the loop below.
		if f.FieldType == "polymorphic_relation" {
			fd.Polymorphic = true
		}
		ddlFields[i] = fd
	}

	// Verify each relation field's FK target exists for THIS tenant BEFORE any DDL runs,
	// mirroring AddField (field_orchestration.go). CreateEntity previously skipped this, so a
	// relation pointing at a never-provisioned target created the entity fine but 500'd on
	// every later record insert (RecordExists probes a missing obj_<target> table). We resolve
	// the target tenant-scoped (a target owned by another tenant counts as not-found) and reject
	// 422 here instead. SELF-REFERENCE is the one exception: a relation whose target is the
	// entity being created (e.g. employees.manager -> employees) names a table that does not yet
	// exist in metadata but is materialized moments later by CreateTable, so we MUST skip the
	// existence check for it — otherwise valid self-referencing entities would be rejected.
	//
	// ENG-REL2 #4: once the target is confirmed (or skipped for self-reference), resolve this
	// field's inverse-field registry row too — a naming collision must reject 422 before any
	// DDL runs, exactly like the target-existence check. inverseClaims tracks names claimed by
	// EARLIER relation fields in THIS SAME entity (batch-local — none of them are persisted yet,
	// so two sibling relation fields pointing at the same parent must not silently collide).
	var pendingRels []metadata.RelationshipDefinition
	inverseClaims := pendingInverseClaims{}
	for i, fd := range ddlFields {
		if fd.FieldType != "relation" {
			continue
		}
		target, err := ddl.SanitizeIdentifier(fd.RelatedEntity)
		if err != nil {
			return nil, nil, invalid("FIELD_RELATION_TARGET_INVALID",
				"field relation %q có options.relatedEntity không hợp lệ: %v", fd.Name, err)
		}
		if target != name { // self-reference: the table is created right below; nothing to verify.
			if _, err := s.meta.GetEntityByName(ctx, tenantID, target); err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return nil, nil, invalid("FIELD_RELATION_TARGET_NOT_FOUND",
						"entity đích %q của field relation %q không tồn tại", target, fd.Name)
				}
				return nil, nil, fmt.Errorf("services.CreateEntity: resolve relation target: %w", err)
			}
		}
		inverse, err := resolveInverseField(ctx, s.meta, inverseClaims, tenantID, target, name, fd.Name, in.Fields[i].Options)
		if err != nil {
			return nil, nil, err
		}
		// ENG-REL2 #5: the registry row's type mirrors the field's resolved cardinality.
		pendingRels = append(pendingRels, newInverseRelationship(tenantID, target, name, fd.Name, inverse, relationCardinality(in.Fields[i].Options)))
	}

	// ACTIVITY-CORE #3a: verify EACH targetEntity of every polymorphic_relation field exists for
	// THIS tenant BEFORE any DDL (mirrors the relation target-existence loop above). A target that
	// is the entity being created (self-reference) is skipped — its table is materialized moments
	// later by CreateTable. A missing/foreign target → 422 FIELD_POLYMORPHIC_TARGET_NOT_FOUND.
	for i, f := range in.Fields {
		if f.FieldType != "polymorphic_relation" {
			continue
		}
		targets, _ := polymorphicTargetList(in.Fields[i].Options) // non-empty guaranteed by ValidateCreateEntity
		for _, t := range targets {
			target, err := ddl.SanitizeIdentifier(t)
			if err != nil {
				return nil, nil, invalid("FIELD_POLYMORPHIC_TARGET_NOT_FOUND",
					"field polymorphic_relation %q có targetEntity %q không hợp lệ: %v", f.Name, t, err)
			}
			if target == name {
				continue // self-reference: table created below
			}
			if _, err := s.meta.GetEntityByName(ctx, tenantID, target); err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return nil, nil, invalid("FIELD_POLYMORPHIC_TARGET_NOT_FOUND",
						"targetEntity %q của field polymorphic_relation %q không tồn tại", target, f.Name)
				}
				return nil, nil, fmt.Errorf("services.CreateEntity: resolve polymorphic target: %w", err)
			}
		}
	}

	// Validate formula field refs against sibling fields in this entity definition.
	// This is the orchestration-layer check that needs the full field list — pure
	// validateFormulaConfig (validateFieldConfig) only catches syntax errors. Here we
	// confirm every identifier in a formula actually names a numeric sibling field.
	lookupTypes, err := lookupValueTypes(ctx, s.meta, tenantID, name, in.Fields)
	if err != nil {
		return nil, nil, fmt.Errorf("services.CreateEntity: %w", err)
	}
	if err := validateFormulaRefs(in.Fields, lookupTypes); err != nil {
		return nil, nil, err
	}

	// ENG-COMPUTED #2: validate lookup field refs against sibling fields in this entity
	// definition (options.via must name a sibling to-one relation field). Mirrors the formula
	// ref check immediately above.
	if err := validateLookupRefs(in.Fields); err != nil {
		return nil, nil, err
	}

	// ENG-COMPUTED #2 (cont'd): validate every lookup field's sourceField against the TARGET
	// entity's field list (sourceField exists, and is not itself another computed field — the
	// nested-guard validateLookupConfig's doc-comment promises). Needs a DB read (or the
	// in-memory self-field list for a self-referential relation), so it runs after
	// validateLookupRefs confirms `via` is a valid sibling relation field.
	relSpecsByName := make(map[string]FieldInput, len(in.Fields))
	for _, f := range in.Fields {
		if f.FieldType == "relation" {
			relSpecsByName[f.Name] = f
		}
	}
	for _, f := range in.Fields {
		if f.FieldType != "lookup" {
			continue
		}
		if err := validateLookupSourceField(ctx, s.meta, tenantID, relSpecsByName, in.Fields, name, f); err != nil {
			return nil, nil, err
		}
	}

	// ENG-COMPUTED #3: validate every rollup field's cross-entity refs (childEntity exists,
	// viaRelation/relationship actually points back at this entity, aggField is a numeric
	// child field). Self-referential rollup (childEntity == name) resolves against in.Fields
	// (the child/self has not been persisted yet).
	for _, f := range in.Fields {
		if f.FieldType != "rollup" {
			continue
		}
		if err := validateRollupCrossEntity(ctx, s.meta, tenantID, name, in.Fields, f); err != nil {
			return nil, nil, err
		}
	}

	// Note whether the table pre-exists so compensation only drops a table WE create —
	// never one a concurrent creator (or a prior entity sharing the name) depends on.
	existedBefore, err := s.schema.TableExists(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("services.CreateEntity: check table: %w", err)
	}

	// 1) DDL first (auto-commits). 2) metadata in a tx. 3) compensate on metadata failure.
	if err := s.schema.CreateTable(ctx, name, ddlFields); err != nil {
		return nil, nil, fmt.Errorf("services.CreateEntity: create table: %w", err)
	}

	// RECONCILE a SHARED obj_* table (bug3). obj_<name> is keyed by entity name, not by
	// tenant, so when a different tenant already created an entity with this name the table
	// pre-exists and CreateTable's `CREATE TABLE IF NOT EXISTS` is a no-op — every column
	// this creator's unique/relation fields need (and the first creator lacked) would be
	// silently missing: unique would never be enforced, a relation's FK column absent. So
	// for a pre-existing table, ALTER in any unique/relation column that isn't there yet.
	// (For a freshly-created table CreateTable already built every column, so we skip this.)
	//
	// We track exactly the columns THIS call ALTERed (never the ones we skipped because they
	// already existed) so the compensation path can undo only our own reconcile ALTERs on a
	// shared table — never drop a co-tenant's column or the shared table itself.
	var reconciled []reconciledCol
	if existedBefore {
		for _, fd := range ddlFields {
			switch {
			case fd.IsUnique:
				exists, err := s.schema.ColumnExists(ctx, name, fd.Name)
				if err != nil {
					return nil, nil, fmt.Errorf("services.CreateEntity: reconcile check unique column %q: %w", fd.Name, err)
				}
				if !exists {
					// RM-28 Slice B: AddUniqueGencolNonUnique materializes a NON-unique idx_
					// index, so this ALTER can never 1062 on a co-tenant's existing duplicate
					// values (CROSS_TENANT_SHARED_UNIQUE — retired, see field_orchestration.go
					// for the full retirement note). Uniqueness for THIS tenant is enforced by
					// the sys_unique_index pivot, populated separately by the normal record
					// write path (dynrepo/pivot.go) — nothing to backfill here at reconcile time
					// since a freshly-reconciled column has no rows yet under this field name.
					if err := s.schema.AddUniqueGencolNonUnique(ctx, name, fd.Name); err != nil {
						// Roll back any reconcile ALTERs we already made on the shared table first.
						compensateReconcile(ctx, s.schema, name, reconciled)
						return nil, nil, fmt.Errorf("services.CreateEntity: reconcile add unique column %q: %w", fd.Name, err)
					}
					reconciled = append(reconciled, reconciledCol{field: fd.Name, kind: reconciledUnique})
				}
			case fd.FieldType == "relation" || fd.FieldType == "user":
				// A relation's (or USER-FIELD's — reuses the same shape) physical column is
				// `<field>_id` (built by ddl.BuildAddRelationColumn / ddl.BuildAddOneToOneRelationColumn
				// — same column name either way). fd.OneToOne is always false for a `user` field
				// (no cardinality option in this slice), so it always takes the plain branch below.
				exists, err := s.schema.ColumnExists(ctx, name, fd.Name+"_id")
				if err != nil {
					return nil, nil, fmt.Errorf("services.CreateEntity: reconcile check relation/user column %q_id: %w", fd.Name, err)
				}
				if !exists {
					if fd.OneToOne {
						// ENG-REL2 #5: the SECOND tenant to register this shared entity-name
						// with a "1-1" cardinality field reconciles in the CASE-guarded FK
						// column + tenant-scoped UNIQUE key. If existing rows on the shared
						// table already violate 1-1 exclusivity for THIS tenant, the UNIQUE
						// key cannot materialize (1062) — surfaced as 409
						// FIELD_RELATION_CARDINALITY_CONFLICT rather than an opaque 500 — this
						// 1-1 relation index is a SEPARATE physical UNIQUE constraint, NOT
						// pivot-backed, NOT retired by Slice B (unlike the plain-unique
						// reconcile branch above, which now uses AddUniqueGencolNonUnique and
						// can never 1062). This is a known shared-table limitation (RM-28
						// family, §8 ARCHITECTURE / api-conventions §6.7): the physical
						// column SHAPE (guarded+unique vs plain) follows whichever tenant
						// registered the entity name first: a tenant whose reconcile ADD
						// succeeds gets the shape it asked for; the DDL degrades to
						// skip-if-already-present (never re-shapes a column another tenant
						// already relies on), never silently drops/downgrades it.
						if err := s.schema.AddOneToOneRelationColumn(ctx, name, fd.Name); err != nil {
							compensateReconcile(ctx, s.schema, name, reconciled)
							if errors.Is(err, dynrepo.ErrDuplicateUnique) {
								return nil, nil, invalid("FIELD_RELATION_CARDINALITY_CONFLICT",
									"không thể thêm field relation 1-1 %q: dữ liệu hiện có trên bảng dùng chung %q vi phạm ràng buộc 1-1 (nhiều bản ghi con cùng trỏ một bản ghi cha)", fd.Name, "obj_"+name)
							}
							return nil, nil, fmt.Errorf("services.CreateEntity: reconcile add one-to-one relation column %q: %w", fd.Name, err)
						}
						reconciled = append(reconciled, reconciledCol{field: fd.Name, kind: reconciledOneToOneRelation})
					} else {
						if err := s.schema.AddRelationColumn(ctx, name, fd.Name); err != nil {
							compensateReconcile(ctx, s.schema, name, reconciled)
							return nil, nil, fmt.Errorf("services.CreateEntity: reconcile add relation column %q: %w", fd.Name, err)
						}
						reconciled = append(reconciled, reconciledCol{field: fd.Name, kind: reconciledRelation})
					}
				}
			case fd.Polymorphic:
				// ACTIVITY-CORE #3a: a polymorphic_relation field materializes `<field>_type` +
				// `<field>_id` (always added together in ONE ALTER). Use `<field>_type` as the
				// existence sentinel — the pair is added/dropped atomically, so either both are
				// present or both absent on the shared table. Reconcile in when absent.
				exists, err := s.schema.ColumnExists(ctx, name, fd.Name+"_type")
				if err != nil {
					return nil, nil, fmt.Errorf("services.CreateEntity: reconcile check polymorphic column %q_type: %w", fd.Name, err)
				}
				if !exists {
					if err := s.schema.AddPolymorphicColumns(ctx, name, fd.Name); err != nil {
						compensateReconcile(ctx, s.schema, name, reconciled)
						return nil, nil, fmt.Errorf("services.CreateEntity: reconcile add polymorphic columns %q: %w", fd.Name, err)
					}
					reconciled = append(reconciled, reconciledCol{field: fd.Name, kind: reconciledPolymorphic})
				}
			default:
				// D1: non-unique, non-relation field with Indexed==true needs a typed generated
				// column + non-unique index on the shared table. Skip if column already exists
				// (a prior tenant or a prior reconcile added it).
				if !fd.Indexed {
					continue
				}
				exists, err := s.schema.ColumnExists(ctx, name, fd.Name)
				if err != nil {
					return nil, nil, fmt.Errorf("services.CreateEntity: reconcile check indexed column %q: %w", fd.Name, err)
				}
				if !exists {
					if err := s.schema.AddIndexedColumn(ctx, name, fd.Name, fd.FieldType); err != nil {
						compensateReconcile(ctx, s.schema, name, reconciled)
						return nil, nil, fmt.Errorf("services.CreateEntity: reconcile add indexed column %q: %w", fd.Name, err)
					}
					reconciled = append(reconciled, reconciledCol{field: fd.Name, kind: reconciledIndexed, fieldType: fd.FieldType})
				}
			}
		}
	}

	entity := &metadata.EntityDefinition{
		ID:          uuid.NewString(),
		TenantID:    tenantID,
		Name:        name,
		Label:       in.Label,
		DataTable:   "obj_" + name,
		Description: in.Description,
	}
	fields := make([]metadata.FieldDefinition, len(in.Fields))
	for i, f := range in.Fields {
		fname, _ := ddl.SanitizeIdentifier(f.Name)
		opts, err := optionsJSON(f.Options)
		if err != nil {
			return nil, nil, err
		}
		// W5: display position — explicit fieldOrder wins; otherwise number by slice position
		// in gaps of 10 ((i+1)*10). Slice position is the declared order of the payload, so a
		// vertical-pack install gets its pack ordering persisted for free.
		fieldOrder := (i + 1) * 10
		if f.FieldOrder != nil {
			fieldOrder = *f.FieldOrder
		}
		fields[i] = metadata.FieldDefinition{
			ID:       uuid.NewString(),
			EntityID: entity.ID,
			// TenantID denormalized from the server-derived tenant (A2 defense-in-depth);
			// it mirrors the owning entity, never a client-supplied value.
			TenantID:    tenantID,
			Name:        fname,
			Label:       f.Label,
			FieldType:   f.FieldType,
			IsRequired:  f.IsRequired,
			IsUnique:    f.IsUnique,
			Options:     opts,
			FieldOrder:  fieldOrder,
			Description: f.Description,
		}
	}

	if err := s.meta.CreateEntityWithFields(ctx, entity, fields, pendingRels); err != nil {
		// RM-27 TOCTOU: a concurrent CreateEntity for the SAME (tenant, name) won the race
		// and committed first, so our metadata INSERT trips uq_sys_entities_tenant_name
		// (metadata.ErrDuplicateEntity). The physical obj_* table now belongs to the WINNER
		// — even if WE observed !existedBefore at the TableExists check, the winner created
		// (or shares) it. We must therefore NEVER DropTable here: dropping would destroy the
		// winner's just-registered table (the exact bug RM-27 hardens against). Return a clean
		// 409 ENTITY_ALREADY_EXISTS, the same code the EntityExists pre-check returns, so the
		// loser is indistinguishable from a sequential duplicate. Any reconcile ALTERs WE made
		// on a pre-existing shared table are still rolled back (they are ours, not the table).
		if errors.Is(err, metadata.ErrDuplicateEntity) {
			compensateReconcile(ctx, s.schema, name, reconciled)
			return nil, nil, invalid("ENTITY_ALREADY_EXISTS", "entity %q đã tồn tại", name)
		}
		// Finding A: one of this entity's relation fields' auto-registered 1-N
		// sys_relationships row collided with an EXISTING relationship row on (tenant_id,
		// name) — an admin-registered N-N literally named the same, or a 1-N row left behind
		// by an entity recreate after soft-delete (RM-21). Unlike ErrDuplicateEntity above
		// (where a WINNER already owns the table), the WHOLE transaction here (entity + fields
		// + relationship rows) rolled back — THIS entity was never persisted — so compensation
		// is the SAME as any other metadata failure below: drop the table we just created, or
		// (pre-existing shared table) undo only the reconcile ALTERs this call made. Return a
		// clean 409 RELATIONSHIP_ALREADY_EXISTS instead of an opaque 500 — the SAME code the
		// N-N RegisterRelationship duplicate-name path already returns.
		if errors.Is(err, metadata.ErrDuplicateRelationship) {
			if s.safeToDropSharedTable(ctx, name, existedBefore) {
				if dropErr := s.schema.DropTable(ctx, name); dropErr != nil {
					return nil, nil, fmt.Errorf("services.CreateEntity: metadata failed (%w) and table cleanup failed: %v", err, dropErr)
				}
			} else {
				compensateReconcile(ctx, s.schema, name, reconciled)
			}
			relNames := make([]string, len(pendingRels))
			for i, r := range pendingRels {
				relNames[i] = r.Name
			}
			return nil, nil, relationshipAlreadyExists(strings.Join(relNames, ", "))
		}
		// Compensation: metadata failed. Only drop the table if WE created it just now AND nobody
		// else has since claimed the name — see safeToDropSharedTable.
		if s.safeToDropSharedTable(ctx, name, existedBefore) {
			if dropErr := s.schema.DropTable(ctx, name); dropErr != nil {
				return nil, nil, fmt.Errorf("services.CreateEntity: metadata failed (%w) and table cleanup failed: %v", err, dropErr)
			}
			return nil, nil, fmt.Errorf("services.CreateEntity: persist metadata (table rolled back): %w", err)
		}
		// Pre-existing SHARED table: never drop it (a co-tenant owns it), but DO undo the
		// reconcile ALTERs THIS call added so a failed metadata write leaves no orphan
		// generated column / unique-or-FK index behind (symmetry with AddField's compensation).
		compensateReconcile(ctx, s.schema, name, reconciled)
		return nil, nil, fmt.Errorf("services.CreateEntity: persist metadata (pre-existing table kept): %w", err)
	}
	// Defensively invalidate any cached entity-def for this name (e.g. a prior lookup) AFTER the
	// commit so a freshly-created entity is resolved from the DB, not a stale entry (O3).
	s.cache.InvalidateEntityDef(tenantID, entity.Name)
	// RM-19: audit the committed entity create (after-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.EntityMeta, audit.ActionCreate, nil, entity)
	return entity, fields, nil
}

// optionsJSON marshals field options to a JSON string pointer for the sys_fields.options
// JSON column. Empty options return nil so the column is stored as NULL (an empty string
// would be rejected by MySQL as invalid JSON). A marshal failure is surfaced, not swallowed.
func optionsJSON(options map[string]any) (*string, error) {
	if len(options) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("services: marshal field options: %w", err)
	}
	s := string(b)
	return &s, nil
}

// parseOptionsJSON is the inverse of optionsJSON: it decodes a stored sys_fields.options
// JSON-string pointer back into a map. A nil pointer (NULL column) or empty/whitespace JSON
// yields a nil map (no options), mirroring how optionsJSON stores "no options" as NULL. It is
// used by EditField to recover the EFFECTIVE options needed to re-validate a field's default
// at edit time, reusing the record-time validators without duplicating type rules.
func parseOptionsJSON(raw *string) (map[string]any, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil, fmt.Errorf("services: unmarshal field options: %w", err)
	}
	return out, nil
}
