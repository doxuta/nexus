package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newLimitRouter builds a gin engine with MaxBodySize(limit) mounted globally and a
// POST /test handler that reads the body via ShouldBindJSON and echoes 200 on success.
func newLimitRouter(limit int64) *gin.Engine {
	r := gin.New()
	r.Use(MaxBodySize(limit))
	r.POST("/test", func(c *gin.Context) {
		var payload map[string]any
		if err := c.ShouldBindJSON(&payload); err != nil {
			if IsMaxBytesError(err) {
				RespondTooLarge(c)
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestMaxBodySize_UnderLimit_Passes(t *testing.T) {
	r := newLimitRouter(1024) // 1 KiB
	body := `{"name":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for under-limit body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMaxBodySize_OverLimit_Returns413(t *testing.T) {
	r := newLimitRouter(10) // 10 bytes — anything JSON-meaningful will exceed this
	body := `{"name":"hello world this is a long string"}`
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for over-limit body, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("REQUEST_TOO_LARGE")) {
		t.Errorf("expected code=REQUEST_TOO_LARGE in body, got: %s", w.Body.String())
	}
}

func TestMaxBodySize_ContentLengthOver_Returns413Fast(t *testing.T) {
	// Content-Length declared over limit should be rejected before reading any bytes.
	r := newLimitRouter(100)
	body := strings.Repeat("x", 200)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 200 // explicitly set to trigger fast-path
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for Content-Length over limit, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMaxBodySize_SlowPath_Returns413 verifies the slow path: when a client omits (or
// lies about) Content-Length the fast-path check is skipped, but http.MaxBytesReader
// still triggers *http.MaxBytesError on the first read past the limit. The handler's
// IsMaxBytesError check must catch this and return 413.
//
// This is the path that defends against chunked transfer-encoding or a malicious client
// that sets Content-Length: 1 but streams megabytes.
func TestMaxBodySize_SlowPath_Returns413(t *testing.T) {
	r := newLimitRouter(20) // 20 bytes — body below will exceed this
	body := `{"name":"this body is definitely longer than twenty bytes"}`
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // Force slow-path: no Content-Length declared
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("slow-path: expected 413 when body exceeds limit without Content-Length, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("REQUEST_TOO_LARGE")) {
		t.Errorf("slow-path: expected code=REQUEST_TOO_LARGE in body, got: %s", w.Body.String())
	}
}

func TestMaxBodySize_ZeroLimit_NoOp(t *testing.T) {
	// A zero limit must be a no-op (middleware disabled).
	r := newLimitRouter(0)
	body := strings.Repeat(`{"x":1}`, 1000) // ~7 KiB, would exceed any non-zero small limit
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Should NOT return 413 (no limit applied); handler may return 200 or 400 depending on body validity.
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("expected no 413 when limit=0 (disabled), got 413")
	}
}
