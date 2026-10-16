package tenantbackup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Column is one native (non-generated) column of a table and its information_schema DATA_TYPE.
type Column struct {
	Name     string `json:"name"`
	DataType string `json:"dataType"`
}

// Querier is the slice of *sqlx.DB / *sqlx.Conn / *sqlx.Tx the codec and CheckSchema read through.
type Querier interface {
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

// identRe is the only shape a table or column name may take when it is spliced into SQL. Names come
// from information_schema (this schema) or from a manifest (an untrusted file on restore), so the
// check stays even though the first source is trusted.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func checkIdent(name string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("tenantbackup: identifier %q is not [A-Za-z0-9_]{1,64}", name)
	}
	return nil
}

// isGeneratedExtra reports whether an information_schema.COLUMNS.EXTRA value marks a generated
// column (cannot be INSERTed: error 3105). It is NOT "contains GENERATED": a plain
// `created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP` reports EXTRA = "DEFAULT_GENERATED" and is a
// real column that must travel (the first draft of the filter dropped every created_at).
func isGeneratedExtra(extra string) bool {
	return strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED")
}

// NativeColumns lists a table's columns in ordinal order WITHOUT the generated ones. For obj_*
// that is the eleven ddl.MandatoryColumns; the extra unique/relation/index columns a field adds
// are STORED/VIRTUAL generated copies of data->'$.x' and are rebuilt by the engine, not restored.
func NativeColumns(ctx context.Context, q Querier, table string) ([]Column, error) {
	var rows []struct {
		Name  string `db:"COLUMN_NAME"`
		Type  string `db:"DATA_TYPE"`
		Extra string `db:"EXTRA"`
	}
	if err := q.SelectContext(ctx, &rows, `
		SELECT COLUMN_NAME, DATA_TYPE, EXTRA FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, table); err != nil {
		return nil, fmt.Errorf("tenantbackup.NativeColumns %s: %w", table, err)
	}
	cols := make([]Column, 0, len(rows))
	for _, r := range rows {
		if isGeneratedExtra(r.Extra) {
			continue
		}
		cols = append(cols, Column{Name: r.Name, DataType: strings.ToLower(r.Type)})
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("tenantbackup.NativeColumns %s: table not found", table)
	}
	return cols, nil
}

// projection is the SELECT expression that makes a column's value travel as lossless text.
// Time values go through DATE_FORMAT %f (microseconds, so a TIMESTAMP(3) keeps its .fff) on a
// connection pinned to time_zone '+00:00'; big integers and decimals through CAST AS CHAR (a JSON
// number would round past 2^53); binary through TO_BASE64; JSON through CAST AS CHAR so MySQL's
// normalised text comes out.
func projection(c Column) (string, error) {
	if err := checkIdent(c.Name); err != nil {
		return "", err
	}
	return projectExpr("`"+c.Name+"`", c.DataType, c.Name)
}

func projectExpr(ref, dataType, name string) (string, error) {
	switch dataType {
	case "timestamp", "datetime":
		return "DATE_FORMAT(" + ref + ",'%Y-%m-%d %H:%i:%s.%f')", nil
	case "date", "time", "json", "year",
		"tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "bit":
		return "CAST(" + ref + " AS CHAR)", nil
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return "TO_BASE64(" + ref + ")", nil
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return ref, nil
	}
	return "", fmt.Errorf("tenantbackup: column %q has unsupported DATA_TYPE %q", name, dataType)
}

// SelectList builds `alias`.`col` AS `col` projections for a table read under a table alias (the
// alias makes the same list usable for a child joined to its parent).
func SelectList(alias string, cols []Column) (string, error) {
	if err := checkIdent(alias); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		if err := checkIdent(c.Name); err != nil {
			return "", err
		}
		expr, err := projectExpr("`"+alias+"`.`"+c.Name+"`", c.DataType, c.Name)
		if err != nil {
			return "", err
		}
		parts = append(parts, expr+" AS `"+c.Name+"`")
	}
	return strings.Join(parts, ", "), nil
}

// EncodeRow writes one row as a JSON object on one line (keys in column order, trailing \n).
// vals are the projected texts in column order. JSON columns are embedded raw so the file stays
// readable; everything else is a JSON string; NULL is null.
func EncodeRow(cols []Column, vals []sql.NullString) ([]byte, error) {
	if len(cols) != len(vals) {
		return nil, fmt.Errorf("tenantbackup.EncodeRow: %d columns, %d values", len(cols), len(vals))
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := marshalString(c.Name)
		b.Write(key)
		b.WriteByte(':')
		v := vals[i]
		switch {
		case !v.Valid:
			b.WriteString("null")
		case c.DataType == "json":
			if !json.Valid([]byte(v.String)) {
				return nil, fmt.Errorf("tenantbackup.EncodeRow: column %q holds invalid JSON", c.Name)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(v.String)); err != nil {
				return nil, fmt.Errorf("tenantbackup.EncodeRow: column %q: %w", c.Name, err)
			}
			b.Write(compact.Bytes())
		default:
			s, err := marshalString(v.String)
			if err != nil {
				return nil, fmt.Errorf("tenantbackup.EncodeRow: column %q: %w", c.Name, err)
			}
			b.Write(s)
		}
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// marshalString is json.Marshal for a string without HTML escaping (<, >, & stay readable).
func marshalString(s string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// DecodeRow is EncodeRow's inverse: one JSONL line → INSERT arguments in column order. Strict on
// shape: a key that is not a declared column, or a value of the wrong JSON type, is an error (a
// file edited by hand must fail loudly, not insert something near-right).
func DecodeRow(cols []Column, line []byte) ([]any, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("tenantbackup.DecodeRow: %w", err)
	}
	known := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		known[c.Name] = struct{}{}
	}
	for k := range m {
		if _, ok := known[k]; !ok {
			return nil, fmt.Errorf("tenantbackup.DecodeRow: unknown column %q", k)
		}
	}
	args := make([]any, len(cols))
	for i, c := range cols {
		raw, ok := m[c.Name]
		if !ok || string(raw) == "null" {
			continue // NULL
		}
		if c.DataType == "json" {
			args[i] = string(raw)
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("tenantbackup.DecodeRow: column %q is not a string: %w", c.Name, err)
		}
		args[i] = s
	}
	return args, nil
}

const (
	maxBatchRows         = 500
	maxBatchPlaceholders = 65535 // MySQL prepared-statement limit
)

// BatchSize is how many rows one multi-row INSERT may carry for a table of n columns: 500, or
// fewer when 500 rows would exceed MySQL's 65 535 placeholders.
func BatchSize(n int) int {
	if n <= 0 {
		return 1
	}
	rows := maxBatchPlaceholders / n
	if rows > maxBatchRows {
		return maxBatchRows
	}
	if rows < 1 {
		return 1
	}
	return rows
}

// BuildInsert renders one multi-row INSERT. Binary columns bind through FROM_BASE64(?); every value
// is a bound argument — names are validated identifiers, never data.
func BuildInsert(table string, cols []Column, rows [][]any) (string, []any, error) {
	if err := checkIdent(table); err != nil {
		return "", nil, err
	}
	if len(rows) == 0 {
		return "", nil, fmt.Errorf("tenantbackup.BuildInsert: no rows")
	}
	names := make([]string, len(cols))
	slot := make([]string, len(cols))
	for i, c := range cols {
		if err := checkIdent(c.Name); err != nil {
			return "", nil, err
		}
		names[i] = "`" + c.Name + "`"
		slot[i] = "?"
		switch c.DataType {
		case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
			slot[i] = "FROM_BASE64(?)"
		}
	}
	tuple := "(" + strings.Join(slot, ",") + ")"
	var q strings.Builder
	q.WriteString("INSERT INTO `" + table + "` (" + strings.Join(names, ",") + ") VALUES ")
	args := make([]any, 0, len(rows)*len(cols))
	for i, r := range rows {
		if len(r) != len(cols) {
			return "", nil, fmt.Errorf("tenantbackup.BuildInsert: row %d has %d values, want %d", i, len(r), len(cols))
		}
		if i > 0 {
			q.WriteByte(',')
		}
		q.WriteString(tuple)
		args = append(args, r...)
	}
	return q.String(), args, nil
}
