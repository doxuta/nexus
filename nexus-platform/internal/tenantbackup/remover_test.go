package tenantbackup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nexus/platform/internal/storage"
)

func TestRemoveTenantDir_RefusesAnythingButATenantUUID(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(filepath.Dir(root), "victim-"+filepath.Base(root))
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(victim)
	for _, bad := range []string{"", ".", "..", "../" + filepath.Base(victim), tenantA + "/../x", "a/b", "not-a-uuid"} {
		if err := RemoveTenantDir(root, bad); err == nil {
			t.Errorf("RemoveTenantDir(%q) must refuse", bad)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a refused removal must delete nothing outside the root: %v", err)
	}
	if err := RemoveTenantDir(root, tenantA); err != nil {
		t.Errorf("a missing customer directory is not an error: %v", err)
	}
}

func TestNewRemover_RemovesKeysThenTheWholeDirectory(t *testing.T) {
	root := t.TempDir()
	be, err := storage.NewLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte("z"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	mk(tenantA + "/one.zip")
	mk(tenantA + "/two.zip")
	mk(tenantA + "/stray.zip") // no row names it
	mk(tenantB + "/keep.zip")  // another customer: untouched

	n, err := NewRemover(be, root)(context.Background(), tenantA, []string{tenantA + "/one.zip", tenantA + "/two.zip"})
	if err != nil || n != 2 {
		t.Fatalf("remover = %d, %v; want 2, nil", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, tenantA)); !os.IsNotExist(err) {
		t.Errorf("the customer's directory, stray file included, must be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, tenantB, "keep.zip")); err != nil {
		t.Errorf("another customer's backup must survive: %v", err)
	}

	// a key outside the customer's directory is refused, never deleted
	mk(tenantB + "/other.zip")
	n, err = NewRemover(be, root)(context.Background(), tenantA, []string{tenantB + "/other.zip"})
	if err == nil || n != 0 {
		t.Errorf("a foreign key must be refused: %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, tenantB, "other.zip")); err != nil {
		t.Errorf("the foreign file must survive: %v", err)
	}
}
