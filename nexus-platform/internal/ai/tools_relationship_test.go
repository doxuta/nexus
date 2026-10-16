package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

func strPtr(s string) *string { return &s }

// twoTenantRelReader extends the two-tenant fixture with relationship rows and a relation field
// (with stored options JSON) so the M5 tools can be proven tenant-scoped and grounding-complete.
//
// tenant-A: entities patients (eA) + treatments (eA2);
//   - 1-N row "treatments_patient" (patients ← treatments, inverseField "treatments")
//   - N-N row "patients_treatments"
//   - relation field "patient" on treatments with relatedEntity/cardinality/inverseField options
//
// tenant-B: entity invoices (eB) + N-N row "invoices_payments".
func twoTenantRelReader() *fakeMetaReader {
	r := twoTenantReader()
	r.entities["tenant-A"] = append(r.entities["tenant-A"],
		metadata.EntityDefinition{ID: "eA2", TenantID: "tenant-A", Name: "treatments", Label: "Điều trị"})
	r.fields["tenant-A"]["eA2"] = []metadata.FieldDefinition{
		{Name: "title", Label: "Tiêu đề", FieldType: "text", IsRequired: true,
			Options: strPtr(`{"maxLength":80}`)},
		{Name: "patient", Label: "Bệnh nhân", FieldType: "relation", IsRequired: true,
			Options: strPtr(`{"relatedEntity":"patients","cardinality":"1-1","inverseField":"treatments","indexed":true}`)},
	}
	r.rels = map[string][]metadata.RelationshipDefinition{
		"tenant-A": {
			{ID: "rA1", TenantID: "tenant-A", Name: "treatments_patient",
				ParentEntityName: "patients", ChildEntityName: "treatments",
				RelationshipType: "1-N", InverseField: "treatments", ChildFKColumn: "patient_id"},
			{ID: "rA2", TenantID: "tenant-A", Name: "patients_treatments",
				ParentEntityName: "patients", ChildEntityName: "treatments",
				RelationshipType: "N-N"},
		},
		"tenant-B": {
			{ID: "rB1", TenantID: "tenant-B", Name: "invoices_payments",
				ParentEntityName: "invoices", ChildEntityName: "payments",
				RelationshipType: "N-N"},
		},
	}
	return r
}

func TestTools_ListRelationships_TenantIsolation(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "list_relationships", "{}")

	ok, _ := parseToolResult(t, res)
	if !ok {
		t.Fatalf("list_relationships should succeed: %s", res)
	}
	// Tenant A must see ONLY its own relationships, never tenant B's "invoices_payments".
	if strings.Contains(res, "invoices_payments") {
		t.Fatalf("tenant A's list leaked tenant B relationship: %s", res)
	}
	for _, want := range []string{"treatments_patient", "patients_treatments", "1-N", "N-N", `"inverseField":"treatments"`} {
		if !strings.Contains(res, want) {
			t.Fatalf("tenant A's list missing %q: %s", want, res)
		}
	}
}

func TestTools_ListRelationships_EntityFilter(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}

	// Filter by a valid entity returns only relationships touching it (parent OR child side).
	ok, m := parseToolResult(t, e.execute(context.Background(), "list_relationships", `{"entity":"treatments"}`))
	if !ok {
		t.Fatalf("filtered list_relationships should succeed: %+v", m)
	}
	result, _ := m["result"].(map[string]any)
	rels, _ := result["relationships"].([]any)
	if len(rels) != 2 {
		t.Fatalf("expected 2 relationships touching 'treatments', got %d: %+v", len(rels), rels)
	}

	// A typo'd / cross-tenant entity name must surface ENTITY_NOT_FOUND, not a silently-empty
	// list the model could misread as "no relationships yet".
	okBad, mBad := parseToolResult(t, e.execute(context.Background(), "list_relationships", `{"entity":"invoices"}`))
	if okBad {
		t.Fatal("filter on tenant B's entity must NOT succeed for tenant A")
	}
	if errStr, _ := mBad["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
}

func TestTools_ProposeRelationship_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	args := `{"name":"patients_treatments_referrals","entityA":"patients","entityB":"treatments"}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", args))
	if !ok {
		t.Fatalf("valid propose_relationship should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "relationship" || e.draft[0].Relationship == nil {
		t.Fatalf("expected 1 buffered relationship draft, got %+v", e.draft)
	}
	rel := e.draft[0].Relationship
	if rel.Name != "patients_treatments_referrals" || rel.EntityA != "patients" || rel.EntityB != "treatments" {
		t.Fatalf("buffered relationship wrong shape: %+v", rel)
	}
	if e.draft[0].Entity != nil || e.draft[0].Field != nil {
		t.Fatalf("relationship draft must not carry entity/field payloads: %+v", e.draft[0])
	}
}

func TestTools_ProposeRelationship_SelfReferential_Rejected(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	args := `{"name":"patients_patients","entityA":"patients","entityB":"patients"}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", args))
	if ok {
		t.Fatal("self-referential N-N must be rejected (parity with ddl.JoinParts)")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RELATION_INVALID_ENTITIES") {
		t.Fatalf("expected RELATION_INVALID_ENTITIES, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
	}
}

func TestTools_ProposeRelationship_MissingEntity_Rejected(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		// Entity that exists nowhere.
		{"unknown entity", `{"name":"patients_courses","entityA":"patients","entityB":"courses"}`},
		// Entity that exists ONLY in tenant B — must be indistinguishable from missing.
		{"cross-tenant entity", `{"name":"patients_invoices","entityA":"patients","entityB":"invoices"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", tt.args))
			if ok {
				t.Fatal("propose_relationship on a missing entity must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
				t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeRelationship_BadName_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"empty name", `{"name":"","entityA":"patients","entityB":"treatments"}`, "RELATION_NAME_REQUIRED"},
		{"whitespace name", `{"name":"   ","entityA":"patients","entityB":"treatments"}`, "RELATION_NAME_REQUIRED"},
		{"over 150 bytes", `{"name":"` + strings.Repeat("x", 151) + `","entityA":"patients","entityB":"treatments"}`, "RELATION_NAME_TOO_LONG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", tt.args))
			if ok {
				t.Fatal("bad relationship name must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}

	// F5: pin the exact 150-byte boundary — the column is VARCHAR(150) and the check is
	// `len(name) > maxRelationshipNameBytes`, so a name of EXACTLY 150 bytes must be ACCEPTED
	// (off-by-one regression guard: a `>=` typo here would silently reject the longest legal name).
	t.Run("exactly 150 bytes accepted", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
		name150 := strings.Repeat("x", maxRelationshipNameBytes)
		if len(name150) != maxRelationshipNameBytes {
			t.Fatalf("fixture bug: name150 is %d bytes, want %d", len(name150), maxRelationshipNameBytes)
		}
		args := `{"name":"` + name150 + `","entityA":"patients","entityB":"treatments"}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", args))
		if !ok {
			t.Fatalf("a name of exactly %d bytes must be accepted, got %+v", maxRelationshipNameBytes, m)
		}
		if len(e.draft) != 1 {
			t.Fatalf("expected 1 buffered relationship draft, got %+v", e.draft)
		}
	})

	// F5: a multibyte (Vietnamese) name that is well under 150 RUNES but over 150 BYTES is
	// REJECTED — the check is deliberately a BYTE budget (mirrors sys_relationships.name
	// VARCHAR(150), a byte-length column in MySQL), not a rune-count budget. This is stricter
	// than "150 Vietnamese characters would fit the column" in the rune sense, but matches what
	// the DB will actually accept — see metadata.tombstoneName's doc comment on the same
	// byte-vs-rune distinction for VARCHAR columns. "à" is U+00E0 (2 bytes in UTF-8): 76 of them
	// is 76 runes (well under 150) but 152 bytes (over 150).
	t.Run("multibyte over 150 bytes rejected despite well under 150 runes", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
		nameVN := strings.Repeat("à", 76)
		if n := len([]rune(nameVN)); n >= maxRelationshipNameBytes {
			t.Fatalf("fixture bug: nameVN has %d runes, want well under %d", n, maxRelationshipNameBytes)
		}
		if len(nameVN) <= maxRelationshipNameBytes {
			t.Fatalf("fixture bug: nameVN is %d bytes, want over %d", len(nameVN), maxRelationshipNameBytes)
		}
		args := `{"name":"` + nameVN + `","entityA":"patients","entityB":"treatments"}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship", args))
		if ok {
			t.Fatal("a name over the byte budget must be rejected even though it is under the rune budget")
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RELATION_NAME_TOO_LONG") {
			t.Fatalf("expected RELATION_NAME_TOO_LONG, got %q", errStr)
		}
		if len(e.draft) != 0 {
			t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
		}
	})
}

// TestTools_ProposeField_Cardinality_Regression proves the 1-N/1-1 path is UNCHANGED by M5:
// relation fields keep flowing through propose_field (services.ValidateAddField parity) — "1-1"
// buffers, "N-N" is rejected at config time (that shape belongs to propose_relationship).
func TestTools_ProposeField_Cardinality_Regression(t *testing.T) {
	t.Run("cardinality 1-1 buffers", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
		args := `{"entity":"treatments","name":"primary_patient","label":"BN chính","fieldType":"relation",
			"options":{"relatedEntity":"patients","cardinality":"1-1"},"required":false}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", args))
		if !ok {
			t.Fatalf("relation field with cardinality 1-1 should buffer: %+v", m)
		}
		if len(e.draft) != 1 || e.draft[0].Kind != "field" || e.draft[0].Field == nil {
			t.Fatalf("expected 1 buffered field draft, got %+v", e.draft)
		}
		if card, _ := e.draft[0].Field.Options["cardinality"].(string); card != "1-1" {
			t.Fatalf("buffered field lost cardinality option: %+v", e.draft[0].Field.Options)
		}
	})

	t.Run("cardinality N-N rejected", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
		args := `{"entity":"treatments","name":"linked_patients","label":"BN liên quan","fieldType":"relation",
			"options":{"relatedEntity":"patients","cardinality":"N-N"}}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", args))
		if ok {
			t.Fatal("cardinality N-N on a relation FIELD must be rejected — use propose_relationship")
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "FIELD_RELATION_CARDINALITY_INVALID") {
			t.Fatalf("expected FIELD_RELATION_CARDINALITY_INVALID, got %q", errStr)
		}
		if len(e.draft) != 0 {
			t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
		}
	})
}

// TestTools_ListFields_SurfacesRelationOptions asserts the M5 grounding upgrade: list_fields now
// carries relatedEntity/cardinality/inverseField for relation fields — and ONLY those keys (the
// rest of the options map stays private to keep the payload small).
func TestTools_ListFields_SurfacesRelationOptions(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "list_fields", `{"entity":"treatments"}`)
	ok, m := parseToolResult(t, res)
	if !ok {
		t.Fatalf("list_fields should succeed: %s", res)
	}

	result, _ := m["result"].(map[string]any)
	fields, _ := result["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d: %+v", len(fields), fields)
	}

	var relRow, textRow map[string]any
	for _, f := range fields {
		row, _ := f.(map[string]any)
		switch row["name"] {
		case "patient":
			relRow = row
		case "title":
			textRow = row
		}
	}
	if relRow == nil || textRow == nil {
		t.Fatalf("fixture rows missing from output: %s", res)
	}

	opts, _ := relRow["options"].(map[string]any)
	if opts == nil {
		t.Fatalf("relation field must surface its relation options: %+v", relRow)
	}
	if opts["relatedEntity"] != "patients" || opts["cardinality"] != "1-1" || opts["inverseField"] != "treatments" {
		t.Fatalf("relation options wrong: %+v", opts)
	}
	// ONLY the 3 relation keys — non-relation option keys (indexed) must NOT leak through.
	if _, leaked := opts["indexed"]; leaked || len(opts) != 3 {
		t.Fatalf("options projection must carry ONLY relatedEntity/cardinality/inverseField, got %+v", opts)
	}
	// A field whose options carry none of the relation keys gets NO options key at all.
	if _, has := textRow["options"]; has {
		t.Fatalf("text field must not carry an options key (maxLength is not a relation key): %+v", textRow)
	}

	// Belt-and-braces on the raw JSON: the non-relation key never appears anywhere.
	var buf map[string]any
	if err := json.Unmarshal([]byte(res), &buf); err != nil {
		t.Fatalf("tool result not JSON: %v", err)
	}
	if strings.Contains(res, "maxLength") || strings.Contains(res, "indexed") {
		t.Fatalf("non-relation option keys leaked into list_fields output: %s", res)
	}
}

// TestTools_ProposeRelationship_DraftAware — EASE Nước 7 enabler: an endpoint proposed as an
// entity draft THIS turn is accepted (one-prompt whole-app: entities + N-N in one batch); the
// deep existence check moves to Apply, where FE dependency order creates entities first.
func TestTools_ProposeRelationship_DraftAware(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	// Buffer an entity draft the way propose_entity would (names are sanitized lowercase).
	e.draft = append(e.draft, DraftItem{Kind: "entity", Entity: &DraftEntity{Name: "leases", Label: "Hợp đồng thuê"}})

	// drafted ↔ live endpoint mix must pass…
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_relationship",
		`{"name":"patients_leases","entityA":"patients","entityB":"leases"}`))
	if !ok {
		t.Fatalf("drafted-entity endpoint must be accepted: %+v", m)
	}
	if len(e.draft) != 2 || e.draft[1].Kind != "relationship" {
		t.Fatalf("expected entity draft + relationship draft, got %+v", e.draft)
	}

	// …while a name that is NEITHER live NOR drafted still rejects (no free-for-all).
	ok, m = parseToolResult(t, e.execute(context.Background(), "propose_relationship",
		`{"name":"ghosts_leases","entityA":"ghosts","entityB":"leases"}`))
	if ok {
		t.Fatalf("undrafted unknown entity must still reject: %+v", m)
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
}

// TestTools_ListFields_SurfacesPolymorphicTargets (queue #139): a polymorphic_relation field's
// options.targetEntities is an ARRAY, so the string-only projection used to drop it — the model
// could not see where an existing "liên quan tới" field points and would propose a near-duplicate.
// The grounding must carry the stored target list verbatim (and still nothing else).
func TestTools_ListFields_SurfacesPolymorphicTargets(t *testing.T) {
	tests := []struct {
		name     string
		options  string
		wantOpts map[string]any // nil ⇒ the row carries no options key at all
	}{
		{
			name:     "stored targets surface as the same array",
			options:  `{"targetEntities":["crm_accounts","crm_contacts"],"indexed":false}`,
			wantOpts: map[string]any{"targetEntities": []any{"crm_accounts", "crm_contacts"}},
		},
		{
			name:     "blank and non-string entries are dropped, the rest kept in order",
			options:  `{"targetEntities":["crm_accounts"," ",7,"crm_leads"]}`,
			wantOpts: map[string]any{"targetEntities": []any{"crm_accounts", "crm_leads"}},
		},
		{name: "empty target list ⇒ no options key", options: `{"targetEntities":[]}`},
		{name: "targetEntities not an array ⇒ no options key", options: `{"targetEntities":"crm_accounts"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := twoTenantReader()
			r.fields["tenant-A"]["eA"] = append(r.fields["tenant-A"]["eA"], metadata.FieldDefinition{
				Name: "related_to", Label: "Liên quan tới", FieldType: "polymorphic_relation", Options: strPtr(tc.options),
			})
			e := &toolExecutor{reader: r, tenantID: "tenant-A"}
			res := e.execute(context.Background(), "list_fields", `{"entity":"patients"}`)
			ok, m := parseToolResult(t, res)
			if !ok {
				t.Fatalf("list_fields should succeed: %s", res)
			}
			result, _ := m["result"].(map[string]any)
			fields, _ := result["fields"].([]any)
			var row map[string]any
			for _, f := range fields {
				if fr, _ := f.(map[string]any); fr["name"] == "related_to" {
					row = fr
				}
			}
			if row == nil {
				t.Fatalf("related_to missing from list_fields: %s", res)
			}
			got, has := row["options"]
			if tc.wantOpts == nil {
				if has {
					t.Fatalf("expected no options key, got %+v", got)
				}
				return
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.wantOpts)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("options = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}
