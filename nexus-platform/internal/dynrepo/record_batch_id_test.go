package dynrepo

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// specs/047 FR-13: a batch's row ids are a pure function of its base id, so a stuck claim can probe
// row 0 and an atomic bulk's response can be rebuilt from the base id alone.
func TestBatchRowID(t *testing.T) {
	base := uuid.NewString()
	if got := BatchRowID(base, 0); got != base {
		t.Fatalf("row 0 = %q, want the base id %q", got, base)
	}
	seen := map[string]bool{base: true}
	for i := 1; i < 200; i++ {
		id := BatchRowID(base, i)
		if _, err := uuid.Parse(id); err != nil || seen[id] {
			t.Fatalf("row %d = %q (err %v, duplicate %v), want a fresh UUID", i, id, err, seen[id])
		}
		if again := BatchRowID(base, i); again != id {
			t.Fatalf("row %d is not deterministic: %q then %q", i, id, again)
		}
		seen[id] = true
	}
	if other := BatchRowID(uuid.NewString(), 1); seen[other] {
		t.Fatalf("two bases produced the same row-1 id %q", other)
	}
	if got := BatchRowID("not-a-uuid", 3); got != "" {
		t.Fatalf("BatchRowID on a non-UUID base = %q, want empty", got)
	}
}

func TestBatchRowIDs(t *testing.T) {
	base := uuid.NewString()
	tests := []struct {
		name    string
		ctx     context.Context
		wantErr bool
		derived bool
	}{
		{"no base mints random ids", context.Background(), false, false},
		{"base derives the ids", WithBatchBaseID(context.Background(), base), false, true},
		{"non-UUID base is refused", WithBatchBaseID(context.Background(), "nope"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := batchRowIDs(tt.ctx, 4)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			for i, id := range ids {
				if got := id == BatchRowID(base, i); got != tt.derived {
					t.Fatalf("row %d = %q, derived from base = %v, want %v", i, id, got, tt.derived)
				}
			}
		})
	}
}
