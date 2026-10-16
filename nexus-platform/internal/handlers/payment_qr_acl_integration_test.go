//go:build integration

// PAYMENT-QR ACL integration test (../specs/016-payment-flow/spec.md §5/§7, queue #53). GET
// /:entity/:id/payment-qr hands out the tenant's bank account plus a record's amount, and until
// now its access control was protected only "by route structure" (it rides the ACL-gated record
// group and RecordQR reads the record under the CALLER's scope). This suite pins that structure
// against the PRODUCTION router (RegisterAPIRoutes with a real ACLService as the aclChecker) over
// a freshly-migrated MySQL 8, with RecordService wired exactly as cmd/server/main.go wires it
// (WithACLAuthorizer + WithFieldACL).
//
// Each denial path pins its OWN status code, and every denial also asserts the security property
// directly — the bank account never appears in the body:
//   - no "view" grant on the entity      → 403 FORBIDDEN (middleware/acl.go refuses before the handler)
//   - view@owner on someone else's row   → 404 RECORD_NOT_FOUND (A5 row-scope inside RecordService.Get)
//   - another tenant's admin             → 404 RECORD_NOT_FOUND (tenant filter; is_admin does not cross tenants)
//   - field hidden by FLS (can_view=0)   → not readable through amountField / memo {{field}}
//   - non-admin PUT of the bank config   → 403 (adminGated) — a rep cannot redirect payments
//
// The top-level name is kept from the queue's acceptance line, but the no-grant path is 403, not
// 404 (queue #53 correction A): 404 only arises when the role CAN view the entity and the row is
// outside its scope.
//
// Run with: go test -tags=integration ./internal/handlers/ -run TestPaymentQR_ACL -v
package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	payQRTenantID  = "payq1000-0000-0000-0000-0000000000a1"
	payQRSlug      = "payq-acme"
	payQRTenant2ID = "payq2000-0000-0000-0000-0000000000b2"
	payQRSlug2     = "payq-other"
	payQREntity    = "it_payqr_invoices"

	// Alphanumeric needles so a substring hit cannot be a coincidental UUID/hex fragment.
	payQRAccount     = "NXQR7788990011"
	payQRAccountName = "CONG TY NXQR ACME"
	payQRSecret      = "SECRET42"
	payQRSecretNum   = 777000123
)

// newPaymentQRRouter is the production /api/v1 surface for this route: Auth + Admin + Record +
// Payment over a non-nil ACL checker. Both Record AND Payment are required — the QR route is
// mounted inside the `h.Record != nil` block — and the ACL checker must be real: with a nil one
// scopeOf fails closed to AccessNone and every denial below would pass for the wrong reason.
func newPaymentQRRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *gin.Engine {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}

	userRepo := auth.NewUserRepository(gdb)
	aclSvc := services.NewACLService(auth.NewRoleRepository(gdb))
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	// Same decorations as cmd/server/main.go (WithACLAuthorizer :636, WithFieldACL :746).
	recordSvc := services.NewRecordService(meta, dyn).
		WithACLAuthorizer(aclSvc).
		WithFieldACL(services.NewFieldACLService(auth.NewFieldACLRepository(gdb)))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:    NewAuthHandler(services.NewAuthService(userRepo, auth.NewRefreshTokenRepository(gdb), tokenSvc), zap.NewNop()),
		Admin:   NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record:  NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Payment: NewPaymentHandler(services.NewTenantPaymentService(userRepo, recordSvc), zap.NewNop()),
	}, zap.NewNop())
	return r
}

func TestPaymentQR_ACL_NonAdminWithoutViewGets404(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newPaymentQRRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, payQRTenantID, payQRSlug, "Acme PaymentQR")
	a4SeedTenant(t, gdb, payQRTenant2ID, payQRSlug2, "Other PaymentQR")

	// viewer: view@all; owner: view@owner + create (seeds its own row); noview: create/edit on the
	// entity but NO view — proves the gate is the view action, not "any grant on the entity".
	viewerRole := a4SeedRole(t, gdb, payQRTenantID, "payq-viewer", []auth.RoleAction{grant(payQREntity, "view")})
	ownerRole := a4SeedRole(t, gdb, payQRTenantID, "payq-owner", []auth.RoleAction{
		{EntityName: payQREntity, Action: "view", AccessLevel: "owner"}, grant(payQREntity, "create"),
	})
	noViewRole := a4SeedRole(t, gdb, payQRTenantID, "payq-noview", []auth.RoleAction{
		grant(payQREntity, "create"), grant(payQREntity, "edit"),
	})
	a4SeedUser(t, gdb, payQRTenantID, "admin@"+payQRSlug, true, nil)
	a4SeedUser(t, gdb, payQRTenantID, "viewer@"+payQRSlug, false, &viewerRole)
	a4SeedUser(t, gdb, payQRTenantID, "owner@"+payQRSlug, false, &ownerRole)
	a4SeedUser(t, gdb, payQRTenantID, "noview@"+payQRSlug, false, &noViewRole)
	a4SeedUser(t, gdb, payQRTenant2ID, "admin@"+payQRSlug2, true, nil)

	a4DropEntity(sx, []string{payQRTenantID, payQRTenant2ID}, payQREntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{payQRTenantID, payQRTenant2ID}, payQREntity) })

	adminTok := itLogin(t, r, payQRSlug, "admin@"+payQRSlug, authPassword).Data.AccessToken
	viewerTok := itLogin(t, r, payQRSlug, "viewer@"+payQRSlug, authPassword).Data.AccessToken
	ownerTok := itLogin(t, r, payQRSlug, "owner@"+payQRSlug, authPassword).Data.AccessToken
	noViewTok := itLogin(t, r, payQRSlug, "noview@"+payQRSlug, authPassword).Data.AccessToken
	admin2Tok := itLogin(t, r, payQRSlug2, "admin@"+payQRSlug2, authPassword).Data.AccessToken

	// The SAME entity in both tenants (one shared obj_ table split by tenant_id), so the
	// cross-tenant probe reaches the row lookup instead of stopping at ENTITY_NOT_FOUND.
	entityBody := map[string]any{
		"name": payQREntity, "label": "Hoá đơn",
		"fields": []map[string]any{
			{"name": "code", "label": "Mã", "fieldType": "text", "isRequired": true},
			{"name": "total", "label": "Tổng tiền", "fieldType": "number"},
			{"name": "secret", "label": "Ghi chú nội bộ", "fieldType": "text"},
			{"name": "secret_num", "label": "Giá vốn", "fieldType": "number"},
		},
	}
	for who, tok := range map[string]string{"tenant1": adminTok, "tenant2": admin2Tok} {
		if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", tok, entityBody, nil); w.Code != http.StatusCreated {
			t.Fatalf("create entity (%s): %d %s", who, w.Code, w.Body.String())
		}
	}
	// FLS: the viewer role cannot view `secret` / `secret_num` (can_view=0).
	for _, f := range []string{"secret", "secret_num"} {
		if _, err := sx.Exec(
			`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 0, 0, NOW(3), NOW(3))`,
			uuid.NewString(), payQRTenantID, viewerRole, payQREntity, f); err != nil {
			t.Fatalf("seed sys_field_acl %s: %v", f, err)
		}
	}

	// Bank config through the real admin PUT (validator + store). Tenant 2 gets its OWN account:
	// RecordQR checks "configured" before reading the record, so an unconfigured tenant 2 would
	// stop at 422 PAYMENT_NOT_CONFIGURED and never exercise the cross-tenant row lookup.
	bank := map[string]any{"bankBin": "970436", "accountNumber": payQRAccount, "accountName": payQRAccountName}
	if w := doAuthed(t, r, http.MethodPut, "/api/v1/admin/tenant/payment", adminTok, bank, nil); w.Code != http.StatusOK {
		t.Fatalf("configure bank: %d %s", w.Code, w.Body.String())
	}
	bank2 := map[string]any{"bankBin": "970415", "accountNumber": "NXQRT2000222"}
	if w := doAuthed(t, r, http.MethodPut, "/api/v1/admin/tenant/payment", admin2Tok, bank2, nil); w.Code != http.StatusOK {
		t.Fatalf("configure bank (tenant2): %d %s", w.Code, w.Body.String())
	}

	create := func(tok string, data map[string]any) string {
		t.Helper()
		w := doAuthed(t, r, http.MethodPost, "/api/v1/"+payQREntity, tok, data, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create record: %d %s", w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Data.ID == "" {
			t.Fatalf("create record: empty id (%s)", w.Body.String())
		}
		return env.Data.ID
	}
	adminInv := create(adminTok, map[string]any{"code": "INV-0001", "total": 1500000, "secret": payQRSecret, "secret_num": payQRSecretNum})
	ownerInv := create(ownerTok, map[string]any{"code": "INV-0002", "total": 250000})

	type qrEnvelope struct {
		Data struct {
			Payload string                 `json:"payload"`
			Amount  int64                  `json:"amount"`
			Memo    string                 `json:"memo"`
			Bank    services.PaymentConfig `json:"bank"`
		} `json:"data"`
		Code string `json:"code"`
	}
	getQR := func(tok, id, amountField, memo string) (int, string, qrEnvelope) {
		t.Helper()
		q := url.Values{"amountField": {amountField}, "memo": {memo}}
		w := doAuthed(t, r, http.MethodGet, "/api/v1/"+payQREntity+"/"+id+"/payment-qr?"+q.Encode(), tok, nil, nil)
		var env qrEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, w.Body.String(), env
	}
	// assertDenied pins the path's exact status + code AND the property that matters on a money
	// surface: neither the account number, its holder name nor the amount is in the body.
	assertDenied := func(t *testing.T, code int, body string, env qrEnvelope, wantStatus int, wantCode string) {
		t.Helper()
		if code != wantStatus || env.Code != wantCode {
			t.Fatalf("got %d %q, want %d %q (body=%s)", code, env.Code, wantStatus, wantCode, body)
		}
		for _, needle := range []string{payQRAccount, payQRAccountName, "1500000"} {
			if strings.Contains(body, needle) {
				t.Fatalf("denied body leaks %q: %s", needle, body)
			}
		}
	}

	t.Run("viewer_with_view_grant_gets_200_with_amount_and_bank", func(t *testing.T) {
		// Positive control: proves the route is reachable for a non-admin and that the leak
		// needles below are findable when they ARE served.
		code, body, env := getQR(viewerTok, adminInv, "total", "TT {{code}}")
		if code != http.StatusOK {
			t.Fatalf("viewer QR: %d %s", code, body)
		}
		if env.Data.Amount != 1500000 || !strings.Contains(env.Data.Payload, "54071500000") {
			t.Fatalf("amount=%d payload=%q; want 1500000 with tag 54 = 07/1500000", env.Data.Amount, env.Data.Payload)
		}
		if env.Data.Bank.AccountNumber != payQRAccount || !strings.Contains(env.Data.Payload, payQRAccount) {
			t.Fatalf("bank=%+v payload=%q; want account %s in both", env.Data.Bank, env.Data.Payload, payQRAccount)
		}
		if env.Data.Memo != "TT INV-0001" {
			t.Fatalf("memo = %q, want %q", env.Data.Memo, "TT INV-0001")
		}
	})

	t.Run("no_view_grant_gets_403_forbidden_without_bank_details", func(t *testing.T) {
		code, body, env := getQR(noViewTok, adminInv, "total", "TT {{code}}")
		assertDenied(t, code, body, env, http.StatusForbidden, "FORBIDDEN")
	})

	t.Run("owner_scope_on_other_users_row_gets_404_without_bank_details", func(t *testing.T) {
		code, body, env := getQR(ownerTok, adminInv, "total", "TT {{code}}")
		assertDenied(t, code, body, env, http.StatusNotFound, "RECORD_NOT_FOUND")
	})

	t.Run("owner_scope_on_own_row_gets_200", func(t *testing.T) {
		// Discriminator for the 404 above: the owner role is not simply locked out.
		code, body, env := getQR(ownerTok, ownerInv, "total", "TT {{code}}")
		if code != http.StatusOK || env.Data.Amount != 250000 {
			t.Fatalf("owner own-row QR: %d amount=%d (body=%s); want 200 / 250000", code, env.Data.Amount, body)
		}
	})

	t.Run("other_tenant_admin_gets_404_without_bank_details", func(t *testing.T) {
		// is_admin bypasses the ACL stage, so this is the strongest caller there is — and it
		// still must not see tenant 1's row or bank account.
		code, body, env := getQR(admin2Tok, adminInv, "total", "TT {{code}}")
		assertDenied(t, code, body, env, http.StatusNotFound, "RECORD_NOT_FOUND")
	})

	t.Run("fls_hidden_field_not_readable_via_memo_or_amount", func(t *testing.T) {
		// Admin reads both hidden fields — proves the data is there, so the viewer's miss below
		// is the FLS strip and not an empty column.
		if code, body, env := getQR(adminTok, adminInv, "secret_num", "{{secret}}"); code != http.StatusOK ||
			env.Data.Amount != payQRSecretNum || env.Data.Memo != payQRSecret {
			t.Fatalf("admin control: %d amount=%d memo=%q (body=%s)", code, env.Data.Amount, env.Data.Memo, body)
		}

		code, body, env := getQR(viewerTok, adminInv, "total", "TT {{secret}}")
		if code != http.StatusOK || strings.Contains(body, payQRSecret) || env.Data.Memo != "TT" {
			t.Fatalf("viewer memo over hidden field: %d memo=%q (body=%s); want 200, memo \"TT\", no %s",
				code, env.Data.Memo, body, payQRSecret)
		}

		code, body, env = getQR(viewerTok, adminInv, "secret_num", "")
		if code != http.StatusUnprocessableEntity || env.Code != "PAYMENT_AMOUNT_INVALID" {
			t.Fatalf("viewer amountField=hidden: %d %q (body=%s); want 422 PAYMENT_AMOUNT_INVALID", code, env.Code, body)
		}
		if strings.Contains(body, "777000123") {
			t.Fatalf("hidden amount leaked: %s", body)
		}
	})

	t.Run("non_admin_cannot_rewrite_bank_account", func(t *testing.T) {
		// Redirecting where customers' money goes is the worst outcome on this surface.
		evil := map[string]any{"bankBin": "970436", "accountNumber": "EVIL000111"}
		if w := doAuthed(t, r, http.MethodPut, "/api/v1/admin/tenant/payment", viewerTok, evil, nil); w.Code != http.StatusForbidden {
			t.Fatalf("viewer PUT bank config: %d %s; want 403", w.Code, w.Body.String())
		}
		// Admin with an invalid BIN: refused 422 on the field, and the stored config is untouched.
		w := doAuthed(t, r, http.MethodPut, "/api/v1/admin/tenant/payment", adminTok,
			map[string]any{"bankBin": "97043", "accountNumber": "EVIL000111"}, nil)
		var bad struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &bad)
		if w.Code != http.StatusUnprocessableEntity || bad.Code != "PAYMENT_INVALID" || bad.Details["field"] != "bankBin" {
			t.Fatalf("invalid BIN PUT: %d %s; want 422 PAYMENT_INVALID on bankBin", w.Code, w.Body.String())
		}
		w = doAuthed(t, r, http.MethodGet, "/api/v1/admin/tenant/payment", adminTok, nil, nil)
		var cur struct {
			Data struct {
				Payment services.PaymentConfig `json:"payment"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &cur)
		if w.Code != http.StatusOK || cur.Data.Payment.AccountNumber != payQRAccount {
			t.Fatalf("bank config changed after refused PUTs: %d %s", w.Code, w.Body.String())
		}
	})
}
