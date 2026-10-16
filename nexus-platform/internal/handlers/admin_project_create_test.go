package handlers

// specs/062-customer-workspace T088 - POST /admin/projects through the REAL router, tokens and
// operator gate (service faked): only the workshop's platform operator reaches it (everyone else
// gets the byte-identical 403 and the service is never called), the body maps onto the service
// input, a 201 carries the one-time password in the same envelope POST /admin/tenants uses, and
// every typed refusal keeps its status.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

const createBody = `{"name":"Phòng khám An","slug":"an","industry":"Y tế","adminEmail":"chu@an.test","adminFullName":"Chủ An",` +
	`"generatePassword":true,"start":{"mode":"pack","pack":"clinic"}}`

func TestProjectCreate_OperatorOnly(t *testing.T) {
	callers := []struct {
		name  string
		token func(f *projectFixture, t *testing.T) string
		want  int
	}{
		{"platform operator", func(f *projectFixture, t *testing.T) string { return f.token(t, "builder", true) }, http.StatusCreated},
		{"customer admin without the operator flag", func(f *projectFixture, t *testing.T) string { return f.token(t, "legacyadmin", true) }, http.StatusForbidden},
		{"customer owner / staff (non-admin)", func(f *projectFixture, t *testing.T) string { return f.token(t, "owner", false) }, http.StatusForbidden},
		{"builder workspace-session token", func(f *projectFixture, t *testing.T) string { return f.sessionToken(t) }, http.StatusForbidden},
		{"no token", func(*projectFixture, *testing.T) string { return "" }, http.StatusUnauthorized},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			f := newProjectFixture(t)
			w := f.do(http.MethodPost, pjList, c.token(f, t), createBody)
			if w.Code != c.want {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), c.want)
			}
			if c.want == http.StatusForbidden && w.Body.String() != gateForbiddenBody {
				t.Errorf("403 body = %s, want byte-identical %s", w.Body.String(), gateForbiddenBody)
			}
			if called := f.svc.calls > 0; called != (c.want == http.StatusCreated) {
				t.Errorf("service reached = %v for status %d", called, c.want)
			}
		})
	}
}

func TestProjectCreate_MapsTheBodyAndAnswers201(t *testing.T) {
	f := newProjectFixture(t)
	f.svc.createPassword = "show-once-pw"
	w := f.do(http.MethodPost, pjList, f.token(t, "builder", true), createBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	in := f.svc.gotCreate
	if in.Name != "Phòng khám An" || in.Slug != "an" || in.Industry != "Y tế" || in.AdminEmail != "chu@an.test" ||
		in.AdminFullName != "Chủ An" || !in.GeneratePassword || in.StartMode != "pack" || in.StartPack != "clinic" {
		t.Errorf("service input = %+v, want every body field mapped", in)
	}
	if f.svc.gotHome.UserID != "builder" || f.svc.gotHome.TenantID != "home-1" || f.svc.gotHome.SessionID != "sid" {
		t.Errorf("home subject = %+v, want the builder's verified token subject", f.svc.gotHome)
	}
	body := w.Body.String()
	for _, want := range []string{`"project":{`, `"admin":{`, `"start":{"mode":"pack","status":"skipped"}`, `"workspace":{`,
		`"oneTimeCredentials":{"password":"show-once-pw"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
}

func TestProjectCreate_NoOneTimeBlockWhenThePasswordWasSupplied(t *testing.T) {
	f := newProjectFixture(t) // fake returns no one-time password
	w := f.do(http.MethodPost, pjList, f.token(t, "builder", true),
		`{"name":"B","slug":"b","adminEmail":"x@b.test","adminPassword":"Mật-khẩu-đủ-dài-1","start":{"mode":"blank"}}`)
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "oneTimeCredentials") {
		t.Fatalf("status = %d body = %s, want 201 without oneTimeCredentials", w.Code, w.Body.String())
	}
}

func TestProjectCreate_BadBodyNeverReachesTheService(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":   `{broken`,
		"empty":       ``,
		"wrong type":  `{"name":5}`,
		"unknown key": `{"slug":"a","stage":"live","start":{"mode":"blank"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newProjectFixture(t)
			w := f.do(http.MethodPost, pjList, f.token(t, "builder", true), body)
			if w.Code != http.StatusBadRequest || f.svc.calls != 0 {
				t.Fatalf("status = %d calls = %d (%s), want 400 and no service call", w.Code, f.svc.calls, w.Body.String())
			}
		})
	}
}

func TestProjectCreate_ErrorStatusMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{&services.ValidationError{Code: "PROJECT_START_MODE_INVALID", Msg: "x"}, http.StatusUnprocessableEntity, "PROJECT_START_MODE_INVALID"},
		{&services.ValidationError{Code: "PROJECT_INDUSTRY_TOO_LONG", Msg: "x"}, http.StatusUnprocessableEntity, "PROJECT_INDUSTRY_TOO_LONG"},
		{&services.ValidationError{Code: "VERTICAL_BUILTIN_NOT_FOUND", Msg: "x"}, http.StatusNotFound, "VERTICAL_BUILTIN_NOT_FOUND"},
		{&services.ValidationError{Code: "TENANT_ALREADY_EXISTS", Msg: "x"}, http.StatusConflict, "TENANT_ALREADY_EXISTS"},
		{&services.ValidationError{Code: "TENANT_FIELD_REQUIRED", Msg: "x"}, http.StatusUnprocessableEntity, "TENANT_FIELD_REQUIRED"},
		{errors.New("boom"), http.StatusInternalServerError, "PROJECT_FAILED"},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			f := newProjectFixture(t)
			f.svc.err = tc.err
			w := f.do(http.MethodPost, pjList, f.token(t, "builder", true), createBody)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(w.Body.String(), "boom") {
				t.Fatalf("internal error text leaked to the client: %s", w.Body.String())
			}
		})
	}
}
