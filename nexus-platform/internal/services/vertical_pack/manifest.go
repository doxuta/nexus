package vertical_pack

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// semverPattern is the SemVer 2.0.0 shape the pack manifest accepts: MAJOR.MINOR.PATCH with an
// optional dash-prerelease and/or plus-build metadata. Compiled ONCE at package load (never in a
// per-install/per-request loop — trap B5) and reused by both config-time (built-in manifests) and
// install-time (uploaded packs) validation so the two can never diverge (trap B6 parity).
var semverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// isValidSemver reports whether v is a well-formed semantic version string.
func isValidSemver(v string) bool {
	return semverPattern.MatchString(strings.TrimSpace(v))
}

// validateManifest checks a supplied manifest before it is used. A nil manifest is valid (the
// manifest is optional — install keys off entity names, not the header). A present manifest MUST
// carry a syntactically valid semver Version; anything else is a config error the caller maps to
// 422 VERTICAL_MANIFEST_INVALID_VERSION. This is called BOTH at install-time (uploaded packs) and
// is exercised by a test over every built-in manifest, so a mistyped version can never "fall
// through silently" (trap B6 parity with the number/rating config-time validators).
func validateManifest(m *Manifest) error {
	if m == nil {
		return nil
	}
	if !isValidSemver(m.Version) {
		return invalid("VERTICAL_MANIFEST_INVALID_VERSION",
			"manifest.version %q không phải semver hợp lệ (định dạng MAJOR.MINOR.PATCH)", m.Version)
	}
	// slug is the only free-text manifest field that is BOTH persisted (ledger) AND echoed back
	// (GET /admin/verticals/installs). Constrain it to a safe kebab handle so a pack imported from
	// an untrusted source can't smuggle markup into an admin's install-history view (security 🔵-2).
	if s := strings.TrimSpace(m.Slug); s != "" && !slugPattern.MatchString(s) {
		return invalid("VERTICAL_MANIFEST_INVALID_SLUG",
			"manifest.slug %q không hợp lệ (chỉ a-z, 0-9, gạch ngang; tối đa 150 ký tự)", m.Slug)
	}
	return nil
}

// slugPattern is the allowlist for a pack slug — compiled once (trap B5), lowercase kebab handle.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,149}$`)

// InstallRecord is one row of the install ledger (sys_pack_installs): a durable note that a pack
// of a given slug+version was installed into a tenant at a point in time, with the InstallResult
// captured as Result. The FE reads these (GET /admin/verticals/installs) to show "đã cài v1.1.0
// ngày…". Slug/Version come from the pack's Manifest (empty when a legacy manifest-less pack is
// installed). Written fail-open: a failed insert is logged, never breaks the install itself.
type InstallRecord struct {
	ID          string         `json:"id"`
	TenantID    string         `json:"-"` // tenant-scoped; never echoed back in the payload
	Slug        string         `json:"slug"`
	Version     string         `json:"version"`
	InstalledAt time.Time      `json:"installedAt"`
	Result      map[string]any `json:"result,omitempty"`
}

// installStore persists and lists the pack-install ledger. Optional dependency: when nil, install
// skips recording (no-op) and ListInstalls returns an empty slice. Satisfied by
// *packinstall.Repository. RecordInstall is best-effort (fail-open); ListInstalls is tenant-scoped
// and newest-first.
type installStore interface {
	RecordInstall(ctx context.Context, rec InstallRecord) error
	ListInstalls(ctx context.Context, tenantID string) ([]InstallRecord, error)
}

// newInstallID mints a UUID for one ledger row.
func newInstallID() string { return uuid.NewString() }

// nowUTC is the ledger timestamp source (UTC), factored out so a test can reason about it.
func nowUTC() time.Time { return time.Now().UTC() }

// installResultToMap turns the typed InstallResult into a generic map for the JSON result_json
// column, via a JSON round-trip so the stored shape matches the API payload exactly. A nil result
// (never expected on the success path) yields nil. Not a hot path (once per install).
func installResultToMap(res *InstallResult) map[string]any {
	if res == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
