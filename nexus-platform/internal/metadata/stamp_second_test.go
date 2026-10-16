package metadata

import (
	"testing"
	"time"
)

func TestBeforeCreateFloorsToSecond(t *testing.T) {
	preset := time.Date(2026, 1, 2, 3, 4, 5, 900_000_000, time.UTC)
	tests := []struct {
		name string
		run  func() (created, updated time.Time)
	}{
		{"entity", func() (time.Time, time.Time) {
			e := &EntityDefinition{}
			_ = e.BeforeCreate(nil)
			return e.CreatedAt, e.UpdatedAt
		}},
		{"field", func() (time.Time, time.Time) {
			f := &FieldDefinition{}
			_ = f.BeforeCreate(nil)
			return f.CreatedAt, f.UpdatedAt
		}},
		{"relationship", func() (time.Time, time.Time) {
			r := &RelationshipDefinition{}
			_ = r.BeforeCreate(nil)
			return r.CreatedAt, r.UpdatedAt
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, u := tc.run()
			if c.IsZero() || c.Nanosecond() != 0 || !u.Equal(c) {
				t.Fatalf("created=%v updated=%v; want equal whole-second stamps", c, u)
			}
		})
	}
	// an explicit value (restore, import) is kept untouched
	f := &FieldDefinition{CreatedAt: preset}
	_ = f.BeforeCreate(nil)
	if !f.CreatedAt.Equal(preset) {
		t.Fatalf("explicit created_at overwritten: %v", f.CreatedAt)
	}
}
