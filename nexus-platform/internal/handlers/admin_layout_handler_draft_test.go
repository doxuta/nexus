package handlers

// admin_layout_handler_draft_test.go — specs/063-draft-publish S7-3: the per-view body of
// GET /metadata/layouts/:entity carries draftState when (and only when) the service set it — the
// FE label "Nháp · Khách chưa thấy" of the screen editor reads it. Found by the third-person acceptance
// run: the service set it and the response shaper dropped it.

import (
	"testing"

	"nexus/platform/internal/services"
)

func TestBuildViewResponse_DraftState(t *testing.T) {
	layout := map[string]any{"version": float64(1), "sections": []any{}}
	cases := []struct {
		name      string
		in        services.LayoutViewResult
		wantState any
	}{
		{"edited draft", services.LayoutViewResult{ID: "L1", DraftState: "edited", LayoutData: layout}, "edited"},
		{"draft-only screen", services.LayoutViewResult{ID: "d-1", DraftState: "new", LayoutData: layout}, "new"},
		{"customer / live read: key absent", services.LayoutViewResult{ID: "L1", LayoutData: layout}, nil},
		{"generated default: key absent", services.LayoutViewResult{IsDefault: true, LayoutData: layout}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := buildViewResponse(&tc.in)
			got, present := out["draftState"]
			if tc.wantState == nil {
				if present {
					t.Fatalf("draftState = %v, want the key absent (the customer payload must not change)", got)
				}
			} else if got != tc.wantState {
				t.Fatalf("draftState = %v, want %v", got, tc.wantState)
			}
			if out["sections"] == nil || out["isDefault"] != tc.in.IsDefault {
				t.Errorf("layout keys must still be flattened into the view: %v", out)
			}
		})
	}
}
