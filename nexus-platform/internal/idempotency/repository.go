// Package idempotency persists the Idempotency-Key claims of POST /:entity, /:entity/bulk and
// /:entity/import (sys_idempotency_keys, migrations 000069 + 000078 — specs/047). It is a thin
// tenant-pinned store: the claim/replay decisions live in services (RecordService.CreateIdempotent,
// runIdempotent). Identifiers are static
// (the fixed table); every value travels as a `?` placeholder and every statement is pinned to
// (tenant_id, principal_id, idem_key) — the claim's unique key.
package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Claim statuses. A claim is written as processing and flipped to completed (with the record id)
// once the create it guards has committed.
const (
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
)

// ErrKeyExists is returned by Claim when (tenant, principal, key) is already claimed — the
// unique-index violation IS the lock (MySQL ER_DUP_ENTRY 1062).
var ErrKeyExists = errors.New("idempotency: key already claimed")

// ErrNotFound is returned by Get when no claim exists for (tenant, principal, key).
var ErrNotFound = errors.New("idempotency: claim not found")

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY — a uq_idem_tenant_principal_key violation.
const mysqlDuplicateEntry = 1062

// Claim is one sys_idempotency_keys row. RecordID is the id the guarded create gives its record,
// chosen BEFORE the insert (specs/047 FR-11) so a stale processing claim can be reconciled against
// obj_*; it also fences Complete/Release to the request that owns the claim. Claims written before
// FR-11 carry no RecordID while processing. A partial-bulk claim (FR-12) carries no RecordID; an
// atomic-bulk / import claim (FR-13) carries the batch's base id in RecordID (row 0's id). Both keep,
// once completed, the Response they answered with — while processing, a leased import's Response is
// the plan saved before its transaction (SavePlan).
type Claim struct {
	TenantID    string
	PrincipalID string
	Key         string
	EntityName  string
	RequestHash string
	Status      string
	RecordID    string
	Response    []byte
	ExpiresAt   time.Time
}

// claimRow is the scan target of Get (record_id is NULL until completed).
type claimRow struct {
	TenantID    string         `db:"tenant_id"`
	PrincipalID string         `db:"principal_id"`
	Key         string         `db:"idem_key"`
	EntityName  string         `db:"entity_name"`
	RequestHash string         `db:"request_hash"`
	Status      string         `db:"status"`
	RecordID    sql.NullString `db:"record_id"`
	Response    []byte         `db:"response"`
	ExpiresAt   time.Time      `db:"expires_at"`
}

// SQLRepository is the sqlx-backed claim store.
type SQLRepository struct {
	db *sqlx.DB
}

// NewSQLRepository wires the store over an existing sqlx connection (the dynamic-record pool).
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db}
}

// Claim inserts c as a processing claim. It returns ErrKeyExists when the key is already claimed
// for (tenant, principal) — whatever the existing row's status or expiry; the caller reads it back
// with Get to decide.
func (r *SQLRepository) Claim(ctx context.Context, c Claim) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sys_idempotency_keys
			(id, tenant_id, principal_id, idem_key, entity_name, request_hash, status, record_id, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), c.TenantID, c.PrincipalID, c.Key, c.EntityName, c.RequestHash,
		StatusProcessing, sql.NullString{String: c.RecordID, Valid: c.RecordID != ""}, c.ExpiresAt.UTC())
	if err != nil {
		var me *gomysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
			return ErrKeyExists
		}
		return fmt.Errorf("idempotency.Claim: %w", err)
	}
	return nil
}

// Get reads the claim for (tenant, principal, key), or ErrNotFound.
func (r *SQLRepository) Get(ctx context.Context, tenantID, principalID, key string) (*Claim, error) {
	var row claimRow
	err := r.db.GetContext(ctx, &row, `
		SELECT tenant_id, principal_id, idem_key, entity_name, request_hash, status, record_id, response, expires_at
		FROM sys_idempotency_keys
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`,
		tenantID, principalID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("idempotency.Get: %w", err)
	}
	return &Claim{
		TenantID: row.TenantID, PrincipalID: row.PrincipalID, Key: row.Key, EntityName: row.EntityName,
		RequestHash: row.RequestHash, Status: row.Status, RecordID: row.RecordID.String, Response: row.Response,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Complete flips the processing claim holding recordID to completed. A claim that is no longer
// processing, or now holds another record id (taken over after its lease — see Takeover), is left
// untouched: only the request that owns the claim can complete it.
func (r *SQLRepository) Complete(ctx context.Context, tenantID, principalID, key, recordID string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE sys_idempotency_keys SET status = ?
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND status = ? AND record_id = ?`,
		StatusCompleted, tenantID, principalID, key, StatusProcessing, recordID); err != nil {
		return fmt.Errorf("idempotency.Complete: %w", err)
	}
	return nil
}

// CompleteResponse flips a processing bulk / import claim to completed and stores the response it
// answered with, for a resend to replay verbatim. recordID is the claim's base id ("" = a claim
// without a lease, which holds none — FR-12; a leased atomic bulk / import claim holds the batch's
// base id — FR-13): as in Complete, only the request that owns the claim can complete it.
func (r *SQLRepository) CompleteResponse(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE sys_idempotency_keys SET status = ?, response = ?
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND status = ? AND record_id <=> ?`,
		StatusCompleted, response, tenantID, principalID, key, StatusProcessing,
		sql.NullString{String: recordID, Valid: recordID != ""}); err != nil {
		return fmt.Errorf("idempotency.CompleteResponse: %w", err)
	}
	return nil
}

// SavePlan stores, on the processing claim holding recordID, the response its batch will answer
// with IF it commits — written BEFORE the batch's transaction (specs/047 FR-13), so that when the
// process dies around the commit, the resend that finds the batch committed can replay it (a CSV
// import's per-row refusals cannot be re-derived from the database). A claim that is no longer
// processing under that id (taken over) is left untouched.
func (r *SQLRepository) SavePlan(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE sys_idempotency_keys SET response = ?
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND status = ? AND record_id = ?`,
		response, tenantID, principalID, key, StatusProcessing, recordID); err != nil {
		return fmt.Errorf("idempotency.SavePlan: %w", err)
	}
	return nil
}

// Release deletes the processing claim holding recordID ("" = a bulk / import claim, which holds
// none), whose write failed before commit, so a resend runs again. A completed claim, or one taken
// over by another request, is never released.
func (r *SQLRepository) Release(ctx context.Context, tenantID, principalID, key, recordID string) error {
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM sys_idempotency_keys
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND status = ? AND record_id <=> ?`,
		tenantID, principalID, key, StatusProcessing, sql.NullString{String: recordID, Valid: recordID != ""}); err != nil {
		return fmt.Errorf("idempotency.Release: %w", err)
	}
	return nil
}

// Takeover hands a stale processing claim — one still holding oldRecordID — to a new attempt: it
// stores newRecordID and a fresh expiry, and drops the previous attempt's saved plan (SavePlan). It
// reports whether this call won; of two resends racing for the same stale claim, exactly one
// matches oldRecordID.
func (r *SQLRepository) Takeover(ctx context.Context, tenantID, principalID, key, oldRecordID, newRecordID string, expiresAt time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE sys_idempotency_keys SET record_id = ?, expires_at = ?, response = NULL
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND status = ? AND record_id = ?`,
		newRecordID, expiresAt.UTC(), tenantID, principalID, key, StatusProcessing, oldRecordID)
	if err != nil {
		return false, fmt.Errorf("idempotency.Takeover: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("idempotency.Takeover: rows affected: %w", err)
	}
	return n == 1, nil
}

// DeleteExpired deletes the claim for (tenant, principal, key) only if it expired before now — the
// janitor has not swept it yet and a fresh claim wants the slot. A live claim is never touched, so
// two racers both clearing the same expired row cannot delete each other's fresh claim.
func (r *SQLRepository) DeleteExpired(ctx context.Context, tenantID, principalID, key string, now time.Time) error {
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM sys_idempotency_keys
		WHERE tenant_id = ? AND principal_id = ? AND idem_key = ? AND expires_at < ?`,
		tenantID, principalID, key, now.UTC()); err != nil {
		return fmt.Errorf("idempotency.DeleteExpired: %w", err)
	}
	return nil
}
