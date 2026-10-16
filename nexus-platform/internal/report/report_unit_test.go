package report

import (
	"encoding/json"
	"testing"
	"time"
)

// TestValidType pins the sys_reports.type enum: exactly the three M2 shapes pass, everything
// else (including case variants — the ENUM is lowercase) is rejected.
func TestValidType(t *testing.T) {
	tests := []struct {
		name string
		in   Type
		want bool
	}{
		{name: "tabular hợp lệ", in: TypeTabular, want: true},
		{name: "summary hợp lệ", in: TypeSummary, want: true},
		{name: "matrix hợp lệ", in: TypeMatrix, want: true},
		{name: "chuỗi rỗng bị từ chối", in: Type(""), want: false},
		{name: "shape lạ bị từ chối", in: Type("pivot"), want: false},
		{name: "viết hoa bị từ chối (enum lowercase)", in: Type("TABULAR"), want: false},
		{name: "khoảng trắng thừa bị từ chối", in: Type(" summary"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidType(tt.in); got != tt.want {
				t.Errorf("ValidType(%q) = %v; want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestRowToRecord covers the row→admin-view projection: fields are forwarded verbatim and an
// empty/NULL stored config is normalized to the JSON literal "null" so the API never emits an
// invalid empty json.RawMessage.
func TestRowToRecord(t *testing.T) {
	now := time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)
	creator := "user-1"

	t.Run("forward mọi field nguyên văn", func(t *testing.T) {
		row := reportRow{
			ID:         "rep-1",
			TenantID:   "tenant-a",
			Name:       "Doanh thu theo stage",
			EntityName: "orders",
			Type:       "summary",
			Config:     json.RawMessage(`{"groupBy":"stage"}`),
			IsActive:   true,
			CreatedBy:  &creator,
			UpdatedBy:  nil,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		got := rowToRecord(&row)
		if got.ID != "rep-1" || got.TenantID != "tenant-a" || got.Name != "Doanh thu theo stage" {
			t.Errorf("identity fields mismatched: %+v", got)
		}
		if got.EntityName != "orders" || got.Type != "summary" || !got.IsActive {
			t.Errorf("definition fields mismatched: %+v", got)
		}
		if string(got.Config) != `{"groupBy":"stage"}` {
			t.Errorf("Config = %s; want forwarded verbatim", got.Config)
		}
		if got.CreatedBy == nil || *got.CreatedBy != "user-1" {
			t.Errorf("CreatedBy = %v; want user-1", got.CreatedBy)
		}
		if got.UpdatedBy != nil {
			t.Errorf("UpdatedBy = %v; want nil", got.UpdatedBy)
		}
		if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
			t.Errorf("timestamps mismatched: %+v", got)
		}
	})

	t.Run("config rỗng chuẩn hoá thành literal null", func(t *testing.T) {
		got := rowToRecord(&reportRow{ID: "rep-2"})
		if string(got.Config) != "null" {
			t.Errorf("Config = %q; want \"null\"", got.Config)
		}
		if !json.Valid(got.Config) {
			t.Errorf("Config %q is not valid JSON", got.Config)
		}
	})
}
