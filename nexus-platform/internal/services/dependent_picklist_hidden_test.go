package services

// Queue #321 (hidden-dependent-picklist-named, api-conventions §4.4b): a dependent picklist whose
// CHILD (or controlling PARENT) is hidden from the caller's role must not be named in
// RECORD_DEPENDENT_PICKLIST_INVALID. The create-side leak needed no guess: the child's stored
// defaultValue was materialized into every create, so a parent that was left empty, or that did not
// allow the default, failed the create naming the hidden child. Queue #255 closed it at the root: such
// a default is now SKIPPED, the create succeeds, and nothing is named. The update door (the parent
// moves while a hidden child's STORED value no longer fits) keeps the scrub and its test below.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

const (
	depHidParent      = "region"
	depHidParentLabel = "Khu vực"
	depHidChild       = "area"
	depHidChildLabel  = "Quận bí mật"
)

func depHiddenCatalog() *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-t", Name: "tickets"},
		fields: []metadata.FieldDefinition{
			{Name: "name", Label: "Tên", FieldType: "text"},
			{Name: depHidParent, Label: depHidParentLabel, FieldType: "picklist", Options: strPtr(`{"values":["A","B"]}`)},
			{Name: depHidChild, Label: depHidChildLabel, FieldType: "picklist", DefaultVal: "b1",
				Options: strPtr(`{"values":["a1","b1"],"controllingField":"region","dependentValues":{"A":["a1"]}}`)},
		},
	}
}

func depHiddenSvc(rows []auth.FieldACL, store RecordStore) *RecordService {
	return NewRecordService(depHiddenCatalog(), store).
		WithFieldACL(NewFieldACLService(&fakeFieldACLLoader{rows: rows}))
}

// namedIn renders the whole refusal the way the wire carries it and reports which of names appear.
func namedIn(err error, names ...string) []string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"code": ve.Code, "msg": ve.Msg, "field": ve.Field, "details": ve.Details})
	var found []string
	for _, n := range names {
		if strings.Contains(string(b), n) {
			found = append(found, n)
		}
	}
	return found
}

func TestHiddenDependentPicklist_CreateDefaultSkippedNotRefused(t *testing.T) {
	ctx := context.Background()
	role := RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all"}
	admin := RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all", IsAdmin: true}
	tests := []struct {
		name    string
		rows    []auth.FieldACL
		scope   RecordScope
		payload map[string]any
	}{
		{"hidden child, parent does not allow its default", []auth.FieldACL{hide(depHidChild)}, role, map[string]any{"name": "x", depHidParent: "A"}},
		{"hidden child, parent left empty", []auth.FieldACL{hide(depHidChild)}, role, map[string]any{"name": "x"}},
		{"hidden parent, visible child whose default needs it", []auth.FieldACL{hide(depHidParent)}, role, map[string]any{"name": "x"}},
		{"admin: same create passes", []auth.FieldACL{hide(depHidChild)}, admin, map[string]any{"name": "x"}},
		{"role that can see both: same create passes", []auth.FieldACL{hide("name")}, role, map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := depHiddenSvc(tt.rows, &fakeRecordStore{}).Create(ctx, tt.scope, "t1", "u1", "tickets", tt.payload); err != nil {
				t.Fatalf("Create = %v; the default does not fit the parent, so it is skipped and the create passes", err)
			}
		})
	}
}

// A visible child the USER sent, with the parent hidden and empty, is judged (not a default) — the
// refusal must still not name the hidden parent.
func TestHiddenDependentPicklist_CreateUserChildNotNamingHiddenParent(t *testing.T) {
	role := RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all"}
	_, err := depHiddenSvc([]auth.FieldACL{hide(depHidParent)}, &fakeRecordStore{}).
		Create(context.Background(), role, "t1", "u1", "tickets", map[string]any{"name": "x", depHidChild: "a1"})
	if codeOf(err) != "RECORD_DEPENDENT_PICKLIST_INVALID" {
		t.Fatalf("code = %q (err=%v), want RECORD_DEPENDENT_PICKLIST_INVALID", codeOf(err), err)
	}
	if leaked := namedIn(err, depHidParent, depHidParentLabel); len(leaked) > 0 {
		t.Fatalf("create refusal names hidden parent %v: %v", leaked, err)
	}
}

// The door that still reaches the check with a hidden field: update (the parent moves while the
// hidden child's STORED value no longer fits). Bulk rows and CSV rows share the create path, where
// the default is now skipped (#255), so they no longer refuse.
func TestHiddenDependentPicklist_NotNamedOnOtherDoors(t *testing.T) {
	ctx := context.Background()
	role := RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all"}
	hideChild := []auth.FieldACL{hide(depHidChild)}
	hidden := []string{depHidChild, depHidChildLabel}

	t.Run("update moves the parent away from the hidden child's stored value", func(t *testing.T) {
		store := &relabelStore{current: map[string]any{"name": "x", depHidParent: "A", depHidChild: "a1"}}
		err := depHiddenSvc(hideChild, store).Update(ctx, role, "t1", "tickets", "rec-1",
			map[string]any{depHidParent: "B"}, nil)
		if codeOf(err) != "RECORD_DEPENDENT_PICKLIST_INVALID" {
			t.Fatalf("code = %q (err=%v), want RECORD_DEPENDENT_PICKLIST_INVALID", codeOf(err), err)
		}
		if leaked := namedIn(err, hidden...); len(leaked) > 0 {
			t.Fatalf("update refusal names hidden field(s) %v: %v", leaked, err)
		}
		if store.updated {
			t.Fatalf("a refused update must not write")
		}
	})
}
