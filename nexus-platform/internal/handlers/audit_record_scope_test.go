package handlers

// Per-record audit history for NON-ADMINS. The tab used to answer 403 for everyone but an admin,
// so a rep could open and edit a lead but never see who changed its status. Widening that read is
// only safe with the two checks the record screen itself makes, and these tests pin both:
//
//   - the ROW must be visible under the caller's scope, and a refusal must look exactly like the
//     record read's refusal (404, not a 403 that would confirm the row exists);
//   - every value in a diff goes through field-level ACL, in BEFORE as well as AFTER — showing
//     only the old value of a salary is still showing the salary.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

type scopeFakeReader struct {
	rows []audit.AuditRow
	got  audit.QueryFilter
}

func (f *scopeFakeReader) Query(_ context.Context, _ string, q audit.QueryFilter) ([]audit.AuditRow, string, error) {
	f.got = q
	// Return copies: the handler redacts in place and the fixture is reused across subtests.
	out := make([]audit.AuditRow, len(f.rows))
	copy(out, f.rows)
	return out, "", nil
}

// scopeFakeProbe answers the visibility question. err != nil ⇒ the record service refused.
type scopeFakeProbe struct {
	err        error
	called     bool
	sharedOnly bool // the caller reaches the record only through a manual share (FR-009 marker)
}

func (p *scopeFakeProbe) Get(_ context.Context, _ services.RecordScope, _, _, _ string, _, _ []string) (*dynrepo.Record, error) {
	p.called = true
	if p.err != nil {
		return nil, p.err
	}
	return &dynrepo.Record{ID: "rec-1", SharedWithMe: p.sharedOnly}, nil
}

// scopeFakeFieldACL removes the named keys from every map it is handed.
type scopeFakeFieldACL struct {
	hidden  []string
	batches int
}

func (f *scopeFakeFieldACL) FilterReadFieldsBatch(_ context.Context, _, _, _ string, isAdmin bool, datas []map[string]any) error {
	f.batches++
	if isAdmin {
		return nil
	}
	for _, d := range datas {
		for _, k := range f.hidden {
			delete(d, k)
		}
	}
	return nil
}

// scopeRouter mounts the per-record route with an injected verified subject. accessLevel mirrors
// what RequireACLAction stashes; isAdmin flips the super-gate.
func scopeRouter(reader AuditReader, probe recordVisibilityProbe, facl fieldReadFilter, isAdmin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "tenant-1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "user-1", TenantID: "tenant-1", RoleID: "role-rep", IsAdmin: isAdmin,
		}})
		c.Set("aclAccessLevel", "owner")
		c.Set("aclUserID", "user-1")
		c.Next()
	})
	h := NewAuditHandler(reader, zap.NewNop())
	if probe != nil || facl != nil {
		h.WithRecordScope(probe, facl)
	}
	h.RegisterRecordRoutes(r.Group("/api/v1"))
	return r
}

func diffRow(before, after map[string]any) audit.AuditRow {
	b, _ := json.Marshal(map[string]map[string]any{"before": before, "after": after})
	return audit.AuditRow{ID: "a1", Action: "update", EntityName: "crm_leads", Diff: b}
}

func doHistory(t *testing.T, r *gin.Engine) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit-log/crm_leads/rec-1", nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestRecordHistory_NonAdminSeesHistoryOfAVisibleRow(t *testing.T) {
	reader := &scopeFakeReader{rows: []audit.AuditRow{
		diffRow(map[string]any{"status": "New"}, map[string]any{"status": "Working"}),
	}}
	probe := &scopeFakeProbe{}
	code, body := doHistory(t, scopeRouter(reader, probe, &scopeFakeFieldACL{}, false))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a user who may open the record may see who changed it", code)
	}
	if !probe.called {
		t.Error("visibility probe was not called: entity-level view is not row-level permission")
	}
	if reader.got.EntityName != "crm_leads" || reader.got.RecordID != "rec-1" {
		t.Errorf("filter = %+v, want it pinned to the requested record", reader.got)
	}
	if rows, _ := body["data"].([]any); len(rows) != 1 {
		t.Errorf("data = %v, want the one entry", body["data"])
	}
}

func TestRecordHistory_InvisibleRowRefusesLikeTheRecordRead(t *testing.T) {
	// Both shapes the record service actually returns. The sentinels are NOT ValidationErrors —
	// a first cut matched only ValidationError and answered 500 for a record that does not exist
	// (caught by asking the live API for a random uuid, not by these fakes).
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"row not visible or absent", services.ErrRecordNotFound, "RECORD_NOT_FOUND"},
		{"entity absent", services.ErrEntityNotFound, "ENTITY_NOT_FOUND"},
		{"service validation refusal", &services.ValidationError{Code: "RECORD_NOT_FOUND", Msg: "x"}, "RECORD_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doHistory(t, scopeRouter(
				&scopeFakeReader{}, &scopeFakeProbe{err: tc.err}, &scopeFakeFieldACL{}, false))
			if code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 — a 403 or 500 here is a different answer than the record read gives", code)
			}
			if body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}

func TestRecordHistory_FieldACLRedactsBothSidesOfTheDiff(t *testing.T) {
	reader := &scopeFakeReader{rows: []audit.AuditRow{
		diffRow(
			map[string]any{"status": "New", "salary": 1000},
			map[string]any{"status": "Working", "salary": 2000},
		),
	}}
	facl := &scopeFakeFieldACL{hidden: []string{"salary"}}
	code, body := doHistory(t, scopeRouter(reader, &scopeFakeProbe{}, facl, false))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	raw, _ := json.Marshal(body)
	if s := string(raw); strings.Contains(s, "salary") || strings.Contains(s, "1000") || strings.Contains(s, "2000") {
		t.Fatalf("denied field survived in the response: %s", s)
	}
	if !strings.Contains(string(raw), "Working") {
		t.Error("redaction ate the permitted field too")
	}
	if facl.batches != 1 {
		t.Errorf("field-ACL called %d times, want ONE batch for the whole page", facl.batches)
	}
}

func TestRecordHistory_AdminSkipsProbeAndRedaction(t *testing.T) {
	reader := &scopeFakeReader{rows: []audit.AuditRow{
		diffRow(map[string]any{"salary": 1000}, map[string]any{"salary": 2000}),
	}}
	probe := &scopeFakeProbe{err: &services.ValidationError{Code: "RECORD_NOT_FOUND", Msg: "x"}}
	facl := &scopeFakeFieldACL{hidden: []string{"salary"}}
	code, body := doHistory(t, scopeRouter(reader, probe, facl, true))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — admin behaviour must be unchanged", code)
	}
	if probe.called {
		t.Error("admin should not pay for a row probe: is_admin already bypasses ACL")
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "salary") {
		t.Error("admin lost a field to redaction that never applied to them")
	}
}

// TestRecordHistory_UnwiredFailsClosed: if the deps are ever left unwired, a non-admin must get
// the old refusal rather than an unscoped trail.
func TestRecordHistory_UnwiredFailsClosed(t *testing.T) {
	code, _ := doHistory(t, scopeRouter(&scopeFakeReader{}, nil, nil, false))
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the record scope is not wired", code)
	}
}

// TestRecordHistory_ShareRowsHiddenFromShareOnlyViewer pins specs/051 §6 (DAILY-QUEUE #226 point 2):
// GET /:entity/:id/shares is pinned to `edit` so a read-only viewer cannot learn who else the record
// is shared with — the share/unshare audit rows carry exactly that (`sharedWith` = a user id) and must
// not leak it through the view-pinned history. A viewer who reaches the record by role keeps them.
func TestRecordHistory_ShareRowsHiddenFromShareOnlyViewer(t *testing.T) {
	rows := []audit.AuditRow{
		{ID: "a1", Action: audit.ActionShare, EntityName: "crm_leads", Diff: audit.RawJSON(`{"after":{"sharedWith":"user-c"}}`)},
		diffRow(map[string]any{"status": "New"}, map[string]any{"status": "Working"}),
		{ID: "a3", Action: audit.ActionUnshare, EntityName: "crm_leads", Diff: audit.RawJSON(`{"before":{"sharedWith":"user-c"}}`)},
	}
	for _, tc := range []struct {
		name       string
		sharedOnly bool
		isAdmin    bool
		wantActs   []string
	}{
		{"share-only viewer: share/unshare rows dropped", true, false, []string{"update"}},
		{"role viewer: unchanged", false, false, []string{"share", "update", "unshare"}},
		{"admin: unchanged", false, true, []string{"share", "update", "unshare"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &scopeFakeProbe{sharedOnly: tc.sharedOnly}
			code, body := doHistory(t, scopeRouter(&scopeFakeReader{rows: rows}, probe, &scopeFakeFieldACL{}, tc.isAdmin))
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			var got []string
			for _, r := range body["data"].([]any) {
				got = append(got, r.(map[string]any)["action"].(string))
			}
			if strings.Join(got, ",") != strings.Join(tc.wantActs, ",") {
				t.Fatalf("actions = %v; want %v", got, tc.wantActs)
			}
		})
	}
}
