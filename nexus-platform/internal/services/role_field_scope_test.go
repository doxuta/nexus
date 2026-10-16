package services

import (
	"context"
	"errors"
	"testing"
)

// #283 rowscope-by-user-field — a grant at access_level "field:<name>" is accepted only when <name>
// is a live user-type field of the grant's own entity (specs/071-rowscope-by-user-field §3.2).

// fakeUserFields answers UserFieldExists from a (tenant, entity) → field set map.
type fakeUserFields struct {
	has map[string]bool // key: tenant\x00entity\x00field
	err error
}

func (f fakeUserFields) UserFieldExists(_ context.Context, tenantID, entity, field string) (bool, error) {
	return f.has[tenantID+"\x00"+entity+"\x00"+field], f.err
}

func TestRole_FieldLevelGrant(t *testing.T) {
	good := fakeUserFields{has: map[string]bool{"t1\x00nk_lich_hen\x00bac_si": true}}
	cases := []struct {
		name     string
		checker  userFieldChecker
		grant    RoleActionInput
		wantCode string // "" = accepted
	}{
		{"user field of the entity is accepted", good,
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:bac_si"}, ""},
		{"field that is not a user field of the entity", good,
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:ghi_chu"}, "ROLE_FIELD_SCOPE_INVALID"},
		{"another entity's user field does not count", good,
			RoleActionInput{EntityName: "nk_benh_nhan", Action: "view", AccessLevel: "field:bac_si"}, "ROLE_FIELD_SCOPE_INVALID"},
		{"wildcard entity cannot carry a per-entity field", good,
			RoleActionInput{EntityName: "*", Action: "view", AccessLevel: "field:bac_si"}, "ROLE_FIELD_SCOPE_INVALID"},
		{"malformed field name", good,
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:Bac Si"}, "ROLE_ACCESS_LEVEL_INVALID"},
		{"empty field name", good,
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:"}, "ROLE_ACCESS_LEVEL_INVALID"},
		{"checker not wired fails closed", nil,
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:bac_si"}, "ROLE_FIELD_SCOPE_INVALID"},
		{"checker error is surfaced, never accepted", fakeUserFields{err: errors.New("db down")},
			RoleActionInput{EntityName: "nk_lich_hen", Action: "view", AccessLevel: "field:bac_si"}, "ERR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, repo := newRoleSvc()
			if c.checker != nil {
				svc.WithUserFieldChecker(c.checker)
			}
			_, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
				Name: "bac_si", Label: "Bác sĩ", Actions: []RoleActionInput{c.grant},
			})
			switch c.wantCode {
			case "":
				if err != nil {
					t.Fatalf("err = %v; want accepted", err)
				}
				stored := 0
				for _, acts := range repo.actions {
					for _, a := range acts {
						if a.AccessLevel != "field:bac_si" || a.EntityName != "nk_lich_hen" {
							t.Fatalf("stored grant = %+v", a)
						}
						stored++
					}
				}
				if stored != 1 {
					t.Fatalf("stored %d grants; want 1", stored)
				}
			case "ERR":
				var ve *ValidationError
				if err == nil || errors.As(err, &ve) {
					t.Fatalf("err = %v; want a plain (500) error", err)
				}
			default:
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != c.wantCode {
					t.Fatalf("err = %v; want %s", err, c.wantCode)
				}
			}
		})
	}
}

func TestRole_UpdateRevalidatesFieldLevel(t *testing.T) {
	svc, _ := newRoleSvc()
	svc.WithUserFieldChecker(fakeUserFields{has: map[string]bool{"t1\x00nk_lich_hen\x00bac_si": true}})
	role, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{Name: "r", Label: "R"})
	if err != nil {
		t.Fatal(err)
	}
	bad := []RoleActionInput{{EntityName: "nk_lich_hen", Action: "edit", AccessLevel: "field:khong_co"}}
	_, err = svc.UpdateRole(context.Background(), "t1", role.ID, UpdateRoleInput{Actions: bad})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_FIELD_SCOPE_INVALID" {
		t.Fatalf("err = %v; want ROLE_FIELD_SCOPE_INVALID", err)
	}
}
