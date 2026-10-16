package handlers

import (
	"encoding/json"
	"testing"

	"nexus/platform/internal/ai"
)

// draft_json round-trips the change envelope; sessions saved before L10-4a (no "change") still decode.
func TestAIHandler_DraftJSON_ChangeRoundTripAndLegacy(t *testing.T) {
	h := NewAIBuilderHandler(nil, nil, nil, nil, nil)

	in := []ai.DraftItem{{Kind: "workflow", Change: &ai.DraftChange{
		DocType: "workflow", DocKey: "wf1", Op: "update", Label: "Nhắc lịch hẹn",
		Before: json.RawMessage(`{"actions":[{"type":"outbound_webhook"}]}`),
		After:  json.RawMessage(`{"actions":[]}`), ChangedPaths: []string{"actions"},
	}}}
	raw := marshalDraft(in)
	got := h.decodeDraft(raw)
	if len(got) != 1 || got[0].Change == nil || got[0].Change.Op != "update" || got[0].Change.DocKey != "wf1" ||
		len(got[0].Change.ChangedPaths) != 1 {
		t.Fatalf("change lost on round trip: %s", raw)
	}

	legacy := h.decodeDraft(json.RawMessage(`[{"kind":"workflow"}]`))
	if len(legacy) != 1 || legacy[0].Change != nil {
		t.Fatalf("legacy draft must decode with nil change, got %+v", legacy)
	}
}
