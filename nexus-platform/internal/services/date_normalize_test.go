package services

import "testing"

// TestRM48_DateTrimAndZeroYear covers RM-48: date/datetime values are trimmed before storage
// (checkType validates the trimmed form), and the zero/negative-year instant is rejected.
func TestRM48_DateTrimAndZeroYear(t *testing.T) {
	fields := []FieldSpec{
		{Name: "d", FieldType: "date"},
		{Name: "dt", FieldType: "datetime"},
	}

	// Whitespace-padded date is accepted AND stored trimmed (no verbatim " 2026-01-01 ").
	clean, err := ValidateRecord(fields, map[string]any{"d": "  2026-01-01  "})
	if err != nil {
		t.Fatalf("padded date: %v", err)
	}
	if clean["d"] != "2026-01-01" {
		t.Errorf("stored date = %q; want trimmed \"2026-01-01\"", clean["d"])
	}

	// Whitespace-padded datetime is trimmed AND canonicalized to the UTC MySQL DATETIME
	// literal (W5-D D1): "2026-01-01T10:00:00Z" → "2026-01-01 10:00:00". Storing the offset-free
	// UTC literal is what makes datetime eq/neq and ordered filters compare by instant and keeps
	// the value CAST(... AS DATETIME)-safe (see normalizeForStore / canonicalDatetimeUTC).
	clean, err = ValidateRecord(fields, map[string]any{"dt": " 2026-01-01T10:00:00Z "})
	if err != nil {
		t.Fatalf("padded datetime: %v", err)
	}
	if clean["dt"] != "2026-01-01 10:00:00" {
		t.Errorf("stored datetime = %q; want canonical UTC literal \"2026-01-01 10:00:00\" (D1)", clean["dt"])
	}

	// Zero-year date rejected.
	if _, err := ValidateRecord(fields, map[string]any{"d": "0000-01-01"}); err == nil {
		t.Error("zero-year date accepted; want RECORD_INVALID_DATE")
	}
	// Zero-year datetime rejected.
	if _, err := ValidateRecord(fields, map[string]any{"dt": "0000-01-01T00:00:00Z"}); err == nil {
		t.Error("zero-year datetime accepted; want RECORD_INVALID_DATE")
	}
	// A normal date still passes unchanged.
	if clean, err := ValidateRecord(fields, map[string]any{"d": "2026-06-25"}); err != nil || clean["d"] != "2026-06-25" {
		t.Errorf("normal date = (%v,%v); want (2026-06-25,nil)", clean["d"], err)
	}
}
