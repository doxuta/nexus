//go:build integration

package tenantbackup

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func colNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func has(cols []Column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

// T010(f): the native-column rule on real information_schema. created_at (EXTRA =
// DEFAULT_GENERATED) travels; the generated copies do not. Pinned on the two system tables that
// carry a generated column today, and on a temp table with both VIRTUAL and STORED ones.
func TestNativeColumns_GeneratedFilter(t *testing.T) {
	db := itDB(t)
	ctx := ctxT(t)

	jobs, err := NativeColumns(ctx, db, "sys_jobs")
	if err != nil {
		t.Fatal(err)
	}
	if has(jobs, "payload_hash") {
		t.Errorf("sys_jobs.payload_hash is a generated column and must not be in the native list: %v", colNames(jobs))
	}
	if !has(jobs, "created_at") {
		t.Errorf("sys_jobs.created_at is a real column and must stay: %v", colNames(jobs))
	}
	creds, err := NativeColumns(ctx, db, "sys_named_credentials")
	if err != nil {
		t.Fatal(err)
	}
	if has(creds, "name_live") {
		t.Errorf("sys_named_credentials.name_live is generated and must not be in the native list: %v", colNames(creds))
	}

	name := "tbk_it_" + randSuffix(t)
	if _, err := db.Exec("CREATE TABLE `" + name + "` (" +
		"id CHAR(36) NOT NULL PRIMARY KEY, tenant_id CHAR(36) NOT NULL, data JSON NULL, " +
		"created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), " +
		"gv VARCHAR(20) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(data, '$.x'))) VIRTUAL, " +
		"gs VARCHAR(20) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(data, '$.y'))) STORED)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS `" + name + "`") })
	cols, err := NativeColumns(ctx, db, name)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(colNames(cols), ","); got != "id,tenant_id,data,created_at" {
		t.Errorf("native columns = %s, want id,tenant_id,data,created_at", got)
	}

	// obj_* tables: the eleven mandatory columns, no generated copy. Uses the seeded dev entity
	// table when present (a fresh migrated DB has none, so create a minimal one by hand is NOT
	// needed — ddl.MandatoryColumns coverage lives in the ddl package).
	if _, err := NativeColumns(ctx, db, "no_such_table_"+randSuffix(t)); err == nil {
		t.Error("a missing table must be an error, not an empty column list")
	}
}

// T010(g): a TIMESTAMP(3) keeps its milliseconds through DATE_FORMAT … %f, on a connection pinned
// to UTC.
func TestProjection_TimestampKeepsMilliseconds(t *testing.T) {
	db := itDB(t)
	ctx := ctxT(t)
	conn, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatal(err)
	}
	cols, err := NativeColumns(ctx, conn, "sys_files")
	if err != nil {
		t.Fatal(err)
	}
	var created Column
	for _, c := range cols {
		if c.Name == "created_at" {
			created = c
		}
	}
	list, err := SelectList("f", []Column{created})
	if err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-" + randSuffix(t) + "0000"
	if _, err := conn.ExecContext(ctx, "INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, created_at) "+
		"VALUES (?, 'tbk-it-tenant', 'e', 'r', 'f', 'n', 1, 'x', 'p', 'u', '2026-01-02 03:04:05.123')", id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM sys_files WHERE id = ?", id) })

	var got sql.NullString
	if err := conn.GetContext(ctx, &got, "SELECT "+strings.Replace(list, " AS `created_at`", "", 1)+" FROM sys_files f WHERE f.id = ?", id); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.String != "2026-01-02 03:04:05.123000" {
		t.Errorf("created_at projection = %q, want 2026-01-02 03:04:05.123000", got.String)
	}
}

// The codec round-trips real MySQL values: dump table A through the projection + EncodeRow,
// DecodeRow + BuildInsert into an identical table B, and compare B's projected text with A's.
func TestCodec_RealMySQLRoundTrip(t *testing.T) {
	db := itDB(t)
	ctx := ctxT(t)
	suffix := randSuffix(t)
	a, b := "tbk_it_"+suffix+"_a", "tbk_it_"+suffix+"_b"
	ddl := func(n string) string {
		return "CREATE TABLE `" + n + "` (id CHAR(36) NOT NULL PRIMARY KEY, " +
			"ts3 TIMESTAMP(3) NULL, dt3 DATETIME(3) NULL, ts0 TIMESTAMP NULL, doc JSON NULL, " +
			"txt MEDIUMTEXT NULL, big BIGINT UNSIGNED NULL, dec6 DECIMAL(20,6) NULL, flag TINYINT(1) NULL, " +
			"kind ENUM('draft','live') NULL, bin BINARY(4) NULL, nul VARCHAR(10) NULL) " +
			"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
	}
	for _, n := range []string{a, b} {
		if _, err := db.Exec(ddl(n)); err != nil {
			t.Fatal(err)
		}
		n := n
		t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS `" + n + "`") })
	}

	conn, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO `"+a+"` VALUES ('r1', '2026-09-30 12:34:56.123', '2026-09-30 12:34:56.999', '2026-09-30 12:34:56', "+
		"?, ?, 18446744073709551615, 12345678901234.123456, 1, 'live', UNHEX('0001FF7F'), NULL)",
		`{"a":{"b":[1,2,{"emoji":"😀","nl":"x\ny"}]},"s":"q\"uote"}`, "dòng 1\ndòng 2 \"q\" \\ <t> & 😀"); err != nil {
		t.Fatal(err)
	}

	cols, err := NativeColumns(ctx, conn, a)
	if err != nil {
		t.Fatal(err)
	}
	list, err := SelectList("t", cols)
	if err != nil {
		t.Fatal(err)
	}
	dump := func(table string) []sql.NullString {
		rows, err := conn.QueryContext(ctx, "SELECT "+list+" FROM `"+table+"` t WHERE t.id = 'r1'")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no row in %s", table)
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		return vals
	}
	va := dump(a)
	line, err := EncodeRow(cols, va)
	if err != nil {
		t.Fatal(err)
	}
	args, err := DecodeRow(cols, line)
	if err != nil {
		t.Fatal(err)
	}
	q, qargs, err := BuildInsert(b, cols, [][]any{args})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, q, qargs...); err != nil {
		t.Fatalf("insert %s: %v\n%s", b, err, q)
	}
	vb := dump(b)
	for i, c := range cols {
		if c.DataType == "json" {
			continue // MySQL re-normalises JSON text; compare structurally below
		}
		if va[i] != vb[i] {
			t.Errorf("column %s: %v != %v", c.Name, va[i], vb[i])
		}
	}
	var same bool
	if err := conn.GetContext(ctx, &same, fmt.Sprintf("SELECT (SELECT doc FROM `%s` WHERE id='r1') = (SELECT doc FROM `%s` WHERE id='r1')", a, b)); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("JSON column changed in the round trip")
	}
}
