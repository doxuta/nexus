// Package dynrepo is the dynamic-record repository: it runs the generated DDL/DML
// against obj_* entity tables using sqlx + parameterized queries. System metadata
// (sys_*) is handled separately by the GORM-based metadata repository.
//
// Safety: table/column identifiers always come from ddl.* (allowlisted), never from
// raw input; record values always go through `?` placeholders.
package dynrepo

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// Repository executes dynamic schema and record operations over a sqlx connection.
type Repository struct {
	db *sqlx.DB
}

// New wires a dynamic-record repository over an existing sqlx connection.
func New(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// CreateTable materializes an entity's obj_* table from its field definitions. The
// statement is built by ddl.BuildCreateTable (which sanitizes every identifier) and is
// idempotent (CREATE TABLE IF NOT EXISTS).
func (r *Repository) CreateTable(ctx context.Context, entity string, fields []ddl.FieldDef) error {
	stmt, err := ddl.BuildCreateTable(entity, fields)
	if err != nil {
		return fmt.Errorf("dynrepo.CreateTable: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.CreateTable: exec: %w", err)
	}
	return nil
}

// TableExists reports whether an entity's obj_* table is already present. The create-entity
// flow checks this before CREATE TABLE so its compensation only drops a table IT created —
// never a pre-existing one (which a concurrent creator or a prior entity may depend on).
func (r *Repository) TableExists(ctx context.Context, entity string) (bool, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.TableExists: %w", err)
	}
	var n int
	err = r.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+name)
	if err != nil {
		return false, fmt.Errorf("dynrepo.TableExists: %w", err)
	}
	return n > 0, nil
}

// DropTable removes an entity's obj_* table. Used by the create-entity compensation
// path when metadata persistence fails after the table was created. The entity name is
// sanitized before being composed into the statement.
func (r *Repository) DropTable(ctx context.Context, entity string) error {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.DropTable: %w", err)
	}
	stmt := fmt.Sprintf("DROP TABLE IF EXISTS `obj_%s`", name)
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropTable: exec: %w", err)
	}
	return nil
}

// ColumnExists reports whether a given physical column already exists on an entity's obj_*
// table (queried via information_schema.COLUMNS, scoped to the current database). It guards
// the unique-field re-add path (spec §7.2 B-1): a soft-deleted field keeps its physical
// generated column, so re-adding a unique field whose metadata row is gone but whose column
// survives must NOT issue ALTER ADD COLUMN onto the existing column (which errors → 500).
// Both identifiers are sanitized; the values bind through `?` placeholders regardless.
func (r *Repository) ColumnExists(ctx context.Context, entity, field string) (bool, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.ColumnExists: entity: %w", err)
	}
	fname, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return false, fmt.Errorf("dynrepo.ColumnExists: field %q: %w", field, err)
	}
	var n int
	err = r.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+name, fname)
	if err != nil {
		return false, fmt.Errorf("dynrepo.ColumnExists: %w", err)
	}
	return n > 0, nil
}

// LiveUniqueValueExists reports whether an entity's obj_* table already holds a LIVE row
// (deleted_at IS NULL) for THIS tenant whose JSON value at $.<field> equals value. It is the
// read-only conflict probe behind the vertical-pack dry-run: a shared obj_* table can retain
// live rows of this tenant after the entity's METADATA was soft-deleted (the record rows keep
// deleted_at NULL), so a re-install's sample insert with the same unique value would hit MySQL
// 1062 → 409 RECORD_DUPLICATE. Surfacing it in the plan lets the admin see the collision before
// running the install live (trap B3).
//
// Semantics mirror the unique index EXACTLY:
//   - tenant-scoped: the unique key is (tenant_id, <col>), so only THIS tenant's rows can
//     collide — a co-tenant's identical value never 1062s and is deliberately not probed.
//   - deleted_at IS NULL: a soft-deleted RECORD projects NULL through the generated column's
//     CASE guard and does not squat the slot, so only live rows are a real collision.
//   - COLLATE utf8mb4_bin: the generated column is byte-distinct (utf8mb4_bin), so 'ABC' and
//     'abc' are DISTINCT slots; probing with the table's default (accent/case-insensitive)
//     collation would over-report. Matching the index collation avoids false positives.
//
// The entity + field identifiers are sanitized (table name and JSON path); tenantID and value
// bind through `?`. Querying the JSON path (not the generated column) makes the probe robust
// even when the shared table was materialized by a co-tenant WITHOUT this unique column.
func (r *Repository) LiveUniqueValueExists(ctx context.Context, tenantID, entity, field, value string) (bool, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.LiveUniqueValueExists: entity: %w", err)
	}
	fname, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return false, fmt.Errorf("dynrepo.LiveUniqueValueExists: field %q: %w", field, err)
	}
	stmt := fmt.Sprintf(
		"SELECT EXISTS(SELECT 1 FROM `obj_%s` "+
			"WHERE `tenant_id` = ? AND `deleted_at` IS NULL "+
			"AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) COLLATE utf8mb4_bin = ?)",
		name, fname)
	var exists int
	if err := r.db.GetContext(ctx, &exists, stmt, tenantID, value); err != nil {
		return false, fmt.Errorf("dynrepo.LiveUniqueValueExists: %w", err)
	}
	return exists == 1, nil
}

// AddUniqueColumn alters an entity's obj_* table to materialize a unique field: a STORED
// generated column projecting the JSON value plus a tenant-scoped unique index. The
// statement is built by ddl.BuildAddUniqueColumn (which sanitizes both identifiers) — used
// by the field service when a unique field is added to an already-registered entity.
func (r *Repository) AddUniqueColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildAddUniqueColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.AddUniqueColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		// ER_DUP_KEYNAME (1061): a concurrent AddField already created the same
		// `uq_<table>_<field>` index (RM-25 DDL race) — the index NAME collides. Surface
		// the typed sentinel so the field service maps it to 409 FIELD_ALREADY_EXISTS
		// rather than a generic 500. Checked before the 1062 branch: a name collision is
		// the concurrent-re-add signature, distinct from a data duplicate (1062) over a
		// shared table.
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddUniqueColumn: %w", ErrDuplicateKeyName)
		}
		// ER_DUP_ENTRY (1062) when MySQL materializes the STORED generated column over a
		// SHARED obj_* table and finds existing rows that already violate the new unique
		// index. Surface the typed sentinel (mapped to 409 CROSS_TENANT_SHARED_UNIQUE by a
		// caller that wants that verdict). RM-28 Slice B: no production runtime path calls
		// THIS method anymore for a plain unique field (see AddUniqueGencolNonUnique) — kept
		// as a tested primitive; see its doc comment for why it still exists.
		if _, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.AddUniqueColumn: %w", ErrDuplicateUnique)
		}
		return fmt.Errorf("dynrepo.AddUniqueColumn: exec: %w", err)
	}
	return nil
}

// DropUniqueColumn reverses AddUniqueColumn (drops the unique index + generated column).
// Used only on the field-service compensation path when the ALTER auto-committed but the
// metadata write failed afterward. The statement is built by ddl.BuildDropUniqueColumn.
func (r *Repository) DropUniqueColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropUniqueColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropUniqueColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropUniqueColumn: exec: %w", err)
	}
	return nil
}

// AddUniqueGencolNonUnique alters an entity's obj_* table to materialize a NEW `is_unique`
// field post RM-28 Slice B: the byte-identical gencol AddUniqueColumn builds, but a NON-unique
// tenant-scoped index. Uniqueness for this field is enforced SOLELY by the sys_unique_index
// pivot (dynrepo/pivot.go), never a physical UNIQUE index on the shared obj_* table. Because
// the index is non-unique, this ALTER can never raise ER_DUP_ENTRY (1062) — only ER_DUP_KEYNAME
// (1061) on a concurrent AddField racing to create the same `idx_` name (RM-25), mapped to the
// same ErrDuplicateKeyName sentinel AddIndexedColumn/AddRelationColumn use.
func (r *Repository) AddUniqueGencolNonUnique(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildAddUniqueGencolNonUnique(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.AddUniqueGencolNonUnique: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddUniqueGencolNonUnique: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.AddUniqueGencolNonUnique: exec: %w", err)
	}
	return nil
}

// DropUniqueGencolNonUnique reverses AddUniqueGencolNonUnique (drops the non-unique idx_ index
// + generated column). Used only on the field/entity-service compensation path when the ALTER
// auto-committed but the metadata write failed afterward. The statement is built by
// ddl.BuildDropUniqueGencolNonUnique.
func (r *Repository) DropUniqueGencolNonUnique(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropUniqueGencolNonUnique(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropUniqueGencolNonUnique: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropUniqueGencolNonUnique: exec: %w", err)
	}
	return nil
}

// AddRelationColumn alters an entity's obj_* table to materialize a 1-N `relation` field
// (FK-on-child, M3): a STORED CHAR(36) `<field>_id` column projecting the JSON value plus a
// non-unique tenant-scoped index. The statement is built by ddl.BuildAddRelationColumn
// (which sanitizes both identifiers) — used by the field service when a relation field is
// added to an already-registered entity.
func (r *Repository) AddRelationColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildAddRelationColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.AddRelationColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		// ER_DUP_KEYNAME (1061): a concurrent AddField already created this relation's
		// `idx_<table>_<field>_id` index (RM-25 DDL race). Map to 409 via the sentinel.
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddRelationColumn: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.AddRelationColumn: exec: %w", err)
	}
	return nil
}

// DropRelationColumn reverses AddRelationColumn (drops the FK index + generated `<field>_id`
// column). Used only on the field-service compensation path when the ALTER auto-committed
// but the metadata write failed afterward. Built by ddl.BuildDropRelationColumn.
func (r *Repository) DropRelationColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropRelationColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropRelationColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropRelationColumn: exec: %w", err)
	}
	return nil
}

// AddOneToOneRelationColumn alters an entity's obj_* table to materialize a 1-1
// `relation` field (ENG-REL2 #5, FK-on-child + tenant-scoped UNIQUE): a STORED CHAR(36)
// `<field>_id` column CASE-guarded against deleted_at, plus a tenant-scoped UNIQUE key.
// The statement is built by ddl.BuildAddOneToOneRelationColumn (which sanitizes both
// identifiers) — used by the field/entity service when a 1-1 relation field is added.
func (r *Repository) AddOneToOneRelationColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildAddOneToOneRelationColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.AddOneToOneRelationColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		// ER_DUP_FIELDNAME/ER_DUP_KEYNAME (1060/1061): a concurrent AddField already
		// materialized this column/index (RM-25 DDL race). Map to 409 via the sentinel.
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddOneToOneRelationColumn: %w", ErrDuplicateKeyName)
		}
		// ER_DUP_ENTRY (1062): MySQL refused to materialize the UNIQUE index because
		// existing rows on a SHARED obj_* table already hold more than one live record
		// pointing at the same parent for this tenant (a 1-1 cardinality violation).
		// Surface the typed sentinel so the orchestration layer maps it to 409
		// FIELD_RELATION_CARDINALITY_CONFLICT instead of an opaque 500.
		if _, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.AddOneToOneRelationColumn: %w", ErrDuplicateUnique)
		}
		return fmt.Errorf("dynrepo.AddOneToOneRelationColumn: exec: %w", err)
	}
	return nil
}

// DropOneToOneRelationColumn reverses AddOneToOneRelationColumn (drops the UNIQUE index +
// generated `<field>_id` column). Used only on the field/entity service's compensation
// path when the ALTER auto-committed but the metadata write failed afterward. Built by
// ddl.BuildDropOneToOneRelationColumn.
func (r *Repository) DropOneToOneRelationColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropOneToOneRelationColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropOneToOneRelationColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropOneToOneRelationColumn: exec: %w", err)
	}
	return nil
}

// AddIndexedColumn alters an entity's obj_* table to materialize a non-unique indexed
// field: a STORED generated column typed per the D1 design table (text→VARCHAR,
// number/rating/currency→DECIMAL, date/datetime→DATETIME) plus a non-unique tenant-scoped
// index idx_<table>_<field>. The statement is built by ddl.BuildAddIndexedColumn (which
// sanitizes all identifiers) — used by the field service when an `indexed` field is added
// to an already-registered entity, and by entity reconcile for a pre-existing shared table.
func (r *Repository) AddIndexedColumn(ctx context.Context, entity, field, fieldType string) error {
	stmt, err := ddl.BuildAddIndexedColumn(entity, field, fieldType)
	if err != nil {
		return fmt.Errorf("dynrepo.AddIndexedColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		// ER_DUP_KEYNAME (1061): a concurrent AddField already created this field's
		// `idx_<table>_<field>` index (RM-25 DDL race). Map to 409 via the sentinel.
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddIndexedColumn: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.AddIndexedColumn: exec: %w", err)
	}
	return nil
}

// DropIndexedColumn reverses AddIndexedColumn (drops the non-unique index + generated
// column). Used on the field-service compensation path when the ALTER auto-committed but
// the metadata write failed afterward, and by EditField when toggling indexed off.
// The statement is built by ddl.BuildDropIndexedColumn.
//
// Shared-table caveat: obj_<name> is shared across all tenants — dropping the generated
// column affects every tenant using this entity-name. This is a known trade-off (§8
// ARCHITECTURE); the toggle is scoped at entity-definition level, not per-tenant.
func (r *Repository) DropIndexedColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropIndexedColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropIndexedColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropIndexedColumn: exec: %w", err)
	}
	return nil
}

// AddPolymorphicColumns alters an entity's obj_* table to materialize a `polymorphic_relation`
// field (ACTIVITY-CORE #3a): two STORED generated columns (`<field>_type` VARCHAR(100),
// `<field>_id` CHAR(36)) projecting the JSON {type,id} object, plus a NON-unique composite
// tenant-scoped index over (tenant_id, <field>_type, <field>_id). The statement is built by
// ddl.BuildAddPolymorphicColumns (which sanitizes both identifiers) — used by the field service
// when a polymorphic field is added to an already-registered entity, and by entity reconcile for
// a pre-existing shared table.
func (r *Repository) AddPolymorphicColumns(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildAddPolymorphicColumns(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.AddPolymorphicColumns: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		// ER_DUP_FIELDNAME/ER_DUP_KEYNAME (1060/1061): a concurrent AddField already materialized
		// this column/index (RM-25 DDL race). Map to 409 via the sentinel.
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddPolymorphicColumns: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.AddPolymorphicColumns: exec: %w", err)
	}
	return nil
}

// DropPolymorphicColumns reverses AddPolymorphicColumns (drops the composite index + both
// generated columns). Used only on the field/entity service's compensation path when the ALTER
// auto-committed but the metadata write failed afterward. Built by ddl.BuildDropPolymorphicColumns.
func (r *Repository) DropPolymorphicColumns(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildDropPolymorphicColumns(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.DropPolymorphicColumns: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropPolymorphicColumns: exec: %w", err)
	}
	return nil
}

// SoftDeleteRecords marks every active record in the entity's shared obj_* table that
// belongs to THIS tenant as deleted (deleted_at = NOW()). It is called during entity
// soft-delete so that a recreated entity with the same name does NOT inherit the old
// tenant's data from the shared physical table.
//
// Why DML-only (no RENAME): obj_* tables are keyed by entity name, not by tenant — a
// single obj_students table is shared by every tenant that registers a "students" entity.
// Renaming the physical table would sever all other tenants from their live data. The
// correct fix is tenant-scoped DML: UPDATE ... SET deleted_at WHERE tenant_id = ? AND
// deleted_at IS NULL.
//
// The entity name is sanitized before being composed into the table identifier; the
// tenant_id is always a `?` placeholder (never string-concatenated).
func (r *Repository) SoftDeleteRecords(ctx context.Context, entity, tenantID string) error {
	if _, err := r.SoftDeleteRecordsAt(ctx, entity, tenantID, time.Now().UTC()); err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecords: %w", err)
	}
	return nil
}

// SoftDeleteRecordsAt is SoftDeleteRecords with the trash instant chosen by the caller and the number
// of rows moved returned (specs/063-draft-publish S7-9). A publish that deletes a table picks ONE
// instant, records it in the version BEFORE sweeping, and a retry passes the same one — so every row
// of one deletion carries the same deleted_at even when the first sweep died halfway, and a later
// rollback can restore exactly those rows (WHERE deleted_at = that instant).
func (r *Repository) SoftDeleteRecordsAt(ctx context.Context, entity, tenantID string, at time.Time) (int64, error) {
	// entity names are ASCII (enforced by ddl.SanitizeIdentifier at registration time),
	// so byte-slice indexing in the sanitizer and here is safe.
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: %w", err)
	}
	stmt := fmt.Sprintf(
		"UPDATE `obj_%s` SET `deleted_at` = ? WHERE `tenant_id` = ? AND `deleted_at` IS NULL",
		name,
	)
	// RM-28: the whole-entity soft-delete and the whole-entity pivot release run in ONE tx so a
	// recreated entity of the same name inherits neither the old tenant's live rows NOR its unique
	// slots. No unique-field list is needed here — DELETE every pivot row of (tenant, entity).
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, stmt, at.UTC(), tenantID)
	if err != nil {
		// RM-28 parity with the single-record mutators: a deadlock/lock-wait (1213/1205) on the
		// whole-entity obj_ UPDATE is transient and safe to retry — classify it the same way so
		// it maps to a retryable 503 instead of degrading to an opaque 500.
		if isWriteConflict(err) {
			return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: %w", ErrWriteConflict)
		}
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: rows affected: %w", err)
	}
	if err := deletePivotByEntity(ctx, tx, tenantID, name); err != nil {
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("dynrepo.SoftDeleteRecordsAt: commit: %w", err)
	}
	return n, nil
}

// RestoreRecordsSweptAt is the inverse of SoftDeleteRecordsAt (specs/063 S7-10b): it brings back the
// records of ONE tenant that a table delete moved to the trash at exactly `at`. Rows the staff trashed
// earlier carry another instant and stay in the trash; other tenants sharing the obj_* table are never
// touched (tenant_id is bound). It runs on the caller's transaction (the rollback tx). The pivot is
// rebuilt by the caller (BackfillUniqueFieldTx) once the rows are back.
func RestoreRecordsSweptAt(ctx context.Context, ex sqlx.ExecerContext, entity, tenantID string, at time.Time) (int64, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.RestoreRecordsSweptAt: %w", err)
	}
	res, err := ex.ExecContext(ctx,
		fmt.Sprintf("UPDATE `obj_%s` SET `deleted_at` = NULL WHERE `tenant_id` = ? AND `deleted_at` = ?", name),
		tenantID, at.UTC())
	if err != nil {
		if isWriteConflict(err) {
			return 0, fmt.Errorf("dynrepo.RestoreRecordsSweptAt: %w", ErrWriteConflict)
		}
		return 0, fmt.Errorf("dynrepo.RestoreRecordsSweptAt: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("dynrepo.RestoreRecordsSweptAt: rows affected: %w", err)
	}
	return n, nil
}

// DeletePivotByField removes ALL pivot rows of ONE tenant's (entity, field) from sys_unique_index
// (RM-28 §3.3). Called by the field service's DeleteField for a UNIQUE field: because DeleteField is
// no-DDL (the obj_* generated column survives the soft-delete), the pivot rows would otherwise
// linger and, after the field is re-created, 409 a record the tenant can no longer see. It is a
// single tenant-scoped statement (no obj_ DML to coordinate, so no transaction). entity/field are
// sanitized; tenant_id binds via `?`.
func (r *Repository) DeletePivotByField(ctx context.Context, entity, tenantID, field string) error {
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.DeletePivotByField: %w", err)
	}
	fname, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return fmt.Errorf("dynrepo.DeletePivotByField: field %q: %w", field, err)
	}
	if err := deletePivotByField(ctx, r.db, tenantID, entityName, fname); err != nil {
		return fmt.Errorf("dynrepo.DeletePivotByField: %w", err)
	}
	return nil
}
