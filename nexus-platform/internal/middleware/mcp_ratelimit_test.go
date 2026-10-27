package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Issue #22 item 2: /mcp had no brake. The handler below stands in for mcpserver: it "looks up" the
// token (lookups counts what would be a SELECT) and answers 200 for a live key, 401 otherwise.
type mcpFake struct {
	live    map[string]bool
	lookups int
}

func (f *mcpFake) router(cfg MCPRateLimitConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/mcp", MCPRateLimit(cfg), func(c *gin.Context) {
		tok, _ := extractBearer(c.GetHeader("Authorization"))
		if !strings.HasPrefix(tok, "nxm_") {
			c.Status(http.StatusUnauthorized) // refused without a lookup, as mcpserver does
			return
		}
		f.lookups++
		if f.live[tok] {
			c.Status(http.StatusOK)
			return
		}
		c.Status(http.StatusUnauthorized)
	})
	return r
}

func mcpCall(r *gin.Engine, ip, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.RemoteAddr = ip + ":5000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMCPRateLimit_GuessesSpendTheIPsBudgetAndAWorkingKeyKeepsWorking(t *testing.T) {
	f := &mcpFake{live: map[string]bool{"nxm_live": true}}
	r := f.router(MCPRateLimitConfig{ProbeMax: 3, KeyMax: 100, Window: time.Minute})
	const office, elsewhere = "203.0.113.7", "198.51.100.2"

	if w := mcpCall(r, office, "nxm_live"); w.Code != http.StatusOK { // first use: charged to the IP (1 of 3)
		t.Fatalf("a live key's first call: %d", w.Code)
	}
	for i, want := range []int{401, 401, 429, 429} { // the prober spends the other 2, then is refused
		if w := mcpCall(r, office, "nxm_guess"+string(rune('a'+i))); w.Code != want {
			t.Fatalf("guess %d: %d; want %d", i+1, w.Code, want)
		}
	}
	if f.lookups != 3 {
		t.Fatalf("lookups = %d; want 3 (a refused guess costs no lookup)", f.lookups)
	}
	w := mcpCall(r, office, "nxm_guessz")
	if w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Fatalf("refusal = %v %s; want Retry-After and the canonical RATE_LIMITED body", w.Header(), w.Body.String())
	}
	// The working key behind the same address is not the prober: it spends its own budget.
	if w := mcpCall(r, office, "nxm_live"); w.Code != http.StatusOK {
		t.Fatalf("the working key after the IP's budget was spent: %d; want 200", w.Code)
	}
	// Another address has its own budget.
	if w := mcpCall(r, elsewhere, "nxm_guess"); w.Code != http.StatusUnauthorized {
		t.Fatalf("a guess from another address: %d; want 401", w.Code)
	}
	// No nxm_ bearer is never braked: the handler refuses it without a lookup.
	for range 5 {
		if w := mcpCall(r, office, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("no credential: %d; want 401", w.Code)
		}
	}
}

func TestMCPRateLimit_AWorkingKeyHasItsOwnBudget(t *testing.T) {
	f := &mcpFake{live: map[string]bool{"nxm_live": true, "nxm_other": true}}
	r := f.router(MCPRateLimitConfig{ProbeMax: 100, KeyMax: 2, Window: time.Minute})
	const ip = "203.0.113.7"
	for i, want := range []int{200, 200, 200, 429} { // first call on the IP, then 2 on the key
		if w := mcpCall(r, ip, "nxm_live"); w.Code != want {
			t.Fatalf("call %d: %d; want %d", i+1, w.Code, want)
		}
	}
	if w := mcpCall(r, ip, "nxm_other"); w.Code != http.StatusOK {
		t.Fatalf("another key: %d; want 200 (budgets are per key)", w.Code)
	}
}

func TestMCPRateLimit_ARevokedKeyGoesBackToTheIPsBudget(t *testing.T) {
	f := &mcpFake{live: map[string]bool{"nxm_live": true}}
	r := f.router(MCPRateLimitConfig{ProbeMax: 2, KeyMax: 100, Window: time.Minute})
	const ip = "203.0.113.7"
	mcpCall(r, ip, "nxm_live")                  // IP budget 1 of 2
	f.live["nxm_live"] = false                  // revoked
	for i, want := range []int{401, 401, 429} { // known: key budget; then forgotten: the IP's last lookup; then braked
		if w := mcpCall(r, ip, "nxm_live"); w.Code != want {
			t.Fatalf("call %d after revoke: %d; want %d", i+1, w.Code, want)
		}
	}
}

func TestMCPRateLimit_OffWhenBudgetsAreZero(t *testing.T) {
	f := &mcpFake{}
	r := f.router(MCPRateLimitConfig{Window: time.Minute})
	for range 10 {
		if w := mcpCall(r, "203.0.113.7", "nxm_guess"); w.Code != http.StatusUnauthorized {
			t.Fatalf("no budget configured: %d; want 401 (no brake)", w.Code)
		}
	}
}

// Behind Caddy every request comes from Caddy's address. Without TRUSTED_PROXY_CIDRS one prober spends every
// client's lookup budget; with the compose network trusted (docker-compose.yml), each client has its own.
func TestMCPRateLimit_BehindTheProxyEachClientHasItsOwnBudget(t *testing.T) {
	_, compose, _ := net.ParseCIDR("10.213.47.0/24")
	viaCaddy := func(r *gin.Engine, client string) int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.RemoteAddr = "10.213.47.2:5000" // Caddy on the compose network
		req.Header.Set("X-Forwarded-For", client)
		req.Header.Set("Authorization", "Bearer nxm_guess")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, tc := range []struct {
		name    string
		trusted []*net.IPNet
		want    int
	}{
		{"proxy not trusted: one bucket for everyone", nil, http.StatusTooManyRequests},
		{"compose network trusted: a bucket per client", []*net.IPNet{compose}, http.StatusUnauthorized},
	} {
		r := (&mcpFake{}).router(MCPRateLimitConfig{ProbeMax: 2, KeyMax: 100, Window: time.Minute, TrustedProxyCIDRs: tc.trusted})
		viaCaddy(r, "198.51.100.1")
		viaCaddy(r, "198.51.100.1") // the prober spends its 2
		if got := viaCaddy(r, "203.0.113.50"); got != tc.want {
			t.Errorf("%s: another client's guess = %d; want %d", tc.name, got, tc.want)
		}
	}
}
