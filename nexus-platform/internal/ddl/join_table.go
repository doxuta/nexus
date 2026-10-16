package ddl

import (
	"fmt"
	"sort"
	"strings"
)

// JoinParts derives the canonical join-table name and the two side-column names from a pair
// of entity names. The pair is sorted alphabetically so JoinParts("b","a") == JoinParts("a","b")
// — caller and callee always agree on the physical table name and column assignment without
// needing to persist which side is "left" or "right".
//
// Returns (tableName, colA, colB) where tableName = "rel_<first>_<second>", colA = "<first>_id",
// colB = "<second>_id" with <first> < <second> alphabetically.
//
// Rejects self-referential pairs (same entity on both sides) because the two FK columns would
// share the same name and produce broken DDL. Returns an error for any invalid name.
func JoinParts(entityA, entityB string) (tableName, colA, colB string, err error) {
	a, err := SanitizeIdentifier(entityA)
	if err != nil {
		return "", "", "", fmt.Errorf("ddl.JoinParts: entityA: %w", err)
	}
	b, err := SanitizeIdentifier(entityB)
	if err != nil {
		return "", "", "", fmt.Errorf("ddl.JoinParts: entityB: %w", err)
	}
	if a == b {
		return "", "", "", fmt.Errorf("ddl.JoinParts: self-referential N-N (%q = %q) is not supported", a, b)
	}

	// Sort to get the canonical order so the table name is deterministic regardless of which
	// side the caller thinks is "parent".
	pair := []string{a, b}
	sort.Strings(pair)
	first, second := pair[0], pair[1]

	tableName = "rel_" + first + "_" + second
	colA = first + "_id"
	colB = second + "_id"
	return tableName, colA, colB, nil
}

// BuildCreateJoinTable generates the CREATE TABLE IF NOT EXISTS statement for an N-N join
// table between two entities. The table name and column names are derived from JoinParts
// (alphabetically sorted, deterministic). The schema is intentionally lean — no team_set_id,
// no updated_at, no deleted_at, no data JSON — because join-table rows are purely link records
// (unlink = hard DELETE, which is instant and removes no tenant data).
//
// Schema (per ../specs/035-relationships-n-n/spec.md §2):
//
//	id          CHAR(36) PK
//	tenant_id   CHAR(36) NOT NULL
//	<first>_id  CHAR(36) NOT NULL
//	<second>_id CHAR(36) NOT NULL
//	created_by  CHAR(36) NOT NULL
//	created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
//	UNIQUE KEY (tenant_id, <first>_id, <second>_id)
//	INDEX (tenant_id, <first>_id)
//	INDEX (tenant_id, <second>_id)
func BuildCreateJoinTable(entityA, entityB string) (string, error) {
	tableName, colA, colB, err := JoinParts(entityA, entityB)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildCreateJoinTable: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS `%s` (\n", tableName)
	b.WriteString("  `id` CHAR(36) NOT NULL,\n")
	b.WriteString("  `tenant_id` CHAR(36) NOT NULL,\n")
	fmt.Fprintf(&b, "  `%s` CHAR(36) NOT NULL,\n", colA)
	fmt.Fprintf(&b, "  `%s` CHAR(36) NOT NULL,\n", colB)
	b.WriteString("  `created_by` CHAR(36) NOT NULL,\n")
	b.WriteString("  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,\n")
	b.WriteString("  PRIMARY KEY (`id`),\n")
	fmt.Fprintf(&b, "  UNIQUE KEY `uq_%s_%s` (`tenant_id`, `%s`, `%s`),\n",
		colA[:len(colA)-3], colB[:len(colB)-3], // strip "_id" suffix for readability
		colA, colB)
	fmt.Fprintf(&b, "  INDEX `idx_%s_a` (`tenant_id`, `%s`),\n", tableName, colA)
	fmt.Fprintf(&b, "  INDEX `idx_%s_b` (`tenant_id`, `%s`)\n", tableName, colB)
	b.WriteString(") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;")

	return b.String(), nil
}
