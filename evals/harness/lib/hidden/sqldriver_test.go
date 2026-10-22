package PKG

// Held out of the eval run: regrade.sh copies this file, with its package name set, next to a case's hidden test.
// A database/sql driver that records every statement and answers from an in-memory table of notes, so the hidden
// tests depend on nothing in the agent's workspace but the code under test.
//
// It answers as the statement asks, after removing its SQL comments: the top-level AND terms of the WHERE clause
// (parenthesised AND groups included) that are `col = ?`, `? = col`, `col IN (?)` or `col IS NULL` filter the rows (a
// top-level OR filters nothing, since it would let rows through anyway; any other term is ignored), ORDER BY sorts
// them by plain or table-qualified columns, LIMIT/OFFSET cut them. Without an ORDER BY the rows keep the order the test stored them in. The tests store rows that only a
// correct query leaves out (another tenant, another record, a soft-deleted row, a newer row stored first, an id that
// sorts the wrong way), so a query that forgets one of them returns the wrong row.

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"sort"
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

// hiddenDB holds the rows of sys_record_notes, in the order a test stored them, and every statement it was sent.
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
	hiddenSelect  = regexp.MustCompile(`(?is)^\s*SELECT\s+(.*?)\s+FROM\s`)
	hiddenWhere   = regexp.MustCompile(`(?is)\bWHERE\b(.*?)(?:\bGROUP\s+BY\b|\bORDER\s+BY\b|\bLIMIT\b|$)`)
	hiddenOrderBy = regexp.MustCompile(`(?is)\bORDER\s+BY\b(.*?)(?:\bLIMIT\b|$)`)
	hiddenLimit   = regexp.MustCompile(`(?is)\bLIMIT\s+(\?|\d+)(?:\s+OFFSET\s+(\?|\d+))?\s*$`)
	hiddenCol     = `(?:\w+\.)?` + "`?" + `(\w+)` + "`?"
	hiddenEq      = regexp.MustCompile(`(?is)^\(?\s*(?:` + hiddenCol + `\s*=\s*\?|\?\s*=\s*` + hiddenCol + `|` + hiddenCol + `\s+IN\s*\(\s*\?\s*\))\s*\)?$`)
	hiddenIsNull  = regexp.MustCompile(`(?is)^\(?\s*` + hiddenCol + `\s+IS\s+NULL\s*\)?$`)
	hiddenComment = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*|#[^\n]*`)
	hiddenOrWord  = regexp.MustCompile(`(?i)^OR\b`)
	hiddenAndWord = regexp.MustCompile(`(?i)^AND\b`)
)

// hiddenEqColumn returns the column a `col = ?`, `? = col` or `col IN (?)` term compares, or "".
func hiddenEqColumn(term string) string {
	m := hiddenEq.FindStringSubmatch(term)
	if m == nil {
		return ""
	}
	for _, c := range m[1:] {
		if c != "" {
			return c
		}
	}
	return ""
}

// hiddenStrip removes SQL comments, so a term written in a comment neither filters rows nor counts as a filter.
func hiddenStrip(q string) string { return hiddenComment.ReplaceAllString(q, " ") }

// hiddenConjunct is one top-level AND term of a WHERE clause, with the index of its first placeholder among the
// statement's arguments (-1 when it has none).
type hiddenConjunct struct {
	text string
	arg  int
}

// hiddenConjuncts splits a statement's WHERE clause, comments removed, into its top-level AND terms; topOr reports a
// top-level OR. A term wrapped whole in parentheses with no OR of its own is split in turn: (a AND b) AND c gives a,
// b and c.
func hiddenConjuncts(q string) (terms []hiddenConjunct, topOr bool) {
	q = hiddenStrip(q)
	loc := hiddenWhere.FindStringSubmatchIndex(q)
	if loc == nil {
		return nil, false
	}
	return hiddenSplit(q, loc[2], loc[3])
}

func hiddenSplit(q string, start, end int) (terms []hiddenConjunct, topOr bool) {
	depth, from := 0, start
	cut := func(to int) {
		a, b := from, to
		for a < b && strings.ContainsRune(" \t\n\r", rune(q[a])) {
			a++
		}
		for b > a && strings.ContainsRune(" \t\n\r", rune(q[b-1])) {
			b--
		}
		if b-a >= 2 && q[a] == '(' && hiddenClosing(q, a) == b-1 {
			if inner, or := hiddenSplit(q, a+1, b-1); !or {
				terms = append(terms, inner...)
				return
			}
		}
		arg := -1
		if strings.Contains(q[a:b], "?") {
			arg = strings.Count(q[:a], "?")
		}
		terms = append(terms, hiddenConjunct{q[a:b], arg})
	}
	for i := start; i < end; i++ {
		switch q[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		boundary := depth == 0 && (i == start || q[i-1] == ' ' || q[i-1] == '\n' || q[i-1] == '\t' || q[i-1] == ')')
		if !boundary {
			continue
		}
		if hiddenOrWord.MatchString(q[i:end]) && (i+2 == end || !isWordByte(q[i+2])) {
			topOr = true
		}
		if hiddenAndWord.MatchString(q[i:end]) && (i+3 == end || !isWordByte(q[i+3])) {
			cut(i)
			from = i + 3
		}
	}
	cut(end)
	return terms, topOr
}

// hiddenClosing returns the index of the parenthesis that closes the one at q[open], or -1.
func hiddenClosing(q string, open int) int {
	depth := 0
	for i := open; i < len(q); i++ {
		switch q[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// answer serves a COUNT with the number of matching rows, and any other SELECT with the columns it names, filtered,
// ordered and limited as the statement says.
func (s *hiddenDB) answer(q string, args []driver.Value) driver.Rows {
	q = hiddenStrip(q)
	m := hiddenSelect.FindStringSubmatch(q)
	if m == nil {
		return &hiddenRows{}
	}
	rows := s.filter(q, args)
	if strings.Contains(strings.ToUpper(m[1]), "COUNT(") {
		return &hiddenRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(len(rows))}}}
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
	hiddenSort(q, rows)
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

// filter keeps the rows every `col = ?` (or `? = col`, `col IN (?)`) and `col IS NULL` term accepts; a top-level OR
// keeps them all. q has its comments removed.
func (s *hiddenDB) filter(q string, args []driver.Value) []map[string]driver.Value {
	terms, topOr := hiddenConjuncts(q)
	var out []map[string]driver.Value
	for _, r := range s.rows {
		keep := true
		for _, t := range terms {
			if topOr {
				break
			}
			if col := hiddenEqColumn(t.text); col != "" && t.arg >= 0 && t.arg < len(args) {
				keep = keep && hiddenEqual(r[col], args[t.arg])
			} else if m := hiddenIsNull.FindStringSubmatch(t.text); m != nil {
				keep = keep && r[m[1]] == nil
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

func hiddenEqual(a, b driver.Value) bool {
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		return ok && ta.Equal(tb)
	}
	return a == b
}

// hiddenSort orders rows by the statement's ORDER BY (column [ASC|DESC], …); without one they stay in the order
// the test stored them, which the tests make wrong on purpose.
func hiddenSort(q string, rows []map[string]driver.Value) {
	m := hiddenOrderBy.FindStringSubmatch(q)
	if m == nil {
		return
	}
	type key struct {
		col  string
		desc bool
	}
	var keys []key
	for _, part := range strings.Split(m[1], ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			continue
		}
		col := f[0]
		if i := strings.LastIndex(col, "."); i >= 0 {
			col = col[i+1:]
		}
		keys = append(keys, key{strings.Trim(col, "`"), len(f) > 1 && strings.EqualFold(f[1], "DESC")})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			c := hiddenCompare(rows[i][k.col], rows[j][k.col])
			if c != 0 {
				return (c < 0) != k.desc
			}
		}
		return false
	})
}

func hiddenCompare(a, b driver.Value) int {
	switch x := a.(type) {
	case time.Time:
		if y, ok := b.(time.Time); ok {
			return x.Compare(y)
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y)
		}
	case bool:
		if y, ok := b.(bool); ok && x != y {
			if x {
				return 1
			}
			return -1
		}
	case int64:
		if y, ok := b.(int64); ok && x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
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
