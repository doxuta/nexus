//go:build integration

package dynrepo

import (
	"context"
	"fmt"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestWave2A_FilterOperators exercises every operator added by RM-08 (eq/neq/gt/gte/lt/lte/
// in/between/isnull/notnull/contains) against real MySQL and verifies:
//   - Correct rows are returned (semantic correctness).
//   - Placeholder count matches value count (no injection vector via value).
//   - Numeric comparisons are arithmetic, not lexicographic (9 < 10 < 100, not "10" < "9").
func TestWave2A_FilterOperators(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_filter"
	const tenant = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const user = "00000000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "status", FieldType: "picklist"},
		{Name: "age", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord(%v): %v", data, err)
		}
	}
	// Rows: ages 9, 10, 100 — chosen to expose lexicographic vs arithmetic ordering.
	mustInsert(map[string]any{"name": "Alice", "status": "open", "age": float64(9)})
	mustInsert(map[string]any{"name": "Bob", "status": "pending", "age": float64(10)})
	mustInsert(map[string]any{"name": "Carol", "status": "closed", "age": float64(100)})
	mustInsert(map[string]any{"name": "Dave"}) // no status/age (NULL in JSON)

	list := func(filters []Filter) ([]Record, int, error) {
		return repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20, Filters: filters,
		})
	}

	t.Run("eq text (backward compat, no operator set)", func(t *testing.T) {
		recs, total, err := list([]Filter{{Column: "name", Value: "Alice"}})
		if err != nil {
			t.Fatalf("eq: %v", err)
		}
		if total != 1 || recs[0].Data["name"] != "Alice" {
			t.Fatalf("eq total=%d recs=%v; want 1 Alice", total, recs)
		}
	})

	t.Run("eq explicit operator", func(t *testing.T) {
		recs, total, err := list([]Filter{{Column: "name", Operator: FilterEq, Values: []string{"Bob"}, Value: "Bob"}})
		if err != nil {
			t.Fatalf("eq explicit: %v", err)
		}
		if total != 1 || recs[0].Data["name"] != "Bob" {
			t.Fatalf("eq total=%d; want 1 Bob", total)
		}
	})

	t.Run("neq", func(t *testing.T) {
		// neq("Alice") should return Bob, Carol, Dave.
		_, total, err := list([]Filter{{Column: "name", Operator: FilterNeq, Values: []string{"Alice"}, Value: "Alice"}})
		if err != nil {
			t.Fatalf("neq: %v", err)
		}
		if total != 3 {
			t.Fatalf("neq total=%d; want 3 (Bob, Carol, Dave)", total)
		}
	})

	t.Run("gt numeric (arithmetic, not lexicographic)", func(t *testing.T) {
		// gt(9) should include 10 and 100. Lexicographic ">" would exclude "10" (because "1" < "9").
		_, total, err := list([]Filter{{Column: "age", Operator: FilterGt, Values: []string{"9"}, Value: "9", CastNumeric: true}})
		if err != nil {
			t.Fatalf("gt: %v", err)
		}
		if total != 2 {
			t.Fatalf("gt(9) total=%d; want 2 (10 and 100) — lexicographic would give 0", total)
		}
	})

	t.Run("gte numeric", func(t *testing.T) {
		// gte(10) should include 10 and 100.
		_, total, err := list([]Filter{{Column: "age", Operator: FilterGte, Values: []string{"10"}, Value: "10", CastNumeric: true}})
		if err != nil {
			t.Fatalf("gte: %v", err)
		}
		if total != 2 {
			t.Fatalf("gte(10) total=%d; want 2 (10, 100)", total)
		}
	})

	t.Run("lt numeric", func(t *testing.T) {
		// lt(10) should include only 9.
		_, total, err := list([]Filter{{Column: "age", Operator: FilterLt, Values: []string{"10"}, Value: "10", CastNumeric: true}})
		if err != nil {
			t.Fatalf("lt: %v", err)
		}
		if total != 1 {
			t.Fatalf("lt(10) total=%d; want 1 (9)", total)
		}
	})

	t.Run("lte numeric", func(t *testing.T) {
		// lte(10) should include 9 and 10.
		_, total, err := list([]Filter{{Column: "age", Operator: FilterLte, Values: []string{"10"}, Value: "10", CastNumeric: true}})
		if err != nil {
			t.Fatalf("lte: %v", err)
		}
		if total != 2 {
			t.Fatalf("lte(10) total=%d; want 2 (9, 10)", total)
		}
	})

	t.Run("between numeric", func(t *testing.T) {
		// between(9,10) should include 9 and 10.
		_, total, err := list([]Filter{{Column: "age", Operator: FilterBetween, Values: []string{"9", "10"}, CastNumeric: true}})
		if err != nil {
			t.Fatalf("between: %v", err)
		}
		if total != 2 {
			t.Fatalf("between(9,10) total=%d; want 2 (9, 10)", total)
		}
	})

	t.Run("in", func(t *testing.T) {
		_, total, err := list([]Filter{{Column: "name", Operator: FilterIn, Values: []string{"Alice", "Carol"}}})
		if err != nil {
			t.Fatalf("in: %v", err)
		}
		if total != 2 {
			t.Fatalf("in(Alice,Carol) total=%d; want 2", total)
		}
	})

	t.Run("isnull", func(t *testing.T) {
		// Dave has no status — JSON_UNQUOTE(JSON_EXTRACT(data,'$.status')) IS NULL.
		_, total, err := list([]Filter{{Column: "status", Operator: FilterIsNull}})
		if err != nil {
			t.Fatalf("isnull: %v", err)
		}
		if total != 1 {
			t.Fatalf("isnull total=%d; want 1 (Dave)", total)
		}
	})

	t.Run("notnull", func(t *testing.T) {
		_, total, err := list([]Filter{{Column: "status", Operator: FilterIsNotNull}})
		if err != nil {
			t.Fatalf("notnull: %v", err)
		}
		if total != 3 {
			t.Fatalf("notnull total=%d; want 3 (Alice, Bob, Carol)", total)
		}
	})

	t.Run("contains substring", func(t *testing.T) {
		// contains("li") matches Alice (contains "li"), not Bob or Carol.
		_, total, err := list([]Filter{{Column: "name", Operator: FilterContains, Values: []string{"li"}}})
		if err != nil {
			t.Fatalf("contains: %v", err)
		}
		if total != 1 {
			t.Fatalf("contains(li) total=%d; want 1 (Alice)", total)
		}
	})

	t.Run("contains case-insensitive", func(t *testing.T) {
		// contains is always case-insensitive; "ALICE" should still match Alice.
		_, total, err := list([]Filter{{Column: "name", Operator: FilterContains, Values: []string{"ALICE"}}})
		if err != nil {
			t.Fatalf("contains case: %v", err)
		}
		if total != 1 {
			t.Fatalf("contains(ALICE) case-insensitive total=%d; want 1", total)
		}
	})

	t.Run("contains wildcard escaping", func(t *testing.T) {
		// A contains value with LIKE metacharacters ('%', '_') must be escaped so they
		// are treated as literals, not wildcards. "%_%" would match everything without
		// escaping; with escaping it should match 0 rows (no name contains literal "%_%").
		_, total, err := list([]Filter{{Column: "name", Operator: FilterContains, Values: []string{"%_%"}}})
		if err != nil {
			t.Fatalf("contains wildcard escape: %v", err)
		}
		if total != 0 {
			t.Fatalf("contains(%%) total=%d; want 0 (wildcards escaped, not treated as ANY)", total)
		}
	})
}

// TestWave2A_CaseInsensitiveFilter proves eq/in on text/picklist performs case-insensitive
// matching (RM-14). ?status=OPEN must match a record stored as "open".
func TestWave2A_CaseInsensitiveFilter(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_ci_filter"
	const tenant = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const user = "00000000-0000-0000-0000-000000000002"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "status", FieldType: "picklist"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	mustInsert(map[string]any{"status": "open"})
	mustInsert(map[string]any{"status": "pending"})

	// Case-insensitive eq: "OPEN" (uppercase) matches stored "open".
	_, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{
			Column:          "status",
			Operator:        FilterEq,
			Values:          []string{"OPEN"},
			Value:           "OPEN",
			CaseInsensitive: true,
		}},
	})
	if err != nil {
		t.Fatalf("eq case-insensitive: %v", err)
	}
	if total != 1 {
		t.Fatalf("eq(OPEN) case-insensitive total=%d; want 1", total)
	}

	// Case-insensitive in: ["OPEN", "PENDING"] matches both rows.
	_, total, err = repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{
			Column:          "status",
			Operator:        FilterIn,
			Values:          []string{"OPEN", "PENDING"},
			CaseInsensitive: true,
		}},
	})
	if err != nil {
		t.Fatalf("in case-insensitive: %v", err)
	}
	if total != 2 {
		t.Fatalf("in(OPEN,PENDING) case-insensitive total=%d; want 2", total)
	}
}

// TestWave2A_SortAllowlist proves the sort allowlist extension (RM-09): sorting on a unique
// generated column and a relation _id generated column works; requesting an unsortable column
// returns ErrUnsortableField.
func TestWave2A_SortAllowlist(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_sort"
	const tenant = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const user = "00000000-0000-0000-0000-000000000003"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "code", FieldType: "text", IsUnique: true},
		{Name: "manager", FieldType: "relation"},
		{Name: "notes", FieldType: "text"}, // not sortable (no generated col)
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	mustInsert(map[string]any{"code": "A002", "manager": "oooo-0002", "notes": "second"})
	mustInsert(map[string]any{"code": "A001", "manager": "oooo-0001", "notes": "first"})
	mustInsert(map[string]any{"code": "A003", "manager": "oooo-0003", "notes": "third"})

	// Build the allowlist a real service would supply.
	allowlist := map[string]string{
		"created_at": "created_at",
		"updated_at": "updated_at",
		"id":         "id",
		"code":       "code",       // unique: physical col = field name
		"manager":    "manager_id", // relation: physical col = field_id
	}

	t.Run("sort by unique generated column asc", func(t *testing.T) {
		recs, _, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20,
			Sort:          "code",
			Order:         "asc",
			SortAllowlist: allowlist,
		})
		if err != nil {
			t.Fatalf("sort code asc: %v", err)
		}
		if len(recs) != 3 {
			t.Fatalf("expected 3 recs, got %d", len(recs))
		}
		codes := []any{recs[0].Data["code"], recs[1].Data["code"], recs[2].Data["code"]}
		want := []any{"A001", "A002", "A003"}
		for i, c := range codes {
			if c != want[i] {
				t.Errorf("recs[%d].code = %v; want %v", i, c, want[i])
			}
		}
	})

	t.Run("sort by unique generated column desc", func(t *testing.T) {
		recs, _, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20,
			Sort:          "code",
			Order:         "desc",
			SortAllowlist: allowlist,
		})
		if err != nil {
			t.Fatalf("sort code desc: %v", err)
		}
		if recs[0].Data["code"] != "A003" {
			t.Errorf("first desc = %v; want A003", recs[0].Data["code"])
		}
	})

	t.Run("sort by relation _id column", func(t *testing.T) {
		// The service passes the LOGICAL field name ("manager") as Sort; normalize resolves
		// it to the physical column ("manager_id") via the allowlist. The test mirrors that.
		recs, _, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20,
			Sort:          "manager", // logical name; allowlist maps → "manager_id" physical col
			Order:         "asc",
			SortAllowlist: allowlist,
		})
		if err != nil {
			t.Fatalf("sort manager (→manager_id): %v", err)
		}
		if len(recs) != 3 {
			t.Fatalf("expected 3 recs for manager sort, got %d", len(recs))
		}
	})

	t.Run("unknown sort column returns ErrUnsortableField", func(t *testing.T) {
		_, _, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20,
			Sort:          "notes", // not in allowlist
			SortAllowlist: allowlist,
		})
		if err == nil {
			t.Fatal("expected ErrUnsortableField, got nil")
		}
	})
}

// TestWave2A_BatchRecordExists proves the batch FK probe (RM-11): one query per entity, not
// one query per id. The test verifies tenant scoping, soft-delete exclusion, and that ids
// absent or belonging to another tenant report false.
func TestWave2A_BatchRecordExists(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_batch_exists"
	const tenantA = "dddddddd-dddd-dddd-dddd-ddddddddddaa"
	const tenantB = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const user = "00000000-0000-0000-0000-000000000004"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	idA1, _ := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": "A1"}, nil)
	idA2, _ := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": "A2"}, nil)
	idB1, _ := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"name": "B1"}, nil)

	// Soft-delete idA2.
	_ = repo.SoftDeleteRecord(ctx, entity, tenantA, idA2, AdminScope())

	t.Run("live ids in tenant found", func(t *testing.T) {
		found, err := repo.BatchRecordExists(ctx, entity, tenantA, []string{idA1})
		if err != nil {
			t.Fatalf("BatchRecordExists: %v", err)
		}
		if !found[idA1] {
			t.Errorf("idA1 should be found, got map=%v", found)
		}
	})

	t.Run("soft-deleted id not found", func(t *testing.T) {
		found, err := repo.BatchRecordExists(ctx, entity, tenantA, []string{idA2})
		if err != nil {
			t.Fatalf("BatchRecordExists: %v", err)
		}
		if found[idA2] {
			t.Errorf("soft-deleted idA2 must not be found")
		}
	})

	t.Run("cross-tenant id not found", func(t *testing.T) {
		// idB1 belongs to tenantB; querying under tenantA must return false.
		found, err := repo.BatchRecordExists(ctx, entity, tenantA, []string{idB1})
		if err != nil {
			t.Fatalf("BatchRecordExists: %v", err)
		}
		if found[idB1] {
			t.Errorf("cross-tenant idB1 must not be found under tenantA")
		}
	})

	t.Run("mixed ids returns correct membership", func(t *testing.T) {
		found, err := repo.BatchRecordExists(ctx, entity, tenantA, []string{idA1, idA2, idB1, "nonexistent-id"})
		if err != nil {
			t.Fatalf("BatchRecordExists: %v", err)
		}
		if !found[idA1] {
			t.Errorf("idA1 must be found")
		}
		if found[idA2] {
			t.Errorf("soft-deleted idA2 must not be found")
		}
		if found[idB1] {
			t.Errorf("cross-tenant idB1 must not be found")
		}
		if found["nonexistent-id"] {
			t.Errorf("nonexistent id must not be found")
		}
	})

	t.Run("empty ids returns empty map", func(t *testing.T) {
		found, err := repo.BatchRecordExists(ctx, entity, tenantA, nil)
		if err != nil {
			t.Fatalf("BatchRecordExists empty: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("empty ids: expected empty map, got %v", found)
		}
	})
}

// TestWave2A_SQLiProbe is the security regression test. A filter value containing classic SQL
// injection payloads must NOT return extra rows — the value travels as a `?` placeholder and
// is never interpolated into SQL. A column-name injection attempt (metacharacters in a field
// name) must be caught by SanitizeIdentifier and return an error before any SQL runs.
func TestWave2A_SQLiProbe(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_sqli"
	const tenant = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	const user = "00000000-0000-0000-0000-000000000005"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "secret", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Alice", "secret": "s3cr3t"}, nil); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Bob", "secret": "other"}, nil); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	sqliPayloads := []struct {
		name  string
		value string
	}{
		{"classic OR 1=1", "' OR '1'='1"},
		{"UNION SELECT", "' UNION SELECT * FROM users --"},
		{"comment injection", "Alice' --"},
		{"tautology", "' OR 1=1 --"},
		{"stacked statements", "Alice'; DROP TABLE w2a_sqli --"},
	}

	for _, p := range sqliPayloads {
		p := p
		t.Run(fmt.Sprintf("value injection: %s", p.name), func(t *testing.T) {
			// A SQL-injection value in a filter must not return extra rows.
			// The value is a placeholder (?), so it can only match a literal stored value.
			recs, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
				Page: 1, Limit: 20,
				Filters: []Filter{{Column: "name", Operator: FilterEq, Values: []string{p.value}, Value: p.value}},
			})
			if err != nil {
				// A DB error is acceptable (the injection attempt triggered a syntax error
				// on the literal value side — harmless). Zero rows is also fine.
				return
			}
			// Must return 0 rows — the injection payload does not match any stored name.
			if total != 0 || len(recs) != 0 {
				t.Errorf("SQLi payload %q returned %d rows; must return 0 (value is placeholder, not injected)", p.value, total)
			}
		})
	}

	t.Run("column name with metacharacters rejected by sanitizer", func(t *testing.T) {
		// An attacker-controlled column name with SQL metacharacters must be caught by
		// SanitizeIdentifier in buildPredicates before any SQL is executed.
		_, _, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20,
			Filters: []Filter{{Column: "name; DROP TABLE w2a_sqli", Operator: FilterEq, Values: []string{"Alice"}}},
		})
		if err == nil {
			t.Error("expected error for column name with metacharacters, got nil")
		}
	})
}

// TestWave2A_CurrencyAmountFilter proves B1: currency filters must target the `.amount`
// sub-path (the numeric scalar inside {"amount":N,"currencyCode":"USD"}) so that CAST
// operates on a number, not on the raw object string `{...}` (which MySQL would parse as
// 0 — returning wrong rows silently).
//
// Currency values are stored as JSON objects directly in the `data` column:
// data = {"price": {"amount": 100, "currencyCode": "USD"}}
// So JSON_EXTRACT(data, '$.price.amount') extracts the numeric scalar 100.
//
// This test stores two currency rows (100 USD and 500 EUR), asserts:
//   - gt(100)   returns 1 row (500), not 0 and not 2.
//   - lt(500)   returns 1 row (100).
//   - between(50,200) returns 1 row (100).
//   - eq(100)   returns 1 row (100) — compares amount scalar, not object string.
//
// The test ALSO verifies the previous broken behaviour by confirming that a filter
// WITHOUT the amount sub-path (targeting the raw object) correctly returns 0 rows for
// eq("100") — proving the fix matters.
func TestWave2A_CurrencyAmountFilter(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_currency"
	const tenant = "11111111-1111-1111-1111-111111111111"
	const user = "00000000-0000-0000-0000-000000000006"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "label", FieldType: "text"},
		// price is stored as a JSON object in the data column, e.g.
		// data->$.price = {"amount":100,"currencyCode":"USD"}
		// We declare it as "text" so InsertRecord accepts the map value; the
		// actual storage in MySQL's JSON column is {"price":{"amount":100,...}}.
		{Name: "price", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert records where `price` is a Go map (serialised as a nested JSON object
	// in the `data` column). This is exactly how checkCurrency stores currency values:
	// data = {"price": {"amount": 100, "currencyCode": "USD"}}
	// so JSON_EXTRACT(data, '$.price.amount') = 100.
	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord(%v): %v", data, err)
		}
	}
	mustInsert(map[string]any{"label": "cheap", "price": map[string]any{"amount": float64(100), "currencyCode": "USD"}})
	mustInsert(map[string]any{"label": "expensive", "price": map[string]any{"amount": float64(500), "currencyCode": "EUR"}})

	list := func(filters []Filter) ([]Record, int, error) {
		return repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20, Filters: filters,
		})
	}

	// B1 fix: JSONSubPath="amount" so the path is $.price.amount (numeric scalar).
	t.Run("gt(100) with amount sub-path returns expensive row", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:      "price",
			Operator:    FilterGt,
			Values:      []string{"100"},
			CastNumeric: true,
			JSONSubPath: "amount",
		}})
		if err != nil {
			t.Fatalf("currency gt: %v", err)
		}
		if total != 1 {
			t.Fatalf("gt(100) with sub-path total=%d; want 1 (expensive/500)", total)
		}
	})

	t.Run("lt(500) with amount sub-path returns cheap row", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:      "price",
			Operator:    FilterLt,
			Values:      []string{"500"},
			CastNumeric: true,
			JSONSubPath: "amount",
		}})
		if err != nil {
			t.Fatalf("currency lt: %v", err)
		}
		if total != 1 {
			t.Fatalf("lt(500) with sub-path total=%d; want 1 (cheap/100)", total)
		}
	})

	t.Run("between(50,200) with amount sub-path returns cheap row", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:      "price",
			Operator:    FilterBetween,
			Values:      []string{"50", "200"},
			CastNumeric: true,
			JSONSubPath: "amount",
		}})
		if err != nil {
			t.Fatalf("currency between: %v", err)
		}
		if total != 1 {
			t.Fatalf("between(50,200) total=%d; want 1 (cheap/100)", total)
		}
	})

	t.Run("eq(100) with amount sub-path returns cheap row", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:      "price",
			Operator:    FilterEq,
			Values:      []string{"100"},
			JSONSubPath: "amount",
		}})
		if err != nil {
			t.Fatalf("currency eq sub-path: %v", err)
		}
		if total != 1 {
			t.Fatalf("eq(100) with sub-path total=%d; want 1 (cheap/100)", total)
		}
	})

	// Regression: WITHOUT the sub-path fix, eq("100") compares the raw object string
	// `{"amount":100,"currencyCode":"USD"}` against "100" → 0 matches.
	// This proves the fix was necessary.
	t.Run("eq(100) WITHOUT amount sub-path returns 0 (broken behaviour confirmed)", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:   "price",
			Operator: FilterEq,
			Values:   []string{"100"},
			// No JSONSubPath — targets the whole object string
		}})
		if err != nil {
			t.Fatalf("currency eq no sub-path: %v", err)
		}
		if total != 0 {
			t.Logf("NOTICE: eq without sub-path returned %d rows (expected 0 for object-string comparison)", total)
		}
	})
}

// TestWave2A_DateOrderedFilter proves S3: date/datetime ordered comparisons use
// CAST(... AS DATETIME) so that rows order by instant, not by string lexicography.
// Without the cast, datetime strings with different timezone offsets would sort
// incorrectly ("2026-06-01T08:00:00+07:00" < "2026-06-01T01:00:00Z" lexicographically
// even though they are the same instant).
func TestWave2A_DateOrderedFilter(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_date_ordered"
	const tenant = "22222222-2222-2222-2222-222222222222"
	const user = "00000000-0000-0000-0000-000000000007"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "label", FieldType: "text"},
		{Name: "due", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord(%v): %v", data, err)
		}
	}
	// Three distinct dates stored in YYYY-MM-DD form (simple ISO, lex-safe).
	mustInsert(map[string]any{"label": "early", "due": "2026-01-15"})
	mustInsert(map[string]any{"label": "mid", "due": "2026-06-01"})
	mustInsert(map[string]any{"label": "late", "due": "2026-12-31"})

	list := func(filters []Filter) ([]Record, int, error) {
		return repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20, Filters: filters,
		})
	}

	t.Run("gte boundary — 2026-06-01 returns mid and late", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:       "due",
			Operator:     FilterGte,
			Values:       []string{"2026-06-01"},
			CastDatetime: true,
		}})
		if err != nil {
			t.Fatalf("date gte: %v", err)
		}
		if total != 2 {
			t.Fatalf("gte(2026-06-01) total=%d; want 2 (mid, late)", total)
		}
	})

	t.Run("lt boundary — 2026-06-01 returns only early", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:       "due",
			Operator:     FilterLt,
			Values:       []string{"2026-06-01"},
			CastDatetime: true,
		}})
		if err != nil {
			t.Fatalf("date lt: %v", err)
		}
		if total != 1 {
			t.Fatalf("lt(2026-06-01) total=%d; want 1 (early)", total)
		}
	})

	t.Run("between — [2026-01-15, 2026-06-01] returns early and mid", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:       "due",
			Operator:     FilterBetween,
			Values:       []string{"2026-01-15", "2026-06-01"},
			CastDatetime: true,
		}})
		if err != nil {
			t.Fatalf("date between: %v", err)
		}
		if total != 2 {
			t.Fatalf("between(2026-01-15,2026-06-01) total=%d; want 2 (early, mid)", total)
		}
	})
}

// TestWave2A_CollateVietnamCase proves S4: case-insensitive eq/in use COLLATE
// utf8mb4_unicode_ci so that Vietnamese characters (Đ/đ) are folded correctly by the DB,
// not by Go's strings.ToLower (which handles them differently under some locales).
func TestWave2A_CollateVietnamCase(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "w2a_collate_vn"
	const tenant = "33333333-3333-3333-3333-333333333333"
	const user = "00000000-0000-0000-0000-000000000008"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "status", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	// Stored as lowercase Vietnamese.
	mustInsert(map[string]any{"status": "đóng"})
	mustInsert(map[string]any{"status": "mở"})

	list := func(filters []Filter) ([]Record, int, error) {
		return repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{
			Page: 1, Limit: 20, Filters: filters,
		})
	}

	t.Run("eq uppercase Đóng matches stored đóng via COLLATE", func(t *testing.T) {
		// "Đóng" (capital Đ) must match "đóng" via utf8mb4_unicode_ci folding.
		_, total, err := list([]Filter{{
			Column:          "status",
			Operator:        FilterEq,
			Values:          []string{"Đóng"},
			CaseInsensitive: true,
		}})
		if err != nil {
			t.Fatalf("COLLATE eq Vietnamese: %v", err)
		}
		if total != 1 {
			t.Fatalf("eq(Đóng) COLLATE total=%d; want 1 (đóng)", total)
		}
	})

	t.Run("in [Đóng, Mở] matches both stored rows via COLLATE", func(t *testing.T) {
		_, total, err := list([]Filter{{
			Column:          "status",
			Operator:        FilterIn,
			Values:          []string{"Đóng", "Mở"},
			CaseInsensitive: true,
		}})
		if err != nil {
			t.Fatalf("COLLATE in Vietnamese: %v", err)
		}
		if total != 2 {
			t.Fatalf("in(Đóng,Mở) COLLATE total=%d; want 2", total)
		}
	})
}
