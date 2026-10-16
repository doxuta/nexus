package handlers

// specs/068-tenant-backup-restore T009 — the four backup routes through the REAL router with REAL
// tokens and the REAL operator gate (service faked): only the workshop's platform operator reaches
// them; a customer's own admin, a customer's staff and a builder workspace-session token all get the
// same 403 body as every other /admin/tenants route, and the service is never called for them.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
	"nexus/platform/internal/tenantbackup"
)

type fakeBackupSvc struct {
	calls   int
	gotMt   tenantbackup.MaintenanceInput
	gotIn   tenantbackup.CreateInput
	gotList tenantbackup.ListInput
	err     error
	file    *tenantbackup.FileDownload
	gotPrev tenantbackup.PreviewInput
	gotRest tenantbackup.RestoreInput

	gotEnabled *bool
	gotKeep    *int
}

func (f *fakeBackupSvc) StartRestore(_ context.Context, in tenantbackup.RestoreInput) (*tenantbackup.RestoreStarted, error) {
	f.calls++
	f.gotRest = in
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.RestoreStarted{RestoreID: "r1", Status: "running"}, nil
}

func (f *fakeBackupSvc) Delete(context.Context, string, tenantbackup.Actor, string) error {
	f.calls++
	return f.err
}

func (f *fakeBackupSvc) GetSchedule(context.Context, string) (*tenantbackup.ScheduleView, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.ScheduleView{Enabled: true, KeepScheduled: 7, HourLocal: 2}, nil
}

func (f *fakeBackupSvc) PutSchedule(_ context.Context, _ string, _ tenantbackup.Actor, enabled *bool, keep *int) (*tenantbackup.ScheduleView, error) {
	f.calls++
	f.gotEnabled, f.gotKeep = enabled, keep
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.ScheduleView{Enabled: true, KeepScheduled: 7, HourLocal: 2}, nil
}

func (f *fakeBackupSvc) Preview(_ context.Context, in tenantbackup.PreviewInput) (*tenantbackup.RestorePlan, error) {
	f.calls++
	f.gotPrev = in
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.RestorePlan{BackupID: in.BackupID, Integrity: tenantbackup.PlanOK{OK: true}}, nil
}

func (f *fakeBackupSvc) Create(_ context.Context, in tenantbackup.CreateInput) (*tenantbackup.BackupView, error) {
	f.calls++
	f.gotIn = in
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.BackupView{ID: "b1", TenantID: in.TenantID, Kind: "manual", Status: "running"}, nil
}

func (f *fakeBackupSvc) List(_ context.Context, in tenantbackup.ListInput) ([]tenantbackup.BackupView, tenantbackup.ListMeta, error) {
	f.calls++
	f.gotList = in
	if f.err != nil {
		return nil, tenantbackup.ListMeta{}, f.err
	}
	return []tenantbackup.BackupView{{ID: "b1", TenantID: in.TenantID, Status: "ready"}}, tenantbackup.ListMeta{CurrentSchemaVersion: 84}, nil
}

func (f *fakeBackupSvc) Get(_ context.Context, tenantID, _, id string) (*tenantbackup.BackupView, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.BackupView{ID: id, TenantID: tenantID, Status: "ready"}, nil
}

func (f *fakeBackupSvc) OpenFile(_ context.Context, _ string, _ tenantbackup.Actor, _ string) (*tenantbackup.FileDownload, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.file, nil
}

func (f *fakeBackupSvc) SetMaintenance(_ context.Context, in tenantbackup.MaintenanceInput) (*tenantbackup.MaintenanceView, error) {
	f.calls++
	f.gotMt = in
	if f.err != nil {
		return nil, f.err
	}
	return &tenantbackup.MaintenanceView{MaintenanceUntil: time.Date(2026, 10, 14, 10, 5, 0, 0, time.UTC)}, nil
}

func (f *fakeBackupSvc) ClearMaintenance(_ context.Context, in tenantbackup.MaintenanceInput) error {
	f.calls++
	f.gotMt = in
	return f.err
}

type backupFixture struct {
	r      *gin.Engine
	tokens *auth.TokenService
	svc    *fakeBackupSvc
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	tokens, err := auth.NewTokenService("a-sufficiently-long-real-secret-value-1234567890", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	users := gateUsers{byID: map[string]*auth.User{
		"builder":     {ID: "builder", Status: "active", IsAdmin: true, IsPlatformOperator: true},
		"legacyadmin": {ID: "legacyadmin", Status: "active", IsAdmin: true},
		"staff":       {ID: "staff", Status: "active", IsAdmin: false},
	}}
	f := &backupFixture{tokens: tokens, svc: &fakeBackupSvc{file: &tenantbackup.FileDownload{
		Name: "nexus-qa-x-20261007-010203-manual.zip", Size: 5, Body: io.NopCloser(strings.NewReader("PK-zip")),
	}}}
	gin.SetMode(gin.TestMode)
	f.r = gin.New()
	RegisterAPIRoutes(f.r.Group("/api/v1"), tokens, nil, APIHandlers{
		Auth:         NewAuthHandler(services.NewAuthService(nil, nil, nil), zap.NewNop()),
		TenantBackup: NewAdminTenantBackupHandler(f.svc, users, time.Minute, zap.NewNop()),
	}, zap.NewNop(), WithLiveSubject(users), WithWorkspaceResolver(&statefulResolver{}))
	return f
}

func (f *backupFixture) token(t *testing.T, uid string, admin bool) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessToken(auth.Subject{UserID: uid, TenantID: "home-1", IsAdmin: admin, SessionID: "sid"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *backupFixture) sessionToken(t *testing.T) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: "builder", TenantID: "customer-1", IsAdmin: true, SessionID: "sid",
		ActorUserID: "builder", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *backupFixture) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

const (
	bkBase = "/api/v1/admin/tenants/11111111-1111-4111-8111-111111111111/backups"
	bkOne  = bkBase + "/22222222-2222-4222-8222-222222222222"
	mtPath = "/api/v1/admin/tenants/11111111-1111-4111-8111-111111111111/maintenance"

	schedPath = "/api/v1/admin/tenants/11111111-1111-4111-8111-111111111111/backup-schedule"
)

func TestBackupSchedule_ForwardsAndMapsRefusals(t *testing.T) {
	f := newBackupFixture(t)
	tok := f.token(t, "builder", true)

	w := f.do(http.MethodPut, schedPath, tok, `{"enabled":false,"keepScheduled":4}`)
	if w.Code != http.StatusOK || f.svc.gotEnabled == nil || *f.svc.gotEnabled || f.svc.gotKeep == nil || *f.svc.gotKeep != 4 {
		t.Fatalf("put: %d %s (saw %v %v)", w.Code, w.Body.String(), f.svc.gotEnabled, f.svc.gotKeep)
	}
	if w := f.do(http.MethodPut, schedPath, tok, `{broken`); w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: %d, want 400", w.Code)
	}
	if w := f.do(http.MethodPut, schedPath, tok, `{"keepScheduled":4}`); w.Code != http.StatusOK || f.svc.gotEnabled != nil {
		t.Errorf("missing enabled must reach the service as nil (its 422 to give): %d", w.Code)
	}
	for _, c := range []struct {
		code string
		want int
	}{{"BACKUP_SCHEDULE_INVALID", http.StatusUnprocessableEntity}, {"BACKUP_DELETE_IN_USE", http.StatusConflict}, {"BACKUP_NOT_FOUND", http.StatusNotFound}} {
		f.svc.err = &tenantbackup.Error{Code: c.code, Msg: "x"}
		method, path := http.MethodDelete, bkOne
		if c.code == "BACKUP_SCHEDULE_INVALID" {
			method, path = http.MethodPut, schedPath
		}
		if w := f.do(method, path, tok, `{"enabled":true,"keepScheduled":0}`); w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.code, w.Code, c.want)
		}
	}
}

func TestBackupRoutes_OperatorOnly(t *testing.T) {
	routes := []struct {
		name, method, path, body string
		okStatus                 int
	}{
		{"list", http.MethodGet, bkBase, "", http.StatusOK},
		{"create", http.MethodPost, bkBase, `{"note":"x"}`, http.StatusAccepted},
		{"get", http.MethodGet, bkOne, "", http.StatusOK},
		{"file", http.MethodGet, bkOne + "/file", "", http.StatusOK},
		{"freeze", http.MethodPost, mtPath, `{"minutes":5}`, http.StatusOK},
		{"unfreeze", http.MethodDelete, mtPath, "", http.StatusNoContent},
		{"delete", http.MethodDelete, bkOne, "", http.StatusNoContent},
		{"schedule get", http.MethodGet, schedPath, "", http.StatusOK},
		{"schedule put", http.MethodPut, schedPath, `{"enabled":true,"keepScheduled":3}`, http.StatusOK},
	}
	callers := []struct {
		name  string
		token func(f *backupFixture, t *testing.T) string
		want  func(ok int) int // expected status
	}{
		{"platform operator", func(f *backupFixture, t *testing.T) string { return f.token(t, "builder", true) }, func(ok int) int { return ok }},
		{"customer admin without the operator flag", func(f *backupFixture, t *testing.T) string { return f.token(t, "legacyadmin", true) }, func(int) int { return http.StatusForbidden }},
		{"customer staff (non-admin)", func(f *backupFixture, t *testing.T) string { return f.token(t, "staff", false) }, func(int) int { return http.StatusForbidden }},
		{"builder workspace-session token", func(f *backupFixture, t *testing.T) string { return f.sessionToken(t) }, func(int) int { return http.StatusForbidden }},
		{"no token", func(*backupFixture, *testing.T) string { return "" }, func(int) int { return http.StatusUnauthorized }},
	}
	for _, rt := range routes {
		for _, c := range callers {
			t.Run(rt.name+"/"+c.name, func(t *testing.T) {
				f := newBackupFixture(t)
				w := f.do(rt.method, rt.path, c.token(f, t), rt.body)
				want := c.want(rt.okStatus)
				if w.Code != want {
					t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), want)
				}
				if want == http.StatusForbidden && w.Body.String() != gateForbiddenBody {
					t.Errorf("403 body = %s, want byte-identical %s", w.Body.String(), gateForbiddenBody)
				}
				if called := f.svc.calls > 0; called != (want == rt.okStatus) {
					t.Errorf("service reached = %v for status %d", called, want)
				}
			})
		}
	}
}

func TestBackupCreate_ForwardsTargetNoteAndWhoIsAsking(t *testing.T) {
	f := newBackupFixture(t)
	w := f.do(http.MethodPost, bkBase, f.token(t, "builder", true), `{"note":"trước nâng cấp"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"status":"running"`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	in := f.svc.gotIn
	if in.TenantID != "11111111-1111-4111-8111-111111111111" || in.Note != "trước nâng cấp" ||
		in.Actor.UserID != "builder" || in.Actor.TenantID != "home-1" {
		t.Errorf("service saw %+v", in)
	}
	// an empty body is fine (the note is optional)
	w = f.do(http.MethodPost, bkBase, f.token(t, "builder", true), "")
	if w.Code != http.StatusAccepted {
		t.Errorf("empty body: status = %d (%s)", w.Code, w.Body.String())
	}
	w = f.do(http.MethodPost, bkBase, f.token(t, "builder", true), "{broken")
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d", w.Code)
	}
}

func TestBackupRoutes_ErrorStatusMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{&tenantbackup.Error{Code: "TENANT_NOT_FOUND", Msg: "x"}, http.StatusNotFound, "TENANT_NOT_FOUND"},
		{&tenantbackup.Error{Code: "BACKUP_NOT_FOUND", Msg: "x"}, http.StatusNotFound, "BACKUP_NOT_FOUND"},
		{&tenantbackup.Error{Code: "BACKUP_IN_PROGRESS", Msg: "x"}, http.StatusConflict, "BACKUP_IN_PROGRESS"},
		{&tenantbackup.Error{Code: "BACKUP_NOT_READY", Msg: "x"}, http.StatusConflict, "BACKUP_NOT_READY"},
		{&tenantbackup.Error{Code: "BACKUP_FILE_MISSING", Msg: "x"}, http.StatusConflict, "BACKUP_FILE_MISSING"},
		{&tenantbackup.Error{Code: "BACKUP_DISK_LOW", Msg: "x", Details: map[string]any{"needBytes": 1, "freeBytes": 0}}, http.StatusConflict, "BACKUP_DISK_LOW"},
		{&tenantbackup.Error{Code: "BACKUP_NOTE_INVALID", Msg: "x"}, http.StatusUnprocessableEntity, "BACKUP_NOTE_INVALID"},
		{&tenantbackup.Error{Code: "MAINTENANCE_SELF", Msg: "x"}, http.StatusUnprocessableEntity, "MAINTENANCE_SELF"},
		{&tenantbackup.Error{Code: "MAINTENANCE_MINUTES_INVALID", Msg: "x"}, http.StatusUnprocessableEntity, "MAINTENANCE_MINUTES_INVALID"},
		{&tenantbackup.Error{Code: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED", Msg: "x", Details: map[string]any{"tables": []string{"t"}}}, http.StatusUnprocessableEntity, "TENANT_BACKUP_SCHEMA_UNCLASSIFIED"},
		{&tenantbackup.Error{Code: "BACKUP_CURSOR_INVALID", Msg: "x"}, http.StatusBadRequest, "BACKUP_CURSOR_INVALID"},
		{io.ErrUnexpectedEOF, http.StatusInternalServerError, "BACKUP_FAILED"},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			f := newBackupFixture(t)
			f.svc.err = tc.err
			w := f.do(http.MethodPost, bkBase, f.token(t, "builder", true), "")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(w.Body.String(), "unexpected EOF") {
				t.Error("a 500 must not echo the internal error")
			}
		})
	}
}

func TestBackupList_PassesPagingAndCaller(t *testing.T) {
	f := newBackupFixture(t)
	w := f.do(http.MethodGet, bkBase+"?limit=5&after=abc", f.token(t, "builder", true), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"currentSchemaVersion":84`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if g := f.svc.gotList; g.Limit != 5 || g.After != "abc" || g.CallerTenantID != "home-1" || g.TenantID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("service saw %+v", g)
	}
	for _, bad := range []string{"?limit=0", "?limit=-3", "?limit=abc"} {
		if w := f.do(http.MethodGet, bkBase+bad, f.token(t, "builder", true), ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, w.Code)
		}
	}
}

// specs/068 T040/T050: POST/DELETE /admin/tenants/:id/maintenance forward the target, the minutes and
// who is asking; a missing / non-integer minutes is a 400 at the edge (the 1..120 range is the
// service's 422); the answer is {data:{maintenanceUntil}} / 204.
func TestMaintenanceRoutes_ForwardAndAnswer(t *testing.T) {
	f := newBackupFixture(t)
	tok := f.token(t, "builder", true)

	w := f.do(http.MethodPost, mtPath, tok, `{"minutes":15}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"maintenanceUntil":"2026-10-14T10:05:00Z"`) {
		t.Fatalf("freeze: %d %s", w.Code, w.Body.String())
	}
	if g := f.svc.gotMt; g.TenantID != "11111111-1111-4111-8111-111111111111" || g.Minutes != 15 ||
		g.Actor.UserID != "builder" || g.Actor.TenantID != "home-1" {
		t.Errorf("service saw %+v", g)
	}
	for _, bad := range []string{``, `{}`, `{"minutes":"5"}`, `{broken`, `{"minutes":1.5}`} {
		if w := f.do(http.MethodPost, mtPath, tok, bad); w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", bad, w.Code)
		}
	}
	if w := f.do(http.MethodDelete, mtPath, tok, ""); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Errorf("unfreeze: %d %q", w.Code, w.Body.String())
	}
	if g := f.svc.gotMt; g.Actor.UserID != "builder" || g.TenantID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("unfreeze service saw %+v", g)
	}
}

func TestBackupDownload_Headers(t *testing.T) {
	f := newBackupFixture(t)
	w := f.do(http.MethodGet, bkOne+"/file", f.token(t, "builder", true), "")
	if w.Code != http.StatusOK || w.Body.String() != "PK-zip" {
		t.Fatalf("status = %d body = %q", w.Code, w.Body.String())
	}
	want := map[string]string{
		"Content-Type":           "application/zip",
		"Content-Disposition":    `attachment; filename="nexus-qa-x-20261007-010203-manual.zip"`,
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Content-Length":         "5",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// specs/068 T060 — POST /admin/tenants/:id/restores (lát C: chỉ kiểm tra).

const rsPath = "/api/v1/admin/tenants/11111111-1111-4111-8111-111111111111/restores"

func TestRestoreRoute_OperatorOnlyPairedWithAdmin200(t *testing.T) {
	body := `{"backupId":"22222222-2222-4222-8222-222222222222"}`
	f := newBackupFixture(t)
	if w := f.do(http.MethodPost, rsPath, f.token(t, "builder", true), body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"integrity":{"ok":true}`) {
		t.Fatalf("operator: status = %d body = %s", w.Code, w.Body.String())
	}
	if g := f.svc.gotPrev; g.TenantID != "11111111-1111-4111-8111-111111111111" || g.BackupID != "22222222-2222-4222-8222-222222222222" || g.CallerTenantID != "home-1" {
		t.Errorf("service saw %+v", g)
	}
	for _, c := range []struct {
		name  string
		token func(g *backupFixture) string
	}{
		{"customer admin", func(g *backupFixture) string { return g.token(t, "legacyadmin", true) }},
		{"staff", func(g *backupFixture) string { return g.token(t, "staff", false) }},
		{"session token", func(g *backupFixture) string { return g.sessionToken(t) }},
	} {
		g := newBackupFixture(t)
		w := g.do(http.MethodPost, rsPath, c.token(g), body)
		if w.Code != http.StatusForbidden || w.Body.String() != gateForbiddenBody || g.svc.calls != 0 {
			t.Errorf("%s: status = %d body = %s calls = %d", c.name, w.Code, w.Body.String(), g.svc.calls)
		}
	}
	if w := f.do(http.MethodPost, rsPath, "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}

func TestRestoreRoute_DryRunDefaultAndStart(t *testing.T) {
	id := `"backupId":"22222222-2222-4222-8222-222222222222"`
	tests := []struct {
		name, body string
		status     int
		code       string
		reached    bool
	}{
		{"dryRun missing means check only", `{` + id + `}`, http.StatusOK, "", true},
		{"dryRun true", `{` + id + `,"dryRun":true}`, http.StatusOK, "", true},
		{"dryRun false starts the restore", `{` + id + `,"confirmSlug":"qa-x","dryRun":false}`, http.StatusAccepted, "", true},
		{"no backup id", `{}`, http.StatusBadRequest, "INVALID_BODY", false},
		{"malformed", `{broken`, http.StatusBadRequest, "INVALID_BODY", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBackupFixture(t)
			w := f.do(http.MethodPost, rsPath, f.token(t, "builder", true), tc.body)
			if w.Code != tc.status || (tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
			}
			if (f.svc.calls > 0) != tc.reached {
				t.Errorf("service reached = %v", f.svc.calls > 0)
			}
		})
	}
}

func TestRestoreRoute_ErrorStatusMapping(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{"TENANT_RESTORE_RETIRED", http.StatusConflict},
		{"TENANT_RESTORE_CONFLICT", http.StatusConflict},
		{"TENANT_RESTORE_CONFIRM_MISMATCH", http.StatusUnprocessableEntity},
		{"TENANT_RESTORE_SCHEMA_MISMATCH", http.StatusConflict},
		{"TENANT_RESTORE_SELF", http.StatusUnprocessableEntity},
		{"TENANT_RESTORE_TENANT_MISMATCH", http.StatusUnprocessableEntity},
		{"TENANT_RESTORE_TOO_LARGE", http.StatusUnprocessableEntity},
		{"TENANT_RESTORE_SCHEMA_DRIFT", http.StatusUnprocessableEntity},
		{"TENANT_BACKUP_CORRUPT", http.StatusUnprocessableEntity},
		{"BACKUP_NOT_FOUND", http.StatusNotFound},
		{"BACKUP_IN_PROGRESS", http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			f := newBackupFixture(t)
			f.svc.err = &tenantbackup.Error{Code: tc.code, Msg: "x"}
			w := f.do(http.MethodPost, rsPath, f.token(t, "builder", true), `{"backupId":"22222222-2222-4222-8222-222222222222"}`)
			if w.Code != tc.status {
				t.Fatalf("status = %d body = %s, want %d", w.Code, w.Body.String(), tc.status)
			}
		})
	}
}

func TestRestoreRoute_StartPassesTheTypedSlugAndTheOperator(t *testing.T) {
	f := newBackupFixture(t)
	w := f.do(http.MethodPost, rsPath, f.token(t, "builder", true),
		`{"backupId":"22222222-2222-4222-8222-222222222222","confirmSlug":"qa-x","dryRun":false}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"restoreId":"r1"`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if f.svc.gotRest.ConfirmSlug != "qa-x" || f.svc.gotRest.BackupID != "22222222-2222-4222-8222-222222222222" || f.svc.gotRest.Actor.UserID == "" {
		t.Errorf("service got %+v", f.svc.gotRest)
	}
}
