package ai

// Unit tests for propose_records (EASE Nước 7 FINISH-THE-BUILD): per-row validator parity with
// services.ValidateRecord over services.FieldSpecsOf specs (the SAME bodies POST /:entity/bulk
// runs per row — unknown-field strict reject, computed/server-assigned reject, required/type/
// picklist checks), the row cap, draft-buffer awareness (records for an entity proposed THIS
// turn, shape-only specs), and tenant isolation.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTools_ProposeRecords_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"patients","rows":[
		{"full_name":"Nguyễn Văn An","age":34},
		{"full_name":"Trần Thị Bích"}
	]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if !ok {
		t.Fatalf("valid propose_records should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "records" || e.draft[0].Records == nil {
		t.Fatalf("expected 1 buffered records draft, got %+v", e.draft)
	}
	r := e.draft[0].Records
	if r.Entity != "patients" || len(r.Rows) != 2 {
		t.Fatalf("records draft wrong shape: %+v", r)
	}
	// Rows are buffered RAW (the model's proposal) — the apply path recomputes clean payloads.
	if r.Rows[0]["full_name"] != "Nguyễn Văn An" {
		t.Fatalf("rows must echo the proposed payloads: %+v", r.Rows[0])
	}
}

// TestTools_ProposeRecords_RowRejects pins the ValidateRecord parity codes, each carrying the
// 1-based row number so the model can self-correct the exact row.
func TestTools_ProposeRecords_RowRejects(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
		wantRow  string
	}{
		{"unknown field", `{"entity":"patients","rows":[{"full_name":"A"},{"full_name":"B","phone":"09"}]}`,
			"RECORD_UNKNOWN_FIELD", "dòng 2"},
		{"required missing", `{"entity":"patients","rows":[{"age":30}]}`,
			"RECORD_REQUIRED_MISSING", "dòng 1"},
		{"computed auto_number set", `{"entity":"tickets","rows":[{"subject":"Hỏng máy","code":"TK-01"}]}`,
			"RECORD_AUTONUMBER_READONLY", "dòng 1"},
		{"picklist outside values", `{"entity":"tickets","rows":[{"subject":"Hỏng máy","status":"weird"}]}`,
			"RECORD_INVALID_PICKLIST", "dòng 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", tt.args))
			if ok {
				t.Fatal("invalid rows must NOT succeed")
			}
			errStr, _ := m["error"].(string)
			if !strings.Contains(errStr, tt.wantCode) || !strings.Contains(errStr, tt.wantRow) {
				t.Fatalf("expected %s at %s, got %q", tt.wantCode, tt.wantRow, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected rows must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeRecords_Caps(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}

	// Empty rows.
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", `{"entity":"patients","rows":[]}`))
	if ok {
		t.Fatal("empty rows must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORDS_ROWS_REQUIRED") {
		t.Fatalf("expected RECORDS_ROWS_REQUIRED, got %q", errStr)
	}

	// 21 rows — one over the cap. Built programmatically so the cap constant stays the only pin.
	rows := make([]map[string]any, maxProposeRecordRows+1)
	for i := range rows {
		rows[i] = map[string]any{"full_name": fmt.Sprintf("Người %d", i+1)}
	}
	blob, _ := json.Marshal(map[string]any{"entity": "patients", "rows": rows})
	ok, m = parseToolResult(t, e.execute(context.Background(), "propose_records", string(blob)))
	if ok {
		t.Fatal("over-cap rows must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORDS_TOO_MANY") {
		t.Fatalf("expected RECORDS_TOO_MANY, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected calls must not buffer, got %+v", e.draft)
	}
}

// TestTools_ProposeRecords_SameTurnDraftedEntity is the one-prompt whole-app flow: entity proposed
// THIS turn, then sample records for it — validated SHAPE-ONLY against the drafted field
// definitions (picklist values enforced), no registry row needed yet.
func TestTools_ProposeRecords_SameTurnDraftedEntity(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	entArgs := `{"name":"products","label":"Sản phẩm","fields":[
		{"name":"title","label":"Tên hàng","fieldType":"text","required":true},
		{"name":"stock_status","label":"Tồn kho","fieldType":"picklist","options":{"values":["Còn hàng","Hết hàng"]}}
	]}`
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", entArgs)); !ok {
		t.Fatalf("fixture propose_entity failed: %+v", m)
	}

	// Valid rows against the DRAFTED shape.
	okArgs := `{"entity":"products","rows":[{"title":"Bàn phím cơ","stock_status":"Còn hàng"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", okArgs))
	if !ok {
		t.Fatalf("records for a same-turn drafted entity should succeed: %+v", m)
	}
	if len(e.draft) != 2 || e.draft[1].Kind != "records" || e.draft[1].Records.Entity != "products" {
		t.Fatalf("expected entity + records drafts, got %+v", e.draft)
	}

	// The drafted picklist's values are ENFORCED even shape-only.
	badArgs := `{"entity":"products","rows":[{"title":"Chuột","stock_status":"Sắp về"}]}`
	ok, m = parseToolResult(t, e.execute(context.Background(), "propose_records", badArgs))
	if ok {
		t.Fatal("a row violating the drafted picklist must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_INVALID_PICKLIST") {
		t.Fatalf("expected RECORD_INVALID_PICKLIST, got %q", errStr)
	}
	if len(e.draft) != 2 {
		t.Fatalf("rejected rows must not buffer, got %d drafts", len(e.draft))
	}
}

func TestTools_ProposeRecords_UnknownEntity_Rejected(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{"entity nowhere", `{"entity":"ghosts","rows":[{"x":1}]}`},
		// Tenant B's entity must be indistinguishable from missing (isolation).
		{"cross-tenant entity", `{"entity":"invoices","rows":[{"total":{"amount":1,"currencyCode":"VND"}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", tt.args))
			if ok {
				t.Fatal("must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
				t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected call must not buffer, got %+v", e.draft)
			}
		})
	}
}

// --- ENG-SAMPLE-REF: "@ref" so a REQUIRED relation no longer forces skipping records ----------
//
// editFixtureReader's "tickets" entity (eT) carries a `patient` relation field targeting
// "patients" (eA) — used below as the "child references parent" pair (mirrors Dự án→Công việc).

// TestTools_ProposeRecords_Ref_CrossCall_MangledTokensMatch is the propose-time half of "cha-con
// trong cùng lô": the PARENT is proposed first (declares "$ref"), the CHILD second (references
// it) — two SEPARATE propose_records calls in the same turn, exactly how the AI would seed a
// parent entity then a child entity. Both buffer successfully, and the mangled token the parent
// declared is BYTE-IDENTICAL to what the child's relation field now holds (turnTag makes this
// deterministic, not merely equal-by-luck to the raw "@bn1" the model typed).
func TestTools_ProposeRecords_Ref_CrossCall_MangledTokensMatch(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A", turnTag: "turn1"}

	parentArgs := `{"entity":"patients","rows":[{"full_name":"Nguyễn Văn An","$ref":"@bn1"}]}`
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", parentArgs)); !ok {
		t.Fatalf("parent propose_records should succeed: %+v", m)
	}
	childArgs := `{"entity":"tickets","rows":[{"subject":"Khám tổng quát","patient":"@bn1"}]}`
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", childArgs)); !ok {
		t.Fatalf("child propose_records should succeed: %+v", m)
	}

	if len(e.draft) != 2 {
		t.Fatalf("expected 2 buffered records drafts, got %d: %+v", len(e.draft), e.draft)
	}
	parentRef, _ := e.draft[0].Records.Rows[0]["$ref"].(string)
	childRef, _ := e.draft[1].Records.Rows[0]["patient"].(string)
	if parentRef == "" || parentRef == "@bn1" {
		t.Fatalf("parent $ref should be MANGLED (turn-namespaced), got %q", parentRef)
	}
	if childRef != parentRef {
		t.Fatalf("child.patient = %q; want it to match parent's mangled ref %q", childRef, parentRef)
	}
}

// TestTools_ProposeRecords_Ref_DollarRefStrippedBeforeValidate proves "$ref" never reaches
// services.ValidateRecord as an unknown field — a row that is otherwise perfectly valid must
// still buffer successfully with "$ref" present.
func TestTools_ProposeRecords_Ref_DollarRefStrippedBeforeValidate(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"patients","rows":[{"full_name":"Trần Thị Bích","$ref":"@bn2"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if !ok {
		t.Fatalf("a row carrying \"$ref\" must not fail RECORD_UNKNOWN_FIELD: %+v", m)
	}
}

// TestTools_ProposeRecords_Ref_Dangling_Rejected: a reference to a token nobody has ever "$ref"'d
// (not this call, not an earlier propose_records call this turn) is REJECTED at propose time
// already (defense in depth — apply time is the authoritative check, but there is no reason to
// let an obviously-dangling token reach a draft the human might Apply).
func TestTools_ProposeRecords_Ref_Dangling_Rejected(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"tickets","rows":[{"subject":"Khám tổng quát","patient":"@khong-ton-tai"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if ok {
		t.Fatal("a dangling @ref must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_REF_UNRESOLVED") {
		t.Fatalf("expected RECORD_REF_UNRESOLVED, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected call must not buffer, got %+v", e.draft)
	}
}

// TestTools_ProposeRecords_Ref_SameCallSibling_Rejected: row 2 references row 1's "$ref" WITHIN
// THE SAME propose_records call — acyclic, but still rejected (RECORD_REF_UNRESOLVED, not
// RECORD_REF_CYCLE), because the apply-time registry only registers a ref AFTER its row is
// persisted, and neither row of this call has been persisted when refs resolve — a same-call
// sibling can never actually resolve, cyclic or not (see proposeRecords' doc comment).
func TestTools_ProposeRecords_Ref_SameCallSibling_Rejected(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"tickets","rows":[
		{"subject":"T1","$ref":"@r1"},
		{"subject":"T2","patient":"@r1"}
	]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if ok {
		t.Fatal("a same-call sibling reference must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_REF_UNRESOLVED") || !strings.Contains(errStr, "dòng 2") {
		t.Fatalf("expected RECORD_REF_UNRESOLVED at dòng 2, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected call must not buffer, got %+v", e.draft)
	}
}

// TestTools_ProposeRecords_Ref_Cycle_Rejected: row 1 and row 2 mutually reference each other's
// "$ref" within the SAME call — a genuine dependency cycle, unsatisfiable in ANY apply order.
// Must be caught HERE (RECORD_REF_CYCLE) with a DIFFERENT code than the plain-dangling/same-call
// case above — this is the one thing apply time cannot tell apart from an ordinary dangling ref.
func TestTools_ProposeRecords_Ref_Cycle_Rejected(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"tickets","rows":[
		{"subject":"T1","$ref":"@r1","patient":"@r2"},
		{"subject":"T2","$ref":"@r2","patient":"@r1"}
	]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if ok {
		t.Fatal("a same-call @ref cycle must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_REF_CYCLE") {
		t.Fatalf("expected RECORD_REF_CYCLE, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected call must not buffer, got %+v", e.draft)
	}
}

// TestTools_ProposeRecords_Ref_ValidationStillRunsFully: a row carrying an ordinary validation
// defect (missing required field) ALONGSIDE a "$ref"/"@ref" pair must still fail for the ORDINARY
// reason — resolving refs must never shadow or loosen services.ValidateRecord.
func TestTools_ProposeRecords_Ref_ValidationStillRunsFully(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	// "subject" is required on tickets and is absent here.
	args := `{"entity":"tickets","rows":[{"patient":"@bn1","$ref":"@t1"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", args))
	if ok {
		t.Fatal("a row missing a required field must NOT succeed just because it carries @ref")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_REQUIRED_MISSING") {
		t.Fatalf("expected RECORD_REQUIRED_MISSING (ref must not bypass required-field validation), got %q", errStr)
	}
}
