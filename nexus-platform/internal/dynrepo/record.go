package dynrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"nexus/platform/internal/ddl"
)

// ErrRecordNotFound is returned when a record does not exist for the given tenant (or has
// been soft-deleted). Cross-tenant reads surface as not-found, never as another tenant's row.
var ErrRecordNotFound = errors.New("record not found")

// ErrDuplicateUnique is a sentinel errors.Is target. Callers that only care whether a
// duplicate occurred use errors.Is(err, ErrDuplicateUnique); callers that also need to
// know which index was violated use errors.As(err, &DuplicateUniqueError{}).
var ErrDuplicateUnique = errors.New("duplicate unique value")

// DuplicateUniqueError is the typed error returned on MySQL ER_DUP_ENTRY (1062). It carries
// the raw MySQL index name (IndexName) so the service layer can extract the field name and
// populate RECORD_DUPLICATE details.field. It wraps ErrDuplicateUnique so errors.Is still
// works for callers that don't care about the index name.
type DuplicateUniqueError struct {
	// IndexName is the raw MySQL constraint/key name from the ER_DUP_ENTRY message,
	// e.g. "uq_obj_accounts_email" or "obj_accounts.uq_obj_accounts_email" (MySQL 8.0.19+
	// may qualify with the table name). The service strips any table-qualifier prefix before
	// interpreting the name.
	IndexName string
	// Field is the ALREADY-RESOLVED field name, set when the duplicate came from a pivot
	// (sys_unique_index) INSERT — RM-28. The pivot INSERT is field-granular (one field per
	// statement), so the write path knows exactly which field's uq_pivot fired and attaches the
	// name directly instead of parsing an index name (whose name is the constant "uq_pivot" and
	// carries no field). The service's asDuplicate prefers this over extractDuplicateField.
	Field string
}

func (e *DuplicateUniqueError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("duplicate unique value (field %q)", e.Field)
	}
	return fmt.Sprintf("duplicate unique value (index %q)", e.IndexName)
}

// Unwrap makes errors.Is(err, ErrDuplicateUnique) return true for DuplicateUniqueError.
func (e *DuplicateUniqueError) Unwrap() error { return ErrDuplicateUnique }

// ErrDuplicateKeyName is a sentinel errors.Is target for the concurrent-ADD-COLUMN/KEY race
// signatures of RM-25: MySQL ER_DUP_FIELDNAME (1060, "Duplicate column name") and
// ER_DUP_KEYNAME (1061, "Duplicate key name"). When two concurrent AddField calls for the
// SAME (tenant, entity, field) both pass the application dup-check and both issue the same
// `ALTER ... ADD COLUMN <field> ..., ADD {UNIQUE} KEY <idx> ...`, the loser's ALTER fails
// atomically — on the ADD COLUMN (1060) when the winner already materialized the generated
// column, or on the ADD KEY (1061) when only the index name collides. Either way the field
// already exists, so the orchestration layer maps this to 409 FIELD_ALREADY_EXISTS instead of
// an opaque 500. (MySQL 8 ALTER is atomic, so the loser's whole statement rolls back; no
// half-added column survives.)
var ErrDuplicateKeyName = errors.New("duplicate column or index name")

// ErrVersionConflict is returned by UpdateRecord when the caller supplied an expected
// version (RM-24 optimistic locking, opt-in) that did NOT match the row's current version —
// a lost-update was prevented. It carries Current so the service can surface
// {currentVersion:N} in the 409 RECORD_VERSION_CONFLICT envelope, letting the FE reconcile
// a stale edit. It is distinct from ErrRecordNotFound: a non-existent / soft-deleted row is
// the latter, a live row at a different version is this.
type VersionConflictError struct {
	// Current is the record's actual version in the database at the time of the failed
	// conditional UPDATE (always ≥ 0). The caller's expected version differed from it.
	Current int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("record version conflict (current %d)", e.Current)
}

// ErrVersionConflict is the sentinel errors.Is target for VersionConflictError. Callers that
// only care that a conflict occurred use errors.Is(err, ErrVersionConflict); callers that need
// the current version use errors.As(err, &VersionConflictError{}).
var ErrVersionConflict = errors.New("record version conflict")

// Unwrap makes errors.Is(err, ErrVersionConflict) return true for VersionConflictError.
func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// mysqlDuplicateErrNo is MySQL's ER_DUP_ENTRY (violated UNIQUE KEY).
const mysqlDuplicateErrNo = 1062

// mysqlDupFieldNameErrNo is MySQL's ER_DUP_FIELDNAME (ALTER adds a column whose name exists).
const mysqlDupFieldNameErrNo = 1060

// mysqlDupKeyNameErrNo is MySQL's ER_DUP_KEYNAME (ALTER adds an index whose name exists).
const mysqlDupKeyNameErrNo = 1061

// mysqlDeadlockErrNo (ER_LOCK_DEADLOCK) and mysqlLockWaitTimeoutErrNo (ER_LOCK_WAIT_TIMEOUT) are
// the two TRANSIENT write-contention signatures (RM-28 §3.5). They can arise on the obj_ UPDATE or
// on a pivot INSERT/DELETE under concurrency (the lock order is always obj_ then sys_unique_index).
// MySQL auto-rolls-back the transaction on 1213 and the statement on 1205, so the whole request is
// safe to retry — the service maps them to a retryable 503 instead of an opaque 500.
const (
	mysqlDeadlockErrNo        = 1213
	mysqlLockWaitTimeoutErrNo = 1205
)

// ErrWriteConflict is the sentinel errors.Is target for a transient MySQL write-contention failure
// (ER_LOCK_DEADLOCK 1213 / ER_LOCK_WAIT_TIMEOUT 1205). The dynamic-record handler maps it to a
// retryable 503 (RECORD_WRITE_CONFLICT) — mirroring the existing DB_TIMEOUT branch — so a caller
// can retry the same request rather than receiving an opaque 500.
var ErrWriteConflict = errors.New("transient write conflict (deadlock or lock-wait timeout)")

// isWriteConflict reports whether err is a MySQL deadlock (1213) or lock-wait timeout (1205).
func isWriteConflict(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlDeadlockErrNo || me.Number == mysqlLockWaitTimeoutErrNo
}

// isDuplicateKeyName reports whether err is a MySQL ER_DUP_FIELDNAME (1060) or ER_DUP_KEYNAME
// (1061) — the two concurrent-ADD race signatures. Used by the DDL helpers (AddUniqueColumn /
// AddRelationColumn / AddIndexedColumn) to classify the race so the service maps it to 409
// rather than 500 (RM-25). A unique-field ALTER adds the column FIRST, so the loser most often
// trips 1060; an index-only collision trips 1061. Both mean: the field already exists.
func isDuplicateKeyName(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlDupFieldNameErrNo || me.Number == mysqlDupKeyNameErrNo
}

// duplicateIndexName extracts the index name from a MySQL ER_DUP_ENTRY error message.
// MySQL 8.0 messages look like:
//
//	"Duplicate entry 'value' for key 'table.index_name'"   (8.0.19+)
//	"Duplicate entry 'value' for key 'index_name'"          (older)
//
// Returns ("", false) when the error is not a duplicate or the index cannot be extracted.
func duplicateIndexName(err error) (string, bool) {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlDuplicateErrNo {
		return "", false
	}
	// Find the last single-quoted token — that is the key name (with optional table. prefix).
	msg := me.Message
	last := strings.LastIndex(msg, "'")
	if last < 0 {
		return "", true // is duplicate, but cannot parse index name
	}
	prev := strings.LastIndex(msg[:last], "'")
	if prev < 0 {
		return "", true
	}
	raw := msg[prev+1 : last]
	// Strip optional table-qualifier (MySQL 8.0.19+ emits "table.index_name").
	if dot := strings.Index(raw, "."); dot >= 0 {
		raw = raw[dot+1:]
	}
	return raw, true
}

// ErrDataConstraint is the sentinel errors.Is target for a record value that violates a
// physical-column constraint at write time: MySQL ER_WARN_DATA_OUT_OF_RANGE (1264),
// ER_DATA_TOO_LONG (1406), ER_TRUNCATED_WRONG_VALUE (1292, e.g. a malformed datetime cast)
// and ER_TRUNCATED_WRONG_VALUE_FOR_FIELD (1366). These arise when a value the JSON `data`
// column accepts still overflows / mistypes a STORED generated column (unique/indexed). The
// service maps it to 422 (RECORD_INVALID_RANGE / RECORD_TOO_LONG) instead of an opaque 500.
// Record-time validation (checkGenColumnText / checkGenColumnDecimal) catches the common
// cases first; this repo classifier is the defense-in-depth backstop (W5-B4).
var ErrDataConstraint = errors.New("record value violates a column constraint")

// DataConstraintKind distinguishes the too-long case (→ RECORD_TOO_LONG) from the
// out-of-range / wrong-value case (→ RECORD_INVALID_RANGE) so the service emits the right code.
type DataConstraintKind int

const (
	// DataConstraintRange covers 1264 (out of range), 1292 / 1366 (wrong/truncated value).
	DataConstraintRange DataConstraintKind = iota
	// DataConstraintTooLong covers 1406 (data too long for column).
	DataConstraintTooLong
)

// DataConstraintError is the typed error for a column-constraint violation (W5-B4). Field is
// the best-effort column name parsed from the MySQL message (may be ""); Kind selects the
// service-level code.
type DataConstraintError struct {
	Kind  DataConstraintKind
	Field string
}

func (e *DataConstraintError) Error() string {
	return fmt.Sprintf("record value violates column constraint (kind %d, field %q)", e.Kind, e.Field)
}

// Unwrap makes errors.Is(err, ErrDataConstraint) return true for DataConstraintError.
func (e *DataConstraintError) Unwrap() error { return ErrDataConstraint }

// MySQL error numbers for value/column constraint failures (W5-B2/B3/B4).
const (
	mysqlOutOfRangeErrNo      = 1264 // ER_WARN_DATA_OUT_OF_RANGE
	mysqlDataTooLongErrNo     = 1406 // ER_DATA_TOO_LONG
	mysqlTruncatedWrongValue  = 1292 // ER_TRUNCATED_WRONG_VALUE (e.g. bad datetime cast)
	mysqlTruncatedWrongForFld = 1366 // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD
)

// asDataConstraintError classifies a MySQL value/column-constraint error (1264/1406/1292/1366)
// into a typed *DataConstraintError, extracting the offending column name from the message when
// present. Returns (nil, false) for any other error so the caller passes it through unchanged.
func asDataConstraintError(err error) (*DataConstraintError, bool) {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) {
		return nil, false
	}
	switch me.Number {
	case mysqlDataTooLongErrNo:
		return &DataConstraintError{Kind: DataConstraintTooLong, Field: columnFromConstraintMsg(me.Message)}, true
	case mysqlOutOfRangeErrNo, mysqlTruncatedWrongValue, mysqlTruncatedWrongForFld:
		return &DataConstraintError{Kind: DataConstraintRange, Field: columnFromConstraintMsg(me.Message)}, true
	default:
		return nil, false
	}
}

// columnFromConstraintMsg extracts the column name from MySQL constraint messages of the form
//
//	"Out of range value for column 'col' at row 1"
//	"Data too long for column 'col' at row 1"
//	"Incorrect datetime value: '...' for column 'col' at row 1"
//
// Returns "" when no `for column 'name'` clause is present. The extracted name is the physical
// generated-column name, which for unique/indexed fields equals the field name (or `<field>_id`).
func columnFromConstraintMsg(msg string) string {
	const marker = "for column '"
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// Record is a dynamic entity record: fixed system fields plus the dynamic `data` map.
type Record struct {
	ID       string         `json:"id"`
	TenantID string         `json:"tenantId"`
	Data     map[string]any `json:"data"`
	// Version is the record's optimistic-lock counter (RM-24). It is returned on reads
	// (GET/List) so a client can echo it back via If-Match on a subsequent PUT; every
	// successful version-checked UPDATE bumps it by one.
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// CreatedBy is the record's immutable owner (the user id that created it). It is read by
	// GetRecord/ListRecords ONLY to back the workflow engine's "$owner" notification-recipient
	// sentinel (M7 MED-1 fix — internal/workflow's runInAppNotification resolves "$owner" to
	// this value via the Event the caller builds). `json:"-"`: this is NOT part of the public
	// record API response shape — exposing it would be a separate, deliberate change, not a
	// side effect of this internal plumbing.
	CreatedBy string `json:"-"`
	// OwnerID is the record's CURRENT owner — obj_*.owner_id when set, else created_by. This is
	// the same value the ACL row filter enforces on (COALESCE(owner_id, created_by)), so what a
	// screen shows and what the permission layer decides can no longer drift apart. Exposed on
	// the API (unlike CreatedBy) because "Đổi chủ" was otherwise a button with no visible
	// effect: the record read never returned the owner, so nothing could display it.
	OwnerID string `json:"ownerId,omitempty"`
	// SharedWithMe is true when the caller sees this record ONLY through a manual share
	// (specs/051-record-manual-share, DAILY-QUEUE #223) — shares are read-only, so the record page hides
	// its edit actions on open instead of answering the save with 403 RECORD_SHARED_READ_ONLY. Set by
	// GetRecord on a share-armed read; absent everywhere else (lists, writes, AccessAll, admin).
	SharedWithMe bool `json:"sharedWithMe,omitempty"`
	// CanEdit is a pointer so "unknown" (nil, omitted) differs from "no": it is &false when the caller's
	// EDIT scope does not reach this record although their VIEW scope does (view=all, edit=own — the
	// rep who sees a colleague's lead; DAILY-QUEUE #334), so the record page hides Sửa on open instead
	// of answering the save with 404. Set only by RecordService.Get (GET /:entity/:id); nil everywhere
	// else and for admins / editable records — absent means "no restriction known", never "forbidden".
	CanEdit *bool `json:"canEdit,omitempty"`
}

// FilterOperator is the comparison operator applied by a single filter predicate.
// The zero value (empty string) is treated as FilterEq so existing callers that omit
// the operator keep working without change.
type FilterOperator string

const (
	// FilterEq is the default equality operator (field = value, case-insensitive for
	// text/picklist/email per RM-14, exact for relation/unique generated columns).
	FilterEq FilterOperator = "eq"
	// FilterNeq is the inequality operator (field != value).
	FilterNeq FilterOperator = "neq"
	// FilterGt / FilterGte / FilterLt / FilterLte are numeric/date comparisons.
	FilterGt  FilterOperator = "gt"
	FilterGte FilterOperator = "gte"
	FilterLt  FilterOperator = "lt"
	FilterLte FilterOperator = "lte"
	// FilterIn matches any of a set of values (IN (?,?,…)).
	FilterIn FilterOperator = "in"
	// FilterBetween matches values in [lo, hi] (BETWEEN ? AND ?).
	FilterBetween FilterOperator = "between"
	// FilterIsNull / FilterIsNotNull match NULL / NOT NULL (no value needed).
	FilterIsNull    FilterOperator = "isnull"
	FilterIsNotNull FilterOperator = "notnull"
	// FilterContains is a substring match (LIKE %term%, with wildcard escaping).
	FilterContains FilterOperator = "contains"
)

// Filter is a single resolved predicate on a dynamic JSON field.
//
// Column MUST be a sanitized, declared field name (the service validates it against
// sys_fields before it reaches here) because it is interpolated into the JSON path.
//
// Operator selects the comparison; the zero value ("") is treated as FilterEq
// (backward-compatible with existing callers that only set Column/Value).
//
// Values holds one or more operand strings; for eq/neq/gt/gte/lt/lte/contains the slice
// has exactly one element; for in, one per member; for between, exactly two ([lo, hi]);
// for isnull/notnull the slice is empty. Value is kept for backward compat (eq alias).
//
// CastNumeric instructs buildPredicates to cast the generated column/JSON path via
// CAST(... AS DECIMAL(38,10)) before comparing, so numeric operators are not
// lexicographic. The service sets this for number fields.
//
// CastDatetime instructs buildPredicates to cast the JSON path via
// CAST(... AS DATETIME) before comparing, so ordered comparisons on date/datetime fields
// are on the datetime instant rather than raw strings (which breaks under mixed timezone
// offsets). The service sets this for date/datetime ordered ops (S3).
//
// CaseInsensitive instructs buildPredicates to use COLLATE utf8mb4_unicode_ci for
// eq/neq/in/contains — letting the DB handle case folding uniformly (RM-14 / S4).
// It is NEVER set for relation (UUID exact-match) or unique generated columns (case
// matters for unique codes/SKUs).
type Filter struct {
	Column       string
	Value        string         // kept for backward compat; mirrors Values[0] for single-value ops
	Operator     FilterOperator // defaults to FilterEq when zero
	Values       []string       // operand(s); replaces Value for multi-value ops
	CastNumeric  bool           // apply CAST(... AS DECIMAL(38,10)) for ordered comparisons
	CastDatetime bool           // apply CAST(... AS DATETIME) for date/datetime ordered comparisons
	// CaseInsensitive uses COLLATE utf8mb4_unicode_ci for eq/neq/in on text types (S4).
	// For contains, case-insensitivity is always applied regardless of this flag.
	CaseInsensitive bool

	// JSONSubPath, when non-empty, appends a nested path segment to the JSON extraction
	// expression. Used for currency fields: the value is stored as {"amount":N,...} so
	// numeric ops must target "$.field.amount" rather than "$.field". The service sets
	// this to "amount" for currency filters.
	JSONSubPath string

	// PhysicalColumn, when non-empty, directs the predicate to a physical generated
	// column (e.g. the unique generated column named after the field, or the relation
	// <field>_id column) instead of JSON_UNQUOTE(JSON_EXTRACT(data, '$.col')). The
	// service sets this after building the sort allowlist (RM-09/RM-13 rewrite).
	PhysicalColumn string
}

// Search is a resolved substring (?q) search. Term is the raw needle (placeholder value);
// Columns are sanitized, declared string-typed field names the needle is matched against
// with case-insensitive LIKE. An empty Columns slice means nothing to search.
type Search struct {
	Term    string
	Columns []string
}

// ListParams controls pagination, ordering, JSON field filtering and substring search.
//
// Sort is the WIRE field name the client asked to order by; normalize() resolves it to the
// physical column THROUGH SortAllowlist (fieldName → physicalColumn) and answers
// ErrUnsortableField for a name that is not a key. So the service hands over the key, not the
// translated column: for a relation/user field the key is "account" and the column is
// "account_id" — passing "account_id" here is a miss (this comment used to say "physical
// column name" and the service believed it; every relation/user sort answered 500 until
// DAILY-QUEUE #4). SortAllowlist is the set the service has verified is safe to sort on for
// this entity; normalize() picks from it rather than the hard-coded sortColumns so dynamic
// unique/relation generated columns (RM-09) are honoured. When SortAllowlist is nil,
// normalize() falls back to the hard-coded system-column allowlist for backward compatibility.
//
// Filters/Search columns are sanitized declared field names supplied by the service
// layer (never raw client input).
type ListParams struct {
	Page    int
	Limit   int
	Sort    string // wire field name — a KEY of SortAllowlist (or of sortColumns when nil)
	Order   string
	Filters []Filter
	Search  *Search
	// SortAllowlist maps WIRE field names (the keys Sort is matched against) to the physical
	// column ORDER BY uses; only the VALUE ever reaches SQL. Built by the service from system
	// cols + unique/relation/user/indexed fields (a relation key "account" maps to "account_id";
	// polymorphic sub-columns "<f>_type"/"<f>_id" are their own keys). When nil, falls back
	// to the hard-coded sortColumns map.
	SortAllowlist map[string]string // wireFieldName → physicalCol
	// OnlyDeleted flips the soft-delete predicate from the default `deleted_at IS NULL`
	// (live rows) to `deleted_at IS NOT NULL` (the recycle bin, W9b). It is applied
	// IDENTICALLY to the COUNT and the page query so totalPages stays consistent. All
	// other scoping (tenant_id, A5 row-scope, filters, search) is unchanged, so a
	// user only ever sees deleted rows they could have seen live.
	OnlyDeleted bool
	// ExcludeSamples drops rows carrying a live sample-data mark (sys_sample_records, specs/064
	// S9-4) from BOTH the count and the page. Only the workflow scheduler's list sets it, so a
	// scheduled rule never emails / webhooks a sample row; every other list still shows samples.
	ExcludeSamples bool
}

// softDeletePredicate returns the `deleted_at` WHERE fragment for a list: the default live
// filter (`deleted_at IS NULL`) or, for the recycle bin (W9b OnlyDeleted), `deleted_at IS NOT
// NULL`. It is a constant SQL fragment (no interpolation) so it is safe to inline.
func softDeletePredicate(onlyDeleted bool) string {
	if onlyDeleted {
		return "`deleted_at` IS NOT NULL"
	}
	return "`deleted_at` IS NULL"
}

// sortColumns is the allowlist of sortable columns. JSON `data` fields are not sortable in
// this slice (would need generated columns); default is created_at.
var sortColumns = map[string]string{
	"created_at": "created_at",
	"updated_at": "updated_at",
	"id":         "id",
}

// CursorParams carries the parameters for a keyset-paginated query. Sort must already be the
// PHYSICAL column name (e.g. "account_id", not "account") as resolved by the service layer.
// The caller (service) has already validated Sort against the sort allowlist — the repo re-checks
// with SanitizeIdentifier for defense-in-depth.
type CursorParams struct {
	Limit   int
	After   string // opaque encoded cursor ("" = first page, no keyset predicate)
	Sort    string // caller-resolved physical column name (already validated)
	Order   string // "ASC" or "DESC"
	Filters []Filter
	Search  *Search
}

// recordRowCursor extends recordRow with the sort column value aliased as __cursor_val.
//
// CAST(`sortCol` AS CHAR): prevents parseTime=true from returning a time.Time for TIMESTAMP
// columns. Without CAST the driver returns time.Time → Go formats as RFC3339Nano, which MySQL
// won't compare correctly in keyset predicates. CAST AS CHAR gives the MySQL-native text
// ("2026-06-24 10:00:00", numerics, UUIDs) that binds back as an exact value.
//
// NULL handling: when the sort column IS NULL, CAST(NULL AS CHAR) produces SQL NULL, which
// sqlx scans as a nil []byte. The cursor encoder checks CursorVal == nil (not string(nil)=="")
// to detect NULL unambiguously — this is what drives the NULLS-LAST predicate branching.
type recordRowCursor struct {
	recordRow
	CursorVal []byte `db:"__cursor_val"` // nil when sort column IS NULL; MySQL-native text otherwise
}

// ClampPageLimit applies the pagination bounds (page>=1, 1<=limit<=100, default 20) so the
// handler and the repository agree on the effective values used for totalPages.
func ClampPageLimit(page, limit int) (int, int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if page <= 0 {
		page = 1
	}
	return page, limit
}

// ErrUnsortableField is returned by normalize when the caller requests sorting on a
// field that has no physical indexed column (RM-09: text/number fields stored only in
// JSON with no generated column). The service maps this to HTTP 422 SORT_NOT_ALLOWED.
var ErrUnsortableField = errors.New("field is not sortable (no physical indexed column)")

// normalize clamps pagination to safe bounds and resolves a safe sort column/direction.
// When SortAllowlist is set (service provides it for dynamic entity sorts), it uses that
// map; otherwise falls back to the built-in system-column allowlist.
// Returns ErrUnsortableField when p.Sort is non-empty but not in any allowlist.
func (p ListParams) normalize() (limit, offset int, sortCol, order string, err error) {
	page, limit := ClampPageLimit(p.Page, p.Limit)
	offset = (page - 1) * limit

	allowlist := p.SortAllowlist
	if allowlist == nil {
		allowlist = sortColumns
	}

	if p.Sort == "" {
		sortCol = "created_at"
	} else if col, ok := allowlist[p.Sort]; ok {
		sortCol = col
	} else {
		// Unknown sort field — 422 instead of silent fallback (RM-09).
		return 0, 0, "", "", ErrUnsortableField
	}

	order = "DESC"
	if p.Order == "asc" || p.Order == "ASC" {
		order = "ASC"
	}
	return limit, offset, sortCol, order, nil
}

// tableFor returns the sanitized obj_* table name for an entity, or an error.
func tableFor(entity string) (string, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return "", err
	}
	return "obj_" + name, nil
}

// ErrCommitOutcomeUnknown marks an InsertRecord failure AT COMMIT: the driver reported an error
// (lost connection, timeout, cancelled context) but MySQL may still have applied the transaction,
// so the row may exist. Every earlier failure is certain — nothing committed. A caller that must
// not repeat a write (the Idempotency-Key claim, specs/047) keeps its lock on this error instead
// of releasing it.
var ErrCommitOutcomeUnknown = errors.New("commit outcome unknown")

type presetRecordIDKey struct{}

// WithRecordID returns ctx carrying the id InsertRecord gives the row instead of minting one. The
// Idempotency-Key claim (specs/047 FR-11) records it BEFORE the insert, so a later request can
// check whether a create whose process died ever committed. The id must be server-generated.
func WithRecordID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, presetRecordIDKey{}, id)
}

// RecordIDFrom returns the id WithRecordID put on ctx, or "" (InsertRecord then mints one).
func RecordIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(presetRecordIDKey{}).(string)
	return id
}

// InsertRecord stores a new record and returns its generated id. Dynamic values go into the
// JSON `data` column; only `?` placeholders carry values. created_by is the VERIFIED user id
// (auth.Subject.UserID), and the record's `team_set_id` is derived from that creator's team
// memberships (A5, §B4): the set is resolved-or-created (deduped by team_md5) so an
// access_level=team viewer who shares any of the creator's teams can later see the row. A
// creator on no team leaves team_set_id NULL (owner-only visibility). The set resolution and
// the record insert run in ONE transaction so a record is never left pointing at a half-built
// set.
func (r *Repository) InsertRecord(ctx context.Context, entity, tenantID, userID string, data map[string]any, uniqueFields []UniqueFieldSpec) (string, error) {
	table, err := tableFor(entity)
	if err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: %w", err)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: marshal data: %w", err)
	}

	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: begin tx: %w", err)
	}
	// Roll back on any error path; a committed tx makes the rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	// Resolve the creator's team_set (A5 §B4). A user on no team → NULL team_set_id
	// (sql.NullString zero value), so the row is visible only to its owner / all / admin.
	var teamSetID sql.NullString
	if userID != "" {
		teamIDs, terr := listUserTeamIDs(ctx, tx, tenantID, userID)
		if terr != nil {
			return "", fmt.Errorf("dynrepo.InsertRecord: %w", terr)
		}
		if len(teamIDs) > 0 {
			setID, serr := resolveOrCreateTeamSet(ctx, tx, tenantID, teamIDs)
			if serr != nil {
				return "", fmt.Errorf("dynrepo.InsertRecord: %w", serr)
			}
			teamSetID = sql.NullString{String: setID, Valid: true}
		}
	}

	id := RecordIDFrom(ctx)
	if id == "" {
		id = uuid.NewString()
	}
	// owner_id starts equal to created_by (TRỤ 2 mục 2 "Giao việc & đổi chủ" — the record's
	// owner defaults to its creator until reassigned via SetOwner). Both columns carry the
	// SAME verified userID.
	stmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `team_set_id`, `created_by`, `owner_id`, `data`) VALUES (?, ?, ?, ?, ?, ?)", table)
	if _, err := tx.ExecContext(ctx, stmt, id, tenantID, teamSetID, userID, userID, payload); err != nil {
		if idxName, ok := duplicateIndexName(err); ok {
			return "", fmt.Errorf("dynrepo.InsertRecord: %w", &DuplicateUniqueError{IndexName: idxName})
		}
		// W5-B4: a value that overflows / mistypes a generated column (1264/1406/1292/1366)
		// is a 422-class input error, not a 500 — classify it for the service layer.
		if dce, ok := asDataConstraintError(err); ok {
			return "", fmt.Errorf("dynrepo.InsertRecord: %w", dce)
		}
		return "", fmt.Errorf("dynrepo.InsertRecord: exec: %w", err)
	}
	// RM-28: dual-write the uniqueness pivot in the SAME tx. Value is read back from each unique
	// field's gencol (§3.2) — a uq_pivot 1062 (or the still-active obj_ index) rolls back the whole
	// insert via the deferred rollback.
	if err := insertPivotForRecord(ctx, tx, table, tenantID, entityName, id, uniqueFields); err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: %w", err)
	}
	// specs/064 S9-4: a sample row is marked in the SAME tx — a failing mark rolls the row back.
	if err := insertSampleMark(ctx, tx, tenantID, entityName, id, userID); err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("dynrepo.InsertRecord: commit: %w: %w", ErrCommitOutcomeUnknown, err)
	}
	return id, nil
}

// recordRow is the scan target; data arrives as raw JSON bytes. CreatedBy backs the workflow
// engine's "$owner" resolution (see Record.CreatedBy) — NOT every SELECT list needs it, but
// GetRecord and ListRecords (the two reads that feed workflow Events, directly or through the
// scheduler's lister) always fetch it so the column is cheap (indexed, on every obj_* row).
type recordRow struct {
	ID        string    `db:"id"`
	TenantID  string    `db:"tenant_id"`
	Data      []byte    `db:"data"`
	Version   int       `db:"version"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
	CreatedBy string    `db:"created_by"`
	// OwnerID is NULLable: rows written before the owner column existed, and any row whose
	// owner was never reassigned away from its creator, carry NULL. toRecord folds the
	// COALESCE so no caller has to remember it.
	OwnerID sql.NullString `db:"owner_id"`
	// SharedOnly is selected only by GetRecord on a share-armed read (sharedOnlyExpr); every other
	// query leaves it false.
	SharedOnly bool `db:"shared_only"`
}

func (row recordRow) toRecord() (Record, error) {
	rec := Record{ID: row.ID, TenantID: row.TenantID, Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CreatedBy: row.CreatedBy, SharedWithMe: row.SharedOnly}
	// The EFFECTIVE owner, resolved in ONE place. The ACL row filter has always read
	// COALESCE(owner_id, created_by); every other consumer read created_by and therefore
	// disagreed with it the moment a record was reassigned (2026-07-29 finding: "Đổi chủ"
	// changed who may edit while the screen, the $owner notification target and the
	// create_record owner all kept pointing at the creator). Folding it here means a caller
	// cannot pick the wrong one.
	if row.OwnerID.Valid && row.OwnerID.String != "" {
		rec.OwnerID = row.OwnerID.String
	} else {
		rec.OwnerID = row.CreatedBy
	}
	if len(row.Data) > 0 {
		if err := json.Unmarshal(row.Data, &rec.Data); err != nil {
			return Record{}, fmt.Errorf("dynrepo: unmarshal data: %w", err)
		}
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	return rec, nil
}

// GetRecord returns a single active record scoped by tenant AND row-level access, or
// ErrRecordNotFound. scope is the resolved A5 row-level access (owner/team/all/none, plus the
// verified user id); a row the scope may not see surfaces as ErrRecordNotFound (never as a
// cross-team/cross-tenant leak, and never distinguishable from a non-existent id). tenant_id
// always leads the predicate; the scope fragment and its args are appended after it.
func (r *Repository) GetRecord(ctx context.Context, entity, tenantID, id string, scope RowScope) (*Record, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.GetRecord: %w", err)
	}
	if scope, err = r.resolveReadScope(ctx, tenantID, entity, scope); err != nil {
		return nil, fmt.Errorf("dynrepo.GetRecord: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	// DAILY-QUEUE #223: on a share-armed read, also say whether the row is visible ONLY through the
	// share (Record.SharedWithMe). Its args precede the WHERE args — the column comes first in the SQL.
	sharedCol, args := "", []any{}
	if expr, exprArgs := scope.sharedOnlyExpr(); expr != "" {
		sharedCol, args = ", "+expr+" AS `shared_only`", exprArgs
	}
	stmt := fmt.Sprintf(
		"SELECT `id`, `tenant_id`, `data`, `version`, `created_at`, `updated_at`, `created_by`, `owner_id`%s FROM `%s` "+
			"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s", sharedCol, table, scopeSQL)
	args = append(append(args, tenantID, id), scopeArgs...)
	var row recordRow
	if err := r.db.GetContext(ctx, &row, stmt, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, fmt.Errorf("dynrepo.GetRecord: %w", err)
	}
	rec, err := row.toRecord()
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// escapeLikeWildcards escapes the LIKE wildcard characters (%, _, \) in a user-supplied
// search term so they are treated as literals when used with LIKE ? ESCAPE '\\'. This is
// a match-correctness measure (the value already travels as a `?` placeholder — it is NOT
// an injection defense). The backslash escape character is doubled first to prevent
// double-escaping the subsequent % and _ replacements.
func escapeLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// jsonPath returns the SQL expression for reading a dynamic field value from the data
// JSON column. When PhysicalColumn is set on the filter the generated column is targeted
// directly (avoids JSON_UNQUOTE/EXTRACT overhead and uses the index); otherwise the
// standard JSON path expression is built using the sanitized field name.
//
// subPath, when non-empty, appends a nested key to the JSON path — used for currency
// fields whose value is stored as {"amount":N,...}, so "$.field.amount" extracts the
// numeric scalar. subPath is a service-supplied literal (not client input); it contains
// only lowercase ASCII letters (e.g. "amount", "currencyCode") — no user data is ever
// interpolated.
//
// The returned expression is already safe to inline into SQL because it is either a
// backtick-quoted physical column name derived from SanitizeIdentifier, or the
// JSON_UNQUOTE/JSON_EXTRACT template whose only user-supplied parts (col, subPath) are
// validated literals. No raw client input is ever interpolated here.
func jsonPath(physicalCol, col, subPath string) string {
	return jsonPathAliased("", physicalCol, col, subPath)
}

// jsonPathAliased is jsonPath with the table qualified by alias, so a joined obj_* table's
// `data` / generated column can be referenced unambiguously (REPORT-JOIN). alias="" reproduces
// jsonPath exactly (unqualified) for the single-table path. alias is a server-generated
// identifier ("base"/"j0"), never client input.
func jsonPathAliased(alias, physicalCol, col, subPath string) string {
	q := "`data`"
	if alias != "" {
		q = "`" + alias + "`.`data`"
	}
	if physicalCol != "" {
		if alias != "" {
			return "`" + alias + "`.`" + physicalCol + "`"
		}
		return "`" + physicalCol + "`"
	}
	if subPath != "" {
		return fmt.Sprintf("JSON_UNQUOTE(JSON_EXTRACT(%s, '$.%s.%s'))", q, col, subPath)
	}
	return fmt.Sprintf("JSON_UNQUOTE(JSON_EXTRACT(%s, '$.%s'))", q, col)
}

// castExpr wraps expr in the appropriate CAST when a flag is set:
//   - CastNumeric → CAST(... AS DECIMAL(38,10)) — arithmetic comparison for number/currency fields.
//   - CastDatetime → CAST(... AS DATETIME)        — instant-based ordering for date/datetime fields.
//
// Without these casts, JSON_UNQUOTE returns a string, so "10" < "9" lexicographically and
// datetime offsets ("...+07:00" vs "...Z") sort incorrectly.
func castExpr(expr string, castNumeric, castDatetime bool) string {
	if castNumeric {
		return "CAST(" + expr + " AS DECIMAL(38,10))"
	}
	if castDatetime {
		return "CAST(" + expr + " AS DATETIME)"
	}
	return expr
}

// buildPredicates turns the resolved filters and search into extra WHERE fragments plus
// their placeholder args. Each filter column and each search column is re-sanitized here as
// a defense-in-depth check (the service already validates against sys_fields) before it is
// inlined into the JSON path; every value travels as a `?` placeholder. The leading
// `tenant_id`/`deleted_at` predicates are added by the caller.
//
// Operator semantics (all values are `?` placeholders — no string concatenation of input):
//   - eq/neq: single value; text/picklist/email use COLLATE utf8mb4_unicode_ci (CaseInsensitive).
//   - gt/gte/lt/lte: single value; number fields get CAST to DECIMAL (CastNumeric);
//     date/datetime get CAST to DATETIME (CastDatetime).
//   - in: one placeholder per element (len(Values) ≥ 1); text types use COLLATE.
//   - between: exactly two Values ([lo, hi]); number/date/datetime fields get CAST.
//   - isnull/notnull: no value placeholder.
//   - contains: LIKE ? ESCAPE '\\' with value wrapped %term% and wildcards escaped;
//     always case-insensitive via COLLATE utf8mb4_unicode_ci.
//
// Case-insensitivity (S4): instead of LOWER(col)=LOWER(?), we emit
// col COLLATE utf8mb4_unicode_ci = ? and pass the value unmodified. The DB handles
// case folding uniformly, avoiding Go/MySQL disagreement on non-ASCII (e.g. Vietnamese).
func buildPredicates(p ListParams) (string, []any, error) {
	return buildPredicatesAliased(p, "")
}

// buildPredicatesAliased is buildPredicates with every column qualified by the given table
// alias (REPORT-JOIN). alias="" reproduces buildPredicates exactly. In v1, report filters/search
// apply to the BASE entity only, so the report join path passes the base alias here; the joined
// entity contributes only its group-by / metric field (resolved separately with its own alias).
func buildPredicatesAliased(p ListParams, alias string) (string, []any, error) {
	var clauses []string
	var args []any

	for _, f := range p.Filters {
		// Defense-in-depth: re-sanitize the column name even though the service already
		// validated it against sys_fields. When PhysicalColumn is set, it was built by
		// the service from SanitizeIdentifier output — still re-check it.
		col, err := ddl.SanitizeIdentifier(f.Column)
		if err != nil {
			return "", nil, fmt.Errorf("dynrepo: filter column %q: %w", f.Column, err)
		}

		// If a physical generated column is referenced, sanitize that name too.
		physCol := ""
		if f.PhysicalColumn != "" {
			pc, err := ddl.SanitizeIdentifier(f.PhysicalColumn)
			if err != nil {
				return "", nil, fmt.Errorf("dynrepo: filter physical column %q: %w", f.PhysicalColumn, err)
			}
			physCol = pc
		}

		// Effective operator (empty string → eq for backward compat with existing callers).
		op := f.Operator
		if op == "" {
			op = FilterEq
		}

		// JSONSubPath is a service-supplied literal (e.g. "amount" for currency fields).
		// It is NOT client input — the service hardcodes it based on field type.
		subPath := f.JSONSubPath

		colExpr := jsonPathAliased(alias, physCol, col, subPath)
		castColExpr := castExpr(colExpr, f.CastNumeric, f.CastDatetime)

		// Resolve operand values: prefer Values slice; fall back to single Value field for
		// backward-compatible callers that set only f.Value.
		values := f.Values
		if len(values) == 0 && f.Value != "" {
			values = []string{f.Value}
		}

		switch op {
		case FilterEq, FilterNeq:
			if len(values) == 0 {
				return "", nil, fmt.Errorf("dynrepo: operator %s on %q requires a value", op, col)
			}
			v := values[0]
			var clause string
			if f.CaseInsensitive {
				// S4: use COLLATE utf8mb4_unicode_ci — DB handles case folding uniformly.
				// Value passes as-is (unmodified); no Go-side ToLower needed.
				if op == FilterEq {
					clause = fmt.Sprintf("%s COLLATE utf8mb4_unicode_ci = ?", colExpr)
				} else {
					clause = fmt.Sprintf("%s COLLATE utf8mb4_unicode_ci != ?", colExpr)
				}
			} else {
				if op == FilterEq {
					clause = fmt.Sprintf("%s = ?", castColExpr)
				} else {
					clause = fmt.Sprintf("%s != ?", castColExpr)
				}
			}
			args = append(args, v)
			clauses = append(clauses, clause)

		case FilterGt, FilterGte, FilterLt, FilterLte:
			if len(values) == 0 {
				return "", nil, fmt.Errorf("dynrepo: operator %s on %q requires a value", op, col)
			}
			var sym string
			switch op {
			case FilterGt:
				sym = ">"
			case FilterGte:
				sym = ">="
			case FilterLt:
				sym = "<"
			default:
				sym = "<="
			}
			clauses = append(clauses, fmt.Sprintf("%s %s ?", castColExpr, sym))
			args = append(args, values[0])

		case FilterBetween:
			if len(values) != 2 {
				return "", nil, fmt.Errorf("dynrepo: between on %q requires exactly 2 values, got %d", col, len(values))
			}
			clauses = append(clauses, fmt.Sprintf("%s BETWEEN ? AND ?", castColExpr))
			args = append(args, values[0], values[1])

		case FilterIn:
			if len(values) == 0 {
				return "", nil, fmt.Errorf("dynrepo: in on %q requires at least one value", col)
			}
			placeholders := strings.Repeat("?,", len(values))
			placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
			var clause string
			if f.CaseInsensitive {
				// S4: COLLATE on the column expression; values pass unmodified.
				clause = fmt.Sprintf("%s COLLATE utf8mb4_unicode_ci IN (%s)", colExpr, placeholders)
			} else {
				clause = fmt.Sprintf("%s IN (%s)", castColExpr, placeholders)
			}
			for _, v := range values {
				args = append(args, v)
			}
			clauses = append(clauses, clause)

		case FilterIsNull:
			clauses = append(clauses, fmt.Sprintf("%s IS NULL", colExpr))

		case FilterIsNotNull:
			clauses = append(clauses, fmt.Sprintf("%s IS NOT NULL", colExpr))

		case FilterContains:
			if len(values) == 0 {
				return "", nil, fmt.Errorf("dynrepo: contains on %q requires a value", col)
			}
			escaped := escapeLikeWildcards(values[0])
			needle := "%" + escaped + "%"
			// Contains is always case-insensitive via COLLATE utf8mb4_unicode_ci (S4).
			// The needle travels as a `?` placeholder — injection defense comes from
			// parameterization; wildcard escaping is a match-correctness measure.
			clauses = append(clauses, fmt.Sprintf("%s COLLATE utf8mb4_unicode_ci LIKE ? ESCAPE '\\\\'", colExpr))
			args = append(args, needle)

		default:
			return "", nil, fmt.Errorf("dynrepo: unknown filter operator %q on %q", op, col)
		}
	}

	if p.Search != nil && len(p.Search.Columns) > 0 {
		var ors []string
		needle := "%" + escapeLikeWildcards(p.Search.Term) + "%"
		for _, c := range p.Search.Columns {
			col, err := ddl.SanitizeIdentifier(c)
			if err != nil {
				return "", nil, fmt.Errorf("dynrepo: search column %q: %w", c, err)
			}
			// Search uses COLLATE utf8mb4_unicode_ci for uniform case-insensitivity (S4).
			ors = append(ors,
				fmt.Sprintf("%s COLLATE utf8mb4_unicode_ci LIKE ? ESCAPE '\\\\'", jsonPathAliased(alias, "", col, "")))
			args = append(args, needle)
		}
		if len(ors) > 0 {
			clauses = append(clauses, "("+strings.Join(ors, " OR ")+")")
		}
	}

	if len(clauses) == 0 {
		return "", nil, nil
	}
	return " AND " + strings.Join(clauses, " AND "), args, nil
}

// ListRecords returns a tenant-scoped, row-level-scoped page of active records plus the total
// count. Optional JSON field filters (?field=value) and a substring search (?q) narrow both
// the count and the page identically so totalPages stays consistent with the returned rows.
// scope is the resolved A5 row-level access (owner/team/all/none); its predicate is applied
// IDENTICALLY to the count and the page query so a user never sees a total that includes rows
// they cannot read. tenant_id always leads; scope and filter/search predicates follow.
func (r *Repository) ListRecords(ctx context.Context, entity, tenantID string, scope RowScope, p ListParams) ([]Record, int, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: %w", err)
	}
	limit, offset, sortCol, order, err := p.normalize()
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: %w", err)
	}

	predSQL, predArgs, err := buildPredicates(p)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: %w", err)
	}

	// Row-level scope predicate (created_by/team) applied to BOTH count and page so the
	// total never over-reports rows the principal cannot see. Args order: tenant, scope, preds.
	// The live list is a read and honors manual shares (specs/051); the recycle bin does not — a
	// share is for viewing a live record, not for browsing what its owner threw away.
	if p.OnlyDeleted {
		scope, err = r.resolveScope(ctx, tenantID, scope)
	} else {
		scope, err = r.resolveReadScope(ctx, tenantID, entity, scope)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()

	// W9b: `deleted_at IS NULL` (live) OR `IS NOT NULL` (recycle bin) — same fragment on the
	// count and the page so totalPages matches the returned rows in both modes.
	delSQL := softDeletePredicate(p.OnlyDeleted)
	// specs/064 S9-4: the scheduler's list leaves marked sample rows out of count AND page; its
	// args come after the predicate args in both statements.
	if p.ExcludeSamples {
		entityName, nerr := entityNameForPivot(entity)
		if nerr != nil {
			return nil, 0, fmt.Errorf("dynrepo.ListRecords: %w", nerr)
		}
		sampleSQL, sampleArgs := excludeSamplesClause(table, tenantID, entityName)
		predSQL += sampleSQL
		predArgs = append(predArgs, sampleArgs...)
	}

	var total int
	countStmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND %s%s%s", table, delSQL, scopeSQL, predSQL)
	countArgs := append([]any{tenantID}, scopeArgs...)
	countArgs = append(countArgs, predArgs...)
	if err := r.db.GetContext(ctx, &total, countStmt, countArgs...); err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: count: %w", err)
	}

	// sortCol/order are from allowlists (normalize); filter/search columns are sanitized in
	// buildPredicates; scope uses placeholders; only values use placeholders.
	// created_by is selected here so the workflow scheduler's RecordLister (which calls
	// ListRecords under an admin scope, see WorkflowRecordLister.ListRecordsForScheduler) can
	// populate ScheduledRecord.CreatedBy — the source the engine resolves the "$owner"
	// notification sentinel from for scheduled rules (M7 MED-1 fix).
	// Tie-break on the primary key so the ordering is TOTAL. `ORDER BY <non-unique col>` leaves
	// tied rows in an undefined order, and OFFSET paging re-runs the query per page: two pages can
	// then hand back the same row twice and skip another. Real exposure on this data — 10 of 14
	// leads in the demo tenant share one `created_at` (TIMESTAMP is second-resolution, and a seed
	// or an import writes many rows inside one second).
	//
	// This is a missing GUARANTEE, not a failure observed today: repeated 3-page reads on that
	// table came back clean, because the plan happens to be stable at this size. The cursor path
	// has always tie-broken on id (see its ORDER BY doc ~L1353) — the two paging modes simply
	// disagreed about whether row order was defined.
	//
	// Free in practice: idx_created_at / idx_updated_at are already (tenant_id, deleted_at, <col>,
	// id), so the index supplies this order and EXPLAIN is byte-identical with and without it.
	// Skipped when sorting BY id, where it would be a no-op duplicate term.
	tieBreak := ""
	if sortCol != "id" {
		tieBreak = fmt.Sprintf(", `id` %s", order)
	}
	listStmt := fmt.Sprintf(
		"SELECT `id`, `tenant_id`, `data`, `version`, `created_at`, `updated_at`, `created_by`, `owner_id` FROM `%s` "+
			"WHERE `tenant_id` = ? AND %s%s%s ORDER BY `%s` %s%s LIMIT ? OFFSET ?",
		table, delSQL, scopeSQL, predSQL, sortCol, order, tieBreak)
	listArgs := append([]any{tenantID}, scopeArgs...)
	listArgs = append(listArgs, predArgs...)
	listArgs = append(listArgs, limit, offset)
	var rows []recordRow
	if err := r.db.SelectContext(ctx, &rows, listStmt, listArgs...); err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListRecords: select: %w", err)
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		rec, err := row.toRecord()
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, total, nil
}

// UpdateRecord applies an apply-when-present (partial-merge) change to a record's data
// (tenant-scoped + row-level scoped). The supplied `data` map holds ONLY the fields present in
// the PUT body (validated upstream); they are merged into the stored JSON via MySQL
// JSON_MERGE_PATCH so fields absent from the request are preserved untouched. COALESCE guards a
// NULL `data` column (records inserted with no values) by treating it as an empty object before
// the merge. An empty merge map is a valid no-op on the data while still bumping modified_by.
// Returns ErrRecordNotFound if no active record matched — including a record the scope may not
// see (A5: a user cannot edit a row outside their owner/team visibility).
//
// RM-24 optimistic locking (OPT-IN): when expectedVersion is non-nil the UPDATE carries an
// extra `version = ?` predicate, so a concurrent writer that already advanced the version
// loses the race deterministically (its UPDATE matches 0 rows). On a 0-row result the method
// probes the row's live version + deleted_at to disambiguate: a missing / soft-deleted row →
// ErrRecordNotFound (404); a live row at a different version → *VersionConflictError carrying
// the current version (→ 409).
//
// W5-C2: `version = version + 1` is bumped on BOTH paths (with and without expectedVersion) —
// every successful UPDATE advances the version. This closes the legacy false-safety hole: a
// writer that omits If-Match used to leave the version untouched, so a stale If-Match="0" from
// a concurrent writer would still match and silently clobber the legacy write. Now the legacy
// write advances 0→1 and the stale conditional write correctly 409s. Because the version always
// changes, a legacy UPDATE that matches the row ALWAYS reports RowsAffected>0 (even for an
// identical-data merge), so on the legacy path a 0-row result now means only not-found /
// out-of-scope; the no-op existence probe below is retained purely as a defensive fallback.
func (r *Repository) UpdateRecord(ctx context.Context, entity, tenantID, id, userID string, scope RowScope, data map[string]any, expectedVersion *int, uniqueFields []UniqueFieldSpec) error {
	table, err := tableFor(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: %w", err)
	}
	patch, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: marshal data: %w", err)
	}
	if scope, err = r.resolveScope(ctx, tenantID, scope); err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()

	// Build the SET clause + WHERE predicate. W5-C2: `version = version + 1` is ALWAYS in
	// the SET clause — BOTH the optimistic-lock branch AND the legacy (no If-Match) branch
	// bump the version. Only the WHERE predicate is conditional: the optimistic-lock branch
	// adds `AND version = ?` so the bump and the guard are one atomic statement (no
	// SELECT-then-UPDATE → no TOCTOU). A legacy writer that omits If-Match still advances the
	// version 0→1, so a concurrent writer holding a now-stale If-Match="0" correctly loses the
	// race (→ 409) instead of silently clobbering the legacy writer's change (false-safety).
	// args order must match the SET/WHERE column order.
	var stmt string
	var args []any
	if expectedVersion != nil {
		stmt = fmt.Sprintf(
			"UPDATE `%s` SET `data` = JSON_MERGE_PATCH(COALESCE(`data`, JSON_OBJECT()), ?), `version` = `version` + 1, `modified_by` = ? "+
				"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL AND `version` = ?%s", table, scopeSQL)
		args = append([]any{patch, userID, tenantID, id, *expectedVersion}, scopeArgs...)
	} else {
		stmt = fmt.Sprintf(
			"UPDATE `%s` SET `data` = JSON_MERGE_PATCH(COALESCE(`data`, JSON_OBJECT()), ?), `version` = `version` + 1, `modified_by` = ? "+
				"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s", table, scopeSQL)
		args = append([]any{patch, userID, tenantID, id}, scopeArgs...)
	}
	// RM-28: the obj_ UPDATE and the pivot reconcile run in ONE repository-owned transaction
	// (UpdateRecord was autocommit before). The obj_ UPDATE goes FIRST so it takes the row X-lock
	// (invariant lock order: obj_ before sys_unique_index); the pivot is reconciled only when a row
	// was actually changed (RowsAffected > 0). A pivot 1062 rolls the obj_ UPDATE back via the
	// deferred rollback. The 0-row disambiguation probes below read INSIDE this tx so they see the
	// same snapshot as the UPDATE.
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		if idxName, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.UpdateRecord: %w", &DuplicateUniqueError{IndexName: idxName})
		}
		// W5-B4: classify a generated-column overflow/mistype (1264/1406/1292/1366) → 422.
		if dce, ok := asDataConstraintError(err); ok {
			return fmt.Errorf("dynrepo.UpdateRecord: %w", dce)
		}
		// RM-28: a transient deadlock/lock-wait on the obj_ UPDATE is retryable, not an opaque 500.
		if isWriteConflict(err) {
			return fmt.Errorf("dynrepo.UpdateRecord: %w", ErrWriteConflict)
		}
		return fmt.Errorf("dynrepo.UpdateRecord: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: rows affected: %w", err)
	}
	if n > 0 {
		// RM-28: reconcile the pivot for every unique field the patch TOUCHED — DELETE by
		// record_id+field then re-INSERT the value read back from the merged gencol (§3.4). Null-clear
		// and empty-string both fall out of the read-back. A collision rolls the whole UPDATE back.
		if err := reconcilePivotForRecord(ctx, tx, table, tenantID, entityName, id, data, uniqueFields); err != nil {
			return fmt.Errorf("dynrepo.UpdateRecord: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("dynrepo.UpdateRecord: commit: %w", err)
		}
		return nil
	}

	// RowsAffected == 0. With optimistic locking the predicate carried a version guard, so a
	// 0-row result can mean EITHER the row does not exist / is out of scope (404) OR the row
	// exists but its version no longer matches (a lost-update was prevented → 409). Probe the
	// SCOPE-AWARE live state to disambiguate. The probe reuses the same tenant + id + row-filter
	// as the UPDATE so a row outside the caller's visibility still surfaces as not-found.
	if expectedVersion != nil {
		curVersion, visible, perr := r.recordVersionInScope(ctx, tx, table, tenantID, id, scope)
		if perr != nil {
			return fmt.Errorf("dynrepo.UpdateRecord: %w", perr)
		}
		if !visible {
			return ErrRecordNotFound
		}
		// Live + in scope but the UPDATE matched 0 rows → the version differed (conflict).
		// (A genuine no-op identical-merge would still have matched the row, because the
		// version predicate held; so a 0-row result here is always a version mismatch.)
		return &VersionConflictError{Current: curVersion}
	}

	// Legacy path (no version supplied): RowsAffected == 0 is AMBIGUOUS in MySQL's default mode
	// (no CLIENT_FOUND_ROWS): EITHER the record does not exist / is outside this scope (a genuine
	// 404), OR the record exists and is visible but the JSON_MERGE_PATCH produced no actual change
	// (a PUT that re-sends identical values — a legitimate no-op that must return 200, NOT 404).
	// Disambiguate with a scope-aware existence probe using the SAME tenant + id + row-filter.
	visible, err := r.recordVisibleInScope(ctx, tx, table, tenantID, id, scope)
	if err != nil {
		return fmt.Errorf("dynrepo.UpdateRecord: %w", err)
	}
	if visible {
		// W5-C2 note: this branch is now DEAD in practice — `version = version + 1` is
		// unconditional in the SET clause (see above), so a matched row always reports
		// RowsAffected>0 and never reaches here. Kept as a defensive no-op (not deleted) in
		// case that invariant ever regresses.
		return nil // existed + in scope, merge changed nothing -> idempotent no-op (200)
	}
	return ErrRecordNotFound
}

// rowGetter is the minimal single-row read surface the 0-row disambiguation probes need
// (GetContext), satisfied by both *sqlx.DB and *sqlx.Tx. UpdateRecord passes its tx so the probes
// read the SAME snapshot as the UPDATE (RM-28 tx-wrap) instead of a separate autocommit read.
type rowGetter interface {
	GetContext(ctx context.Context, dest any, query string, args ...any) error
}

// recordVersionInScope reads a live record's current `version` (RM-24) AND whether it is
// visible under the supplied row-level scope, in one query. It mirrors the UpdateRecord WHERE
// clause (tenant_id + id + deleted_at + scope.rowFilter) so a row the scope may not mutate is
// reported as not-visible (version 0, visible=false) rather than leaking its version. Used only
// on the 0-row optimistic-lock path to tell a missing/soft-deleted/out-of-scope row (404) from a
// live row at a different version (409). `table` is the already-sanitized obj_* table name;
// tenant_id/id and all scope values travel as `?` placeholders.
func (r *Repository) recordVersionInScope(ctx context.Context, g rowGetter, table, tenantID, id string, scope RowScope) (version int, visible bool, err error) {
	scopeSQL, scopeArgs := scope.rowFilter()
	stmt := fmt.Sprintf(
		"SELECT `version` FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s",
		table, scopeSQL)
	args := append([]any{tenantID, id}, scopeArgs...)
	var v int
	if gerr := g.GetContext(ctx, &v, stmt, args...); gerr != nil {
		if errors.Is(gerr, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("recordVersionInScope: %w", gerr)
	}
	return v, true, nil
}

// recordVisibleInScope reports whether a live record with the given id exists for the tenant
// AND is visible under the supplied row-level scope. It mirrors the UpdateRecord/SoftDelete
// WHERE clause (tenant_id + id + deleted_at + scope.rowFilter) so "visible" here means exactly
// "a row this scope is allowed to mutate", never a cross-tenant or out-of-scope row. `table` is
// the already-resolved, sanitized obj_* table name; tenant_id/id and all scope values travel as
// `?` placeholders.
func (r *Repository) recordVisibleInScope(ctx context.Context, g rowGetter, table, tenantID, id string, scope RowScope) (bool, error) {
	scopeSQL, scopeArgs := scope.rowFilter()
	stmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s",
		table, scopeSQL)
	args := append([]any{tenantID, id}, scopeArgs...)
	var n int
	if err := g.GetContext(ctx, &n, stmt, args...); err != nil {
		return false, fmt.Errorf("recordVisibleInScope: %w", err)
	}
	return n > 0, nil
}

// SoftDeleteRecord marks a record deleted (tenant-scoped + row-level scoped) without removing
// the row. A record the scope may not see is left untouched and reported as ErrRecordNotFound
// (A5: a user cannot delete a row outside their owner/team visibility).
func (r *Repository) SoftDeleteRecord(ctx context.Context, entity, tenantID, id string, scope RowScope) error {
	table, err := tableFor(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: %w", err)
	}
	if scope, err = r.resolveScope(ctx, tenantID, scope); err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	stmt := fmt.Sprintf(
		"UPDATE `%s` SET `deleted_at` = NOW() "+
			"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s", table, scopeSQL)
	args := append([]any{tenantID, id}, scopeArgs...)
	// RM-28: obj_ soft-delete + pivot release run in ONE tx (was autocommit). obj_ UPDATE first
	// (X-lock), then release the record's unique slots. A 0-row UPDATE (not-found / out-of-scope)
	// touches no pivot and rolls back.
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		if isWriteConflict(err) {
			return fmt.Errorf("dynrepo.SoftDeleteRecord: %w", ErrWriteConflict)
		}
		return fmt.Errorf("dynrepo.SoftDeleteRecord: exec: %w", err)
	}
	if err := requireAffected(res, "dynrepo.SoftDeleteRecord"); err != nil {
		return err // ErrRecordNotFound — nothing changed, nothing to release
	}
	// The record left the live set (its gencols project NULL); free ALL its pivot rows so the
	// value is reusable, mirroring the gencol → NULL soft-delete semantics.
	if err := deletePivotByRecord(ctx, tx, tenantID, entityName, id); err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dynrepo.SoftDeleteRecord: commit: %w", err)
	}
	return nil
}

// RestoreRecord clears a soft-deleted record's `deleted_at` (tenant-scoped + row-level scoped),
// bringing it back to the live set. It is the inverse of SoftDeleteRecord and is authorized with
// the SAME "delete" grant (a caller who could not delete a row may not restore it). A record that
// is not currently soft-deleted, does not exist, belongs to another tenant, or lies outside the
// caller's row-scope is left untouched and reported as ErrRecordNotFound (→ 404) — the WHERE
// requires `deleted_at IS NOT NULL`, so restoring an already-live row matches 0 rows. userID is
// the verified restorer, recorded as modified_by. version is deliberately NOT bumped (mirrors
// SoftDeleteRecord, which does not bump it either).
//
// Unique-collision trap (W9b): a unique field's generated column is
// `CASE WHEN deleted_at IS NULL THEN <value> ELSE NULL END`, so a soft-deleted row projects NULL
// and frees its unique slot. If a LIVE row was created with the same value AFTER the delete, the
// restore recomputes the column back to <value> and trips MySQL ER_DUP_ENTRY (1062). We classify
// it as *DuplicateUniqueError so the service maps it to 409 RECORD_DUPLICATE with details.field
// (identical to a create/update collision) instead of an opaque 500.
func (r *Repository) RestoreRecord(ctx context.Context, entity, tenantID, id, userID string, scope RowScope, uniqueFields []UniqueFieldSpec) error {
	table, err := tableFor(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: %w", err)
	}
	if scope, err = r.resolveScope(ctx, tenantID, scope); err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	stmt := fmt.Sprintf(
		"UPDATE `%s` SET `deleted_at` = NULL, `modified_by` = ?, `updated_at` = `updated_at` "+
			"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NOT NULL%s", table, scopeSQL)
	args := append([]any{userID, tenantID, id}, scopeArgs...)
	// RM-28: obj_ restore + pivot re-claim run in ONE tx (was autocommit). obj_ UPDATE first
	// (X-lock, and the still-active unique index re-materializes the gencol so an obj_ 1062 fires
	// here), then re-INSERT the pivot from the CURRENT gencol of every unique field.
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		// A live row with the restored unique value now occupies the slot → 1062. Classify it so
		// the service returns 409 RECORD_DUPLICATE (details.field), not a 500.
		if idxName, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.RestoreRecord: %w", &DuplicateUniqueError{IndexName: idxName})
		}
		// Defense-in-depth: a generated-column overflow/mistype is a 422-class input error.
		if dce, ok := asDataConstraintError(err); ok {
			return fmt.Errorf("dynrepo.RestoreRecord: %w", dce)
		}
		if isWriteConflict(err) {
			return fmt.Errorf("dynrepo.RestoreRecord: %w", ErrWriteConflict)
		}
		return fmt.Errorf("dynrepo.RestoreRecord: exec: %w", err)
	}
	// RowsAffected == 0 is unambiguous here: the SET always changes deleted_at (NOT NULL → NULL),
	// so a matched row always reports 1. Zero rows means no matching soft-deleted, in-scope,
	// tenant-owned record → not-found (never the B1 identical-update no-op ambiguity).
	if err := requireAffected(res, "dynrepo.RestoreRecord"); err != nil {
		return err
	}
	// The record is back in the live set → re-claim its unique slots. Read-back the CURRENT gencol
	// (§3.3 restore): a value taken by a live row created during the delete window → pivot 1062 →
	// *DuplicateUniqueError → the obj_ restore rolls back too.
	if err := insertPivotForRecord(ctx, tx, table, tenantID, entityName, id, uniqueFields); err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dynrepo.RestoreRecord: commit: %w", err)
	}
	return nil
}

// SetOwner reassigns a record's CURRENT owner (tenant-scoped + row-level scoped) — TRỤ 2 mục 2
// "Giao việc & đổi chủ". It writes ONLY `owner_id` (+ `modified_by`); `created_by` is never
// touched (it stays the immutable creator — see rowscope.go's rowFilter doc and
// dynrepo.Record.CreatedBy). No pivot interaction: owner_id is not a unique field, so this is a
// plain autocommit UPDATE, unlike UpdateRecord/SoftDeleteRecord/RestoreRecord. `updated_at` is
// pinned (`= updated_at` stops the ON UPDATE auto-stamp): a reassignment is not a content edit, so
// it must not reset an inactivity timer anchored on the last edit (#287). Returns
// ErrRecordNotFound if no active record matched — including a record the scope may not see
// (the caller is expected to have already required access_level "all" or admin before calling
// this; the row-filter is still applied here as defense-in-depth, never bypassed).
func (r *Repository) SetOwner(ctx context.Context, entity, tenantID, id, userID string, scope RowScope, newOwnerID string) error {
	table, err := tableFor(entity)
	if err != nil {
		return fmt.Errorf("dynrepo.SetOwner: %w", err)
	}
	if scope, err = r.resolveScope(ctx, tenantID, scope); err != nil {
		return fmt.Errorf("dynrepo.SetOwner: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	stmt := fmt.Sprintf(
		"UPDATE `%s` SET `owner_id` = ?, `modified_by` = ?, `updated_at` = `updated_at` "+
			"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL%s", table, scopeSQL)
	args := append([]any{newOwnerID, userID, tenantID, id}, scopeArgs...)
	res, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		if isWriteConflict(err) {
			return fmt.Errorf("dynrepo.SetOwner: %w", ErrWriteConflict)
		}
		return fmt.Errorf("dynrepo.SetOwner: exec: %w", err)
	}
	return requireAffected(res, "dynrepo.SetOwner")
}

// SetComputedValues merges values (formula results the service recomputed after a formula field
// was added or edited) into one live row's `data`. It is a derived-value refresh, not an edit:
// `version`, `modified_by` and `updated_at` stay as they are (`updated_at` = itself stops the ON
// UPDATE auto-stamp), so no open form hits a false 409 and "last modified" keeps meaning a person.
// The write is guarded by the version the caller read: a user save landing in between wins and
// this returns false (also false when nothing matched), so the caller re-reads the row. Formula
// fields are never unique, so no pivot row is touched. Tenant-scoped; values travel as one `?`.
func (r *Repository) SetComputedValues(ctx context.Context, entity, tenantID, id string, version int, values map[string]any) (bool, error) {
	table, err := tableFor(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.SetComputedValues: %w", err)
	}
	patch, err := json.Marshal(values)
	if err != nil {
		return false, fmt.Errorf("dynrepo.SetComputedValues: marshal: %w", err)
	}
	stmt := fmt.Sprintf(
		"UPDATE `%s` SET `data` = JSON_MERGE_PATCH(COALESCE(`data`, JSON_OBJECT()), ?), `updated_at` = `updated_at` "+
			"WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL AND `version` = ?", table)
	res, err := r.db.ExecContext(ctx, stmt, patch, tenantID, id, version)
	if err != nil {
		if dce, ok := asDataConstraintError(err); ok {
			return false, fmt.Errorf("dynrepo.SetComputedValues: %w", dce)
		}
		if isWriteConflict(err) {
			return false, fmt.Errorf("dynrepo.SetComputedValues: %w", ErrWriteConflict)
		}
		return false, fmt.Errorf("dynrepo.SetComputedValues: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("dynrepo.SetComputedValues: rows affected: %w", err)
	}
	return n > 0, nil
}

// requireAffected maps a zero-rows update to ErrRecordNotFound so callers can return 404.
func requireAffected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return ErrRecordNotFound
	}
	return nil
}

// RecordExists reports whether a live (non-soft-deleted) record with the given id exists in
// the entity's obj_* table for the tenant. It is the FK-existence probe used by relation
// (1-N) record validation (M3-T2): a child's `<f>_id` must point to a real parent record in
// the SAME tenant, so the lookup is tenant-scoped — a parent id belonging to another tenant
// (or a soft-deleted one) reports false, never a cross-tenant hit. The entity name is
// sanitized; tenant_id and id always travel as `?` placeholders.
func (r *Repository) RecordExists(ctx context.Context, entity, tenantID, id string) (bool, error) {
	table, err := tableFor(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.RecordExists: %w", err)
	}
	var n int
	stmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL", table)
	if err := r.db.GetContext(ctx, &n, stmt, tenantID, id); err != nil {
		return false, fmt.Errorf("dynrepo.RecordExists: %w", err)
	}
	return n > 0, nil
}

// ListRecordsCursor executes a keyset (cursor) paginated query and returns up to limit records
// plus an opaque nextCursor. There is NO COUNT(*) on any path. Cost depends on the sort column:
//   - NOT-NULL columns (created_at/updated_at/id — the common timeline cursor) backed by a composite
//     index (tenant_id, col, id): MySQL constant-folds `(col IS NULL)` away and, because the id
//     tie-break follows the sort direction, reads exactly limit rows via a forward/REVERSE index
//     scan — true O(limit) at any depth, no filesort (verified via EXPLAIN). The composite index is
//     mandatory: created_at/updated_at carry one in mandatoryIndexes; unique/relation fields get
//     (tenant_id, col)→(…, id) implicitly. Without the direction-following tie-break this fell to a
//     filesort over the whole tenant set for the DESC default (O(N) per page) — the O1 fix.
//   - NULLABLE columns (generated columns for optional business fields): the NULLS-LAST
//     `ORDER BY (col IS NULL) …` is a computed-expression prefix the index can't serve, so MySQL
//     filesorts the tenant-filtered set per page (no COUNT, but NOT strictly O(limit) at depth on
//     a large tenant). Correctness is unaffected (no gap/dup/loss) — this is a perf characteristic.
//
// # NULLS-LAST ordering (both directions)
//
// Nullable sort columns (generated columns for optional business fields) must not silently drop
// rows: `(sortCol, id) >/< (?, ?)` evaluates to NULL when sortCol IS NULL → excluded by WHERE.
// We use explicit NULLS-LAST ordering and a branched keyset predicate instead.
//
// ORDER BY (always):
//
//	ORDER BY (`sortCol` IS NULL) ASC, `sortCol` [ASC|DESC], `id` [ASC|DESC]
//
// "IS NULL" returns 0 for non-null rows, 1 for NULL rows; so non-null rows always sort first,
// then the NULL tail. The id tie-break FOLLOWS the sort direction (id DESC when order=DESC) so the
// ORDER BY is uniform-direction — a single index (tenant_id, sortCol, id) serves it via a forward
// or REVERSE scan with no filesort. A mixed `sortCol DESC, id ASC` would force a filesort over the
// whole tenant-filtered set (O(N) per page); following the direction restores O(limit) (O1).
//
// Keyset predicate when p.After != "" (all values bound via '?' — zero concatenation). idCmp is
// `>` for ASC, `<` for DESC — same direction as the ORDER BY tie-break:
//
//   - Boundary NON-null, ASC:
//     AND ( (`sortCol` IS NULL) OR (`sortCol` > ?) OR (`sortCol` = ? AND `id` > ?) )
//
//   - Boundary NON-null, DESC:
//     AND ( (`sortCol` IS NULL) OR (`sortCol` < ?) OR (`sortCol` = ? AND `id` < ?) )
//
//   - Boundary NULL (NULL tail, ordered id in `order` direction):
//     AND ( `sortCol` IS NULL AND `id` [>|<] ? )
//
// # Lookahead
//
// Fetch limit+1 rows; return limit; nextCursor from row[limit-1] (0-indexed) if extra row
// exists — avoids spurious non-null cursor when the result is exactly full-page-then-empty.
//
// # Identifier safety
//
// p.Sort is validated via ddl.SanitizeIdentifier before interpolation — defense-in-depth even
// though the service already checked it. All values (sortVal, id) travel as '?' only.
func (r *Repository) ListRecordsCursor(ctx context.Context, entity, tenantID string, scope RowScope, p CursorParams) ([]Record, string, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: %w", err)
	}

	// Clamp limit (same rules as offset mode).
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// Sanitize sort column as a defense-in-depth identifier check. p.Sort is already the
	// physical column name validated by the service, but we guard here too (mirrors GetRecord
	// and buildPredicates). Falls back to "created_at" on blank.
	sortCol := DefaultCursorSort
	if p.Sort != "" {
		safe, sanitErr := ddl.SanitizeIdentifier(p.Sort)
		if sanitErr != nil {
			return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: invalid sort column: %w", sanitErr)
		}
		sortCol = safe
	}

	order := "DESC"
	if p.Order == "ASC" || p.Order == "asc" {
		order = "ASC"
	}

	// Row-level scope (a read: honors manual shares when the scope opted in).
	if scope, err = r.resolveReadScope(ctx, tenantID, entity, scope); err != nil {
		return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()

	// Build filter/search predicates (same function as offset mode).
	listParamsForPreds := ListParams{Filters: p.Filters, Search: p.Search}
	predSQL, predArgs, err := buildPredicates(listParamsForPreds)
	if err != nil {
		return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: %w", err)
	}

	// D3 (collation-folding sort, reconciled empirically 2026-06-25): a keyset cursor on a
	// case/accent-INSENSITIVE text column does NOT lose rows, because the `id` tie-break below
	// FOLLOWS the sort direction and uses the SAME collation on both sides — ORDER BY (col, id)
	// and predicate `(col > ?) OR (col = ? AND id > ?)` both coerce `col` to its default ci
	// collation, so `(col_ci, id)` is a STRICT TOTAL ORDER and `id` (unique) absorbs every
	// collation-tie regardless of col's non-injectivity. Verified: an abc/ABC/Abc (all ci-equal)
	// limit-1 walk returns every row, ASC and DESC. The red-team "row-loss" repro was hand-SQL
	// whose ORDER BY omitted the matching `id` term — a model of the code, not the code. So NO
	// `COLLATE utf8mb4_bin` override is applied here: forcing byte-order would (a) change the
	// user-visible sort to codepoint order (scattering Vietnamese: "Ánh" far from "anh") and
	// (b) make cursor-mode disagree with offset-mode (which sorts ci), i.e. trade a non-bug for
	// a real regression. The invariant that keeps this correct: predicate collation == ORDER BY
	// collation (both bare `col` → both ci).

	// Keyset predicate — NULLS-LAST aware. All values bound via '?'; no string concatenation.
	// The id tie-break FOLLOWS the sort direction (idCmp) so the ORDER BY is uniform-direction and
	// an index (tenant_id, sortCol, id) can serve it via a forward/reverse scan with NO filesort —
	// restoring true O(limit) at depth, esp. for the DESC default timeline cursor (O1, 2026-06-25).
	var cursorSQL string
	var cursorArgs []any
	if p.After != "" {
		// Bound to the resolved (post-default, post-sanitize) sortCol + order: a token minted under
		// another sort/order is ErrCursorSortMismatch, never a keyset on the wrong column.
		isNull, sortVal, cursorID, decErr := DecodeCursor(p.After, sortCol, order)
		if decErr != nil {
			return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: %w", decErr)
		}

		// idCmp follows order so the tie-break direction matches the ORDER BY (kept consistent with
		// the `id <order>` clause below — a mismatch would cause page gaps/dupes, not just slowness).
		idCmp := ">"
		if order == "DESC" {
			idCmp = "<"
		}
		if isNull {
			// Boundary row had NULL sort value — we're in the NULL tail (ordered by id in `order`
			// direction). Include only NULL-sorted rows past the boundary id in that direction.
			cursorSQL = fmt.Sprintf(" AND (`%s` IS NULL AND `id` %s ?)", sortCol, idCmp)
			cursorArgs = []any{cursorID}
		} else {
			// Boundary row had a non-null sort value. Non-null rows come first; NULL rows all come
			// after any non-null boundary. Both sortCol and id comparisons follow `order` (idCmp).
			// sortCol uses its DEFAULT collation here, identical to the ORDER BY below — the shared
			// collation + unique `id` tie-break make the keyset a strict total order (D3 note above).
			cursorSQL = fmt.Sprintf(
				" AND ((`%s` IS NULL) OR (`%s` %s ?) OR (`%s` = ? AND `id` %s ?))",
				sortCol, sortCol, idCmp, sortCol, idCmp)
			cursorArgs = []any{sortVal, sortVal, cursorID}
		}
	}

	// CAST(`sortCol` AS CHAR) → prevents parseTime=true from returning a time.Time for
	// TIMESTAMP/DATETIME columns (which would format as RFC3339Nano — MySQL can't compare
	// that back in the keyset predicate). For SQL NULL, CAST(NULL AS CHAR) stays NULL, so
	// sqlx scans it as a nil []byte — that nil is how we detect NULL in the cursor encoder.
	//
	// ORDER BY (`sortCol` IS NULL) ASC → 0 for non-null, 1 for NULL → NULLS-LAST in both
	// directions. The sort term uses sortCol's DEFAULT collation — identical to the keyset
	// predicate above — and the `id` tie-break follows `order`, so (col, id) is a strict total
	// order served by the (tenant_id, col, id) index without filesort.
	//
	// `created_by`, `owner_id` are selected exactly as ListRecords/GetRecord do, so toRecord folds
	// the SAME effective ownerId: without them a cursor row silently lost `ownerId` while the offset
	// row for the same id carried it (specs/039-cursor-bootstrap §6).
	listStmt := fmt.Sprintf(
		"SELECT `id`, `tenant_id`, `data`, `version`, `created_at`, `updated_at`, `created_by`, `owner_id`, CAST(`%s` AS CHAR) AS `__cursor_val` FROM `%s` "+
			"WHERE `tenant_id` = ? AND `deleted_at` IS NULL%s%s%s "+
			"ORDER BY (`%s` IS NULL) ASC, `%s` %s, `id` %s LIMIT ?",
		sortCol, table, scopeSQL, predSQL, cursorSQL, sortCol, sortCol, order, order)

	// Args order: tenant, scope, pred, cursor, limit.
	listArgs := append([]any{tenantID}, scopeArgs...)
	listArgs = append(listArgs, predArgs...)
	listArgs = append(listArgs, cursorArgs...)
	listArgs = append(listArgs, limit+1)

	var rows []recordRowCursor
	if err := r.db.SelectContext(ctx, &rows, listStmt, listArgs...); err != nil {
		return nil, "", fmt.Errorf("dynrepo.ListRecordsCursor: select: %w", err)
	}

	// Determine nextCursor using the lookahead row.
	var nextCursor string
	if len(rows) > limit {
		// Extra row exists → more pages. Encode cursor from rows[limit-1] (last returned row).
		lastRow := rows[limit-1]
		isNullSort := lastRow.CursorVal == nil // nil []byte means SQL NULL, NOT empty string
		nextCursor = EncodeCursor(sortCol, order, isNullSort, string(lastRow.CursorVal), lastRow.ID)
		rows = rows[:limit]
	}

	// Convert to Record.
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		rec, convErr := row.recordRow.toRecord()
		if convErr != nil {
			return nil, "", convErr
		}
		records = append(records, rec)
	}
	return records, nextCursor, nil
}

// BatchRecordExists checks in ONE query per entity whether a set of record ids are all live
// (non-soft-deleted) in the given entity's table for the tenant (RM-11: N+1 elimination
// for relation FK validation). It returns a map from id → true for every id that exists
// and is not soft-deleted. Ids absent from the map (or mapped to false) do not exist.
//
// Tenant-wide on purpose: this is the SYSTEM probe (workflow field_update, import id check, FK
// integrity), which must see any record of the tenant. A probe made on behalf of a user's write
// must use BatchRecordExistsInScope.
func (r *Repository) BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error) {
	return r.BatchRecordExistsInScope(ctx, entity, tenantID, ids, AdminScope())
}

// BatchRecordExistsInScope is BatchRecordExists narrowed to the rows the caller's row scope may
// VIEW (owner / team / manual share — the same predicate GetRecord applies), so "not visible" is
// indistinguishable from "does not exist" (queue #331: a relation FK may only point at a record the
// writer may see). The query uses a parameterized IN clause (one `?` per id) — no string
// concatenation; tenant_id always leads the predicate. An empty ids slice returns immediately.
func (r *Repository) BatchRecordExistsInScope(ctx context.Context, entity, tenantID string, ids []string, scope RowScope) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchRecordExists: %w", err)
	}
	if scope, err = r.resolveReadScope(ctx, tenantID, entity, scope); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchRecordExists: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	// Build parameterized IN placeholders.
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	stmt := fmt.Sprintf(
		"SELECT `id` FROM `%s` WHERE `tenant_id` = ? AND `id` IN (%s) AND `deleted_at` IS NULL%s",
		table, placeholders, scopeSQL)
	args := make([]any, 0, 1+len(ids)+len(scopeArgs))
	args = append(args, tenantID)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, scopeArgs...)
	var found []string
	if err := r.db.SelectContext(ctx, &found, stmt, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchRecordExists: %w", err)
	}
	result := make(map[string]bool, len(found))
	for _, id := range found {
		result[id] = true
	}
	return result, nil
}
