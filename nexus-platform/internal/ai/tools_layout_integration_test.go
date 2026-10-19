//go:build integration

// Integration tests for the M6 layout tools against the REAL *metadata.Repository and the REAL
// services.LayoutService. They prove what the in-memory fake cannot: (a) *metadata.Repository
// structurally satisfies the widened MetadataReader interface (ListLayoutViews) with no adapter,
// (b) propose_layout writes NOTHING, and (c) VALIDATOR PARITY — the exact layoutJson a propose
// buffered is accepted by the real SaveLayout apply path, and an invalid layoutJson is rejected
// by BOTH sides with the SAME ValidationError code (one function body: ValidateLayoutConfig).
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestToolsLayout_IT -count=1
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	layoutITTenantA = "aiaiaiai-m6m6-4444-8888-tenanta00001"
	layoutITTenantB = "aiaiaiai-m6m6-4444-8888-tenantb00002"
	// The SAME entity name is seeded under BOTH tenants (different rows) — the strongest
	// isolation probe for ListLayoutViews: only the tenant_id predicate can keep tenant A's
	// custom layout out of tenant B's list.
	layoutITEntity = "it_ai_m6_patients" // 17 chars — within SanitizeIdentifier's 26-char cap
)

// cleanupLayoutIT hard-deletes every row the suite seeds, keyed by its throwaway tenant IDs, so
// reruns are idempotent and nothing leaks into other suites sharing the DB.
func cleanupLayoutIT(gdb *gorm.DB) {
	gdb.Exec(`DELETE FROM sys_layouts WHERE tenant_id IN (?, ?)`, layoutITTenantA, layoutITTenantB)
	gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, layoutITTenantA, layoutITTenantB)
	gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, layoutITTenantA, layoutITTenantB)
}

// seedLayoutIT registers the entity under both tenants; tenant A's copy carries one REQUIRED
// field (full_name) plus one optional field (note) so the required-omitted parity case is real.
func seedLayoutIT(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	cleanupLayoutIT(gdb)
	t.Cleanup(func() { cleanupLayoutIT(gdb) })

	seed := []struct {
		entID, tenant string
	}{
		{"itai-m6e1-0000-0000-000000000001", layoutITTenantA},
		{"itai-m6e2-0000-0000-000000000002", layoutITTenantB},
	}
	for _, s := range seed {
		ent := metadata.EntityDefinition{
			ID:        s.entID,
			TenantID:  s.tenant,
			Name:      layoutITEntity,
			Label:     "M6 IT patients",
			DataTable: "obj_" + layoutITEntity,
		}
		if err := gdb.Create(&ent).Error; err != nil {
			t.Fatalf("seed entity for %s: %v", s.tenant, err)
		}
	}

	fields := []metadata.FieldDefinition{
		{ID: "itai-m6f1-0000-0000-000000000001", EntityID: seed[0].entID, TenantID: layoutITTenantA,
			Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true, FieldOrder: 10},
		{ID: "itai-m6f2-0000-0000-000000000002", EntityID: seed[0].entID, TenantID: layoutITTenantA,
			Name: "note", Label: "Ghi chú", FieldType: "text", IsRequired: false, FieldOrder: 20},
		{ID: "itai-m6f3-0000-0000-000000000003", EntityID: seed[1].entID, TenantID: layoutITTenantB,
			Name: "total", Label: "Tổng", FieldType: "currency", IsRequired: false, FieldOrder: 10},
	}
	for i := range fields {
		if err := gdb.Create(&fields[i]).Error; err != nil {
			t.Fatalf("seed field %s: %v", fields[i].Name, err)
		}
	}
}

func TestToolsLayout_IT_ProposeParity_SaveLayoutRoundTrip(t *testing.T) {
	repo, gdb := newRelITReader(t) // same single-source DSN helper as the M5 suite
	seedLayoutIT(t, gdb)
	ctx := context.Background()

	// Compile-time + runtime proof: the REAL *metadata.Repository satisfies the widened
	// MetadataReader with no adapter.
	execA := &toolExecutor{reader: repo, tenantID: layoutITTenantA,
		docs: &DocReaders{Layouts: services.NewLayoutService(repo, metadata.NewLayoutRepository(gdb), nil)}}

	// Happy path: a full edit layout (both required + optional fields placed) buffers.
	goodLayout := `{"version":1,"sections":[{"title":"Thông tin chung","columns":2,"fields":["full_name","note"]}]}`
	res := execA.execute(ctx, "propose_layout",
		`{"entity":"`+layoutITEntity+`","viewType":"edit","layoutJson":`+goodLayout+`}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_layout should succeed against real metadata: %s", res)
	}
	if len(execA.draft) != 1 || execA.draft[0].Kind != "layout" || execA.draft[0].Layout == nil {
		t.Fatalf("expected 1 buffered layout draft, got %+v", execA.draft)
	}

	// HITL: nothing was written — the draft is a proposal, not a sys_layouts row.
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_layouts WHERE tenant_id = ?`, layoutITTenantA).
		Scan(&n).Error; err != nil {
		t.Fatalf("count check: %v", err)
	}
	if n != 0 {
		t.Fatalf("propose_layout must NOT write sys_layouts; found %d rows", n)
	}

	// PARITY (accept side): applying the buffered draft VERBATIM through the real SaveLayout —
	// the apply path behind POST /admin/layouts — must succeed, because the tool validated with
	// the SAME function body (ValidateLayoutConfig) SaveLayout runs.
	svc := services.NewLayoutService(repo, metadata.NewLayoutRepository(gdb), nil)
	d := execA.draft[0].Layout
	row, err := svc.SaveLayout(ctx, layoutITTenantA, services.SaveLayoutInput{
		EntityName: d.Entity,
		ViewType:   d.ViewType,
		LayoutJSON: d.LayoutJSON,
	})
	if err != nil {
		t.Fatalf("parity broken: SaveLayout rejected a layout propose_layout accepted: %v", err)
	}
	if row == nil || row.ID == "" {
		t.Fatalf("SaveLayout returned no persisted row: %+v", row)
	}

	// Grounding round-trip: get_layout surfaces the applied view as custom through the REAL service.
	execA.docs = &DocReaders{Layouts: svc}
	resL := execA.execute(ctx, "get_layout", `{"entity":"`+layoutITEntity+`"}`)
	if ok, _ := parseToolResult(t, resL); !ok || !strings.Contains(resL, `{"isCustom":true,"viewType":"edit"}`) {
		t.Fatalf("get_layout should surface the applied edit view as custom: %s", resL)
	}

	// PARITY (reject side): an edit layout omitting the required full_name is rejected by the
	// TOOL with REQUIRED_FIELD_OMITTED...
	badLayout := `{"version":1,"sections":[{"columns":1,"fields":["note"]}]}`
	resBad := execA.execute(ctx, "propose_layout",
		`{"entity":"`+layoutITEntity+`","viewType":"edit","layoutJson":`+badLayout+`}`)
	okBad, mBad := parseToolResult(t, resBad)
	if okBad {
		t.Fatalf("propose_layout must reject an edit layout missing a required field: %s", resBad)
	}
	if errStr, _ := mBad["error"].(string); !strings.Contains(errStr, "REQUIRED_FIELD_OMITTED") {
		t.Fatalf("expected REQUIRED_FIELD_OMITTED from the tool, got %q", errStr)
	}
	if len(execA.draft) != 1 { // still only the good draft
		t.Fatalf("rejected proposal must not buffer, got %+v", execA.draft)
	}
	// ...and by the REAL SaveLayout with the SAME code — both sides run one validator body.
	var badJSON map[string]any
	if err := json.Unmarshal([]byte(badLayout), &badJSON); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	_, errSave := svc.SaveLayout(ctx, layoutITTenantA, services.SaveLayoutInput{
		EntityName: layoutITEntity, ViewType: "edit", LayoutJSON: badJSON,
	})
	var ve *services.ValidationError
	if !errors.As(errSave, &ve) || ve.Code != "REQUIRED_FIELD_OMITTED" {
		t.Fatalf("parity broken: SaveLayout should reject with REQUIRED_FIELD_OMITTED, got %v", errSave)
	}
}

func TestToolsLayout_IT_GetLayout_TenantIsolation(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedLayoutIT(t, gdb)
	ctx := context.Background()

	// Give tenant A a custom edit layout through the real apply path.
	svc := services.NewLayoutService(repo, metadata.NewLayoutRepository(gdb), nil)
	if _, err := svc.SaveLayout(ctx, layoutITTenantA, services.SaveLayoutInput{
		EntityName: layoutITEntity,
		ViewType:   "edit",
		LayoutJSON: map[string]any{
			"version": 1,
			"sections": []any{map[string]any{
				"columns": float64(1), "fields": []any{"full_name"},
			}},
		},
	}); err != nil {
		t.Fatalf("seed tenant A layout: %v", err)
	}

	// Tenant A sees its own custom view.
	docs := &DocReaders{Layouts: svc}
	execA := &toolExecutor{reader: repo, tenantID: layoutITTenantA, docs: docs}
	resA := execA.execute(ctx, "get_layout", `{"entity":"`+layoutITEntity+`"}`)
	if ok, _ := parseToolResult(t, resA); !ok || !strings.Contains(resA, `{"isCustom":true,"viewType":"edit"}`) {
		t.Fatalf("tenant A should see its custom edit view: %s", resA)
	}

	// Tenant B owns an entity with the SAME name — its list must be EMPTY: only the SQL
	// tenant_id predicate separates the two tenants' sys_layouts rows here.
	execB := &toolExecutor{reader: repo, tenantID: layoutITTenantB, docs: docs}
	resB := execB.execute(ctx, "get_layout", `{"entity":"`+layoutITEntity+`"}`)
	if ok, _ := parseToolResult(t, resB); !ok {
		t.Fatalf("tenant B list_layouts on its own same-named entity should succeed: %s", resB)
	}
	if strings.Contains(resB, `"isCustom":true`) {
		t.Fatalf("tenant B leaked tenant A's layout rows (want no custom view): %s", resB)
	}
}

// #30 items 1 and 2, apply side, on the REAL services: a table, an index and the layouts that lean
// on them, all proposed in ONE draft. The layouts are refused by SaveLayout while what they lean on is
// still a draft (the apply path re-validates against the real schema), and accepted VERBATIM once the
// entity / the field edit is applied first — the FE's apply order (entities first, the rest in draft
// order).
func TestToolsLayout_IT_SameDraftTableAndIndex_ApplyInOrder(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sx := connectAI(t)
	ctx := context.Background()
	const tenant = "aiaiaiai-l30l-4444-8888-tenant000030"
	const name = "it_ai_l30_cands"
	clean := func() {
		gdb.Exec(`DELETE FROM sys_layouts WHERE tenant_id = ?`, tenant)
		gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id = ?`, tenant)
		gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id = ?`, tenant)
		gdb.Exec(`DROP TABLE IF EXISTS obj_` + name)
	}
	clean()
	t.Cleanup(clean)

	layouts := services.NewLayoutService(repo, metadata.NewLayoutRepository(gdb), nil)
	e := &toolExecutor{reader: repo, tenantID: tenant, docs: &DocReaders{Layouts: layouts}}
	mustBuffer(t, e, "propose_entity", `{"name":"`+name+`","label":"Ứng viên","fields":[
		{"name":"full_name","label":"Họ tên","fieldType":"text","required":true},
		{"name":"email","label":"Email","fieldType":"email"},
		{"name":"code","label":"Mã","fieldType":"text","options":{"maxLength":40}}]}`)
	mustBuffer(t, e, "propose_layout", `{"entity":"`+name+`","viewType":"edit","layoutJson":{
		"version":1,"sections":[{"columns":2,"fields":["full_name","email","code"]}]}}`)
	edit := e.draft[1].Layout

	// Apply out of order: the layout of a table that does not exist yet is refused.
	if _, err := layouts.SaveLayout(ctx, tenant, services.SaveLayoutInput{EntityName: edit.Entity, ViewType: edit.ViewType, LayoutJSON: edit.LayoutJSON}); err == nil {
		t.Fatal("SaveLayout accepted a layout for a table that is still only a draft")
	}

	// Apply in order: the entity, then the layout verbatim.
	d := e.draft[0].Entity
	in := services.CreateEntityInput{Name: d.Name, Label: d.Label}
	for _, f := range d.Fields {
		in.Fields = append(in.Fields, services.FieldInput{Name: f.Name, Label: f.Label, FieldType: f.FieldType, IsRequired: f.Required, Options: f.Options})
	}
	if _, _, err := services.NewEntityService(dynrepo.New(sx), repo).CreateEntity(ctx, tenant, in); err != nil {
		t.Fatalf("apply entity: %v", err)
	}
	if _, err := layouts.SaveLayout(ctx, tenant, services.SaveLayoutInput{EntityName: edit.Entity, ViewType: edit.ViewType, LayoutJSON: edit.LayoutJSON}); err != nil {
		t.Fatalf("parity broken: SaveLayout refused the same-draft layout once its table exists: %v", err)
	}

	// Item 2: a list sorted by "code" leans on an index proposed in the same draft.
	e.draft = nil
	mustBuffer(t, e, "propose_edit_field", `{"entity":"`+name+`","field":"code","changes":{"options":{"maxLength":40,"indexed":true}}}`)
	mustBuffer(t, e, "propose_layout", `{"entity":"`+name+`","viewType":"list","layoutJson":{
		"version":1,"columns":[{"field":"full_name"},{"field":"code"}],"defaultSort":{"field":"code","order":"asc"}}}`)
	list := e.draft[1].Layout
	if _, err := layouts.SaveLayout(ctx, tenant, services.SaveLayoutInput{EntityName: list.Entity, ViewType: list.ViewType, LayoutJSON: list.LayoutJSON}); err == nil {
		t.Fatal("SaveLayout accepted a sort on a field whose index is still only a draft")
	}
	ef := e.draft[0].EditField
	if _, err := services.NewFieldService(dynrepo.New(sx), repo).EditField(ctx, tenant, services.EditFieldInput{FieldID: ef.FieldID, Options: ef.After.Options}); err != nil {
		t.Fatalf("apply edit_field: %v", err)
	}
	if _, err := layouts.SaveLayout(ctx, tenant, services.SaveLayoutInput{EntityName: list.Entity, ViewType: list.ViewType, LayoutJSON: list.LayoutJSON}); err != nil {
		t.Fatalf("parity broken: SaveLayout refused the list once its index exists: %v", err)
	}
}
