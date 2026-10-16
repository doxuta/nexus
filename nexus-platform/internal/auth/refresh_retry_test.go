package auth

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestRetryOnDeadlock(t *testing.T) {
	dead := &mysql.MySQLError{Number: mysqlDeadlock}
	other := &mysql.MySQLError{Number: 1062}
	plain := errors.New("boom")
	tests := []struct {
		name      string
		errs      []error // result of each successive call; the last one repeats
		wantCalls int
		wantErr   error
	}{
		{"success first try", []error{nil}, 1, nil},
		{"deadlock then success", []error{dead, nil}, 2, nil},
		{"wrapped deadlock then success", []error{fmt.Errorf("x: %w", dead), nil}, 2, nil},
		{"persistent deadlock gives up after the bound", []error{dead}, deadlockRetries, dead},
		{"other mysql error is not retried", []error{other}, 1, other},
		{"plain error is not retried", []error{plain}, 1, plain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryOnDeadlock(func() error {
				i := calls
				if i >= len(tc.errs) {
					i = len(tc.errs) - 1
				}
				calls++
				return tc.errs[i]
			})
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if !errors.Is(err, tc.wantErr) && err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
