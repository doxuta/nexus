package services

// DASH-WIDGET-SCHEMA: the dashboard widget schema now carries react-grid-layout coordinates
// (x/y/w/h) and accepts the "funnel" render type. These unit tests prove: the layout keys
// round-trip through validate+marshal, "funnel" is accepted, an out-of-bounds coordinate is
// rejected with WIDGET_LAYOUT_INVALID (→ 422 via statusForCode default), and the [0,48] bounds
// are inclusive.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateWidgets_Layout(t *testing.T) {
	// The new "funnel" render type is accepted (previously WIDGET_TYPE_INVALID).
	t.Run("funnel type accepted", func(t *testing.T) {
		w := WidgetConfig{ID: "w1", Title: "t", Type: "funnel", Entity: "deals", Metric: "count"}
		if _, err := validateAndMarshalWidgets([]WidgetConfig{w}); err != nil {
			t.Fatalf("funnel type must be accepted; err=%v", err)
		}
	})

	// x/y/w/h survive marshal → unmarshal AND are emitted verbatim (no omitempty drop at origin).
	t.Run("x/y/w/h round-trip", func(t *testing.T) {
		w := WidgetConfig{ID: "w1", Title: "t", Type: "bar", Entity: "deals", Metric: "count", X: 2, Y: 3, W: 6, H: 4}
		raw, err := validateAndMarshalWidgets([]WidgetConfig{w})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var back []WidgetConfig
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(back) != 1 {
			t.Fatalf("len=%d; want 1", len(back))
		}
		if back[0].X != 2 || back[0].Y != 3 || back[0].W != 6 || back[0].H != 4 {
			t.Errorf("layout round-trip = {x:%d y:%d w:%d h:%d}; want {2 3 6 4}", back[0].X, back[0].Y, back[0].W, back[0].H)
		}
		s := string(raw)
		for _, want := range []string{`"x":2`, `"y":3`, `"w":6`, `"h":4`} {
			if !strings.Contains(s, want) {
				t.Errorf("marshaled widget missing %s: %s", want, s)
			}
		}
	})

	// Origin/max boundary values are inclusive-valid.
	t.Run("boundary values accepted", func(t *testing.T) {
		w := WidgetConfig{ID: "w1", Title: "t", Type: "kpi", Entity: "deals", Metric: "count", X: 0, Y: 0, W: widgetCoordMax, H: widgetCoordMax}
		if _, err := validateAndMarshalWidgets([]WidgetConfig{w}); err != nil {
			t.Fatalf("boundary values must be accepted; err=%v", err)
		}
	})

	// Unset layout (all coords 0) is the sentinel for "FE auto-places/sizes" — must be accepted
	// (a freshly-added widget carries no explicit grid footprint; this is the common persisted shape).
	t.Run("unset layout (all zero) accepted", func(t *testing.T) {
		w := WidgetConfig{ID: "w1", Title: "t", Type: "kpi", Entity: "deals", Metric: "count"}
		if _, err := validateAndMarshalWidgets([]WidgetConfig{w}); err != nil {
			t.Fatalf("all-zero (unset) layout must be accepted; err=%v", err)
		}
	})

	// An out-of-bounds coordinate (negative or above widgetCoordMax) is rejected.
	t.Run("out-of-bounds rejected", func(t *testing.T) {
		cases := []struct {
			name string
			mut  func(*WidgetConfig)
		}{
			{"x negative", func(w *WidgetConfig) { w.X = -1 }},
			{"y too large", func(w *WidgetConfig) { w.Y = widgetCoordMax + 1 }},
			{"w too large", func(w *WidgetConfig) { w.W = 100 }},
			{"h negative", func(w *WidgetConfig) { w.H = -5 }},
			// A SINGLE-zero extent (one of w/h zero while the other is set) is a degenerate
			// zero-area tile — rejected. (Both-zero is the valid "unset" sentinel, tested below.)
			{"w zero while h set (degenerate)", func(w *WidgetConfig) { w.W = 0 }},
			{"h zero while w set (degenerate)", func(w *WidgetConfig) { w.H = 0 }},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				// Base widget has VALID extents (W/H ≥ 1) so each case isolates its coordinate.
				w := WidgetConfig{ID: "w1", Title: "t", Type: "bar", Entity: "deals", Metric: "count", W: 6, H: 4}
				c.mut(&w)
				_, err := validateAndMarshalWidgets([]WidgetConfig{w})
				if got := w4CodeOf(err); got != "WIDGET_LAYOUT_INVALID" {
					t.Fatalf("code = %q (err=%v); want WIDGET_LAYOUT_INVALID", got, err)
				}
			})
		}
	})
}
