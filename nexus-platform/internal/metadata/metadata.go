package metadata

import (
	"time"

	"gorm.io/gorm"
)

// EntityDefinition defines a dynamic entity/object registered via metadata (e.g. Account, Student, Ticket).
// The platform reads these rows at runtime to expose CRUD APIs — entities are NOT hardcoded as Go structs.
type EntityDefinition struct {
	ID          string         `gorm:"type:varchar(36);primaryKey" json:"id"`
	TenantID    string         `gorm:"type:varchar(36);index" json:"tenantId"`
	Name        string         `gorm:"type:varchar(100);index" json:"name"`                  // e.g. "students", "accounts"
	Label       string         `gorm:"type:varchar(100)" json:"label"`                       // e.g. "Học viên", "Khách hàng"
	DataTable   string         `gorm:"column:table_name;type:varchar(100)" json:"tableName"` // dynamic data table, e.g. "obj_students"
	Description string         `gorm:"type:text" json:"description,omitempty"`
	Options     *string        `gorm:"type:json" json:"options,omitempty"` // icon, color, feature flags (JSON); NULL when unset
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	// DraftState (specs/063 S7-9) is NOT a column: the builder's draft-overlay list stamps "deleted" on a
	// table marked "sẽ xoá khi xuất bản". Empty for every other reader.
	DraftState string `gorm:"-" json:"draftState,omitempty"`
}

// TableName pins the metadata registry table (system table, GORM-managed).
func (EntityDefinition) TableName() string { return "sys_entities" }

// FieldDefinition defines a field inside an entity.
type FieldDefinition struct {
	ID       string `gorm:"type:varchar(36);primaryKey" json:"id"`
	EntityID string `gorm:"type:varchar(36);index" json:"entityId"`
	// TenantID denormalizes the owning tenant onto the field row (added in migration 000004,
	// enforced NOT NULL by migration 000010 after backfill from sys_entities). It is NOT
	// exposed in the API payload (json:"-") and is NEVER read from a client request:
	// CreateField sets it from the resolved entity's tenant, and the mutating repo methods
	// re-pin it in their WHERE clause as a defense-in-depth tenant guard on top of the
	// GetFieldByID JOIN.
	TenantID   string  `gorm:"column:tenant_id;type:varchar(36);not null;index" json:"-"`
	Name       string  `gorm:"type:varchar(100);index" json:"name"`
	Label      string  `gorm:"type:varchar(100)" json:"label"`
	FieldType  string  `gorm:"type:varchar(50)" json:"fieldType"` // text, number, date, picklist, lookup, ...
	IsRequired bool    `gorm:"type:tinyint(1);default:0" json:"isRequired"`
	IsUnique   bool    `gorm:"type:tinyint(1);default:0" json:"isUnique"`
	DefaultVal string  `gorm:"type:varchar(255)" json:"defaultValue,omitempty"`
	Options    *string `gorm:"type:json" json:"options,omitempty"` // picklist options / validation rules; NULL when unset
	// FieldOrder (W5, migration 000033) is the admin-controlled display position within the
	// entity. GET /metadata/fields/:entity sorts by it (tie-break: name), so it drives the
	// default ordering of generated form/list layouts. Writers always assign it: entity-create
	// numbers fields by slice position ((i+1)*10), add-field appends (max+10), and
	// PUT /admin/fields/reorder renumbers a whole entity in one transaction.
	FieldOrder int `gorm:"column:field_order;not null;default:0" json:"fieldOrder"`
	// Description (W5, migration 000033) is the per-field admin description / builder help
	// text (≤500 runes, enforced at the service layer). Distinct from a layout cell's
	// per-view helpText (sys_layouts JSON).
	Description string         `gorm:"type:varchar(500)" json:"description,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	// DraftState / PendingRequired (specs/063 S7-8) are NOT columns: the builder's draft-overlay read
	// stamps "new" (born after the last publish) / "edited" (has an unpublished edit), and
	// PendingRequired when "bắt buộc" is on in the draft only.
	DraftState      string `gorm:"-" json:"draftState,omitempty"`
	PendingRequired bool   `gorm:"-" json:"pendingRequired,omitempty"`
}

// TableName pins the metadata registry table.
func (FieldDefinition) TableName() string { return "sys_fields" }

// RelationshipDefinition defines a relation between two entities (1-1, 1-N, N-N).
//
// TenantID was added to sys_relationships as NULLABLE in migration 000004 and enforced
// NOT NULL in migration 000010. Originally the table was inert (0 rows; real 1-N
// relationships were modeled ONLY via sys_fields.options.relatedEntity, with no registry
// row). ENG-REL2 #4 (migration 000028) is the FIRST writer of non-N-N rows: every 1-N
// `relation` field (FK-on-child) now ALSO gets a companion sys_relationships row (written
// atomically with the sys_fields row — see metadata.Repository.CreateFieldWithRelationship /
// CreateEntityWithFields) so the inverse (parent) side of the relation is discoverable via
// GET /metadata/relationships. EVERY writer MUST set TenantID from the owning tenant before
// inserting — the NOT NULL constraint at DB level will refuse any insert that omits it. Do
// NOT add rows without TenantID.
//
// RM-41 schema extensions: 000028 adds ONLY InverseField + ChildFKColumn. join_key_lhs/rhs
// and primary_flag_column remain deferred to a milestone that actively needs them.
type RelationshipDefinition struct {
	ID string `gorm:"type:varchar(36);primaryKey" json:"id"`
	// TenantID enforced NOT NULL since migration 000010 (RM-18). Must be set server-side from the
	// owning tenant on every insert — NEVER from a client request body. json:"-" (mirrors
	// FieldDefinition.TenantID) so a future M3 writer that binds a request struct cannot let a
	// client inject tenant_id — the exact cross-tenant class RM-18 closes for fields.
	TenantID         string `gorm:"column:tenant_id;type:varchar(36);not null;index" json:"-"`
	Name             string `gorm:"type:varchar(150);index" json:"name"`
	ParentEntityName string `gorm:"type:varchar(100);index" json:"parentEntityName"`
	ChildEntityName  string `gorm:"type:varchar(100);index" json:"childEntityName"`
	RelationshipType string `gorm:"type:varchar(20)" json:"relationshipType"` // "1-1", "1-N", "N-N"
	// JoinTableName is a *string (NOT a plain string) so an unset value is stored as SQL NULL,
	// not an empty string. This matters because migration 000025 added
	// UNIQUE(tenant_id, join_table_name): MySQL treats each NULL as distinct for uniqueness, but
	// treats "" as a real, colliding value. A 1-N row (which has no join table) never sets this
	// field; if it defaulted to the Go zero value ("") instead of nil, the SECOND 1-N relation
	// field ever registered for a tenant would trip the unique index on (tenant_id, "") and the
	// whole 1-N registry write would fail. N-N rows (RegisterRelationship) always set a non-nil
	// pointer to the real join table name.
	JoinTableName *string        `gorm:"type:varchar(100)" json:"joinTableName,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"-"`
	// InverseField (ENG-REL2 #4, migration 000028) names the field exposed on the PARENT side
	// of a 1-N relation — e.g. "contacts" on `accounts`, mirroring the FK-on-child field
	// "account" on `contacts`. Empty for N-N rows (no single inverse field — N-N is symmetric,
	// discovered via the relationship name itself on either side).
	InverseField string `gorm:"column:inverse_field;type:varchar(100)" json:"inverseField,omitempty"`
	// ChildFKColumn (ENG-REL2 #4, migration 000028) names the physical `<field>_id` column on
	// the CHILD (owning) entity's obj_* table, so a reader never has to re-derive it from the
	// field name. Empty for N-N rows (no FK-on-child column; the relation lives in a join table).
	ChildFKColumn string `gorm:"column:child_fk_column;type:varchar(100)" json:"childFkColumn,omitempty"`
	// ViewConfig (ENG-RELVIEW, migration 000044) is the OPTIONAL related-list ("subpanel")
	// rendering config as a raw JSON document: {label?, order?, columns?, sort?, allowCreate?,
	// allowLink?}. nil = no config = the FE's default auto-derived rendering. It is validated
	// and written ONLY by RelationLifecycleService.SetViewConfig (columns/sort pinned to the
	// CHILD entity's fields). json:"-" on purpose (mirrors FieldDefinition.Options handling,
	// RM-02): a raw *string would serialize as an ESCAPED JSON string — the metadata read
	// endpoint parses it into a real object in its response DTO instead.
	ViewConfig *string `gorm:"column:view_config;type:json" json:"-"`
}

// TableName pins the metadata registry table.
func (RelationshipDefinition) TableName() string { return "sys_relationships" }

// LayoutDefinition defines how forms and grids are structured in the UI.
type LayoutDefinition struct {
	ID         string         `gorm:"type:varchar(36);primaryKey" json:"id"`
	TenantID   string         `gorm:"type:varchar(36);index" json:"tenantId"`
	EntityName string         `gorm:"type:varchar(100);index" json:"entityName"`
	ViewType   string         `gorm:"type:varchar(50)" json:"viewType"` // "edit", "detail", "list"
	LayoutJSON string         `gorm:"type:json" json:"layoutConfig,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
	// DraftState is set only by the draft layer's overlay read (specs/063-draft-publish): "live",
	// "edited" or "new". Never stored.
	DraftState string `gorm:"-" json:"-"`
}

// TableName pins the metadata registry table.
func (LayoutDefinition) TableName() string { return "sys_layouts" }

// secondNow is the stamp BeforeCreate puts on created_at / updated_at of the three tables the
// publish cutoff is compared against (sys_entities, sys_fields, sys_relationships). Those columns
// are TIMESTAMP(0) and MySQL ROUNDS the fractional time GORM would otherwise send, so a row born at
// 12:00:00.857 was stored as 12:00:01 — AFTER a publish cutoff floored to 12:00:00 taken a moment
// later, and a field the publish had just released stayed hidden from the customer. Flooring here
// keeps created_at <= the real birth instant, the same direction as the cutoff.
func secondNow() time.Time { return time.Now().Truncate(time.Second) }

func stampSecond(created, updated *time.Time) {
	if created.IsZero() {
		*created = secondNow()
	}
	if updated.IsZero() {
		*updated = *created
	}
}

// BeforeCreate floors created_at / updated_at to the second (see secondNow).
func (e *EntityDefinition) BeforeCreate(*gorm.DB) error {
	stampSecond(&e.CreatedAt, &e.UpdatedAt)
	return nil
}

// BeforeCreate floors created_at / updated_at to the second (see secondNow).
func (f *FieldDefinition) BeforeCreate(*gorm.DB) error {
	stampSecond(&f.CreatedAt, &f.UpdatedAt)
	return nil
}

// BeforeCreate floors created_at / updated_at to the second (see secondNow).
func (r *RelationshipDefinition) BeforeCreate(*gorm.DB) error {
	stampSecond(&r.CreatedAt, &r.UpdatedAt)
	return nil
}
