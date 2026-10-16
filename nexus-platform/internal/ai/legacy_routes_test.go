package ai_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Guards against Go-side strings (persona, notifications) pointing at Studio routes that kh7 deleted.
var legacyRoute = regexp.MustCompile(`/studio/(model|layout|relationships|workflows|approvals|workqueues|hooks|branding|dashboards|reports|deploy|apps)\b|/admin/(doctor|api-keys)\b`)

func TestNoLegacyStudioRoutesInUserFacingStrings(t *testing.T) {
	root := filepath.Join("..") // internal/
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		// API paths (/admin/doctor/scan...) live in handlers; only scan persona + notification text sources.
		if !(strings.HasSuffix(p, ".md") || strings.HasSuffix(p, "doctor_scheduler.go")) {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if m := legacyRoute.FindString(string(b)); m != "" {
			t.Errorf("%s still mentions deleted route %q", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
