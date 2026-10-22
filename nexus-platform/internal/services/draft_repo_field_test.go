package services

import (
	"reflect"
	"testing"
)

func TestIndexedOnlyOptions(t *testing.T) {
	str := func(s string) *string { return &s }
	live := str(`{"values": ["a", "b"]}`)
	overlay := str(`{"values": ["a", "b", "c"]}`) // a pending draft added "c"
	tests := []struct {
		name string
		sent map[string]any
		want map[string]any
	}{
		{"live options + indexed: the flag on the builder's view", map[string]any{"values": []any{"a", "b"}, "indexed": true},
			map[string]any{"values": []any{"a", "b", "c"}, "indexed": true}},
		{"overlay options + indexed: meant whole", map[string]any{"values": []any{"a", "b", "c"}, "indexed": true}, nil},
		{"hand revert to live, indexed untouched: meant whole", map[string]any{"values": []any{"a", "b"}}, nil},
		{"another option changed too: meant whole", map[string]any{"values": []any{"a"}, "indexed": true}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := indexedOnlyOptions(tc.sent, live, overlay)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	// toggling off from a live read keeps the overlay and drops the flag
	got, err := indexedOnlyOptions(map[string]any{"values": []any{"a", "b"}}, str(`{"values": ["a", "b"], "indexed": true}`),
		str(`{"values": ["a", "b", "c"], "indexed": true}`))
	if err != nil || !reflect.DeepEqual(got, map[string]any{"values": []any{"a", "b", "c"}}) {
		t.Fatalf("toggle off = %v, %v", got, err)
	}
}
