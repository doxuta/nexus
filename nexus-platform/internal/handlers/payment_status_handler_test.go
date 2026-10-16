package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// GET /tenant/payment-status — the USER-level "is payment set up?" probe (MỘT XƯỞNG §5.4).
//
// Why it exists: minting a QR has always been user-level, but the FE decided whether to SHOW the
// button by reading GET /admin/tenant/payment. That is admin-only, so every non-admin got 403,
// `bank` stayed undefined, and the button was invisible to precisely the salespeople and support
// agents whose job is collecting money — while each record view paid for a request that could
// never succeed. Found 2026-07-29 walking the customer face as a sales rep.
//
// The contract these pin: a BOOLEAN and nothing else. If this endpoint ever starts echoing the
// account number it would be a downgrade of the admin gate, since it is reachable by every
// authenticated user of the tenant.

// fakePaymentTenantStore satisfies the unexported services.paymentTenantStore structurally so the
// test drives a REAL TenantPaymentService without a database.
type fakePaymentTenantStore struct {
	payment json.RawMessage
	err     error
}

func (f *fakePaymentTenantStore) GetTenantByID(_ context.Context, id string) (*auth.Tenant, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &auth.Tenant{ID: id, Payment: f.payment}, nil
}

func (f *fakePaymentTenantStore) UpdateTenantPayment(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}

func paymentStatusResponse(t *testing.T, store *fakePaymentTenantStore) (int, string, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewPaymentHandler(services.NewTenantPaymentService(store, nil), nil)

	r := gin.New()
	grp := r.Group("/api/v1")
	// "tenantID" is the key auth.GetContextTenantID reads (auth/auth.go) — the middleware's own.
	grp.Use(func(c *gin.Context) { c.Set("tenantID", "t-1"); c.Next() })
	h.RegisterStatusRoute(grp)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/tenant/payment-status", nil))

	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, w.Body.String(), body
}

func TestPaymentStatus_ConfiguredTenant(t *testing.T) {
	code, raw, body := paymentStatusResponse(t, &fakePaymentTenantStore{
		payment: json.RawMessage(`{"bankBin":"970415","accountNumber":"113000000123","accountName":"CONG TY Nexus"}`),
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", code, raw)
	}
	data, _ := body["data"].(map[string]any)
	if data["configured"] != true {
		t.Fatalf("configured = %v; want true (body %s)", data["configured"], raw)
	}
	// The whole point of a separate endpoint: the details stay behind the admin gate.
	for _, secret := range []string{"113000000123", "CONG TY Nexus", "970415", "accountNumber"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("response leaked %q — this route is reachable by EVERY authenticated user: %s", secret, raw)
		}
	}
}

func TestPaymentStatus_UnconfiguredTenant(t *testing.T) {
	for name, store := range map[string]*fakePaymentTenantStore{
		"no payment column":   {payment: nil},
		"empty object":        {payment: json.RawMessage(`{}`)},
		"malformed json":      {payment: json.RawMessage(`{oops`)},
		"details but no bank": {payment: json.RawMessage(`{"accountName":"X"}`)},
		"store read failed":   {err: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			code, raw, body := paymentStatusResponse(t, store)
			// Even a read failure answers 200/false: hiding an optional button must never 500 a
			// record screen.
			if code != http.StatusOK {
				t.Fatalf("status = %d; want 200 (body %s)", code, raw)
			}
			data, _ := body["data"].(map[string]any)
			if data["configured"] != false {
				t.Fatalf("configured = %v; want false (body %s)", data["configured"], raw)
			}
		})
	}
}
