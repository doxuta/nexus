// Package middleware — body_limit.go implements the request body-size cap (RM-16).
// It protects the server from OOM caused by a client sending a very large JSON payload.
//
// Two rejection paths:
//
//  1. Fast-path: Content-Length header is present and already exceeds the limit →
//     reject immediately with 413 before reading any bytes. This is the common case
//     for well-behaved clients and avoids any unnecessary I/O.
//
//  2. Slow-path: Content-Length is absent or misleading (chunked transfer-encoding,
//     malicious client that lies about size) → the body is wrapped in an
//     http.MaxBytesReader. The downstream JSON binder (c.ShouldBindJSON via bindJSON)
//     receives *http.MaxBytesError on the first read past the limit. bindJSON calls
//     IsMaxBytesError and writes the standard 413 envelope immediately.
//
// The post-c.Next() c.Errors scan was removed: bindJSON always writes its own response
// (and aborts the chain) when it hits a MaxBytesError, so there is nothing left for
// the middleware to clean up after the chain returns.
//
// When limit ≤ 0 the middleware is a no-op (no limit applied).
package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxBodySize returns a gin middleware that limits the size of every request body to
// limit bytes (RM-16). It wraps http.Request.Body in an http.MaxBytesReader so that
// any downstream read beyond the limit surfaces an *http.MaxBytesError.
//
// When limit ≤ 0 the middleware is a no-op (no limit applied).
func MaxBodySize(limit int64) gin.HandlerFunc {
	if limit <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		// Fast-path: reject when Content-Length already exceeds the limit.
		// This avoids reading any bytes for clearly-oversized requests.
		if c.Request.ContentLength > limit {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":   "Request body exceeds the maximum allowed size",
				"code":    "REQUEST_TOO_LARGE",
				"details": gin.H{},
			})
			return
		}

		// Slow-path: wrap the body so reads past limit return MaxBytesError.
		// The wrapper is installed before c.Next() so every downstream handler sees it.
		// bindJSON (handlers/bind.go) calls IsMaxBytesError on bind error and writes 413.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}

// isMaxBytesError reports whether err (or any error in its chain) is the
// *http.MaxBytesError returned by http.MaxBytesReader when the limit is exceeded.
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// IsMaxBytesError is the exported form for use by gin handlers that need to detect an
// over-limit body and reply with 413 themselves (e.g. the bindJSON helper in
// handlers/bind.go).
func IsMaxBytesError(err error) bool {
	return isMaxBytesError(err)
}

// RespondTooLarge writes the standard 413 error envelope and aborts the gin chain.
// Handlers that receive a MaxBytesError from ShouldBindJSON should call this helper
// instead of writing their own error response.
func RespondTooLarge(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
		"error":   "Request body exceeds the maximum allowed size",
		"code":    "REQUEST_TOO_LARGE",
		"details": gin.H{},
	})
}
