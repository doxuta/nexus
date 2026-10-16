package vertical_pack

import (
	"fmt"
	"testing"
)

// T008: diffPaths is the unbounded sibling of diffValue (which stops at changedPathsCap).
func TestDiffPaths_Unbounded(t *testing.T) {
	a, b := map[string]any{}, map[string]any{}
	for i := 0; i < 60; i++ {
		a[fmt.Sprintf("k%02d", i)] = "x"
		b[fmt.Sprintf("k%02d", i)] = "y"
	}
	if got := diffPaths(a, b); len(got) != 60 {
		t.Fatalf("diffPaths want 60 paths, got %d", len(got))
	}
	var capped []string
	diffValue("", a, b, &capped)
	if len(capped) != changedPathsCap {
		t.Fatalf("diffValue must keep stopping at %d, got %d", changedPathsCap, len(capped))
	}
}

func TestOverlapPaths(t *testing.T) {
	tests := []struct {
		name       string
		ours, thrs []string
		want       []string
	}{
		{"child under parent", []string{"actions[0]"}, []string{"actions[0].subject"}, []string{"actions[0]"}},
		{"parent under child", []string{"actions[0].subject"}, []string{"actions[0]"}, []string{"actions[0]"}},
		{"array vs element", []string{"actions"}, []string{"actions[1].x"}, []string{"actions"}},
		{"siblings do not overlap", []string{"condition.rules"}, []string{"actions[0]"}, nil},
		{"name prefix is not a path prefix", []string{"label"}, []string{"labelExtra"}, nil},
		{"identical", []string{"a.b"}, []string{"a.b"}, []string{"a.b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := overlapPaths(tt.ours, tt.thrs)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("want %v got %v", tt.want, got)
			}
		})
	}
}
