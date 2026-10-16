package ai

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The JSON file is the referee between Go and the FE PANEL_LABELS (vitest reads the same file).
func TestStudioPanelHints_MatchTestdata(t *testing.T) {
	raw, err := os.ReadFile("testdata/studio_panels.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range studioPanelHints {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("panels = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("panels = %v; want %v", got, want)
		}
	}
}
