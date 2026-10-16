package bpm

import (
	"strings"
	"testing"
)

// TestPublicHistory: a person who is not an admin reads the timeline without the machine's own
// lines; the slice with none of them is returned as it is.
func TestPublicHistory(t *testing.T) {
	h := []HistoryEntry{
		{Action: HistorySubmitted}, {Action: HistoryGateway, Comment: "cap-1"}, {Action: HistoryApproved},
		{Action: HistoryAuto, Comment: "code=A"}, {Action: HistoryEffectFailed, Comment: "boom"}, {Action: HistoryRetried}, {Action: HistoryEnded},
	}
	got := PublicHistory(h)
	var actions []string
	for _, e := range got {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "submitted,approved" {
		t.Fatalf("public history %v; want submitted, approved", actions)
	}
	if len(h) != 7 {
		t.Fatal("PublicHistory changed its input")
	}
	plain := []HistoryEntry{{Action: HistoryApproved}}
	if &PublicHistory(plain)[0] != &plain[0] {
		t.Fatal("a history with no machine line was copied")
	}
	if PublicHistory(nil) != nil {
		t.Fatal("nil history must stay nil")
	}
}
