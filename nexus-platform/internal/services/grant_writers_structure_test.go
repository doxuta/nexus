package services

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// grantWriters pins the "ghi mục direct" rule of specs/063 FR-032: every place that writes a role,
// a grant or a field permission either records a direct version entry or is exempt WITH a reason
// (FR-070 "áp ngay, chưa có vết"). A new caller matches neither list => red, which is the
// "connection nobody wrote" bug class: a grant path that silently skips the trail.
var grantWriters = map[string]string{
	// records a direct entry itself
	"services/role_service.go":          "",
	"services/field_acl_admin.go":       "",
	"services/app_acl_seed.go":          "",
	"handlers/admin_role_handler.go":    "calls RoleService, which records",
	"services/publish_revert.go":        "S7-7b undo goes through RoleService / FieldACLAdminService, which record the new entry",
	"services/vertical_pack/install.go": "pack install is refused on a delivered project (PROJECT_DELIVERED_INSTALL_BLOCKED); goes through RoleService anyway",
	// exempt, with the reason
	"services/app_service.go":                        "FR-070: cleanupSeededRoles only deletes UNHELD default roles; SeedForEntity rebuilds them on demand",
	"services/vertical_pack/upgrade_adapters_acl.go": "pack upgrade goes through RoleService, which records",
}

func TestGrantWritersAreTraced(t *testing.T) {
	re := regexp.MustCompile(`\.(CreateRole|UpdateRole|DeleteRole|AddGrants)\(|ReplaceFieldACL\(`)
	root := filepath.Join("..") // internal/
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "auth" { // the repositories that implement them
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if re.Match(b) {
			if _, ok := grantWriters[rel]; !ok {
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("new role/grant/field-ACL writer(s) with no direct version entry and no FR-070 exemption: %v\n"+
			"record a direct entry (VersionRecorder.RecordDirect) or add the file to grantWriters with a reason", offenders)
	}
}
