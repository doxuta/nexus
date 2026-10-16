// Package credential is the named-credential vault's domain + store (../specs/050-named-credentials):
// one sys_named_credentials row = one way to call a partner system — base URL, auth type,
// non-secret parameters, and the secret sealed AES-256-GCM under the master keyring
// (internal/crypto.Keyring), bound to (tenant, id, slot) so an envelope copied elsewhere never
// opens. Fixed system table ⇒ GORM (rules/database.md); every query is pinned to tenant_id.
//
// This package never seals or opens: the service holds the keyring. Its validators are pure so
// every write path (admin API, pack install, Doctor) runs the same body.
package credential

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// AuthType is how a request authenticates to the partner (immutable per credential).
type AuthType string

const (
	AuthHeader   AuthType = "header"    // <headerName>: <headerPrefix><secret>
	AuthBasic    AuthType = "basic"     // Authorization: Basic base64(username:secret)
	AuthHMAC     AuthType = "hmac"      // X-Nexus-Signature (V1) + timestamp + V2
	AuthURLToken AuthType = "url_token" // secret substituted into {secret} of the base URL path
)

// Status is DERIVED from the stored data (plan D-16) — there is no status column to drift.
type Status string

const (
	StatusReady          Status = "ready"
	StatusNeedsSecret    Status = "needs_secret"    // a slot created without a secret
	StatusGrace          Status = "grace"           // hmac rotated, previous key still signing V2-Previous
	StatusKeyUnavailable Status = "key_unavailable" // sealed under a master key this server lacks
)

// Slot names one sealed column; it is part of the AAD so an envelope cannot move between slots.
type Slot string

const (
	SlotSecret       Slot = "secret"
	SlotPrevSecret   Slot = "prev_secret"
	SlotExtraHeaders Slot = "extra_headers"
)

// AAD is the GCM additional data binding an envelope to its row and slot (plan D-3).
func AAD(tenantID, id string, slot Slot) []byte {
	return []byte(tenantID + "|" + id + "|" + string(slot))
}

// Credential is one sys_named_credentials row. The sealed columns and the master-key id are
// json:"-": audit diffs are built with json.Marshal (audit.StructToMap), so they can never reach
// the trail. The generated column name_live (unique live name) is deliberately NOT mapped —
// writing a generated column is MySQL error 3105.
type Credential struct {
	ID           string   `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID     string   `gorm:"type:char(36);not null" json:"-"`
	Name         string   `gorm:"type:varchar(64);not null" json:"name"`
	Label        string   `gorm:"type:varchar(150);not null" json:"label"`
	BaseURL      string   `gorm:"column:base_url;type:varchar(500);not null" json:"baseUrl"`
	AuthType     AuthType `gorm:"type:varchar(16);not null" json:"authType"`
	HeaderName   string   `gorm:"type:varchar(100)" json:"headerName,omitempty"`
	HeaderPrefix string   `gorm:"type:varchar(32)" json:"headerPrefix,omitempty"`
	Username     string   `gorm:"type:varchar(200)" json:"username,omitempty"`

	SecretEnc        *string    `gorm:"type:text" json:"-"` // NULL = slot waiting for its secret
	SecretHint       string     `gorm:"type:varchar(8)" json:"secretHint,omitempty"`
	PrevSecretEnc    *string    `gorm:"type:text" json:"-"`
	PrevExpiresAt    *time.Time `json:"previousExpiresAt,omitempty"`
	ExtraHeadersEnc  *string    `gorm:"type:text" json:"-"` // envelope of JSON {name: value}
	ExtraHeaderNames []string   `gorm:"serializer:json" json:"extraHeaderNames,omitempty"`
	KeyID            *string    `gorm:"type:varchar(32)" json:"-"` // master key sealing EVERY envelope of the row

	RotatedAt *time.Time `json:"rotatedAt,omitempty"`
	CreatedBy string     `gorm:"type:char(36)" json:"createdBy,omitempty"`
	UpdatedBy string     `gorm:"type:char(36)" json:"updatedBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	DeletedAt *time.Time `json:"-"`
}

// TableName pins the fixed system table.
func (Credential) TableName() string { return "sys_named_credentials" }

// Status derives the displayed state at now. hasKey reports whether this server holds a master
// key id (crypto.Keyring.Has; nil = vault not configured). Ids carry a fingerprint of the key, so a
// DB restored onto a different key under the same label also lands here. A row sealed under a
// missing key is key_unavailable before anything else — nothing sealed on it can be used.
func (c *Credential) Status(now time.Time, hasKey func(keyID string) bool) Status {
	if c.KeyID != nil && *c.KeyID != "" && (hasKey == nil || !hasKey(*c.KeyID)) {
		return StatusKeyUnavailable
	}
	if c.SecretEnc == nil {
		return StatusNeedsSecret
	}
	if c.PrevSecretEnc != nil && c.PrevExpiresAt != nil && c.PrevExpiresAt.After(now) {
		return StatusGrace
	}
	return StatusReady
}

// Validation sentinels; the service maps each to its API code (CREDENTIAL_NAME_INVALID,
// CREDENTIAL_BASE_URL_INVALID, CREDENTIAL_HEADER_INVALID).
var (
	ErrNameInvalid    = errors.New("credential: invalid name")
	ErrBaseURLInvalid = errors.New("credential: invalid base URL")
	ErrHeaderInvalid  = errors.New("credential: invalid header")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidateName checks the immutable machine name: a lowercase slug, at most 64 characters.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must be 1-64 of a-z 0-9 _ - starting with a letter", ErrNameInvalid, name)
	}
	return nil
}

const maxBaseURLLen = 500 // sys_named_credentials.base_url VARCHAR(500)

// ValidateBaseURL checks a partner base URL (FR-010): https, or http only to a loopback host
// unless allowInsecureHTTP (CREDENTIAL_ALLOW_INSECURE_HTTP); a host; no userinfo, query,
// fragment or whitespace — a credential must never ride in the URL itself. url_token's
// {secret} placeholder is checked by ValidateURLTokenBase.
func ValidateBaseURL(raw string, allowInsecureHTTP bool) error {
	if raw == "" || len(raw) > maxBaseURLLen {
		return fmt.Errorf("%w: must be 1-%d characters", ErrBaseURLInvalid, maxBaseURLLen)
	}
	if strings.ContainsAny(raw, "?#") || strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return fmt.Errorf("%w: no query, fragment, whitespace or control characters", ErrBaseURLInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error quotes the whole URL, which may carry a password — never echo it.
		return fmt.Errorf("%w: not a parseable URL", ErrBaseURLInvalid)
	}
	if u.User != nil {
		return fmt.Errorf("%w: no user:password@ in the URL — use a basic credential", ErrBaseURLInvalid)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: must be an absolute URL with a host", ErrBaseURLInvalid)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecureHTTP && !isLoopback(u.Hostname()) {
			return fmt.Errorf("%w: http is only allowed to a loopback host — use https", ErrBaseURLInvalid)
		}
	default:
		return fmt.Errorf("%w: scheme must be https", ErrBaseURLInvalid)
	}
	return nil
}

// SecretPlaceholder is where a url_token credential's secret goes in its base URL.
const SecretPlaceholder = "{secret}"

// ValidateURLTokenBase checks, on top of ValidateBaseURL, that a url_token base holds exactly one
// {secret} and that it sits in the path — never in the host (the key would pick the destination);
// ValidateBaseURL already refuses a query. An encoded look-alike (%7Bsecret%7D) beside the literal
// counts as a second placeholder, because the path is compared decoded.
func ValidateURLTokenBase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || strings.Count(raw, SecretPlaceholder) != 1 || strings.Count(u.Path, SecretPlaceholder) != 1 {
		return fmt.Errorf("%w: a url_token base needs exactly one %s in its path", ErrBaseURLInvalid, SecretPlaceholder)
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SameOrigin reports whether two URLs share scheme, host and port (default ports made explicit)
// — the boundary a plain metadata update may not cross (plan D-12).
func SameOrigin(a, b string) bool {
	oa, okA := origin(a)
	ob, okB := origin(b)
	return okA && okB && oa == ob
}

func origin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port, true
}

const (
	maxHeaderNameLen   = 100  // header_name VARCHAR(100)
	maxExtraHeaders    = 10   // plan: <= 10 extra headers
	maxExtraHeaderSize = 4096 // plan: <= 4KB of names + values
)

// reservedHeaders are set by the HTTP stack or by Nexus itself; X-Nexus-* is checked by prefix.
var reservedHeaders = map[string]bool{
	"host": true, "content-type": true, "content-length": true, "transfer-encoding": true, "connection": true,
}

// ValidateHeaderName accepts an RFC 7230 token that is not reserved (Host, Content-Type,
// Content-Length, Transfer-Encoding, Connection, X-Nexus-*).
func ValidateHeaderName(name string) error {
	if name == "" || len(name) > maxHeaderNameLen {
		return fmt.Errorf("%w: name must be 1-%d characters", ErrHeaderInvalid, maxHeaderNameLen)
	}
	for i := 0; i < len(name); i++ {
		if !isTokenChar(name[i]) {
			return fmt.Errorf("%w: %q is not an HTTP header name", ErrHeaderInvalid, name)
		}
	}
	lower := strings.ToLower(name)
	if reservedHeaders[lower] || strings.HasPrefix(lower, "x-nexus-") {
		return fmt.Errorf("%w: %q is reserved", ErrHeaderInvalid, name)
	}
	return nil
}

// isTokenChar is RFC 7230 tchar: ALPHA / DIGIT / "!#$%&'*+-.^_`|~".
func isTokenChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// ValidateHeaderValue rejects control characters other than horizontal tab — CR/LF would inject
// headers, and net/http refuses to send any of them, so the config-time check matches send time.
func ValidateHeaderValue(value string) error {
	for i := 0; i < len(value); i++ {
		if c := value[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return fmt.Errorf("%w: value contains a control character", ErrHeaderInvalid)
		}
	}
	return nil
}

// ValidateExtraHeaders checks the sealed extra headers as a set: at most 10, at most 4KB of names
// and values together, each name/value valid, no two names equal ignoring case (they would
// collide on the wire).
func ValidateExtraHeaders(headers map[string]string) error {
	if len(headers) > maxExtraHeaders {
		return fmt.Errorf("%w: at most %d extra headers", ErrHeaderInvalid, maxExtraHeaders)
	}
	size, seen := 0, map[string]bool{}
	for name, value := range headers {
		if err := ValidateHeaderName(name); err != nil {
			return err
		}
		if err := ValidateHeaderValue(value); err != nil {
			return fmt.Errorf("header %q: %w", name, err)
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return fmt.Errorf("%w: %q is listed twice", ErrHeaderInvalid, name)
		}
		seen[lower] = true
		size += len(name) + len(value)
	}
	if size > maxExtraHeaderSize {
		return fmt.Errorf("%w: extra headers exceed %d bytes", ErrHeaderInvalid, maxExtraHeaderSize)
	}
	return nil
}
