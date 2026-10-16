package services

// EXP-01: ?expand must gate the resolved labelField against the caller's FIELD-level ACL on the
// TARGET entity — not only the entity-level ACL (W5-A2). A role allowed to VIEW the target entity
// but denied can_view on its name/labelField must NOT receive the label via ?expand: the expand
// resolves to {id} only. This mirrors applyLookups' sourceDenied gate. Get + LIST (bulk harvester)
// are both covered. The hidden-field set is loaded ONCE per target entity per call (hoisted).

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakeExpandLabelACL is a fieldACLEnforcer double used by the EXP-01 tests. Only HiddenFields is
// meaningful here (applyExpand consults nothing else); the rest are no-ops. hidden maps an entity
// name → the set of field names the role may not view. hiddenCalls counts HiddenFields invocations
// so a test can prove the per-call hoist (one load per target entity, not per record).
type fakeExpandLabelACL struct {
	hidden      map[string]map[string]struct{}
	hiddenCalls int
}

func (f *fakeExpandLabelACL) FilterReadFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}
func (f *fakeExpandLabelACL) FilterReadFieldsBatch(_ context.Context, _, _, _ string, _ bool, _ []map[string]any) error {
	return nil
}
func (f *fakeExpandLabelACL) CheckWriteFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (f *fakeExpandLabelACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}
func (f *fakeExpandLabelACL) AssertFieldVisible(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (f *fakeExpandLabelACL) AssertFieldsVisible(_ context.Context, _, _, _ string, _ bool, _ []string) error {
	return nil
}
func (f *fakeExpandLabelACL) HiddenFields(_ context.Context, _, _, entity string, _ bool) (map[string]struct{}, error) {
	f.hiddenCalls++
	if f.hidden == nil {
		return nil, nil
	}
	return f.hidden[entity], nil
}

// TestEXP01_Expand_DeniedLabelField_ReturnsIDOnly is the core RED test: the caller may VIEW the
// target entity (authz allows) but is denied can_view on the labelField ("name") → the expand must
// resolve to {id} only, never {id,label}, and the label fetcher must not run (no denied value
// pulled into memory).
func TestEXP01_Expand_DeniedLabelField_ReturnsIDOnly(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}                           // entity view allowed
	facl := &fakeExpandLabelACL{hidden: map[string]map[string]struct{}{"accounts": {"name": {}}}} // labelField denied
	scope := RecordScope{UserID: "u1", RoleID: "role-1", IsAdmin: false}

	if err := applyExpand(context.Background(), fetcher, authz, facl, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	// Must be {id} only — no label key leaked.
	got, ok := records[0].Data["account"].(expandedIDOnly)
	if !ok {
		t.Fatalf("denied labelField: account=%#v; want expandedIDOnly{id} only (no label leak)", records[0].Data["account"])
	}
	if got.ID != "acc-1" {
		t.Errorf("expandedIDOnly.ID = %q; want \"acc-1\"", got.ID)
	}
	// The label must never be fetched for a denied labelField.
	if fetcher.calls["accounts"] != 0 {
		t.Errorf("fetcher must not run for a denied labelField; calls=%d", fetcher.calls["accounts"])
	}
}

// TestEXP01_Expand_AllowedLabelField_ResolvesLabel confirms the non-regression: when the labelField
// is NOT hidden, the expand resolves the full {id,label} exactly as before.
func TestEXP01_Expand_AllowedLabelField_ResolvesLabel(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}
	facl := &fakeExpandLabelACL{hidden: map[string]map[string]struct{}{"accounts": {"secret": {}}}} // name NOT hidden
	scope := RecordScope{UserID: "u1", RoleID: "role-1"}

	if err := applyExpand(context.Background(), fetcher, authz, facl, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	ev, ok := records[0].Data["account"].(expandedValue)
	if !ok || ev.Label != "Acme Corp" {
		t.Errorf("allowed labelField: account=%#v; want expandedValue{label:\"Acme Corp\"}", records[0].Data["account"])
	}
}

// TestEXP01_Expand_AdminBypassesFieldACL confirms an admin caller resolves the label despite a
// hidden set that would deny a non-admin — and never consults the field-ACL enforcer.
func TestEXP01_Expand_AdminBypassesFieldACL(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}
	facl := &fakeExpandLabelACL{hidden: map[string]map[string]struct{}{"accounts": {"name": {}}}}
	scope := RecordScope{UserID: "admin", IsAdmin: true}

	if err := applyExpand(context.Background(), fetcher, authz, facl, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	if _, ok := records[0].Data["account"].(expandedValue); !ok {
		t.Errorf("admin must bypass field-ACL; account=%#v", records[0].Data["account"])
	}
	if facl.hiddenCalls != 0 {
		t.Errorf("admin path must not consult field-ACL; hiddenCalls=%d", facl.hiddenCalls)
	}
}

// TestEXP01_Expand_Bulk_DeniedLabelField_NoLeak covers LIST (the bulk harvester): every record's
// expanded value is {id} only, no label leaks across the page, and the hidden-field set is loaded
// exactly ONCE (hoisted) despite N records.
func TestEXP01_Expand_Bulk_DeniedLabelField_NoLeak(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp", "acc-2": "Globex", "acc-3": "Initech"})
	specs := []FieldSpec{makeRelationSpec("account", "accounts", "name")}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
		{ID: "r3", Data: map[string]any{"account": "acc-3"}},
	}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}
	facl := &fakeExpandLabelACL{hidden: map[string]map[string]struct{}{"accounts": {"name": {}}}}
	scope := RecordScope{UserID: "u1", RoleID: "role-1"}

	if err := applyExpand(context.Background(), fetcher, authz, facl, scope, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}
	for _, rec := range records {
		v, ok := rec.Data["account"].(expandedIDOnly)
		if !ok {
			t.Errorf("record %s: account=%#v; want expandedIDOnly (no label leak)", rec.ID, rec.Data["account"])
		}
		if v.ID == "" {
			t.Errorf("record %s: expandedIDOnly.ID empty", rec.ID)
		}
	}
	if fetcher.calls["accounts"] != 0 {
		t.Errorf("bulk: fetcher must not run for a denied labelField; calls=%d", fetcher.calls["accounts"])
	}
	// Hoisted: the hidden set is loaded once per target entity per call, not once per record.
	if facl.hiddenCalls != 1 {
		t.Errorf("hidden set must be loaded once (hoisted); hiddenCalls=%d, want 1", facl.hiddenCalls)
	}
}
