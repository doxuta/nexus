//go:build integration

package jobs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// specs/066 T131: the health reads pin the tenant (tenant B and tenant-NULL system jobs never
// count), and each rides its intended index.
func TestJobsHealthReads_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	now := time.Now().UTC()

	tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, "", now.Add(-time.Hour))
	tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, "", now.Add(-2*time.Hour))
	tjSeed(t, db, store, tjTenantA, "email_alert", StatusFailed, "", now.Add(-30*24*time.Hour))
	tjSeed(t, db, store, tjTenantA, "email_alert", StatusDone, "", now.Add(-time.Hour))
	tjSeed(t, db, store, tjTenantB, "outbound_webhook", StatusFailed, "", now.Add(-time.Hour))
	tjSeed(t, db, store, "", "system_mail", StatusFailed, "", now.Add(-time.Hour))

	got, err := store.FailedByTypeSince(ctx, tjTenantA, now.Add(-7*24*time.Hour))
	if err != nil || len(got) != 1 || got[0].JobType != "outbound_webhook" || got[0].Count != 2 {
		t.Fatalf("FailedByTypeSince = %+v err %v, want only tenant A outbound_webhook x2", got, err)
	}

	stuckA := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusQueued, "", now.Add(-time.Hour))
	tjSeed(t, db, store, tjTenantB, "outbound_webhook", StatusQueued, "", now.Add(-time.Hour))
	if _, err := db.Exec(`UPDATE sys_jobs SET run_at = ? WHERE id = ?`, now.Add(-20*time.Minute), stuckA); err != nil {
		t.Fatal(err)
	}
	if n, err := store.StuckQueuedForTenant(ctx, tjTenantA, now.Add(-15*time.Minute)); err != nil || n != 1 {
		t.Fatalf("StuckQueuedForTenant = %d err %v, want 1", n, err)
	}

	for name, q := range map[string]string{
		"idx_sys_jobs_tenant_list": `EXPLAIN SELECT /*+ INDEX(sys_jobs idx_sys_jobs_tenant_list) */ job_type, COUNT(*), MAX(created_at) FROM sys_jobs WHERE tenant_id = 'x' AND created_at >= NOW() AND status = 'failed' GROUP BY job_type`,
		"idx_sys_jobs_claim":       `EXPLAIN SELECT /*+ INDEX(sys_jobs idx_sys_jobs_claim) */ COUNT(*) FROM sys_jobs WHERE status = 'queued' AND run_at < NOW() AND tenant_id = 'x'`,
	} {
		rows, err := db.Queryx(q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			m := map[string]any{}
			if err := rows.MapScan(m); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, strings.TrimSpace(string(toBytes(m["key"]))))
		}
		_ = rows.Close()
		if !strings.Contains(strings.Join(plan, ","), name) {
			t.Errorf("EXPLAIN keys %v, want %s", plan, name)
		}
	}
}

func toBytes(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return nil
}
