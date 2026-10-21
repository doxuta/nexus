package PKG

// Held out of the eval run: regrade.sh copies this file, with its package name set, next to a case's hidden test.
// A database/sql driver that records every statement and answers from an in-memory table of notes, so the hidden
// tests depend on nothing in the agent's workspace but the code under test.

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

type hiddenCall struct {
	query string
	args  []driver.Value
}

// hiddenDB holds the rows of sys_record_notes, newest first, and every statement it was sent.
type hiddenDB struct {
	mu    sync.Mutex
	calls []hiddenCall
	rows  []map[string]driver.Value
}

var hiddenStores sync.Map // dsn → *hiddenDB

func init() { sql.Register("hidden-notes-sql", hiddenDriver{}) }

func newHiddenDB(t *testing.T, rows ...map[string]driver.Value) (*sqlx.DB, *hiddenDB) {
	t.Helper()
	s := &hiddenDB{rows: rows}
	dsn := t.Name() + "/" + strconv.FormatInt(time.Now().UnixNano(), 10)
	hiddenStores.Store(dsn, s)
	db, err := sql.Open("hidden-notes-sql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlx.NewDb(db, "mysql"), s
}

// hiddenNote is one sys_record_notes row of tenant t1, record r1 of crm_accounts.
func hiddenNote(id, body string, at time.Time) map[string]driver.Value {
	return map[string]driver.Value{
		"id": id, "tenant_id": "t1", "entity_name": "crm_accounts", "record_id": "r1", "body": body,
		"created_by": "u1", "created_at": at, "updated_at": at, "deleted_at": nil, "pinned": false,
	}
}

func (s *hiddenDB) lastCall(t *testing.T) hiddenCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("no statement reached the database")
	}
	return s.calls[len(s.calls)-1]
}

type hiddenDriver struct{}

func (hiddenDriver) Open(dsn string) (driver.Conn, error) {
	s, _ := hiddenStores.Load(dsn)
	return hiddenConn{s.(*hiddenDB)}, nil
}

type hiddenConn struct{ s *hiddenDB }

func (c hiddenConn) Prepare(q string) (driver.Stmt, error) { return hiddenStmt{c.s, q}, nil }
func (c hiddenConn) Close() error                          { return nil }
func (c hiddenConn) Begin() (driver.Tx, error)             { return hiddenTx{}, nil }

type hiddenTx struct{}

func (hiddenTx) Commit() error   { return nil }
func (hiddenTx) Rollback() error { return nil }

type hiddenStmt struct {
	s *hiddenDB
	q string
}

func (st hiddenStmt) Close() error  { return nil }
func (st hiddenStmt) NumInput() int { return -1 }

func (st hiddenStmt) Exec(args []driver.Value) (driver.Result, error) {
	st.s.record(st.q, args)
	return driver.RowsAffected(1), nil
}

func (st hiddenStmt) Query(args []driver.Value) (driver.Rows, error) {
	st.s.record(st.q, args)
	return st.s.answer(st.q, args), nil
}

func (s *hiddenDB) record(q string, args []driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, hiddenCall{q, args})
}

var (
	hiddenSelect = regexp.MustCompile(`(?is)^\s*SELECT\s+(.*?)\s+FROM\s`)
	hiddenLimit  = regexp.MustCompile(`(?is)\bLIMIT\s+(\?|\d+)(?:\s+OFFSET\s+(\?|\d+))?\s*$`)
)

// answer serves a COUNT with the number of rows, and any other SELECT with the columns it names, newest row
// first, honoring a trailing LIMIT/OFFSET.
func (s *hiddenDB) answer(q string, args []driver.Value) driver.Rows {
	m := hiddenSelect.FindStringSubmatch(q)
	if m == nil {
		return &hiddenRows{}
	}
	if strings.Contains(strings.ToUpper(m[1]), "COUNT(") {
		return &hiddenRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(len(s.rows))}}}
	}
	var cols []string
	for _, c := range strings.Split(m[1], ",") {
		c = strings.TrimSpace(c)
		if i := strings.LastIndex(strings.ToLower(c), " as "); i >= 0 {
			c = c[i+4:]
		}
		if i := strings.LastIndex(c, "."); i >= 0 {
			c = c[i+1:]
		}
		cols = append(cols, strings.Trim(strings.TrimSpace(c), "`"))
	}
	rows := s.rows
	if lm := hiddenLimit.FindStringSubmatchIndex(q); lm != nil {
		n := strings.Count(q[:lm[0]], "?")
		val := func(lo, hi int) int {
			if lo < 0 {
				return 0
			}
			if tok := q[lo:hi]; tok != "?" {
				v, _ := strconv.Atoi(tok)
				return v
			}
			v, _ := args[n].(int64)
			n++
			return int(v)
		}
		limit, offset := val(lm[2], lm[3]), val(lm[4], lm[5])
		if offset > len(rows) {
			offset = len(rows)
		}
		rows = rows[offset:]
		if limit < len(rows) {
			rows = rows[:limit]
		}
	}
	out := &hiddenRows{cols: cols}
	for _, r := range rows {
		vals := make([]driver.Value, len(cols))
		for i, c := range cols {
			vals[i] = r[c]
		}
		out.vals = append(out.vals, vals)
	}
	return out
}

type hiddenRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *hiddenRows) Columns() []string { return r.cols }
func (r *hiddenRows) Close() error      { return nil }

func (r *hiddenRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}
