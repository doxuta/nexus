package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nexus/platform/internal/appmeta"
)

// ErrEntityNotFound is returned when no active entity matches a tenant + name.
var ErrEntityNotFound = errors.New("entity not found")

// ErrFieldNotFound is returned when no active field matches an entity + name (the field
// either never existed or was already soft-deleted).
var ErrFieldNotFound = errors.New("field not found")

// ErrDuplicateEntity is returned when persisting an entity row violates the sys_entities
// unique key uq_sys_entities_tenant_name (tenant_id, name). It is the metadata-layer
// serializer behind RM-27: two concurrent CreateEntity calls for the SAME (tenant, name)
// both pass the EntityExists pre-check (TOCTOU) and race to INSERT; the loser's INSERT fails
// with MySQL ER_DUP_ENTRY (1062) on that index. CreateEntity uses errors.Is on this sentinel
// to (a) map to 409 ENTITY_ALREADY_EXISTS and — critically — (b) NOT DropTable, because the
// physical table now belongs to the winner. errors.Is is the service-side contract.
var ErrDuplicateEntity = errors.New("duplicate entity (tenant_id, name)")

// ErrDuplicateField is returned when persisting a field row violates the sys_fields unique
// key uq_sys_fields_tenant_entity_name (tenant_id, entity_id, name) — migration 000012. It
// is the metadata-layer serializer behind RM-25: two concurrent AddField calls that both
// passed the application dup-check race to INSERT, and the loser's INSERT fails with MySQL
// ER_DUP_ENTRY (1062) on that index. The field service maps this to 409 FIELD_ALREADY_EXISTS
// instead of an opaque 500. errors.Is on this sentinel is the service-side contract.
var ErrDuplicateField = errors.New("duplicate field (tenant_id, entity_id, name)")

// mysqlDupEntryErrNo is MySQL's ER_DUP_ENTRY (violated UNIQUE KEY).
const mysqlDupEntryErrNo = 1062

// isDuplicateFieldErr reports whether err is a MySQL ER_DUP_ENTRY (1062) on the sys_fields
// uniqueness index uq_sys_fields_tenant_entity_name. We scope to that specific index name so
// an unrelated 1062 (e.g. a PK clash on a forged id, which should remain a 500/bug signal) is
// NOT mis-mapped to a benign 409 duplicate-field. The index name appears in the MySQL error
// message ("...for key 'sys_fields.uq_sys_fields_tenant_entity_name'").
func isDuplicateFieldErr(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlDupEntryErrNo {
		return false
	}
	return strings.Contains(me.Message, "uq_sys_fields_tenant_entity_name")
}

// isDuplicateEntityErr reports whether err is a MySQL ER_DUP_ENTRY (1062) on the sys_entities
// uniqueness index uq_sys_entities_tenant_name. Scoped to that index name so an unrelated
// 1062 (e.g. a PK clash on a forged entity id) is not mis-mapped to a benign 409. Backs RM-27:
// the concurrent-CreateEntity loser must be told the entity already exists AND must not drop
// the winner's table.
func isDuplicateEntityErr(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlDupEntryErrNo {
		return false
	}
	return strings.Contains(me.Message, "uq_sys_entities_tenant_name")
}

// isDuplicateRelationshipName reports whether err is a MySQL ER_DUP_ENTRY (1062) on the
// sys_relationships uniqueness index uq_sysrel_tenant_name (migration 000025,
// UNIQUE(tenant_id, name)). Scoped to that specific index name so an unrelated 1062 — e.g. a PK
// clash on a forged id, or the OTHER sys_relationships unique index
// uq_sysrel_tenant_join_table (which a 1-N row never trips: JoinTableName is always nil for
// 1-N, see RelationshipDefinition's doc comment) — is NOT mis-mapped to a benign 409 duplicate.
func isDuplicateRelationshipName(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlDupEntryErrNo {
		return false
	}
	return strings.Contains(me.Message, "uq_sysrel_tenant_name")
}

// ErrRelationshipNotFound is returned when no active relationship (of any type — 1-1, 1-N,
// N-N) matches the given lookup key (tenant + name, or tenant + parent + inverse field).
var ErrRelationshipNotFound = errors.New("relationship not found")

// ErrDuplicateRelationship is returned when persisting a sys_relationships row violates the
// UNIQUE(tenant_id, name) index uq_sysrel_tenant_name (migration 000025). Finding A: the
// derived registry name for a 1-N `relation` field's auto-registered row
// (<childEntity>_<fieldName>, see newInverseRelationship in services/relation_registry.go) can
// collide with an EXISTING relationship row of ANY type — an admin-registered N-N literally
// named the same, or a prior 1-N row left behind by an entity recreate after soft-delete
// (RM-21). Before this sentinel, that 1062 was unclassified and wrapped into a generic 500
// (ENTITY_CREATE_FAILED / FIELD_ADD_FAILED). The service layer uses errors.Is on this sentinel
// to map it to 409 RELATIONSHIP_ALREADY_EXISTS — the SAME code (and, via statusForCode, the
// SAME HTTP status) the N-N RegisterRelationship duplicate-name path already returns
// (relation_service.go), so a name collision looks identical to the client no matter which
// writer triggered it. The transaction (CreateEntityWithFields / CreateFieldWithRelationship)
// rolls back exactly as it already did before this change — this sentinel only changes how the
// error is CLASSIFIED, never the compensation/rollback behaviour.
var ErrDuplicateRelationship = errors.New("duplicate relationship (tenant_id, name)")

// Repository provides access to the metadata registry tables (sys_*).
//
// System/metadata tables use GORM (fixed schema); dynamic obj_* entity data is
// queried with sqlx + parameterized SQL by the (future) dynamic-record repository.
type Repository struct {
	db *gorm.DB
}

// NewRepository wires a metadata repository over an existing GORM connection.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// ListEntities returns active (non-deleted) entity definitions for a single tenant,
// ordered by name. Multi-tenancy: the query is always scoped by tenant_id — there is
// no code path that returns cross-tenant rows. Soft-deleted rows are excluded by GORM
// via the gorm.DeletedAt field.
func (r *Repository) ListEntities(tenantID string) ([]EntityDefinition, error) {
	var entities []EntityDefinition
	if err := r.db.
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Find(&entities).Error; err != nil {
		return nil, fmt.Errorf("metadata.ListEntities: %w", err)
	}
	return entities, nil
}

// GetEntityNameByID returns the entity name for a given entity id, scoped by tenant_id.
// Returns ErrEntityNotFound when no active entity exists. Used by EditField when it needs
// to issue DDL against the entity's obj_* table — the field row carries only EntityID, not
// the entity name, so a separate lookup is required.
func (r *Repository) GetEntityNameByID(ctx context.Context, tenantID, entityID string) (string, error) {
	var e EntityDefinition
	err := r.db.WithContext(ctx).
		Select("id, tenant_id, name").
		Where("id = ? AND tenant_id = ?", entityID, tenantID).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrEntityNotFound
	}
	if err != nil {
		return "", fmt.Errorf("metadata.GetEntityNameByID: %w", err)
	}
	return e.Name, nil
}

// GetEntityByName returns the active entity definition for a tenant + name, or
// ErrEntityNotFound. Used by the dynamic record dispatcher to resolve :entity.
func (r *Repository) GetEntityByName(ctx context.Context, tenantID, name string) (*EntityDefinition, error) {
	var e EntityDefinition
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrEntityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metadata.GetEntityByName: %w", err)
	}
	return &e, nil
}

// ListFields returns the active field definitions of an entity, ordered by field_order
// (W5 admin-controlled position, tie-break name for stability — legacy rows backfilled by
// migration 000033 keep a deterministic order even on equal field_order).
// tenantID is required as a defense-in-depth predicate (RM-18): after migration 000010
// sys_fields.tenant_id is NOT NULL, so this predicate is enforced at DB level on top of
// the entity_id filter. This ensures no query path can return a cross-tenant field row
// even if a caller passes an entityID that somehow resolves to another tenant.
func (r *Repository) ListFields(ctx context.Context, tenantID, entityID string) ([]FieldDefinition, error) {
	var fields []FieldDefinition
	if err := r.db.WithContext(ctx).
		Where("entity_id = ? AND tenant_id = ?", entityID, tenantID).
		Order("field_order ASC, name ASC").
		Find(&fields).Error; err != nil {
		return nil, fmt.Errorf("metadata.ListFields: %w", err)
	}
	return fields, nil
}

// ListRollupsAggregating returns the active rollup fields of a tenant ("parent.field", sorted)
// whose options aggregate <childEntity>.<aggField>. Queue #234: EditField refuses to change the
// resultType of a formula such a rollup reads. Both tables are pinned to tenant_id; the rollup's
// childEntity/aggField are bound parameters.
func (r *Repository) ListRollupsAggregating(ctx context.Context, tenantID, childEntity, aggField string) ([]string, error) {
	var out []string
	err := r.db.WithContext(ctx).Raw(
		"SELECT CONCAT(e.name, '.', f.name) FROM sys_fields f "+
			"JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id AND e.deleted_at IS NULL "+
			"WHERE f.tenant_id = ? AND f.deleted_at IS NULL AND f.field_type = 'rollup' "+
			"AND JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.childEntity')) = ? "+
			"AND JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.aggField')) = ? "+
			"ORDER BY e.name, f.name",
		tenantID, childEntity, aggField).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("metadata.ListRollupsAggregating: %w", err)
	}
	return out, nil
}

// StoredRollupField is one stored roll-up (options.stored=true) that aggregates a given child
// entity: the parent entity that owns it, its name, and its raw options JSON.
type StoredRollupField struct {
	ParentEntity string  `gorm:"column:parent_entity"`
	Name         string  `gorm:"column:name"`
	Label        string  `gorm:"column:label"`
	Options      *string `gorm:"column:options"`
}

// ListStoredRollupsOfChild returns the active roll-up fields of a tenant that keep a stored value
// (options.stored = true) over <childEntity>. specs/083: every write on the child entity asks this
// to know which parent totals to refresh. Both tables are pinned to tenant_id; childEntity is a
// bound parameter.
//
// ponytail: one small indexed query per child write; a TTL cache keyed by (tenant, child) is the
// upgrade path if write-heavy imports show it.
func (r *Repository) ListStoredRollupsOfChild(ctx context.Context, tenantID, childEntity string) ([]StoredRollupField, error) {
	var out []StoredRollupField
	err := r.db.WithContext(ctx).Raw(
		"SELECT e.name AS parent_entity, f.name AS name, f.label AS label, f.options AS options FROM sys_fields f "+
			"JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id AND e.deleted_at IS NULL "+
			"WHERE f.tenant_id = ? AND f.deleted_at IS NULL AND f.field_type = 'rollup' "+
			"AND JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.childEntity')) = ? "+
			"AND JSON_EXTRACT(f.options, '$.stored') = TRUE "+
			"ORDER BY e.name, f.name",
		tenantID, childEntity).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("metadata.ListStoredRollupsOfChild: %w", err)
	}
	return out, nil
}

// ListFieldsIncludingDeleted returns ALL field definitions of an entity — including
// soft-deleted rows (deleted_at IS NOT NULL) — via GORM's Unscoped(). It exists for the
// add-field dup-check (spec §7.2 B-1): ListFields hides soft-deleted rows, so a name that
// was added then soft-deleted would slip past a duplicate check and (for a unique field)
// drive an ALTER ADD COLUMN onto an already-existing physical column → 500, or (for an
// ordinary field) create a second sys_fields row. The dup-check must see the soft-deleted
// row too and reject the re-add. tenantID is included as defense-in-depth (RM-18) matching
// ListFields: the entityID is resolved via a tenant-scoped GetEntityByName before this is
// called, and the additional tenant_id predicate ensures no cross-tenant row is returned
// even when Unscoped() bypasses the soft-delete filter.
func (r *Repository) ListFieldsIncludingDeleted(ctx context.Context, tenantID, entityID string) ([]FieldDefinition, error) {
	var fields []FieldDefinition
	if err := r.db.WithContext(ctx).
		Unscoped().
		Where("entity_id = ? AND tenant_id = ?", entityID, tenantID).
		Order("name ASC").
		Find(&fields).Error; err != nil {
		return nil, fmt.Errorf("metadata.ListFieldsIncludingDeleted: %w", err)
	}
	return fields, nil
}

// EntityExists reports whether an active entity with this name already exists for the
// tenant. Used to reject duplicate creation before any DDL runs.
func (r *Repository) EntityExists(ctx context.Context, tenantID, name string) (bool, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&EntityDefinition{}).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("metadata.EntityExists: %w", err)
	}
	return n > 0, nil
}

// NameClaimedByAnyTenant reports whether ANY tenant currently registers an active entity with this
// name. Deliberately UNSCOPED — the one query in this package that is — because obj_<name> tables
// are keyed by entity name and shared across tenants (see dynrepo: "a single obj_students table is
// shared by every tenant that registers a students entity").
//
// It exists so the CreateEntity compensation path can ask an OWNERSHIP question ("is this table
// somebody's?") instead of a time-of-check one ("did it exist when I looked, several statements
// ago?"). The latter is a TOCTOU: if another tenant creates the same-named entity inside that
// window, the losing request's rollback would DROP the winner's live table and its rows.
func (r *Repository) NameClaimedByAnyTenant(ctx context.Context, name string) (bool, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&EntityDefinition{}).
		Where("name = ?", name).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("metadata.NameClaimedByAnyTenant: %w", err)
	}
	return n > 0, nil
}

// CreateEntityWithFields persists an entity definition, all its fields, AND (ENG-REL2 #4)
// any 1-N relationship registry rows the entity's `relation` fields need — all in ONE
// transaction: everything lands or nothing does. rels is nil/empty when the entity declares
// no relation fields (the overwhelming common case); pre-000028 callers pass nil, unchanged
// behaviour. The caller (entity service) is responsible for the physical obj_* table and for
// compensating (DROP) if this fails.
func (r *Repository) CreateEntityWithFields(ctx context.Context, e *EntityDefinition, fields []FieldDefinition, rels []RelationshipDefinition) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(e).Error; err != nil {
			// RM-27 serializer: a concurrent CreateEntity won the race and already persisted
			// this (tenant_id, name), so our INSERT trips uq_sys_entities_tenant_name. Surface
			// the typed sentinel so the service maps it to 409 AND skips the table DROP (the
			// physical table now belongs to the winner).
			if isDuplicateEntityErr(err) {
				return fmt.Errorf("metadata.CreateEntityWithFields: entity: %w", ErrDuplicateEntity)
			}
			return fmt.Errorf("metadata.CreateEntityWithFields: entity: %w", err)
		}
		for i := range fields {
			if err := tx.Create(&fields[i]).Error; err != nil {
				if isDuplicateFieldErr(err) {
					return fmt.Errorf("metadata.CreateEntityWithFields: field %q: %w", fields[i].Name, ErrDuplicateField)
				}
				return fmt.Errorf("metadata.CreateEntityWithFields: field %q: %w", fields[i].Name, err)
			}
		}
		// ENG-REL2 #4: persist the 1-N registry row(s) for this entity's relation field(s) in
		// the SAME transaction as the entity+fields above, so a registry-insert failure rolls
		// back the whole entity create (no half-registered relation: a field with no matching
		// sys_relationships row, or vice versa).
		for i := range rels {
			if err := tx.Create(&rels[i]).Error; err != nil {
				// Finding A: the derived registry name collided with an existing
				// sys_relationships row (see ErrDuplicateRelationship's doc comment). Surface
				// the typed sentinel so the service maps it to 409 instead of an opaque 500.
				if isDuplicateRelationshipName(err) {
					return fmt.Errorf("metadata.CreateEntityWithFields: relationship %q: %w", rels[i].Name, ErrDuplicateRelationship)
				}
				return fmt.Errorf("metadata.CreateEntityWithFields: relationship %q: %w", rels[i].Name, err)
			}
		}
		return nil
	})
}

// CreateField persists a single field definition into sys_fields. Used by the field
// service to add a field to an already-registered entity. The entity_id is supplied by
// the service after a tenant-scoped GetEntityByName, so the field inherits that scoping.
func (r *Repository) CreateField(ctx context.Context, f *FieldDefinition) error {
	if err := r.db.WithContext(ctx).Create(f).Error; err != nil {
		// RM-25 last-resort serializer: a concurrent AddField that won the race already
		// persisted this (tenant_id, entity_id, name), so our INSERT trips the 000012
		// unique key. Surface the typed sentinel so the service maps it to 409.
		if isDuplicateFieldErr(err) {
			return fmt.Errorf("metadata.CreateField: %w", ErrDuplicateField)
		}
		return fmt.Errorf("metadata.CreateField: %w", err)
	}
	return nil
}

// CreateFieldWithRelationship persists a single field definition AND (ENG-REL2 #4) its
// companion 1-N sys_relationships registry row in ONE transaction: either both land or
// neither does. Used ONLY by AddField for a `relation`-type field; every other field type
// continues to use the plain CreateField (rel is always nil there). Atomicity matters here
// because a field with no matching registry row (or a registry row with no field) is a
// silently drifted, half-registered relation.
func (r *Repository) CreateFieldWithRelationship(ctx context.Context, f *FieldDefinition, rel *RelationshipDefinition) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(f).Error; err != nil {
			if isDuplicateFieldErr(err) {
				return fmt.Errorf("metadata.CreateFieldWithRelationship: field: %w", ErrDuplicateField)
			}
			return fmt.Errorf("metadata.CreateFieldWithRelationship: field: %w", err)
		}
		if rel != nil {
			if err := tx.Create(rel).Error; err != nil {
				// Finding A: the derived registry name (<childEntity>_<fieldName>) collided
				// with an existing sys_relationships row — an admin-registered N-N literally
				// named the same, or a 1-N row left behind by an entity recreate after
				// soft-delete (RM-21). Surface the typed sentinel so AddField maps it to 409
				// RELATIONSHIP_ALREADY_EXISTS instead of an opaque 500.
				if isDuplicateRelationshipName(err) {
					return fmt.Errorf("metadata.CreateFieldWithRelationship: relationship %q: %w", rel.Name, ErrDuplicateRelationship)
				}
				return fmt.Errorf("metadata.CreateFieldWithRelationship: relationship: %w", err)
			}
		}
		return nil
	})
}

// GetFieldByID returns the active field definition for a field id, scoped to the tenant
// that owns the field's parent entity, or ErrFieldNotFound. Tenant isolation is enforced
// on TWO independent levels (RM-18 defense-in-depth):
//
//  1. JOIN sys_entities filter: sys_entities.tenant_id = tenantID — a field id belonging
//     to another tenant resolves to not-found via the JOIN, never to a cross-tenant row.
//  2. Direct column pin: sys_fields.tenant_id = tenantID — after migration 000010 this
//     column is NOT NULL, so the predicate is always evaluated and never matches a NULL.
//     A field whose denormalized tenant_id differs from its entity's tenant_id (data
//     corruption) will also not match, providing an additional cross-tenant write guard.
//
// Soft-deleted fields/entities are excluded via GORM's DeletedAt handling on both models.
func (r *Repository) GetFieldByID(ctx context.Context, tenantID, fieldID string) (*FieldDefinition, error) {
	var f FieldDefinition
	err := r.db.WithContext(ctx).
		Model(&FieldDefinition{}).
		Joins("JOIN sys_entities ON sys_entities.id = sys_fields.entity_id AND sys_entities.deleted_at IS NULL").
		Where("sys_fields.id = ? AND sys_entities.tenant_id = ?", fieldID, tenantID).
		Where("sys_fields.tenant_id = ?", tenantID).
		First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrFieldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metadata.GetFieldByID: %w", err)
	}
	return &f, nil
}

// UpdateFieldMutable persists ONLY the editable attributes of a field — label,
// isRequired, defaultValue (default_val), options, field_order and description (W5: order
// and description are mutable, unlike field_type/name). field_type, name, entity_id and
// uniqueness are deliberately excluded so an edit can never rewrite immutable schema-bound
// columns. The caller must have resolved the field via GetFieldByID (tenant-scoped) first;
// the WHERE clause additionally re-pins BOTH the id AND the tenant_id (defense-in-depth):
// a stale/forged id that belongs to another tenant matches 0 rows → ErrFieldNotFound, so a
// cross-tenant write is impossible even if it reaches this method directly.
func (r *Repository) UpdateFieldMutable(ctx context.Context, tenantID string, f *FieldDefinition) error {
	res := r.db.WithContext(ctx).
		Model(&FieldDefinition{}).
		Where("id = ? AND tenant_id = ?", f.ID, tenantID).
		Select("label", "is_required", "default_val", "options", "field_order", "description").
		Updates(map[string]any{
			"label":       f.Label,
			"is_required": f.IsRequired,
			"default_val": f.DefaultVal,
			"options":     f.Options,
			"field_order": f.FieldOrder,
			"description": f.Description,
		})
	if res.Error != nil {
		return fmt.Errorf("metadata.UpdateFieldMutable: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrFieldNotFound
	}
	return nil
}

// ReorderFields renumbers the given fields of ONE entity in a single transaction: the i-th
// id in orderedIDs gets field_order = (i+1)*10 (gaps of 10, mirroring the entity-create
// numbering). Membership is verified INSIDE the transaction with SELECT … FOR UPDATE,
// re-pinning tenant_id AND entity_id (defense-in-depth on top of the service-layer check):
// a forged/cross-tenant/cross-entity/soft-deleted id fails the count and the WHOLE
// transaction rolls back with ErrFieldNotFound — all-or-nothing. The row lock also blocks a
// concurrent soft-delete (an UPDATE of deleted_at) until commit, closing the check-then-write
// race. Existence is deliberately NOT inferred from UPDATE RowsAffected: MySQL reports 0
// affected rows for a value-identical update (e.g. a field keeping its position in a
// re-submitted order within the same updated_at second), which is a no-op, not a missing row.
func (r *Repository) ReorderFields(ctx context.Context, tenantID, entityID string, orderedIDs []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedIDs []string
		if err := tx.Model(&FieldDefinition{}).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ? AND tenant_id = ? AND entity_id = ?", orderedIDs, tenantID, entityID).
			Pluck("id", &lockedIDs).Error; err != nil {
			return fmt.Errorf("metadata.ReorderFields: lock fields: %w", err)
		}
		if len(lockedIDs) != len(orderedIDs) {
			return fmt.Errorf("metadata.ReorderFields: %d/%d field khớp (tenant/entity/soft-delete mismatch): %w",
				len(lockedIDs), len(orderedIDs), ErrFieldNotFound)
		}
		for i, id := range orderedIDs {
			if err := tx.Model(&FieldDefinition{}).
				Where("id = ? AND tenant_id = ? AND entity_id = ?", id, tenantID, entityID).
				Update("field_order", (i+1)*10).Error; err != nil {
				return fmt.Errorf("metadata.ReorderFields: field %q: %w", id, err)
			}
		}
		return nil
	})
}

// SoftDeleteField soft-deletes a single field (sets sys_fields.deleted_at). It performs
// NO DDL: the underlying obj_* column / data is preserved per §4.4 (reversible, safe).
// The caller must resolve the field via GetFieldByID (tenant-scoped) first; the WHERE
// clause re-pins BOTH the id AND the tenant_id (defense-in-depth) so a stale/forged id
// from another tenant deletes 0 rows → ErrFieldNotFound. GORM's Delete on a model with a
// DeletedAt field issues an UPDATE ... SET deleted_at = NOW(), not a hard DELETE.
//
// Queue #322: the field's sys_field_acl rows (keyed by entity_name + field_name, not by id) go in
// the SAME transaction. Left behind, a later field created under the same name — Studio re-create,
// vertical-pack reinstall — inherited the old hide/lock without passing the PutGrants / EditField
// dead-end guards. The row set is resolved from the (soft-deleted) field row itself, which raw SQL
// still sees, so no second lookup and no window where the field is gone but its restrictions stay.
func (r *Repository) SoftDeleteField(ctx context.Context, tenantID, fieldID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND tenant_id = ?", fieldID, tenantID).Delete(&FieldDefinition{})
		if res.Error != nil {
			return fmt.Errorf("metadata.SoftDeleteField: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrFieldNotFound
		}
		if err := tx.Exec(`DELETE a FROM sys_field_acl a
			JOIN sys_fields f ON f.id = ? AND f.tenant_id = ?
			JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = ?
			WHERE a.tenant_id = ? AND a.entity_name = e.name AND a.field_name = f.name`,
			fieldID, tenantID, tenantID, tenantID).Error; err != nil {
			return fmt.Errorf("metadata.SoftDeleteField: drop field ACL rows: %w", err)
		}
		return nil
	})
}

// UpdateEntityMutable updates the mutable attributes of an entity (label, description,
// options). The entity name and table_name are intentionally excluded — they are
// schema-bound and cannot change without a DDL rename (not supported). The caller must
// have resolved the entity via GetEntityByName (tenant-scoped) first; the WHERE clause
// re-pins BOTH the id AND the tenant_id (defense-in-depth IDOR guard) so a forged/stale
// id from another tenant matches 0 rows → ErrEntityNotFound.
func (r *Repository) UpdateEntityMutable(ctx context.Context, tenantID string, e *EntityDefinition) error {
	res := r.db.WithContext(ctx).
		Model(&EntityDefinition{}).
		Where("id = ? AND tenant_id = ?", e.ID, tenantID).
		Select("label", "description", "options").
		Updates(map[string]any{
			"label":       e.Label,
			"description": e.Description,
			"options":     e.Options,
		})
	if res.Error != nil {
		return fmt.Errorf("metadata.UpdateEntityMutable: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrEntityNotFound
	}
	return nil
}

// CreateRelationship persists a new relationship definition into sys_relationships.
// The tenant_id is supplied by the service after a tenant-scoped lookup of the parent
// entity — never read from client input. The caller is responsible for DDL compensation
// if the write fails after the join table was already created.
func (r *Repository) CreateRelationship(ctx context.Context, rel *RelationshipDefinition) error {
	if err := r.db.WithContext(ctx).Create(rel).Error; err != nil {
		return fmt.Errorf("metadata.CreateRelationship: %w", err)
	}
	return nil
}

// tombstoneSuffix derives the short "__deleted_" suffix source from a row id: the first
// 8 chars of the UUID are sufficient uniqueness to free a unique-index slot repeatedly
// (the same name deleted multiple times gets a different id, hence a different suffix).
func tombstoneSuffix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// tombstoneName returns "<name>__deleted_<suffix>", truncating the original name portion
// (never the suffix) so the result fits the column's maxLen (a BYTE budget — MySQL VARCHAR
// lengths are in characters, but this codebase's identifiers are ASCII except for
// admin-supplied `name`/`join_table_name` values, which can carry multibyte UTF-8, e.g.
// Vietnamese). Used by every soft-delete that must free a unique-index slot (sys_entities
// name, sys_relationships name / join_table_name) — a bare soft-delete would keep the slot
// occupied because MySQL unique indexes span soft-deleted rows.
//
// W2 review fix (robustness): the truncation is RUNE-SAFE — it walks whole runes and stops
// before the first one that would not fit in the remaining byte budget, never slicing a
// multi-byte UTF-8 sequence in half. A byte-index slice (name[:keep]) can land mid-rune for
// a long multibyte name (e.g. a 50+-char Vietnamese relationship name > 150 bytes),
// producing invalid UTF-8 that MySQL rejects with error 1366 ("Incorrect string value") —
// turning a routine soft-delete into a 500 and leaving the row stuck (undeletable via the
// API, since the very tombstone step that would free it fails).
func tombstoneName(name string, maxLen int, suffix string) string {
	tomb := name + "__deleted_" + suffix
	if len(tomb) > maxLen {
		keepBytes := maxLen - len("__deleted_"+suffix)
		if keepBytes < 1 {
			keepBytes = 1
		}
		truncated := name
		if len(truncated) > keepBytes {
			b := 0
			for _, r := range name {
				rl := utf8.RuneLen(r)
				if rl < 0 {
					rl = 1 // defensive: utf8.RuneError on invalid input, treat as 1 byte
				}
				if b+rl > keepBytes {
					break
				}
				b += rl
			}
			truncated = name[:b]
		}
		tomb = truncated + "__deleted_" + suffix
	}
	return tomb
}

// SoftDeleteEntity soft-deletes a single entity (sets sys_entities.deleted_at). The
// physical obj_* table is preserved (per spec §RM-03 — soft-delete only, reversible).
// To allow recreating an entity with the same name after deletion, the stored `name` is
// tombstone-renamed to "<name>__deleted_<id_prefix>" before the soft-delete so the unique
// index (tenant_id, name) does not block a future entity with the original name. The
// physical table_name is NOT changed (DDL table rename is not supported). The caller must
// have resolved the entity via GetEntityByName (tenant-scoped) first; the WHERE clause
// re-pins BOTH the id AND the tenant_id (IDOR guard).
func (r *Repository) SoftDeleteEntity(ctx context.Context, tenantID, entityID string) error {
	// Derive a short suffix from the entity ID to avoid collisions when the same name
	// is deleted multiple times. First 8 chars of the UUID is sufficient uniqueness.
	suffix := tombstoneSuffix(entityID)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Tombstone-rename the `name` column so the unique index frees the original name.
		//    varchar(100) truncation guard: the original name + suffix must fit in 100 chars.
		var existing EntityDefinition
		if err := tx.Where("id = ? AND tenant_id = ?", entityID, tenantID).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrEntityNotFound
			}
			return fmt.Errorf("load entity: %w", err)
		}
		tombstone := tombstoneName(existing.Name, 100, suffix)
		if err := tx.Model(&EntityDefinition{}).
			Where("id = ? AND tenant_id = ?", entityID, tenantID).
			Update("name", tombstone).Error; err != nil {
			return fmt.Errorf("tombstone rename: %w", err)
		}
		// 2. Soft-delete (GORM sets deleted_at = NOW()).
		res := tx.Where("id = ? AND tenant_id = ?", entityID, tenantID).Delete(&EntityDefinition{})
		if res.Error != nil {
			return fmt.Errorf("soft delete: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrEntityNotFound
		}
		// 3. ENG-REL2 #4 (security audit MED), broadened by ENG-REL2 #5 and again by
		//    REL-RM21-NN: tombstone + soft-delete EVERY registry row that references this
		//    entity on EITHER side (parent_entity_name OR child_entity_name = the ORIGINAL
		//    name resolved above, ANY relationship_type — 1-1, 1-N, N-N), in the SAME
		//    transaction as the entity tombstone. Two failure families this closes:
		//      * Unique-slot brick: each row keeps its (tenant_id, name) slot — and, for
		//        N-N, its (tenant_id, join_table_name) slot — occupied forever
		//        (uq_sysrel_tenant_name / uq_sysrel_tenant_join_table, migration 000025,
		//        span soft-deleted rows), so recreating the entity + the same relation
		//        field / N-N relationship (vertical-pack rollback → retry, or a plain
		//        admin delete → recreate) would be permanently bricked with 409
		//        RELATIONSHIP_ALREADY_EXISTS and no in-product recovery. Both
		//        unique-indexed columns are tombstone-renamed (mirroring
		//        SoftDeleteRelationshipByID) because a bare soft-delete would NOT free
		//        the slots.
		//      * Phantom edges (REL-RM21-NN): parent-side 1-N/1-1 rows and ALL N-N rows
		//        used to be left active, so they kept surfacing through
		//        ListAllRelationships / ListRelationshipsForEntity →
		//        GET /metadata/relationships → ghost edges in the Studio ERD and the
		//        Related tab pointing at a deleted entity.
		//    The physical rel_* join table is deliberately NOT touched (shared across
		//    tenants; grace-window reuse — see SoftDeleteRelationshipByID's doc comment).
		//    STILL deferred (RM-21 family, by design): when this entity was the PARENT of
		//    a 1-N/1-1 row, the owning relation FIELD on the OTHER entity's sys_fields is
		//    NOT deleted here (sys_fields orphan cleanup). It is harmless-but-visible:
		//    writing a value into it now fails record validation (relation target not
		//    found), and an admin can delete the field.
		var ownedRels []RelationshipDefinition
		if err := tx.Where("tenant_id = ? AND (parent_entity_name = ? OR child_entity_name = ?)",
			tenantID, existing.Name, existing.Name).Find(&ownedRels).Error; err != nil {
			return fmt.Errorf("list relationships involving entity: %w", err)
		}
		for i := range ownedRels {
			// Per-row suffix from the RELATIONSHIP id (not the entity id), mirroring
			// SoftDeleteRelationshipByID/ByChildFK: two long names swept in the same
			// delete can never truncate-collide onto the same tombstone (distinct row
			// ids → distinct suffixes), and each row's slot is freed deterministically.
			relSuffix := tombstoneSuffix(ownedRels[i].ID)
			// name is VARCHAR(150); join_table_name is VARCHAR(100).
			updates := map[string]any{"name": tombstoneName(ownedRels[i].Name, 150, relSuffix)}
			if ownedRels[i].JoinTableName != nil && *ownedRels[i].JoinTableName != "" {
				updates["join_table_name"] = tombstoneName(*ownedRels[i].JoinTableName, 100, relSuffix)
			}
			if err := tx.Model(&RelationshipDefinition{}).
				Where("id = ? AND tenant_id = ?", ownedRels[i].ID, tenantID).
				Updates(updates).Error; err != nil {
				return fmt.Errorf("tombstone relationship %q: %w", ownedRels[i].Name, err)
			}
			if err := tx.Where("id = ? AND tenant_id = ?", ownedRels[i].ID, tenantID).
				Delete(&RelationshipDefinition{}).Error; err != nil {
				return fmt.Errorf("soft delete relationship %q: %w", ownedRels[i].Name, err)
			}
		}
		// 4. App-container cascade (../specs/010-app-container/spec.md INV-4): remove every sys_app_nav_items
		//    row referencing this entity, in the SAME transaction as the tombstone — otherwise
		//    the app sidebar would keep a dead tab pointing at the deleted entity. The shared
		//    purge (appmeta.PurgeNavForRef) is the ONE cascade implementation for all delete
		//    paths; tx.Statement.ConnPool is the transaction's connection, so the raw DELETE
		//    commits/rolls back with the rest of this function.
		if err := appmeta.PurgeNavForRef(ctx, tx.Statement.ConnPool, tenantID, appmeta.ItemTypeEntity, entityID); err != nil {
			return fmt.Errorf("purge app nav refs: %w", err)
		}
		// 5. Field-ACL cascade (queue #322): sys_field_acl rows are keyed by the entity NAME, which
		//    the tombstone rename just freed — an entity re-created or re-installed under that name
		//    (Studio, vertical pack) would adopt the old hide/lock rows without passing the
		//    PutGrants / EditField dead-end guards. Same transaction, ORIGINAL name, tenant-pinned.
		if err := tx.Exec(`DELETE FROM sys_field_acl WHERE tenant_id = ? AND entity_name = ?`,
			tenantID, existing.Name).Error; err != nil {
			return fmt.Errorf("drop field ACL rows: %w", err)
		}
		return nil
	})
}

// GetRelationshipByName returns the active relationship definition (of ANY type — "1-1",
// "1-N", or "N-N") for a tenant + name. Tenant isolation is enforced by filtering on
// tenant_id (added in migration 000004). Returns ErrRelationshipNotFound when none exists.
//
// BLOCKER FIX (ENG-REL2 #4): this used to hardcode `relationship_type = 'N-N'` because
// sys_relationships was, until migration 000028, written ONLY by N-N registration. 000028
// makes this the FIRST writer of 1-N rows (the inverse-field registry); the N-N predicate
// would have silently filtered every 1-N row out of this lookup. Callers that must resolve
// N-N specifically (RelationService.Link/Unlink/ListLinked, rollup's options.relationship)
// now check rel.RelationshipType == "N-N" themselves after the call.
func (r *Repository) GetRelationshipByName(ctx context.Context, tenantID, name string) (*RelationshipDefinition, error) {
	var rel RelationshipDefinition
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		First(&rel).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRelationshipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metadata.GetRelationshipByName: %w", err)
	}
	return &rel, nil
}

// GetRelationshipByInverseField returns the active relationship definition (of any type)
// whose parent_entity_name + inverse_field match, scoped to the tenant. Used by the
// ENG-REL2 #4 inverse-field naming rule to detect a collision before registering a new 1-N
// relationship row: two relation fields (on the same or different child entities) must not
// both claim the same inverse_field on the same parent entity. Returns
// ErrRelationshipNotFound when no such row exists (not an error case — "name available").
func (r *Repository) GetRelationshipByInverseField(ctx context.Context, tenantID, parentEntityName, inverseField string) (*RelationshipDefinition, error) {
	var rel RelationshipDefinition
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND parent_entity_name = ? AND inverse_field = ?", tenantID, parentEntityName, inverseField).
		First(&rel).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRelationshipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metadata.GetRelationshipByInverseField: %w", err)
	}
	return &rel, nil
}

// SoftDeleteRelationshipByChildFK soft-deletes the 1-N OR 1-1 sys_relationships row that
// backs a relation field's FK column, identified by (tenant, child_entity_name,
// child_fk_column, relationship_type IN ('1-N','1-1')). Called when the owning relation
// field is soft-deleted (ENG-REL2 #4; broadened to 1-1 by ENG-REL2 #5) so the registry row
// does not outlive the field it describes. A missing row (a pre-000028 relation field
// predates the registry, or the row was already cleaned up) is a no-op, NOT an error — this
// is a go-forward-only cleanup.
//
// W2 review fix (MED): the row's `name` (e.g. "contacts_account") is tombstone-renamed
// BEFORE the soft-delete, mirroring SoftDeleteEntity's relationship cleanup and
// SoftDeleteRelationshipByID — a bare soft-delete would leave the (tenant_id, name) slot
// permanently occupied (uq_sysrel_tenant_name, migration 000025 spans soft-deleted rows).
// Brick scenario this closes: DeleteField soft-deletes a `relation` field and (via this
// method) its registry row WITHOUT tombstoning -> the child entity is later deleted
// (SoftDeleteEntity's own tombstone loop only lists still-LIVE owned rows, so it skips one
// this method already soft-deleted) -> recreating the child entity + the same-named
// relation field re-registers the SAME derived name -> INSERT trips the unique index ->
// permanent 409 RELATIONSHIP_ALREADY_EXISTS with no in-product recovery. join_table_name is
// always nil for 1-N/1-1 rows (see RelationshipDefinition's doc comment) so only `name`
// needs tombstoning here; the guard below is defensive symmetry with SoftDeleteRelationshipByID
// in case that invariant is ever relaxed.
func (r *Repository) SoftDeleteRelationshipByChildFK(ctx context.Context, tenantID, childEntityName, childFKColumn string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []RelationshipDefinition
		if err := tx.Where("tenant_id = ? AND child_entity_name = ? AND child_fk_column = ? AND relationship_type IN ('1-N','1-1')",
			tenantID, childEntityName, childFKColumn).Find(&rows).Error; err != nil {
			return fmt.Errorf("metadata.SoftDeleteRelationshipByChildFK: load: %w", err)
		}
		for i := range rows {
			suffix := tombstoneSuffix(rows[i].ID)
			updates := map[string]any{"name": tombstoneName(rows[i].Name, 150, suffix)}
			if rows[i].JoinTableName != nil && *rows[i].JoinTableName != "" {
				updates["join_table_name"] = tombstoneName(*rows[i].JoinTableName, 100, suffix)
			}
			if err := tx.Model(&RelationshipDefinition{}).
				Where("id = ? AND tenant_id = ?", rows[i].ID, tenantID).
				Updates(updates).Error; err != nil {
				return fmt.Errorf("metadata.SoftDeleteRelationshipByChildFK: tombstone %q: %w", rows[i].Name, err)
			}
			if err := tx.Where("id = ? AND tenant_id = ?", rows[i].ID, tenantID).
				Delete(&RelationshipDefinition{}).Error; err != nil {
				return fmt.Errorf("metadata.SoftDeleteRelationshipByChildFK: soft delete %q: %w", rows[i].Name, err)
			}
		}
		return nil
	})
}

// GetRelationshipByID returns the active relationship definition (of ANY type — "1-1",
// "1-N", "N-N") for a tenant + row id, or ErrRelationshipNotFound. Tenant isolation is
// enforced by pinning tenant_id in the WHERE clause: a relationship id owned by another
// tenant resolves to not-found, never to a cross-tenant row (IDOR guard, mirrors
// GetFieldByID). Soft-deleted rows are excluded via GORM's DeletedAt handling, so a
// re-delete of an already-deleted relationship also resolves to not-found (→ 404).
// Backs the W2 relationship lifecycle endpoints (impact / patch / delete by id).
func (r *Repository) GetRelationshipByID(ctx context.Context, tenantID, relID string) (*RelationshipDefinition, error) {
	var rel RelationshipDefinition
	err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", relID, tenantID).
		First(&rel).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRelationshipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metadata.GetRelationshipByID: %w", err)
	}
	return &rel, nil
}

// SoftDeleteRelationshipByID soft-deletes a single sys_relationships row by id (tenant-
// scoped), tombstone-renaming its unique-indexed columns first so their slots are freed:
//
//   - name            → "<name>__deleted_<id_prefix>" (uq_sysrel_tenant_name, migration 000025)
//   - join_table_name → "<join>__deleted_<id_prefix>" when non-NULL (uq_sysrel_tenant_join_table)
//
// Without the tombstones a deleted N-N relationship would permanently brick re-registering
// the SAME entity pair (both unique indexes span soft-deleted rows) — the exact RM-21
// failure family SoftDeleteEntity's relationship tombstone already guards against. The
// physical rel_* join table is deliberately NOT touched here (grace-window semantics: the
// caller decides if/when to reclaim it; re-registering the pair reuses it via CREATE TABLE
// IF NOT EXISTS, preserving existing links).
//
// Used by the W2 relationship lifecycle DELETE for N-N rows and as the orphan-row cleanup
// fallback for 1-N/1-1 rows whose owning relation field is already gone. Returns
// ErrRelationshipNotFound when no active row matches (id unknown, cross-tenant, or already
// deleted) so the caller can map it to a clean 404.
func (r *Repository) SoftDeleteRelationshipByID(ctx context.Context, tenantID, relID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing RelationshipDefinition
		if err := tx.Where("id = ? AND tenant_id = ?", relID, tenantID).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRelationshipNotFound
			}
			return fmt.Errorf("metadata.SoftDeleteRelationshipByID: load: %w", err)
		}
		suffix := tombstoneSuffix(relID)
		// name is VARCHAR(150); join_table_name is VARCHAR(100).
		updates := map[string]any{"name": tombstoneName(existing.Name, 150, suffix)}
		if existing.JoinTableName != nil && *existing.JoinTableName != "" {
			updates["join_table_name"] = tombstoneName(*existing.JoinTableName, 100, suffix)
		}
		if err := tx.Model(&RelationshipDefinition{}).
			Where("id = ? AND tenant_id = ?", relID, tenantID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("metadata.SoftDeleteRelationshipByID: tombstone: %w", err)
		}
		res := tx.Where("id = ? AND tenant_id = ?", relID, tenantID).Delete(&RelationshipDefinition{})
		if res.Error != nil {
			return fmt.Errorf("metadata.SoftDeleteRelationshipByID: soft delete: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrRelationshipNotFound
		}
		return nil
	})
}

// ListRelationshipsForEntity returns all active relationship definitions (any type) involving
// a given entity (as either parent or child). The query is tenant-scoped. Broadened by
// ENG-REL2 #4 — see GetRelationshipByName's doc comment for why the N-N predicate was removed.
func (r *Repository) ListRelationshipsForEntity(ctx context.Context, tenantID, entityName string) ([]RelationshipDefinition, error) {
	var rels []RelationshipDefinition
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND (parent_entity_name = ? OR child_entity_name = ?)",
			tenantID, entityName, entityName).
		Order("name ASC").
		Find(&rels).Error; err != nil {
		return nil, fmt.Errorf("metadata.ListRelationshipsForEntity: %w", err)
	}
	return rels, nil
}

// ListAllRelationships returns every active relationship definition (any type) for a tenant
// (for the Studio ERD canvas / GET /metadata/relationships). Tenant-scoped. Broadened by
// ENG-REL2 #4 — see GetRelationshipByName's doc comment for why the N-N predicate was removed.
func (r *Repository) ListAllRelationships(ctx context.Context, tenantID string) ([]RelationshipDefinition, error) {
	var rels []RelationshipDefinition
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Find(&rels).Error; err != nil {
		return nil, fmt.Errorf("metadata.ListAllRelationships: %w", err)
	}
	return rels, nil
}

// UpdateRelationshipViewConfig sets (viewConfig != nil) or clears (viewConfig == nil) the
// view_config JSON of one sys_relationships row, pinned to the tenant in the WHERE clause
// (defense-in-depth: the service already resolved the row via the tenant-scoped
// GetRelationshipByID). RowsAffected is deliberately NOT checked for not-found semantics:
// MySQL reports 0 affected rows for a same-value idempotent PUT (rows CHANGED, not rows
// matched), so 0 here is ambiguous between "no such row" and "no-op re-save". The caller's
// prior GetRelationshipByID owns the 404; a row deleted in the read→write window simply
// makes this a no-op (benign — the config dies with the row).
func (r *Repository) UpdateRelationshipViewConfig(ctx context.Context, tenantID, relID string, viewConfig *string) error {
	if err := r.db.WithContext(ctx).
		Model(&RelationshipDefinition{}).
		Where("id = ? AND tenant_id = ?", relID, tenantID).
		Update("view_config", viewConfig).Error; err != nil {
		return fmt.Errorf("metadata.UpdateRelationshipViewConfig: %w", err)
	}
	return nil
}

// ListRoleNames returns the tenant's role names, ascending. It lives here rather than on the role
// repository so *metadata.Repository keeps satisfying ai.MetadataReader with no adapter — the same
// reason: the AI reads documents through the services, not this repo.
//
// Names only: the AI builder needs to know WHICH roles it may name, and a role's grants are not
// its business (it proposes; a human applies through the gated admin API).
func (r *Repository) ListRoleNames(ctx context.Context, tenantID string) ([]string, error) {
	// acl_roles (migration 000005) — hard-deleted, so no deleted_at filter exists or is needed.
	// The original query addressed a nonexistent `roles` table and only unit-test fakes covered
	// it, so list_roles failed on every real call (security audit 2026-09-01, specs/020 §Nợ).
	var names []string
	if err := r.db.WithContext(ctx).
		Table("acl_roles").
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Pluck("name", &names).Error; err != nil {
		return nil, fmt.Errorf("metadata.Repository.ListRoleNames: %w", err)
	}
	return names, nil
}
