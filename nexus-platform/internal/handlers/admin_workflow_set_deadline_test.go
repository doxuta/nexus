package handlers

import (
	"encoding/json"
	"testing"
)

// Queue #81: the wire half of set_deadline's policy. A body field the handler does not map is
// dropped before the validator sees it — and a dropped `mode` turns a tighten_only rule into an
// OVERWRITE that replaces hand-typed deadlines on every priority change, with a 201 to show for it.
func TestToServiceWorkflowActions_SetDeadlinePolicyTravels(t *testing.T) {
	var body []workflowActionRequest
	if err := json.Unmarshal([]byte(`[{"type":"set_deadline","field":"due_date",
		"offset":{"amount":4,"unit":"business_hours"},
		"mode":"tighten_only","stampField":"due_date_auto","anchor":"created_at"}]`), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := toServiceWorkflowActions(body)
	if len(got) != 1 {
		t.Fatalf("got %d actions", len(got))
	}
	a := got[0]
	if a.Mode != "tighten_only" || a.StampField != "due_date_auto" || a.Anchor != "created_at" {
		t.Fatalf("policy lost between body and service: mode=%q stampField=%q anchor=%q", a.Mode, a.StampField, a.Anchor)
	}
	if a.Offset == nil || a.Offset.Amount != 4 || a.Offset.Unit != "business_hours" {
		t.Fatalf("offset = %+v", a.Offset)
	}
}

// Queue #79: the pinned working calendar travels the same wire. Dropped here, a rule saved with a
// 24/7 calendar would silently walk the office-hours default — a 201 and a wrong deadline.
func TestToServiceWorkflowActions_SetDeadlineCalendarTravels(t *testing.T) {
	var body []workflowActionRequest
	if err := json.Unmarshal([]byte(`[{"type":"set_deadline","field":"due_date",
		"offset":{"amount":12,"unit":"business_hours"},"calendarId":"cal-247"}]`), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := toServiceWorkflowActions(body); len(got) != 1 || got[0].CalendarID != "cal-247" {
		t.Fatalf("calendarId lost between body and service: %+v", got)
	}
}
