//go:build integration

// A7 Security Alerts — integration suite (../specs/014-security-alerts/spec.md §7, the two "Integration"
// rows). Run with:
//
//	go test -tags=integration -race ./internal/handlers/
//
// It requires a reachable MySQL 8 (the docker-compose `mysql` service, `make docker-up`) AND the
// golang-migrate CLI on PATH or at $GOPATH/bin/migrate (set A4_MIGRATE_BIN to override) — the same
// preconditions as the A4 suite. It reuses the A4 migrated-DB harness (a4SetupMigratedDB) which
// brings a THROWAWAY database up with the REAL `migrate up` from version 0; because the A7 table
// lives in migration 000009_create_security_alerts, this suite additionally asserts the schema
// reached version >= 9 (so the security_alerts table came from the real migration, never from
// AutoMigrate — bài học integration drift: migrate-từ-0 trên DB SẠCH, KHÔNG tin AutoMigrate).
//
// WHAT IS EXERCISED (end to end, through the production HTTP surface):
//
//	login (admin) → PUT /admin/users/:id (password change) → UserService.UpdateUser emits the
//	E1 password_changed SecurityEvent → AsyncDispatcher (real) → EmailSender (test) +
//	notify.SQLAlertRepository (real, MySQL) writes/updates a row in security_alerts.
//
// Only the EmailSender is a test double (a fake recorder, a real in-memory SMTP server, or a
// failing sender). The dispatcher, the audit repository, the user service, the router middleware
// chain, the MySQL schema and the migration are all the genuine production code paths — NO
// production source is modified for this suite (helpers live here, in *_integration_test.go).
package handlers

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/services"
)

// ----------------------------------------------------------------------------------------------
// Test EmailSender doubles (the ONLY production-path substitution; everything else is real).
// ----------------------------------------------------------------------------------------------

// recordingSender is an in-memory notify.EmailSender that records every Mail it is asked to send.
// It lets a test assert the dispatcher rendered and delivered exactly one message with the right
// To/Subject/Body without standing up a network server (used for the audit/tenant-isolation
// tests where the SMTP wire format is not the thing under test). Safe for concurrent use by the
// dispatcher's worker pool.
type recordingSender struct {
	mu    sync.Mutex
	mails []notify.Mail
}

func (s *recordingSender) Send(_ context.Context, m notify.Mail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mails = append(s.mails, m)
	return nil
}

func (s *recordingSender) snapshot() []notify.Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notify.Mail, len(s.mails))
	copy(out, s.mails)
	return out
}

// failingSender is a notify.EmailSender that always returns an error, simulating a down/broken
// SMTP server. It proves the fail-open contract: the originating business flow (password change)
// must still succeed (HTTP 200) and the audit row must end up status=failed.
type failingSender struct {
	mu    sync.Mutex
	calls int
}

func (s *failingSender) Send(context.Context, notify.Mail) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return &smtpDownError{}
}

type smtpDownError struct{}

func (*smtpDownError) Error() string { return "simulated SMTP failure: connection refused" }

// ----------------------------------------------------------------------------------------------
// In-memory SMTP server + a real-protocol test sender (the "SMTP giả" of spec §7).
//
// The server speaks just enough plaintext ESMTP (greeting, EHLO, MAIL FROM, RCPT TO, DATA) to
// capture a single delivered message. inMemSMTPSender is a notify.EmailSender that performs a
// GENUINE SMTP wire exchange against it (EHLO → MAIL → RCPT → DATA → QUIT) using the production
// notify.buildMIME-equivalent envelope, so the test exercises the real SMTP protocol shape rather
// than an in-process function call. We deliberately do NOT use the production notify.SMTPSender
// here: it mandates STARTTLS and verifies the server certificate against cfg.Host, which a
// self-signed in-memory cert cannot satisfy without modifying production TLS config — out of
// scope and forbidden (no production changes). The protocol exchange below is the same MAIL/RCPT/
// DATA contract, captured server-side, which is exactly what the §7 row asserts: "nhận đúng 1
// mail, đúng To, đúng subject".
// ----------------------------------------------------------------------------------------------

// capturedMail is what the in-memory SMTP server records for one delivered message.
type capturedMail struct {
	from string
	to   string
	data string // full DATA payload (headers + body)
}

// inMemSMTPServer is a minimal single-connection-at-a-time ESMTP server. It records every fully
// delivered message (DATA terminated by a lone "."). It is started on 127.0.0.1:0 and serves
// until Close. Safe for concurrent reads of received().
type inMemSMTPServer struct {
	ln       net.Listener
	mu       sync.Mutex
	received []capturedMail
	wg       sync.WaitGroup
	closed   chan struct{}
}

// newInMemSMTPServer starts the server on a free loopback port and returns it; the caller must
// call Close (typically via t.Cleanup).
func newInMemSMTPServer(t *testing.T) *inMemSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("inmem smtp listen: %v", err)
	}
	s := &inMemSMTPServer{ln: ln, closed: make(chan struct{})}
	s.wg.Add(1)
	go s.serve()
	return s
}

func (s *inMemSMTPServer) addr() string { return s.ln.Addr().String() }

func (s *inMemSMTPServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return // listener closed → orderly shutdown
			default:
				return
			}
		}
		s.handle(conn)
	}
}

// handle plays one SMTP session, recording the delivered message on QUIT (or connection end).
func (s *inMemSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	w := func(line string) { _, _ = conn.Write([]byte(line)) }

	var from, to string
	var body strings.Builder
	inData := false
	have := false

	w("220 inmem ESMTP ready\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if have {
				s.record(from, to, body.String())
			}
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				have = true
				w("250 2.0.0 Ok: queued\r\n")
				continue
			}
			// RFC 5321 transparency: a leading dot is doubled; undo it.
			body.WriteString(strings.TrimPrefix(line, "."))
			continue
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// Advertise no extensions: this is a plaintext capture server (see file header).
			w("250 inmem\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			from = extractAddr(line)
			w("250 2.1.0 Ok\r\n")
		case strings.HasPrefix(cmd, "RCPT TO"):
			to = extractAddr(line)
			w("250 2.1.5 Ok\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			inData = true
			w("354 End data with <CR><LF>.<CR><LF>\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			if have {
				s.record(from, to, body.String())
			}
			w("221 2.0.0 Bye\r\n")
			return
		case strings.HasPrefix(cmd, "RSET"):
			from, to, body, inData, have = "", "", strings.Builder{}, false, false
			w("250 2.0.0 Ok\r\n")
		default:
			w("250 2.0.0 Ok\r\n")
		}
	}
}

func (s *inMemSMTPServer) record(from, to, data string) {
	s.mu.Lock()
	s.received = append(s.received, capturedMail{from: from, to: to, data: data})
	s.mu.Unlock()
}

func (s *inMemSMTPServer) messages() []capturedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedMail, len(s.received))
	copy(out, s.received)
	return out
}

func (s *inMemSMTPServer) Close() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	_ = s.ln.Close()
	s.wg.Wait()
}

// extractAddr pulls the address out of "MAIL FROM:<x@y>" / "RCPT TO:<x@y>", tolerating spacing.
func extractAddr(line string) string {
	i := strings.Index(line, "<")
	j := strings.Index(line, ">")
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	// Fallback: take whatever follows the colon.
	if c := strings.Index(line, ":"); c >= 0 {
		return strings.TrimSpace(line[c+1:])
	}
	return strings.TrimSpace(line)
}

// inMemSMTPSender is a notify.EmailSender that delivers each Mail to the in-memory SMTP server via
// a genuine SMTP protocol exchange (real EHLO/MAIL/RCPT/DATA over TCP). The DATA payload mirrors
// the production envelope shape (From/To/Subject headers + body), so the server-side capture can
// assert To and Subject exactly as a real MTA would see them.
type inMemSMTPSender struct {
	addr string
	from string
}

func (s *inMemSMTPSender) Send(ctx context.Context, m notify.Mail) error {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	expect := func(want byte) error {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if len(line) == 0 || line[0] != want {
			return &smtpProtoError{got: line}
		}
		return nil
	}
	send := func(line string) { _, _ = conn.Write([]byte(line)) }

	if err := expect('2'); err != nil { // greeting
		return err
	}
	send("EHLO inmem-test\r\n")
	if err := expect('2'); err != nil {
		return err
	}
	send("MAIL FROM:<" + s.from + ">\r\n")
	if err := expect('2'); err != nil {
		return err
	}
	send("RCPT TO:<" + m.To + ">\r\n")
	if err := expect('2'); err != nil {
		return err
	}
	send("DATA\r\n")
	if err := expect('3'); err != nil { // 354
		return err
	}
	// Minimal RFC5322 envelope mirroring the production headers we assert on.
	send("From: " + s.from + "\r\n")
	send("To: " + m.To + "\r\n")
	send("Subject: " + m.Subject + "\r\n")
	send("\r\n")
	send(m.Body + "\r\n")
	send(".\r\n")
	if err := expect('2'); err != nil {
		return err
	}
	send("QUIT\r\n")
	_ = expect('2')
	return nil
}

type smtpProtoError struct{ got string }

func (e *smtpProtoError) Error() string { return "unexpected SMTP reply: " + e.got }

// ----------------------------------------------------------------------------------------------
// A7 router + dispatcher wiring (production code paths; only the EmailSender is injectable).
// ----------------------------------------------------------------------------------------------

// a7Router is the migrated-DB-backed production router with the security-alert dispatcher wired
// into the user service exactly as cmd/server/main.go does (WithNotifier(dispatcher)), but with a
// test-supplied EmailSender so the suite can choose fake/in-memory-SMTP/failing delivery. The real
// notify.SQLAlertRepository writes the audit rows to the migrated security_alerts table.
type a7Router struct {
	engine     *gin.Engine
	gdb        *gorm.DB
	sx         *sqlx.DB
	dispatcher *notify.AsyncDispatcher
}

// newA7Router builds the router. sender is the EmailSender under test; a single worker + buffer
// keeps delivery ordering deterministic for the assertions. The caller MUST drain the dispatcher
// (via drain()) before querying the audit table so the async send/mark has completed.
func newA7Router(t *testing.T, gdb *gorm.DB, sx *sqlx.DB, sender notify.EmailSender) *a7Router {
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
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)

	// Real audit repository over the migrated security_alerts table + real async dispatcher.
	alertRepo := notify.NewSQLAlertRepository(sx)
	dispatcher := notify.NewAsyncDispatcher(
		notify.DispatcherConfig{Buffer: 16, Workers: 1},
		sender, alertRepo, zap.NewNop(),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(ctx)
	})

	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)
	// WithNotifier(dispatcher) is the exact production wiring (cmd/server/main.go). WithTokenRevoker
	// mirrors main so a password change also revokes refresh tokens (W1) without affecting alerts.
	userSvc := services.NewUserService(userRepo).
		WithTokenRevoker(refreshRepo).
		WithNotifier(dispatcher)
	roleSvc := services.NewRoleService(roleRepo)
	aclSvc := services.NewACLService(roleRepo)
	teamSvc := services.NewTeamService(auth.NewTeamRepository(gdb))

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(services.NewRecordService(meta, dyn), zap.NewNop()),
	}, zap.NewNop())

	return &a7Router{engine: r, gdb: gdb, sx: sx, dispatcher: dispatcher}
}

// drain shuts the dispatcher down, blocking until every buffered alert has been processed (the
// async send + audit write completed). After drain the security_alerts table reflects the final
// status for every alert this test emitted, so the row assertions are race-free.
func (rt *a7Router) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain did not complete: %v", err)
	}
}

// a7SecurityAlertRow mirrors the columns we assert on from the security_alerts audit table.
type a7SecurityAlertRow struct {
	ID        string `db:"id"`
	TenantID  string `db:"tenant_id"`
	UserID    string `db:"user_id"`
	AlertType string `db:"alert_type"`
	Channel   string `db:"channel"`
	Recipient string `db:"recipient"`
	Status    string `db:"status"`
}

// queryAlerts returns every security_alerts row for the given tenant (tenant-scoped query — the
// multi-tenant invariant). It is the test-side mirror of how the production audit repository pins
// every statement to tenant_id.
func queryAlerts(t *testing.T, sx *sqlx.DB, tenantID string) []a7SecurityAlertRow {
	t.Helper()
	var rows []a7SecurityAlertRow
	if err := sx.Select(&rows,
		`SELECT id, tenant_id, user_id, alert_type, channel, recipient, status
		   FROM security_alerts WHERE tenant_id = ? ORDER BY created_at`, tenantID); err != nil {
		t.Fatalf("query security_alerts (tenant=%s): %v", tenantID, err)
	}
	return rows
}

// a7SeedAdmin seeds a tenant + an active admin user (real bcrypt hash for authPassword) so a real
// login mints an admin token, and returns the admin user's id and email.
func a7SeedAdmin(t *testing.T, gdb *gorm.DB, tenantID, slug, email string) (userID, userEmail string) {
	t.Helper()
	a4SeedTenant(t, gdb, tenantID, slug, slug)
	id := a4SeedUser(t, gdb, tenantID, email, true, nil)
	return id, email
}

// changePassword issues PUT /admin/users/:id with a new password as the given admin token. It is
// the production E1 trigger: a successful password change emits a password_changed SecurityEvent.
func (rt *a7Router) changePassword(t *testing.T, adminToken, userID, newPass string) (status int, body string) {
	t.Helper()
	w := doAuthed(t, rt.engine, "PUT", "/api/v1/admin/users/"+userID, adminToken,
		map[string]any{"password": newPass}, nil)
	return w.Code, w.Body.String()
}

// ==============================================================================================
// Tests
// ==============================================================================================

// TestA7_PasswordChange_WritesSentAuditRow is the headline §7 "Integration (MySQL)" case:
// changing a user's password via PUT /admin/users/:id (admin auth) makes the production async
// dispatcher write EXACTLY ONE security_alerts row with a (fake, succeeding) sender —
// status=sent, alert_type=password_changed, recipient = the user's email, and the correct
// tenant_id. Everything but the EmailSender is the real production path (router → service →
// dispatcher → SQL audit repo → migrated MySQL).
func TestA7_PasswordChange_WritesSentAuditRow(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	a7RequireSchemaV9(t, sx)

	sender := &recordingSender{}
	rt := newA7Router(t, gdb, sx, sender)

	const tenantID = "a7000000-0000-0000-0000-0000000000a1"
	const slug = "a7-acme"
	const email = "alice@a7-acme.test"
	adminID, _ := a7SeedAdmin(t, gdb, tenantID, slug, email)

	login := itLogin(t, rt.engine, slug, email, authPassword)
	adminToken := login.Data.AccessToken

	status, body := rt.changePassword(t, adminToken, adminID, "Br4nd-New-P@ssw0rd!")
	if status != 200 {
		t.Fatalf("password change: got HTTP %d, want 200; body=%s", status, body)
	}

	// Drain the async dispatcher so the send + MarkSent have completed before we read the table.
	rt.drain(t)

	rows := queryAlerts(t, sx, tenantID)
	if len(rows) != 1 {
		t.Fatalf("security_alerts rows for tenant %s = %d; want exactly 1\nrows=%+v", tenantID, len(rows), rows)
	}
	r := rows[0]
	if r.AlertType != notify.AlertPasswordChanged {
		t.Errorf("alert_type = %q; want %q", r.AlertType, notify.AlertPasswordChanged)
	}
	if r.Status != "sent" {
		t.Errorf("status = %q; want \"sent\" (fake sender succeeds)", r.Status)
	}
	if r.Recipient != email {
		t.Errorf("recipient = %q; want the user's email %q", r.Recipient, email)
	}
	if r.TenantID != tenantID {
		t.Errorf("tenant_id = %q; want %q", r.TenantID, tenantID)
	}
	if r.UserID != adminID {
		t.Errorf("user_id = %q; want %q", r.UserID, adminID)
	}
	if r.Channel != "email" {
		t.Errorf("channel = %q; want \"email\"", r.Channel)
	}

	// The fake sender must have been asked to deliver exactly one mail to the right recipient.
	mails := sender.snapshot()
	if len(mails) != 1 {
		t.Fatalf("sender delivered %d mails; want exactly 1", len(mails))
	}
	if mails[0].To != email {
		t.Errorf("mail To = %q; want %q", mails[0].To, email)
	}
}

// TestA7_TenantIsolation proves the multi-tenant invariant for the audit table: a password-change
// alert raised in tenant A NEVER appears under tenant B. We change a password in BOTH tenants and
// assert each tenant's tenant-scoped query returns ONLY its own single row (no cross-tenant leak),
// matching the way the production audit repository pins every query to tenant_id.
func TestA7_TenantIsolation(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	a7RequireSchemaV9(t, sx)

	sender := &recordingSender{}
	rt := newA7Router(t, gdb, sx, sender)

	const (
		tenantA = "a7000000-0000-0000-0000-0000000000aa"
		slugA   = "a7-tenant-a"
		emailA  = "admin@a7-tenant-a.test"

		tenantB = "a7000000-0000-0000-0000-0000000000bb"
		slugB   = "a7-tenant-b"
		emailB  = "admin@a7-tenant-b.test"
	)
	adminA, _ := a7SeedAdmin(t, gdb, tenantA, slugA, emailA)
	adminB, _ := a7SeedAdmin(t, gdb, tenantB, slugB, emailB)

	tokenA := itLogin(t, rt.engine, slugA, emailA, authPassword).Data.AccessToken
	tokenB := itLogin(t, rt.engine, slugB, emailB, authPassword).Data.AccessToken

	if st, b := rt.changePassword(t, tokenA, adminA, "A-new-P@ss-1!"); st != 200 {
		t.Fatalf("tenant A password change: HTTP %d; body=%s", st, b)
	}
	if st, b := rt.changePassword(t, tokenB, adminB, "B-new-P@ss-1!"); st != 200 {
		t.Fatalf("tenant B password change: HTTP %d; body=%s", st, b)
	}

	rt.drain(t)

	rowsA := queryAlerts(t, sx, tenantA)
	rowsB := queryAlerts(t, sx, tenantB)

	if len(rowsA) != 1 {
		t.Fatalf("tenant A alerts = %d; want exactly 1\nrows=%+v", len(rowsA), rowsA)
	}
	if len(rowsB) != 1 {
		t.Fatalf("tenant B alerts = %d; want exactly 1\nrows=%+v", len(rowsB), rowsB)
	}
	// Isolation: tenant A's row is for user A in tenant A only; B's only for B. Neither query
	// returns the other's row.
	if rowsA[0].TenantID != tenantA || rowsA[0].UserID != adminA || rowsA[0].Recipient != emailA {
		t.Errorf("tenant A row leaked/mismatched: %+v", rowsA[0])
	}
	if rowsB[0].TenantID != tenantB || rowsB[0].UserID != adminB || rowsB[0].Recipient != emailB {
		t.Errorf("tenant B row leaked/mismatched: %+v", rowsB[0])
	}
	// Belt-and-braces: assert tenant A's query contains NO row carrying tenant B's id (and vice
	// versa) — i.e. the tenant filter truly isolates.
	for _, r := range rowsA {
		if r.TenantID == tenantB || r.Recipient == emailB {
			t.Errorf("tenant A query returned a tenant B alert: %+v", r)
		}
	}
	for _, r := range rowsB {
		if r.TenantID == tenantA || r.Recipient == emailA {
			t.Errorf("tenant B query returned a tenant A alert: %+v", r)
		}
	}
}

// TestA7_InMemorySMTP_ReceivesExactlyOneMail is the §7 "Integration (SMTP giả)" success case: with
// a real in-memory SMTP server and a sender that performs a genuine SMTP wire exchange, a password
// change delivers EXACTLY ONE mail, addressed to the user (correct To), with the subject for the
// password_changed type — and the body leaks NO token/hash/password (spec §1/§7).
func TestA7_InMemorySMTP_ReceivesExactlyOneMail(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	a7RequireSchemaV9(t, sx)

	smtpSrv := newInMemSMTPServer(t)
	t.Cleanup(smtpSrv.Close)

	sender := &inMemSMTPSender{addr: smtpSrv.addr(), from: "no-reply@nexus.test"}
	rt := newA7Router(t, gdb, sx, sender)

	const tenantID = "a7000000-0000-0000-0000-0000000000c1"
	const slug = "a7-smtp"
	const email = "carol@a7-smtp.test"
	adminID, _ := a7SeedAdmin(t, gdb, tenantID, slug, email)

	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	const newPass = "Smtp-New-P@ssw0rd-99!"
	if st, b := rt.changePassword(t, token, adminID, newPass); st != 200 {
		t.Fatalf("password change: HTTP %d; body=%s", st, b)
	}

	rt.drain(t)

	msgs := smtpSrv.messages()
	if len(msgs) != 1 {
		t.Fatalf("in-memory SMTP server received %d mails; want exactly 1", len(msgs))
	}
	m := msgs[0]
	if m.to != email {
		t.Errorf("RCPT TO = %q; want the user's email %q", m.to, email)
	}
	// Subject must be the password_changed copy. We render via the production template to avoid
	// hard-coding the exact Vietnamese string (and to prove the dispatcher used the right type).
	wantSubject, _, _ := a7RenderSubject(notify.AlertPasswordChanged)
	if !strings.Contains(m.data, "Subject: "+wantSubject) {
		t.Errorf("delivered DATA missing expected Subject %q\n--- DATA ---\n%s", wantSubject, m.data)
	}
	// Anti-leak: the mail body must NOT contain the plaintext password, nor any obvious secret
	// marker. (The production template never interpolates secrets; this guards against regressions.)
	if strings.Contains(m.data, newPass) {
		t.Errorf("delivered mail leaked the plaintext password!\n--- DATA ---\n%s", m.data)
	}
	for _, banned := range []string{"password_hash", "$2a$", "$2b$", "bearer ", "refresh_token"} {
		if strings.Contains(strings.ToLower(m.data), banned) {
			t.Errorf("delivered mail leaked a secret marker %q\n--- DATA ---\n%s", banned, m.data)
		}
	}
	// And the audit row for this mail must be status=sent (the SMTP exchange succeeded).
	rows := queryAlerts(t, sx, tenantID)
	if len(rows) != 1 || rows[0].Status != "sent" {
		t.Fatalf("audit row after successful SMTP = %+v; want exactly 1 with status=sent", rows)
	}
}

// TestA7_SMTPFailure_FailOpen is the §7 "Integration (SMTP giả)" failure case: when the SMTP
// sender errors, the originating business flow (password change) MUST still return 200 (NEVER
// 500), and the audit row MUST be recorded as status=failed. The business operation must not be
// undone by a delivery failure (fail-open, spec §1).
func TestA7_SMTPFailure_FailOpen(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	a7RequireSchemaV9(t, sx)

	sender := &failingSender{}
	rt := newA7Router(t, gdb, sx, sender)

	const tenantID = "a7000000-0000-0000-0000-0000000000d1"
	const slug = "a7-failopen"
	const email = "dave@a7-failopen.test"
	adminID, _ := a7SeedAdmin(t, gdb, tenantID, slug, email)

	token := itLogin(t, rt.engine, slug, email, authPassword).Data.AccessToken

	const newPass = "FailOpen-New-P@ss-7!"
	status, body := rt.changePassword(t, token, adminID, newPass)
	// Fail-open: the business op succeeds with 200 even though SMTP is down. A 500 here would be
	// the bug this test exists to catch.
	if status != 200 {
		t.Fatalf("fail-open broken: password change returned HTTP %d (want 200) when SMTP fails; body=%s", status, body)
	}

	rt.drain(t)

	rows := queryAlerts(t, sx, tenantID)
	if len(rows) != 1 {
		t.Fatalf("security_alerts rows = %d; want exactly 1\nrows=%+v", len(rows), rows)
	}
	if rows[0].Status != "failed" {
		t.Errorf("status = %q; want \"failed\" (SMTP send errored)", rows[0].Status)
	}
	if rows[0].AlertType != notify.AlertPasswordChanged {
		t.Errorf("alert_type = %q; want %q", rows[0].AlertType, notify.AlertPasswordChanged)
	}

	// PROOF the business op was NOT undone: the new password actually works for login. The old
	// password must no longer authenticate. This is the heart of "fail-open": SMTP died, the
	// password change still took effect.
	if w := postLogin(t, rt.engine, `{"tenant":"`+slug+`","email":"`+email+`","password":"`+newPass+`"}`); w.Code != 200 {
		t.Errorf("new password does not authenticate after fail-open change: HTTP %d; body=%s", w.Code, w.Body.String())
	}
	if w := postLogin(t, rt.engine, `{"tenant":"`+slug+`","email":"`+email+`","password":"`+authPassword+`"}`); w.Code == 200 {
		t.Errorf("old password still authenticates after change — password change was not applied")
	}

	if sender.calls == 0 {
		t.Error("failing sender was never invoked — the dispatcher did not attempt delivery")
	}
}

// ----------------------------------------------------------------------------------------------
// shared small helpers
// ----------------------------------------------------------------------------------------------

// a7RequireSchemaV9 asserts the migrated schema reached version >= 9 (the migration that creates
// security_alerts). a4SetupMigratedDB already proved version >= 7 via the real migrate chain; this
// raises the floor so the A7 table is guaranteed to come from migration 000009, never AutoMigrate.
func a7RequireSchemaV9(t *testing.T, sx *sqlx.DB) {
	t.Helper()
	var ver struct {
		Version int  `db:"version"`
		Dirty   bool `db:"dirty"`
	}
	if err := sx.Get(&ver, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if ver.Version < 9 || ver.Dirty {
		t.Fatalf("schema_migrations = version %d dirty %v; want version >= 9 dirty false (migration 000009_create_security_alerts must be applied via real migrate up)", ver.Version, ver.Dirty)
	}
	// Sanity: the security_alerts table physically exists with the expected columns.
	var cnt int
	if err := sx.Get(&cnt,
		`SELECT COUNT(*) FROM information_schema.columns
		   WHERE table_schema = DATABASE() AND table_name = 'security_alerts'
		     AND column_name IN ('id','tenant_id','user_id','alert_type','channel','recipient','status')`); err != nil {
		t.Fatalf("inspect security_alerts columns: %v", err)
	}
	if cnt != 7 {
		t.Fatalf("security_alerts is missing expected columns (found %d of 7) — migration 000009 schema drift", cnt)
	}
}

// a7RenderSubject returns the production-rendered subject for an alert type, so subject assertions
// track the real template instead of a duplicated literal. It builds the same SecurityEvent the
// dispatcher would and reuses the same code path indirectly via a minimal probe sender.
func a7RenderSubject(alertType string) (subject, text, html string) {
	// Run one event through the dispatcher with a capturing sender to obtain the rendered subject
	// exactly as production renders it.
	cap := &recordingSender{}
	d := notify.NewAsyncDispatcher(notify.DispatcherConfig{Buffer: 1, Workers: 1}, cap, nil, zap.NewNop())
	d.EnqueueSecurityAlert(context.Background(), notify.SecurityEvent{
		Type:     alertType,
		TenantID: "probe",
		UserID:   "probe",
		Email:    "probe@example.test",
		At:       time.Now(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Shutdown(ctx)
	mails := cap.snapshot()
	if len(mails) == 1 {
		return mails[0].Subject, mails[0].Body, mails[0].HTMLBody
	}
	return "", "", ""
}
