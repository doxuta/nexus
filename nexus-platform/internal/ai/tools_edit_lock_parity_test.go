package ai

// Queue #319: propose_edit_field must see sys_field_acl locks, so a card that turns "bắt buộc" ON
// for a locked field fails at PROPOSE time with the apply path's own FIELD_REQUIRED_WHILE_LOCKED.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeFieldLocks stands in for *auth.FieldACLRepository.ListFieldRestrictions.
type fakeFieldLocks struct {
	rows                           []auth.FieldACLRestriction
	err                            error
	gotTenant, gotEntity, gotField string
}

func (f *fakeFieldLocks) ListFieldRestrictions(_ context.Context, tenantID, entity, field string) ([]auth.FieldACLRestriction, error) {
	f.gotTenant, f.gotEntity, f.gotField = tenantID, entity, field
	return f.rows, f.err
}

func TestTools_ProposeEditField_RequiredWhileLocked(t *testing.T) {
	lock := auth.FieldACLRestriction{RoleID: "r1", RoleName: "crm_sales_rep", RoleLabel: "Nhân viên kinh doanh", CanView: true}
	tests := []struct {
		name      string
		locks     *fakeFieldLocks // nil = seam not wired (plain reader)
		args      string
		wantCode  string // "" = proposal buffered
		wantInMsg string
		wantRead  bool // the lock table had to be read
	}{
		{"required on locked field refused", &fakeFieldLocks{rows: []auth.FieldACLRestriction{lock}},
			`{"entity":"patients","field":"age","changes":{"isRequired":true}}`, "FIELD_REQUIRED_WHILE_LOCKED", "Nhân viên kinh doanh", true},
		{"default in same edit fills it", &fakeFieldLocks{rows: []auth.FieldACLRestriction{lock}},
			`{"entity":"patients","field":"age","changes":{"isRequired":true,"defaultValue":"18"}}`, "", "", false},
		{"no lock rows passes", &fakeFieldLocks{},
			`{"entity":"patients","field":"age","changes":{"isRequired":true}}`, "", "", true},
		{"label-only edit never reads locks", &fakeFieldLocks{rows: []auth.FieldACLRestriction{lock}},
			`{"entity":"patients","field":"age","changes":{"label":"Tuổi mới"}}`, "", "", false},
		{"lock read failure fails closed", &fakeFieldLocks{err: errors.New("db down")},
			`{"entity":"patients","field":"age","changes":{"isRequired":true}}`, "VALIDATION_FAILED", "db down", true},
		{"seam not wired keeps old behaviour", nil,
			`{"entity":"patients","field":"age","changes":{"isRequired":true}}`, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reader MetadataReader = editFixtureReader()
			if tt.locks != nil {
				reader = ReaderWithAccessProbes(reader, &AccessProbes{FieldLocks: tt.locks})
			}
			e := &toolExecutor{reader: reader, tenantID: "tenant-A"}
			raw := e.execute(context.Background(), "propose_edit_field", tt.args)
			ok, m := parseToolResult(t, raw)
			if tt.locks != nil && (tt.locks.gotField != "") != tt.wantRead {
				t.Fatalf("lock table read = %v, want %v", tt.locks.gotField != "", tt.wantRead)
			}
			if tt.wantRead && tt.locks != nil && (tt.locks.gotTenant != "tenant-A" || tt.locks.gotEntity != "patients" || tt.locks.gotField != "age") {
				t.Fatalf("lock read must be pinned to tenant/entity/field, got %q/%q/%q", tt.locks.gotTenant, tt.locks.gotEntity, tt.locks.gotField)
			}
			if tt.wantCode == "" {
				if !ok || len(e.draft) != 1 {
					t.Fatalf("want buffered proposal, ok=%v draft=%d m=%+v", ok, len(e.draft), m)
				}
				return
			}
			if ok || len(e.draft) != 0 || !strings.Contains(raw, tt.wantCode) || !strings.Contains(raw, tt.wantInMsg) {
				t.Fatalf("want %s containing %q and no draft, ok=%v draft=%d raw=%s", tt.wantCode, tt.wantInMsg, ok, len(e.draft), raw)
			}
		})
	}
}
