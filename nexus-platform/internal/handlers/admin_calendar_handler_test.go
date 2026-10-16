package handlers

// httptest — AdminCalendarHandler (CALENDAR-1). Pins the wire contract:
//   * every route sits behind RequireAdmin (non-admin → 403, nothing executed);
//   * create 201 {data:{calendar}} with hours/holidays echoed; list {data:{calendars}};
//   * validation codes map to the right statuses (422 tz/hours, 409 dup, 404 unknown id);
//   * the handler passes the VERIFIED tenant from context — never anything client-sent.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// hFakeCalendarStore is the handlers-side in-memory store (structurally satisfies the
// service's calendarStore interface). It records the tenant of the last call for the
// tenant-pin assert.
type hFakeCalendarStore struct {
	rows       map[string]*calmeta.BusinessCalendar
	seq        int
	lastTenant string
	usages     []calmeta.Usage // rules pinning every calendar (handler-level fake)
}

func newHFakeCalendarStore() *hFakeCalendarStore {
	return &hFakeCalendarStore{rows: map[string]*calmeta.BusinessCalendar{}}
}

func (f *hFakeCalendarStore) Create(_ context.Context, in calmeta.CreateInput) (string, error) {
	f.lastTenant = in.TenantID
	for _, r := range f.rows {
		if r.TenantID == in.TenantID && r.Name == in.Name {
			return "", calmeta.ErrCalendarDuplicateName
		}
	}
	f.seq++
	id := "cal-" + string(rune('0'+f.seq))
	f.rows[id] = &calmeta.BusinessCalendar{
		ID: id, TenantID: in.TenantID, Name: in.Name, Timezone: in.Timezone,
		Hours: in.Hours, Holidays: in.Holidays, IsDefault: in.IsDefault,
	}
	return id, nil
}

func (f *hFakeCalendarStore) GetByID(_ context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	f.lastTenant = tenantID
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, calmeta.ErrCalendarNotFound
	}
	cp := *r
	if len(cp.Holidays) == 0 {
		cp.Holidays = json.RawMessage("[]")
	}
	return &cp, nil
}

func (f *hFakeCalendarStore) List(_ context.Context, tenantID string) ([]calmeta.BusinessCalendar, error) {
	f.lastTenant = tenantID
	out := make([]calmeta.BusinessCalendar, 0)
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *hFakeCalendarStore) Update(_ context.Context, tenantID, id string, in calmeta.UpdateInput) error {
	f.lastTenant = tenantID
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return calmeta.ErrCalendarNotFound
	}
	r.Name, r.Timezone, r.Hours, r.Holidays, r.IsDefault = in.Name, in.Timezone, in.Hours, in.Holidays, in.IsDefault
	return nil
}

func (f *hFakeCalendarStore) SoftDelete(_ context.Context, tenantID, id string) error {
	f.lastTenant = tenantID
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return calmeta.ErrCalendarNotFound
	}
	delete(f.rows, id)
	return nil
}

func (f *hFakeCalendarStore) Usages(_ context.Context, _, _ string) ([]calmeta.Usage, error) {
	return f.usages, nil
}

// DAILY-QUEUE #326: DELETE of a pinned calendar → 409 CALENDAR_IN_USE with details.usages.
func TestAdminCalendarHandler_DeletePinnedIs409(t *testing.T) {
	r, store := newCalendarTestRouter(true)
	if w := doCalendarReq(r, http.MethodPost, "/admin/business-calendars", validCalendarBody()); w.Code != http.StatusCreated {
		t.Fatalf("seed = %d", w.Code)
	}
	var id string
	for k := range store.rows {
		id = k
	}
	store.usages = []calmeta.Usage{{WorkflowID: "w1", WorkflowName: "Nhắc hạn"}}

	w := doCalendarReq(r, http.MethodDelete, "/admin/business-calendars/"+id, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete pinned = %d body=%s; want 409", w.Code, w.Body.String())
	}
	for _, want := range []string{`"code":"CALENDAR_IN_USE"`, `"workflowName":"Nhắc hạn"`, `"workflowId":"w1"`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(want)) {
			t.Errorf("body %s lacks %s", w.Body.String(), want)
		}
	}
	if len(store.rows) != 1 {
		t.Fatal("row deleted despite 409")
	}
}

// newCalendarTestRouter mounts claims-injection (simulating AuthMiddleware output) → the
// REAL middleware.RequireAdmin → the handler, mirroring the production adminGated chain.
func newCalendarTestRouter(isAdmin bool) (*gin.Engine, *hFakeCalendarStore) {
	store := newHFakeCalendarStore()
	svc := services.NewBusinessCalendarService(store, nil)
	h := NewAdminCalendarHandler(svc, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "actor-1", TenantID: "tenant-1", IsAdmin: isAdmin},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "tenant-1")
		c.Next()
	})
	gated := r.Group("")
	gated.Use(middleware.RequireAdmin(zap.NewNop()))
	h.RegisterRoutes(gated)
	return r, store
}

func doCalendarReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func validCalendarBody() map[string]any {
	return map[string]any{
		"name":     "Giờ hành chính",
		"timezone": "Asia/Ho_Chi_Minh",
		"hours": map[string]any{
			"mon": []map[string]string{{"start": "08:00", "end": "12:00"}, {"start": "13:00", "end": "17:00"}},
		},
		"holidays":  []string{"2026-09-02"},
		"isDefault": true,
	}
}

func TestAdminCalendarHandler_RequireAdminGate(t *testing.T) {
	r, store := newCalendarTestRouter(false) // non-admin

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/admin/business-calendars"},
		{http.MethodGet, "/admin/business-calendars/cal-1"},
		{http.MethodPost, "/admin/business-calendars"},
		{http.MethodPut, "/admin/business-calendars/cal-1"},
		{http.MethodDelete, "/admin/business-calendars/cal-1"},
	} {
		w := doCalendarReq(r, req.method, req.path, validCalendarBody())
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s as non-admin = %d; want 403", req.method, req.path, w.Code)
		}
	}
	if len(store.rows) != 0 {
		t.Fatal("non-admin request reached the store")
	}
}

func TestAdminCalendarHandler_CRUDRoundTrip(t *testing.T) {
	r, store := newCalendarTestRouter(true)

	// Create → 201 with the calendar envelope.
	w := doCalendarReq(r, http.MethodPost, "/admin/business-calendars", validCalendarBody())
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s; want 201", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Calendar calmeta.BusinessCalendar `json:"calendar"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	cal := created.Data.Calendar
	if cal.Name != "Giờ hành chính" || cal.Timezone != "Asia/Ho_Chi_Minh" || !cal.IsDefault {
		t.Fatalf("created = %+v", cal)
	}
	if !bytes.Contains(cal.Hours, []byte(`"08:00"`)) || !bytes.Contains(cal.Holidays, []byte("2026-09-02")) {
		t.Fatalf("hours/holidays not echoed: hours=%s holidays=%s", cal.Hours, cal.Holidays)
	}
	// Tenant pin: the store only ever saw the VERIFIED context tenant.
	if store.lastTenant != "tenant-1" {
		t.Fatalf("store tenant = %q; want tenant-1", store.lastTenant)
	}

	// List → the calendar shows up under data.calendars.
	w = doCalendarReq(r, http.MethodGet, "/admin/business-calendars", nil)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"calendars"`)) {
		t.Fatalf("list = %d body=%s", w.Code, w.Body.String())
	}

	// Update (rename) → 200 with the new name.
	body := validCalendarBody()
	body["name"] = "Ca hành chính v2"
	w = doCalendarReq(r, http.MethodPut, "/admin/business-calendars/"+cal.ID, body)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("Ca hành chính v2")) {
		t.Fatalf("update = %d body=%s", w.Code, w.Body.String())
	}

	// Delete → 204; the row is gone.
	w = doCalendarReq(r, http.MethodDelete, "/admin/business-calendars/"+cal.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d; want 204", w.Code)
	}
	if len(store.rows) != 0 {
		t.Fatal("row survived delete")
	}
}

func TestAdminCalendarHandler_ErrorStatuses(t *testing.T) {
	r, _ := newCalendarTestRouter(true)

	// Seed one valid calendar for the duplicate check.
	if w := doCalendarReq(r, http.MethodPost, "/admin/business-calendars", validCalendarBody()); w.Code != http.StatusCreated {
		t.Fatalf("seed = %d", w.Code)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		mutate     func(map[string]any)
		wantStatus int
		wantCode   string
	}{
		{"bad timezone", http.MethodPost, "/admin/business-calendars",
			func(b map[string]any) { b["name"] = "x"; b["timezone"] = "Mars/Olympus" }, 422, "CALENDAR_TIMEZONE_INVALID"},
		{"bad hours", http.MethodPost, "/admin/business-calendars",
			func(b map[string]any) {
				b["name"] = "y"
				b["hours"] = map[string]any{"mon": []map[string]string{{"start": "12:00", "end": "08:00"}}}
			}, 422, "CALENDAR_HOURS_INVALID"},
		{"bad holidays", http.MethodPost, "/admin/business-calendars",
			func(b map[string]any) { b["name"] = "z"; b["holidays"] = []string{"09/02/2026"} }, 422, "CALENDAR_HOLIDAYS_INVALID"},
		{"empty hours", http.MethodPost, "/admin/business-calendars",
			func(b map[string]any) { b["name"] = "w"; b["hours"] = map[string]any{} }, 422, "CALENDAR_HOURS_EMPTY"},
		{"duplicate name", http.MethodPost, "/admin/business-calendars",
			func(b map[string]any) {}, 409, "CALENDAR_ALREADY_EXISTS"},
		{"unknown id on update", http.MethodPut, "/admin/business-calendars/ghost",
			func(b map[string]any) { b["name"] = "u" }, 404, "CALENDAR_NOT_FOUND"},
		{"unknown id on delete", http.MethodDelete, "/admin/business-calendars/ghost",
			nil, 404, "CALENDAR_NOT_FOUND"},
		{"unknown id on get", http.MethodGet, "/admin/business-calendars/ghost",
			nil, 404, "CALENDAR_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]any
			if tt.mutate != nil {
				body = validCalendarBody()
				tt.mutate(body)
			}
			w := doCalendarReq(r, tt.method, tt.path, body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s; want %d", w.Code, w.Body.String(), tt.wantStatus)
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(tt.wantCode)) {
				t.Fatalf("body %s missing code %s", w.Body.String(), tt.wantCode)
			}
		})
	}
}
