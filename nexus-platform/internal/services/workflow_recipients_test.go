package services

import (
	"context"
	"testing"

	"nexus/platform/internal/auth"
)

// #281: the recipient of an in_app_notification is validated when the rule is saved.
func TestValidateWorkflowConfig_NotificationRecipient(t *testing.T) {
	ft := map[string]string{"bac_si": "user", "ten": "text"}
	tests := []struct {
		name    string
		userID  string
		wantErr bool
	}{
		{"owner", "$owner", false},
		{"fixed user", "5a1c1f0e-0000-4000-8000-000000000001", false},
		{"user field", "$field:bac_si", false},
		{"text field is not a person", "$field:ten", true},
		{"unknown field", "$field:nope", true},
		{"role", "$role:abc", false},
		{"team", "$team:abc", false},
		{"role without id", "$role:", true},
		{"unknown sentinel", "$everyone", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("lich_hen", "after_create", nil,
				[]WorkflowActionInput{{Type: "in_app_notification", UserID: tc.userID, Message: "m"}}, ft)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

type fakeTeamChecker struct{ known map[string]bool }

func (f fakeTeamChecker) GetTeamByID(_ context.Context, _, id string) (*auth.Team, error) {
	if f.known[id] {
		return &auth.Team{ID: id}, nil
	}
	return nil, auth.ErrTeamNotFound
}

// #281: a "$team:<id>" notification recipient gets the same live-team check as an assign's team.
func TestCheckAssignTeams_NotificationTeamRecipient(t *testing.T) {
	s := &WorkflowService{teams: fakeTeamChecker{known: map[string]bool{"t1": true}}}
	tests := []struct {
		name    string
		a       WorkflowActionInput
		wantErr bool
	}{
		{"live team", WorkflowActionInput{Type: "in_app_notification", UserID: "$team:t1"}, false},
		{"unknown team", WorkflowActionInput{Type: "in_app_notification", UserID: "$team:zz"}, true},
		{"role is not checked here", WorkflowActionInput{Type: "in_app_notification", UserID: "$role:zz"}, false},
		{"assign unknown team", WorkflowActionInput{Type: "assign", Team: "zz"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.checkAssignTeams(context.Background(), "ten", []WorkflowActionInput{tc.a})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
