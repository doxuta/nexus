package bpm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInstance_JSONHidesEngineVocabulary: the record's approvals tab and the inbox serialize
// Instance verbatim to everyone who may see the record. last_error names fields and refused values —
// engine vocabulary the customer face never shows — so it must not ride that body (an admin reads
// it from the detail and the per-process list).
func TestInstance_JSONHidesEngineVocabulary(t *testing.T) {
	ctxMap, err := (InstanceContext)(nil).WithGateway("route", "cap-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Instance{ID: "i1", LastError: `field "ma" đã tồn tại`, Context: ctxMap})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"LastError", "đã tồn tại", "Context", "cap-1"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("instance JSON %s leaks %q", raw, leak)
		}
	}
}
