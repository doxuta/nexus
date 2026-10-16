// Package middleware — recovery.go implements the zap panic-recovery middleware (RM-37, OBS-1).
// gin.Default()'s Recovery() printed the stack to stderr with fmt.Fprintf — invisible to the
// structured zap pipeline the rest of the app logs to. This recovery catches a handler panic,
// logs ONE zap Error entry (panic value, stack, route, request id) and answers a clean 500 JSON
// envelope so the client never sees a dropped connection.
//
// Broken-pipe edge case (mirrors gin's Recovery): when the panic is a *net.OpError wrapping a
// syscall "broken pipe" / "connection reset by peer", the CLIENT is gone — writing a 500 would
// itself fail. In that case the error is logged (without a stack: the condition is the peer's,
// not a code bug) and the request is aborted without writing a response.
package middleware

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Recovery returns the zap panic-recovery middleware. Mount it AFTER RequestID and AccessLog so
// (a) the logged entry carries the correlation id and (b) the outer access log still records the
// 500 this recovery writes. Everything mounted after it (CORS, auth, handlers) is covered.
// logger may be nil (no-op logging; the 500 envelope is still written).
func Recovery(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// Normalize the panic value to an error for zap.Error.
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("middleware.Recovery: panic: %v", r)
			}

			fields := []zap.Field{
				zap.Error(err),
				zap.String("method", c.Request.Method),
				zap.String("route", c.FullPath()),
			}
			if id, ok := RequestIDFromContext(c); ok {
				fields = append(fields, zap.String("request_id", id))
			}
			if tenantID, ok := TenantFromContext(c); ok {
				fields = append(fields, zap.String("tenant_id", tenantID))
			}

			if isBrokenPipe(r) {
				// The connection is dead: we cannot write a status. Log and abort only.
				logger.Error("middleware.Recovery: broken pipe during handler", fields...)
				c.Abort()
				return
			}

			fields = append(fields, zap.String("stack", string(debug.Stack())))
			logger.Error("middleware.Recovery: panic recovered", fields...)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "Lỗi hệ thống",
				"code":  "INTERNAL",
			})
		}()
		c.Next()
	}
}

// isBrokenPipe reports whether a recovered panic value is the net-layer "client hung up"
// condition (same detection as gin's built-in Recovery): a *net.OpError whose syscall error
// text contains "broken pipe" or "connection reset by peer".
func isBrokenPipe(r any) bool {
	ne, ok := r.(*net.OpError)
	if !ok {
		return false
	}
	var se *os.SyscallError
	if !errors.As(ne, &se) {
		return false
	}
	msg := strings.ToLower(se.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}
