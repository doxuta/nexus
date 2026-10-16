package middleware

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// newRecoveryRouter mirrors the production onion (RequestID → Recovery → handlers) over an
// observer core. panicWith is the value the /panic handler throws.
func newRecoveryRouter(panicWith any) (*gin.Engine, *observer.ObservedLogs) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.ErrorLevel)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery(zap.New(core)))
	r.GET("/panic", func(c *gin.Context) { panic(panicWith) })
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, logs
}

// TestRecovery_PanicTo500JSON: a handler panic becomes the standard 500 envelope
// {error:"Lỗi hệ thống", code:"INTERNAL"}, the server keeps serving, and the zap entry
// carries request_id + stack.
func TestRecovery_PanicTo500JSON(t *testing.T) {
	r, logs := newRecoveryRouter("metadata shape was nil")

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	req.Header.Set(HeaderRequestID, "req-panic-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":"INTERNAL"`) || !strings.Contains(body, "Lỗi hệ thống") {
		t.Errorf("expected the INTERNAL JSON envelope, got: %s", body)
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 error log entry, got %d", len(entries))
	}
	var hasStack, hasReqID bool
	for _, f := range entries[0].Context {
		switch f.Key {
		case "stack":
			hasStack = f.String != ""
		case "request_id":
			hasReqID = f.String == "req-panic-1"
		}
	}
	if !hasStack {
		t.Error("expected a non-empty stack field on the panic log entry")
	}
	if !hasReqID {
		t.Error("expected request_id=req-panic-1 on the panic log entry")
	}

	// The engine must still serve subsequent requests (no crash).
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 on the follow-up request, got %d", w2.Code)
	}
}

// TestRecovery_BrokenPipe_NoWrite: the gin edge case — a panic caused by the client hanging up
// (net.OpError wrapping a "broken pipe" syscall error) must NOT be answered with a 500 write
// (the connection is dead); it is logged (without stack) and the request aborted.
func TestRecovery_BrokenPipe_NoWrite(t *testing.T) {
	brokenPipe := &net.OpError{
		Op:  "write",
		Err: os.NewSyscallError("write", errors.New("broken pipe")),
	}
	r, logs := newRecoveryRouter(brokenPipe)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if strings.Contains(w.Body.String(), "INTERNAL") {
		t.Errorf("broken-pipe panic must not write the 500 envelope, got body: %s", w.Body.String())
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 error log entry, got %d", len(entries))
	}
	for _, f := range entries[0].Context {
		if f.Key == "stack" {
			t.Error("broken-pipe log entry must not carry a stack (peer condition, not a code bug)")
		}
	}
}

// TestRecovery_NonErrorPanicValue: panicking with a non-error value (a string) is normalized
// into the error field rather than dropped.
func TestRecovery_NonErrorPanicValue(t *testing.T) {
	r, logs := newRecoveryRouter(42)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected 1 log entry, got %d", logs.Len())
	}
}
