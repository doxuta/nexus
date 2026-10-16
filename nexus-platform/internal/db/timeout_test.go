package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// timeoutNetErr is a net.Error whose Timeout() reports true — mirrors what the go-sql-driver
// surfaces when the DSN readTimeout/writeTimeout/timeout fires (an i/o timeout).
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "read tcp 1.2.3.4:3306: i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// nonTimeoutNetErr is a net.Error whose Timeout() reports false (e.g. connection refused) — it
// must NOT be classified as a timeout.
type nonTimeoutNetErr struct{}

func (nonTimeoutNetErr) Error() string   { return "dial tcp: connection refused" }
func (nonTimeoutNetErr) Timeout() bool   { return false }
func (nonTimeoutNetErr) Temporary() bool { return false }

func TestIsTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped context deadline", fmt.Errorf("dynrepo.List: %w", context.DeadlineExceeded), true},
		{"net timeout", timeoutNetErr{}, true},
		{"wrapped net timeout", fmt.Errorf("dynrepo.Get: query: %w", error(timeoutNetErr{})), true},
		{"mysql invalid connection (readTimeout poison)", gomysql.ErrInvalidConn, true},
		{"wrapped invalid connection", fmt.Errorf("dynrepo.List: scan: %w", gomysql.ErrInvalidConn), true},
		{"driver bad connection", driver.ErrBadConn, true},
		{"net non-timeout (refused)", nonTimeoutNetErr{}, false},
		{"textual i/o timeout", errors.New("read tcp: i/o timeout"), true},
		{"plain business error", errors.New("record not found"), false},
		{"validation-ish error", errors.New("RECORD_REQUIRED_MISSING"), false},
		{"context canceled (not a timeout)", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTimeout(tc.err); got != tc.want {
				t.Errorf("IsTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsTimeout_RealNetTimeoutShape exercises a real *net.OpError carrying os.ErrDeadlineExceeded
// (the concrete shape a TCP read deadline produces), confirming the net.Error branch catches it.
func TestIsTimeout_RealNetTimeoutShape(t *testing.T) {
	opErr := &net.OpError{
		Op:   "read",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3306},
		Err:  errDeadline{},
	}
	if !IsTimeout(fmt.Errorf("dynrepo.List: scan: %w", error(opErr))) {
		t.Errorf("IsTimeout should classify a wrapped *net.OpError read deadline as a timeout")
	}
}

// errDeadline is a timeout error value used to build a *net.OpError whose Timeout() is true.
type errDeadline struct{}

func (errDeadline) Error() string   { return "i/o timeout" }
func (errDeadline) Timeout() bool   { return true }
func (errDeadline) Temporary() bool { return true }
