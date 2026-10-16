//go:build integration

// DAILY-QUEUE #52 integration (specs/045-api-key-throttle) — the per-API-key data-plane throttle
// on real MySQL through the REAL composite verifier (JWT + `nxk_`) and production middleware order:
//
//	a key bound to a NON-admin role gets 200 on requests 1..max of GET /:entity and 429
//	RATE_LIMITED + Retry-After on max+1 (no counter header) → a non-admin STAFF browser session in
//	the SAME tenant keeps loading the same grid far past the cap → once the window slides the key
//	is admitted again.
//
// Run:  go test -tags=integration ./internal/handlers/ -run TestAPIKey_DataRateLimit
package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

func TestAPIKey_DataRateLimit_KeyThrottledBrowserSessionUntouched(t *testing.T) {
	const (
		max    = 5
		window = 2 * time.Second
		entity = "apikey_rl_items"
		tenant = "ten-keyrl-aaaa"
		slug   = "keyrl-ten"
	)
	gdb, sx := a4SetupMigratedDB(t)
	r := newAPIKeyITRouter(t, gdb, sx,
		WithDataRateLimiter(middleware.DataRateLimit(middleware.DataRateLimitConfig{MaxAttempts: max, Window: window})))

	a4SeedTenant(t, gdb, tenant, slug, "Key RL Tenant")
	sales := a4SeedRole(t, gdb, tenant, "sales", []auth.RoleAction{grant(entity, "view"), grant(entity, "create")})
	a4SeedUser(t, gdb, tenant, "admin@keyrl.test", true, nil)
	a4SeedUser(t, gdb, tenant, "staff@keyrl.test", false, &sales)
	t.Cleanup(func() { a4DropEntity(sx, []string{tenant}, entity) })

	adminTok := itLogin(t, r, slug, "admin@keyrl.test", authPassword).Data.AccessToken
	staffTok := itLogin(t, r, slug, "staff@keyrl.test", authPassword).Data.AccessToken
	itCreateEntity(t, r, adminTok, entity)

	wIssue := doAuthed(t, r, http.MethodPost, "/api/v1/admin/api-keys", adminTok,
		map[string]any{"name": "retrying integration", "roleId": sales}, nil)
	if wIssue.Code != http.StatusCreated {
		t.Fatalf("issue key: %d %s", wIssue.Code, wIssue.Body.String())
	}
	var issued struct {
		Data struct {
			Secret string `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wIssue.Body.Bytes(), &issued); err != nil || issued.Data.Secret == "" {
		t.Fatalf("decode issued secret: %v (body=%s)", err, wIssue.Body.String())
	}
	key := issued.Data.Secret

	// ---- the key: 1..max admitted, max+1 refused --------------------------------------------
	for i := 1; i <= max; i++ {
		w := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, key, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("key request %d: %d %s, want 200", i, w.Code, w.Body.String())
		}
	}
	w := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, key, nil, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("key request %d: %d %s, want 429", max+1, w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Retry-After"), strconv.Itoa(int(window.Seconds())); got != want {
		t.Errorf("Retry-After = %q, want %q", got, want)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Code != "RATE_LIMITED" {
		t.Errorf("429 body = %s, want code RATE_LIMITED", w.Body.String())
	}
	for h := range w.Header() {
		if strings.HasPrefix(strings.ToLower(h), "x-ratelimit") {
			t.Errorf("429 carries counter header %q — contract is 429 + Retry-After only", h)
		}
	}

	// ---- a non-admin staff browser session, SAME tenant, SAME grid: never counted -----------
	for i := 1; i <= 4*max; i++ {
		ws := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, staffTok, nil, nil)
		if ws.Code != http.StatusOK {
			t.Fatalf("staff session request %d while the key is throttled: %d %s, want 200", i, ws.Code, ws.Body.String())
		}
	}

	// ---- the window slides → the key runs again ---------------------------------------------
	time.Sleep(window + 200*time.Millisecond)
	if w := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, key, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("key after the window slid: %d %s, want 200", w.Code, w.Body.String())
	}
}
