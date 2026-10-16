package services

import (
	"strings"
	"testing"

	"nexus/platform/internal/appmeta"
)

// The builder's home queues / per-role default must survive the draft document round trip
// (draft -> publish writes appDoc.updateInput back to sys_apps.options).
func TestAppDoc_CarriesHomeQueuesAndRoleDefault(t *testing.T) {
	a := &appmeta.App{Name: "nha_khoa", Label: "Nha khoa", HomeQueueIDs: []string{"q-1"}, DefaultRoleIDs: []string{"r-1"}}
	in := appDocOf(a, nil).updateInput("u")
	if in.HomeQueueIDs == nil || len(*in.HomeQueueIDs) != 1 || (*in.HomeQueueIDs)[0] != "q-1" {
		t.Fatalf("HomeQueueIDs = %v; want [q-1]", in.HomeQueueIDs)
	}
	if in.DefaultRoleIDs == nil || (*in.DefaultRoleIDs)[0] != "r-1" {
		t.Fatalf("DefaultRoleIDs = %v; want [r-1]", in.DefaultRoleIDs)
	}
	// An app with none writes explicit empties (clears a stale list), and the document stays canonical.
	empty := appDocOf(&appmeta.App{Name: "x", Label: "X"}, nil)
	b, _ := empty.marshal()
	if got := string(b); strings.Contains(got, "homeQueueIds") || strings.Contains(got, "defaultRoleIds") {
		t.Fatalf("empty lists must be omitted from the canonical doc: %s", got)
	}
	if in2 := empty.updateInput("u"); in2.HomeQueueIDs == nil || len(*in2.HomeQueueIDs) != 0 {
		t.Fatalf("empty doc must clear: %v", in2.HomeQueueIDs)
	}
}
