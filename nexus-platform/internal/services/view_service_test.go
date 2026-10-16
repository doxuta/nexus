package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/view"
)

// fakeViewRepo is an in-memory viewRepo for unit tests: it records the last create/update input
// and returns the configured Get result so the ownership branches can be exercised without MySQL.
type fakeViewRepo struct {
	createIn   *view.CreateInput
	getRec     *view.Record
	getErr     error
	updatedID  string
	deletedID  string
	listResult []view.Record
}

func (f *fakeViewRepo) Create(_ context.Context, in view.CreateInput) (string, error) {
	f.createIn = &in
	return "view-new", nil
}
func (f *fakeViewRepo) ListForUser(_ context.Context, _, _, _ string) ([]view.Record, error) {
	return f.listResult, nil
}
func (f *fakeViewRepo) Get(_ context.Context, _, _ string) (*view.Record, error) {
	return f.getRec, f.getErr
}
func (f *fakeViewRepo) Update(_ context.Context, _, id string, _ view.UpdateInput) error {
	f.updatedID = id
	return nil
}
func (f *fakeViewRepo) Delete(_ context.Context, _, id string) error {
	f.deletedID = id
	return nil
}

// fakeViewEntities implements viewEntityChecker: names in `known` exist, everything else is
// metadata.ErrEntityNotFound.
type fakeViewEntities struct{ known map[string]bool }

func (f fakeViewEntities) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if f.known[name] {
		return &metadata.EntityDefinition{Name: name}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

// viewErrCode extracts the ValidationError code from an error, or "" when it is not a ValidationError.
func viewErrCode(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func newViewSvc(repo viewRepo) *ViewService {
	return NewViewService(repo, fakeViewEntities{known: map[string]bool{"accounts": true}}, nil)
}

const validViewConfig = `{"filters":[],"sort":{"field":"name","dir":"asc"},"columns":["name"]}`

func TestViewService_Create(t *testing.T) {
	tests := []struct {
		name       string
		in         CreateViewInput
		isAdmin    bool
		wantErr    string // expected ValidationError code; "" = success
		wantShared bool   // when success: expect repo saw a shared (nil owner) view
	}{
		{
			name: "personal create by regular user",
			in:   CreateViewInput{EntityName: "accounts", Name: "My open", Config: json.RawMessage(validViewConfig)},
		},
		{
			name:       "shared create by admin",
			in:         CreateViewInput{EntityName: "accounts", Name: "Team view", Config: json.RawMessage(validViewConfig), Shared: true},
			isAdmin:    true,
			wantShared: true,
		},
		{
			name:    "shared create by non-admin is forbidden",
			in:      CreateViewInput{EntityName: "accounts", Name: "Team view", Config: json.RawMessage(validViewConfig), Shared: true},
			wantErr: "VIEW_FORBIDDEN",
		},
		{
			name:    "unknown entity",
			in:      CreateViewInput{EntityName: "ghosts", Name: "x", Config: json.RawMessage(validViewConfig)},
			wantErr: "ENTITY_NOT_FOUND",
		},
		{
			name:    "empty name",
			in:      CreateViewInput{EntityName: "accounts", Name: "   ", Config: json.RawMessage(validViewConfig)},
			wantErr: "VIEW_NAME_REQUIRED",
		},
		{
			name:    "config is a JSON array (not object)",
			in:      CreateViewInput{EntityName: "accounts", Name: "x", Config: json.RawMessage(`[1,2,3]`)},
			wantErr: "VIEW_CONFIG_INVALID",
		},
		{
			name:    "config is null",
			in:      CreateViewInput{EntityName: "accounts", Name: "x", Config: json.RawMessage(`null`)},
			wantErr: "VIEW_CONFIG_INVALID",
		},
		{
			name:    "config is malformed JSON",
			in:      CreateViewInput{EntityName: "accounts", Name: "x", Config: json.RawMessage(`{bad`)},
			wantErr: "VIEW_CONFIG_INVALID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeViewRepo{}
			svc := newViewSvc(repo)
			_, err := svc.Create(context.Background(), "t1", "u1", tt.isAdmin, tt.in)
			if tt.wantErr != "" {
				if got := viewErrCode(err); got != tt.wantErr {
					t.Fatalf("want code %q, got err %v (code %q)", tt.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo.createIn == nil {
				t.Fatal("repo.Create was not called")
			}
			if tt.wantShared {
				if repo.createIn.UserID != nil {
					t.Fatalf("shared view should have nil owner, got %v", *repo.createIn.UserID)
				}
			} else {
				if repo.createIn.UserID == nil || *repo.createIn.UserID != "u1" {
					t.Fatalf("personal view should be owned by u1, got %v", repo.createIn.UserID)
				}
			}
		})
	}
}

func TestViewService_Create_NameTooLong(t *testing.T) {
	long := make([]byte, viewNameMaxRunes+1)
	for i := range long {
		long[i] = 'a'
	}
	repo := &fakeViewRepo{}
	svc := newViewSvc(repo)
	_, err := svc.Create(context.Background(), "t1", "u1", false,
		CreateViewInput{EntityName: "accounts", Name: string(long), Config: json.RawMessage(validViewConfig)})
	if got := viewErrCode(err); got != "VIEW_NAME_TOO_LONG" {
		t.Fatalf("want VIEW_NAME_TOO_LONG, got %v (%q)", err, got)
	}
}

// TestViewService_Mutate covers the ownership gate shared by Update and Delete (mutableView).
func TestViewService_Mutate(t *testing.T) {
	uid := "u1"
	other := "u2"
	tests := []struct {
		name    string
		getRec  *view.Record
		getErr  error
		isAdmin bool
		wantErr string // "" = allowed
	}{
		{
			name:   "own personal view is mutable",
			getRec: &view.Record{ID: "v1", EntityName: "accounts", UserID: &uid},
		},
		{
			name:    "another user's personal view is not-found (no leak)",
			getRec:  &view.Record{ID: "v1", EntityName: "accounts", UserID: &other},
			wantErr: "VIEW_NOT_FOUND",
		},
		{
			name:    "shared view by non-admin is forbidden",
			getRec:  &view.Record{ID: "v1", EntityName: "accounts", UserID: nil},
			wantErr: "VIEW_FORBIDDEN",
		},
		{
			name:    "shared view by admin is mutable",
			getRec:  &view.Record{ID: "v1", EntityName: "accounts", UserID: nil},
			isAdmin: true,
		},
		{
			name:    "view addressed under the wrong entity is not-found",
			getRec:  &view.Record{ID: "v1", EntityName: "contacts", UserID: &uid},
			wantErr: "VIEW_NOT_FOUND",
		},
		{
			name:    "missing view is not-found",
			getErr:  view.ErrViewNotFound,
			wantErr: "VIEW_NOT_FOUND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise BOTH Update and Delete — they share mutableView so must agree.
			for _, op := range []string{"update", "delete"} {
				repo := &fakeViewRepo{getRec: tt.getRec, getErr: tt.getErr}
				svc := newViewSvc(repo)
				var err error
				if op == "update" {
					err = svc.Update(context.Background(), "t1", uid, tt.isAdmin, "accounts", "v1",
						UpdateViewInput{Name: "renamed", Config: json.RawMessage(validViewConfig)})
				} else {
					err = svc.Delete(context.Background(), "t1", uid, tt.isAdmin, "accounts", "v1")
				}
				if tt.wantErr != "" {
					if got := viewErrCode(err); got != tt.wantErr {
						t.Fatalf("[%s] want %q, got %v (%q)", op, tt.wantErr, err, got)
					}
					if op == "update" && repo.updatedID != "" {
						t.Fatalf("[update] repo.Update must not run on a refused mutate")
					}
					if op == "delete" && repo.deletedID != "" {
						t.Fatalf("[delete] repo.Delete must not run on a refused mutate")
					}
					continue
				}
				if err != nil {
					t.Fatalf("[%s] unexpected error: %v", op, err)
				}
				if op == "update" && repo.updatedID != "v1" {
					t.Fatalf("[update] repo.Update was not called")
				}
				if op == "delete" && repo.deletedID != "v1" {
					t.Fatalf("[delete] repo.Delete was not called")
				}
			}
		})
	}
}

func TestViewService_List_RequiresEntity(t *testing.T) {
	svc := newViewSvc(&fakeViewRepo{})
	if _, err := svc.List(context.Background(), "t1", "u1", "  "); viewErrCode(err) != "VIEW_ENTITY_REQUIRED" {
		t.Fatalf("want VIEW_ENTITY_REQUIRED, got %v", err)
	}
}
