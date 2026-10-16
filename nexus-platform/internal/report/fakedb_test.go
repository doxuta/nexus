package report

// This file is a self-contained fake database/sql driver used ONLY by default-build unit tests.
// It is stdlib-only (database/sql/driver) — sqlmock is intentionally NOT a module dependency —
// and it records every SQL statement + its args while replaying programmable exec/query results.
// That makes the repository's generated SQL (tenant_id pin, deleted_at guard), argument ordering,
// and error mapping (sql.ErrNoRows / zero-rows-affected → typed not-found) unit-testable without
// MySQL. Real-MySQL round-trips stay in the //go:build integration suites.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
)

// loggedCall is one statement the repository sent to the driver (post arg conversion).
type loggedCall struct {
	query string
	args  []driver.Value
}

type execReply struct {
	rowsAffected int64
	err          error
}

type queryReply struct {
	cols []string
	rows [][]driver.Value
	err  error
}

// fakeDB is the programmable recorder shared by every connection the sql.DB pool opens.
// Un-queued calls default to exec→(1 row affected, nil) and query→(zero rows, nil).
type fakeDB struct {
	mu           sync.Mutex
	execCalls    []loggedCall
	queryCalls   []loggedCall
	execReplies  []execReply
	queryReplies []queryReply
}

func (f *fakeDB) queueExec(rowsAffected int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execReplies = append(f.execReplies, execReply{rowsAffected: rowsAffected, err: err})
}

func (f *fakeDB) queueQuery(cols []string, rows [][]driver.Value, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryReplies = append(f.queryReplies, queryReply{cols: cols, rows: rows, err: err})
}

func (f *fakeDB) lastExec(t *testing.T) loggedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execCalls) == 0 {
		t.Fatal("no exec call recorded")
	}
	return f.execCalls[len(f.execCalls)-1]
}

func (f *fakeDB) lastQuery(t *testing.T) loggedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queryCalls) == 0 {
		t.Fatal("no query call recorded")
	}
	return f.queryCalls[len(f.queryCalls)-1]
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: Prepare not supported (ExecContext/QueryContext only)")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("fakeConn: Begin not supported")
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals := namedToValues(args)
	c.db.mu.Lock()
	c.db.execCalls = append(c.db.execCalls, loggedCall{query: query, args: vals})
	reply := execReply{rowsAffected: 1}
	if len(c.db.execReplies) > 0 {
		reply = c.db.execReplies[0]
		c.db.execReplies = c.db.execReplies[1:]
	}
	c.db.mu.Unlock()
	if reply.err != nil {
		return nil, reply.err
	}
	return stubResult{rows: reply.rowsAffected}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := namedToValues(args)
	c.db.mu.Lock()
	c.db.queryCalls = append(c.db.queryCalls, loggedCall{query: query, args: vals})
	var reply queryReply
	if len(c.db.queryReplies) > 0 {
		reply = c.db.queryReplies[0]
		c.db.queryReplies = c.db.queryReplies[1:]
	}
	c.db.mu.Unlock()
	if reply.err != nil {
		return nil, reply.err
	}
	return &stubRows{cols: reply.cols, rows: reply.rows}, nil
}

type stubResult struct{ rows int64 }

func (r stubResult) LastInsertId() (int64, error) { return 0, nil }
func (r stubResult) RowsAffected() (int64, error) { return r.rows, nil }

type stubRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB with fakeConnector")
}

type fakeConnector struct{ db *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{db: c.db}, nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

// newFakeDB returns a sqlx handle over the fake driver plus its recorder. The driver name is
// "mysql" so sqlx uses QUESTION bind placeholders, exactly like production.
func newFakeDB(t *testing.T) (*sqlx.DB, *fakeDB) {
	t.Helper()
	rec := &fakeDB{}
	db := sqlx.NewDb(sql.OpenDB(fakeConnector{db: rec}), "mysql")
	t.Cleanup(func() { _ = db.Close() })
	return db, rec
}

func namedToValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

// mustContainSQL fails the test unless every want substring appears in the recorded SQL. Used to
// pin the tenant filter and soft-delete guard of each generated statement.
func mustContainSQL(t *testing.T, query string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(query, w) {
			t.Errorf("SQL missing %q:\n%s", w, query)
		}
	}
}
