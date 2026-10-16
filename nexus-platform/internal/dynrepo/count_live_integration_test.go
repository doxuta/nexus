//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func TestCountLive_Integration(t *testing.T) {
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v)", err)
	}
	defer db.Close()
	ctx := context.Background()
	entity := "cl" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	table := "obj_" + entity
	if _, err := db.Exec("CREATE TABLE `" + table + "` (id CHAR(36) PRIMARY KEY, tenant_id CHAR(36) NOT NULL, " +
		"deleted_at TIMESTAMP(3) NULL, created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), " +
		"KEY idx_created_at (tenant_id, deleted_at, created_at, id))"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec("DROP TABLE `" + table + "`") }()
	for _, r := range []struct {
		tenant string
		del    bool
	}{{"ta", false}, {"ta", false}, {"ta", true}, {"tb", false}} {
		del := "NULL"
		if r.del {
			del = "NOW(3)"
		}
		if _, err := db.Exec("INSERT INTO `"+table+"` (id, tenant_id, deleted_at) VALUES (?, ?, "+del+")", uuid.NewString(), r.tenant); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CountLive(ctx, db, entity, "ta"); err != nil || n != 2 {
		t.Fatalf("CountLive(ta) = %d, %v; want 2 (the binned row and the other tenant's row do not count)", n, err)
	}
	rows, err := db.Queryx("EXPLAIN SELECT COUNT(*) FROM `"+table+"` WHERE tenant_id = ? AND deleted_at IS NULL", "ta")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	key := ""
	for rows.Next() {
		m := map[string]any{}
		if err := rows.MapScan(m); err != nil {
			t.Fatal(err)
		}
		if k, ok := m["key"].([]byte); ok {
			key = string(k)
		}
	}
	if key != "idx_created_at" {
		t.Fatalf("plan key = %q; want idx_created_at", key)
	}
	if _, err := CountLive(ctx, db, "bad name;drop", "ta"); err == nil {
		t.Fatal("an invalid entity name must be rejected before any SQL")
	}
	_, err = CountLive(ctx, db, "no_such_table_zz", "ta")
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1146 {
		t.Fatalf("missing table: err = %v; want a wrapped MySQL 1146", err)
	}
}
