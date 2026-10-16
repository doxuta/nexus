package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/workflow"
)

// Named-credential vault — admin API (../specs/050-named-credentials S1b). A customer admin stores a
// partner's base URL + auth type + secret; the secret is sealed AES-256-GCM under the dedicated
// master keyring, bound to AAD(tenant, id, slot), and NEVER read back: no response, audit row, log
// line or error carries a secret, an envelope or a master-key id. The only secret that ever leaves
// is one Nexus generated for an hmac key, in the create response only (mẫu API key, plan D-5).
// Rotation (S4) and the webhook resolver (S2b) build on this service.

const (
	// credentialSecretPrefix marks a Nexus-generated hmac key (like nxk_ for API keys).
	credentialSecretPrefix = "nxw_"
	// credentialSecretBytes is the CSPRNG budget of a generated key: 256 bits → 43 base64url chars.
	credentialSecretBytes = 32
	// credentialMinHMACLen is the floor for a typed-in hmac key (CREDENTIAL_SECRET_WEAK).
	credentialMinHMACLen = 32
	// credentialMaxSecretLen bounds any secret: it must fit a header and its envelope a TEXT column.
	credentialMaxSecretLen = 4096
	// credentialHintMinLen: below this a 4-character hint gives away too much of the secret (FR-004).
	credentialHintMinLen = 16
	credentialLabelMax   = 150 // label VARCHAR(150)
	credentialPrefixMax  = 32  // header_prefix VARCHAR(32)
	credentialUserMax    = 200 // username VARCHAR(200)
	// credentialDefaultHeader / credentialDefaultPrefix: a header credential with neither set is a
	// Bearer token — the common case.
	credentialDefaultHeader = "Authorization"
	credentialDefaultPrefix = "Bearer "
)

// credentialStore is the persistence surface — satisfied by *credential.Repository, faked in tests.
type credentialStore interface {
	Create(ctx context.Context, c *credential.Credential) error
	Get(ctx context.Context, tenantID, id string) (*credential.Credential, error)
	List(ctx context.Context, tenantID string) ([]credential.Credential, error)
	UpdateMetadata(ctx context.Context, tenantID string, c *credential.Credential) error
	WithTxForUpdate(ctx context.Context, tenantID, id string, fn func(c *credential.Credential) error) error
	SoftDeleteShred(ctx context.Context, tenantID, id, deletedBy string) error
	Usages(ctx context.Context, tenantID, id string) ([]credential.Usage, error)
	CountUsagesByTenant(ctx context.Context, tenantID string) (map[string]int, error)
}

// CredentialService is the vault's business layer. keys nil = CREDENTIAL_ENC_KEY absent: every
// method answers CREDENTIALS_NOT_CONFIGURED (503) and nothing is stored (FR-011).
type CredentialService struct {
	repo              credentialStore
	keys              *crypto.Keyring
	allowInsecureHTTP bool // CREDENTIAL_ALLOW_INSECURE_HTTP
	auditor           audit.Auditor
	logger            *zap.Logger
	now               func() time.Time
	genID             func() string
}

// NewCredentialService wires the vault. keys may be nil (vault inert); auditor defaults to a noop
// and logger to a nop until WithAuditor / WithLogger.
func NewCredentialService(repo credentialStore, keys *crypto.Keyring, allowInsecureHTTP bool) *CredentialService {
	return &CredentialService{
		repo: repo, keys: keys, allowInsecureHTTP: allowInsecureHTTP,
		auditor: audit.NewNoopAuditor(), logger: zap.NewNop(), now: time.Now, genID: uuid.NewString,
	}
}

// WithAuditor injects the RM-19 audit dispatcher (create/update/delete → __named_credential__).
func (s *CredentialService) WithAuditor(a audit.Auditor) *CredentialService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithLogger injects the structured logger.
func (s *CredentialService) WithLogger(l *zap.Logger) *CredentialService {
	if l != nil {
		s.logger = l
	}
	return s
}

// CredentialView is the ONLY shape a credential leaves the service in (API responses and audit
// diffs alike). It is an explicit allowlist: a column added to credential.Credential later cannot
// reach a response by forgetting a json:"-".
type CredentialView struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Label             string              `json:"label"`
	BaseURL           string              `json:"baseUrl"`
	AuthType          credential.AuthType `json:"authType"`
	HeaderName        string              `json:"headerName"`
	HeaderPrefix      string              `json:"headerPrefix"`
	Username          string              `json:"username"`
	Status            credential.Status   `json:"status"`
	SecretHint        string              `json:"secretHint"`
	ExtraHeaderNames  []string            `json:"extraHeaderNames"`
	RotatedAt         *time.Time          `json:"rotatedAt"`
	PreviousExpiresAt *time.Time          `json:"previousExpiresAt"`
	UsageCount        int                 `json:"usageCount"`
	CreatedAt         time.Time           `json:"createdAt"`
	UpdatedAt         time.Time           `json:"updatedAt"`
}

func (s *CredentialService) view(c *credential.Credential, usageCount int) CredentialView {
	names := c.ExtraHeaderNames
	if names == nil {
		names = []string{}
	}
	return CredentialView{
		ID: c.ID, Name: c.Name, Label: c.Label, BaseURL: c.BaseURL, AuthType: c.AuthType,
		HeaderName: c.HeaderName, HeaderPrefix: c.HeaderPrefix, Username: c.Username,
		Status: c.Status(s.now(), s.keys.Has), SecretHint: c.SecretHint, ExtraHeaderNames: names,
		RotatedAt: c.RotatedAt, PreviousExpiresAt: c.PrevExpiresAt, UsageCount: usageCount,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// CredentialInput is the create payload. Pointer fields distinguish "absent" from "empty": an
// explicit empty headerPrefix means "no prefix" (X-Api-Key: <secret>).
type CredentialInput struct {
	Name           string
	Label          string
	BaseURL        string
	AuthType       string
	HeaderName     *string
	HeaderPrefix   *string
	Username       *string
	Secret         *string // absent/empty ⇒ a slot waiting for its secret (needs_secret)
	GenerateSecret bool    // hmac only: Nexus mints the key and returns it ONCE
	ExtraHeaders   map[string]string
	ActorID        string
}

// CredentialUpdate is the metadata-only PUT. Name/AuthType are immutable (echoing the current
// value is fine); secrets and extra headers change only through rotation (S4), so sending them
// here is refused rather than silently dropped.
type CredentialUpdate struct {
	Label, BaseURL, HeaderName, HeaderPrefix, Username *string
	Name, AuthType                                     *string
	Secret                                             *string
	GenerateSecret                                     bool
	ExtraHeaders                                       map[string]string
	ActorID                                            string
}

func (s *CredentialService) configured() error {
	if s.keys == nil {
		return invalid("CREDENTIALS_NOT_CONFIGURED", "Máy chủ chưa cấu hình khoá mã hoá cho kho khoá kết nối — liên hệ Nexus")
	}
	return nil
}

func credentialNotFound() error {
	return invalid("CREDENTIAL_NOT_FOUND", "Không tìm thấy khoá kết nối")
}

// Create validates, seals and stores a credential. It returns the view plus the generated secret
// ("" unless GenerateSecret) — the only time a secret leaves the vault.
func (s *CredentialService) Create(ctx context.Context, tenantID string, in CredentialInput) (*CredentialView, string, error) {
	return s.create(ctx, tenantID, in, false)
}

// CredentialLegacyLabelSuffix ends the label of every key the Doctor's C10 step stored from a rule's
// inline key (specs/050 S3b) — the marker FindLegacyImport reuses by, so a re-run mints no duplicate.
const CredentialLegacyLabelSuffix = " (dời từ quy trình)"

// ImportLegacy is Create for the Doctor's C10 step alone (never mounted on HTTP): every check runs
// except CREDENTIAL_SECRET_WEAK — a signing key saved on a rule before the vault existed must keep
// signing V1 byte for byte however short it is (the finding advises rotating it). Nothing is
// generated: the key already exists on the rule.
func (s *CredentialService) ImportLegacy(ctx context.Context, tenantID string, in CredentialInput) (*CredentialView, error) {
	in.GenerateSecret = false
	v, _, err := s.create(ctx, tenantID, in, true)
	return v, err
}

// FindLegacyImport is the reuse-before-create lookup of C10: the id of a live key of the tenant that
// C10 stored (label ends with CredentialLegacyLabelSuffix) at the same base URL with the same auth
// type, which OPENS to exactly want — the signing key and the headers a rule sends today. "" when
// there is none. A candidate that does not open is skipped (logged by ResolveAuth), never reused.
// ponytail: lists the tenant's keys and opens the matching few — keys per tenant are a handful.
func (s *CredentialService) FindLegacyImport(ctx context.Context, tenantID, baseURL string, authType credential.AuthType, want workflow.WebhookAuth) (string, error) {
	if err := s.configured(); err != nil {
		return "", err
	}
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("services.CredentialService.FindLegacyImport: %w", err)
	}
	for i := range rows {
		c := &rows[i]
		if c.BaseURL != baseURL || c.AuthType != authType || !strings.HasSuffix(c.Label, CredentialLegacyLabelSuffix) {
			continue
		}
		got, err := s.ResolveAuth(ctx, tenantID, c.ID)
		var coded *workflow.CodedError
		if errors.As(err, &coded) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("services.CredentialService.FindLegacyImport: %w", err)
		}
		if sameWireAuth(got, want) {
			return c.ID, nil
		}
	}
	return "", nil
}

// sameWireAuth reports whether two resolved auths put the same thing on the wire: the signing keys
// and every header name and value (no header and an empty set are the same).
func sameWireAuth(a, b workflow.WebhookAuth) bool {
	if a.SigningSecret != b.SigningSecret || a.PrevSigningSecret != b.PrevSigningSecret || len(a.Headers) != len(b.Headers) {
		return false
	}
	for k, v := range a.Headers {
		if w, ok := b.Headers[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Configured reports whether the vault holds a master key (C10 refuses to move anything without one).
func (s *CredentialService) Configured() bool { return s.keys != nil }

// AllowsInsecureHTTP reports CREDENTIAL_ALLOW_INSECURE_HTTP: whether a plain-http base URL to a
// non-loopback host can be stored (C10 leaves such actions where they are otherwise).
func (s *CredentialService) AllowsInsecureHTTP() bool { return s.allowInsecureHTTP }

// create is Create's body; legacy (ImportLegacy only) lifts CREDENTIAL_SECRET_WEAK and nothing else.
func (s *CredentialService) create(ctx context.Context, tenantID string, in CredentialInput, legacy bool) (*CredentialView, string, error) {
	if err := s.configured(); err != nil {
		return nil, "", err
	}
	c := &credential.Credential{
		TenantID: tenantID, Name: strings.TrimSpace(in.Name), Label: strings.TrimSpace(in.Label),
		BaseURL: strings.TrimSpace(in.BaseURL), AuthType: credential.AuthType(in.AuthType),
		CreatedBy: in.ActorID, UpdatedBy: in.ActorID,
	}
	if err := credential.ValidateName(c.Name); err != nil {
		return nil, "", invalidField("CREDENTIAL_NAME_INVALID", "name", "Tên máy phải là chữ thường, số, _ hoặc -, bắt đầu bằng chữ, tối đa 64 ký tự")
	}
	if c.Label == "" {
		c.Label = c.Name
	}
	switch c.AuthType {
	case credential.AuthHeader, credential.AuthBasic, credential.AuthHMAC, credential.AuthURLToken:
	default:
		return nil, "", invalidField("CREDENTIAL_AUTH_TYPE_INVALID", "authType", "Kiểu xác thực phải là header, basic, hmac hoặc url_token")
	}
	if c.AuthType == credential.AuthHeader {
		c.HeaderName = credentialDefaultHeader
		if in.HeaderName != nil && strings.TrimSpace(*in.HeaderName) != "" {
			c.HeaderName = strings.TrimSpace(*in.HeaderName)
		}
		if in.HeaderPrefix != nil {
			c.HeaderPrefix = *in.HeaderPrefix
		} else if strings.EqualFold(c.HeaderName, credentialDefaultHeader) {
			c.HeaderPrefix = credentialDefaultPrefix
		}
	}
	if in.Username != nil {
		c.Username = strings.TrimSpace(*in.Username)
	}
	if err := s.validateMetadata(c, in.HeaderName, in.HeaderPrefix, in.Username); err != nil {
		return nil, "", err
	}

	secret := ""
	if in.Secret != nil {
		secret = *in.Secret
	}
	generated := ""
	if in.GenerateSecret {
		if c.AuthType != credential.AuthHMAC {
			return nil, "", invalidField("CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret", "Chỉ khoá HMAC mới để Nexus tự sinh")
		}
		if secret != "" {
			return nil, "", invalidField("CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret", "Chọn một: tự nhập khoá hoặc để Nexus tự sinh")
		}
		var err error
		if generated, err = newCredentialSecret(); err != nil {
			return nil, "", fmt.Errorf("services.CredentialService.Create: generate secret: %w", err)
		}
		secret = generated
	}
	if err := validateCredentialSecret(c, secret, legacy); err != nil {
		return nil, "", err
	}
	if err := validateExtraHeaders(c, in.ExtraHeaders); err != nil {
		return nil, "", err
	}

	// The id is minted before sealing: it is part of every envelope's AAD.
	c.ID = s.genID()
	if err := s.seal(c, secret, in.ExtraHeaders); err != nil {
		return nil, "", fmt.Errorf("services.CredentialService.Create: %w", err)
	}
	if err := s.repo.Create(ctx, c); err != nil {
		if errors.Is(err, credential.ErrNameTaken) {
			return nil, "", invalidField("CREDENTIAL_NAME_TAKEN", "name", "Tổ chức đã có khoá kết nối tên %q", c.Name)
		}
		return nil, "", fmt.Errorf("services.CredentialService.Create: %w", err)
	}
	v := s.view(c, 0)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.NamedCredentialMeta, c.ID, audit.ActionCreate, nil, v)
	s.logger.Info("services.CredentialService: credential created",
		zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID),
		zap.String("auth_type", string(c.AuthType)), zap.String("status", string(v.Status)),
		zap.String("user_id", in.ActorID))
	return &v, generated, nil
}

// seal writes the envelopes of a new row: the secret (if any) and the extra headers (if any), all
// under the active master key, each bound to its own slot. A row with no envelope keeps key_id NULL.
func (s *CredentialService) seal(c *credential.Credential, secret string, extra map[string]string) error {
	if secret != "" {
		env, err := s.keys.SealAAD([]byte(secret), credential.AAD(c.TenantID, c.ID, credential.SlotSecret))
		if err != nil {
			return fmt.Errorf("seal secret: %w", err)
		}
		c.SecretEnc = &env
		if utf8.RuneCountInString(secret) >= credentialHintMinLen {
			r := []rune(secret)
			c.SecretHint = string(r[len(r)-4:])
		}
	}
	if len(extra) > 0 {
		raw, err := json.Marshal(extra)
		if err != nil {
			return fmt.Errorf("encode extra headers: %w", err)
		}
		env, err := s.keys.SealAAD(raw, credential.AAD(c.TenantID, c.ID, credential.SlotExtraHeaders))
		if err != nil {
			return fmt.Errorf("seal extra headers: %w", err)
		}
		c.ExtraHeadersEnc = &env
		names := make([]string, 0, len(extra))
		for n := range extra {
			names = append(names, n)
		}
		sort.Strings(names)
		c.ExtraHeaderNames = names
	}
	if c.SecretEnc != nil || c.ExtraHeadersEnc != nil {
		id := s.keys.ActiveID()
		c.KeyID = &id
	}
	return nil
}

// validateMetadata checks the non-secret columns of c against its auth type. headerName,
// headerPrefix and username are the caller's raw inputs: a non-empty value on a type that does not
// use it is refused (never silently dropped).
func (s *CredentialService) validateMetadata(c *credential.Credential, headerName, headerPrefix, username *string) error {
	if utf8.RuneCountInString(c.Label) > credentialLabelMax {
		return invalidField("CREDENTIAL_NAME_INVALID", "label", "Nhãn tối đa %d ký tự", credentialLabelMax)
	}
	if err := credential.ValidateBaseURL(c.BaseURL, s.allowInsecureHTTP); err != nil {
		return invalidField("CREDENTIAL_BASE_URL_INVALID", "baseUrl", "Địa chỉ gốc phải là https (http chỉ cho máy nội bộ), không kèm tài khoản, query hay #")
	}
	if c.AuthType == credential.AuthURLToken && credential.ValidateURLTokenBase(c.BaseURL) != nil {
		return invalidField("CREDENTIAL_BASE_URL_INVALID", "baseUrl", "Địa chỉ gốc của khoá trong đường dẫn phải chứa đúng một chỗ %s trong phần đường dẫn (không ở tên miền)", credential.SecretPlaceholder)
	}
	if c.AuthType != credential.AuthHeader {
		for field, v := range map[string]*string{"headerName": headerName, "headerPrefix": headerPrefix} {
			if v != nil && *v != "" {
				return invalidField("CREDENTIAL_AUTH_TYPE_INVALID", field, "%s chỉ dùng cho kiểu header", field)
			}
		}
	}
	if c.AuthType != credential.AuthBasic && username != nil && strings.TrimSpace(*username) != "" {
		return invalidField("CREDENTIAL_AUTH_TYPE_INVALID", "username", "username chỉ dùng cho kiểu basic")
	}
	switch c.AuthType {
	case credential.AuthHeader:
		if err := credential.ValidateHeaderName(c.HeaderName); err != nil {
			return invalidField("CREDENTIAL_HEADER_INVALID", "headerName", "Tên header không hợp lệ hoặc thuộc nhóm dành riêng (Host, Content-Type, X-Nexus-*…)")
		}
		if len(c.HeaderPrefix) > credentialPrefixMax || credential.ValidateHeaderValue(c.HeaderPrefix) != nil {
			return invalidField("CREDENTIAL_HEADER_INVALID", "headerPrefix", "Tiền tố tối đa %d ký tự, không chứa ký tự điều khiển", credentialPrefixMax)
		}
		for _, n := range c.ExtraHeaderNames {
			if strings.EqualFold(n, c.HeaderName) {
				return invalidField("CREDENTIAL_HEADER_INVALID", "headerName", "Header %q đã là một header bổ sung của khoá này", n)
			}
		}
	case credential.AuthBasic:
		if utf8.RuneCountInString(c.Username) > credentialUserMax || strings.Contains(c.Username, ":") ||
			credential.ValidateHeaderValue(c.Username) != nil {
			return invalidField("CREDENTIAL_HEADER_INVALID", "username", "Tên đăng nhập tối đa %d ký tự, không chứa dấu : hay ký tự điều khiển", credentialUserMax)
		}
		if c.SecretEnc != nil && c.Username == "" {
			return invalidField("CREDENTIAL_SECRET_REQUIRED", "username", "Khoá basic đã có mật khẩu thì phải có tên đăng nhập")
		}
	}
	return nil
}

// validateCredentialSecret checks a secret about to be sealed ("" = none). allowShortHMAC lifts the
// hmac length floor for a key moved from a rule by the Doctor (ImportLegacy) — nothing else.
func validateCredentialSecret(c *credential.Credential, secret string, allowShortHMAC bool) error {
	if secret == "" {
		return nil
	}
	if len(secret) > credentialMaxSecretLen {
		return invalidField("CREDENTIAL_SECRET_INVALID", "secret", "Khoá dài tối đa %d byte", credentialMaxSecretLen)
	}
	switch c.AuthType {
	case credential.AuthHeader:
		// The secret rides verbatim in a header value: CR/LF would inject headers.
		if credential.ValidateHeaderValue(secret) != nil {
			return invalidField("CREDENTIAL_SECRET_INVALID", "secret", "Khoá chứa ký tự điều khiển, không gửi được trong header")
		}
	case credential.AuthHMAC:
		if len(secret) < credentialMinHMACLen && !allowShortHMAC {
			return invalidField("CREDENTIAL_SECRET_WEAK", "secret", "Khoá HMAC tự nhập phải dài ít nhất %d ký tự — hoặc để Nexus tự sinh", credentialMinHMACLen)
		}
	case credential.AuthBasic:
		if c.Username == "" {
			return invalidField("CREDENTIAL_SECRET_REQUIRED", "username", "Khoá basic có mật khẩu thì phải có tên đăng nhập")
		}
	case credential.AuthURLToken:
		// The token becomes a path segment: a dot segment would climb out of the base's prefix, and a
		// control character is never part of a real token.
		if secret == "." || secret == ".." || strings.IndexFunc(secret, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
			return invalidField("CREDENTIAL_SECRET_INVALID", "secret", "Khoá trong đường dẫn không được là . hoặc .. và không chứa ký tự điều khiển")
		}
	}
	return nil
}

// validateExtraHeaders checks the sealed extra headers, and that none collides with the header the
// auth type itself sets (it would silently overwrite it on the wire).
func validateExtraHeaders(c *credential.Credential, extra map[string]string) error {
	if len(extra) == 0 {
		return nil
	}
	if err := credential.ValidateExtraHeaders(extra); err != nil {
		return invalidField("CREDENTIAL_HEADER_INVALID", "extraHeaders", "Header bổ sung không hợp lệ: tối đa 10 header / 4KB, không trùng tên, không thuộc nhóm dành riêng, không chứa ký tự điều khiển")
	}
	own := ""
	switch c.AuthType {
	case credential.AuthHeader:
		own = c.HeaderName
	case credential.AuthBasic:
		own = credentialDefaultHeader
	}
	for n := range extra {
		if own != "" && strings.EqualFold(n, own) {
			return invalidField("CREDENTIAL_HEADER_INVALID", "extraHeaders", "Header bổ sung %q trùng header xác thực của khoá", n)
		}
	}
	return nil
}

// newCredentialSecret mints nxw_ + base64url(32 CSPRNG bytes).
func newCredentialSecret() (string, error) {
	b := make([]byte, credentialSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("services.newCredentialSecret: %w", err)
	}
	return credentialSecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// List returns every live credential of the tenant with its usage count (rules pointing at it).
func (s *CredentialService) List(ctx context.Context, tenantID string) ([]CredentialView, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.CredentialService.List: %w", err)
	}
	counts, err := s.repo.CountUsagesByTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.CredentialService.List: %w", err)
	}
	out := make([]CredentialView, 0, len(rows))
	for i := range rows {
		out = append(out, s.view(&rows[i], counts[rows[i].ID]))
	}
	return out, nil
}

// Get returns one credential and the rule actions that use it.
func (s *CredentialService) Get(ctx context.Context, tenantID, id string) (*CredentialView, []credential.Usage, error) {
	if err := s.configured(); err != nil {
		return nil, nil, err
	}
	c, err := s.load(ctx, tenantID, id)
	if err != nil {
		return nil, nil, err
	}
	usages, err := s.repo.Usages(ctx, tenantID, c.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.CredentialService.Get: %w", err)
	}
	if usages == nil {
		usages = []credential.Usage{}
	}
	v := s.view(c, ruleCount(usages))
	return &v, usages, nil
}

// ruleCount is the number of distinct rules among usages (a rule may use a key in several actions).
func ruleCount(usages []credential.Usage) int {
	rules := map[string]bool{}
	for _, u := range usages {
		rules[u.WorkflowID] = true
	}
	return len(rules)
}

func (s *CredentialService) load(ctx context.Context, tenantID, id string) (*credential.Credential, error) {
	c, err := s.repo.Get(ctx, tenantID, strings.TrimSpace(id))
	if errors.Is(err, credential.ErrNotFound) {
		return nil, credentialNotFound()
	}
	if err != nil {
		return nil, fmt.Errorf("services.CredentialService.load: %w", err)
	}
	return c, nil
}

// Update changes the non-secret metadata. It loads the row and applies only the fields sent, so
// the repository's full rewrite of the five metadata columns never blanks one the caller left out.
// The base URL may move within its origin only: another scheme/host/port would send the stored
// secret somewhere new, which takes a rotation with a fresh secret (plan D-12).
func (s *CredentialService) Update(ctx context.Context, tenantID, id string, in CredentialUpdate) (*CredentialView, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	c, err := s.load(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != c.Name {
		return nil, invalidField("CREDENTIAL_FIELD_IMMUTABLE", "name", "Không đổi được tên máy của khoá kết nối")
	}
	if in.AuthType != nil && credential.AuthType(*in.AuthType) != c.AuthType {
		return nil, invalidField("CREDENTIAL_FIELD_IMMUTABLE", "authType", "Không đổi được kiểu xác thực — tạo khoá mới")
	}
	// An empty value changes nothing (a form echoing blank fields); a real one would be silently lost.
	for field, sent := range map[string]bool{"secret": in.Secret != nil && *in.Secret != "", "generateSecret": in.GenerateSecret, "extraHeaders": len(in.ExtraHeaders) > 0} {
		if sent {
			return nil, invalidField("CREDENTIAL_FIELD_IMMUTABLE", field, "Bí mật và header bổ sung chỉ đổi qua Xoay khoá")
		}
	}
	before := s.view(c, 0)
	oldBase := c.BaseURL
	if in.Label != nil {
		if c.Label = strings.TrimSpace(*in.Label); c.Label == "" {
			c.Label = c.Name
		}
	}
	if in.BaseURL != nil {
		c.BaseURL = strings.TrimSpace(*in.BaseURL)
	}
	if in.HeaderName != nil && c.AuthType == credential.AuthHeader {
		if c.HeaderName = strings.TrimSpace(*in.HeaderName); c.HeaderName == "" {
			c.HeaderName = credentialDefaultHeader
		}
	}
	if in.HeaderPrefix != nil && c.AuthType == credential.AuthHeader {
		c.HeaderPrefix = *in.HeaderPrefix
	}
	if in.Username != nil && c.AuthType == credential.AuthBasic {
		c.Username = strings.TrimSpace(*in.Username)
	}
	if err := s.validateMetadata(c, in.HeaderName, in.HeaderPrefix, in.Username); err != nil {
		return nil, err
	}
	if !credential.SameOrigin(oldBase, c.BaseURL) {
		return nil, invalidField("CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE", "baseUrl", "Đổi sang máy chủ khác phải làm bằng Xoay khoá kèm bí mật mới")
	}
	c.UpdatedBy = in.ActorID
	if err := s.repo.UpdateMetadata(ctx, tenantID, c); err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return nil, credentialNotFound()
		}
		return nil, fmt.Errorf("services.CredentialService.Update: %w", err)
	}
	// The after-image is the row as stored (updated_at comes from the database).
	saved, err := s.load(ctx, tenantID, c.ID)
	if err != nil {
		return nil, err
	}
	after := s.view(saved, 0)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.NamedCredentialMeta, c.ID, audit.ActionUpdate, before, after)
	s.logger.Info("services.CredentialService: credential updated",
		zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID), zap.String("user_id", in.ActorID))
	usages, err := s.repo.Usages(ctx, tenantID, c.ID)
	if err != nil {
		return nil, fmt.Errorf("services.CredentialService.Update: %w", err)
	}
	after.UsageCount = ruleCount(usages)
	return &after, nil
}

// ResolveAuth opens a credential for ONE send (specs/050 S2b): the engine's
// workflow.CredentialResolver, and the seam any custom outbound code must go through (spec Nợ 11 —
// the name is deliberately not webhook-specific). A point read pinned to the tenant, the envelopes
// opened under their AAD, the wire auth built for the type. A key that cannot be used answers a
// *workflow.CodedError and no auth at all, so the caller sends nothing (FR-016):
//
//	vault inert (no CREDENTIAL_ENC_KEY)     → CREDENTIALS_NOT_CONFIGURED
//	unknown / deleted / another tenant's    → CREDENTIAL_NOT_FOUND
//	slot still waiting for its secret       → CREDENTIAL_NOT_READY
//	ANY failure to open (master key absent,
//	AAD mismatch, tampered envelope)        → CREDENTIAL_KEY_UNAVAILABLE (cause logged, never returned)
//
// A store error stays a plain error — transient, the durable queue retries it.
// ponytail: no cache — one indexed point read + a GCM open per send (< 1 ms), so a rotation is on
// the very next send (plan D-6); cache on (tenant, id, updated_at) if send volume ever needs it.
func (s *CredentialService) ResolveAuth(ctx context.Context, tenantID, credentialID string) (workflow.WebhookAuth, error) {
	if s.keys == nil {
		return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialsNotConfigured,
			Msg: "máy chủ chưa cấu hình khoá mã hoá cho kho khoá kết nối"}
	}
	c, err := s.repo.Get(ctx, tenantID, strings.TrimSpace(credentialID))
	if errors.Is(err, credential.ErrNotFound) {
		return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialNotFound,
			Msg: "không tìm thấy khoá kết nối (đã xoá, hoặc không thuộc tổ chức này)", Err: err}
	}
	if err != nil {
		return workflow.WebhookAuth{}, fmt.Errorf("services.CredentialService.ResolveAuth: %w", err)
	}
	if c.SecretEnc == nil {
		return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialNotReady,
			Msg: fmt.Sprintf("khoá kết nối %q chưa được nhập bí mật", c.Name)}
	}
	open := func(env string, slot credential.Slot) ([]byte, error) {
		pt, oerr := s.keys.OpenAAD(env, credential.AAD(c.TenantID, c.ID, slot))
		if oerr != nil {
			s.logger.Warn("services.CredentialService: credential cannot be opened — nothing sent",
				zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID),
				zap.String("slot", string(slot)), zap.Error(oerr))
			return nil, &workflow.CodedError{Code: workflow.CodeCredentialKeyUnavailable,
				Msg: fmt.Sprintf("không mở được khoá kết nối %q — khoá mã hoá chủ của máy chủ không khớp", c.Name), Err: oerr}
		}
		return pt, nil
	}
	secret, err := open(*c.SecretEnc, credential.SlotSecret)
	if err != nil {
		return workflow.WebhookAuth{}, err
	}
	headers := map[string]string{}
	if c.ExtraHeadersEnc != nil {
		raw, err := open(*c.ExtraHeadersEnc, credential.SlotExtraHeaders)
		if err != nil {
			return workflow.WebhookAuth{}, err
		}
		if err := json.Unmarshal(raw, &headers); err != nil {
			return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialKeyUnavailable,
				Msg: fmt.Sprintf("header bổ sung của khoá kết nối %q hỏng", c.Name), Err: err}
		}
	}
	auth := workflow.WebhookAuth{BaseURL: c.BaseURL, DisplayBase: c.BaseURL, Headers: headers}
	switch c.AuthType {
	case credential.AuthHeader:
		headers[c.HeaderName] = c.HeaderPrefix + string(secret)
	case credential.AuthBasic:
		headers[credentialDefaultHeader] = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+string(secret)))
	case credential.AuthHMAC:
		auth.SigningSecret = string(secret)
		// A rotated key keeps signing V2-Previous until its grace ends — the clock alone ends it.
		if c.PrevSecretEnc != nil && c.PrevExpiresAt != nil && c.PrevExpiresAt.After(s.now()) {
			prev, err := open(*c.PrevSecretEnc, credential.SlotPrevSecret)
			if err != nil {
				return workflow.WebhookAuth{}, err
			}
			auth.PrevSigningSecret = string(prev)
		}
	case credential.AuthURLToken:
		// Only the SEND form carries the token (path-escaped, so a "/" in it cannot add a segment);
		// DisplayBase keeps {secret} for errors, the run history and the dry-run. Every write path
		// refuses a base without exactly one {secret}; a row edited by hand must not go out bare (FR-016).
		if strings.Count(c.BaseURL, credential.SecretPlaceholder) != 1 {
			return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialNotReady,
				Msg: "địa chỉ gốc của khoá trong đường dẫn không còn đúng một chỗ " + credential.SecretPlaceholder}
		}
		auth.BaseURL = strings.Replace(c.BaseURL, credential.SecretPlaceholder, neturl.PathEscape(string(secret)), 1)
	default:
		return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialNotReady,
			Msg: fmt.Sprintf("kiểu xác thực %q chưa gửi được", c.AuthType)}
	}
	return auth, nil
}

// Describe returns a credential's view WITHOUT opening anything — the dry-run's
// credential{id,name,authType,status} and the save-time existence check of a rule naming it.
// Answers CREDENTIAL_NOT_FOUND for an unknown, deleted or cross-tenant id; works on an inert vault.
func (s *CredentialService) Describe(ctx context.Context, tenantID, id string) (*CredentialView, error) {
	c, err := s.load(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	v := s.view(c, 0)
	return &v, nil
}

// Delete refuses while any live rule references the credential (409 with the rules, FR-008);
// otherwise it tombstones the row and shreds every envelope in one statement.
// ponytail: check-then-delete — a rule saved between the two statements keeps a dangling id,
// which the runtime reports as CREDENTIAL_NOT_FOUND (spec Nợ 2); a reference table closes it.
func (s *CredentialService) Delete(ctx context.Context, tenantID, id, actorID string) error {
	if err := s.configured(); err != nil {
		return err
	}
	c, err := s.load(ctx, tenantID, id)
	if err != nil {
		return err
	}
	usages, err := s.repo.Usages(ctx, tenantID, c.ID)
	if err != nil {
		return fmt.Errorf("services.CredentialService.Delete: %w", err)
	}
	if len(usages) > 0 {
		return &ValidationError{
			Code:    "CREDENTIAL_IN_USE",
			Msg:     "Khoá đang được quy trình dùng — gỡ khoá khỏi các quy trình đó trước",
			Details: map[string]any{"usages": usages},
		}
	}
	if err := s.repo.SoftDeleteShred(ctx, tenantID, c.ID, actorID); err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return credentialNotFound()
		}
		return fmt.Errorf("services.CredentialService.Delete: %w", err)
	}
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.NamedCredentialMeta, c.ID, audit.ActionDelete, s.view(c, 0), nil)
	s.logger.Info("services.CredentialService: credential deleted (secrets shredded)",
		zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID), zap.String("user_id", actorID))
	return nil
}
