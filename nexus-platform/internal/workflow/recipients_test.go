package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeRecipients struct {
	roles, teams map[string][]string
	err          error
}

func (f fakeRecipients) RoleMembers(_ context.Context, tenantID, id string) ([]string, error) {
	return f.roles[tenantID+"/"+id], f.err
}
func (f fakeRecipients) TeamMembers(_ context.Context, tenantID, id string) ([]string, error) {
	return f.teams[tenantID+"/"+id], f.err
}

func TestResolveRecipients(t *testing.T) {
	r := fakeRecipients{
		roles: map[string][]string{"t1/lt": {"u1", "u2", "u1"}},
		teams: map[string][]string{"t1/tm": {"u3"}},
	}
	evt := Event{TenantID: "t1", OwnerUserID: "own", Data: map[string]any{"doctor": "dr1", "blank": ""}}
	tests := []struct {
		name, spec string
		res        RecipientResolver
		want       []string
		wantErr    bool
	}{
		{"owner", "$owner", r, []string{"own"}, false},
		{"fixed user", "u9", r, []string{"u9"}, false},
		{"field", "$field:doctor", r, []string{"dr1"}, false},
		{"field blank", "$field:blank", r, []string{}, false},
		{"field absent", "$field:nope", r, []string{}, false},
		{"role deduped", "$role:lt", r, []string{"u1", "u2"}, false},
		{"team", "$team:tm", r, []string{"u3"}, false},
		{"role of another tenant id yields nobody", "$role:other", r, []string{}, false},
		{"role without resolver", "$role:lt", nil, nil, true},
		{"resolver error", "$team:tm", fakeRecipients{err: errors.New("db down")}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRecipients(context.Background(), evt, tc.spec, tc.res)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateRecipientSpec(t *testing.T) {
	for spec, ok := range map[string]bool{
		"$owner": true, "u1": true, "$field:doctor": true, "$role:x": true, "$team:x": true,
		"": false, "$field:": false, "$role: ": false, "$bogus": false,
	} {
		if err := ValidateRecipientSpec(spec); (err == nil) != ok {
			t.Errorf("ValidateRecipientSpec(%q) = %v, want ok=%v", spec, err, ok)
		}
	}
}

// #280: the title/message of a notification are rendered ({{field}} filled), not stored literally.
func TestRunInAppNotification_RendersTemplate(t *testing.T) {
	n := &fakeNotif{}
	evt := Event{TenantID: "t1", EntityName: "phieu", RecordID: "r1", OwnerUserID: "own", Data: map[string]any{"code": "PSC-00001"}}
	a := Action{Type: ActionInAppNotification, UserID: "$owner", Title: "Phiếu {{code}}", Message: "Bạn có phiếu {{code}} ({{missing}})"}
	if err := runInAppNotification(context.Background(), Workflow{ID: "w"}, evt, a, n, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(n.rows) != 1 || n.rows[0].Title != "Phiếu PSC-00001" || n.rows[0].Message != "Bạn có phiếu PSC-00001 ()" {
		t.Fatalf("rows = %+v", n.rows)
	}
}

// #281: a role fans out to every member; an empty user field is a named failure, not a dead row.
func TestRunInAppNotification_FanOutAndEmpty(t *testing.T) {
	n := &fakeNotif{}
	r := fakeRecipients{roles: map[string][]string{"t1/lt": {"u1", "u2"}}}
	evt := Event{TenantID: "t1", Data: map[string]any{}}
	a := Action{Type: ActionInAppNotification, UserID: "$role:lt", Message: "m"}
	if err := runInAppNotification(context.Background(), Workflow{}, evt, a, n, nil, r); err != nil {
		t.Fatal(err)
	}
	if len(n.rows) != 2 || n.rows[0].UserID != "u1" || n.rows[1].UserID != "u2" {
		t.Fatalf("rows = %+v", n.rows)
	}
	a.UserID = "$field:doctor"
	if err := runInAppNotification(context.Background(), Workflow{}, evt, a, n, nil, r); err == nil {
		t.Fatal("expected an error for an empty user field")
	}
	if len(n.rows) != 2 {
		t.Fatalf("a dead row was written: %+v", n.rows)
	}
}

// #280: "Chia việc rồi Thông báo $owner" tells the person who just got the work, not the creator.
func TestRunActions_OwnerAfterAssignIsAssignee(t *testing.T) {
	n := &fakeNotif{}
	mut := &fakeMutator{}
	wf := Workflow{ID: "w", TenantID: "t1", Actions: []Action{
		{Type: ActionAssign, Strategy: StrategyRoundRobin, Team: "tm", Field: "ktv"},
		{Type: ActionInAppNotification, UserID: "$owner", Message: "Việc {{ktv}}"},
		{Type: ActionInAppNotification, UserID: "$field:ktv", Message: "x"},
	}}
	evt := Event{TenantID: "t1", EntityName: "phieu", RecordID: "r1", Trigger: TriggerAfterCreate, OwnerUserID: "coordinator", Data: map[string]any{"code": "A"}}
	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut, notif: n, assign: &fakeResolver{seq: []string{"ktv1"}}}); err != nil {
		t.Fatal(err)
	}
	if len(n.rows) != 2 || n.rows[0].UserID != "ktv1" || n.rows[1].UserID != "ktv1" || n.rows[0].Message != "Việc ktv1" {
		t.Fatalf("rows = %+v", n.rows)
	}
	if evt.OwnerUserID != "coordinator" || evt.Data["ktv"] != nil {
		t.Fatal("the shared event was mutated")
	}
}
