package ddl

import (
	"strings"
	"testing"
)

// SanitizeIdentifier is the single guard between user-supplied entity/field names
// and generated DDL. DDL identifiers cannot be parameterized with `?`, so any name
// that reaches a CREATE TABLE / ALTER statement MUST pass this allowlist first.
// These cases pin the contract: valid snake_case in, everything dangerous rejected.
func TestSanitizeIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		// valid
		{"simple", "courses", "courses", false},
		{"with_underscore", "student_code", "student_code", false},
		{"with_digits", "field2", "field2", false},

		// invalid — uppercase rejected, NOT silently lowercased (would break record
		// round-trip: a field stored lowercased can't be written under its declared key)
		{"uppercase first rejected", "Courses", "", true},
		{"camelCase rejected", "isActive", "", true},
		{"all caps rejected", "CODE", "", true},

		// invalid — shape
		{"empty", "", "", true},
		{"leading digit", "2cool", "", true},
		{"leading underscore", "_hidden", "", true},
		{"trailing space", "courses ", "", true},
		{"inner space", "my courses", "", true},
		{"hyphen", "my-courses", "", true},

		// length cap — names are capped at maxIdentifierLen (26) so the longest
		// identifier the engine derives (idx_obj_<entity>_<field>_id) stays within
		// MySQL's 64-char limit. See TestSanitizeIdentifierLengthCap for the boundary
		// proof; here we pin clearly-over-cap names reject.
		{"cap+1 (27) rejected", "a23456789012345678901234567", "", true}, // 27 chars
		{"too long (over cap)", "a234567890123456789012345678901234567890123456789012345678901234", "", true},

		// invalid — injection attempts
		{"semicolon", "courses; drop table users", "", true},
		{"backtick", "cour`ses", "", true},
		{"quote", "cour'ses", "", true},
		{"comment", "courses--", "", true},
		{"paren", "courses()", "", true},

		// invalid — reserved word (SQL + route names that static routes would shadow)
		{"reserved select", "select", "", true},
		{"reserved table", "table", "", true},
		{"reserved order", "order", "", true},
		{"reserved admin route", "admin", "", true},
		{"reserved metadata route", "metadata", "", true},
		{"reserved health route", "health", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SanitizeIdentifier(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SanitizeIdentifier(%q) = %q, nil; want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SanitizeIdentifier(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("SanitizeIdentifier(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// mysqlMaxIdentifierLen is MySQL's hard limit on table/column/index identifier length.
// Every name the DDL engine generates MUST stay within it.
const mysqlMaxIdentifierLen = 64

// TestSanitizeIdentifierLengthCap pins the length boundary: a name of exactly
// maxIdentifierLen chars passes, one char longer rejects. This is the contract that
// guarantees generated index names fit MySQL (see TestGeneratedIdentifiersFitMySQL).
func TestSanitizeIdentifierLengthCap(t *testing.T) {
	atCap := "a" + strings.Repeat("b", maxIdentifierLen-1) // exactly maxIdentifierLen chars
	if len(atCap) != maxIdentifierLen {
		t.Fatalf("test setup: atCap has %d chars, want %d", len(atCap), maxIdentifierLen)
	}
	if got, err := SanitizeIdentifier(atCap); err != nil {
		t.Errorf("SanitizeIdentifier(len=%d) rejected a name at the cap: %v", maxIdentifierLen, err)
	} else if got != atCap {
		t.Errorf("SanitizeIdentifier(%q) = %q; want unchanged", atCap, got)
	}

	overCap := atCap + "c" // maxIdentifierLen+1 chars
	if _, err := SanitizeIdentifier(overCap); err == nil {
		t.Errorf("SanitizeIdentifier(len=%d) accepted a name over the cap; want error", len(overCap))
	}
}

// TestGeneratedIdentifiersFitMySQL is the whole point of the length cap: with BOTH the
// entity and field at the maximum allowed length, every identifier the DDL builders
// actually compose (the unique index uq_obj_<entity>_<field> and the relation index
// idx_obj_<entity>_<field>_id — the longest one) must fit MySQL's 64-char limit. If a
// future change lengthens a prefix without lowering the cap, this test fails loudly
// here instead of as a cryptic runtime DDL error.
func TestGeneratedIdentifiersFitMySQL(t *testing.T) {
	entity := "a" + strings.Repeat("e", maxIdentifierLen-1) // 26 chars
	field := "a" + strings.Repeat("f", maxIdentifierLen-1)  // 26 chars

	// Sanity: both inputs sit exactly at the cap, so they're the worst case.
	for _, n := range []string{entity, field} {
		if _, err := SanitizeIdentifier(n); err != nil {
			t.Fatalf("worst-case input %q rejected by SanitizeIdentifier: %v", n, err)
		}
	}

	// The relation index is the longest generated identifier: idx_obj_<entity>_<field>_id.
	relIdx := "idx_obj_" + entity + "_" + field + "_id"
	if len(relIdx) > mysqlMaxIdentifierLen {
		t.Errorf("relation index %q is %d chars, exceeds MySQL limit %d", relIdx, len(relIdx), mysqlMaxIdentifierLen)
	}
	// The unique index is shorter but still verified: uq_obj_<entity>_<field>.
	uqIdx := "uq_obj_" + entity + "_" + field
	if len(uqIdx) > mysqlMaxIdentifierLen {
		t.Errorf("unique index %q is %d chars, exceeds MySQL limit %d", uqIdx, len(uqIdx), mysqlMaxIdentifierLen)
	}
	// The relation column idx is the binding constraint; confirm it equals exactly the
	// limit at the cap, documenting that 26 is the largest cap that still fits.
	if len(relIdx) != mysqlMaxIdentifierLen {
		t.Errorf("relation index at cap = %d chars; expected exactly %d (cap of %d is the binding constraint)",
			len(relIdx), mysqlMaxIdentifierLen, maxIdentifierLen)
	}

	// Belt-and-suspenders: drive the real builders and confirm no error and the
	// generated SQL contains the expected (≤64-char) index names. BuildAddRelationColumn
	// produces the longest one.
	relSQL, err := BuildAddRelationColumn(entity, field)
	if err != nil {
		t.Fatalf("BuildAddRelationColumn(worst-case) error: %v", err)
	}
	if !strings.Contains(relSQL, "`"+relIdx+"`") {
		t.Errorf("BuildAddRelationColumn output missing expected index %q; got:\n%s", relIdx, relSQL)
	}
	uqSQL, err := BuildAddUniqueColumn(entity, field)
	if err != nil {
		t.Fatalf("BuildAddUniqueColumn(worst-case) error: %v", err)
	}
	if !strings.Contains(uqSQL, "`"+uqIdx+"`") {
		t.Errorf("BuildAddUniqueColumn output missing expected index %q; got:\n%s", uqIdx, uqSQL)
	}
}
