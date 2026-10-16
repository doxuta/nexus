package services

// W5-A2: ?expand must only resolve a relation's label when the caller may VIEW the relation's
// TARGET entity. Otherwise a label (e.g. a related person's name) leaks across an entity/ACL
// boundary via ?expand — and List makes it a bulk harvester. Entity-level gate (v1): a denied
// target leaves the raw id; an allowed target resolves the label; an admin bypasses the gate.

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakeExpandAuthz is a test double for entityAuthorizer: it allows view on the entities present
// in `allow` (true) and denies the rest, counting how many times it was consulted.
type fakeExpandAuthz struct {
	allow map[string]bool
	calls int
}

func (f *fakeExpandAuthz) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	f.calls++
	return f.allow[entity], "all", nil
}

func TestW5A2_Expand_DeniedTargetEntity_LeavesRawID(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": false}} // caller cannot view accounts
	scope := RecordScope{UserID: "u1", RoleID: "role-1", IsAdmin: false}

	if err := applyExpand(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	// The label must NOT be resolved — the raw id stays (no cross-entity label leak).
	if got, ok := records[0].Data["account"].(string); !ok || got != "acc-1" {
		t.Errorf("denied target: account=%#v; want raw id \"acc-1\" (no label leak)", records[0].Data["account"])
	}
	// The label fetcher must not even run for a denied target.
	if fetcher.calls["accounts"] != 0 {
		t.Errorf("fetcher must not run for a denied target; calls=%d", fetcher.calls["accounts"])
	}
}

func TestW5A2_Expand_AllowedTargetEntity_ResolvesLabel(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}
	scope := RecordScope{UserID: "u1", RoleID: "role-1"}

	if err := applyExpand(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	ev, ok := records[0].Data["account"].(expandedValue)
	if !ok || ev.Label != "Acme Corp" {
		t.Errorf("allowed target: account=%#v; want expandedValue{label:\"Acme Corp\"}", records[0].Data["account"])
	}
}

func TestW5A2_Expand_AdminBypassesAuthz(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": false}} // would deny a non-admin
	scope := RecordScope{UserID: "admin", IsAdmin: true}

	if err := applyExpand(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	// Admin bypasses the entity-ACL gate → label resolves despite authz denying.
	if _, ok := records[0].Data["account"].(expandedValue); !ok {
		t.Errorf("admin must bypass authz; account=%#v", records[0].Data["account"])
	}
	if authz.calls != 0 {
		t.Errorf("admin path must not consult authz; calls=%d", authz.calls)
	}
}
