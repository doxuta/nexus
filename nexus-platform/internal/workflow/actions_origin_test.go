package workflow

import (
	"context"
	"testing"
)

// originSpy records the origin the action handed the sender through ctx.
type originSpy struct {
	got EmailOrigin
	ok  bool
}

func (s *originSpy) SendEmail(ctx context.Context, _, _, _ string) error {
	s.got, s.ok = EmailOriginFrom(ctx)
	return nil
}

// specs/066 L1a: runAction tells the sender WHERE a mail comes from (config ids only).
func TestRunAction_EmailAlertCarriesOrigin(t *testing.T) {
	wf := Workflow{ID: "w1"}
	a := Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "Chào {{name}}", Body: "b"}
	evt := Event{TenantID: "t1", EntityName: "crm_contacts", RecordID: "r1", Data: map[string]any{"email": "x@y.vn", "name": "Lan"}}

	t.Run("origin attached", func(t *testing.T) {
		spy := &originSpy{}
		if _, err := runAction(context.Background(), wf, evt, a, 3, actionDeps{email: spy}); err != nil {
			t.Fatal(err)
		}
		want := EmailOrigin{TenantID: "t1", WorkflowID: "w1", ActionIndex: 3, EntityName: "crm_contacts", RecordID: "r1",
			SubjectTemplate: "Chào {{name}}", ActionHash: EmailActionHash(a)}
		if !spy.ok || spy.got != want {
			t.Errorf("origin = %+v ok=%v, want %+v", spy.got, spy.ok, want)
		}
	})
	t.Run("an origin that already has a LogID is not overwritten", func(t *testing.T) {
		spy := &originSpy{}
		pre := EmailOrigin{TenantID: "t1", LogID: "pre", ResendOf: "old", CreatedBy: "u1"}
		if _, err := runAction(WithEmailOrigin(context.Background(), pre), wf, evt, a, 0, actionDeps{email: spy}); err != nil {
			t.Fatal(err)
		}
		if spy.got != pre {
			t.Errorf("origin = %+v, want the pre-set %+v", spy.got, pre)
		}
	})
}
