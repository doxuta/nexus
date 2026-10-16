package shares

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type fakeGuard struct{ err error }

func (g fakeGuard) ResolveShareTarget(context.Context, services.RecordScope, string, string, string) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	return "crm_contracts", nil
}

type fakeUsers map[string]bool

func (u fakeUsers) BatchActiveUserExists(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if u[id] {
			out[id] = true
		}
	}
	return out, nil
}

type fakeStore struct {
	upserts []string // "entity|record|user|sharedBy"
	revoked []string
	live    bool
	team    bool // Revoke reports the revoked share was a team's
	// unchanged: the grantee already held a live share, so Upsert changes nothing (changed=false).
	unchanged bool
}

func (s *fakeStore) Upsert(_ context.Context, _, _, entity, recordID, userID, teamID, sharedBy string) (*Share, bool, error) {
	g := userID
	if teamID != "" {
		g = "team:" + teamID
	}
	s.upserts = append(s.upserts, entity+"|"+recordID+"|"+g+"|"+sharedBy)
	return &Share{EntityName: entity, RecordID: recordID, UserID: userID, TeamID: teamID, SharedBy: sharedBy}, !s.unchanged, nil
}
func (s *fakeStore) ListByRecord(context.Context, string, string, string) ([]Share, error) {
	return []Share{}, nil
}
func (s *fakeStore) Revoke(_ context.Context, _, entity, recordID, granteeID string) (bool, error) {
	if !s.live {
		return false, ErrShareNotFound
	}
	s.revoked = append(s.revoked, entity+"|"+recordID+"|"+granteeID)
	return s.team, nil
}

// fakeTeams: the ids that exist; anything else is auth.ErrTeamNotFound (what the tenant-pinned lookup
// returns for an unknown or foreign team alike).
type fakeTeams map[string]bool

func (t fakeTeams) GetTeamByID(_ context.Context, _, id string) (*auth.Team, error) {
	if !t[id] {
		return nil, auth.ErrTeamNotFound
	}
	return &auth.Team{ID: id}, nil
}

type captureAudit struct{ events []audit.Event }

func (a *captureAudit) Record(_ context.Context, e audit.Event) { a.events = append(a.events, e) }

// TestService_Create: the gate runs first, the grantee must be another active user of the tenant,
// sharedBy is always the verified caller, and every grant leaves an audit row.
func TestService_Create(t *testing.T) {
	caller := services.RecordScope{UserID: "u-a", AccessLevel: "owner"}
	cases := []struct {
		name     string
		guardErr error
		userID   string
		wantCode string
		wantErr  error
	}{
		{name: "active colleague → stored + audited", userID: "u-b"},
		{name: "record gate refuses → that error, nothing stored", guardErr: services.ErrRecordSharedReadOnly, userID: "u-b", wantErr: services.ErrRecordSharedReadOnly},
		{name: "self", userID: "u-a", wantCode: "RECORD_SHARE_SELF"},
		{name: "blank", userID: "  ", wantCode: "RECORD_SHARE_USER_INVALID"},
		{name: "not an active user of the tenant", userID: "u-ghost", wantCode: "RECORD_SHARE_USER_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, au := &fakeStore{}, &captureAudit{}
			svc := &Service{repo: st, guard: fakeGuard{err: tc.guardErr}, users: fakeUsers{"u-a": true, "u-b": true}, auditor: au, newID: func() string { return "id-1" }}
			_, err := svc.Create(context.Background(), caller, "t1", "Crm_Contracts", "rec-1", tc.userID)
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
					t.Fatalf("Create: %v", err)
				}
				if len(st.upserts) != 1 || st.upserts[0] != "crm_contracts|rec-1|u-b|u-a" {
					t.Fatalf("upserts = %v; want the canonical entity + caller as sharer", st.upserts)
				}
				if len(au.events) != 1 || au.events[0].Action != audit.ActionShare || au.events[0].Diff.After["sharedWith"] != "u-b" {
					t.Fatalf("audit = %+v", au.events)
				}
				return
			}
			if len(st.upserts) != 0 || len(au.events) != 0 {
				t.Fatalf("refused share still wrote: upserts=%v audit=%v", st.upserts, au.events)
			}
		})
	}
}

func TestService_Delete(t *testing.T) {
	caller := services.RecordScope{UserID: "u-m", AccessLevel: "all"}
	t.Run("live share → revoked + audited", func(t *testing.T) {
		st, au := &fakeStore{live: true}, &captureAudit{}
		svc := &Service{repo: st, guard: fakeGuard{}, auditor: au}
		if err := svc.Delete(context.Background(), caller, "t1", "crm_contracts", "rec-1", " u-b "); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(st.revoked) != 1 || st.revoked[0] != "crm_contracts|rec-1|u-b" || len(au.events) != 1 || au.events[0].Action != audit.ActionUnshare {
			t.Fatalf("revoked=%v audit=%+v", st.revoked, au.events)
		}
	})
	t.Run("no live share → ErrShareNotFound, no audit", func(t *testing.T) {
		st, au := &fakeStore{}, &captureAudit{}
		svc := &Service{repo: st, guard: fakeGuard{}, auditor: au}
		if err := svc.Delete(context.Background(), caller, "t1", "crm_contracts", "rec-1", "u-b"); !errors.Is(err, ErrShareNotFound) {
			t.Fatalf("err = %v; want ErrShareNotFound", err)
		}
		if len(au.events) != 0 {
			t.Fatalf("audit on a failed revoke: %v", au.events)
		}
	})
}

// TestService_Create_ReshareWritesNoSecondAuditRow pins DAILY-QUEUE #226 point 1: sharing again with
// someone who already holds a live share changes nothing in the table, so the History tab must not
// get a second "share" line for it. The caller still gets the share back (201, idempotent).
func TestService_Create_ReshareWritesNoSecondAuditRow(t *testing.T) {
	st, au := &fakeStore{unchanged: true}, &captureAudit{}
	svc := &Service{repo: st, guard: fakeGuard{}, users: fakeUsers{"u-b": true}, auditor: au, newID: func() string { return "id-1" }}
	sh, err := svc.Create(context.Background(), services.RecordScope{UserID: "u-a", AccessLevel: "owner"}, "t1", "crm_contracts", "rec-1", "u-b")
	if err != nil || sh == nil || sh.UserID != "u-b" {
		t.Fatalf("Create = (%+v, %v); want the existing share back", sh, err)
	}
	if len(au.events) != 0 {
		t.Fatalf("a no-op re-share wrote audit rows: %+v", au.events)
	}
}
