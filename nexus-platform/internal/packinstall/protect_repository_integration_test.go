//go:build integration

package packinstall

import (
	"context"
	"testing"

	"nexus/platform/internal/services/vertical_pack"
)

// SetProtected flips only the exact (tenant, slug, type, key) row and refuses the sub-item types
// that have no baseline row of their own (a row for them would be reported "missing" by the diff).
func TestSetProtected_PinnedAndRefusesSubItems(t *testing.T) {
	gdb := piSetup(t)
	r := New(gdb)
	ctx := context.Background()
	for _, ten := range []string{tA, tB} {
		if err := r.SaveItems(ctx, ten, "p", "1.0.0", []vertical_pack.PackItem{{Type: "workflow", Key: "k", Content: []byte(`{}`), Wrote: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SetProtected(ctx, tA, "p", "workflow", "k", true); err != nil {
		t.Fatal(err)
	}
	flag := func(ten string) bool {
		rows, _ := r.ListItems(ctx, ten, "p")
		return len(rows) == 1 && rows[0].Protected
	}
	if !flag(tA) || flag(tB) {
		t.Fatalf("protected A=%v B=%v; want true,false", flag(tA), flag(tB))
	}
	if err := r.SetProtected(ctx, tA, "p", "workflow", "k", true); err != nil { // same value twice: idempotent
		t.Fatalf("same-value SetProtected: %v", err)
	}
	if err := r.SetProtected(ctx, tA, "p", "workflow", "k", false); err != nil || flag(tA) {
		t.Fatalf("clear: err=%v flag=%v", err, flag(tA))
	}
	for _, typ := range []string{"field", "role_grant"} {
		if err := r.SetProtected(ctx, tA, "p", typ, "x.y", true); err == nil {
			t.Errorf("%s: SetProtected must refuse", typ)
		}
	}
	if rows, _ := r.ListItems(ctx, tA, "p"); len(rows) != 1 {
		t.Errorf("a refused call inserted a row: %v", rows)
	}
	if err := r.SetProtected(ctx, tA, "p", "workflow", "missing", true); err == nil {
		t.Error("an absent row must be an error, not a silent no-op")
	}
}
