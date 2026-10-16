package workflow

import (
	"encoding/json"
	"testing"
)

// The admin read must always carry intervalSeconds (0 included): the builder sends it back on a
// re-save, and a missing key made it invent 3600 (queue #282).
func TestWorkflowRecordCarriesIntervalSeconds(t *testing.T) {
	for _, n := range []int{0, 604800} {
		row := workflowRow{IntervalSeconds: n}
		b, err := json.Marshal(rowToRecord(&row))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, ok := m["intervalSeconds"].(float64)
		if !ok || int(got) != n {
			t.Fatalf("intervalSeconds = %v (present=%v), want %d", m["intervalSeconds"], ok, n)
		}
	}
}
