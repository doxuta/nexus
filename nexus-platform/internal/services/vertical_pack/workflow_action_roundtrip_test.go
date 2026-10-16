package vertical_pack

import (
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// Export drops a workflow action's params by OMISSION: `export.go` builds a PackWorkflowAction
// field-by-field, so any field of workflow.Action it does not name is silently lost, and the pack
// then dies at install Pass 8 (WORKFLOW_ACTION_INVALID, no rollback) — or worse, installs a rule
// with a piece missing. The project has now paid for that twice: `create_record`
// (entityName/values, PACK-EXPORT-FULL) and `set_deadline` (offset).
//
// These tests make the eighth action type fail HERE, at compile-and-run time in this package,
// instead of on a customer's install. They are structural: adding a field to workflow.Action
// without a pack path breaks them, and the failure message says exactly which field.
//
// packFieldFor maps a workflow.Action field name to the PackWorkflowAction field carrying it (the
// two names differ where the wire shape differs — CreateEntity travels as EntityName).
var packFieldFor = map[string]string{
	"Type":         "Type",
	"Field":        "Field",
	"Value":        "Value",
	"ChildEntity":  "ChildEntity",
	"ChildVia":     "ChildVia",
	"UserID":       "UserID",
	"Title":        "Title",
	"Message":      "Message",
	"To":           "To",
	"Subject":      "Subject",
	"Body":         "Body",
	"URL":          "URL",
	"Secret":       "Secret",
	"Headers":      "Headers",
	"Strategy":     "Strategy",
	"Team":         "Team",
	"CreateEntity": "EntityName",
	"Values":       "Values",
	"Offset":       "Offset",
	"Mode":         "Mode",
	"StampField":   "StampField",
	"Anchor":       "Anchor",
	"CalendarID":   "CalendarID",
	"Via":          "Via",
	"OnlyIfEmpty":  "OnlyIfEmpty",
	"ValueSource":  "ValueSource",
	"Ops":          "Ops",
	"ProcessID":    "ProcessName",
	// specs/050 S6: a vault key travels as its NAME (the id is the source tenant's) + the path.
	"CredentialID": "CredentialName",
	"Path":         "Path",
}

func TestPackWorkflowAction_CarriesEveryEngineActionField(t *testing.T) {
	engine := reflect.TypeOf(workflow.Action{})
	pack := reflect.TypeOf(PackWorkflowAction{})
	for i := 0; i < engine.NumField(); i++ {
		name := engine.Field(i).Name
		packName, mapped := packFieldFor[name]
		if !mapped {
			t.Fatalf("workflow.Action.%s has no entry in packFieldFor — a new action param was added "+
				"without deciding how it travels through a pack. Add it to PackWorkflowAction, copy it in "+
				"export.go, map it back in install.go toWorkflowInput, then list it here.", name)
		}
		if _, ok := pack.FieldByName(packName); !ok {
			t.Fatalf("workflow.Action.%s maps to PackWorkflowAction.%s, which does not exist — export "+
				"would drop this param in silence and the install would fail at Pass 8.", name, packName)
		}
	}
}

// The service input is the OTHER half: install maps PackWorkflowAction → WorkflowActionInput, and a
// field missing there is the same silent drop one layer down.
func TestWorkflowActionInput_CarriesEveryEngineActionField(t *testing.T) {
	engine := reflect.TypeOf(workflow.Action{})
	input := reflect.TypeOf(services.WorkflowActionInput{})
	for i := 0; i < engine.NumField(); i++ {
		name := engine.Field(i).Name
		if name == "Type" {
			continue // carried as Type on the input too, but checked explicitly below
		}
		if _, ok := input.FieldByName(name); !ok {
			t.Fatalf("workflow.Action.%s has no matching services.WorkflowActionInput field — the "+
				"install path (toWorkflowInput) has nowhere to put it.", name)
		}
	}
	if _, ok := input.FieldByName("Type"); !ok {
		t.Fatal("services.WorkflowActionInput has no Type field")
	}
}

// A live round-trip of the two action types whose params are nested (and therefore the easiest to
// drop): engine action → pack action → service input, asserting the values survive both hops.
func TestPackWorkflowAction_SetDeadlineRoundTrip(t *testing.T) {
	src := workflow.Action{
		Type:  workflow.ActionSetDeadline,
		Field: "due_date",
		Offset: &workflow.TimerOffset{
			Amount: 4,
			Unit:   workflow.TimerUnitHours,
		},
	}

	// --- export hop (the same field-by-field copy export.go performs) ---
	pa := PackWorkflowAction{Type: src.Type, Field: src.Field}
	if src.Offset != nil {
		pa.Offset = &PackWorkflowTimerOffset{Amount: src.Offset.Amount, Unit: src.Offset.Unit}
	}
	if pa.Offset == nil {
		t.Fatal("export hop dropped the offset — the pack JSON would carry a set_deadline with no offset")
	}

	// --- install hop (toWorkflowInput) ---
	in := services.WorkflowActionInput{Type: pa.Type, Field: pa.Field}
	if pa.Offset != nil {
		in.Offset = &services.WorkflowTimerOffsetInput{Amount: pa.Offset.Amount, Unit: pa.Offset.Unit}
	}
	if in.Offset == nil || in.Offset.Amount != 4 || in.Offset.Unit != workflow.TimerUnitHours {
		t.Fatalf("install hop produced %+v; want amount=4 unit=hours", in.Offset)
	}

	// And the installed input must actually validate — a round-trip that survives the copies but
	// fails the validator is the same dead install, just one line later.
	if err := services.ValidateWorkflowConfig("tk_tickets", "after_create", nil,
		[]services.WorkflowActionInput{in}, map[string]string{"due_date": "datetime"}); err != nil {
		t.Fatalf("round-tripped set_deadline no longer validates: %v", err)
	}

	// The failure this guards: an offset lost anywhere above.
	in.Offset = nil
	err := services.ValidateWorkflowConfig("tk_tickets", "after_create", nil,
		[]services.WorkflowActionInput{in}, map[string]string{"due_date": "datetime"})
	if err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("a set_deadline with no offset must fail install validation; got %v", err)
	}
}
