package services

import "testing"

// USER-FIELD slice (../specs/017-user-field-type/spec.md): config-time validation for the new `user` field
// type. It reuses `relation`'s plain 1-N physical shape (a `<f>_id` FK-on-child column) but
// targets the FIXED `users` system table — no options.relatedEntity, no target-existence check,
// no sys_relationships registry row. These tests pin the config-time contract: allowlisted,
// no required options, never unique, and the SAME derived-column collision guard `relation` gets.

// TestValidateCreateEntity_User_ReservedDerivedColumn mirrors
// TestValidateCreateEntity_ReservedName_W5C1's "relation derived team_set_id" case
// (wave5_hardening_test.go): a `user` field named "team_set" derives "team_set_id", which
// collides with the mandatory system column — rejected before any DDL runs.
func TestValidateCreateEntity_User_ReservedDerivedColumn(t *testing.T) {
	in := CreateEntityInput{
		Name:  "tickets",
		Label: "Phiếu",
		Fields: []FieldInput{
			{Name: "team_set", Label: "TS", FieldType: "user"},
		},
	}
	if got := codeOf(ValidateCreateEntity(in)); got != "FIELD_NAME_RESERVED" {
		t.Fatalf("user field deriving team_set_id: code=%q; want FIELD_NAME_RESERVED", got)
	}
}

func TestCheckFieldType_User(t *testing.T) {
	if err := checkFieldType("user"); err != nil {
		t.Fatalf("checkFieldType(user) = %v; want nil (allowlisted)", err)
	}
}

// TestValidateFieldConfig_User asserts a `user` field needs no options at all — unlike
// `relation`, which requires options.relatedEntity.
func TestValidateFieldConfig_User(t *testing.T) {
	cases := []map[string]any{nil, {}, {"anything": "ignored"}}
	for _, opts := range cases {
		if err := validateFieldConfig("assigned_to", "user", opts); err != nil {
			t.Errorf("validateFieldConfig(user, %v) = %v; want nil", opts, err)
		}
	}
}

// TestValidateUserFieldFlags mirrors TestValidateRelationFieldFlags (field_relation_*_test.go):
// a `user` field may never be declared unique — many records are routinely assigned to the same
// user (validateUserFieldFlags's doc explains both the dispatch-order hazard and the semantic
// reason). Every other type is a no-op through this guard.
func TestValidateUserFieldFlags(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		isUnique  bool
		wantCode  string
	}{
		{"user + unique rejected", "user", true, "FIELD_USER_UNIQUE_UNSUPPORTED"},
		{"user + not unique ok", "user", false, ""},
		{"other type + unique is a no-op here", "text", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUserFieldFlags("assigned_to", tc.fieldType, tc.isUnique)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("validateUserFieldFlags(%q, unique=%v) code = %q; want %q", tc.fieldType, tc.isUnique, got, tc.wantCode)
			}
		})
	}
}

// TestValidateCreateEntity_User_Accepted proves a bare `user` field (no options) creates
// cleanly — the target is implicit (`users`), unlike `relation` which requires
// options.relatedEntity.
func TestValidateCreateEntity_User_Accepted(t *testing.T) {
	in := CreateEntityInput{
		Name:  "tickets",
		Label: "Phiếu",
		Fields: []FieldInput{
			{Name: "subject", Label: "Chủ đề", FieldType: "text"},
			{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"},
		},
	}
	if err := ValidateCreateEntity(in); err != nil {
		t.Fatalf("ValidateCreateEntity(user field) = %v; want nil", err)
	}
}

// TestValidateCreateEntity_User_RejectsUnique proves the field-flag guard runs from the
// entity-create path too (not just AddField), same as relation's equivalent test.
func TestValidateCreateEntity_User_RejectsUnique(t *testing.T) {
	in := CreateEntityInput{
		Name:  "tickets",
		Label: "Phiếu",
		Fields: []FieldInput{
			{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user", IsUnique: true},
		},
	}
	if err := ValidateCreateEntity(in); codeOf(err) != "FIELD_USER_UNIQUE_UNSUPPORTED" {
		t.Fatalf("ValidateCreateEntity(user+unique) code = %q (err=%v); want FIELD_USER_UNIQUE_UNSUPPORTED", codeOf(err), err)
	}
}

// TestValidateCreateEntity_UserColumnConflict mirrors
// TestValidateCreateEntity_RelationColumnConflict (entity_rating_relation_test.go): a `user`
// field <f> implicitly materializes a "<f>_id" FK column — the SAME derived-column shape a
// `relation` field gets — so entity-create must reject a collision with a literal field of that
// name, OR with a `relation`/`user` field that derives the same column, in EITHER declaration
// order.
func TestValidateCreateEntity_UserColumnConflict(t *testing.T) {
	user := func(name string) FieldInput {
		return FieldInput{Name: name, Label: name, FieldType: "user"}
	}
	rel := func(name, target string) FieldInput {
		return FieldInput{Name: name, Label: name, FieldType: "relation",
			Options: map[string]any{"relatedEntity": target}}
	}
	text := func(name string) FieldInput {
		return FieldInput{Name: name, Label: name, FieldType: "text"}
	}
	base := func(fields ...FieldInput) CreateEntityInput {
		return CreateEntityInput{Name: "tickets", Label: "Phiếu", Fields: fields}
	}

	cases := []struct {
		name     string
		in       CreateEntityInput
		wantCode string // "" = success
	}{
		{"user then colliding text field",
			base(user("assigned_to"), text("assigned_to_id")), "FIELD_RELATION_COLUMN_CONFLICT"},
		{"colliding text field then user",
			base(text("assigned_to_id"), user("assigned_to")), "FIELD_RELATION_COLUMN_CONFLICT"},
		{"user collides with a relation field deriving the same column",
			base(rel("assigned_to", "accounts"), user("assigned_to")), "FIELD_DUPLICATE"}, // same field name caught first
		{"user plus distinct fields ok",
			base(user("assigned_to"), text("note")), ""},
		{"user alone ok",
			base(user("assigned_to")), ""},
		{"user plus an unrelated relation ok (different derived columns)",
			base(user("assigned_to"), rel("account", "accounts")), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreateEntity(tc.in)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("ValidateCreateEntity() code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// MOT-XUONG review finding (2026-07-29): a defaultValue on a `user` field used to be ACCEPTED at
// config-time (ValidateFieldDefaultNotSupported had no "user" case) and then silently dropped at
// Create-time (record_service.defaultValue has no "user" branch, so it fell through to the default
// arm returning (nil,false,nil)). That is the "accept-then-swallow" bug class the campaign exists to
// kill. Reject at the trust boundary instead; pin both directions so it cannot regress.
func TestValidateFieldDefaultNotSupported_UserRejected(t *testing.T) {
	if got := codeOf(ValidateFieldDefaultNotSupported("user", "b1551a1a-5b97-4640-91b5-f49d13bb118f")); got != "FIELD_DEFAULT_NOT_SUPPORTED" {
		t.Fatalf("user default: code = %q; want FIELD_DEFAULT_NOT_SUPPORTED", got)
	}
	// Empty default stays legal — the guard must only fire on a non-empty value.
	if err := ValidateFieldDefaultNotSupported("user", ""); err != nil {
		t.Fatalf("empty default on user must be allowed, got %v", err)
	}
	if err := ValidateFieldDefaultNotSupported("user", "   "); err != nil {
		t.Fatalf("blank default on user must be allowed, got %v", err)
	}
}
