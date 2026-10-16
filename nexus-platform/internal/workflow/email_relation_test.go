package workflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// specs/059 (DAILY-QUEUE #271): an email_alert on an appointment must be able to write to the
// PATIENT the appointment points at. Before this, renderTemplate read evt.Data only — the raw JSON
// of the triggering row — so {{benh_nhan.email}} and a lookup field {{email_bn}} (computed at READ
// time, never stored) always rendered "", and every such rule failed with "empty recipient".

// fakeTemplateResolver answers placeholders the event data cannot, and records what it was asked.
type fakeTemplateResolver struct {
	mu     sync.Mutex
	values map[string]any
	err    error
	calls  []resolveCall
}

type resolveCall struct {
	tenant, entity string
	names          []string
}

func (f *fakeTemplateResolver) ResolveTemplateValues(_ context.Context, tenantID, entity string, _ map[string]any, names []string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, resolveCall{tenantID, entity, slices.Clone(names)})
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]any{}
	for _, n := range names {
		if v, ok := f.values[n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

func TestRunEmailAlert_RendersFieldsOfTheLinkedRecord(t *testing.T) {
	email := &fakeEmailSender{}
	res := &fakeTemplateResolver{values: map[string]any{
		"benh_nhan.email":  "lan@nhakhoa.vn",
		"benh_nhan.ho_ten": "Nguyễn Thị Lan",
		"email_bn":         "lan@nhakhoa.vn",
	}}
	a := Action{
		Type:    ActionEmailAlert,
		To:      "{{benh_nhan.email}}",
		Subject: "Nhắc hẹn {{ benh_nhan.ho_ten }} lúc {{gio_hen}}",
		Body:    "Gửi {{email_bn}} — {{benh_nhan.email}}",
	}
	data := map[string]any{"benh_nhan": "p-1", "gio_hen": "09:00"}
	evt := Event{TenantID: "t1", EntityName: "lich_hen", RecordID: "a-1", Data: data}

	if err := runEmailAlert(context.Background(), evt, a, email, res); err != nil {
		t.Fatalf("runEmailAlert: %v", err)
	}
	if len(email.calls) != 1 {
		t.Fatalf("sent %d mails, want exactly 1", len(email.calls))
	}
	c := email.calls[0]
	if c.to != "lan@nhakhoa.vn" || c.subject != "Nhắc hẹn Nguyễn Thị Lan lúc 09:00" || c.body != "Gửi lan@nhakhoa.vn — lan@nhakhoa.vn" {
		t.Errorf("sent %+v; want the linked patient's fields rendered", c)
	}
	// ONE resolver call per render, asked ONLY for what the event data cannot answer, each name
	// once, pinned to the event's tenant + entity.
	if len(res.calls) != 1 {
		t.Fatalf("resolver called %d times, want 1", len(res.calls))
	}
	got := res.calls[0]
	slices.Sort(got.names)
	if want := []string{"benh_nhan.email", "benh_nhan.ho_ten", "email_bn"}; !slices.Equal(got.names, want) || got.tenant != "t1" || got.entity != "lich_hen" {
		t.Errorf("resolver asked %+v; want tenant t1, entity lich_hen, names %v", got, want)
	}
	// evt.Data is shared read-only across every action of every rule: resolved values must never
	// be written back into it.
	if len(data) != 2 {
		t.Errorf("evt.Data was mutated: %v", data)
	}
}

// A key present but null renders "" like an absent one, so it is not an answer: the resolver is
// asked, and a lookup the row's JSON happens to carry as null still reaches the linked record.
func TestRenderEmail_NullKeyStillAsksTheResolver(t *testing.T) {
	res := &fakeTemplateResolver{values: map[string]any{"email_bn": "lan@nhakhoa.vn"}}
	got, err := RenderEmail(context.Background(), Event{Data: map[string]any{"email_bn": nil}},
		Action{Type: ActionEmailAlert, To: "{{email_bn}}", Subject: "s", Body: "b"}, res)
	if err != nil || got.To != "lan@nhakhoa.vn" {
		t.Fatalf("RenderEmail = %+v, %v; want the resolved lookup", got, err)
	}
}

func TestRunEmailAlert_NoResolverCallWhenTheRecordAnswersEverything(t *testing.T) {
	res := &fakeTemplateResolver{}
	a := Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "Chào {{name}}", Body: "b"}
	evt := Event{Data: map[string]any{"email": "a@b.vn", "name": "An"}}
	if err := runEmailAlert(context.Background(), evt, a, &fakeEmailSender{}, res); err != nil {
		t.Fatalf("runEmailAlert: %v", err)
	}
	if len(res.calls) != 0 {
		t.Errorf("resolver called %d times for a template the record answers alone — a DB read per mail for nothing", len(res.calls))
	}
}

// A read failure must not degrade to "send with an empty field": a reminder that silently drops
// the patient's name, or goes nowhere, is worse than a FAILED run that says why.
func TestRunEmailAlert_ResolverErrorFailsWithoutSending(t *testing.T) {
	email := &fakeEmailSender{}
	cause := errors.New("db down")
	res := &fakeTemplateResolver{err: cause}
	a := Action{Type: ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "s", Body: "b"}
	err := runEmailAlert(context.Background(), Event{Data: map[string]any{"benh_nhan": "p-1"}}, a, email, res)
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("err = %v; want the read failure wrapped", err)
	}
	if len(email.calls) != 0 {
		t.Errorf("sent %d mails after a failed read, want 0", len(email.calls))
	}
}

// The patient has no email (or the appointment is not linked to one): the run history must say so
// in words the builder can act on, naming the TEMPLATE (the rule's config) — never record data.
func TestRunEmailAlert_EmptyRecipientNamesTheTemplate(t *testing.T) {
	email := &fakeEmailSender{}
	res := &fakeTemplateResolver{values: map[string]any{}}
	a := Action{Type: ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "s", Body: "b"}
	err := runEmailAlert(context.Background(), Event{Data: map[string]any{"benh_nhan": "p-1"}}, a, email, res)
	if err == nil {
		t.Fatal("an empty recipient must fail the action")
	}
	if !containsAll(err.Error(), "{{benh_nhan.email}}", "không có địa chỉ") {
		t.Errorf("error %q must name the recipient template and say there is no address", err.Error())
	}
	if len(email.calls) != 0 {
		t.Errorf("sent %d mails to an empty recipient, want 0", len(email.calls))
	}
}

// The dry-run renders through the SAME function as the live action (RenderEmail), so "Chạy thử"
// can never show an address the real send would not use.
func TestRenderEmail_IsTheLiveRenderPath(t *testing.T) {
	res := &fakeTemplateResolver{values: map[string]any{"khach_hang.email": "khach@dienlanh.vn"}}
	a := Action{Type: ActionEmailAlert, To: " {{khach_hang.email}} ", Subject: "Phiếu {{ma}}", Body: "b"}
	got, err := RenderEmail(context.Background(), Event{Data: map[string]any{"ma": "PH-01"}}, a, res)
	if err != nil {
		t.Fatalf("RenderEmail: %v", err)
	}
	if got.To != "khach@dienlanh.vn" || got.Subject != "Phiếu PH-01" || got.Body != "b" {
		t.Errorf("RenderEmail = %+v", got)
	}
	// No resolver wired (legacy wiring / unit fakes): old behaviour — unknown placeholders render "".
	got, err = RenderEmail(context.Background(), Event{Data: map[string]any{}}, a, nil)
	if err != nil || got.To != "" {
		t.Errorf("nil resolver: got %+v, %v; want empty To, nil error", got, err)
	}
}

func TestTemplatePlaceholders(t *testing.T) {
	got := TemplatePlaceholders("{{a}} {{ b.c }} {{a}}", "x {{d-e}}", "")
	if want := []string{"a", "b.c", "d-e"}; !slices.Equal(got, want) {
		t.Errorf("TemplatePlaceholders = %v; want %v (in order, deduplicated)", got, want)
	}
	if got := TemplatePlaceholders("no braces"); len(got) != 0 {
		t.Errorf("no placeholder → %v; want none", got)
	}
}

// The dispatcher hands its wired resolver to the action (WithTemplateResolver) — the engine seam a
// production wiring can forget. Driven through Emit, not runEmailAlert directly.
func TestDispatcher_EmailAlertUsesTheWiredTemplateResolver(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "lich_hen", Trigger: TriggerAfterCreate, IsActive: true,
		Actions: []Action{{Type: ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "s", Body: "b"}},
	}}}
	email := &fakeEmailSender{}
	res := &fakeTemplateResolver{values: map[string]any{"benh_nhan.email": "lan@nhakhoa.vn"}}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, &fakeMutator{}, nil, nil).
		WithEmailSender(email).
		WithTemplateResolver(res)
	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "lich_hen", RecordID: "a-1", Trigger: TriggerAfterCreate,
		Data: map[string]any{"benh_nhan": "p-1"},
	})
	drainDispatcher(t, d)
	if len(email.calls) != 1 || !strings.EqualFold(email.calls[0].to, "lan@nhakhoa.vn") {
		t.Fatalf("sent %+v; want one mail to the linked patient", email.calls)
	}
}
