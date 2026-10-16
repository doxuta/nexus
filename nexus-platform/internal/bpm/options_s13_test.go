package bpm

import (
	"testing"

	"nexus/platform/internal/workflow"
)

// TestCheckOptions_PauseAndCalendar (B6 validator parity): the S13 options are judged at save, not
// left to misbehave at run time.
func TestCheckOptions_PauseAndCalendar(t *testing.T) {
	changed := cond("", workflow.Rule{Field: "status", Op: workflow.OpChanged})
	cases := []struct {
		name  string
		opts  Options
		field string
	}{
		{"pin + pauseWhen are fine", Options{CalendarID: "cal-1", PauseWhen: pendingCond()}, ""},
		{"padded calendar id", Options{CalendarID: " cal-1"}, "calendarId"},
		{"too-long calendar id", Options{CalendarID: "0123456789012345678901234567890123456"}, "calendarId"},
		{"pauseWhen with no rules", Options{PauseWhen: &workflow.Condition{}}, "pauseWhen"},
		{"pauseWhen on a before-image op", Options{PauseWhen: changed}, "pauseWhen"},
		{"pauseWhen with a timer", Options{PauseWhen: &workflow.Condition{Rules: pendingCond().Rules, Timer: &workflow.TimerConfig{}}}, "pauseWhen"},
	}
	for _, tc := range cases {
		if got, _ := CheckOptions(&tc.opts); got != tc.field {
			t.Errorf("%s: blamed %q, want %q", tc.name, got, tc.field)
		}
	}
}
