package handlers

import (
	"net/http"
	"testing"
)

// specs/043 S3b (T068): the second of two decisions racing for one step loses its optimistic guard
// and must be told so as a 409 (a state conflict — reload and look again), not a 422 as if its
// input were wrong. The other lifecycle refusals keep their status: the caller can do nothing
// about "not the approver" or "already finished" by reloading.
func TestApprovalHandler_ConflictIs409(t *testing.T) {
	for code, want := range map[string]int{
		"APPROVAL_CONFLICT":       http.StatusConflict,
		"APPROVAL_NOT_PENDING":    http.StatusUnprocessableEntity,
		"APPROVAL_NOT_AUTHORIZED": http.StatusUnprocessableEntity,
	} {
		if got := statusForCode(code); got != want {
			t.Errorf("statusForCode(%s) = %d, want %d", code, got, want)
		}
	}
}
