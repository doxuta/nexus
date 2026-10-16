package tenantbackup

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

var null = sql.NullString{}

func TestProjection(t *testing.T) {
	tests := []struct {
		dataType string
		want     string
	}{
		{"timestamp", "DATE_FORMAT(`c`,'%Y-%m-%d %H:%i:%s.%f')"},
		{"datetime", "DATE_FORMAT(`c`,'%Y-%m-%d %H:%i:%s.%f')"},
		{"date", "CAST(`c` AS CHAR)"},
		{"json", "CAST(`c` AS CHAR)"},
		{"bigint", "CAST(`c` AS CHAR)"},
		{"decimal", "CAST(`c` AS CHAR)"},
		{"tinyint", "CAST(`c` AS CHAR)"},
		{"binary", "TO_BASE64(`c`)"},
		{"blob", "TO_BASE64(`c`)"},
		{"varchar", "`c`"},
		{"enum", "`c`"},
		{"mediumtext", "`c`"},
		{"char", "`c`"},
	}
	for _, tc := range tests {
		got, err := projection(Column{Name: "c", DataType: tc.dataType})
		if err != nil || got != tc.want {
			t.Errorf("projection(%s) = %q, %v; want %q", tc.dataType, got, err, tc.want)
		}
	}
	if _, err := projection(Column{Name: "g", DataType: "geometry"}); err == nil {
		t.Error("an unknown DATA_TYPE must be refused, not silently dumped wrong")
	}
	if _, err := projection(Column{Name: "bad`name", DataType: "int"}); err == nil {
		t.Error("a column name outside [A-Za-z0-9_] must be refused")
	}
}

func TestSelectList(t *testing.T) {
	cols := []Column{{"id", "char"}, {"created_at", "timestamp"}}
	got, err := SelectList("c", cols)
	if err != nil {
		t.Fatal(err)
	}
	want := "`c`.`id` AS `id`, DATE_FORMAT(`c`.`created_at`,'%Y-%m-%d %H:%i:%s.%f') AS `created_at`"
	if got != want {
		t.Errorf("SelectList = %q\nwant %q", got, want)
	}
}

// The codec must round-trip every value the engine stores. Values enter as the projection's text
// (what the driver returns for the projected expression) and must come back byte-identical.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cols := []Column{
		{"ts3", "timestamp"}, {"dt3", "datetime"}, {"ts0", "timestamp"},
		{"doc", "json"}, {"txt", "text"}, {"big", "bigint"}, {"dec", "decimal"},
		{"flag", "tinyint"}, {"kind", "enum"}, {"bin", "binary"}, {"nul", "varchar"},
	}
	vals := []sql.NullString{
		ns("2026-09-30 12:34:56.123000"), ns("2026-09-30 12:34:56.999000"), ns("2026-09-30 12:34:56.000000"),
		ns(`{"a":{"b":[1,2,{"emoji":"😀","nl":"x\ny"}]},"s":"q\"uote"}`),
		ns("dòng 1\ndòng 2\t\"quoted\" \\ <tag> &   😀"),
		ns("18446744073709551615"), ns("12345678901234.123456"),
		ns("1"), ns("draft"), ns("AAECA/8="), null,
	}
	line, err := EncodeRow(cols, vals)
	if err != nil {
		t.Fatalf("EncodeRow: %v", err)
	}
	if line[len(line)-1] != '\n' || strings.Count(string(line), "\n") != 1 {
		t.Fatalf("a row must be exactly one line ending in \\n: %q", line)
	}
	if !json.Valid(line) {
		t.Fatalf("a row must be valid JSON: %s", line)
	}
	if !strings.Contains(string(line), `"big":"18446744073709551615"`) {
		t.Errorf("a BIGINT UNSIGNED must travel as a string, not a float64: %s", line)
	}
	if !strings.Contains(string(line), `"doc":{"a":`) {
		t.Errorf("a JSON column must be embedded raw, not quoted: %s", line)
	}
	if !strings.Contains(string(line), `"nul":null`) {
		t.Errorf("NULL must be JSON null: %s", line)
	}

	args, err := DecodeRow(cols, line)
	if err != nil {
		t.Fatalf("DecodeRow: %v", err)
	}
	for i, c := range cols {
		switch {
		case !vals[i].Valid:
			if args[i] != nil {
				t.Errorf("%s: NULL must decode to nil, got %v", c.Name, args[i])
			}
		case c.DataType == "json":
			var a, b any
			_ = json.Unmarshal([]byte(vals[i].String), &a)
			_ = json.Unmarshal([]byte(args[i].(string)), &b)
			ab, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			if string(ab) != string(bb) {
				t.Errorf("%s: JSON changed: %s vs %s", c.Name, ab, bb)
			}
		default:
			if args[i] != vals[i].String {
				t.Errorf("%s: %q != %q", c.Name, args[i], vals[i].String)
			}
		}
	}
}

func TestDecodeRow_Errors(t *testing.T) {
	cols := []Column{{"a", "varchar"}}
	for name, line := range map[string]string{
		"not json":        "{nope\n",
		"string expected": `{"a":5}` + "\n",
		"not an object":   `[1]` + "\n",
		"unknown column":  `{"a":"x","zzz":"y"}` + "\n",
	} {
		if _, err := DecodeRow(cols, []byte(line)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBatchSize(t *testing.T) {
	tests := []struct{ cols, want int }{
		{1, 500}, {10, 500}, {131, 500}, {132, 496}, {200, 327}, {65535, 1},
	}
	for _, tc := range tests {
		if got := BatchSize(tc.cols); got != tc.want {
			t.Errorf("BatchSize(%d) = %d, want %d", tc.cols, got, tc.want)
		}
	}
}

func TestBuildInsert(t *testing.T) {
	cols := []Column{{"id", "char"}, {"doc", "json"}, {"bin", "binary"}}
	q, args, err := BuildInsert("sys_files", cols, [][]any{
		{"1", `{"a":1}`, "AAEC"},
		{"2", nil, nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `sys_files` (`id`,`doc`,`bin`) VALUES (?,?,FROM_BASE64(?)),(?,?,FROM_BASE64(?))"
	if q != want {
		t.Errorf("query = %q\nwant %q", q, want)
	}
	if len(args) != 6 || args[1] != `{"a":1}` || args[4] != nil {
		t.Errorf("args = %v", args)
	}
	if _, _, err := BuildInsert("bad`table", cols, [][]any{{"1", "2", "3"}}); err == nil {
		t.Error("table name outside the identifier charset must be refused")
	}
	if _, _, err := BuildInsert("t", cols, [][]any{{"1"}}); err == nil {
		t.Error("a row with the wrong arity must be refused")
	}
}

func TestIsGeneratedExtra(t *testing.T) {
	tests := []struct {
		extra string
		want  bool
	}{
		{"", false},
		{"DEFAULT_GENERATED", false}, // created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP — a REAL column
		{"DEFAULT_GENERATED on update CURRENT_TIMESTAMP", false},
		{"VIRTUAL GENERATED", true},
		{"STORED GENERATED", true},
		{"auto_increment", false},
	}
	for _, tc := range tests {
		if got := isGeneratedExtra(tc.extra); got != tc.want {
			t.Errorf("isGeneratedExtra(%q) = %v, want %v", tc.extra, got, tc.want)
		}
	}
}
