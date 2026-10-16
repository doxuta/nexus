package ddl_test

import (
	"strings"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestJoinParts verifies the canonical name helper: alphabetical sort, correct column names.
func TestJoinParts(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		wantTable string
		wantColA  string
		wantColB  string
		wantErr   bool
	}{
		{
			name:      "already sorted",
			a:         "accounts",
			b:         "contacts",
			wantTable: "rel_accounts_contacts",
			wantColA:  "accounts_id",
			wantColB:  "contacts_id",
		},
		{
			name:      "reversed input sorts alphabetically",
			a:         "zebra",
			b:         "alpha",
			wantTable: "rel_alpha_zebra",
			wantColA:  "alpha_id",
			wantColB:  "zebra_id",
		},
		{
			name:      "same-length names",
			a:         "orders",
			b:         "items",
			wantTable: "rel_items_orders",
			wantColA:  "items_id",
			wantColB:  "orders_id",
		},
		{
			name:    "self-referential rejected",
			a:       "contacts",
			b:       "contacts",
			wantErr: true,
		},
		{
			name:    "bad name a rejected",
			a:       "bad name!",
			b:       "contacts",
			wantErr: true,
		},
		{
			name:    "bad name b rejected",
			a:       "accounts",
			b:       "123bad",
			wantErr: true,
		},
		{
			name:    "reserved word rejected",
			a:       "select",
			b:       "contacts",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, colA, colB, err := ddl.JoinParts(tt.a, tt.b)
			if tt.wantErr {
				if err == nil {
					t.Errorf("JoinParts(%q, %q): expected error, got nil", tt.a, tt.b)
				}
				return
			}
			if err != nil {
				t.Fatalf("JoinParts(%q, %q): unexpected error: %v", tt.a, tt.b, err)
			}
			if table != tt.wantTable {
				t.Errorf("table: got %q, want %q", table, tt.wantTable)
			}
			if colA != tt.wantColA {
				t.Errorf("colA: got %q, want %q", colA, tt.wantColA)
			}
			if colB != tt.wantColB {
				t.Errorf("colB: got %q, want %q", colB, tt.wantColB)
			}
		})
	}
}

// TestBuildCreateJoinTable verifies the generated DDL shape.
func TestBuildCreateJoinTable(t *testing.T) {
	t.Run("basic shape accounts-contacts", func(t *testing.T) {
		sql, err := ddl.BuildCreateJoinTable("accounts", "contacts")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Table name must be alphabetically sorted.
		if !strings.Contains(sql, "rel_accounts_contacts") {
			t.Errorf("expected table name rel_accounts_contacts in DDL, got:\n%s", sql)
		}
		// Mandatory columns.
		for _, col := range []string{"`id`", "`tenant_id`", "`accounts_id`", "`contacts_id`", "`created_by`", "`created_at`"} {
			if !strings.Contains(sql, col) {
				t.Errorf("expected column %s in DDL, got:\n%s", col, sql)
			}
		}
		// NO obj_* mandatory columns that don't belong in join tables.
		for _, col := range []string{"`team_set_id`", "`modified_by`", "`updated_at`", "`deleted_at`", "`data`"} {
			if strings.Contains(sql, col) {
				t.Errorf("unexpected column %s found in join-table DDL:\n%s", col, sql)
			}
		}
		// Unique constraint scoped to tenant.
		if !strings.Contains(sql, "uq_accounts_contacts") {
			t.Errorf("expected unique key uq_accounts_contacts:\n%s", sql)
		}
		// Two side indexes (include rel_ prefix in the index name).
		if !strings.Contains(sql, "idx_rel_accounts_contacts_a") {
			t.Errorf("expected index idx_rel_accounts_contacts_a:\n%s", sql)
		}
		if !strings.Contains(sql, "idx_rel_accounts_contacts_b") {
			t.Errorf("expected index idx_rel_accounts_contacts_b:\n%s", sql)
		}
		// Must be idempotent.
		if !strings.Contains(sql, "IF NOT EXISTS") {
			t.Errorf("expected IF NOT EXISTS:\n%s", sql)
		}
	})

	t.Run("reversed input sorts alphabetically", func(t *testing.T) {
		sql, err := ddl.BuildCreateJoinTable("zebra", "alpha")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "rel_alpha_zebra") {
			t.Errorf("expected rel_alpha_zebra (sorted), got:\n%s", sql)
		}
		if !strings.Contains(sql, "`alpha_id`") {
			t.Errorf("expected alpha_id column:\n%s", sql)
		}
		if !strings.Contains(sql, "`zebra_id`") {
			t.Errorf("expected zebra_id column:\n%s", sql)
		}
	})

	t.Run("self-referential rejected", func(t *testing.T) {
		_, err := ddl.BuildCreateJoinTable("contacts", "contacts")
		if err == nil {
			t.Error("expected error for self-referential N-N, got nil")
		}
	})

	t.Run("bad entity name rejected", func(t *testing.T) {
		_, err := ddl.BuildCreateJoinTable("bad-name!", "contacts")
		if err == nil {
			t.Error("expected error for invalid entity name, got nil")
		}
	})

	t.Run("too-long entity name rejected", func(t *testing.T) {
		_, err := ddl.BuildCreateJoinTable("aaaaaaaaaaaaaaaaaaaaaaaaaaa", "contacts") // 27 chars
		if err == nil {
			t.Error("expected error for too-long entity name, got nil")
		}
	})

	t.Run("reserved word rejected", func(t *testing.T) {
		_, err := ddl.BuildCreateJoinTable("select", "contacts")
		if err == nil {
			t.Error("expected error for reserved word, got nil")
		}
	})
}
