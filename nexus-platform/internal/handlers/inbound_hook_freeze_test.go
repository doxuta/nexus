package handlers

// specs/068-tenant-backup-restore T038 (handler half): a correctly signed signal for a write-frozen
// tenant answers 503 TENANT_MAINTENANCE + Retry-After; a wrong signature on the same frozen tenant
// stays the uniform 401 (the freeze is never revealed to an unsigned prober).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/services"
)

type alwaysFrozen struct{ until time.Time }

func (a alwaysFrozen) FrozenUntil(context.Context, string) (time.Time, bool, error) {
	return a.until, true, nil
}

func TestInboundHookReceive_FrozenTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hook := &inboundhook.Hook{ID: "h1", TenantID: "t1", Name: "feed", EntityName: "contacts", Secret: hookTestSecret,
		IsActive: true, Mapping: []inboundhook.Mapping{{Source: "name", Field: "name"}}}
	svc := services.NewInboundHookService(&hookStoreFake{hook: hook}, nil, slugFake{}, zap.NewNop()).
		WithTenantFreeze(alwaysFrozen{until: time.Now().Add(2 * time.Minute)})
	r := gin.New()
	NewInboundHookHandler(svc, zap.NewNop()).RegisterPublicRoute(r.Group("/api/v1"))

	body := []byte(`{"name":"Acme"}`)
	mac := hmac.New(sha256.New, []byte(hookTestSecret))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	for _, tc := range []struct {
		name, sig string
		want      int
		wantCode  string
	}{
		{"signed", good, http.StatusServiceUnavailable, "TENANT_MAINTENANCE"},
		{"bad signature", "deadbeef", http.StatusUnauthorized, "HOOK_UNAUTHORIZED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/acme/feed", bytes.NewReader(body))
			req.Header.Set("X-Nexus-Signature", tc.sig)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"`+tc.wantCode+`"`)) {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.want, tc.wantCode)
			}
			if tc.want == http.StatusServiceUnavailable {
				if s, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || s < 1 {
					t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
				}
			}
		})
	}
}
