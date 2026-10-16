//go:build integration

// Report matrix integration tests (specs/049-report-grouping-pivot, slice S2b report-matrix-run,
// T028–T031): a saved Matrix report with a column axis, run through the PRODUCTION router (auth →
// tenant → report-service ACL → grid engine, real sys_field_acl) against a real migrated MySQL 8,
// by an employee who is NOT an admin. What only this layer proves: the config key survives the
// admin API, /run returns the cross-tab shape with every number limited to the caller's rows, a
// hidden axis is refused before any value is out, the run overlay (?ownerId/?dateFrom/?dateTo)
// narrows the grid without widening the caller's scope, a matrix saved before S2b answers byte for
// byte like a summary, and the cap reaches the client as a 422 envelope. Slice S3
// report-multilevel-run (T044–T046) adds the Summary with sub-levels: the tree's level-1 sub-totals and grand total equal the one-level summary / the
// scalar aggregate run with the SAME token for every metric, the rep sees only the branches
// holding its rows, a time-bucketed level reads in calendar order, a hidden level 2 or 3 is
// refused, and more than 1000 leaf groups is a 422 envelope. Slice S4 report-grid-csv (T053): the
// matrix and tree CSV the employee downloads equal its /run cell for cell, errors stay JSON. Slice
// S5 report-having (T059, T061–T063): a threshold keeps only the leaf groups that pass it — judged
// on the caller's own rows — every total adds only the kept leaves, a threshold nothing passes is
// an empty 200, a one-level summary with a threshold is capped instead of cut, and a malformed
// threshold is refused on save with its field named. Slice S6 report-axis-labels (T073a): an
// employee who is not an admin reads the customer's name and the picklist's Vietnamese label on the
// axes of /run AND in the file, the same text in both; one who may not view the customers reads the
// raw key in both; the one-level summary gains a label on /run while its file keeps its bytes.
// Run with: go test -tags=integration -run 'TestReportMatrix|TestReportGridCSV|TestReportMultiLevel|TestReportHaving' ./internal/handlers/
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
)

const (
	repGridTenantID = "regr1000-0000-0000-0000-0000000000b2"
	repGridSlug     = "regr-acme"
	repGridEntity   = "it_repgrid_opps"
	// repGridRelEntity is the customer entity the opportunities point at (#240 joined axis); the
	// rep may view it, so a refusal can only come from a field-ACL row.
	repGridRelEntity = "it_repgrid_accts"
	repGridRelName   = repGridEntity + "_account" // registry name of the relation field "account"
)

// repGridEnv is one seeded tenant: an admin, a rep (view@owner + create — NOT admin), an employee
// with no grant at all, and 5 opportunities of which the rep created 2.
type repGridEnv struct {
	r                         *gin.Engine
	gdb                       *gorm.DB
	sx                        *sqlx.DB
	adminTok, repTok, noneTok string
	repRole, repID, adminID   string
	createReport              func(name, typ string, cfg map[string]any) string
	run                       func(tok, reportID string) (int, []byte)
	runData                   func(tok, reportID string) map[string]any
}

func seedReportGrid(t *testing.T) *repGridEnv {
	t.Helper()
	gdb, sx := a4SetupMigratedDB(t)
	r := newReportACLRouter(t, gdb, sx)
	env := &repGridEnv{r: r, gdb: gdb, sx: sx}
	seedReportGridUsers(t, gdb, env)

	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", env.adminTok, map[string]any{
		"name": repGridEntity, "label": "Cơ hội",
		"fields": []map[string]any{
			{"name": "name", "label": "Tên", "fieldType": "text"},
			{"name": "stage", "label": "Giai đoạn", "fieldType": "text", "options": map[string]any{"indexed": true}},
			{"name": "close_date", "label": "Ngày chốt", "fieldType": "date", "options": map[string]any{"indexed": true}},
			{"name": "secret", "label": "Mã nội bộ", "fieldType": "text", "options": map[string]any{"indexed": true}},
			{"name": "amount", "label": "Giá trị", "fieldType": "number"},
		},
	}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	// 3 rows by the admin, 2 by the rep. Amounts carry decimals so a float detour would show.
	for _, row := range []struct {
		tok, stage, date, secret string
		amount                   float64
	}{
		{env.adminTok, "Proposal", "2026-01-15", "alpha-hidden", 100.25},
		{env.adminTok, "Proposal", "2026-02-10", "alpha-hidden", 200.5},
		{env.adminTok, "Won", "2026-01-20", "beta-hidden", 300},
		{env.repTok, "Proposal", "2026-01-05", "alpha-hidden", 10.1},
		{env.repTok, "Won", "2026-02-28", "beta-hidden", 20.2},
	} {
		if w := doAuthed(t, r, http.MethodPost, "/api/v1/"+repGridEntity, row.tok, map[string]any{
			"name": "opp", "stage": row.stage, "close_date": row.date, "secret": row.secret, "amount": row.amount,
		}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed record: %d %s", w.Code, w.Body.String())
		}
	}
	return env
}

func seedReportGridUsers(t *testing.T, gdb *gorm.DB, env *repGridEnv) {
	t.Helper()
	a4SeedTenant(t, gdb, repGridTenantID, repGridSlug, "Acme ReportGrid")
	env.repRole = a4SeedRole(t, gdb, repGridTenantID, "grid-rep", []auth.RoleAction{
		{EntityName: repGridEntity, Action: "view", AccessLevel: "owner"}, grant(repGridEntity, "create"),
		grant(repGridRelEntity, "view"),
	})
	noneRole := a4SeedRole(t, gdb, repGridTenantID, "grid-none", nil)
	env.adminID = a4SeedUser(t, gdb, repGridTenantID, "admin@"+repGridSlug, true, nil)
	env.repID = a4SeedUser(t, gdb, repGridTenantID, "rep@"+repGridSlug, false, &env.repRole)
	a4SeedUser(t, gdb, repGridTenantID, "none@"+repGridSlug, false, &noneRole)
	env.adminTok = itLogin(t, env.r, repGridSlug, "admin@"+repGridSlug, authPassword).Data.AccessToken
	env.repTok = itLogin(t, env.r, repGridSlug, "rep@"+repGridSlug, authPassword).Data.AccessToken
	env.noneTok = itLogin(t, env.r, repGridSlug, "none@"+repGridSlug, authPassword).Data.AccessToken

	env.createReport = func(name, typ string, cfg map[string]any) string {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodPost, "/api/v1/admin/reports", env.adminTok, map[string]any{
			"name": name, "entityName": repGridEntity, "type": typ, "config": cfg,
		}, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create report %q: %d %s", name, w.Code, w.Body.String())
		}
		var created struct {
			Data struct {
				Report struct {
					ID string `json:"id"`
				} `json:"report"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		return created.Data.Report.ID
	}
	env.run = func(tok, reportID string) (int, []byte) {
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+reportID+"/run", tok, nil, nil)
		return w.Code, w.Body.Bytes()
	}
	env.runData = func(tok, reportID string) map[string]any {
		t.Helper()
		code, body := env.run(tok, reportID)
		if code != http.StatusOK {
			t.Fatalf("run %s: %d %s", reportID, code, body)
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode run: %v (%s)", err, body)
		}
		return out.Data
	}
}

// decEq compares two decimal texts exactly (no float).
func decEq(got any, want string) bool {
	s, ok := got.(string)
	if !ok {
		return false
	}
	g, ok1 := new(big.Rat).SetString(s)
	w, ok2 := new(big.Rat).SetString(want)
	return ok1 && ok2 && g.Cmp(w) == 0
}

// headerKV renders a rows/columns/groups list as "key=value/count" for one-line comparison.
func headerKV(list any) []string {
	var out []string
	for _, h := range list.([]any) {
		m := h.(map[string]any)
		v := "null"
		if s, ok := m["value"].(string); ok {
			r, _ := new(big.Rat).SetString(s)
			v = r.FloatString(2)
		}
		out = append(out, fmt.Sprintf("%v=%s/%v", m["key"], v, m["count"]))
	}
	return out
}

func matrixCfg(extra map[string]any) map[string]any {
	cfg := map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount",
		"columnGroup": map[string]any{"field": "close_date", "bucket": "month"}}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// TestReportMatrix_OwnerScopedCellsAndTotals is T028.
func TestReportMatrix_OwnerScopedCellsAndTotals(t *testing.T) {
	env := seedReportGrid(t)
	matrixID := env.createReport("Giá trị theo giai đoạn × tháng chốt", "matrix", matrixCfg(nil))
	summaryID := env.createReport("Giá trị theo giai đoạn", "summary",
		map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount"})

	// The column axis survives the admin API (request → service → sys_reports.config → response).
	w := doAuthed(t, env.r, http.MethodGet, "/api/v1/admin/reports/"+matrixID, env.adminTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"columnGroup":{"field":"close_date","bucket":"month"}`) {
		t.Fatalf("GET report: %d %s — want config.columnGroup close_date/month", w.Code, w.Body.String())
	}

	t.Run("rep (not admin, view@owner): every number adds only the 2 rows the rep owns", func(t *testing.T) {
		d := env.runData(env.repTok, matrixID)
		if d["type"] != "matrix" {
			t.Fatalf("type = %v, want matrix (%v)", d["type"], d)
		}
		if !reflect.DeepEqual(d["rowAxis"], map[string]any{"field": "stage", "bucket": ""}) ||
			!reflect.DeepEqual(d["columnAxis"], map[string]any{"field": "close_date", "bucket": "month"}) {
			t.Fatalf("axes = %v × %v", d["rowAxis"], d["columnAxis"])
		}
		rows, cols := headerKV(d["rows"]), headerKV(d["columns"])
		if !reflect.DeepEqual(rows, []string{"Won=20.20/1", "Proposal=10.10/1"}) ||
			!reflect.DeepEqual(cols, []string{"2026-01=10.10/1", "2026-02=20.20/1"}) {
			t.Fatalf("rows=%v columns=%v, want [Won=20.20/1 Proposal=10.10/1] [2026-01=10.10/1 2026-02=20.20/1]", rows, cols)
		}
		cells := d["cells"].([]any)
		if len(cells) != 2 {
			t.Fatalf("cells = %v, want 2 (sparse: only cells holding data)", cells)
		}
		c0, c1 := cells[0].(map[string]any), cells[1].(map[string]any)
		if c0["r"] != 0.0 || c0["c"] != 1.0 || !decEq(c0["value"], "20.2") || c0["count"] != 1.0 ||
			c1["r"] != 1.0 || c1["c"] != 0.0 || !decEq(c1["value"], "10.1") || c1["count"] != 1.0 {
			t.Fatalf("cells = %v, want Won×2026-02=20.2 and Proposal×2026-01=10.1", cells)
		}
		total := d["total"].(map[string]any)
		if !decEq(total["value"], "30.3") || total["count"] != 2.0 {
			t.Fatalf("total = %v, want 30.3 over 2 rows", total)
		}
		if !reflect.DeepEqual(d["groups"], d["rows"]) {
			t.Fatalf("groups = %v, want = rows for the single-axis readers", d["groups"])
		}
		// Each row total == the summary by stage, SAME token, exact decimals.
		sum := env.runData(env.repTok, summaryID)
		if !reflect.DeepEqual(headerKV(sum["groups"]), rows) {
			t.Fatalf("summary by stage (rep) = %v, matrix row totals = %v — must be equal", headerKV(sum["groups"]), rows)
		}
	})

	t.Run("admin sees all 5, and its row totals equal its own summary", func(t *testing.T) {
		d := env.runData(env.adminTok, matrixID)
		total := d["total"].(map[string]any)
		if !decEq(total["value"], "631.05") || total["count"] != 5.0 || len(d["cells"].([]any)) != 4 {
			t.Fatalf("admin total = %v cells = %v, want 631.05/5 over 4 cells", total, d["cells"])
		}
		sum := env.runData(env.adminTok, summaryID)
		if !reflect.DeepEqual(headerKV(sum["groups"]), headerKV(d["rows"])) {
			t.Fatalf("admin summary %v != matrix rows %v", headerKV(sum["groups"]), headerKV(d["rows"]))
		}
	})

	t.Run("no view grant: 403 before any query", func(t *testing.T) {
		code, body := env.run(env.noneTok, matrixID)
		if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"FORBIDDEN"`) {
			t.Fatalf("no-grant run: %d %s, want 403 FORBIDDEN", code, body)
		}
	})
}

// TestReportMatrix_OverlayNarrowsTheGrid pins the run overlay (api-conventions §4.4e "overlay như
// §4.4") on the grid path: ?ownerId / ?dateFrom / ?dateTo reach the cross-tab and narrow every
// number, and they AND with the caller's scope instead of replacing it. Without this the handler
// would still echo appliedFilters over an unfiltered grid — the screen says "filtered", the
// numbers are not.
func TestReportMatrix_OverlayNarrowsTheGrid(t *testing.T) {
	env := seedReportGrid(t)
	matrixID := env.createReport("Ma trận có lọc", "matrix", matrixCfg(nil))
	runQ := func(tok, query string) map[string]any {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+matrixID+"/run"+query, tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("run%s: %d %s", query, w.Code, w.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode run: %v (%s)", err, w.Body.String())
		}
		if out.Data["type"] != "matrix" {
			t.Fatalf("run%s type = %v, want matrix", query, out.Data["type"])
		}
		return out.Data
	}

	t.Run("admin ?ownerId=<rep>: the grid is exactly the rep's 2 rows", func(t *testing.T) {
		d := runQ(env.adminTok, "?ownerId="+env.repID)
		total := d["total"].(map[string]any)
		if !decEq(total["value"], "30.3") || total["count"] != 2.0 || len(d["cells"].([]any)) != 2 {
			t.Fatalf("total = %v cells = %v, want 30.3/2 over 2 cells (unfiltered is 631.05/5)", total, d["cells"])
		}
		if rows := headerKV(d["rows"]); !reflect.DeepEqual(rows, []string{"Won=20.20/1", "Proposal=10.10/1"}) {
			t.Fatalf("rows = %v, want the rep's [Won=20.20/1 Proposal=10.10/1]", rows)
		}
		if !reflect.DeepEqual(d["appliedFilters"], map[string]any{"ownerId": env.repID}) {
			t.Fatalf("appliedFilters = %v, want ownerId echoed", d["appliedFilters"])
		}
	})

	t.Run("created_at window before every row: an empty grid, not the unfiltered one", func(t *testing.T) {
		d := runQ(env.adminTok, "?dateFrom=2020-01-01&dateTo=2020-01-31")
		if total := d["total"].(map[string]any); total["count"] != 0.0 ||
			len(d["rows"].([]any)) != 0 || len(d["columns"].([]any)) != 0 || len(d["cells"].([]any)) != 0 {
			t.Fatalf("total = %v rows = %v columns = %v cells = %v, want an empty grid", total, d["rows"], d["columns"], d["cells"])
		}
		if !reflect.DeepEqual(d["appliedFilters"], map[string]any{"dateFrom": "2020-01-01", "dateTo": "2020-01-31"}) {
			t.Fatalf("appliedFilters = %v, want the window echoed", d["appliedFilters"])
		}
	})

	t.Run("rep ?ownerId=<admin>: the overlay cannot widen the owner scope to the admin's rows", func(t *testing.T) {
		d := runQ(env.repTok, "?ownerId="+env.adminID)
		if total := d["total"].(map[string]any); total["count"] != 0.0 || len(d["cells"].([]any)) != 0 {
			t.Fatalf("total = %v cells = %v, want 0 — overlay ANDs with view@owner", total, d["cells"])
		}
	})
}

// TestReportMatrix_HiddenAxisFieldForbidden is T029 (base-field half on the real sys_field_acl;
// the joined and polymorphic axes are pinned on the same run path by
// services.TestReportRun_MatrixHiddenAxisForbidden).
func TestReportMatrix_HiddenAxisFieldForbidden(t *testing.T) {
	env := seedReportGrid(t)
	if _, err := env.sx.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'secret', 0, 0, NOW(3), NOW(3))`,
		uuid.NewString(), repGridTenantID, env.repRole, repGridEntity); err != nil {
		t.Fatalf("seed sys_field_acl: %v", err)
	}
	colHidden := env.createReport("Theo mã nội bộ (cột)", "matrix", matrixCfg(map[string]any{
		"columnGroup": map[string]any{"field": "secret"}}))
	rowHidden := env.createReport("Theo mã nội bộ (hàng)", "matrix", matrixCfg(map[string]any{"groupBy": "secret"}))
	for name, id := range map[string]string{"column axis hidden": colHidden, "row axis hidden": rowHidden} {
		t.Run(name, func(t *testing.T) {
			code, body := env.run(env.repTok, id)
			if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"FIELD_FORBIDDEN"`) {
				t.Fatalf("rep run: %d %s, want 403 FIELD_FORBIDDEN", code, body)
			}
			if strings.Contains(string(body), "-hidden") {
				t.Fatalf("a value of the hidden field is in the body: %s", body)
			}
			// The field is only hidden from the rep: the admin gets the grid.
			if code, body := env.run(env.adminTok, id); code != http.StatusOK || !strings.Contains(string(body), "alpha-hidden") {
				t.Fatalf("admin run: %d %s, want the grid", code, body)
			}
		})
	}
}

// TestReportMatrix_HiddenJoinedAndPolymorphicAxisForbidden is #240 (the other half of T029, which
// the unit test TestReportRun_MatrixHiddenAxisForbidden pins with a fake catalog): through the
// production router, a rep (not admin) whose field-ACL hides (a) a field of the RELATED entity used
// as the column axis "<rel>.<field>", or (b) a polymorphic_relation field used through its
// "<f>_type" axis, gets 403 FIELD_FORBIDDEN before any query, while the same shapes over a visible
// field run (so the 403 is the ACL, not the join). The relation and the polymorphic field are built
// through the admin API, the way an app builder does.
func TestReportMatrix_HiddenJoinedAndPolymorphicAxisForbidden(t *testing.T) {
	env := seedReportGrid(t)
	post := func(path string, body map[string]any) {
		t.Helper()
		if w := doAuthed(t, env.r, http.MethodPost, path, env.adminTok, body, nil); w.Code != http.StatusCreated {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	post("/api/v1/admin/entities", map[string]any{
		"name": repGridRelEntity, "label": "Khách hàng",
		"fields": []map[string]any{
			{"name": "industry", "label": "Ngành", "fieldType": "text", "options": map[string]any{"indexed": true}},
			{"name": "region", "label": "Vùng", "fieldType": "text", "options": map[string]any{"indexed": true}},
		},
	})
	// FK-on-child: the opportunity holds the customer; the registry row is named <entity>_<field>.
	post("/api/v1/admin/fields", map[string]any{
		"entityName": repGridEntity, "name": "account", "label": "Khách hàng", "fieldType": "relation",
		"options": map[string]any{"relatedEntity": repGridRelEntity},
	})
	post("/api/v1/admin/fields", map[string]any{
		"entityName": repGridEntity, "name": "about", "label": "Liên quan tới", "fieldType": "polymorphic_relation",
		"options": map[string]any{"targetEntities": []string{repGridRelEntity}},
	})
	for _, f := range [][2]string{{repGridRelEntity, "industry"}, {repGridEntity, "about"}} {
		if _, err := env.sx.Exec(
			`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 0, 0, NOW(3), NOW(3))`,
			uuid.NewString(), repGridTenantID, env.repRole, f[0], f[1]); err != nil {
			t.Fatalf("seed sys_field_acl: %v", err)
		}
	}
	joined := func(field string) map[string]any {
		return matrixCfg(map[string]any{
			"joins":       []map[string]any{{"relationship": repGridRelName}},
			"columnGroup": map[string]any{"field": repGridRelName + "." + field}})
	}
	cases := []struct {
		name   string
		cfg    map[string]any
		status int
	}{
		{"hidden joined column axis", joined("industry"), http.StatusForbidden},
		{"hidden polymorphic <f>_type column axis", matrixCfg(map[string]any{
			"columnGroup": map[string]any{"field": "about_type"}}), http.StatusForbidden},
		{"visible joined column axis (control)", joined("region"), http.StatusOK},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			id := env.createReport(tt.name, "matrix", tt.cfg)
			code, body := env.run(env.repTok, id)
			if code != tt.status {
				t.Fatalf("rep run: %d %s, want %d", code, body, tt.status)
			}
			if tt.status == http.StatusForbidden && !strings.Contains(string(body), `"code":"FIELD_FORBIDDEN"`) {
				t.Fatalf("rep run body %s, want FIELD_FORBIDDEN", body)
			}
			// Hidden only from the rep: the admin always gets the grid.
			if code, body := env.run(env.adminTok, id); code != http.StatusOK {
				t.Fatalf("admin run: %d %s, want 200", code, body)
			}
		})
	}
}

// TestReportMatrix_LegacySavedMatrixUnchanged is T030: a matrix without a column axis (every matrix
// saved before S2b) answers exactly as a summary with the same config — the whole data object.
func TestReportMatrix_LegacySavedMatrixUnchanged(t *testing.T) {
	env := seedReportGrid(t)
	cfg := map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount"}
	legacyID := env.createReport("Ma trận cũ", "matrix", cfg)
	summaryID := env.createReport("Tổng hợp", "summary", cfg)
	for _, who := range []struct{ name, tok string }{{"rep", env.repTok}, {"admin", env.adminTok}} {
		_, legacy := env.run(who.tok, legacyID)
		_, summary := env.run(who.tok, summaryID)
		if !bytes.Equal(legacy, summary) {
			t.Fatalf("%s: legacy matrix %s != summary %s", who.name, legacy, summary)
		}
		if !strings.Contains(string(legacy), `"type":"summary"`) || strings.Contains(string(legacy), `"cells"`) {
			t.Fatalf("%s: legacy matrix %s, want the single-axis summary shape", who.name, legacy)
		}
	}
}

// TestReportMatrix_TooManyCells422 is T031: the envelope a client gets when the dense grid is over
// the cap (the edge numbers 10×100 / 11×100 are pinned in services T035b). The rep's own 32 rows
// with 32 distinct keys × the 32 record ids = 1024 dense cells, 32 of them holding data.
func TestReportMatrix_TooManyCells422(t *testing.T) {
	env := seedReportGrid(t)
	for i := 0; i < 32; i++ {
		if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+repGridEntity, env.repTok, map[string]any{
			"name": "big", "stage": fmt.Sprintf("S%02d", i), "amount": 1,
		}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", w.Code, w.Body.String())
		}
	}
	id := env.createReport("Quá lớn", "matrix", map[string]any{"groupBy": "stage", "metric": "count",
		"filters":     []map[string]any{{"field": "name", "op": "eq", "values": []string{"big"}}},
		"columnGroup": map[string]any{"field": "id"}})
	code, body := env.run(env.repTok, id)
	var env422 struct {
		Code    string         `json:"code"`
		Error   string         `json:"error"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(body, &env422)
	want := map[string]any{"cap": 1000.0, "cells": 1024.0, "rows": 32.0, "columns": 32.0, "lowerBound": false}
	if code != http.StatusUnprocessableEntity || env422.Code != "REPORT_MATRIX_TOO_MANY_CELLS" || !reflect.DeepEqual(env422.Details, want) {
		t.Fatalf("run: %d %s, want 422 REPORT_MATRIX_TOO_MANY_CELLS %v", code, body, want)
	}
	if strings.Contains(env422.Error, "columnGroup") || strings.Contains(env422.Error, "stage") {
		t.Fatalf("error text %q names a config token", env422.Error)
	}
}

// ---- specs/049 S4: the grid / tree CSV ----------------------------------------------------------

// csvKey / csvVal render a /run key or value the way the file prints it: a group's label when the
// run names it (S6), else its key; a NULL key is the "(trống)" group, a NULL value an empty cell,
// decimals as the exact text the server sent.
func csvKey(g any) string {
	if l, ok := g.(map[string]any)["label"].(string); ok {
		return l
	}
	if k, ok := g.(map[string]any)["key"].(string); ok {
		return k
	}
	return "(trống)"
}

func csvVal(g any) string {
	if v, ok := g.(map[string]any)["value"].(string); ok {
		return v
	}
	return ""
}

// matrixCSVFromRun states the file contract of a matrix (FR-020) from a /run payload: heading
// "<rows> \ <columns>", the column keys, "Tổng"; per row its cells (empty where none) + row total;
// the "Tổng" line of column totals + grand total.
func matrixCSVFromRun(d map[string]any, rowTitle, colTitle string) [][]string {
	cols := d["columns"].([]any)
	head := []string{rowTitle + ` \ ` + colTitle}
	last := []string{"Tổng"}
	for _, c := range cols {
		head, last = append(head, csvKey(c)), append(last, csvVal(c))
	}
	out := [][]string{append(head, "Tổng")}
	rows := d["rows"].([]any)
	grid := make([][]string, len(rows))
	for i, r := range rows {
		grid[i] = make([]string, len(cols)+2)
		grid[i][0], grid[i][len(cols)+1] = csvKey(r), csvVal(r)
	}
	for _, c := range d["cells"].([]any) {
		m := c.(map[string]any)
		grid[int(m["r"].(float64))][1+int(m["c"].(float64))] = csvVal(m)
	}
	return append(append(out, grid...), append(last, csvVal(d["total"])))
}

// treeCSVFromRun states the file contract of a tree from a /run payload: one column per level +
// "Giá trị,Số lượng", depth first, leaves repeating their ancestors, "Tổng <key>" after each
// branch in the branch's own column, "Tổng cộng" last.
func treeCSVFromRun(d map[string]any, titles ...string) [][]string {
	n := len(titles)
	out := [][]string{append(append([]string{}, titles...), "Giá trị", "Số lượng")}
	line := func(path []string, label string, g any) []string {
		l := make([]string, n+2)
		copy(l, path)
		l[len(path)], l[n], l[n+1] = label, csvVal(g), fmt.Sprint(g.(map[string]any)["count"])
		return l
	}
	var walk func(list []any, path []string)
	walk = func(list []any, path []string) {
		for _, g := range list {
			if kids, ok := g.(map[string]any)["children"]; ok {
				walk(kids.([]any), append(path[:len(path):len(path)], csvKey(g)))
				out = append(out, line(path, "Tổng "+csvKey(g), g))
				continue
			}
			out = append(out, line(path, csvKey(g), g))
		}
	}
	walk(d["groups"].([]any), nil)
	return append(out, line(nil, "Tổng cộng", d["total"]))
}

// TestReportGridCSV_MatchesRun is T053 (replaces T032): the employee who is NOT an admin downloads
// the matrix and the tree through the production router; the file is the grid on screen — every
// cell, row / column total, sub-total and grand total equal to /run with the SAME token (exact
// decimal text), headings from the field labels. The admin's file equals the admin's run (the
// larger numbers), the run overlay reaches the file as it reaches /run, a caller with no view grant
// gets 403 and an over-cap grid a 422 — both JSON envelopes, not one CSV byte. A legacy matrix still
// downloads the key,value,count file. (The over-cap case seeds 32 more rows, so it runs last.)
func TestReportGridCSV_MatchesRun(t *testing.T) {
	env := seedReportGrid(t)
	matrixID := env.createReport("Giá trị theo giai đoạn × tháng chốt", "matrix", matrixCfg(nil))
	treeID := env.createReport("Giá trị theo giai đoạn → tháng chốt", "summary", treeCfg("sum", closeMonth))
	download := func(t *testing.T, tok, id, query string) [][]string {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+id+"/export.csv"+query, tok, nil, nil)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
			t.Fatalf("export %s%s: %d %q %s", id, query, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		return parseDownloadedCSV(t, w.Body.String())
	}
	runQ := func(t *testing.T, tok, id, query string) map[string]any {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+id+"/run"+query, tok, nil, nil)
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
			t.Fatalf("run %s%s: %d %s", id, query, w.Code, w.Body.String())
		}
		return out.Data
	}

	t.Run("rep (not admin): the matrix file is the rep's grid, cell for cell", func(t *testing.T) {
		got := download(t, env.repTok, matrixID, "")
		want := [][]string{
			{`Giai đoạn \ Ngày chốt`, "2026-01", "2026-02", "Tổng"},
			{"Won", "", "20.2000000000", "20.2000000000"},
			{"Proposal", "10.1000000000", "", "10.1000000000"},
			{"Tổng", "10.1000000000", "20.2000000000", "30.3000000000"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rep matrix file = %q\nwant %q", got, want)
		}
		if run := matrixCSVFromRun(runQ(t, env.repTok, matrixID, ""), "Giai đoạn", "Ngày chốt"); !reflect.DeepEqual(got, run) {
			t.Fatalf("rep matrix file = %q\n/run (same token) = %q", got, run)
		}
	})

	t.Run("rep (not admin): the tree file is the rep's tree, sub-total for sub-total", func(t *testing.T) {
		got := download(t, env.repTok, treeID, "")
		want := [][]string{
			{"Giai đoạn", "Ngày chốt", "Giá trị", "Số lượng"},
			{"Won", "2026-02", "20.2000000000", "1"},
			{"Tổng Won", "", "20.2000000000", "1"},
			{"Proposal", "2026-01", "10.1000000000", "1"},
			{"Tổng Proposal", "", "10.1000000000", "1"},
			{"Tổng cộng", "", "30.3000000000", "2"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rep tree file = %q\nwant %q", got, want)
		}
		if run := treeCSVFromRun(runQ(t, env.repTok, treeID, ""), "Giai đoạn", "Ngày chốt"); !reflect.DeepEqual(got, run) {
			t.Fatalf("rep tree file = %q\n/run (same token) = %q", got, run)
		}
	})

	t.Run("admin: each file equals the admin's own run (all 5 rows)", func(t *testing.T) {
		m := download(t, env.adminTok, matrixID, "")
		if run := matrixCSVFromRun(runQ(t, env.adminTok, matrixID, ""), "Giai đoạn", "Ngày chốt"); !reflect.DeepEqual(m, run) {
			t.Fatalf("admin matrix file = %q\n/run = %q", m, run)
		}
		if last := m[len(m)-1]; !decEq(last[len(last)-1], "631.05") {
			t.Fatalf("admin grand total = %q, want 631.05", last)
		}
		tr := download(t, env.adminTok, treeID, "")
		if run := treeCSVFromRun(runQ(t, env.adminTok, treeID, ""), "Giai đoạn", "Ngày chốt"); !reflect.DeepEqual(tr, run) {
			t.Fatalf("admin tree file = %q\n/run = %q", tr, run)
		}
	})

	t.Run("the run overlay narrows the file exactly as it narrows /run", func(t *testing.T) {
		q := "?ownerId=" + env.repID
		got := download(t, env.adminTok, matrixID, q)
		if run := matrixCSVFromRun(runQ(t, env.adminTok, matrixID, q), "Giai đoạn", "Ngày chốt"); !reflect.DeepEqual(got, run) {
			t.Fatalf("admin ?ownerId=<rep> file = %q\n/run = %q", got, run)
		}
		if last := got[len(got)-1]; !decEq(last[len(last)-1], "30.3") {
			t.Fatalf("admin ?ownerId=<rep> grand total = %q, want the rep's 30.3", last)
		}
	})

	t.Run("no view grant: 403 envelope, not one CSV byte", func(t *testing.T) {
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+matrixID+"/export.csv", env.noneTok, nil, nil)
		if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") ||
			w.Header().Get("Content-Disposition") != "" || strings.HasPrefix(w.Body.String(), "\xef\xbb\xbf") {
			t.Fatalf("no-grant export: %d %q %q %s", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"), w.Body.String())
		}
	})

	t.Run("a legacy matrix still downloads key,value,count", func(t *testing.T) {
		legacyID := env.createReport("Ma trận cũ", "matrix", map[string]any{"groupBy": "stage", "metric": "count"})
		if rows := download(t, env.repTok, legacyID, ""); len(rows) != 3 || !reflect.DeepEqual(rows[0], []string{"key", "value", "count"}) {
			t.Fatalf("legacy export rows = %v, want the key,value,count header + the rep's 2 stages", rows)
		}
	})

	t.Run("over the cap: 422 envelope with the numbers, not one CSV byte", func(t *testing.T) {
		for i := 0; i < 32; i++ {
			if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+repGridEntity, env.repTok, map[string]any{
				"name": "big", "stage": fmt.Sprintf("S%02d", i), "amount": 1,
			}, nil); w.Code != http.StatusCreated {
				t.Fatalf("seed: %d %s", w.Code, w.Body.String())
			}
		}
		id := env.createReport("Quá lớn", "matrix", map[string]any{"groupBy": "stage", "metric": "count",
			"filters":     []map[string]any{{"field": "name", "op": "eq", "values": []string{"big"}}},
			"columnGroup": map[string]any{"field": "id"}})
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+id+"/export.csv", env.repTok, nil, nil)
		var env422 struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env422)
		if w.Code != http.StatusUnprocessableEntity || env422.Code != "REPORT_MATRIX_TOO_MANY_CELLS" || env422.Details["cells"] != 1024.0 ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Content-Disposition") != "" ||
			strings.HasPrefix(w.Body.String(), "\xef\xbb\xbf") {
			t.Fatalf("over-cap export: %d %q %s, want a 422 REPORT_MATRIX_TOO_MANY_CELLS JSON envelope", w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	})

}

// ---- specs/049 S3: Summary with sub-levels (a tree with sub-totals) ------------------------------

// treeCfg is a summary by stage then the given sub-levels ({field} or {field, bucket} pairs).
func treeCfg(metric string, subs ...map[string]any) map[string]any {
	cfg := map[string]any{"groupBy": "stage", "metric": metric, "subGroups": subs}
	if metric != "count" {
		cfg["field"] = "amount"
	}
	return cfg
}

var closeMonth = map[string]any{"field": "close_date", "bucket": "month"}

// treeKV renders a tree's groups as "key=value/count[children…]" (values exact, as the server sent
// them), so one comparison pins keys, sub-totals, counts, nesting and sibling order.
func treeKV(t *testing.T, list any) []string {
	t.Helper()
	var out []string
	for _, h := range list.([]any) {
		m := h.(map[string]any)
		s := fmt.Sprintf("%v=%v/%v", m["key"], m["value"], m["count"])
		if kids, ok := m["children"]; ok {
			s += fmt.Sprintf("%v", treeKV(t, kids))
		}
		out = append(out, s)
	}
	return out
}

// TestReportMultiLevel_SubtotalsEqualSingleLevel is T044: for every metric, the tree's level-1
// sub-totals are EXACTLY (same decimal text) the one-level summary by stage, and its grand total is
// exactly the scalar aggregate — each run with the same token, for the rep (not admin) and the
// admin. The rep owns 3 Proposal rows over 2 months, so an average of averages would show.
func TestReportMultiLevel_SubtotalsEqualSingleLevel(t *testing.T) {
	env := seedReportGrid(t)
	for _, row := range []struct {
		date   string
		amount float64
	}{{"2026-01-20", 5.5}, {"2026-02-15", 30}} {
		if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+repGridEntity, env.repTok, map[string]any{
			"name": "opp", "stage": "Proposal", "close_date": row.date, "secret": "alpha-hidden", "amount": row.amount,
		}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed record: %d %s", w.Code, w.Body.String())
		}
	}
	for _, metric := range []string{"count", "sum", "avg", "min", "max"} {
		treeID := env.createReport("Theo giai đoạn → tháng chốt ("+metric+")", "summary", treeCfg(metric, closeMonth))
		one := map[string]any{"groupBy": "stage", "metric": metric}
		agg := "/api/v1/" + repGridEntity + "/aggregate?metric=" + metric
		if metric != "count" {
			one["field"], agg = "amount", agg+"&field=amount"
		}
		oneID := env.createReport("Theo giai đoạn ("+metric+")", "summary", one)
		for _, who := range []struct{ name, tok string }{{"rep", env.repTok}, {"admin", env.adminTok}} {
			t.Run(metric+"/"+who.name, func(t *testing.T) {
				d := env.runData(who.tok, treeID)
				if d["type"] != "summary" || !reflect.DeepEqual(d["levels"], []any{
					map[string]any{"field": "stage", "bucket": ""}, map[string]any{"field": "close_date", "bucket": "month"}}) {
					t.Fatalf("type = %v levels = %v, want a summary with levels stage → close_date(month)", d["type"], d["levels"])
				}
				var level1 []any
				for _, g := range d["groups"].([]any) {
					m := g.(map[string]any)
					if len(m["children"].([]any)) == 0 {
						t.Fatalf("level-1 group %v has no children", m)
					}
					for _, c := range m["children"].([]any) {
						if _, ok := c.(map[string]any)["children"]; ok {
							t.Fatalf("leaf %v carries a children key", c)
						}
					}
					level1 = append(level1, map[string]any{"key": m["key"], "value": m["value"], "count": m["count"]})
				}
				if want := env.runData(who.tok, oneID)["groups"]; !reflect.DeepEqual(level1, want) {
					t.Fatalf("level-1 sub-totals %v != one-level summary %v (same token)", level1, want)
				}
				w := doAuthed(t, env.r, http.MethodGet, agg, who.tok, nil, nil)
				var scalar struct {
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &scalar); w.Code != http.StatusOK || err != nil {
					t.Fatalf("scalar aggregate: %d %s", w.Code, w.Body.String())
				}
				total := d["total"].(map[string]any)
				if total["value"] != scalar.Data["value"] || total["count"] != scalar.Data["count"] {
					t.Fatalf("grand total %v != scalar aggregate %v (same token)", total, scalar.Data)
				}
			})
		}
	}
	// The average of Proposal for the rep is over its 3 rows (15.2), not the average of the two
	// month averages (18.9).
	d := env.runData(env.repTok, env.createReport("avg rep", "summary", treeCfg("avg", closeMonth)))
	for _, g := range d["groups"].([]any) {
		if m := g.(map[string]any); m["key"] == "Proposal" && (!decEq(m["value"], "15.2") || m["count"] != 3.0) {
			t.Fatalf("rep Proposal avg = %v/%v, want 15.2 over 3 rows", m["value"], m["count"])
		}
	}
}

// TestReportMultiLevel_OwnerScoped is T045: the rep (view@owner, not admin) gets only the branches
// holding its 2 rows, every sub-total and the grand total adding only those; the admin gets all 5.
// The month level reads in calendar order (admin Proposal: 2026-01 before the larger 2026-02).
func TestReportMultiLevel_OwnerScoped(t *testing.T) {
	env := seedReportGrid(t)
	id := env.createReport("Giá trị theo giai đoạn → tháng chốt", "summary", treeCfg("sum", closeMonth))

	// The sub-levels survive the admin API (request → service → sys_reports.config → response).
	w := doAuthed(t, env.r, http.MethodGet, "/api/v1/admin/reports/"+id, env.adminTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subGroups":[{"field":"close_date","bucket":"month"}]`) {
		t.Fatalf("GET report: %d %s — want config.subGroups close_date/month", w.Code, w.Body.String())
	}

	rep := env.runData(env.repTok, id)
	if got, want := treeKV(t, rep["groups"]), []string{
		"Won=20.2000000000/1[2026-02=20.2000000000/1]",
		"Proposal=10.1000000000/1[2026-01=10.1000000000/1]",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rep tree = %v\nwant %v", got, want)
	}
	if total := rep["total"].(map[string]any); !decEq(total["value"], "30.3") || total["count"] != 2.0 {
		t.Fatalf("rep total = %v, want 30.3 over 2 rows", total)
	}

	admin := env.runData(env.adminTok, id)
	if got, want := treeKV(t, admin["groups"]), []string{
		"Won=320.2000000000/2[2026-01=300.0000000000/1 2026-02=20.2000000000/1]",
		"Proposal=310.8500000000/3[2026-01=110.3500000000/2 2026-02=200.5000000000/1]",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admin tree = %v\nwant %v", got, want)
	}
	if total := admin["total"].(map[string]any); !decEq(total["value"], "631.05") || total["count"] != 5.0 {
		t.Fatalf("admin total = %v, want 631.05 over 5 rows", total)
	}

	if code, body := env.run(env.noneTok, id); code != http.StatusForbidden || !strings.Contains(string(body), `"code":"FORBIDDEN"`) {
		t.Fatalf("no-grant run: %d %s, want 403 FORBIDDEN", code, body)
	}
	// The tree's CSV (S4) is pinned cell for cell against /run in TestReportGridCSV_MatchesRun.
}

// TestReportMultiLevel_BucketLevel is T046 (bucket half): "created month → owner". A time-bucketed
// level reads in calendar order even when a later month holds the larger number; a plain level
// (the owner) reads by value, largest first. Rows are re-dated so created_at = close_date.
func TestReportMultiLevel_BucketLevel(t *testing.T) {
	env := seedReportGrid(t)
	table, err := dynrepo.TableFor(repGridEntity)
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	if _, err := env.sx.Exec("UPDATE `"+table+"` SET created_at = CAST(JSON_UNQUOTE(JSON_EXTRACT(data, '$.close_date')) AS DATETIME) WHERE tenant_id = ?",
		repGridTenantID); err != nil {
		t.Fatalf("re-date rows: %v", err)
	}
	id := env.createReport("Nhỏ nhất theo tháng tạo → người phụ trách", "summary", map[string]any{
		"groupBy": "created_at", "bucket": "month", "metric": "min", "field": "amount",
		"subGroups": []map[string]any{{"field": "ownerId"}}})

	// Rep: 2026-01 min 10.1, 2026-02 min 20.2 — by value the later month would come first.
	if got, want := treeKV(t, env.runData(env.repTok, id)["groups"]), []string{
		"2026-01=10.1000000000/1[" + env.repID + "=10.1000000000/1]",
		"2026-02=20.2000000000/1[" + env.repID + "=20.2000000000/1]",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rep tree = %v\nwant %v", got, want)
	}
	// Admin: the owner level by value — the admin's larger minimum before the rep's.
	if got, want := treeKV(t, env.runData(env.adminTok, id)["groups"]), []string{
		"2026-01=10.1000000000/3[" + env.adminID + "=100.2500000000/2 " + env.repID + "=10.1000000000/1]",
		"2026-02=20.2000000000/2[" + env.adminID + "=200.5000000000/1 " + env.repID + "=20.2000000000/1]",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admin tree = %v\nwant %v", got, want)
	}
}

// TestReportMultiLevel_HiddenSubLevelForbidden is T046 (ACL half): a level-2 or level-3 axis hidden
// from the rep by field-ACL is refused 403 before any query, no value of the hidden field in the
// body; the admin (for whom it is not hidden) gets the tree.
func TestReportMultiLevel_HiddenSubLevelForbidden(t *testing.T) {
	env := seedReportGrid(t)
	if _, err := env.sx.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'secret', 0, 0, NOW(3), NOW(3))`,
		uuid.NewString(), repGridTenantID, env.repRole, repGridEntity); err != nil {
		t.Fatalf("seed sys_field_acl: %v", err)
	}
	secret := map[string]any{"field": "secret"}
	for name, id := range map[string]string{
		"level 2 hidden": env.createReport("Theo mã nội bộ (cấp 2)", "summary", treeCfg("count", secret)),
		"level 3 hidden": env.createReport("Theo mã nội bộ (cấp 3)", "summary", treeCfg("count", closeMonth, secret)),
	} {
		t.Run(name, func(t *testing.T) {
			code, body := env.run(env.repTok, id)
			if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"FIELD_FORBIDDEN"`) {
				t.Fatalf("rep run: %d %s, want 403 FIELD_FORBIDDEN", code, body)
			}
			if strings.Contains(string(body), "-hidden") {
				t.Fatalf("a value of the hidden field is in the body: %s", body)
			}
			if code, body := env.run(env.adminTok, id); code != http.StatusOK || !strings.Contains(string(body), "alpha-hidden") {
				t.Fatalf("admin run: %d %s, want the tree", code, body)
			}
		})
	}
}

// TestReportMultiLevel_TooManyGroups422 is T046 (cap half): 1001 leaf groups under one level-1
// group, owned by the rep, is refused 422 REPORT_TOO_MANY_GROUPS with the exact count (N = 2:
// 1001 leaves + 1 sub-total + 1 total stay under the LIMIT 2·1000+2, so the count is not a lower
// bound) — never a truncated tree. The message names no config token.
func TestReportMultiLevel_TooManyGroups422(t *testing.T) {
	env := seedReportGrid(t)
	table, err := dynrepo.TableFor(repGridEntity)
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	digits := "(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 " +
		"UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
	if _, err := env.sx.Exec("INSERT INTO `"+table+"` (`id`, `tenant_id`, `created_by`, `owner_id`, `data`) "+
		"SELECT UUID(), ?, ?, ?, JSON_OBJECT('name', 'big', 'stage', 'Bulk', 'amount', 1) FROM ("+
		"SELECT a.d + 10*b.d + 100*c.d + 1000*e.d AS n FROM "+digits+" a, "+digits+" b, "+digits+" c, "+
		"(SELECT 0 d UNION ALL SELECT 1) e) s WHERE s.n < ?",
		repGridTenantID, env.repID, env.repID, 1001); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	id := env.createReport("Quá nhiều nhóm", "summary", map[string]any{"groupBy": "stage", "metric": "count",
		"filters":   []map[string]any{{"field": "stage", "op": "eq", "values": []string{"Bulk"}}},
		"subGroups": []map[string]any{{"field": "id"}}})
	code, body := env.run(env.repTok, id)
	var env422 struct {
		Code    string         `json:"code"`
		Error   string         `json:"error"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(body, &env422)
	want := map[string]any{"cap": 1000.0, "groups": 1001.0, "lowerBound": false}
	if code != http.StatusUnprocessableEntity || env422.Code != "REPORT_TOO_MANY_GROUPS" || !reflect.DeepEqual(env422.Details, want) {
		t.Fatalf("run: %d %s, want 422 REPORT_TOO_MANY_GROUPS %v", code, body, want)
	}
	if strings.Contains(env422.Error, "subGroups") || strings.Contains(env422.Error, "stage") {
		t.Fatalf("error text %q names a config token", env422.Error)
	}
}

// ---- specs/049 S5: a threshold on the leaf groups (HAVING) ---------------------------------------

// havingCfg is a threshold as JSON decodes it ([]any values), so an echoed one compares equal.
func havingCfg(on, op string, vals ...string) map[string]any {
	values := make([]any, len(vals))
	for i, v := range vals {
		values[i] = v
	}
	return map[string]any{"on": on, "op": op, "values": values}
}

// withHaving returns cfg plus the threshold (a copy — the base config stays reusable).
func withHaving(cfg map[string]any, h map[string]any) map[string]any {
	out := map[string]any{"having": h}
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

// ratSum adds the exact decimal values of a /run list (a NULL value adds nothing).
func ratSum(list []any) *big.Rat {
	s := new(big.Rat)
	for _, g := range list {
		if v, ok := g.(map[string]any)["value"].(string); ok {
			r, _ := new(big.Rat).SetString(v)
			s.Add(s, r)
		}
	}
	return s
}

// assertTreeAddsKeptLeaves: for a SUM tree, every node's value and count are exactly those of its
// children, and the total those of the level-1 groups — i.e. every total adds only the kept leaves.
func assertTreeAddsKeptLeaves(t *testing.T, d map[string]any) {
	t.Helper()
	var walk func(list []any)
	walk = func(list []any) {
		for _, g := range list {
			m := g.(map[string]any)
			kids, ok := m["children"].([]any)
			if !ok {
				continue
			}
			var cnt float64
			for _, k := range kids {
				cnt += k.(map[string]any)["count"].(float64)
			}
			if !decEq(m["value"], ratSum(kids).FloatString(10)) || m["count"] != cnt {
				t.Fatalf("sub-total %v != the sum of its kept children %v", m, kids)
			}
			walk(kids)
		}
	}
	groups := d["groups"].([]any)
	walk(groups)
	var cnt float64
	for _, g := range groups {
		cnt += g.(map[string]any)["count"].(float64)
	}
	total := d["total"].(map[string]any)
	if total["count"] != cnt || (len(groups) > 0 && !decEq(total["value"], ratSum(groups).FloatString(10))) {
		t.Fatalf("total %v != the sum of the kept groups %v", total, groups)
	}
}

// TestReportHaving_TotalsOverKeptGroupsOnly is T061 (part 1): a threshold keeps only the LEAF groups
// that pass it — the cells of a matrix, the deepest level of a tree, the groups of a one-level
// summary — and every sub-total, row total, column total and grand total adds exactly the kept
// leaves (Excel "value filter"), never the whole set. The run echoes the threshold it applied.
// Admin view of the 5 seeded rows: Proposal×01 = 110.35 (2 rows), Proposal×02 = 200.5,
// Won×01 = 300, Won×02 = 20.2.
func TestReportHaving_TotalsOverKeptGroupsOnly(t *testing.T) {
	env := seedReportGrid(t)
	over150 := havingCfg("value", "gt", "150")

	t.Run("matrix: only cells over 150; row/column/grand totals add the kept cells", func(t *testing.T) {
		d := env.runData(env.adminTok, env.createReport("Ma trận ô > 150", "matrix", matrixCfg(map[string]any{"having": over150})))
		if d["type"] != "matrix" || !reflect.DeepEqual(d["having"], over150) {
			t.Fatalf("type = %v having = %v, want matrix echoing %v", d["type"], d["having"], over150)
		}
		rows, cols := headerKV(d["rows"]), headerKV(d["columns"])
		if !reflect.DeepEqual(rows, []string{"Won=300.00/1", "Proposal=200.50/1"}) ||
			!reflect.DeepEqual(cols, []string{"2026-01=300.00/1", "2026-02=200.50/1"}) {
			t.Fatalf("rows=%v columns=%v, want the kept cells' totals only", rows, cols)
		}
		cells := d["cells"].([]any)
		byRow, byCol := map[float64][]any{}, map[float64][]any{}
		for _, c := range cells {
			m := c.(map[string]any)
			byRow[m["r"].(float64)] = append(byRow[m["r"].(float64)], c)
			byCol[m["c"].(float64)] = append(byCol[m["c"].(float64)], c)
		}
		for i, r := range d["rows"].([]any) {
			if !decEq(r.(map[string]any)["value"], ratSum(byRow[float64(i)]).FloatString(10)) {
				t.Fatalf("row total %v != its kept cells %v", r, byRow[float64(i)])
			}
		}
		for i, c := range d["columns"].([]any) {
			if !decEq(c.(map[string]any)["value"], ratSum(byCol[float64(i)]).FloatString(10)) {
				t.Fatalf("column total %v != its kept cells %v", c, byCol[float64(i)])
			}
		}
		total := d["total"].(map[string]any)
		if len(cells) != 2 || !decEq(total["value"], "500.5") || total["count"] != 2.0 ||
			!decEq(total["value"], ratSum(cells).FloatString(10)) {
			t.Fatalf("cells = %v total = %v, want 2 kept cells adding up to 500.5 (not 631.05)", cells, total)
		}
	})

	t.Run("tree: only deepest-level groups over 150; sub-totals and total add the kept leaves", func(t *testing.T) {
		d := env.runData(env.adminTok, env.createReport("Cây lá > 150", "summary", withHaving(treeCfg("sum", closeMonth), over150)))
		if got, want := treeKV(t, d["groups"]), []string{
			"Won=300.0000000000/1[2026-01=300.0000000000/1]",
			"Proposal=200.5000000000/1[2026-02=200.5000000000/1]",
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("tree = %v\nwant %v", got, want)
		}
		assertTreeAddsKeptLeaves(t, d)
		if !reflect.DeepEqual(d["having"], over150) {
			t.Fatalf("having echo = %v, want %v", d["having"], over150)
		}
	})

	t.Run("tree: a count threshold keeps the one month with 2 rows", func(t *testing.T) {
		atLeast2 := havingCfg("count", "gte", "2")
		d := env.runData(env.adminTok, env.createReport("Cây ≥ 2 dòng", "summary", withHaving(treeCfg("sum", closeMonth), atLeast2)))
		if got, want := treeKV(t, d["groups"]), []string{"Proposal=110.3500000000/2[2026-01=110.3500000000/2]"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("tree = %v, want %v", got, want)
		}
		assertTreeAddsKeptLeaves(t, d)
	})

	t.Run("one-level summary: a one-level tree whose total adds the kept groups", func(t *testing.T) {
		d := env.runData(env.adminTok, env.createReport("Giai đoạn > 315", "summary",
			map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount", "having": havingCfg("value", "gt", "315")}))
		if d["type"] != "summary" || !reflect.DeepEqual(d["levels"], []any{map[string]any{"field": "stage", "bucket": ""}}) {
			t.Fatalf("type = %v levels = %v, want a one-level summary tree", d["type"], d["levels"])
		}
		if got, want := treeKV(t, d["groups"]), []string{"Won=320.2000000000/2"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("groups = %v, want %v (Proposal 310.85 is under the threshold)", got, want)
		}
		if total := d["total"].(map[string]any); !decEq(total["value"], "320.2") || total["count"] != 2.0 {
			t.Fatalf("total = %v, want 320.2 over 2 rows", total)
		}
	})

	t.Run("a matrix without a column axis runs as a one-level tree", func(t *testing.T) {
		d := env.runData(env.adminTok, env.createReport("Ma trận một trục > 315", "matrix",
			map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount", "having": havingCfg("value", "gt", "315")}))
		if d["type"] != "summary" || d["levels"] == nil || !reflect.DeepEqual(treeKV(t, d["groups"]), []string{"Won=320.2000000000/2"}) {
			t.Fatalf("data = %v, want the one-level summary tree", d)
		}
	})
}

// TestReportHaving_AllGroupsRemoved is T061 (part 2, US3 scenario 2): a threshold nothing passes is
// not an error — 200, empty lists (never null), total {value:null, count:0} built by the server
// (MySQL's WITH ROLLUP returns no grand-total row over nothing), the threshold still echoed.
func TestReportHaving_AllGroupsRemoved(t *testing.T) {
	env := seedReportGrid(t)
	never := havingCfg("value", "gt", "1000000")
	for name, id := range map[string]string{
		"matrix":    env.createReport("Ma trận không ô nào", "matrix", matrixCfg(map[string]any{"having": never})),
		"tree":      env.createReport("Cây không nhóm nào", "summary", withHaving(treeCfg("sum", closeMonth), never)),
		"one level": env.createReport("Một cấp không nhóm nào", "summary", map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount", "having": never}),
	} {
		t.Run(name, func(t *testing.T) {
			code, body := env.run(env.repTok, id)
			if code != http.StatusOK {
				t.Fatalf("run: %d %s, want 200", code, body)
			}
			for _, want := range []string{`"groups":[]`, `"total":{"count":0,"value":null}`, `"having":{"on":"value","op":"gt","values":["1000000"]}`} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("body %s lacks %s", body, want)
				}
			}
			if name == "matrix" {
				for _, want := range []string{`"rows":[]`, `"columns":[]`, `"cells":[]`} {
					if !strings.Contains(string(body), want) {
						t.Fatalf("body %s lacks %s", body, want)
					}
				}
			}
		})
	}
}

// TestReportHaving_OwnerScopedEvaluation is T062 (US3 scenario 5 / the spec's "group A"): the
// threshold is judged on the rows the CALLER may read. Stage Alpha holds 3 rows for the admin (900)
// of which the rep owns 1 (300); Beta is the rep's 600. "Value > 500": the admin keeps Alpha and
// Beta, the rep keeps only Beta — Alpha's presence never leaks the admin's larger sum to the rep —
// and the rep's total is exactly the sum of the rep's kept groups. A count threshold is judged the
// same way.
func TestReportHaving_OwnerScopedEvaluation(t *testing.T) {
	env := seedReportGrid(t)
	for _, row := range []struct {
		tok, stage string
		amount     float64
	}{{env.adminTok, "Alpha", 300}, {env.adminTok, "Alpha", 300}, {env.repTok, "Alpha", 300}, {env.repTok, "Beta", 600}} {
		if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+repGridEntity, row.tok, map[string]any{
			"name": "opp", "stage": row.stage, "close_date": "2026-03-01", "secret": "gamma-hidden", "amount": row.amount,
		}, nil); w.Code != http.StatusCreated {
			t.Fatalf("seed record: %d %s", w.Code, w.Body.String())
		}
	}
	base := map[string]any{"groupBy": "stage", "metric": "sum", "field": "amount"}
	over500 := env.createReport("Giai đoạn > 500", "summary", withHaving(base, havingCfg("value", "gt", "500")))

	admin := env.runData(env.adminTok, over500)
	if got, want := treeKV(t, admin["groups"]), []string{"Alpha=900.0000000000/3", "Beta=600.0000000000/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admin groups = %v, want %v", got, want)
	}
	if total := admin["total"].(map[string]any); !decEq(total["value"], "1500") || total["count"] != 4.0 {
		t.Fatalf("admin total = %v, want 1500 over 4 rows", total)
	}

	rep := env.runData(env.repTok, over500)
	if got, want := treeKV(t, rep["groups"]), []string{"Beta=600.0000000000/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rep groups = %v, want %v — Alpha is 300 on the rep's rows", got, want)
	}
	assertTreeAddsKeptLeaves(t, rep)
	// Alpha exists for the rep (300 over its 1 row): the threshold removed it, not the scope.
	if got := treeKV(t, env.runData(env.repTok, env.createReport("Giai đoạn", "summary", base))["groups"]); !strings.Contains(strings.Join(got, " "), "Alpha=300.0000000000/1") {
		t.Fatalf("rep's one-level summary = %v, want Alpha=300/1 among its groups", got)
	}

	twoRows := env.createReport("Giai đoạn ≥ 2 dòng", "summary", withHaving(base, havingCfg("count", "gte", "2")))
	if got, want := treeKV(t, env.runData(env.adminTok, twoRows)["groups"]),
		[]string{"Alpha=900.0000000000/3", "Won=320.2000000000/2", "Proposal=310.8500000000/3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admin ≥ 2 rows = %v, want %v", got, want)
	}
	if d := env.runData(env.repTok, twoRows); len(d["groups"].([]any)) != 0 || d["total"].(map[string]any)["count"] != 0.0 {
		t.Fatalf("rep ≥ 2 rows = %v, want nothing (the rep holds 1 row per stage)", d)
	}
}

// TestReportHaving_NullValueGroupExcluded is T062 (US3 scenario 3): a group whose value is NULL —
// an average over rows with no amount — fails every threshold (SQL NULL comparison), "≠ 0" included.
func TestReportHaving_NullValueGroupExcluded(t *testing.T) {
	env := seedReportGrid(t)
	if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+repGridEntity, env.adminTok, map[string]any{
		"name": "opp", "stage": "Gamma", "close_date": "2026-03-01", "secret": "gamma-hidden",
	}, nil); w.Code != http.StatusCreated {
		t.Fatalf("seed record: %d %s", w.Code, w.Body.String())
	}
	base := map[string]any{"groupBy": "stage", "metric": "avg", "field": "amount"}
	if got := treeKV(t, env.runData(env.adminTok, env.createReport("TB theo giai đoạn", "summary", base))["groups"]); !strings.Contains(strings.Join(got, " "), "Gamma=<nil>/1") {
		t.Fatalf("groups without a threshold = %v, want Gamma with a NULL average", got)
	}
	for _, h := range []map[string]any{havingCfg("value", "neq", "0"), havingCfg("value", "lt", "1000000000"), havingCfg("value", "between", "-1", "1000000000")} {
		d := env.runData(env.adminTok, env.createReport(fmt.Sprintf("TB %v %v", h["op"], h["values"]), "summary", withHaving(base, h)))
		got := treeKV(t, d["groups"])
		if strings.Contains(strings.Join(got, " "), "Gamma") || len(got) != 2 || d["total"].(map[string]any)["count"] != 5.0 {
			t.Fatalf("%v: groups = %v total = %v, want Won and Proposal only (5 rows), Gamma excluded", h, got, d["total"])
		}
	}
}

// TestReportHaving_SingleLevelRejectsOverCap is T063: a one-level summary WITH a threshold goes the
// grid path, so more than 1000 kept groups is refused 422 REPORT_TOO_MANY_GROUPS (N = 1: 1001
// leaves + the total hit LIMIT 1002, so the count is a lower bound) — never cut; the same report
// WITHOUT a threshold keeps the old top-1000 cut byte for byte (spec "Nợ còn mở"); and the cap is
// judged AFTER the threshold (one that keeps nothing runs).
func TestReportHaving_SingleLevelRejectsOverCap(t *testing.T) {
	env := seedReportGrid(t)
	table, err := dynrepo.TableFor(repGridEntity)
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	digits := "(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 " +
		"UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
	if _, err := env.sx.Exec("INSERT INTO `"+table+"` (`id`, `tenant_id`, `created_by`, `owner_id`, `data`) "+
		"SELECT UUID(), ?, ?, ?, JSON_OBJECT('name', 'big', 'stage', 'Bulk', 'amount', 1) FROM ("+
		"SELECT a.d + 10*b.d + 100*c.d + 1000*e.d AS n FROM "+digits+" a, "+digits+" b, "+digits+" c, "+
		"(SELECT 0 d UNION ALL SELECT 1) e) s WHERE s.n < ?",
		repGridTenantID, env.repID, env.repID, 1001); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	base := map[string]any{"groupBy": "id", "metric": "count",
		"filters": []map[string]any{{"field": "stage", "op": "eq", "values": []string{"Bulk"}}}}

	code, body := env.run(env.repTok, env.createReport("Theo mã, ≥ 1 dòng", "summary", withHaving(base, havingCfg("count", "gte", "1"))))
	var env422 struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(body, &env422)
	if want := (map[string]any{"cap": 1000.0, "groups": 1001.0, "lowerBound": true}); code != http.StatusUnprocessableEntity ||
		env422.Code != "REPORT_TOO_MANY_GROUPS" || !reflect.DeepEqual(env422.Details, want) {
		t.Fatalf("run with a threshold: %d %s, want 422 REPORT_TOO_MANY_GROUPS %v", code, body, want)
	}

	plain := env.runData(env.repTok, env.createReport("Theo mã", "summary", base))
	if _, tree := plain["levels"]; tree || len(plain["groups"].([]any)) != 1000 {
		t.Fatalf("run without a threshold: levels=%v groups=%d, want the old one-level top-1000", plain["levels"], len(plain["groups"].([]any)))
	}

	none := env.runData(env.repTok, env.createReport("Theo mã, ≥ 2 dòng", "summary", withHaving(base, havingCfg("count", "gte", "2"))))
	if len(none["groups"].([]any)) != 0 {
		t.Fatalf("a threshold nothing passes must run under the cap: %v groups", len(none["groups"].([]any)))
	}
}

// TestReportHaving_ConfigThroughAdminAPI is T059 (API half): the threshold survives the admin API
// normalized, and a malformed one is refused on save with the SAME code the validator gives the AI
// Builder — the envelope carrying details {field:"having", reason} so the screen can point at it.
func TestReportHaving_ConfigThroughAdminAPI(t *testing.T) {
	env := seedReportGrid(t)
	id := env.createReport("Ngưỡng chuẩn hoá", "summary", map[string]any{"groupBy": "stage", "metric": "count",
		"having": map[string]any{"on": " Count ", "op": "BETWEEN", "values": []string{" 2 ", "5"}}})
	w := doAuthed(t, env.r, http.MethodGet, "/api/v1/admin/reports/"+id, env.adminTok, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"having":{"on":"count","op":"between","values":["2","5"]}`) {
		t.Fatalf("GET report: %d %s — want config.having count between 2 and 5, normalized", w.Code, w.Body.String())
	}

	for _, tt := range []struct {
		name, typ, reason, code string
		cfg                     map[string]any
	}{
		{"reversed range", "summary", "range", "REPORT_HAVING_INVALID",
			map[string]any{"groupBy": "stage", "metric": "count", "having": havingCfg("count", "between", "10", "9.5")}},
		{"not a number", "matrix", "value", "REPORT_HAVING_INVALID",
			matrixCfg(map[string]any{"having": havingCfg("value", "gt", "1e5")})},
		{"tabular", "tabular", "", "REPORT_CONFIG_INVALID",
			map[string]any{"columns": []string{"stage"}, "having": havingCfg("value", "gt", "1")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := doAuthed(t, env.r, http.MethodPost, "/api/v1/admin/reports", env.adminTok, map[string]any{
				"name": "Sai " + tt.name, "entityName": repGridEntity, "type": tt.typ, "config": tt.cfg}, nil)
			var e struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != http.StatusUnprocessableEntity || e.Code != tt.code {
				t.Fatalf("POST: %d %s, want 422 %s", w.Code, w.Body.String(), tt.code)
			}
			if tt.reason != "" && !reflect.DeepEqual(e.Details, map[string]any{"field": "having", "reason": tt.reason}) {
				t.Fatalf("details = %v, want {field:having, reason:%s}", e.Details, tt.reason)
			}
		})
	}
}

// ---- specs/049 S6: names on the axes -------------------------------------------------------------

const (
	repGridAccts = "it_repgrid_accts"
	repGridDeals = "it_repgrid_deals"
)

// TestReportGridCSV_UsesSameAxisLabelsAsRun is T073a (+ the MySQL half of T073): a Matrix "loại ×
// khách hàng" and a tree "khách hàng → loại" over deals whose `kind` is a picklist with Vietnamese
// labels and whose `account` is a relation to customers. Two employees who are NOT admins, both
// seeing every deal: the one who may view the customers reads "Công ty ABC" and "Báo giá" on the
// row/column headings and the sub-total lines of /run, and EXACTLY the same text in the file — no
// customer UUID anywhere in it; the one who may not view the customers reads the raw id in both
// (the name never leaves the server), while the picklist label — metadata of the deal itself —
// still shows. The one-level summary gains `label` on /run while its file keeps key,value,count
// with the stored code (FR-016).
func TestReportGridCSV_UsesSameAxisLabelsAsRun(t *testing.T) {
	env := seedReportGrid(t)
	for _, e := range []map[string]any{
		{"name": repGridAccts, "label": "Khách hàng", "fields": []map[string]any{
			{"name": "name", "label": "Tên công ty", "fieldType": "text"},
		}},
		{"name": repGridDeals, "label": "Thương vụ", "fields": []map[string]any{
			{"name": "name", "label": "Tên", "fieldType": "text"},
			{"name": "kind", "label": "Loại", "fieldType": "picklist", "options": map[string]any{
				"values": []string{"Proposal", "Won"}, "valueLabels": map[string]any{"Proposal": "Báo giá", "Won": "Thắng"}, "indexed": true}},
			{"name": "account", "label": "Khách hàng", "fieldType": "relation", "options": map[string]any{"relatedEntity": repGridAccts}},
			{"name": "amount", "label": "Giá trị", "fieldType": "number"},
		}},
	} {
		if w := doAuthed(t, env.r, http.MethodPost, "/api/v1/admin/entities", env.adminTok, e, nil); w.Code != http.StatusCreated {
			t.Fatalf("create entity %v: %d %s", e["name"], w.Code, w.Body.String())
		}
	}
	create := func(entity string, body map[string]any) string {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodPost, "/api/v1/"+entity, env.adminTok, body, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", entity, w.Code, w.Body.String())
		}
		var out struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Data.ID
	}
	abc := create(repGridAccts, map[string]any{"name": "Công ty ABC"})
	xyz := create(repGridAccts, map[string]any{"name": "Công ty XYZ"})
	for _, d := range []map[string]any{
		{"name": "d1", "kind": "Proposal", "account": abc, "amount": 10.1},
		{"name": "d2", "kind": "Won", "account": xyz, "amount": 20.2},
		{"name": "d3", "kind": "Won", "account": abc, "amount": 5},
		{"name": "d4", "kind": "Proposal", "amount": 1},
	} {
		create(repGridDeals, d)
	}
	seerRole := a4SeedRole(t, env.gdb, repGridTenantID, "lbl-seer", []auth.RoleAction{grant(repGridDeals, "view"), grant(repGridAccts, "view")})
	blindRole := a4SeedRole(t, env.gdb, repGridTenantID, "lbl-blind", []auth.RoleAction{grant(repGridDeals, "view")})
	a4SeedUser(t, env.gdb, repGridTenantID, "seer@"+repGridSlug, false, &seerRole)
	a4SeedUser(t, env.gdb, repGridTenantID, "blind@"+repGridSlug, false, &blindRole)
	seerTok := itLogin(t, env.r, repGridSlug, "seer@"+repGridSlug, authPassword).Data.AccessToken
	blindTok := itLogin(t, env.r, repGridSlug, "blind@"+repGridSlug, authPassword).Data.AccessToken

	report := func(name, typ string, cfg map[string]any) string {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodPost, "/api/v1/admin/reports", env.adminTok, map[string]any{
			"name": name, "entityName": repGridDeals, "type": typ, "config": cfg}, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create report %q: %d %s", name, w.Code, w.Body.String())
		}
		var out struct {
			Data struct {
				Report struct {
					ID string `json:"id"`
				} `json:"report"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Data.Report.ID
	}
	matrixID := report("Loại × khách hàng", "matrix", map[string]any{"groupBy": "kind", "metric": "sum", "field": "amount",
		"columnGroup": map[string]any{"field": "account"}})
	treeID := report("Khách hàng → loại", "summary", map[string]any{"groupBy": "account", "metric": "sum", "field": "amount",
		"subGroups": []map[string]any{{"field": "kind"}}})
	flatID := report("Theo loại", "summary", map[string]any{"groupBy": "kind", "metric": "sum", "field": "amount"})
	download := func(t *testing.T, tok, id string) [][]string {
		t.Helper()
		w := doAuthed(t, env.r, http.MethodGet, "/api/v1/reports/"+id+"/export.csv", tok, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("export %s: %d %s", id, w.Code, w.Body.String())
		}
		return parseDownloadedCSV(t, w.Body.String())
	}
	labels := func(list any) []string {
		var out []string
		for _, g := range list.([]any) {
			l, _ := g.(map[string]any)["label"].(string)
			out = append(out, fmt.Sprintf("%v=%s", g.(map[string]any)["key"], l))
		}
		return out
	}
	noCustomerID := func(t *testing.T, file [][]string) {
		t.Helper()
		for _, line := range file {
			for _, cell := range line {
				if strings.Contains(cell, abc) || strings.Contains(cell, xyz) {
					t.Fatalf("file = %q carries a customer id where the name belongs", file)
				}
			}
		}
	}

	t.Run("may view the customers: names on /run, the same names in the matrix file", func(t *testing.T) {
		d := env.runData(seerTok, matrixID)
		if got, want := labels(d["rows"]), []string{"Won=Thắng", "Proposal=Báo giá"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("row labels = %v, want %v", got, want)
		}
		if got, want := labels(d["columns"]), []string{xyz + "=Công ty XYZ", abc + "=Công ty ABC", "<nil>="}; !reflect.DeepEqual(got, want) {
			t.Fatalf("column labels = %v, want %v", got, want)
		}
		file := download(t, seerTok, matrixID)
		if want := []string{`Loại \ Khách hàng`, "Công ty XYZ", "Công ty ABC", "(trống)", "Tổng"}; !reflect.DeepEqual(file[0], want) {
			t.Fatalf("heading = %q, want %q", file[0], want)
		}
		if got := []string{file[1][0], file[2][0], file[3][0]}; !reflect.DeepEqual(got, []string{"Thắng", "Báo giá", "Tổng"}) {
			t.Fatalf("row headings = %q, want Thắng, Báo giá, Tổng", got)
		}
		if run := matrixCSVFromRun(d, "Loại", "Khách hàng"); !reflect.DeepEqual(file, run) {
			t.Fatalf("file = %q\n/run (same token) = %q", file, run)
		}
		noCustomerID(t, file)
	})

	t.Run("may view the customers: the tree's sub-total lines carry the same names", func(t *testing.T) {
		d := env.runData(seerTok, treeID)
		file := download(t, seerTok, treeID)
		if run := treeCSVFromRun(d, "Khách hàng", "Loại"); !reflect.DeepEqual(file, run) {
			t.Fatalf("file = %q\n/run (same token) = %q", file, run)
		}
		var subtotals []string
		for _, line := range file {
			if strings.HasPrefix(line[0], "Tổng ") {
				subtotals = append(subtotals, line[0])
			}
		}
		if want := []string{"Tổng Công ty XYZ", "Tổng Công ty ABC", "Tổng (trống)", "Tổng cộng"}; !reflect.DeepEqual(subtotals, want) {
			t.Fatalf("sub-total lines = %q, want %q", subtotals, want)
		}
		noCustomerID(t, file)
	})

	t.Run("may NOT view the customers: the raw id on /run and in the file, never the name", func(t *testing.T) {
		d := env.runData(blindTok, matrixID)
		for _, c := range d["columns"].([]any) {
			if _, has := c.(map[string]any)["label"]; has {
				t.Fatalf("column %v carries a label for an employee who may not view the customers", c)
			}
		}
		if got, want := labels(d["rows"]), []string{"Won=Thắng", "Proposal=Báo giá"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("row labels = %v, want %v (the picklist is the deal's own metadata)", got, want)
		}
		file := download(t, blindTok, matrixID)
		if want := []string{`Loại \ Khách hàng`, xyz, abc, "(trống)", "Tổng"}; !reflect.DeepEqual(file[0], want) {
			t.Fatalf("heading = %q, want the raw ids %q", file[0], want)
		}
		if run := matrixCSVFromRun(d, "Loại", "Khách hàng"); !reflect.DeepEqual(file, run) {
			t.Fatalf("file = %q\n/run (same token) = %q", file, run)
		}
		tree := download(t, blindTok, treeID)
		if run := treeCSVFromRun(env.runData(blindTok, treeID), "Khách hàng", "Loại"); !reflect.DeepEqual(tree, run) {
			t.Fatalf("tree file = %q\n/run (same token) = %q", tree, run)
		}
		if strings.Contains(fmt.Sprint(tree), "Công ty") {
			t.Fatalf("tree file = %q leaks a customer name", tree)
		}
	})

	t.Run("one-level summary: label on /run, the file keeps key,value,count with the code", func(t *testing.T) {
		d := env.runData(seerTok, flatID)
		if got, want := labels(d["groups"]), []string{"Won=Thắng", "Proposal=Báo giá"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("group labels = %v, want %v", got, want)
		}
		file := download(t, seerTok, flatID)
		if len(file) != 3 || !reflect.DeepEqual(file[0], []string{"key", "value", "count"}) || file[1][0] != "Won" || file[2][0] != "Proposal" {
			t.Fatalf("one-level file = %q, want key,value,count with the stored codes", file)
		}
	})
}
