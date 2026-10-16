package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// queue #357 — a `user` column names the person in charge by id, e-mail or full name.

// userStoreFake is a RelationBatchChecker that also answers the two user lookups, tenant-wide over
// an in-memory directory. A disabled user is simply not in `active`.
type userStoreFake struct {
	fakeImportStore
	active   []dynrepo.UserLoginMatch // lower-cased email / name, as the repository returns them
	existsFn func(ids []string) (map[string]bool, error)
	loginErr error
	lookups  int // BatchActiveUsersByLogin calls
}

func (u *userStoreFake) BatchRecordExists(context.Context, string, string, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (u *userStoreFake) BatchActiveUserExists(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	if u.existsFn != nil {
		return u.existsFn(ids)
	}
	out := map[string]bool{}
	for _, id := range ids {
		for _, a := range u.active {
			if a.ID == id {
				out[id] = true
			}
		}
	}
	return out, nil
}

func (u *userStoreFake) BatchActiveUsersByLogin(_ context.Context, _ string, values []string) ([]dynrepo.UserLoginMatch, error) {
	u.lookups++
	if u.loginErr != nil {
		return nil, u.loginErr
	}
	var out []dynrepo.UserLoginMatch
	for _, a := range u.active {
		for _, v := range values {
			if a.Email == v || a.Name == v {
				out = append(out, a)
				break
			}
		}
	}
	return out, nil
}

func usersTasksCatalog() *fakeImportCatalog {
	return &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "tasks", Name: "tasks", Label: "Việc"},
		fields: []metadata.FieldDefinition{
			{Name: "title", FieldType: "text", IsRequired: true},
			{Name: "assignee", Label: "Người phụ trách", FieldType: "user"},
		},
	}
}

func TestImport_UserCellByIdEmailOrName(t *testing.T) {
	directory := []dynrepo.UserLoginMatch{
		{ID: "u-an", Email: "an@example.com", Name: "nguyễn văn an"},
		{ID: "u-binh", Email: "binh@example.com", Name: "trần bình"},
		{ID: "u-lan1", Email: "lan1@example.com", Name: "lê lan"},
		{ID: "u-lan2", Email: "lan2@example.com", Name: "lê lan"},
	}
	tests := []struct {
		name     string
		cell     string
		wantID   string // "" = the row must fail
		wantCode string
	}{
		{"by id", "u-binh", "u-binh", ""},
		{"by email, any case and padding", "  AN@example.com ", "u-an", ""},
		{"by full name, any case", "NGUYỄN VĂN AN", "u-an", ""},
		{"name is accent-sensitive (no silent pick)", "nguyen van an", "", "RECORD_USER_INVALID"},
		{"name shared by two users", "Lê Lan", "", "IMPORT_USER_AMBIGUOUS"},
		{"nobody (also a disabled user: not in the active set)", "ghost@example.com", "", "RECORD_USER_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &userStoreFake{active: directory}
			svc := NewImportService(usersTasksCatalog(), store, nil).WithRelationChecker(store)
			res, err := svc.Import(context.Background(), ImportInput{
				TenantID: "t1", UserID: "u1", EntityName: "tasks", IsAdmin: true,
				Reader:  strings.NewReader("Việc,Người\nGọi khách," + strings.TrimSpace(tt.cell) + "\n"),
				Mapping: map[string]string{"Việc": "title", "Người": "assignee"},
			})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if tt.wantID != "" {
				if res.Inserted != 1 || store.insertedRows[0]["assignee"] != tt.wantID {
					t.Fatalf("result = %+v rows = %#v; want assignee %q", res, store.insertedRows, tt.wantID)
				}
				return
			}
			if res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != tt.wantCode || res.Errors[0].Field != "assignee" {
				t.Fatalf("result = %+v; want one %s on assignee and nothing written", res, tt.wantCode)
			}
		})
	}
}

// One id probe + one e-mail/name lookup for the whole file; an id never reaches the name lookup.
func TestImport_UserCellsLookedUpInBatches(t *testing.T) {
	store := &userStoreFake{active: []dynrepo.UserLoginMatch{{ID: "u-an", Email: "an@example.com", Name: "an"}}}
	svc := NewImportService(usersTasksCatalog(), store, nil).WithRelationChecker(store)
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u1", EntityName: "tasks", IsAdmin: true,
		Reader:  strings.NewReader("Việc,Người\nA,an@example.com\nB,an@example.com\nC,u-an\n"),
		Mapping: map[string]string{"Việc": "title", "Người": "assignee"},
	})
	if err != nil || res.Inserted != 3 || store.lookups != 1 {
		t.Fatalf("res = %+v err = %v lookups = %d; want 3 inserted with ONE login lookup", res, err, store.lookups)
	}
}

// A lookup failure is a row error of that cell, not a silent pass; an unwired lookup fails closed.
func TestImport_UserCellLookupFailure(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		store := &userStoreFake{loginErr: errors.New("db down")}
		svc := NewImportService(usersTasksCatalog(), store, nil).WithRelationChecker(store)
		res, err := svc.Import(context.Background(), ImportInput{
			TenantID: "t1", UserID: "u1", EntityName: "tasks", IsAdmin: true,
			Reader:  strings.NewReader("Việc,Người\nA,an@example.com\n"),
			Mapping: map[string]string{"Việc": "title", "Người": "assignee"},
		})
		if err != nil || res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "IMPORT_ERROR" {
			t.Fatalf("res = %+v err = %v; want one IMPORT_ERROR and nothing written", res, err)
		}
	})
	t.Run("checker without the user seam fails closed", func(t *testing.T) {
		store := &matchStore{}
		svc := NewImportService(usersTasksCatalog(), store, nil).WithRelationChecker(store)
		res, err := svc.Import(context.Background(), ImportInput{
			TenantID: "t1", UserID: "u1", EntityName: "tasks", IsAdmin: true,
			Reader:  strings.NewReader("Việc,Người\nA,u-an\n"),
			Mapping: map[string]string{"Việc": "title", "Người": "assignee"},
		})
		if err != nil || res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_USER_UNAVAILABLE" {
			t.Fatalf("res = %+v err = %v; want RECORD_USER_UNAVAILABLE", res, err)
		}
	})
}

// queue #362 meets #357: a row already failing on another cell still has its person named.
func TestImport_UserCellOfAnAlreadyFailedRowIsNamed(t *testing.T) {
	store := &userStoreFake{}
	svc := NewImportService(usersTasksCatalog(), store, nil).WithRelationChecker(store)
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u1", EntityName: "tasks", IsAdmin: true, DryRun: true,
		Reader:  strings.NewReader("Việc,Người\n,ghost@example.com\n"),
		Mapping: map[string]string{"Việc": "title", "Người": "assignee"},
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got := map[string]string{}
	for _, e := range res.Errors {
		got[e.Field] = e.Code
	}
	if got["title"] != "RECORD_REQUIRED_MISSING" || got["assignee"] != "RECORD_USER_INVALID" {
		t.Fatalf("errors = %v; want the missing title AND the unknown person", got)
	}
}
