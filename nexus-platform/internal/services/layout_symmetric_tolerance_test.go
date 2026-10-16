package services

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/metadata"
)

// Nước 3 — SYMMETRIC serve-time tolerance: fields born AFTER the stored layout's last save
// auto-append into a trailing "Mới thêm" section; older (deliberately omitted) fields do not.
func TestAppendFieldsNewerThan(t *testing.T) {
	saved := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	layout := map[string]any{
		"sections": []any{
			map[string]any{"title": "Chung", "columns": float64(2), "fields": []any{"name", map[string]any{"field": "status"}}},
		},
	}
	fields := []metadata.FieldDefinition{
		{Name: "name", CreatedAt: saved.Add(-24 * time.Hour)},
		{Name: "status", CreatedAt: saved.Add(-24 * time.Hour)},
		// Deliberately omitted BEFORE the save — stays omitted (save bumped UpdatedAt above it).
		{Name: "old_omitted", CreatedAt: saved.Add(-1 * time.Hour)},
		// Born after the save — must auto-appear.
		{Name: "fresh_field", CreatedAt: saved.Add(48 * time.Hour)},
	}

	out := appendFieldsNewerThan(layout, fields, saved)
	b, _ := json.Marshal(out)
	s := string(b)

	if !containsStr(s, "Mới thêm") || !containsStr(s, "fresh_field") {
		t.Fatalf("fresh field must append into a 'Mới thêm' section: %s", s)
	}
	if containsStr(s, "old_omitted") {
		t.Fatalf("deliberately-omitted pre-save field must NOT re-append: %s", s)
	}

	// No fresh fields → structurally unchanged (no empty trailing section).
	out2 := appendFieldsNewerThan(layout, fields[:3], saved)
	b2, _ := json.Marshal(out2)
	if containsStr(string(b2), "Mới thêm") {
		t.Fatalf("no-op case must not add a section: %s", string(b2))
	}

	// Referenced-but-new field (rare: layout saved referencing it, clock skew) must not duplicate.
	out3 := appendFieldsNewerThan(layout, []metadata.FieldDefinition{
		{Name: "status", CreatedAt: saved.Add(time.Hour)},
	}, saved)
	b3, _ := json.Marshal(out3)
	if containsStr(string(b3), "Mới thêm") {
		t.Fatalf("already-referenced field must not re-append: %s", string(b3))
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
