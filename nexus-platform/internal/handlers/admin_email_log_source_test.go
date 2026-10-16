package handlers

// specs/066 L3 (T074): GET /admin/email-log?source= and the rows the auth sources produce.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/services"
)

func TestAdminEmailLogList_SourceFilter(t *testing.T) {
	cases := []struct {
		name, query string
		status      int
		code        string
		reached     bool
		wantSource  string
	}{
		{name: "reset_link passes through", query: "?source=reset_link", status: 200, reached: true, wantSource: "reset_link"},
		{name: "password_reset passes through", query: "?source=password_reset", status: 200, reached: true, wantSource: "password_reset"},
		{name: "workflow passes through", query: "?source=workflow&status=failed", status: 200, reached: true, wantSource: "workflow"},
		{name: "no source = any", query: "", status: 200, reached: true},
		{name: "unknown source", query: "?source=x", status: 400, code: "EMAIL_LOG_FILTER_INVALID"},
		{name: "source is case-sensitive, not guessed", query: "?source=Reset_Link", status: 400, code: "EMAIL_LOG_FILTER_INVALID"},
		{name: "SQL-ish value is just an unknown source", query: "?source=workflow'%20OR%201=1", status: 400, code: "EMAIL_LOG_FILTER_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &hFakeEmailLogStore{}
			r := buildEmailLogEngine(t, services.NewEmailLogService(store))
			w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log"+tc.query, "tok-admin")
			if w.Code != tc.status || (store.calls > 0) != tc.reached {
				t.Fatalf("status %d reached=%v, want %d / %v: %s", w.Code, store.calls > 0, tc.status, tc.reached, w.Body)
			}
			if tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("body %s lacks code %s", w.Body, tc.code)
			}
			if tc.reached && store.lastFilter.Source != tc.wantSource {
				t.Errorf("filter.Source = %q, want %q", store.lastFilter.Source, tc.wantSource)
			}
		})
	}
	// B21: the same path answers 403 to staff and 200 to the tenant admin.
	store := &hFakeEmailLogStore{}
	r := buildEmailLogEngine(t, services.NewEmailLogService(store))
	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log?source=reset_link", "tok-user"); w.Code != http.StatusForbidden || store.calls != 0 {
		t.Errorf("staff = %d (calls %d), want 403 and no store call", w.Code, store.calls)
	}
}

func TestAdminEmailLogList_AuthRows(t *testing.T) {
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	store := &hFakeEmailLogStore{rows: map[string][]emaillog.Row{"t1": {
		{ID: "pw", CreatedAt: at, Source: "password_reset", Status: "failed", RecipientMasked: "t***@example.com", SubjectTemplate: strPtr("Đặt lại mật khẩu")},
		{ID: "lk", CreatedAt: at, Source: "reset_link", Status: "failed", RecipientMasked: "t***@example.com", CreatedBy: strPtr("u-adm"), CreatedByName: strPtr("Quản trị An")},
		{ID: "wf", CreatedAt: at, Source: "workflow", Status: "failed", RecipientMasked: "a***@x.vn",
			WorkflowID: strPtr("w"), ActionIndex: intPtr(0), RecordID: strPtr("r"), WorkflowName: strPtr("Rule")},
	}}}
	r := buildEmailLogEngine(t, services.NewEmailLogService(store))
	w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, row := range body.Data {
		byID[row["id"].(string)] = row
	}
	// A failed auth mail is never resendable: its token is single use (issue a new link instead).
	if byID["pw"]["canResend"] != false || byID["lk"]["canResend"] != false || byID["wf"]["canResend"] != true {
		t.Errorf("canResend pw=%v lk=%v wf=%v, want false/false/true", byID["pw"]["canResend"], byID["lk"]["canResend"], byID["wf"]["canResend"])
	}
	if byID["lk"]["createdByName"] != "Quản trị An" || byID["lk"]["createdBy"] != "u-adm" {
		t.Errorf("reset_link row issuer = %v / %v", byID["lk"]["createdBy"], byID["lk"]["createdByName"])
	}
	if v, ok := byID["pw"]["createdByName"]; !ok || v != nil {
		t.Errorf("a forgot-password row has no issuer: createdByName = %v (present=%v), want an explicit null", v, ok)
	}
}
