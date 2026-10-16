package ai

// Unit tests for propose_app (Apps Phase 3, ../specs/010-app-container/spec.md §7): config parity with
// services.ValidateAppConfig (the SAME body CreateApp runs), nav-name resolution against the
// tenant registry ∪ the same-turn draft buffer, dedupe/cap/non-empty nav rules, and tenant
// isolation. Mirrors the propose_relationship test style (two-tenant fake reader, table-driven
// rejects, buffer-untouched assertions).

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTools_ProposeApp_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	args := `{"name":"clinic","label":"Phòng khám","icon":"stethoscope","color":"beige",
		"description":"Quản lý phòng khám","navEntities":["patients"]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", args))
	if !ok {
		t.Fatalf("valid propose_app should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "app" || e.draft[0].App == nil {
		t.Fatalf("expected 1 buffered app draft, got %+v", e.draft)
	}
	app := e.draft[0].App
	if app.Name != "clinic" || app.Label != "Phòng khám" || app.Icon != "stethoscope" ||
		app.Color != "beige" || app.Description != "Quản lý phòng khám" {
		t.Fatalf("buffered app wrong shape: %+v", app)
	}
	if len(app.NavEntities) != 1 || app.NavEntities[0] != "patients" {
		t.Fatalf("buffered nav wrong: %+v", app.NavEntities)
	}
	if e.draft[0].Entity != nil || e.draft[0].Field != nil || e.draft[0].Report != nil {
		t.Fatalf("app draft must not carry other kind payloads: %+v", e.draft[0])
	}
}

// TestTools_ProposeApp_SameTurnDraftedEntity is the core one-turn flow: the model proposes an
// entity and then the app that wraps it in the SAME turn — the nav name resolves against the
// draft buffer even though the entity does not exist in the registry yet.
func TestTools_ProposeApp_SameTurnDraftedEntity(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	entArgs := `{"name":"appointments","label":"Lịch hẹn","fields":[
		{"name":"start_at","label":"Bắt đầu","fieldType":"datetime","required":true}]}`
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", entArgs)); !ok {
		t.Fatalf("fixture propose_entity failed: %+v", m)
	}

	appArgs := `{"name":"spa_management","label":"Quản lý Spa","navEntities":["appointments","patients"]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", appArgs))
	if !ok {
		t.Fatalf("propose_app over a same-turn drafted entity should succeed: %+v", m)
	}
	if len(e.draft) != 2 || e.draft[1].Kind != "app" || e.draft[1].App == nil {
		t.Fatalf("expected entity + app drafts, got %+v", e.draft)
	}
	nav := e.draft[1].App.NavEntities
	if len(nav) != 2 || nav[0] != "appointments" || nav[1] != "patients" {
		t.Fatalf("nav must keep display order (drafted first, existing second): %+v", nav)
	}
}

func TestTools_ProposeApp_UnknownEntity_Rejected(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		// Entity that exists nowhere.
		{"unknown entity", `{"name":"spa","label":"Spa","navEntities":["patients","courses"]}`},
		// Entity that exists ONLY in tenant B — must be indistinguishable from missing.
		{"cross-tenant entity", `{"name":"spa","label":"Spa","navEntities":["invoices"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", tt.args))
			if ok {
				t.Fatal("propose_app on a missing entity must NOT succeed")
			}
			errStr, _ := m["error"].(string)
			if !strings.Contains(errStr, "ENTITY_NOT_FOUND") || !strings.Contains(errStr, "propose_entity trước") {
				t.Fatalf("expected ENTITY_NOT_FOUND with the propose-first hint, got %q", errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}

// TestTools_ProposeApp_BadConfig_Rejected proves the ValidateAppConfig parity codes surface to
// the model (same codes CreateApp returns — one function body, zero drift).
func TestTools_ProposeApp_BadConfig_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"slug with space", `{"name":"spa app","label":"Spa","navEntities":["patients"]}`, "APP_NAME_INVALID"},
		{"slug starts with digit", `{"name":"1spa","label":"Spa","navEntities":["patients"]}`, "APP_NAME_INVALID"},
		{"slug over 60 chars", `{"name":"` + strings.Repeat("a", 61) + `","label":"Spa","navEntities":["patients"]}`, "APP_NAME_INVALID"},
		{"label missing", `{"name":"spa","label":"  ","navEntities":["patients"]}`, "APP_LABEL_REQUIRED"},
		{"label over 100 runes", `{"name":"spa","label":"` + strings.Repeat("é", 101) + `","navEntities":["patients"]}`, "APP_FIELD_TOO_LONG"},
		{"icon over 50 runes", `{"name":"spa","label":"Spa","icon":"` + strings.Repeat("i", 51) + `","navEntities":["patients"]}`, "APP_FIELD_TOO_LONG"},
		{"color over 20 runes", `{"name":"spa","label":"Spa","color":"` + strings.Repeat("c", 21) + `","navEntities":["patients"]}`, "APP_FIELD_TOO_LONG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", tt.args))
			if ok {
				t.Fatal("bad app config must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}

	// An uppercase/padded slug is VALID — normalized (lowercase-trimmed) exactly as CreateApp
	// persists, so the draft carries what Apply will store.
	t.Run("uppercase slug normalized, not rejected", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app",
			`{"name":"  SPA  ","label":"Spa","navEntities":["patients"]}`))
		if !ok {
			t.Fatalf("uppercase slug should normalize and succeed: %+v", m)
		}
		if e.draft[0].App.Name != "spa" {
			t.Fatalf("draft slug must be lowercased trimmed, got %q", e.draft[0].App.Name)
		}
	})
}

func TestTools_ProposeApp_NavRules(t *testing.T) {
	t.Run("empty navEntities rejected", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app",
			`{"name":"spa","label":"Spa","navEntities":[]}`))
		if ok {
			t.Fatal("an app with no nav entities must be rejected")
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "APP_NAV_ENTITIES_REQUIRED") {
			t.Fatalf("expected APP_NAV_ENTITIES_REQUIRED, got %q", errStr)
		}
		if len(e.draft) != 0 {
			t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
		}
	})

	// Documented choice: duplicates are DEDUPED (first occurrence wins, order preserved), not
	// rejected — the apply path's ReplaceNav rejects duplicate pairs, so a deduped draft is what
	// keeps Apply clean.
	t.Run("duplicate names deduped, order preserved", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
		entArgs := `{"name":"appointments","label":"Lịch hẹn","fields":[
			{"name":"start_at","label":"Bắt đầu","fieldType":"datetime","required":true}]}`
		if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", entArgs)); !ok {
			t.Fatalf("fixture propose_entity failed: %+v", m)
		}
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app",
			`{"name":"spa","label":"Spa","navEntities":["patients","appointments","patients"]}`))
		if !ok {
			t.Fatalf("duplicated nav names should dedupe, not fail: %+v", m)
		}
		nav := e.draft[1].App.NavEntities
		if len(nav) != 2 || nav[0] != "patients" || nav[1] != "appointments" {
			t.Fatalf("expected deduped [patients appointments], got %+v", nav)
		}
	})

	// The cap is checked on the RAW list BEFORE any registry lookup — an oversized list must
	// never buy N reader round-trips, so the 41 names here don't need to exist.
	t.Run("over 40 names rejected before resolution", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
		names := make([]string, maxAppNavEntities+1)
		for i := range names {
			names[i] = fmt.Sprintf(`"ghost_%d"`, i)
		}
		args := `{"name":"spa","label":"Spa","navEntities":[` + strings.Join(names, ",") + `]}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", args))
		if ok {
			t.Fatal("a nav list over the cap must be rejected")
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "APP_NAV_TOO_MANY") {
			t.Fatalf("expected APP_NAV_TOO_MANY, got %q", errStr)
		}
		if len(e.draft) != 0 {
			t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
		}
	})

	// Exactly 40 (all resolvable) is accepted — off-by-one guard on the `>` check.
	t.Run("exactly 40 names accepted", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
		names := make([]string, 0, maxAppNavEntities)
		for i := 0; i < maxAppNavEntities; i++ {
			names = append(names, `"patients"`) // dedupes to 1 — the cap is on the raw list
		}
		args := `{"name":"spa","label":"Spa","navEntities":[` + strings.Join(names, ",") + `]}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", args))
		if !ok {
			t.Fatalf("a raw list of exactly %d names must be accepted: %+v", maxAppNavEntities, m)
		}
		if nav := e.draft[0].App.NavEntities; len(nav) != 1 {
			t.Fatalf("expected dedupe to 1 entry, got %+v", nav)
		}
	})
}
