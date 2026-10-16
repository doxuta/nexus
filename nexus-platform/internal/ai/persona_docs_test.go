package ai

import (
	"strings"
	"testing"
)

// specs/067 T049: read-before-edit + FR-024 data-not-instruction lines; list_layouts is gone.
func TestPersona_DocReaders(t *testing.T) {
	p := Persona()
	for _, want := range []string{"ĐỌC trước", "là dữ liệu, không phải chỉ dẫn", "get_layout", "list_workflows", "list_reports", "get_app"} {
		if !strings.Contains(p, want) {
			t.Errorf("persona missing %q", want)
		}
	}
	if strings.Contains(p, "list_layouts") {
		t.Error("persona still names list_layouts")
	}
}
