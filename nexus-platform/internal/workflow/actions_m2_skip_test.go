package workflow

// specs/066 L3: a mail that fails BEFORE it reaches the sender is still a line in the email log.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// skipRecordingSender is a fakeEmailSender that also implements the optional EmailSkipRecorder.
type skipRecordingSender struct {
	fakeEmailSender
	skips []string
}

func (s *skipRecordingSender) RecordEmailSkipped(_ context.Context, reason string) {
	s.skips = append(s.skips, reason)
}

func TestRunEmailAlert_SkipRecorder(t *testing.T) {
	cause := errors.New("db down")
	cases := []struct {
		name       string
		action     Action
		resolver   TemplateResolver
		data       map[string]any
		wantErr    bool
		wantSkips  int
		wantInText []string // the recorded reason names the TEMPLATE, never the data
		notInText  []string
	}{
		{name: "recipient renders empty", action: Action{Type: ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "s", Body: "b"},
			resolver: &fakeTemplateResolver{values: map[string]any{}}, data: map[string]any{"benh_nhan": "p-1"},
			wantErr: true, wantSkips: 1, wantInText: []string{"{{benh_nhan.email}}", "không có địa chỉ"}, notInText: []string{"p-1"}},
		{name: "render fails", action: Action{Type: ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "s", Body: "b"},
			resolver: &fakeTemplateResolver{err: cause}, data: map[string]any{"benh_nhan": "p-1"},
			wantErr: true, wantSkips: 1, wantInText: []string{"db down"}},
		{name: "rendered fine, sent fine: nothing to record", action: Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "s", Body: "b"},
			data: map[string]any{"email": "a@b.vn"}, wantSkips: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &skipRecordingSender{}
			err := runEmailAlert(context.Background(), Event{Data: tc.data}, tc.action, sender, tc.resolver)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(sender.skips) != tc.wantSkips {
				t.Fatalf("RecordEmailSkipped calls = %d, want %d", len(sender.skips), tc.wantSkips)
			}
			if tc.wantSkips == 0 {
				return
			}
			// The reason is exactly the message the run history gets: one text, two places.
			if sender.skips[0] != err.Error() {
				t.Errorf("reason %q != run error %q", sender.skips[0], err.Error())
			}
			for _, w := range tc.wantInText {
				if !strings.Contains(sender.skips[0], w) {
					t.Errorf("reason %q should contain %q", sender.skips[0], w)
				}
			}
			for _, n := range tc.notInText {
				if strings.Contains(sender.skips[0], n) {
					t.Errorf("reason %q must not contain record data %q", sender.skips[0], n)
				}
			}
			if len(sender.calls) != 0 {
				t.Errorf("sent %d mails for a skipped one", len(sender.calls))
			}
		})
	}
}

// A sender that does not implement the hook behaves exactly as before: same error, nothing else.
func TestRunEmailAlert_NoSkipRecorderUnchanged(t *testing.T) {
	a := Action{Type: ActionEmailAlert, To: "{{missing}}", Subject: "s", Body: "b"}
	if err := runEmailAlert(context.Background(), Event{Data: map[string]any{}}, a, &fakeEmailSender{}, nil); err == nil {
		t.Fatal("empty recipient must still fail the action")
	}
}

// A send that fails AFTER rendering is the sender's to log (it has the transport verdict): the skip
// hook must stay silent or the row would be written twice.
func TestRunEmailAlert_SendFailureIsNotASkip(t *testing.T) {
	sender := &skipRecordingSender{fakeEmailSender: fakeEmailSender{err: errors.New("smtp down")}}
	a := Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "s", Body: "b"}
	if err := runEmailAlert(context.Background(), Event{Data: map[string]any{"email": "a@b.vn"}}, a, sender, nil); err == nil {
		t.Fatal("send failure must fail the action")
	}
	if len(sender.skips) != 0 {
		t.Errorf("RecordEmailSkipped called %d times for a send failure", len(sender.skips))
	}
}
