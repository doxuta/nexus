package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// specs/065 lát C (T056): CreateStaff + AssertManageable.

type capAuditor struct{ events []audit.Event }

func (c *capAuditor) Record(_ context.Context, e audit.Event) { c.events = append(c.events, e) }

func TestCreateStaff(t *testing.T) {
	role := func(s string) *string { return &s }
	cases := []struct {
		name    string
		actor   string
		in      OrgCreateInput
		wantErr string // ValidationError code, "forbidden" or ""
	}{
		{"happy", "owner1", OrgCreateInput{Email: " New@Acme.test ", FullName: "Mới", RoleID: role("r-sales")}, ""},
		{"no role ok", "owner1", OrgCreateInput{Email: "n2@acme.test"}, ""},
		{"empty email", "owner1", OrgCreateInput{Email: "  "}, "USER_FIELD_REQUIRED"},
		{"bad email", "owner1", OrgCreateInput{Email: "not-an-email"}, "USER_EMAIL_INVALID"},
		{"display-name email", "owner1", OrgCreateInput{Email: "A <a@b.test>"}, "USER_EMAIL_INVALID"},
		{"duplicate", "owner1", OrgCreateInput{Email: "staff1@acme.test"}, "USER_ALREADY_EXISTS"},
		{"owner role", "owner1", OrgCreateInput{Email: "n3@acme.test", RoleID: role("r-owner")}, "ROLE_NOT_ASSIGNABLE"},
		{"other tenant role", "owner1", OrgCreateInput{Email: "n4@acme.test", RoleID: role("r-other")}, "ROLE_INVALID"},
		{"actor is staff", "staff1", OrgCreateInput{Email: "n5@acme.test"}, "forbidden"},
		{"actor is admin", "admin1", OrgCreateInput{Email: "n6@acme.test"}, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnerFixture(t)
			aud := &capAuditor{}
			svc := f.svc.WithAuditor(aud)
			v, err := svc.CreateStaff(audit.WithActor(context.Background(), tc.actor), "t1", tc.actor, tc.in)
			switch tc.wantErr {
			case "":
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				row := f.repo.rows[f.repo.key("t1", v.ID)]
				if row == nil || row.IsAdmin || row.CanManageUsers || row.IsPlatformOperator || row.Status != "active" {
					t.Fatalf("row = %+v", row)
				}
				if row.PasswordHash == "" || (row.Email != "New@Acme.test" && row.Email != "n2@acme.test") {
					t.Fatalf("hash/email = %q/%q", row.PasswordHash, row.Email)
				}
				if v.Tier != "staff" || !v.Manageable {
					t.Errorf("view = %+v", v)
				}
				raw, _ := json.Marshal(v)
				if strings.Contains(string(raw), row.PasswordHash) || strings.Contains(strings.ToLower(string(raw)), "password") {
					t.Errorf("view leaks password: %s", raw)
				}
				if len(aud.events) != 1 || aud.events[0].Action != audit.ActionCreate || aud.events[0].ActorUserID != tc.actor || aud.events[0].RecordID != v.ID {
					t.Fatalf("audit = %+v", aud.events)
				}
				ev, _ := json.Marshal(aud.events[0])
				if strings.Contains(string(ev), row.PasswordHash) {
					t.Errorf("audit leaks hash: %s", ev)
				}
			case "forbidden":
				if !errors.Is(err, ErrUserForbidden) {
					t.Fatalf("err = %v", err)
				}
			default:
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantErr {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
			}
			if tc.wantErr != "" && len(aud.events) != 0 {
				t.Errorf("failed create audited: %+v", aud.events)
			}
		})
	}
}

func TestAssertManageable(t *testing.T) {
	cases := []struct {
		name, actor, target string
		want                error
		wantCode            string
	}{
		{"staff ok", "owner1", "staff1", nil, ""},
		{"admin", "owner1", "admin1", auth.ErrUserNotManageable, ""},
		{"operator", "owner1", "op1", auth.ErrUserNotManageable, ""},
		{"other owner", "owner1", "owner2", auth.ErrUserNotManageable, ""},
		{"self", "owner1", "owner1", auth.ErrUserNotManageable, ""},
		{"other tenant id", "owner1", "x1", nil, "USER_NOT_FOUND"},
		{"actor not owner", "staff1", "staff1", ErrUserForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnerFixture(t)
			err := f.svc.AssertManageable(context.Background(), "t1", tc.actor, tc.target)
			if tc.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
