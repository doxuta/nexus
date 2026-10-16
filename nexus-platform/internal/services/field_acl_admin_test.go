package services

// W7 field-ACL admin — unit tests over in-memory fakes: validation matrix (contradiction,
// duplicate, unknown role/entity/field, empty payload), the upsert-vs-delete split (full
// access ⇒ DELETE the row — the "no restriction row" decision), hidden-normalization
// (canEdit forced false when hidden), delete-filter rules, and the __field_acl__ audit emit.
// The MySQL truth (real transaction + enforcement pickup) lives in the W7 integration suite.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

// --- fakes --------------------------------------------------------------------------------

type fakeFieldACLRepo struct {
	rows    []auth.FieldACL
	upserts []auth.FieldACL
	deletes []auth.FieldACLKey
	fail    error
	deleted int64
}

func (f *fakeFieldACLRepo) ListFieldACL(_ context.Context, tenantID, roleID, entity string) ([]auth.FieldACL, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var out []auth.FieldACL
	for _, r := range f.rows {
		if r.TenantID != tenantID {
			continue
		}
		if roleID != "" && r.RoleID != roleID {
			continue
		}
		if entity != "" && r.EntityName != entity {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeFieldACLRepo) ReplaceFieldACL(_ context.Context, _ string, upserts []auth.FieldACL, deletes []auth.FieldACLKey) error {
	if f.fail != nil {
		return f.fail
	}
	f.upserts = upserts
	f.deletes = deletes
	return nil
}

func (f *fakeFieldACLRepo) DeleteFieldACLWhere(_ context.Context, _, _, _, _ string) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	return f.deleted, nil
}

type fakeFieldACLRoles struct{ known map[string]bool }

func (f *fakeFieldACLRoles) GetRoleByID(_ context.Context, _, id string) (*auth.Role, error) {
	if f.known[id] {
		return &auth.Role{ID: id}, nil
	}
	return nil, auth.ErrRoleNotFound
}

type fakeFieldACLCatalog struct {
	entities map[string]string                     // name → id
	fields   map[string][]metadata.FieldDefinition // entityID → field definitions (name + required/default)
}

func (f *fakeFieldACLCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	id, ok := f.entities[name]
	if !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return &metadata.EntityDefinition{ID: id, Name: name}, nil
}

func (f *fakeFieldACLCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return f.fields[entityID], nil
}

type captureAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *captureAuditor) Record(_ context.Context, evt audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
}

func newFieldACLAdminForTest() (*FieldACLAdminService, *fakeFieldACLRepo, *captureAuditor) {
	repo := &fakeFieldACLRepo{deleted: 2}
	roles := &fakeFieldACLRoles{known: map[string]bool{"role-1": true, "role-2": true}}
	catalog := &fakeFieldACLCatalog{
		entities: map[string]string{"accounts": "ent-1"},
		fields: map[string][]metadata.FieldDefinition{"ent-1": {
			{Name: "name"}, {Name: "revenue"}, {Name: "formula_margin"},
			// field-lock-signal (Q2): the two shapes the PutGrants guard must tell apart.
			{Name: "code", FieldType: "text", IsRequired: true},                      // required, NO default
			{Name: "status", FieldType: "text", IsRequired: true, DefaultVal: "new"}, // required, default fills it
			{Name: "seq", FieldType: "auto_number", IsRequired: true},                // required but server-assigned
			// queue #62: required types that can NEVER carry a default (the config gate rejects
			// one), plus a legacy default the engine silently drops (composite).
			{Name: "value", FieldType: "currency", IsRequired: true},
			{Name: "owner", FieldType: "user", IsRequired: true},
			{Name: "profile", FieldType: "composite", IsRequired: true, DefaultVal: `{"first":"A"}`},
			// queue #145: the engine accepts a record id as a relation default, but Studio has no
			// record picker — advice to "set a default" is only followable through the API.
			{Name: "account", FieldType: "relation", IsRequired: true},
		}},
	}
	aud := &captureAuditor{}
	svc := NewFieldACLAdminService(repo, roles, catalog).WithAuditor(aud)
	return svc, repo, aud
}

// --- PutGrants ------------------------------------------------------------------------------

func TestFieldACLAdmin_PutGrants(t *testing.T) {
	g := func(role, entity, field string, view, edit bool) FieldACLGrantInput {
		return FieldACLGrantInput{RoleID: role, EntityName: entity, FieldName: field, CanView: view, CanEdit: edit}
	}
	tests := []struct {
		name        string
		grants      []FieldACLGrantInput
		wantCode    string
		wantApplied int
		wantCleared int
	}{
		{
			name:     "empty payload rejected",
			grants:   nil,
			wantCode: "FIELD_ACL_EMPTY",
		},
		{
			name:     "canEdit without canView is contradictory",
			grants:   []FieldACLGrantInput{g("role-1", "accounts", "revenue", false, true)},
			wantCode: "FIELD_ACL_CONTRADICTORY",
		},
		{
			name:     "missing key parts rejected",
			grants:   []FieldACLGrantInput{g("role-1", "accounts", "  ", false, false)},
			wantCode: "FIELD_ACL_GRANT_INVALID",
		},
		{
			name: "duplicate (role,entity,field) in one payload rejected",
			grants: []FieldACLGrantInput{
				g("role-1", "accounts", "revenue", false, false),
				g("role-1", "accounts", "revenue", true, false),
			},
			wantCode: "FIELD_ACL_DUPLICATE_GRANT",
		},
		{
			name:     "unknown role rejected (tenant-scoped lookup)",
			grants:   []FieldACLGrantInput{g("role-ghost", "accounts", "revenue", false, false)},
			wantCode: "FIELD_ACL_ROLE_NOT_FOUND",
		},
		{
			name:     "unknown entity rejected",
			grants:   []FieldACLGrantInput{g("role-1", "tickets", "revenue", false, false)},
			wantCode: "FIELD_ACL_ENTITY_NOT_FOUND",
		},
		{
			name:     "unknown field rejected",
			grants:   []FieldACLGrantInput{g("role-1", "accounts", "salary", false, false)},
			wantCode: "FIELD_ACL_FIELD_UNKNOWN",
		},
		{
			name:        "computed field is a legit restriction target",
			grants:      []FieldACLGrantInput{g("role-1", "accounts", "formula_margin", false, false)},
			wantApplied: 1,
		},
		{
			name: "full access deletes the row; restrictions upsert",
			grants: []FieldACLGrantInput{
				g("role-1", "accounts", "revenue", true, true),   // → delete (no-restriction)
				g("role-1", "accounts", "name", true, false),     // → read-only upsert
				g("role-2", "accounts", "revenue", false, false), // → hidden upsert
			},
			wantApplied: 2,
			wantCleared: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newFieldACLAdminForTest()
			res, err := svc.PutGrants(context.Background(), "tenant-a", tt.grants)
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
				}
				if repo.upserts != nil || repo.deletes != nil {
					t.Fatalf("validation failure must not write (all-or-nothing): %+v / %+v", repo.upserts, repo.deletes)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutGrants: %v", err)
			}
			if res.Applied != tt.wantApplied || res.Cleared != tt.wantCleared {
				t.Fatalf("result = %+v; want applied=%d cleared=%d", res, tt.wantApplied, tt.wantCleared)
			}
		})
	}
}

func TestFieldACLAdmin_PutGrants_NormalizesHiddenAndAudits(t *testing.T) {
	svc, repo, aud := newFieldACLAdminForTest()
	// A hidden grant arriving as (false, false) stores canEdit=false; the (false, true) shape
	// is rejected upstream, so stored rows are only ever (false,false) or (true,false).
	_, err := svc.PutGrants(context.Background(), "tenant-a", []FieldACLGrantInput{
		{RoleID: "role-1", EntityName: "accounts", FieldName: "revenue", CanView: false, CanEdit: false},
		{RoleID: "role-1", EntityName: "accounts", FieldName: "name", CanView: true, CanEdit: true},
	})
	if err != nil {
		t.Fatalf("PutGrants: %v", err)
	}
	if len(repo.upserts) != 1 || repo.upserts[0].CanView || repo.upserts[0].CanEdit {
		t.Fatalf("upserts = %+v; want one (canView=false, canEdit=false) row", repo.upserts)
	}
	if len(repo.deletes) != 1 || repo.deletes[0].FieldName != "name" {
		t.Fatalf("deletes = %+v; want the full-access 'name' key", repo.deletes)
	}
	if len(aud.events) != 1 || aud.events[0].EntityName != audit.FieldACLMeta || aud.events[0].Action != audit.ActionUpdate {
		t.Fatalf("audit events = %+v; want one __field_acl__ update", aud.events)
	}
}

// field-lock-signal (Q2 2026-09-05): can_edit=0 (read-only OR hidden — hidden implies locked) on a
// REQUIRED field with NO default is a config that ships an unsubmittable create form for that role
// (the strict create guard refuses the key, and omitting it fails RECORD_REQUIRED_MISSING). Refuse
// it at the source, per cell, so the matrix editor can point at the offending cell.
func TestFieldACLAdmin_PutGrants_LockedRequiredGuard(t *testing.T) {
	g := func(field string, view, edit bool) FieldACLGrantInput {
		return FieldACLGrantInput{RoleID: "role-1", EntityName: "accounts", FieldName: field, CanView: view, CanEdit: edit}
	}
	tests := []struct {
		name        string
		grant       FieldACLGrantInput
		wantCode    string
		wantApplied int
		wantCleared int
	}{
		{name: "required + no default + read-only → refused", grant: g("code", true, false), wantCode: "FIELD_ACL_LOCKED_REQUIRED"},
		{name: "required + no default + hidden → refused (hidden implies locked)", grant: g("code", false, false), wantCode: "FIELD_ACL_LOCKED_REQUIRED"},
		{name: "required + default + read-only → ok", grant: g("status", true, false), wantApplied: 1},
		{name: "required + server-assigned (auto_number) + hidden → ok (no role ever writes it)", grant: g("seq", false, false), wantApplied: 1},
		{name: "optional + read-only → ok", grant: g("revenue", true, false), wantApplied: 1},
		{name: "required + no default + full access → ok (clears, never locks)", grant: g("code", true, true), wantCleared: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newFieldACLAdminForTest()
			res, err := svc.PutGrants(context.Background(), "tenant-a", []FieldACLGrantInput{tt.grant})
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
				}
				if ve.Details["field"] != tt.grant.FieldName || ve.Details["index"] != 0 {
					t.Fatalf("details = %v; want the offending cell (field=%s, index=0)", ve.Details, tt.grant.FieldName)
				}
				if repo.upserts != nil || repo.deletes != nil {
					t.Fatalf("refusal must not write: %+v / %+v", repo.upserts, repo.deletes)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutGrants: %v", err)
			}
			if res.Applied != tt.wantApplied || res.Cleared != tt.wantCleared {
				t.Fatalf("result = %+v; want applied=%d cleared=%d", res, tt.wantApplied, tt.wantCleared)
			}
		})
	}
}

// queue #62 (field-acl-locked-required-deadend): the refusal used to tell EVERY admin "set a
// default first" — including for currency/user fields, where the field builder refuses any
// default (FIELD_DEFAULT_NOT_SUPPORTED). The admin followed the advice into a second 422 and had
// no way out. The advice must be one the machine will accept, and the guard must ask the SAME
// question Create asks (does defaultValue materialize it?), not "is the string non-empty".
func TestFieldACLAdmin_PutGrants_LockedRequiredRemedy(t *testing.T) {
	tests := []struct {
		name             string
		field            string
		defaultSupported bool
	}{
		{name: "currency: no default possible → advice must not say 'set a default'", field: "value", defaultSupported: false},
		{name: "user: no default possible → advice must not say 'set a default'", field: "owner", defaultSupported: false},
		{name: "composite with a legacy default the engine drops → still refused", field: "profile", defaultSupported: false},
		{name: "relation: Studio has a record picker (queue #320) → advice offers a default", field: "account", defaultSupported: true},
		{name: "text: a default is a real way out → advice offers it", field: "code", defaultSupported: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newFieldACLAdminForTest()
			_, err := svc.PutGrants(context.Background(), "tenant-a", []FieldACLGrantInput{
				{RoleID: "role-1", EntityName: "accounts", FieldName: tt.field, CanView: true, CanEdit: false},
			})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "FIELD_ACL_LOCKED_REQUIRED" {
				t.Fatalf("err = %v; want ValidationError FIELD_ACL_LOCKED_REQUIRED", err)
			}
			if got, ok := ve.Details["defaultSupported"].(bool); !ok || got != tt.defaultSupported {
				t.Fatalf("details.defaultSupported = %v (present=%v); want %v", ve.Details["defaultSupported"], ok, tt.defaultSupported)
			}
			if mentionsDefault := strings.Contains(ve.Msg, "giá trị mặc định cho field"); mentionsDefault != tt.defaultSupported {
				t.Fatalf("msg %q: offers 'set a default' = %v; want %v", ve.Msg, mentionsDefault, tt.defaultSupported)
			}
			if !strings.Contains(ve.Msg, "bắt buộc") {
				t.Fatalf("msg %q must name the exit every type has: turn off 'bắt buộc'", ve.Msg)
			}
			if repo.upserts != nil || repo.deletes != nil {
				t.Fatalf("refusal must not write: %+v / %+v", repo.upserts, repo.deletes)
			}
		})
	}
}

// --- ListGrants / DeleteGrants ----------------------------------------------------------------

func TestFieldACLAdmin_ListGrants(t *testing.T) {
	svc, repo, _ := newFieldACLAdminForTest()
	repo.rows = []auth.FieldACL{
		{TenantID: "tenant-a", RoleID: "role-1", EntityName: "accounts", FieldName: "revenue", CanView: false, CanEdit: false},
		{TenantID: "tenant-b", RoleID: "role-1", EntityName: "accounts", FieldName: "revenue", CanView: false, CanEdit: false},
	}

	t.Run("tenant-scoped happy path", func(t *testing.T) {
		got, err := svc.ListGrants(context.Background(), "tenant-a", "role-1", "accounts")
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		if len(got) != 1 || got[0].FieldName != "revenue" || got[0].CanView {
			t.Fatalf("grants = %+v; want tenant-a's single hidden revenue row", got)
		}
	})

	t.Run("unknown roleId filter is a client error, not silent full-access", func(t *testing.T) {
		_, err := svc.ListGrants(context.Background(), "tenant-a", "role-ghost", "")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "FIELD_ACL_ROLE_NOT_FOUND" {
			t.Fatalf("err = %v; want FIELD_ACL_ROLE_NOT_FOUND", err)
		}
	})

	t.Run("unknown entity filter rejected", func(t *testing.T) {
		_, err := svc.ListGrants(context.Background(), "tenant-a", "", "ghosts")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "FIELD_ACL_ENTITY_NOT_FOUND" {
			t.Fatalf("err = %v; want FIELD_ACL_ENTITY_NOT_FOUND", err)
		}
	})
}

func TestFieldACLAdmin_DeleteGrants(t *testing.T) {
	tests := []struct {
		name     string
		roleID   string
		entity   string
		field    string
		wantCode string
		wantN    int64
	}{
		{name: "roleId required", roleID: "", wantCode: "FIELD_ACL_ROLE_REQUIRED"},
		{name: "field without entity is ambiguous", roleID: "role-1", field: "revenue", wantCode: "FIELD_ACL_FILTER_INVALID"},
		{name: "delete by role+entity+field", roleID: "role-1", entity: "accounts", field: "revenue", wantN: 2},
		{name: "delete all restrictions of a role", roleID: "role-1", wantN: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, aud := newFieldACLAdminForTest()
			n, err := svc.DeleteGrants(context.Background(), "tenant-a", tt.roleID, tt.entity, tt.field)
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeleteGrants: %v", err)
			}
			if n != tt.wantN {
				t.Fatalf("deleted = %d; want %d", n, tt.wantN)
			}
			if len(aud.events) != 1 || aud.events[0].Action != audit.ActionDelete {
				t.Fatalf("audit events = %+v; want one __field_acl__ delete", aud.events)
			}
		})
	}
}

// TestFieldACLAdmin_RepoErrorWrapped pins the %w contract on the repo failure path.
func TestFieldACLAdmin_RepoErrorWrapped(t *testing.T) {
	svc, repo, _ := newFieldACLAdminForTest()
	boom := errors.New("db down")
	repo.fail = boom
	if _, err := svc.ListGrants(context.Background(), "tenant-a", "", ""); !errors.Is(err, boom) {
		t.Fatalf("ListGrants err = %v; want wrapped %v", err, boom)
	}
	if _, err := svc.PutGrants(context.Background(), "tenant-a", []FieldACLGrantInput{
		{RoleID: "role-1", EntityName: "accounts", FieldName: "revenue", CanView: false, CanEdit: false},
	}); !errors.Is(err, boom) {
		t.Fatalf("PutGrants err = %v; want wrapped %v", err, boom)
	}
}

// --- DeadEndFields (queue #306) --------------------------------------------------------------

// The served predicate must be the engine's own (requiredEngineCannotFill → defaultFills), so a
// stored default the engine cannot coerce (a picklist value since removed from the list) is a dead
// end the matrix can show — the FE type-list mirror would call it "fillable" while Create 422s.
func TestFieldACLAdmin_DeadEndFields(t *testing.T) {
	opts := `{"values":["a","b"]}`
	svc, _, _ := newFieldACLAdminForTest()
	cat := svc.catalog.(*fakeFieldACLCatalog)
	cat.fields["ent-1"] = append(cat.fields["ent-1"],
		metadata.FieldDefinition{Name: "stage_ok", FieldType: "picklist", IsRequired: true, DefaultVal: "a", Options: &opts},
		metadata.FieldDefinition{Name: "stage_gone", FieldType: "picklist", IsRequired: true, DefaultVal: "zzz", Options: &opts},
	)

	t.Run("serves the server predicate per field", func(t *testing.T) {
		got, err := svc.DeadEndFields(context.Background(), "tenant-a", "accounts")
		if err != nil {
			t.Fatalf("DeadEndFields: %v", err)
		}
		has := map[string]bool{}
		for _, n := range got {
			has[n] = true
		}
		for _, n := range []string{"code", "value", "owner", "profile", "account", "stage_gone"} {
			if !has[n] {
				t.Errorf("%q missing from %v; want dead end if locked", n, got)
			}
		}
		for _, n := range []string{"name", "status", "seq", "stage_ok"} {
			if has[n] {
				t.Errorf("%q in %v; want fillable", n, got)
			}
		}
	})

	t.Run("unknown entity is a client error", func(t *testing.T) {
		_, err := svc.DeadEndFields(context.Background(), "tenant-a", "ghosts")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "FIELD_ACL_ENTITY_NOT_FOUND" {
			t.Fatalf("err = %v; want FIELD_ACL_ENTITY_NOT_FOUND", err)
		}
	})
}
