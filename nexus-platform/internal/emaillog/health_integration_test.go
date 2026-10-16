//go:build integration

package emaillog

import (
	"context"
	"testing"
	"time"
)

// specs/066 T131: FailedSummarySince pins the tenant, counts every source in failed7d/failed24h and
// only an unresent source='workflow' row as unresent.
func TestFailedSummarySince_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed(t, r, tenA, "hs-wf-old", StatusFailed)
	seed(t, r, tenA, "hs-wf-new", StatusFailed)
	seed(t, r, tenA, "hs-sent", StatusSent)
	seed(t, r, tenB, "hs-other", StatusFailed)
	pw := Entry{ID: "hs-pw", TenantID: tenA, Source: SourcePasswordReset, Status: StatusFailed, RecipientMasked: "a***@x.vn"}
	if err := r.Insert(ctx, pw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sys_email_log SET created_at = ? WHERE tenant_id = ? AND id = 'hs-wf-old'`, now.Add(-3*24*time.Hour), tenA); err != nil {
		t.Fatal(err)
	}

	got, err := r.FailedSummarySince(ctx, tenA, now.Add(-7*24*time.Hour), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Failed7d != 3 || got.Failed24h != 2 || got.UnresentFailed != 2 {
		t.Fatalf("summary = %+v, want 7d=3 24h=2 unresent=2 (password_reset not unresent, tenant B excluded)", got)
	}

	if _, err := db.Exec(`UPDATE sys_email_log SET resent_at = NOW(3) WHERE tenant_id = ? AND id = 'hs-wf-new'`, tenA); err != nil {
		t.Fatal(err)
	}
	got, err = r.FailedSummarySince(ctx, tenA, now.Add(-7*24*time.Hour), now.Add(-24*time.Hour))
	if err != nil || got.UnresentFailed != 1 {
		t.Fatalf("after resend = %+v err %v, want unresent 1", got, err)
	}
}
