// Package middleware — request_id.go implements the request-correlation middleware (RM-35,
// OBS-1). Every request gets a request id: an incoming X-Request-ID header is honored when it
// is well-formed (so a gateway/CDN/FE-generated id survives end-to-end), otherwise a fresh
// UUID v4 is generated. The id is (a) stored in the gin context for downstream log enrichment
// (access log, recovery, handler error logs) and (b) echoed on the response header so a client
// can quote it when reporting a failure — ops then greps one id across the whole log stream.
//
// It MUST be the FIRST middleware in the chain: everything downstream (access log, recovery,
// handlers) reads the id via RequestIDFromContext.
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HeaderRequestID is the request/response header carrying the correlation id.
const HeaderRequestID = "X-Request-ID"

// ctxKeyRequestID is the gin-context key under which RequestID stores the id. Private —
// downstream code must read it via RequestIDFromContext (same contract as ctxKeyTenantID).
const ctxKeyRequestID = "requestID"

// maxRequestIDLen caps an inbound id. Anything longer is treated as garbage and regenerated —
// the id is reflected into logs and the response header, so it must stay bounded and clean.
const maxRequestIDLen = 64

// RequestID returns the first-in-chain correlation middleware: honor a valid inbound
// X-Request-ID (≤64 chars of [A-Za-z0-9_-]) or generate a UUID v4, stash it in the context,
// and set it on the response header before any handler runs.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		c.Set(ctxKeyRequestID, id)
		// Set on the response header NOW (not after c.Next()) so the id survives even when a
		// handler panics or streams: headers are committed on first write.
		c.Writer.Header().Set(HeaderRequestID, id)
		c.Next()
	}
}

// validRequestID reports whether an inbound id is safe to reflect into logs and headers:
// non-empty, at most maxRequestIDLen bytes, alphanumeric plus '-' and '_' only. A byte loop
// (not a regexp) — this runs on every request and the alphabet is ASCII-only by design.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '-', b == '_':
		default:
			return false
		}
	}
	return true
}

// RequestIDFromContext returns the correlation id RequestID stored, or ("", false) when the
// request did not pass through the middleware. Same typed-accessor contract as
// TenantFromContext: never read the raw context key elsewhere.
func RequestIDFromContext(c *gin.Context) (string, bool) {
	v, exists := c.Get(ctxKeyRequestID)
	if !exists {
		return "", false
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
