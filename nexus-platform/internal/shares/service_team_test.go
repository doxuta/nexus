package shares

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/services"
)

// TestService_CreateTeam (DAILY-QUEUE #224): the same gate as a person share, then the team must be a
// live team of the tenant; the grant is stored under the team and audited as sharedWithTeam.
func TestService_CreateTeam(t *testing.T) {
	caller := services.RecordScope{UserID: "u-a", AccessLevel: "owner"}
	cases := []struct {
		name     string
		guardErr error
		teamID   string
		wantCode string
		wantErr  error
	}{
		{name: "live team: stored and audited as a team share", teamID: "team-x"},
		{name: "record gate refuses: that error, nothing stored", guardErr: services.ErrRecordSharedReadOnly, teamID: "team-x", wantErr: services.ErrRecordSharedReadOnly},
		{name: "unknown or foreign team", teamID: "team-ghost", wantCode: "RECORD_SHARE_TEAM_INVALID"},
		{name: "blank", teamID: "  ", wantCode: "RECORD_SHARE_TEAM_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, au := &fakeStore{}, &captureAudit{}
			svc := (&Service{repo: st, guard: fakeGuard{err: tc.guardErr}, auditor: au, newID: func() string { return "id-1" }}).WithTeams(fakeTeams{"team-x": true})
			_, err := svc.CreateTeam(context.Background(), caller, "t1", "Crm_Contracts", "rec-1", tc.teamID)
			var ve *services.ValidationError
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v; want %v", err, tc.wantErr)
				}
			case tc.wantCode != "":
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v; want %s", err, tc.wantCode)
				}
			default:
				if err != nil {
					t.Fatalf("CreateTeam: %v", err)
				}
				if len(st.upserts) != 1 || st.upserts[0] != "crm_contracts|rec-1|team:team-x|u-a" {
					t.Fatalf("upserts = %v", st.upserts)
				}
				if len(au.events) != 1 || au.events[0].Action != audit.ActionShare || au.events[0].Diff.After["sharedWithTeam"] != "team-x" {
					t.Fatalf("audit = %+v", au.events)
				}
				return
			}
			if len(st.upserts) != 0 || len(au.events) != 0 {
				t.Fatalf("refused share still wrote: upserts=%v audit=%v", st.upserts, au.events)
			}
		})
	}
	t.Run("team probe not wired: loud error, not a silent allow", func(t *testing.T) {
		svc := &Service{repo: &fakeStore{}, guard: fakeGuard{}, auditor: &captureAudit{}}
		if _, err := svc.CreateTeam(context.Background(), caller, "t1", "crm_contracts", "rec-1", "team-x"); err == nil {
			t.Fatal("CreateTeam without a team probe succeeded")
		}
	})
}

// A revoke of a team share is audited as sharedWithTeam; a person's stays sharedWith.
func TestService_Delete_TeamShareAudit(t *testing.T) {
	st, au := &fakeStore{live: true, team: true}, &captureAudit{}
	svc := &Service{repo: st, guard: fakeGuard{}, auditor: au}
	if err := svc.Delete(context.Background(), services.RecordScope{UserID: "u-m", AccessLevel: "all"}, "t1", "crm_contracts", "rec-1", "team-x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(au.events) != 1 || au.events[0].Diff.Before["sharedWithTeam"] != "team-x" {
		t.Fatalf("audit = %+v", au.events)
	}
}
