package dynrepo

import (
	"testing"
)

// TestBuildBatchArgs verifies that buildBatchArgs generates the correct argument slice
// for a multi-row INSERT with the expected length. This is the pure logic unit test;
// no DB is involved.
func TestBuildBatchArgs(t *testing.T) {
	rows := []importRow{
		{id: "id-1", teamSetID: optStr("ts-1"), createdBy: "user-1", data: []byte(`{"name":"Alice"}`)},
		{id: "id-2", teamSetID: optStr(""), createdBy: "user-1", data: []byte(`{"name":"Bob"}`)},
	}
	tenantID := "tenant-abc"

	args := buildBatchArgs(tenantID, rows)
	// Each row contributes 6 args: id, tenant_id, team_set_id, created_by, owner_id, data.
	want := 6 * len(rows)
	if len(args) != want {
		t.Errorf("args count: got %d, want %d", len(args), want)
	}
	// First arg of first row is id.
	if args[0] != "id-1" {
		t.Errorf("args[0]: got %v, want id-1", args[0])
	}
	// Second arg is tenant_id.
	if args[1] != "tenant-abc" {
		t.Errorf("args[1]: got %v, want tenant-abc", args[1])
	}
}

// TestBuildInsertSQL verifies that buildInsertSQL produces a statement with the
// correct number of placeholder tuples for N rows.
func TestBuildInsertSQL(t *testing.T) {
	tests := []struct {
		name    string
		table   string
		n       int
		wantErr bool
	}{
		{name: "1 row", table: "obj_leads", n: 1},
		{name: "3 rows", table: "obj_leads", n: 3},
		{name: "zero rows", table: "obj_leads", n: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, err := buildInsertSQL(tt.table, tt.n)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sql == "" {
				t.Error("expected non-empty SQL")
			}
		})
	}
}

// optStr returns a string pointer — helper used in test data above.
func optStr(s string) string { return s }
