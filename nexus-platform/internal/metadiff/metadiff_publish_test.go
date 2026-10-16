package metadiff

import (
	"fmt"
	"testing"
)

// specs/063 S7-6 T097: noise rules for the types the publish history diffs but the pack/AI diff
// never needed. Saved-as-is must read as no change; a real edit must still show.
func TestCanonicalize_PublishTypes(t *testing.T) {
	tests := []struct {
		name, typ, a, b string
		wantDiff        string
	}{
		{"dashboard: widgets re-ordered by storage", "dashboard",
			`{"name":"d","widgets":[{"id":"w2","t":"kpi"},{"id":"w1","t":"chart"}]}`,
			`{"name":"d","widgets":[{"id":"w1","t":"chart"},{"id":"w2","t":"kpi"}]}`, "[]"},
		{"dashboard: a real widget edit shows", "dashboard",
			`{"widgets":[{"id":"w1","t":"chart"},{"id":"w2","t":"kpi"}]}`,
			`{"widgets":[{"id":"w2","t":"table"},{"id":"w1","t":"chart"}]}`, "[widgets[1].t]"},
		{"dashboard: widgets without ids keep their order", "dashboard",
			`{"widgets":[{"t":"b"},{"t":"a"}]}`, `{"widgets":[{"t":"a"},{"t":"b"}]}`, "[widgets[0].t widgets[1].t]"},
		{"branding: empty values are absent", "branding",
			`{"primaryColor":"#fff","logoUrl":"","tagline":null}`, `{"primaryColor":"#fff"}`, "[]"},
		{"branding: colour change shows", "branding",
			`{"primaryColor":"#fff"}`, `{"primaryColor":"#000"}`, "[primaryColor]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := dec(t, tc.a), dec(t, tc.b)
			Canonicalize(tc.typ, a)
			Canonicalize(tc.typ, b)
			d := Diff(a, b)
			if d == nil {
				d = []string{}
			}
			if got := fmt.Sprint(d); got != tc.wantDiff {
				t.Fatalf("diff = %s, want %s", got, tc.wantDiff)
			}
		})
	}
}

// Layout/menu order IS meaning: moving a section is a change, never normalised away.
func TestCanonicalize_LayoutOrderIsMeaning(t *testing.T) {
	a := dec(t, `{"layout":{"sections":[{"id":"a"},{"id":"b"}]}}`)
	b := dec(t, `{"layout":{"sections":[{"id":"b"},{"id":"a"}]}}`)
	Canonicalize("layout", a)
	Canonicalize("layout", b)
	if len(Diff(a, b)) == 0 {
		t.Fatal("re-ordered sections read as unchanged")
	}
}
