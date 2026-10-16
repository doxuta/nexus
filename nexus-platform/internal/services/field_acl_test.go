package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeFieldACLLoader is an in-memory fieldACLLoader for unit-testing FieldACLService without a
// DB. It returns the stored grant rows for any (tenant, role, entity) lookup, or a forced error
// to exercise the fail-closed paths. A nil rows slice models the default-allow "no restriction".
type fakeFieldACLLoader struct {
	rows []auth.FieldACL
	err  error
}

func (f *fakeFieldACLLoader) LoadFieldACLForEntity(_ context.Context, _, _, _ string) ([]auth.FieldACL, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

const (
	flsTenant = "11111111-1111-1111-1111-111111111111"
	flsRole   = "22222222-2222-2222-2222-222222222222"
	flsEntity = "accounts"
)

// hide builds a can_view=0 (also implicitly un-editable) grant row for a field.
func hide(field string) auth.FieldACL {
	return auth.FieldACL{TenantID: flsTenant, RoleID: flsRole, EntityName: flsEntity, FieldName: field, CanView: false, CanEdit: false}
}

// lock builds a can_view=1 but can_edit=0 grant row (visible but not editable).
func lock(field string) auth.FieldACL {
	return auth.FieldACL{TenantID: flsTenant, RoleID: flsRole, EntityName: flsEntity, FieldName: field, CanView: true, CanEdit: false}
}

func newFLS(rows []auth.FieldACL, err error) *FieldACLService {
	return NewFieldACLService(&fakeFieldACLLoader{rows: rows, err: err})
}

// --- Read strip ----------------------------------------------------------------

func TestFLS_FilterReadFields_StripsHiddenField(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	data := map[string]any{"name": "Acme", "revenue": float64(9000)}
	if err := svc.FilterReadFields(context.Background(), flsTenant, flsRole, flsEntity, false, data); err != nil {
		t.Fatalf("FilterReadFields: %v", err)
	}
	if _, ok := data["revenue"]; ok {
		t.Fatalf("hidden field 'revenue' was NOT stripped: %v", data)
	}
	if v, ok := data["name"]; !ok || v != "Acme" {
		t.Fatalf("non-hidden field 'name' was wrongly stripped: %v", data)
	}
}

func TestFLS_FilterReadFields_AdminBypass(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	data := map[string]any{"name": "Acme", "revenue": float64(9000)}
	if err := svc.FilterReadFields(context.Background(), flsTenant, flsRole, flsEntity, true /*admin*/, data); err != nil {
		t.Fatalf("FilterReadFields admin: %v", err)
	}
	if _, ok := data["revenue"]; !ok {
		t.Fatalf("admin must see 'revenue'; it was stripped: %v", data)
	}
}

func TestFLS_FilterReadFields_DefaultAllowWhenNoRow(t *testing.T) {
	svc := newFLS(nil, nil) // no grant rows → default-allow
	data := map[string]any{"name": "Acme", "revenue": float64(9000)}
	if err := svc.FilterReadFields(context.Background(), flsTenant, flsRole, flsEntity, false, data); err != nil {
		t.Fatalf("FilterReadFields: %v", err)
	}
	if len(data) != 2 {
		t.Fatalf("default-allow must keep all fields, got: %v", data)
	}
}

func TestFLS_FilterReadFields_FailClosedDropsAll(t *testing.T) {
	svc := newFLS(nil, errors.New("db down"))
	data := map[string]any{"name": "Acme", "revenue": float64(9000)}
	err := svc.FilterReadFields(context.Background(), flsTenant, flsRole, flsEntity, false, data)
	if err == nil {
		t.Fatalf("expected error on loader failure")
	}
	if len(data) != 0 {
		t.Fatalf("fail-closed must drop ALL business fields, got: %v", data)
	}
}

func TestFLS_FilterReadFieldsBatch_StripsEveryRow(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	datas := []map[string]any{
		{"name": "Acme", "revenue": float64(1)},
		{"name": "Globex", "revenue": float64(2)},
	}
	if err := svc.FilterReadFieldsBatch(context.Background(), flsTenant, flsRole, flsEntity, false, datas); err != nil {
		t.Fatalf("batch: %v", err)
	}
	for i, d := range datas {
		if _, ok := d["revenue"]; ok {
			t.Fatalf("row %d still has hidden 'revenue': %v", i, d)
		}
	}
}

func TestFLS_FilterReadFieldsBatch_FailClosedDropsAll(t *testing.T) {
	svc := newFLS(nil, errors.New("boom"))
	datas := []map[string]any{{"name": "A", "revenue": float64(1)}}
	if err := svc.FilterReadFieldsBatch(context.Background(), flsTenant, flsRole, flsEntity, false, datas); err == nil {
		t.Fatalf("expected error")
	}
	if len(datas[0]) != 0 {
		t.Fatalf("fail-closed batch must drop all fields, got: %v", datas[0])
	}
}

// --- Write guard ---------------------------------------------------------------

func TestFLS_CheckWriteFields_RejectsLockedField(t *testing.T) {
	svc := newFLS([]auth.FieldACL{lock("revenue")}, nil)
	err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, false, map[string]any{"revenue": float64(5)})
	assertFieldForbidden(t, err)
}

func TestFLS_CheckWriteFields_HiddenIsAlsoLocked(t *testing.T) {
	// can_view=0 implies can_edit=0 (cannot edit what you cannot see) even though the row's
	// stored CanEdit is false anyway — verify a hidden field is rejected on write. Rejected as an
	// UNKNOWN field, not FIELD_FORBIDDEN: 403 would confirm the name exists (queue #317, §4.4b).
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, false, map[string]any{"revenue": float64(5)})
	assertUnknownFieldRefusal(t, err, "revenue")
}

// A visible-but-read-only key still wins over a hidden one: {hidden, locked} must read the same as
// {missing, locked}, which is the explicit 403 for the locked key (queue #317).
func TestFLS_CheckWriteFields_LockedVisibleOutranksHidden(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue"), lock("notes")}, nil)
	for i := 0; i < 50; i++ { // map iteration order is random; the answer must not depend on it
		err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, false,
			map[string]any{"revenue": float64(5), "notes": "x"})
		assertFieldForbidden(t, err)
	}
}

func TestFLS_CheckWriteFields_AllowsEditableField(t *testing.T) {
	svc := newFLS([]auth.FieldACL{lock("revenue")}, nil)
	if err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, false, map[string]any{"name": "Acme"}); err != nil {
		t.Fatalf("editable field 'name' must be allowed, got: %v", err)
	}
}

func TestFLS_CheckWriteFields_AdminBypass(t *testing.T) {
	svc := newFLS([]auth.FieldACL{lock("revenue")}, nil)
	if err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, true, map[string]any{"revenue": float64(5)}); err != nil {
		t.Fatalf("admin must bypass write guard, got: %v", err)
	}
}

func TestFLS_CheckWriteFields_FailClosedRejects(t *testing.T) {
	svc := newFLS(nil, errors.New("db down"))
	err := svc.CheckWriteFields(context.Background(), flsTenant, flsRole, flsEntity, false, map[string]any{"name": "Acme"})
	assertFieldForbidden(t, err)
}

// --- Update-path write guard: hidden vs locked, nil vs value -------------------

// The edit form seeds a HIDDEN field empty (the read strips it) and resends it as null on save.
// That null is the absence of a value the caller never saw — not an intent to clear it — so it
// is a no-op AND the key is REMOVED from the payload (the payload flows on into the UPDATE, where
// an explicit null removes the stored key). A VISIBLE-but-locked field is different: the caller
// saw the value, so a null against it IS a change and stays refused.
func TestFLS_CheckWriteFieldsAgainst_HiddenNilVsLockedNil(t *testing.T) {
	stored := func() map[string]any { return map[string]any{"name": "Acme", "revenue": float64(9000)} }
	cases := []struct {
		name    string
		rows    []auth.FieldACL
		payload map[string]any
		current map[string]any
		wantErr bool // refusal expected: FIELD_FORBIDDEN for a visible-locked key, unknown-field for a hidden one
		wantKey bool // "revenue" must still be a key of payload afterwards
	}{
		{
			name:    "hidden + nil against a stored value → no-op, key dropped",
			rows:    []auth.FieldACL{hide("revenue")},
			payload: map[string]any{"name": "Acme 2", "revenue": nil},
			current: stored(),
		},
		{
			name:    "hidden + nil with no current (record read failed) → still dropped, never written",
			rows:    []auth.FieldACL{hide("revenue")},
			payload: map[string]any{"name": "Acme 2", "revenue": nil},
			current: nil,
		},
		{
			name:    "hidden + value → FIELD_FORBIDDEN",
			rows:    []auth.FieldACL{hide("revenue")},
			payload: map[string]any{"revenue": float64(1)},
			current: stored(),
			wantErr: true, wantKey: true,
		},
		{
			name:    "hidden + value identical to stored → FIELD_FORBIDDEN (no equality oracle)",
			rows:    []auth.FieldACL{hide("revenue")},
			payload: map[string]any{"name": "Acme 2", "revenue": float64(9000)},
			current: stored(),
			wantErr: true, wantKey: true,
		},
		{
			name:    "locked-visible + nil against a stored value → FIELD_FORBIDDEN (the caller saw it)",
			rows:    []auth.FieldACL{lock("revenue")},
			payload: map[string]any{"revenue": nil},
			current: stored(),
			wantErr: true, wantKey: true,
		},
		{
			name:    "locked-visible + unchanged value → ok",
			rows:    []auth.FieldACL{lock("revenue")},
			payload: map[string]any{"name": "Acme 2", "revenue": float64(9000)},
			current: stored(),
			wantKey: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, hadName := tc.payload["name"]
			err := newFLS(tc.rows, nil).CheckWriteFieldsAgainst(context.Background(), flsTenant, flsRole, flsEntity, false, tc.payload, tc.current)
			if tc.wantErr {
				if !tc.rows[0].CanView { // a hidden field answers like an unknown one (queue #317)
					assertUnknownFieldRefusal(t, err, "revenue")
				} else {
					assertFieldForbidden(t, err)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if _, ok := tc.payload["revenue"]; ok != tc.wantKey {
				t.Fatalf("'revenue' present in payload = %v, want %v (payload=%v)", ok, tc.wantKey, tc.payload)
			}
			if _, ok := tc.payload["name"]; hadName && !ok {
				t.Fatalf("editable 'name' must never be dropped: %v", tc.payload)
			}
		})
	}
}

// --- Anti-oracle (filter/sort/aggregate on hidden field) -----------------------

func TestFLS_AssertFieldsVisible_RejectsHidden(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	err := svc.AssertFieldsVisible(context.Background(), flsTenant, flsRole, flsEntity, false, []string{"name", "revenue"})
	assertFieldForbidden(t, err)
}

func TestFLS_AssertFieldsVisible_AllowsVisible(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	if err := svc.AssertFieldsVisible(context.Background(), flsTenant, flsRole, flsEntity, false, []string{"name"}); err != nil {
		t.Fatalf("visible field must pass, got: %v", err)
	}
}

func TestFLS_AssertFieldVisible_AdminBypass(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue")}, nil)
	if err := svc.AssertFieldVisible(context.Background(), flsTenant, flsRole, flsEntity, true, "revenue"); err != nil {
		t.Fatalf("admin must bypass, got: %v", err)
	}
}

func TestFLS_HiddenFields_ReturnsSet(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue"), lock("margin")}, nil)
	hidden, err := svc.HiddenFields(context.Background(), flsTenant, flsRole, flsEntity, false)
	if err != nil {
		t.Fatalf("HiddenFields: %v", err)
	}
	if _, ok := hidden["revenue"]; !ok {
		t.Fatalf("'revenue' must be in hidden set: %v", hidden)
	}
	if _, ok := hidden["margin"]; ok {
		t.Fatalf("'margin' is locked-but-visible, must NOT be hidden: %v", hidden)
	}
}

// --- Locked set (field-lock-signal) ------------------------------------------------------------

// LockedFields is the per-caller "you may see this but not edit it" set the metadata read exports
// as `readOnly`. A hidden field is ALWAYS in it too (can't edit what you can't see) — the caller
// strips hidden first, so the flag only ever lands on a field that is actually served.
func TestFLS_LockedFields_ReturnsLockedAndHidden(t *testing.T) {
	svc := newFLS([]auth.FieldACL{hide("revenue"), lock("margin")}, nil)
	locked, err := svc.LockedFields(context.Background(), flsTenant, flsRole, flsEntity, false)
	if err != nil {
		t.Fatalf("LockedFields: %v", err)
	}
	if _, ok := locked["margin"]; !ok {
		t.Fatalf("'margin' (can_edit=0) must be in the locked set: %v", locked)
	}
	if _, ok := locked["revenue"]; !ok {
		t.Fatalf("'revenue' (can_view=0) must be in the locked set too: %v", locked)
	}
	if _, ok := locked["name"]; ok {
		t.Fatalf("'name' has no grant row → default-allow, must NOT be locked: %v", locked)
	}
}

func TestFLS_LockedFields_AdminBypass(t *testing.T) {
	svc := newFLS([]auth.FieldACL{lock("margin")}, nil)
	locked, err := svc.LockedFields(context.Background(), flsTenant, flsRole, flsEntity, true /*admin*/)
	if err != nil {
		t.Fatalf("LockedFields admin: %v", err)
	}
	if len(locked) != 0 {
		t.Fatalf("admin must get an empty locked set, got %v", locked)
	}
}

// FAIL-CLOSED: a grant-load failure is returned, so the metadata read refuses the whole answer
// rather than serving a field list that claims "editable" for a field it could not verify.
func TestFLS_LockedFields_LoadErrorReturned(t *testing.T) {
	svc := newFLS(nil, errors.New("db down"))
	if _, err := svc.LockedFields(context.Background(), flsTenant, flsRole, flsEntity, false); err == nil {
		t.Fatalf("expected the load error to surface, got nil")
	}
}

// assertFieldForbidden asserts err is a *ValidationError with code FIELD_FORBIDDEN (→ HTTP 403).
func assertFieldForbidden(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected FIELD_FORBIDDEN error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "FIELD_FORBIDDEN" {
		t.Fatalf("expected code FIELD_FORBIDDEN, got %q", ve.Code)
	}
}

// assertUnknownFieldRefusal pins the answer a HIDDEN key gets (queue #317, §4.4b): RECORD_UNKNOWN_FIELD,
// byte-for-byte what a name the entity never declared gets (same constructor), with no details.
func assertUnknownFieldRefusal(t *testing.T, err error, key string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	want := unknownFieldError(key)
	if ve.Code != want.Code || ve.Msg != want.Msg || ve.Field != "" || len(ve.Details) != 0 {
		t.Fatalf("hidden key answered %+v, want the unknown-field shape %+v", ve, want)
	}
}
