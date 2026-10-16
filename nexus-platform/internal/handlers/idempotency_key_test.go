package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestIdempotencyKeyOf pins the header grammar of specs/047 FR-2: absent → no key; 1..255 visible
// ASCII → the key; anything else → an error (400 INVALID_IDEMPOTENCY_KEY in Create).
func TestIdempotencyKeyOf(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		wantKey string
		wantHas bool
		wantErr bool
	}{
		{name: "absent", values: nil},
		{name: "uuid", values: []string{"2f1c0b7e-7a55-4b39-9c3e-0d6a1f0e9a11"}, wantKey: "2f1c0b7e-7a55-4b39-9c3e-0d6a1f0e9a11", wantHas: true},
		{name: "surrounding spaces trimmed", values: []string{"  abc-1 "}, wantKey: "abc-1", wantHas: true},
		{name: "255 chars", values: []string{strings.Repeat("k", 255)}, wantKey: strings.Repeat("k", 255), wantHas: true},
		{name: "punctuation is visible ASCII", values: []string{"order:1001/v2~#"}, wantKey: "order:1001/v2~#", wantHas: true},
		{name: "empty", values: []string{""}, wantErr: true},
		{name: "blank", values: []string{"   "}, wantErr: true},
		{name: "256 chars", values: []string{strings.Repeat("k", 256)}, wantErr: true},
		{name: "inner space", values: []string{"abc 1"}, wantErr: true},
		{name: "tab", values: []string{"abc\t1"}, wantErr: true},
		{name: "non-ASCII", values: []string{"khóa-1"}, wantErr: true},
		{name: "sent twice", values: []string{"a", "b"}, wantErr: true},
	}
	gin.SetMode(gin.TestMode)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/leads", nil)
			for _, v := range tt.values {
				c.Request.Header.Add("Idempotency-Key", v)
			}
			key, has, err := idempotencyKeyOf(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if key != tt.wantKey || has != tt.wantHas {
				t.Fatalf("= (%q, %v), want (%q, %v)", key, has, tt.wantKey, tt.wantHas)
			}
		})
	}
}
