package vertical_pack

import (
	"errors"
	"testing"

	"nexus/platform/internal/services"
)

func TestIsValidSemver(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"1.0.0", true},
		{"1.1.0", true},
		{"0.0.1", true},
		{"10.20.30", true},
		{"1.2.3-beta.1", true},
		{"1.0.0+build.5", true},
		{"1.2.3-rc.1+exp.sha.5114f85", true},
		{" 1.0.0 ", true}, // trimmed
		{"1.0", false},
		{"v1.0.0", false},
		{"1.0.0.0", false},
		{"01.2.3", false}, // no leading zeros
		{"", false},
		{"abc", false},
		{"1.0.0-", false},
	}
	for _, tt := range tests {
		t.Run(tt.v, func(t *testing.T) {
			if got := isValidSemver(tt.v); got != tt.want {
				t.Errorf("isValidSemver(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestValidateManifest(t *testing.T) {
	// nil manifest is valid — the manifest is optional.
	if err := validateManifest(nil); err != nil {
		t.Errorf("validateManifest(nil) = %v, want nil", err)
	}
	// valid semver → no error.
	if err := validateManifest(&Manifest{Name: "X", Version: "2.3.4"}); err != nil {
		t.Errorf("validateManifest(valid) = %v, want nil", err)
	}
	// malformed semver → VERTICAL_MANIFEST_INVALID_VERSION (parity with number/rating
	// config-time validators — a mistyped version must not fall through silently, trap B6).
	err := validateManifest(&Manifest{Name: "X", Version: "1.0"})
	if err == nil {
		t.Fatalf("validateManifest(bad version) = nil, want error")
	}
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "VERTICAL_MANIFEST_INVALID_VERSION" {
		t.Errorf("validateManifest(bad version) code = %v, want VERTICAL_MANIFEST_INVALID_VERSION", err)
	}
}

// TestBuiltinManifestsParse asserts every shipped built-in pack carries a manifest whose version
// is valid semver and whose slug matches the registry key — so a mistyped built-in version can
// never reach production undetected.
func TestBuiltinManifestsParse(t *testing.T) {
	for _, slug := range BuiltinSlugs() {
		pack, ok := BuiltinPack(slug)
		if !ok {
			t.Fatalf("BuiltinPack(%q) not found", slug)
		}
		if pack.Manifest == nil {
			t.Errorf("built-in %q has no manifest", slug)
			continue
		}
		if err := validateManifest(pack.Manifest); err != nil {
			t.Errorf("built-in %q manifest invalid: %v", slug, err)
		}
		if pack.Manifest.Slug != slug {
			t.Errorf("built-in %q manifest slug = %q, want %q", slug, pack.Manifest.Slug, slug)
		}
	}
}
