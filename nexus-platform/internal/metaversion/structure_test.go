package metaversion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FR-016: the engine never reads a draft. Only this package may name sys_meta_drafts in production
// code, so no hot path (record save, scheduler, ACL, loader) can come to depend on a draft by accident.
func TestOnlyMetaversionNamesTheDraftsTable(t *testing.T) {
	root := filepath.Join("..") // internal/
	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "metaversion" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The per-customer backup CLASSIFIES every table by name (a draft is configuration that a
		// restore must put back); that file never reads or writes a draft on an engine path
		// (specs/068 D-20). The ONLY allowed exception — keep it one file.
		if filepath.ToSlash(path) == "../tenantbackup/policy.go" {
			return nil
		}
		scanned++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "sys_meta_drafts") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files — the scan is not looking at internal/", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("sys_meta_drafts named outside internal/metaversion: %v", offenders)
	}
}
