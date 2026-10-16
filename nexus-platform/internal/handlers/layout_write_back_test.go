package handlers

// layout_write_back_test.go — DAILY-QUEUE #65 `layout-write-back-merge`, measured 2026-09-28.
//
// The queued bug: a user whose role hides a field (sys_field_acl can_view=0) opens Bố cục →
// Danh sách, changes something unrelated and saves. GET /metadata/layouts strips hidden fields
// at serve time (StripHiddenLayoutFields), so the JSON that user sends back no longer carries
// the maker's `kanbanField` (or the column) on that field — and the save would erase it from
// sys_layouts for everyone, with no signal beforehand.
//
// Tracing every writer of sys_layouts shows that round trip cannot happen today:
//   - the only HTTP writers, POST/PUT /admin/layouts, are mounted on builderGated
//     (RequireAdmin, plus the operator tier in production — router.go);
//   - the pack installer writes pack content and Doctor rewrites raw rows, neither from a
//     stripped read; API keys are floored at IsAdmin:false and never pass RequireAdmin;
//   - an admin bypasses field-ACL on the read twice over (hiddenFieldsFor returns nil,
//     FieldACLService.HiddenFields returns an empty set), even when the admin's own role
//     carries a can_view=0 grant.
//
// So everyone who can write a layout was served the unstripped layout, and a merge-on-write
// keyed on the saver's hidden set would be a no-op for every caller who can reach it. This
// test pins that invariant as the property the bug is actually about — "a GET-then-save round
// trip never erases a declaration on a field the saver cannot see" — through the production
// router, for the two roles that matter. The router is built WITHOUT the operator tier: that is
// the wider of the two gates, so if even it refuses the field-hidden member, production does too.
//
// What this test catches (each proven by a mutation that turned it red):
//   - dropping RequireAdmin/builderGated from the existing POST/PUT/DELETE /admin/layouts;
//   - a NEW non-GET route that this router mounts with "layout" in its path or an
//     AdminLayoutHandler method as its handler, reachable by the field-hidden member (step 2b);
//   - dropping BOTH admin bypasses on the read (dropping only one leaves the other covering).
// What it does NOT catch — the reviewer of such a slice must build the merge-on-write in
// LayoutService first (keep the stored parts the saver may not view; specs/028 Nợ còn mở,
// "Ghi-đè-mù khi lưu lại"), because this test stays green:
//   - a gate that admits a named role (a non-admin "designer", per-role layout editing):
//     r-sales-rep holds no such grant, so it is still refused — green by construction;
//   - a sys_layouts writer on another handler under a path without "layout" (e.g. saved "views"):
//     only the Layout handler is mounted here;
//   - a non-HTTP writer (pack install, Doctor, AI Builder) that starts reading a stripped layout.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	wbTenant   = "t-wb"
	wbEntity   = "wb_activities"
	wbLayoutID = "layout-wb-list"
	wbRepRole  = "r-sales-rep"
	wbAdmRole  = "r-admin-own"
)

// wbParam matches a gin path parameter, so the route walk can fill it with a real id.
var wbParam = regexp.MustCompile(`:[^/]+`)

// wbMeta serves one entity with two picklists and a date field, so the declared kanbanField is
// not simply "the first picklist" (the #5 fixture rule: declared and guessed must diverge).
type wbMeta struct{}

func (wbMeta) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if tenantID != wbTenant || name != wbEntity {
		return nil, metadata.ErrEntityNotFound
	}
	return &metadata.EntityDefinition{ID: "e-wb", TenantID: wbTenant, Name: wbEntity}, nil
}

func (wbMeta) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	born := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(name, ft string) metadata.FieldDefinition {
		return metadata.FieldDefinition{ID: "f-" + name, EntityID: "e-wb", TenantID: wbTenant, Name: name, FieldType: ft, CreatedAt: born}
	}
	return []metadata.FieldDefinition{mk("subject", "text"), mk("type", "picklist"), mk("status", "picklist"), mk("due_date", "date")}, nil
}

// wbRepo is an in-memory sys_layouts with the production upsert key (tenant, entity, view) and a
// write counter, so a refused write can be shown to have touched nothing.
type wbRepo struct {
	rows   map[string]*metadata.LayoutDefinition
	writes int
}

func (r *wbRepo) GetByEntity(_ context.Context, tenantID, entityName string) ([]metadata.LayoutDefinition, error) {
	var out []metadata.LayoutDefinition
	for _, row := range r.rows {
		if row.TenantID == tenantID && row.EntityName == entityName {
			out = append(out, *row)
		}
	}
	return out, nil
}

func (r *wbRepo) GetByID(_ context.Context, tenantID, id string) (*metadata.LayoutDefinition, error) {
	if row, ok := r.rows[id]; ok && row.TenantID == tenantID {
		cp := *row
		return &cp, nil
	}
	return nil, metadata.ErrLayoutNotFound
}

func (r *wbRepo) Upsert(_ context.Context, l *metadata.LayoutDefinition) error {
	r.writes++
	for _, row := range r.rows {
		if row.TenantID == l.TenantID && row.EntityName == l.EntityName && row.ViewType == l.ViewType {
			row.LayoutJSON = l.LayoutJSON
			return nil
		}
	}
	cp := *l
	r.rows[l.ID] = &cp
	return nil
}

func (r *wbRepo) UpdateByID(_ context.Context, tenantID, id, layoutJSON string) error {
	r.writes++
	row, ok := r.rows[id]
	if !ok || row.TenantID != tenantID {
		return metadata.ErrLayoutNotFound
	}
	row.LayoutJSON = layoutJSON
	return nil
}

func (r *wbRepo) HardDelete(context.Context, string, string) error { return nil }

// wbGrants is the sys_field_acl loader: `status` is can_view=0 for BOTH roles — the sales rep's
// and the admin's own role — so the admin row proves the bypass, not the absence of a grant.
type wbGrants struct{}

func (wbGrants) LoadFieldACLForEntity(_ context.Context, tenantID, roleID, entity string) ([]auth.FieldACL, error) {
	if tenantID != wbTenant || entity != wbEntity || (roleID != wbRepRole && roleID != wbAdmRole) {
		return nil, nil
	}
	return []auth.FieldACL{{TenantID: tenantID, RoleID: roleID, EntityName: entity, FieldName: "status", CanView: false, CanEdit: false}}, nil
}

// storedWBList decodes the list layout exactly as persisted.
func storedWBList(t *testing.T, repo *wbRepo) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(repo.rows[wbLayoutID].LayoutJSON), &out); err != nil {
		t.Fatalf("decode stored layout: %v", err)
	}
	return out
}

// hasDeclarationAndColumn reports whether a list layout still declares kanbanField=status AND
// places the status column — the two things the round trip must never erase.
func hasDeclarationAndColumn(list map[string]any) bool {
	if list["kanbanField"] != "status" {
		return false
	}
	cols, _ := list["columns"].([]any)
	for _, c := range cols {
		if m, ok := c.(map[string]any); ok && m["field"] == "status" {
			return true
		}
	}
	return false
}

func TestLayoutWriteBack_RoundTripNeverErasesHiddenDeclaration(t *testing.T) {
	cases := []struct {
		name     string
		claims   *auth.Claims
		stripped bool // whether this principal is served the list WITHOUT `status` (asserted, so a case cannot pass vacuously)
		wantW    int  // status of both writes (POST upsert + PUT by id)
	}{
		// The queue item's saver: a non-admin whose role hides `status`. It IS served a stripped
		// layout — sending that back would erase the declaration — so the write must be refused
		// before any handler or repository runs.
		{"field-hidden member (crm_sales_rep shape)", claimsFor(wbTenant, wbRepRole, false), true, http.StatusForbidden},
		// Everyone who CAN write is an admin, and an admin is served the unstripped layout even
		// when its own role carries the same can_view=0 grant — so its round trip is lossless.
		{"admin whose own role hides the field", claimsFor(wbTenant, wbAdmRole, true), false, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &wbRepo{rows: map[string]*metadata.LayoutDefinition{wbLayoutID: {
				ID: wbLayoutID, TenantID: wbTenant, EntityName: wbEntity, ViewType: services.LayoutViewList,
				LayoutJSON: `{"version":1,"columns":[{"field":"subject"},{"field":"type"},{"field":"status"}],"kanbanField":"status","calendarField":"due_date"}`,
			}}}
			svc := services.NewLayoutService(wbMeta{}, repo, zap.NewNop())

			gin.SetMode(gin.TestMode)
			r := gin.New()
			RegisterAPIRoutes(r.Group("/api/v1"), &stubVerifier{byToken: map[string]*auth.Claims{"tok": tc.claims}}, nil, APIHandlers{
				Layout: NewAdminLayoutHandler(svc, zap.NewNop()).WithFieldACL(services.NewFieldACLService(wbGrants{})),
			}, zap.NewNop())
			call := func(method, path string, body any) *httptest.ResponseRecorder {
				var buf []byte
				if body != nil {
					buf, _ = json.Marshal(body)
				}
				req := httptest.NewRequest(method, path, bytes.NewReader(buf))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer tok")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				return w
			}

			// 1. Open the layout the way Studio does.
			w := call(http.MethodGet, "/api/v1/metadata/layouts/"+wbEntity+"?view=list", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("GET layout = %d %s", w.Code, w.Body.String())
			}
			var got struct {
				Data struct {
					Views map[string]map[string]any `json:"views"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode GET: %v", err)
			}
			served := got.Data.Views[services.LayoutViewList]
			if served == nil {
				t.Fatalf("GET layout served no list view: %s", w.Body.String())
			}
			// Envelope keys the handler adds; a client edits the layout, not the envelope.
			delete(served, "isDefault")
			delete(served, "id")
			servedJSON, _ := json.Marshal(served)
			if names := strings.Contains(string(servedJSON), `"status"`); names == tc.stripped {
				t.Fatalf("served list names status=%v, want %v: %s", names, !tc.stripped, servedJSON)
			}

			// 2. "Change something unrelated" and save it back — both write routes Studio uses.
			served["defaultSort"] = map[string]any{"field": "updated_at", "order": "desc"}
			before := repo.writes
			if w := call(http.MethodPost, "/api/v1/admin/layouts", map[string]any{
				"entityName": wbEntity, "viewType": services.LayoutViewList, "layoutJson": served,
			}); w.Code != tc.wantW {
				t.Fatalf("POST /admin/layouts = %d %s; want %d", w.Code, w.Body.String(), tc.wantW)
			}
			if w := call(http.MethodPut, "/api/v1/admin/layouts/"+wbLayoutID, map[string]any{"layoutJson": served}); w.Code != tc.wantW {
				t.Fatalf("PUT /admin/layouts/:id = %d %s; want %d", w.Code, w.Body.String(), tc.wantW)
			}
			if tc.wantW == http.StatusForbidden && repo.writes != before {
				t.Fatalf("a refused write reached the repository (%d writes)", repo.writes-before)
			}

			// 2b. Every write route this router mounts for layouts — not only the two above — must
			// refuse the field-hidden member. Catches a NEW write route (e.g. a POST on the protected
			// group) that the explicit calls would never hit. The body is one a lax gate would accept.
			if tc.wantW == http.StatusForbidden {
				seen := map[string]bool{}
				for _, ri := range r.Routes() {
					if ri.Method == http.MethodGet || ri.Method == http.MethodHead || ri.Method == http.MethodOptions {
						continue
					}
					if !strings.Contains(strings.ToLower(ri.Path), "layout") && !strings.Contains(ri.Handler, "AdminLayoutHandler") {
						continue
					}
					seen[ri.Method+" "+ri.Path] = true
					path := wbParam.ReplaceAllStringFunc(ri.Path, func(p string) string {
						if p == ":entity" {
							return wbEntity
						}
						return wbLayoutID
					})
					before := repo.writes
					if w := call(ri.Method, path, map[string]any{
						"entityName": wbEntity, "viewType": services.LayoutViewList, "layoutJson": served,
					}); w.Code != http.StatusForbidden {
						t.Fatalf("%s %s = %d for the field-hidden member; want 403 (%s)", ri.Method, ri.Path, w.Code, w.Body.String())
					}
					if repo.writes != before {
						t.Fatalf("%s %s: a refused write reached the repository", ri.Method, ri.Path)
					}
				}
				// Anti-vacuity: the walk must have seen the three known writers.
				for _, want := range []string{"POST /api/v1/admin/layouts", "PUT /api/v1/admin/layouts/:id", "DELETE /api/v1/admin/layouts/:id"} {
					if !seen[want] {
						t.Fatalf("route walk never saw %s; saw %v", want, seen)
					}
				}
			}

			// 3. The property: the other maker's declaration and column survive the round trip.
			if stored := storedWBList(t, repo); !hasDeclarationAndColumn(stored) {
				t.Fatalf("round trip erased kanbanField/column on the hidden field; stored now: %v", stored)
			}
		})
	}
}
