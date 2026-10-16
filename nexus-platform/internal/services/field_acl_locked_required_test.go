package services

// queue #62 (field-acl-locked-required-deadend) — the RUNTIME half. PutGrants refuses to lock a
// required field without a working default, but that guard cannot reach every path to the same
// state: rows saved before it existed, a field made required (or its default cleared) AFTER the
// lock. For such a role every create fails — and it used to fail as RECORD_REQUIRED_MISSING, i.e.
// "fill in X", on a form (LayoutForm renders X as text) or a grid ("Tạo nhiều dòng" drops X's
// column since #13) that offers no way to fill X. The error must say what is actually wrong.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
)

// countingFLSLoader counts grant loads and can start failing after the first N (the write guard
// runs first on every create, so "fail after 1" isolates the explainer's own load).
type countingFLSLoader struct {
	rows      []auth.FieldACL
	calls     int
	failAfter int // 0 = never fail
}

func (l *countingFLSLoader) LoadFieldACLForEntity(_ context.Context, _, _, _ string) ([]auth.FieldACL, error) {
	l.calls++
	if l.failAfter > 0 && l.calls > l.failAfter {
		return nil, errors.New("db down")
	}
	return l.rows, nil
}

func TestCreate_RequiredFieldLockedForRole_SaysLockedNotMissing(t *testing.T) {
	tests := []struct {
		name      string
		rows      []auth.FieldACL
		scope     RecordScope
		failAfter int
		wantCode  string
		wantField string // "" = the error must not name the field at all
		wantMsg   string // a way out the reader can reach, when the case has one
	}{
		{name: "visible but locked → RECORD_REQUIRED_LOCKED", rows: []auth.FieldACL{lock("name")}, scope: bulkRoleScope(), wantCode: "RECORD_REQUIRED_LOCKED", wantField: "name"},
		// queue #143 — anti-oracle (api-conventions §4.4b): the caller may not see this field, so the
		// error must not name it (details.field / message). The create still cannot succeed, so the
		// code says "locked" and sends the admin to the screen that shows it: the customer admin
		// manages /admin/field-acl, where a hidden required cell carries a warning. Doctor C9 names it
		// too, but Doctor is operator-only (RequirePlatformOperator) — advice the reader's admin
		// cannot follow is no way out.
		{name: "hidden → RECORD_REQUIRED_LOCKED without the field's name", rows: []auth.FieldACL{hide("name")}, scope: bulkRoleScope(), wantCode: "RECORD_REQUIRED_LOCKED", wantMsg: "Phân quyền Field"},
		{name: "no restriction → real omission", rows: nil, scope: bulkRoleScope(), wantCode: "RECORD_REQUIRED_MISSING", wantField: "name"},
		{name: "admin bypasses locks → real omission", rows: []auth.FieldACL{hide("name")}, scope: bulkAdminScope(), wantCode: "RECORD_REQUIRED_MISSING", wantField: "name"},
		// The explanation is best-effort, the secrecy is not: an unverifiable lock set never produces
		// a lock claim, and never lets the name out either (the field might be hidden — fail closed).
		{name: "lock load fails → RECORD_REQUIRED_MISSING without the name", rows: []auth.FieldACL{hide("name")}, scope: bulkRoleScope(), failAfter: 1, wantCode: "RECORD_REQUIRED_MISSING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := &countingFLSLoader{rows: tt.rows, failAfter: tt.failAfter}
			store := &bulkTestStore{}
			svc := newBulkSvc(store, &fakeSequenceClaimer{}, &fakeBulkInserter{}, NewFieldACLService(loader), t)
			_, err := svc.Create(context.Background(), tt.scope, "tenant-1", "u1", "widgets", map[string]any{"email": "a@b.vn"})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
			}
			if ve.Field != tt.wantField {
				t.Fatalf("field = %q; want %q", ve.Field, tt.wantField)
			}
			if tt.wantField == "" {
				assertNamesNoField(t, ve, "name")
			}
			if !strings.Contains(ve.Msg, tt.wantMsg) {
				t.Fatalf("msg %q; want it to point at %q", ve.Msg, tt.wantMsg)
			}
			if strings.Contains(ve.Msg, "Doctor") {
				t.Fatalf("msg %q sends the admin to Doctor, which only a platform operator can open", ve.Msg)
			}
			if len(store.inserted) != 0 {
				t.Fatalf("nothing may be written: %+v", store.inserted)
			}
		})
	}
}

// assertNamesNoField fails when a validation error discloses field anywhere the client reads it.
func assertNamesNoField(t *testing.T, ve *ValidationError, field string) {
	t.Helper()
	if _, ok := ve.Details["field"]; ok {
		t.Fatalf("details = %v; must not carry a field", ve.Details)
	}
	if strings.Contains(ve.Msg, strconv.Quote(field)) {
		t.Fatalf("msg %q names the hidden field %q", ve.Msg, field)
	}
}

// queue #143: "Tạo nhiều dòng" answers per row — every row of a hidden required field must stay
// nameless, with the grant set still loaded once per batch.
func TestCreateBulk_RequiredFieldHiddenForRole_NoRowNamesIt(t *testing.T) {
	loader := &countingFLSLoader{rows: []auth.FieldACL{hide("name")}}
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, NewFieldACLService(loader), t)
	payloads := []map[string]any{{"email": "a@b.vn"}, {"email": "c@d.vn"}}
	res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	for i, r := range res.Results {
		if r.Status != "failed" || r.Code != "RECORD_REQUIRED_LOCKED" {
			t.Fatalf("row %d = %+v; want failed RECORD_REQUIRED_LOCKED", i, r)
		}
		if _, ok := r.Details["field"]; ok || strings.Contains(r.Error, strconv.Quote("name")) {
			t.Fatalf("row %d = %+v names the hidden field", i, r)
		}
	}
	if want := len(payloads) + 1; loader.calls != want {
		t.Fatalf("grant loads = %d; want %d", loader.calls, want)
	}
}

// The bulk grid is where #13 made this bite: every row fails the same way. Each row must carry the
// explanation, and the lock set must be read ONCE for the batch, not once per failing row.
func TestCreateBulk_RequiredFieldLockedForRole_EveryRowSaysLocked_OneExtraLoad(t *testing.T) {
	loader := &countingFLSLoader{rows: []auth.FieldACL{lock("name")}}
	svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, NewFieldACLService(loader), t)
	payloads := []map[string]any{{"email": "a@b.vn"}, {"email": "c@d.vn"}, {"email": "e@f.vn"}}
	res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	for i, r := range res.Results {
		if r.Status != "failed" || r.Code != "RECORD_REQUIRED_LOCKED" || r.Details["field"] != "name" {
			t.Fatalf("row %d = %+v; want failed RECORD_REQUIRED_LOCKED on field name", i, r)
		}
	}
	// One load per row for the write guard (CheckWriteFields, unchanged) + ONE for the explainer.
	if want := len(payloads) + 1; loader.calls != want {
		t.Fatalf("grant loads = %d; want %d (explainer must load once per batch)", loader.calls, want)
	}
}
