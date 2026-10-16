//go:build integration

package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestJobsCredentialName_Integration pins #342: a listed webhook job carries the NAME of the vault
// key its payload names (credentialId), looked up inside the caller's tenant only — a key id of
// another tenant, a deleted key, a job with no key and a non-webhook job all answer nil, and the
// secret columns are never read.
func TestJobsCredentialName_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	cred := func(id, tenant, name string, deleted bool) {
		if _, err := db.Exec(`INSERT INTO sys_named_credentials
			(id, tenant_id, name, label, base_url, auth_type, deleted_at)
			VALUES (?, ?, ?, ?, 'https://partner.example', 'header', IF(?, NOW(3), NULL))`,
			id, tenant, name, name, deleted); err != nil {
			t.Fatalf("seed credential %s: %v", id, err)
		}
	}
	const (
		live    = "11111111-0000-0000-0000-000000000001"
		gone    = "11111111-0000-0000-0000-000000000002"
		foreign = "11111111-0000-0000-0000-000000000003"
	)
	cred(live, tjTenantA, "stripe-prod", false)
	cred(gone, tjTenantA, "old-key", true)
	cred(foreign, tjTenantB, "b-secret-name", false)
	wh := func(credID string) string {
		return fmt.Sprintf(`{"workflowId":"wf-1","actionIndex":0,"credentialId":%q,"body":"{}"}`, credID)
	}
	withKey := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, wh(live), at)
	deleted := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, wh(gone), at.Add(time.Second))
	crossTenant := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, wh(foreign), at.Add(2*time.Second))
	noKey := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed,
		`{"workflowId":"wf-1","actionIndex":0,"body":"{}"}`, at.Add(3*time.Second))
	timer := tjSeed(t, db, store, tjTenantA, "workflow_timer", StatusQueued, `{"workflowId":"wf-2"}`, at.Add(4*time.Second))

	got := map[string]*string{}
	for _, j := range tjWalk(t, store, tjTenantA, TenantJobFilter{}) {
		got[j.ID] = j.CredentialName
	}
	if got[withKey] == nil || *got[withKey] != "stripe-prod" {
		t.Errorf("job with a live key: credentialName = %v, want stripe-prod", got[withKey])
	}
	for label, id := range map[string]string{"deleted key": deleted, "other tenant's key": crossTenant, "no key": noKey, "timer": timer} {
		if got[id] != nil {
			t.Errorf("%s: credentialName = %q, want nil", label, *got[id])
		}
	}
	one, err := store.GetForTenant(ctx, tjTenantA, withKey)
	if err != nil || one.CredentialName == nil || *one.CredentialName != "stripe-prod" {
		t.Errorf("GetForTenant credentialName = %v (err %v), want stripe-prod", one, err)
	}
}
