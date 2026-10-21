//go:build integration

// specs/048 Nợ 16 on real MySQL: a file delete whose record write is REFUSED (a before_save veto on
// a locked record) leaves the attachment exactly as it was — the sys_files row live (the download
// still works), the blob on disk — and Restore never reaches across tenants. Run with:
//
//	go test -tags=integration -run TestDelete_RefusedRecordWrite ./internal/files/
package files_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/files"
	"nexus/platform/internal/services"
	"nexus/platform/internal/storage"
)

const (
	drTenantA = "aaaa1111-0000-0000-0000-00000000d401"
	drTenantB = "bbbb2222-0000-0000-0000-00000000d402"
)

// lockGuard authorizes every record and refuses every ref-array write while locked, the way
// RecordService.SetFileRefs answers under an active before_save veto.
type lockGuard struct{ locked bool }

func (g *lockGuard) ResolveFileField(_ context.Context, _ services.RecordScope, _, entity, _, _ string) (string, error) {
	return entity, nil
}

func (g *lockGuard) ExplainWriteMiss(_ context.Context, _ services.RecordScope, _, _, _ string, err error) error {
	return err
}

func (g *lockGuard) SetFileRefs(context.Context, services.RecordScope, string, string, string, string, []map[string]any) error {
	if g.locked {
		return &services.ValidationError{Code: "RECORD_HOOK_VETO", Msg: "Hồ sơ đã khoá, không sửa được"}
	}
	return nil
}

func TestDelete_RefusedRecordWrite_KeepsTheFile_RealMySQL(t *testing.T) {
	db, err := sqlx.Connect("mysql", packageTestDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	clean := func() {
		if _, err := db.ExecContext(ctx, "DELETE FROM sys_files WHERE tenant_id IN (?, ?)", drTenantA, drTenantB); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	backend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	insert := func(tenant, id string, deleted bool) {
		q := `INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, deleted_at)
		      VALUES (?, ?, 'deals', 'rec-1', 'docs', 'hop-dong.pdf', 11, 'application/pdf', ?, 'user-1', IF(?, NOW(3), NULL))`
		if _, err := db.ExecContext(ctx, q, id, tenant, tenant+"/"+id, deleted); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		if _, err := backend.Put(ctx, tenant+"/"+id, strings.NewReader("hello world")); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	insert(drTenantA, "dr-a1", false)
	insert(drTenantB, "dr-b1", true) // another tenant's tombstone — must stay one

	repo := files.NewRepository(db)
	guard := &lockGuard{locked: true}
	svc := files.NewService(repo, backend, guard, files.Limits{}, nil)
	admin := services.RecordScope{UserID: "user-1", IsAdmin: true}

	err = svc.Delete(ctx, admin, drTenantA, "dr-a1")
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_HOOK_VETO" {
		t.Fatalf("locked delete err = %v; want RECORD_HOOK_VETO", err)
	}
	rc, f, err := svc.Open(ctx, admin, drTenantA, "dr-a1")
	if err != nil {
		t.Fatalf("download after a refused delete: %v; want the file intact", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello world" || f.Name != "hop-dong.pdf" {
		t.Fatalf("download = %q (%s); want the original bytes", body, f.Name)
	}

	if err := repo.Restore(ctx, drTenantA, "dr-b1"); err != nil {
		t.Fatalf("cross-tenant Restore: %v", err)
	}
	if _, err := repo.GetByID(ctx, drTenantB, "dr-b1"); !errors.Is(err, files.ErrFileNotFound) {
		t.Fatalf("tenant B tombstone after tenant A Restore: err %v; want still deleted", err)
	}

	guard.locked = false // the rule is switched off: the same delete now goes through
	if err := svc.Delete(ctx, admin, drTenantA, "dr-a1"); err != nil {
		t.Fatalf("unlocked delete: %v", err)
	}
	if _, _, err := svc.Open(ctx, admin, drTenantA, "dr-a1"); !errors.Is(err, files.ErrFileNotFound) {
		t.Fatalf("download after the delete: err %v; want ErrFileNotFound", err)
	}
	if _, err := backend.Open(ctx, drTenantA+"/dr-a1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("blob after the delete: err %v; want gone", err)
	}
}
