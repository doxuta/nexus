package tenantbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nexus/platform/internal/storage"
)

// RemoveTenantDir removes the whole `<root>/<tenantID>/` directory of backups. tenantID must be a
// UUID (it has been through the tenant lookup, but this function builds a filesystem path and does
// not trust that) and the result must be a direct child of root: nothing here can name a path
// outside the backup directory.
func RemoveTenantDir(root, tenantID string) error {
	if !uuidRe.MatchString(tenantID) {
		return fmt.Errorf("tenantbackup.RemoveTenantDir: %q is not a tenant id", tenantID)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("tenantbackup.RemoveTenantDir: abs root: %w", err)
	}
	dir := filepath.Join(abs, tenantID)
	if filepath.Dir(dir) != abs {
		return fmt.Errorf("tenantbackup.RemoveTenantDir: %q escapes the backup root", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("tenantbackup.RemoveTenantDir: %w", err)
	}
	return nil
}

// NewRemover builds the closure the tenant purge calls AFTER its transaction commits
// (services.TenantProvisionService.WithBackupRemover): remove each backup file by key, then the
// customer's whole directory. The directory sweep is what makes the purge complete against a race
// — a backup that was writing while the purge ran inserted its row after the purge read the keys
// (FR-060) — and it also takes any stray file nobody has a row for. Returns how many of the given
// keys were removed; a key outside `<tenantID>/` is refused, never deleted.
func NewRemover(backend storage.Backend, root string) func(ctx context.Context, tenantID string, keys []string) (int, error) {
	return func(ctx context.Context, tenantID string, keys []string) (int, error) {
		removed := 0
		var errs []error
		for _, k := range keys {
			if !strings.HasPrefix(k, tenantID+"/") {
				errs = append(errs, fmt.Errorf("tenantbackup.Remover: key %q is outside the customer's directory", k))
				continue
			}
			if err := backend.Delete(ctx, k); err != nil {
				errs = append(errs, err)
				continue
			}
			removed++
		}
		if err := RemoveTenantDir(root, tenantID); err != nil {
			errs = append(errs, err)
		}
		return removed, errors.Join(errs...)
	}
}
