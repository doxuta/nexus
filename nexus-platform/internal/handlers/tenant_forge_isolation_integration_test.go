//go:build integration

// Forge-tenant isolation suite (../specs/001-auth-security/spec.md §5 "Cô lập tenant", §6 Testing).
//
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// The A2 acceptance test (a2_auth_chain_acceptance_integration_test.go) proves the headline
// claim-only path end to end through a real login. This file hardens the SECURITY-CRITICAL
// edges that the acceptance test does not exercise, so a regression in tenant scoping fails a
// test instead of leaking data:
//
//   - WRITE isolation across the FULL CRUD surface: tenant B cannot update, delete, or read
//     tenant A's record by id (every mutating route is scoped by the claim's tenant_id, so A's
//     row is invisible — 404 — to B). The A2 test only covered list + get.
//   - SPOOF on MUTATING routes: an X-Tenant-Id header and ?tenantId query claiming tenant A,
//     presented with tenant B's token on PUT/DELETE, have NO effect (the verified claim wins, the
//     spoof never crosses the boundary); and a body-injected tenant_id is rejected outright as an
//     unknown field — there is no client-writable tenant channel at all. (A2 spoofed read routes.)
//   - INVALID / FORGED token through the REAL verifier: a Bearer token signed with a DIFFERENT
//     secret (the classic forgery) and a garbage token are both rejected with 401 by the
//     production AuthMiddleware → RequireTenant chain — the integration-level counterpart to the
//     unit tests that use a fake verifier.
//   - NO tenant claim through the REAL chain: a validly-signed access token whose claim carries
//     an EMPTY tenant id is rejected 401 by RequireTenant before any tenant-scoped query runs.
//
// These tests use the synthetic-token harness (newFullRouter + setAuth, which mint verified
// Bearer tokens with the shared accessVerifier secret) so isolation can be asserted across many
// CRUD calls cheaply; the tenant a handler sees is ALWAYS the one inside the signed token.
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"nexus/platform/internal/auth"
)

// newJSONRequest builds a JSON httptest request with the Content-Type set. A nil body sends an
// empty payload. It centralizes the body-marshal boilerplate the forge helpers share.
func newJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	return req
}

const forgeEntity = "it_forge_secrets"

// seedForgeEntityForBoth registers forgeEntity for BOTH tenant A and tenant B (a shared obj_*
// table separated by tenant_id) and inserts one private record owned by tenant A, returning its
// id. It fails the test on any setup error so the isolation assertions stand on solid fixtures.
func seedForgeEntityForBoth(t *testing.T, r *gin.Engine) (idA string) {
	t.Helper()
	entityBody := map[string]any{
		"name":  forgeEntity,
		"label": "Bí mật",
		"fields": []map[string]any{
			{"name": "secret", "label": "Nội dung", "fieldType": "text", "isRequired": true},
		},
	}
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", tenantA, entityBody); w.Code != http.StatusCreated {
		t.Fatalf("seed entity for A: %d %s", w.Code, w.Body.String())
	}
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", tenantB, entityBody); w.Code != http.StatusCreated {
		t.Fatalf("seed entity for B: %d %s", w.Code, w.Body.String())
	}
	w := doAs(t, r, http.MethodPost, "/api/v1/"+forgeEntity, tenantA, map[string]any{"secret": "A-only"})
	if w.Code != http.StatusCreated {
		t.Fatalf("A insert: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Data.ID == "" {
		t.Fatal("A insert returned empty id")
	}
	return created.Data.ID
}

// listTotal does a tenant-scoped list and returns pagination.total, failing on a non-200.
func listTotal(t *testing.T, r *gin.Engine, tenant string) int {
	t.Helper()
	w := doAs(t, r, http.MethodGet, "/api/v1/"+forgeEntity, tenant, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list as %s: %d %s", tenant, w.Code, w.Body.String())
	}
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	return list.Pagination.Total
}

// TestForgeTenant_WriteIsolationAcrossCRUD proves tenant B — using its OWN verified token — cannot
// mutate or read tenant A's record through ANY CRUD route. A's row is scoped out by the claim's
// tenant_id, so to B it simply does not exist: read/update/delete by A's id all 404, and B's own
// writes never touch A's partition. The acceptance test (§9.1) is the read-only complement; this
// closes the write half of "token B không đọc/ghi được data A" (spec §5).
func TestForgeTenant_WriteIsolationAcrossCRUD(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupAllNamed(sx, forgeEntity)
	t.Cleanup(func() { cleanupAllNamed(sx, forgeEntity) })

	idA := seedForgeEntityForBoth(t, r)

	// --- B cannot READ A's record by id -> 404 (invisible, not 403: existence is hidden). ----
	if w := doAs(t, r, http.MethodGet, "/api/v1/"+forgeEntity+"/"+idA, tenantB, nil); w.Code != http.StatusNotFound {
		t.Errorf("B GET A's record: %d; want 404 (isolation breached)", w.Code)
	}

	// --- B cannot UPDATE A's record by id -> 404, and A's data is unchanged. ------------------
	if w := doAs(t, r, http.MethodPut, "/api/v1/"+forgeEntity+"/"+idA, tenantB,
		map[string]any{"secret": "hacked-by-B"}); w.Code != http.StatusNotFound {
		t.Errorf("B PUT A's record: %d; want 404 (cross-tenant write must not resolve the row)", w.Code)
	}
	// Confirm A still reads its ORIGINAL value (the forged update did not land). The record
	// payload is nested under data.data (the envelope is {data:{id,tenantId,data:{...fields},...}}).
	wA := doAs(t, r, http.MethodGet, "/api/v1/"+forgeEntity+"/"+idA, tenantA, nil)
	if wA.Code != http.StatusOK {
		t.Fatalf("A GET own record after B's update attempt: %d %s", wA.Code, wA.Body.String())
	}
	var got struct {
		Data struct {
			Fields map[string]any `json:"data"`
		} `json:"data"`
	}
	json.Unmarshal(wA.Body.Bytes(), &got)
	if got.Data.Fields["secret"] != "A-only" {
		t.Errorf("A's record was mutated across tenants: secret=%v; want \"A-only\"", got.Data.Fields["secret"])
	}

	// --- B cannot DELETE A's record by id -> 404, and A's record survives. --------------------
	if w := doAs(t, r, http.MethodDelete, "/api/v1/"+forgeEntity+"/"+idA, tenantB, nil); w.Code != http.StatusNotFound {
		t.Errorf("B DELETE A's record: %d; want 404 (cross-tenant delete must not resolve the row)", w.Code)
	}
	if total := listTotal(t, r, tenantA); total != 1 {
		t.Errorf("after B's delete attempt, A sees %d records; want 1 (A's row must survive)", total)
	}

	// --- B's OWN create lands ONLY in B's partition; A's count is untouched. -------------------
	if w := doAs(t, r, http.MethodPost, "/api/v1/"+forgeEntity, tenantB,
		map[string]any{"secret": "B-only"}); w.Code != http.StatusCreated {
		t.Fatalf("B insert own record: %d %s", w.Code, w.Body.String())
	}
	if total := listTotal(t, r, tenantB); total != 1 {
		t.Errorf("B sees %d records; want 1 (only its own)", total)
	}
	if total := listTotal(t, r, tenantA); total != 1 {
		t.Errorf("A sees %d records after B inserted into the shared table; want 1 (B's row must not leak to A)", total)
	}
}

// TestForgeTenant_SpoofHeaderQueryBodyOnMutatingRoutes proves a client-supplied tenant — via the
// X-Tenant-Id header, ?tenantId query, AND a tenant_id body field, all naming tenant A — is IGNORED
// on the MUTATING routes when the verified token belongs to tenant B. The tenant comes only from
// the claim, so every spoofed mutation resolves against tenant B (where A's id does not exist) and
// 404s without touching A. This is the write-route counterpart to the A2 read-route spoof checks.
func TestForgeTenant_SpoofHeaderQueryBodyOnMutatingRoutes(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupAllNamed(sx, forgeEntity)
	t.Cleanup(func() { cleanupAllNamed(sx, forgeEntity) })

	idA := seedForgeEntityForBoth(t, r)
	spoof := map[string]string{"X-Tenant-Id": tenantA} // a forged "I am tenant A" header

	// PUT A's id as B, spoofing tenant A via header + query (NO body tenant field) -> 404: the
	// claim (B) scopes the row lookup, A's id does not exist for B, header/query are ignored.
	if w := doAsWithSpoof(t, r, http.MethodPut,
		"/api/v1/"+forgeEntity+"/"+idA+"?tenantId="+tenantA, tenantB,
		map[string]any{"secret": "spoofed"}, spoof); w.Code != http.StatusNotFound {
		t.Errorf("spoofed PUT (header+query=A) as B: %d; want 404 (spoof must not cross tenant)", w.Code)
	}

	// DELETE A's id as B, spoofing tenant A via header + query -> 404 (claim B wins).
	if w := doAsWithSpoof(t, r, http.MethodDelete,
		"/api/v1/"+forgeEntity+"/"+idA+"?tenantId="+tenantA, tenantB, nil, spoof); w.Code != http.StatusNotFound {
		t.Errorf("spoofed DELETE (header+query=A) as B: %d; want 404 (spoof must not cross tenant)", w.Code)
	}

	// BODY-INJECTED tenant_id: there is no client-writable tenant channel at all. Injecting a
	// `tenant_id` field into the create body is rejected as an UNKNOWN FIELD (422) — the engine
	// has no concept of a client-supplied tenant, so the spoof cannot even be expressed, let alone
	// honored. (This proves the absence of a body path; isolation never depends on the value.)
	if w := doAs(t, r, http.MethodPost, "/api/v1/"+forgeEntity, tenantB,
		map[string]any{"secret": "x", "tenant_id": tenantA}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("body-injected tenant_id on create as B: %d; want 422 (no client-writable tenant field)", w.Code)
	}

	// A, using ONLY its claim (no spoof), still sees its untouched record.
	if total := listTotal(t, r, tenantA); total != 1 {
		t.Errorf("after spoofed mutations, A sees %d records; want 1 (A's row must be intact)", total)
	}
}

// TestForgeTenant_InvalidTokenRejectedThroughRealChain proves the PRODUCTION verifier rejects a
// FORGED Bearer token. It stands up the real AuthMiddleware → RequireTenant chain (with the genuine
// accessVerifier secret) and presents (1) a token signed by a DIFFERENT secret — the canonical
// forgery — and (2) a structurally-garbage token. Both must 401 before any handler runs, so a
// forged credential can never be used to "self-declare" a tenant. This is the integration-level
// proof complementing the unit tests that drive a fake verifier.
func TestForgeTenant_InvalidTokenRejectedThroughRealChain(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupAllNamed(sx, forgeEntity)
	t.Cleanup(func() { cleanupAllNamed(sx, forgeEntity) })

	// A token signed with the WRONG secret: structurally a valid JWT, but the production verifier
	// (built from accessVerifier's secret) must reject the signature.
	forgerSvc, err := auth.NewTokenService("a-totally-different-attacker-secret-9999", 15*time.Minute, 168*time.Hour)
	if err != nil {
		t.Fatalf("build forger token service: %v", err)
	}
	forgedToken, err := forgerSvc.IssueAccessToken(auth.Subject{
		UserID:   uuid.NewString(),
		TenantID: tenantA, // the attacker tries to claim tenant A
		IsAdmin:  true,
	})
	if err != nil {
		t.Fatalf("issue forged token: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"wrong-secret signature (forged)", forgedToken},
		{"garbage token", "not.a.valid.jwt.at.all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Protected read route.
			if w := doWithBearer(t, r, http.MethodGet, "/api/v1/"+forgeEntity, tc.token, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("forged token on GET: %d; want 401 (real verifier must reject)", w.Code)
			}
			// Protected admin (write) route — same rejection, before any handler runs.
			if w := doWithBearer(t, r, http.MethodPost, "/api/v1/admin/entities", tc.token,
				map[string]any{"name": forgeEntity, "label": "x"}); w.Code != http.StatusUnauthorized {
				t.Errorf("forged token on admin POST: %d; want 401", w.Code)
			}
		})
	}
}

// TestForgeTenant_EmptyTenantClaimRejectedThroughRealChain proves RequireTenant fails closed at the
// integration level: a token that is correctly signed by the production secret but whose claim
// carries an EMPTY tenant id is rejected 401 before any tenant-scoped query runs. Without this
// guard a malformed/legacy token could reach a repository with a blank tenant_id filter.
func TestForgeTenant_EmptyTenantClaimRejectedThroughRealChain(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupAllNamed(sx, forgeEntity)
	t.Cleanup(func() { cleanupAllNamed(sx, forgeEntity) })

	// Validly-signed (accessVerifier secret) but tenant-less access token.
	tokenlessTenant, err := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   uuid.NewString(),
		TenantID: "", // no tenant claim
		IsAdmin:  true,
	})
	if err != nil {
		t.Fatalf("issue tenant-less token: %v", err)
	}

	if w := doWithBearer(t, r, http.MethodGet, "/api/v1/"+forgeEntity, tokenlessTenant, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("tenant-less but validly-signed token: %d; want 401 (RequireTenant must fail closed)", w.Code)
	}
}

// ---- request helpers specific to the forge suite ----------------------------

// doAsWithSpoof issues a request authenticated as `tenant` (verified Bearer claim) while ALSO
// attaching attacker-controlled spoof headers — used to prove those headers are ignored in favor
// of the claim. An empty tenant sends no Authorization header.
func doAsWithSpoof(t *testing.T, r *gin.Engine, method, path, tenant string, body any, spoofHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := newJSONRequest(t, method, path, body)
	setAuth(t, req, tenant)
	for k, v := range spoofHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doWithBearer issues a request carrying a RAW Bearer token verbatim (no minting) — used to drive
// forged/garbage tokens through the real verifier. An empty token sends no Authorization header.
func doWithBearer(t *testing.T, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := newJSONRequest(t, method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
