//go:build integration

package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// seedGaugeWorld: tenant A has an active, a disabled and a deleted user; sign-ins on the day and 20
// days before; a record table with live + binned rows; two attachments (one binned). Tenant B owns
// look-alikes of everything that must not leak into A's numbers.
func seedGaugeWorld(t *testing.T, db *sqlx.DB) (entity string) {
	t.Helper()
	slugA, slugB := "ug-a-"+uuid.NewString()[:8], "ug-b-"+uuid.NewString()[:8]
	for _, r := range [][2]string{{tenA, slugA}, {tenB, slugB}} {
		must(t, db, `INSERT INTO tenants (id, slug, name) VALUES (?, ?, 'ug') ON DUPLICATE KEY UPDATE name = name`, r[0], r[1])
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM refresh_tokens WHERE tenant_id IN (?, ?)`, `DELETE FROM users WHERE tenant_id IN (?, ?)`,
			`DELETE FROM sys_files WHERE tenant_id IN (?, ?)`, `DELETE FROM sys_usage_daily WHERE tenant_id IN (?, ?)`,
			`DELETE FROM tenants WHERE id IN (?, ?)`} {
			_, _ = db.Exec(q, tenA, tenB)
		}
	})
	user := func(tenant, status string, deleted bool) string {
		id := uuid.NewString()
		del := "NULL"
		if deleted {
			del = "NOW()"
		}
		must(t, db, `INSERT INTO users (id, tenant_id, email, password_hash, status, deleted_at) VALUES (?, ?, ?, 'x', ?, `+del+`)`,
			id, tenant, id+"@ug.test", status)
		return id
	}
	token := func(tenant, userID string, at time.Time) {
		must(t, db, `INSERT INTO refresh_tokens (id, user_id, tenant_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), userID, tenant, strings.Repeat("a", 64), at.Add(24*time.Hour), at)
	}
	u1, u2 := user(tenA, "active", false), user(tenA, "disabled", false)
	user(tenA, "active", true)
	u4 := user(tenB, "active", false)
	in := dayFrom.Add(time.Hour)
	token(tenA, u1, in)
	token(tenA, u1, in.Add(time.Hour)) // a refresh of the same person
	token(tenA, u2, in)
	token(tenA, u1, dayFrom.Add(-20*24*time.Hour))
	token(tenB, u4, in)

	entity = "ug" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	must(t, db, "CREATE TABLE `obj_"+entity+"` (id CHAR(36) PRIMARY KEY, tenant_id CHAR(36) NOT NULL, deleted_at TIMESTAMP(3) NULL, "+
		"created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), KEY idx_created_at (tenant_id, deleted_at, created_at, id))")
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE `obj_" + entity + "`") })
	for _, r := range [][2]string{{tenA, "NULL"}, {tenA, "NULL"}, {tenA, "NOW(3)"}, {tenB, "NULL"}} {
		must(t, db, "INSERT INTO `obj_"+entity+"` (id, tenant_id, deleted_at) VALUES (?, ?, "+r[1]+")", uuid.NewString(), r[0])
	}
	file := func(tenant string, size int, binned bool) {
		del := "NULL"
		if binned {
			del = "NOW(3)"
		}
		must(t, db, `INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, path, created_by, deleted_at)
			VALUES (?, ?, 'e', 'r', 'f', 'n', ?, 'p', 'u', `+del+`)`, uuid.NewString(), tenant, size)
	}
	file(tenA, 100, false)
	file(tenA, 200, true) // in the bin: the bytes are still on disk
	file(tenB, 999, false)
	return entity
}

func TestUsersSeenAndGauges_Integration(t *testing.T) {
	db, r := usageDB(t)
	ctx := context.Background()
	entity := seedGaugeWorld(t, db)

	got, err := r.CountersForDay(ctx, tenA, dayFrom, dayTo)
	if err != nil {
		t.Fatal(err)
	}
	if v := got[Key{UsersSeen, ""}]; v != 2 {
		t.Errorf("users_seen = %d; want 2 DISTINCT people (u1 twice counts once; tenant B's sign-in and the 20-days-old one do not)", v)
	}
	if k := explainKeys(t, db, `SELECT COUNT(DISTINCT user_id) FROM refresh_tokens WHERE tenant_id = ? AND created_at >= ? AND created_at < ?`, tenA, dayFrom, dayTo); !strings.Contains(k, "idx_rt_tenant_created") {
		t.Errorf("users_seen plan key = %q; want idx_rt_tenant_created", k)
	}

	g, err := r.Gauges(ctx, tenA, dayFrom.Add(-29*24*time.Hour), []string{entity, "ug_no_table_zz"}, nil)
	if err != nil {
		t.Fatalf("Gauges: %v (one missing table must not fail the block)", err)
	}
	if g.UsersActive != 1 {
		t.Errorf("users_active = %d; want 1 (disabled and deleted accounts do not count)", g.UsersActive)
	}
	if g.UsersSeen30d != 2 {
		t.Errorf("users_seen_30d = %d; want 2", g.UsersSeen30d)
	}
	if len(g.Records) != 1 || g.Records[0].Entity != entity || g.Records[0].Count != 2 {
		t.Errorf("records = %+v; want only %s=2 (binned row, tenant B's row and the table-less entity are out)", g.Records, entity)
	}
	if g.FileBytes != 300 || g.FileCount != 2 {
		t.Errorf("files = %d bytes / %d; want 300 / 2 (the binned file still weighs)", g.FileBytes, g.FileCount)
	}

	// the stored snapshot round-trips
	day := "2026-10-14"
	if err := r.Upsert(ctx, g.Rows(tenA, day), Overwrite); err != nil {
		t.Fatal(err)
	}
	back, at, err := r.LatestGauge(ctx, tenA, day)
	if err != nil || back == nil || at == nil || back.FileBytes != 300 || back.UsersActive != 1 || len(back.Records) != 1 {
		t.Fatalf("LatestGauge = %+v %v %v", back, at, err)
	}
	if none, _, err := r.LatestGauge(ctx, tenB, day); err != nil || none != nil {
		t.Errorf("tenant B has no snapshot; got %+v %v", none, err)
	}
	// a hard delete of the binned file lowers the figure at the next snapshot
	must(t, db, `DELETE FROM sys_files WHERE tenant_id = ? AND deleted_at IS NOT NULL`, tenA)
	g2, err := r.Gauges(ctx, tenA, dayFrom, nil, nil)
	if err != nil || g2.FileBytes != 100 || g2.FileCount != 1 {
		t.Errorf("after purge: %+v %v; want 100 / 1", g2, err)
	}
}
