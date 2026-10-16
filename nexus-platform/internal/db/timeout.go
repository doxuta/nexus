package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"
)

// IsTimeout reports whether err is a database statement/connection timeout (W5-E2).
//
// It is the single classifier the handler layer uses to turn a slow/hung-MySQL failure into a
// clean 503 DB_TIMEOUT instead of a generic 500 or a hang. It recognises the shapes the timeout
// can take with the go-sql-driver + a context deadline in front of it:
//
//   - context.DeadlineExceeded — a per-request/per-query context.WithTimeout fired.
//   - a net.Error whose Timeout() is true — the DSN readTimeout/writeTimeout/timeout fired and
//     the driver surfaced an i/o timeout (e.g. "read tcp ... i/o timeout", dial timeout).
//   - mysql.ErrInvalidConn / driver.ErrBadConn ("invalid connection") — when a DSN readTimeout
//     aborts an in-flight read, the go-sql-driver poisons the connection and (after exhausting
//     its bad-conn retries) returns ErrInvalidConn to the caller rather than the raw net error.
//     This is the dominant shape a statement timeout takes through database/sql, so it must
//     classify as a DB timeout (it is an availability failure, not a client/business error).
//   - the driver's textual "i/o timeout" / "invalid connection" fallbacks for code paths that
//     wrap the cause as a plain error (string match is the last resort, kept narrow).
//
// It walks the wrap chain (errors.As / errors.Is) so a timeout buried under the standard
// fmt.Errorf("pkg.method: %w", err) wrapping is still detected.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	// 1) A context deadline fired (per-query/per-request context.WithTimeout).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// 2) A net.Error timeout (DSN readTimeout/writeTimeout/timeout → driver i/o timeout).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// 3) The driver's poisoned-connection sentinels. A DSN readTimeout that aborts an in-flight
	//    read returns mysql.ErrInvalidConn (== driver.ErrBadConn after retries) — the dominant
	//    statement-timeout shape through database/sql.
	if errors.Is(err, gomysql.ErrInvalidConn) || errors.Is(err, driver.ErrBadConn) {
		return true
	}
	// 4) Last-resort textual match for the same conditions when the cause is not exposed as a
	//    typed sentinel through the wrap chain. Kept narrow so it cannot misclassify business errors.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "invalid connection") {
		return true
	}
	return false
}
