package tenantbackup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// SnapshotSource opens a consistent read of the database for ONE backup.
type SnapshotSource interface {
	Open(ctx context.Context) (Snapshot, error)
}

// Snapshot is one consistent (REPEATABLE READ, read-only) view of the database.
type Snapshot interface {
	// SchemaVersion is MAX(schema_migrations.version) and its dirty flag as of the snapshot.
	SchemaVersion() (int64, bool)
	// Branding is the customer's tenants.branding (the only column of the tenants row that is backed up).
	Branding(ctx context.Context, tenantID string) (json.RawMessage, error)
	// Plan lists the tables to export with their file columns.
	Plan(ctx context.Context) ([]TablePlan, error)
	// Dump streams one table's rows of ONE customer. vals is reused between calls: copy to keep.
	Dump(ctx context.Context, tp TablePlan, tenantID string, row func(vals []sql.NullString) error) error
	// Close ends the snapshot and puts the connection back exactly as it was found (session time
	// zone included). Idempotent.
	Close(ctx context.Context)
}

// SqlxSnapshots is the MySQL SnapshotSource.
type SqlxSnapshots struct{ db *sqlx.DB }

// NewSqlxSnapshots wires the source on the application pool.
func NewSqlxSnapshots(db *sqlx.DB) *SqlxSnapshots { return &SqlxSnapshots{db: db} }

type sqlxSnapshot struct {
	conn    *sqlx.Conn
	tx      *sqlx.Tx
	prevTZ  string
	tzSet   bool
	version int64
	dirty   bool
	once    sync.Once
}

// Open takes a connection of its own, pins its session to UTC (so DATE_FORMAT of a TIMESTAMP is
// independent of the DSN's loc and of the server's zone — prod runs loc=Local, CI loc=Local, the
// host zone is not guaranteed), starts a REPEATABLE READ READ ONLY transaction and reads
// schema_migrations FIRST: the first consistent read fixes the snapshot for every table after it.
func (s *SqlxSnapshots) Open(ctx context.Context) (Snapshot, error) {
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.SqlxSnapshots.Open: conn: %w", err)
	}
	snap := &sqlxSnapshot{conn: conn}
	fail := func(e error) (Snapshot, error) {
		snap.Close(ctx)
		return nil, e
	}
	if err := conn.GetContext(ctx, &snap.prevTZ, "SELECT @@session.time_zone"); err != nil {
		return fail(fmt.Errorf("tenantbackup.SqlxSnapshots.Open: read time_zone: %w", err))
	}
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		return fail(fmt.Errorf("tenantbackup.SqlxSnapshots.Open: set time_zone: %w", err))
	}
	snap.tzSet = true
	tx, err := conn.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return fail(fmt.Errorf("tenantbackup.SqlxSnapshots.Open: begin: %w", err))
	}
	snap.tx = tx
	var row struct {
		Version int64 `db:"version"`
		Dirty   bool  `db:"dirty"`
	}
	if err := tx.GetContext(ctx, &row, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		return fail(fmt.Errorf("tenantbackup.SqlxSnapshots.Open: schema_migrations: %w", err))
	}
	snap.version, snap.dirty = row.Version, row.Dirty
	return snap, nil
}

func (s *sqlxSnapshot) SchemaVersion() (int64, bool) { return s.version, s.dirty }

func (s *sqlxSnapshot) Branding(ctx context.Context, tenantID string) (json.RawMessage, error) {
	var b []byte
	// Unscoped: a retired customer still has its branding backed up.
	if err := s.tx.GetContext(ctx, &b, "SELECT branding FROM tenants WHERE id = ?", tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("tenantbackup.sqlxSnapshot.Branding: %w", err)
	}
	return b, nil
}

func (s *sqlxSnapshot) Plan(ctx context.Context) ([]TablePlan, error) {
	refs, err := exportTables(ctx, s.tx)
	if err != nil {
		return nil, err
	}
	plans := make([]TablePlan, 0, len(refs))
	for _, ref := range refs {
		tp, err := planTable(ctx, s.tx, ref)
		if err != nil {
			return nil, err
		}
		plans = append(plans, tp)
	}
	return plans, nil
}

func (s *sqlxSnapshot) Dump(ctx context.Context, tp TablePlan, tenantID string, row func([]sql.NullString) error) error {
	list, err := SelectList("t", tp.Cols)
	if err != nil {
		return err
	}
	fw, err := tp.fromWhere()
	if err != nil {
		return err
	}
	// #nosec G202 -- list/fw are built from validated identifiers (information_schema + policy); the
	// only value, the tenant id, is a bound argument.
	rows, err := s.tx.QueryContext(ctx, "SELECT "+list+" "+fw, tenantID)
	if err != nil {
		return fmt.Errorf("tenantbackup.sqlxSnapshot.Dump %s: %w", tp.Table, err)
	}
	defer rows.Close()
	vals := make([]sql.NullString, len(tp.Cols))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("tenantbackup.sqlxSnapshot.Dump %s: scan: %w", tp.Table, err)
		}
		if err := row(vals); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tenantbackup.sqlxSnapshot.Dump %s: %w", tp.Table, err)
	}
	return nil
}

// Close commits the read-only transaction (shortening how long MVCC history is held), restores the
// session time zone and releases the connection. A connection whose zone could not be restored is
// discarded rather than returned to the pool: a pooled connection that silently runs in UTC would
// shift every later TIMESTAMP read of an unrelated request.
func (s *sqlxSnapshot) Close(ctx context.Context) {
	s.once.Do(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if s.tx != nil {
			_ = s.tx.Commit()
		}
		if s.tzSet {
			if _, err := s.conn.ExecContext(cctx, "SET time_zone = ?", s.prevTZ); err != nil {
				_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		_ = s.conn.Close()
	})
}
