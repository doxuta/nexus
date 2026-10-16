package handlers

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// TestWriteMFAError_CodeInvalid_LogsWarnWithoutSecret is the reviewer LOW-1 regression test: a
// wrong/replayed/reused MFA code (services.ErrMFACodeInvalid) must be logged at Warn level with
// tenant_id/user_id/op context — useful signal for brute-force detection that was previously
// silently discarded — and must NEVER log the code/token/secret itself.
func TestWriteMFAError_CodeInvalid_LogsWarnWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.WarnLevel)
	h := NewAuthHandler(nil, zap.New(core))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.writeMFAError(c, "MFADisable", "t-1", "u-1", services.ErrMFACodeInvalid)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want %d", w.Code, http.StatusUnauthorized)
	}
	assertSingleWarnEntry(t, logs, "t-1", "u-1")
}

// TestWriteMFAError_PendingInvalid_LogsWarn extends the same coverage to
// auth.ErrMFAPendingInvalid (a forged/expired/reused pendingToken) — a reasonable companion
// signal since it can indicate probing of the two-step-login bridge token, not just typos.
func TestWriteMFAError_PendingInvalid_LogsWarn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.WarnLevel)
	h := NewAuthHandler(nil, zap.New(core))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// MFAVerify runs pre-auth, so tenantID/userID are genuinely empty at this call site
	// (mirrors the real h.writeMFAError(c, "MFAVerify", "", "", err) call in MFAVerify).
	h.writeMFAError(c, "MFAVerify", "", "", auth.ErrMFAPendingInvalid)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want %d", w.Code, http.StatusUnauthorized)
	}
	assertSingleWarnEntry(t, logs, "", "")
}

// TestWriteMFAError_TooManyAttempts_429WithRetryAfter proves the reviewer MED-1 / security
// review 🟡#4 throttle's HTTP mapping: services.ErrMFATooManyAttempts becomes 429
// MFA_TOO_MANY_ATTEMPTS with a Retry-After header set to the lockout window, and — like the
// other MFA error branches — is logged at Warn (never leaking the code/token).
func TestWriteMFAError_TooManyAttempts_429WithRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.WarnLevel)
	h := NewAuthHandler(nil, zap.New(core))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.writeMFAError(c, "MFADisable", "t-1", "u-1", services.ErrMFATooManyAttempts)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want %d", w.Code, http.StatusTooManyRequests)
	}
	if !strings.Contains(w.Body.String(), "MFA_TOO_MANY_ATTEMPTS") {
		t.Errorf("body missing MFA_TOO_MANY_ATTEMPTS code: %s", w.Body.String())
	}
	wantRetry := strconv.Itoa(int(services.MFAAttemptLockoutWindow.Seconds()))
	if got := w.Header().Get("Retry-After"); got != wantRetry {
		t.Errorf("Retry-After = %q; want %q", got, wantRetry)
	}
	assertSingleWarnEntry(t, logs, "t-1", "u-1")
}

// assertSingleWarnEntry asserts exactly one Warn-level entry was recorded, carrying the
// expected tenant_id/user_id fields and NONE of the secret-bearing field names.
func assertSingleWarnEntry(t *testing.T, logs *observer.ObservedLogs, wantTenant, wantUser string) {
	t.Helper()
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d; want 1 (entries=%+v)", len(entries), entries)
	}
	entry := entries[0]
	if entry.Level != zapcore.WarnLevel {
		t.Fatalf("log level = %v; want Warn", entry.Level)
	}
	fields := entry.ContextMap()
	if fields["tenant_id"] != wantTenant {
		t.Errorf("tenant_id field = %v; want %q", fields["tenant_id"], wantTenant)
	}
	if fields["user_id"] != wantUser {
		t.Errorf("user_id field = %v; want %q", fields["user_id"], wantUser)
	}
	for _, key := range []string{"code", "token", "secret", "pendingToken"} {
		if _, ok := fields[key]; ok {
			t.Errorf("log fields must never carry %q, got: %+v", key, fields)
		}
	}
}
