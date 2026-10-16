package services

// relation_edit_scope_test.go — specs/060 (DAILY-QUEUE #351). Link/Unlink act on an EXISTING source
// record, so the service must gate the source row by the caller's EDIT scope itself — the way
// files.Service.Upload does — instead of trusting whatever scope the route stage handed down. The
// bug: the routes sat on the create-mapped group, so a role with create:all + edit:own arrived with
// scope "all" and could attach links to a colleague's record it cannot even read.

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// actionAuthz answers per (entity, action): "entity:action" → level; missing → denied.
type actionAuthz map[string]string

func (a actionAuthz) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	lvl, ok := a[entity+":"+action]
	return ok, lvl, nil
}

// ownerFetcher is a scope-aware RecordFetcher: it honours owner level the way dynrepo does.
type ownerFetcher struct{ owners map[string]string } // record id → owner user id

func (f ownerFetcher) GetRecord(_ context.Context, _, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	owner, ok := f.owners[id]
	if !ok {
		return nil, dynrepo.ErrRecordNotFound
	}
	switch {
	case scope.IsAdmin, scope.Level == dynrepo.AccessAll:
	case scope.Level == dynrepo.AccessOwner && owner == scope.UserID:
	default:
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, TenantID: tenantID, OwnerID: owner}, nil
}

func nnRelFixture() (*fakeJoinRepo, *fakeRelMeta) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	return joinRepo, relMeta
}

func TestLink_SourceGatedByEditScopeNotHandedScope(t *testing.T) {
	// create:all + edit:own: the route stage (create-mapped, the bug) hands scope "all".
	authz := actionAuthz{
		"accounts:view": "owner", "accounts:edit": "owner", "accounts:create": "all",
		"contacts:view": "all",
	}
	fetcher := ownerFetcher{owners: map[string]string{
		"mine": "user-1", "theirs": "user-2", "tgt-1": "user-2",
	}}
	handedScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessAll}

	tests := []struct {
		name        string
		sourceID    string
		wantCode    string // "" = success
		wantCreated bool
	}{
		{"hồ sơ của mình ⇒ liên kết được", "mine", "", true},
		{"hồ sơ đồng nghiệp ⇒ từ chối y như không tồn tại", "theirs", "RECORD_RELATION_INVALID", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joinRepo, relMeta := nnRelFixture()
			svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

			created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
				"accounts", tt.sourceID, "accounts_contacts", "tgt-1",
				newCheckerAllExist(), nil, fetcher, handedScope)

			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q (err=%v), want %q", got, err, tt.wantCode)
			}
			if created != tt.wantCreated {
				t.Errorf("created = %v, want %v", created, tt.wantCreated)
			}
			if tt.wantCode != "" && len(joinRepo.links) != 0 {
				t.Errorf("join rows written despite the refusal: %v", joinRepo.links)
			}
		})
	}
}

func TestLink_NoEditGrantIsRefusedAsInvalid(t *testing.T) {
	// create:all but no edit grant at all: the service must not fall back to the handed scope.
	authz := actionAuthz{"accounts:view": "all", "accounts:create": "all", "contacts:view": "all"}
	fetcher := ownerFetcher{owners: map[string]string{"mine": "user-1", "tgt-1": "user-1"}}
	joinRepo, relMeta := nnRelFixture()
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

	created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "mine", "accounts_contacts", "tgt-1",
		newCheckerAllExist(), nil, fetcher, dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessAll})

	if codeOf(err) != "RECORD_RELATION_INVALID" || created {
		t.Fatalf("created=%v err=%v; want RECORD_RELATION_INVALID — no edit grant must not link", created, err)
	}
}

func TestLink_AdminKeepsHandedScope(t *testing.T) {
	// Admin bypasses ACL everywhere; the authorizer denies everything and must not be consulted.
	fetcher := ownerFetcher{owners: map[string]string{"theirs": "user-2", "tgt-1": "user-2"}}
	joinRepo, relMeta := nnRelFixture()
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(actionAuthz{})

	created, err := svc.Link(context.Background(), "tenant-1", "admin-1", "role-admin", true,
		"accounts", "theirs", "accounts_contacts", "tgt-1",
		newCheckerAllExist(), nil, fetcher, dynrepo.RowScope{UserID: "admin-1", Level: dynrepo.AccessAll, IsAdmin: true})

	if err != nil || !created {
		t.Fatalf("created=%v err=%v; want admin to link", created, err)
	}
}

func TestUnlink_SourceGatedByEditScopeNotHandedScope(t *testing.T) {
	// delete:all + edit:own: the DELETE-mapped route stage (the bug) hands scope "all".
	authz := actionAuthz{"accounts:view": "owner", "accounts:edit": "owner", "accounts:delete": "all"}
	fetcher := ownerFetcher{owners: map[string]string{"mine": "user-1", "theirs": "user-2"}}
	handedScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessAll}

	tests := []struct {
		name     string
		sourceID string
		wantCode string
	}{
		{"hồ sơ của mình ⇒ bỏ liên kết được", "mine", ""},
		{"hồ sơ đồng nghiệp ⇒ từ chối y như không tồn tại", "theirs", "RECORD_RELATION_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joinRepo, relMeta := nnRelFixture()
			svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

			err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
				"accounts", tt.sourceID, "accounts_contacts", "tgt-1", fetcher, handedScope)

			if got := codeOf(err); got != tt.wantCode || (tt.wantCode == "" && err != nil) {
				t.Fatalf("code = %q (err=%v), want %q", got, err, tt.wantCode)
			}
		})
	}
}

func TestUnlink_NoEditGrantIsRefusedAsInvalid(t *testing.T) {
	authz := actionAuthz{"accounts:view": "all", "accounts:delete": "all"}
	fetcher := ownerFetcher{owners: map[string]string{"mine": "user-1"}}
	joinRepo, relMeta := nnRelFixture()
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

	err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "mine", "accounts_contacts", "tgt-1", fetcher, dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessAll})

	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID — no edit grant must not unlink", err)
	}
}
