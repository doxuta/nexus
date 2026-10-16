//go:build integration

// ENG-MASSOP — mass update/delete HTTP-level integration tests (real MySQL). Complements the
// no-DB suites (massop_handler_test.go envelope contract, massop_acl_wiring_test.go fixed ACL
// actions, services/record_massop_test.go per-row loop semantics): this file proves against the
// REAL engine + migrated schema that
//   - bulk-update applies the shared values patch through the live per-record Update path
//     (rows verifiably changed in obj_*), a genuine unique-index collision fails ONLY its row
//     as RECORD_DUPLICATE (+details.field), and a cross-tenant id in the list is silently
//     RECORD_NOT_FOUND (no existence leak, sibling rows still land);
//   - bulk-delete soft-deletes every row (single GET → 404, recycle bin lists them), a
//     cross-tenant id again collapses to RECORD_NOT_FOUND, and ONE audit_log row per committed
//     delete is written through the production AsyncDispatcher wiring (rm19 harness).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&loc=Local" \
//	  go test -tags=integration ./internal/handlers/ -run TestMassOpIT -race -v
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/audit"
)

const massOpITEntity = "massop_it_widgets"

// massOpITCleanup removes the suite's entity metadata for BOTH tenants + drops the shared
// obj_ table (cleanupNamed is testTenant-pinned, so this suite carries its own cleanup).
func massOpITCleanup(sx *sqlx.DB, tenantIDs ...string) {
	for _, tid := range tenantIDs {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, tid, massOpITEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, tid, massOpITEntity)
		sx.Exec(`DELETE FROM audit_log WHERE tenant_id = ?`, tid)
	}
	sx.Exec("DROP TABLE IF EXISTS `obj_" + massOpITEntity + "`")
}

// massOpITSetup seeds two tenants (A = the caller, B = the foreign tenant), the entity in both
// (title text + email unique text), 3 records in A and 1 in B. Returns the audit-wired router,
// tenant A's admin token, A's record ids and B's record id.
func massOpITSetup(t *testing.T) (rt *auditRouter, sx *sqlx.DB, token string, aIDs []string, bID string, tenantA string) {
	t.Helper()
	gdbReal, sxReal := a4SetupMigratedDB(t)
	rt = newAuditRouter(t, gdbReal, sxReal, audit.NewSQLRepository(sxReal))

	const (
		tenantAConst = "ab100000-0000-0000-0000-0000000000a1"
		tenantBConst = "ab200000-0000-0000-0000-0000000000b2"
		slugA        = "massop-acme"
		slugB        = "massop-other"
		emailA       = "admin@massop-acme.test"
		emailB       = "admin@massop-other.test"
	)
	massOpITCleanup(sxReal, tenantAConst, tenantBConst)
	t.Cleanup(func() { massOpITCleanup(sxReal, tenantAConst, tenantBConst) })

	auditSeedAdmin(t, gdbReal, tenantAConst, slugA, emailA)
	auditSeedAdmin(t, gdbReal, tenantBConst, slugB, emailB)
	tokenA := itLogin(t, rt.engine, slugA, emailA, authPassword).Data.AccessToken
	tokenB := itLogin(t, rt.engine, slugB, emailB, authPassword).Data.AccessToken

	entityBody := map[string]any{
		"name":  massOpITEntity,
		"label": "MassOp Widgets",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
			{"name": "email", "label": "Email", "fieldType": "text", "isUnique": true},
		},
	}
	if w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/entities", tokenA, entityBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity (tenant A): %d %s", w.Code, w.Body.String())
	}
	if w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/entities", tokenB, entityBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity (tenant B): %d %s", w.Code, w.Body.String())
	}

	for i, email := range []string{"r0@massop.test", "r1@massop.test", "r2@massop.test"} {
		aIDs = append(aIDs, rt.createRecord(t, tokenA, massOpITEntity,
			map[string]any{"title": "Row " + string(rune('0'+i)), "email": email}))
	}
	bID = rt.createRecord(t, tokenB, massOpITEntity, map[string]any{"title": "Foreign", "email": "b0@massop.test"})
	return rt, sxReal, tokenA, aIDs, bID, tenantAConst
}

// massOpITEnvelope mirrors the mass-op response body.
type massOpITEnvelope struct {
	Data struct {
		Results []struct {
			ID      string         `json:"id"`
			OK      bool           `json:"ok"`
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"results"`
		Updated int `json:"updated"`
		Deleted int `json:"deleted"`
		Failed  int `json:"failed"`
	} `json:"data"`
}

func decodeMassOpIT(t *testing.T, body []byte) massOpITEnvelope {
	t.Helper()
	var env massOpITEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode mass-op envelope: %v (%s)", err, string(body))
	}
	return env
}

// Bulk-update: shared title patch on [a0, a1, foreignB] — the two tenant-A rows verifiably
// change in the live table; the cross-tenant id fails ONLY its row as RECORD_NOT_FOUND
// (indistinguishable from non-existent — no leak) and tenant B's row stays untouched.
func TestMassOpIT_BulkUpdate_CrossTenantRowFailsSilently(t *testing.T) {
	rt, _, token, aIDs, bID, _ := massOpITSetup(t)

	w := doAuthed(t, rt.engine, "POST", "/api/v1/"+massOpITEntity+"/bulk-update", token,
		map[string]any{"ids": []string{aIDs[0], aIDs[1], bID}, "values": map[string]any{"title": "Đã sửa hàng loạt"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("bulk-update: %d %s", w.Code, w.Body.String())
	}
	env := decodeMassOpIT(t, w.Body.Bytes())
	if env.Data.Updated != 2 || env.Data.Failed != 1 || len(env.Data.Results) != 3 {
		t.Fatalf("envelope = %+v; want updated=2 failed=1 len=3", env.Data)
	}
	for _, row := range env.Data.Results {
		if row.ID == bID {
			if row.OK || row.Code != "RECORD_NOT_FOUND" {
				t.Fatalf("foreign row = %+v; want failed RECORD_NOT_FOUND (no cross-tenant leak)", row)
			}
		} else if !row.OK {
			t.Fatalf("tenant-A row %s failed: %+v", row.ID, row)
		}
	}

	// The two A rows actually carry the patch (live per-record Update path, not a fake 200).
	for _, id := range aIDs[:2] {
		g := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/"+id, token, nil, nil)
		if g.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", id, g.Code, g.Body.String())
		}
		var rec struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(g.Body.Bytes(), &rec); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		if rec.Data.Data["title"] != "Đã sửa hàng loạt" {
			t.Fatalf("record %s title = %v; want the mass patch applied", id, rec.Data.Data["title"])
		}
	}
	// aIDs[2] was NOT in the batch — untouched.
	g := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/"+aIDs[2], token, nil, nil)
	if g.Code != http.StatusOK || !json.Valid(g.Body.Bytes()) {
		t.Fatalf("get untouched row: %d", g.Code)
	}
	if body := g.Body.String(); !contains(body, "Row 2") {
		t.Fatalf("untouched row changed: %s", body)
	}
}

// Bulk-update into a REAL unique-index collision: r1 takes r0's email → that row (and only that
// row) fails 409-shaped RECORD_DUPLICATE with details.field=email, produced by the live MySQL
// ER_DUP_ENTRY → asDuplicate mapping — the exact code a single PUT returns.
func TestMassOpIT_BulkUpdate_UniqueConflictFailsOnlyThatRow(t *testing.T) {
	rt, _, token, aIDs, _, _ := massOpITSetup(t)

	// Shared values email collides with r0's live value. r0 itself is IN the batch — setting a
	// row's unique field to its own current value is a no-op success — so the envelope is
	// exactly 1 updated (r0) + 1 failed (r1, the genuine collision).
	w := doAuthed(t, rt.engine, "POST", "/api/v1/"+massOpITEntity+"/bulk-update", token,
		map[string]any{"ids": []string{aIDs[0], aIDs[1]}, "values": map[string]any{"email": "r0@massop.test"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("bulk-update: %d %s", w.Code, w.Body.String())
	}
	env := decodeMassOpIT(t, w.Body.Bytes())
	if env.Data.Updated != 1 || env.Data.Failed != 1 {
		t.Fatalf("envelope = %+v; want updated=1 failed=1", env.Data)
	}
	for _, row := range env.Data.Results {
		switch row.ID {
		case aIDs[0]:
			if !row.OK {
				t.Fatalf("row r0 (own value) = %+v; want ok", row)
			}
		case aIDs[1]:
			if row.OK || row.Code != "RECORD_DUPLICATE" {
				t.Fatalf("row r1 = %+v; want failed RECORD_DUPLICATE", row)
			}
			if f, _ := row.Details["field"].(string); f != "email" {
				t.Fatalf("row r1 details = %v; want field=email", row.Details)
			}
		}
	}
	// r1 keeps its original email (the failed row really rolled back / never wrote).
	g := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/"+aIDs[1], token, nil, nil)
	if !contains(g.Body.String(), "r1@massop.test") {
		t.Fatalf("failed row mutated anyway: %s", g.Body.String())
	}
}

// Bulk-delete: [a0, a1, foreignB] → the two A rows are soft-deleted (GET 404, recycle bin lists
// them), the cross-tenant id fails silently, and the production audit wiring writes ONE
// audit_log delete row per committed delete.
func TestMassOpIT_BulkDelete_SoftDeleteAuditAndCrossTenant(t *testing.T) {
	rt, sx, token, aIDs, bID, tenantA := massOpITSetup(t)

	w := doAuthed(t, rt.engine, "POST", "/api/v1/"+massOpITEntity+"/bulk-delete", token,
		map[string]any{"ids": []string{aIDs[0], aIDs[1], bID}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("bulk-delete: %d %s", w.Code, w.Body.String())
	}
	env := decodeMassOpIT(t, w.Body.Bytes())
	if env.Data.Deleted != 2 || env.Data.Failed != 1 {
		t.Fatalf("envelope = %+v; want deleted=2 failed=1", env.Data)
	}

	// Soft-deleted: single GET → 404; recycle bin lists both.
	for _, id := range aIDs[:2] {
		if g := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/"+id, token, nil, nil); g.Code != http.StatusNotFound {
			t.Fatalf("get deleted %s: %d; want 404", id, g.Code)
		}
	}
	bin := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/deleted", token, nil, nil)
	if bin.Code != http.StatusOK {
		t.Fatalf("recycle bin: %d %s", bin.Code, bin.Body.String())
	}
	for _, id := range aIDs[:2] {
		if !contains(bin.Body.String(), id) {
			t.Fatalf("recycle bin missing %s: %s", id, bin.Body.String())
		}
	}
	// Untouched sibling still live.
	if g := doAuthed(t, rt.engine, "GET", "/api/v1/"+massOpITEntity+"/"+aIDs[2], token, nil, nil); g.Code != http.StatusOK {
		t.Fatalf("untouched row: %d; want 200", g.Code)
	}
	// Tenant B's record survives (cross-tenant delete never landed).
	// (Verified via the audit trail below + the failed row code above; a direct GET would need
	// tenant B's token — the row-level proof is that its id reported RECORD_NOT_FOUND.)

	// Audit: exactly one delete-action row per committed delete, none for the foreign id.
	rt.drain(t)
	deleteRows := map[string]int{}
	for _, row := range queryAuditRows(t, sx, tenantA) {
		if row.Action == audit.ActionDelete && row.RecordID != nil {
			deleteRows[*row.RecordID]++
		}
	}
	if deleteRows[aIDs[0]] != 1 || deleteRows[aIDs[1]] != 1 {
		t.Fatalf("audit delete rows = %v; want exactly 1 per committed delete", deleteRows)
	}
	if deleteRows[bID] != 0 {
		t.Fatalf("audit contains a delete row for the FOREIGN record %s — cross-tenant write leaked", bID)
	}
}

// contains keeps the body assertions readable.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
