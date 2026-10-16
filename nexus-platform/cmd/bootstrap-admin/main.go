// Command bootstrap-admin creates the FIRST real tenant + admin user for a fresh Nexus
// deployment. Since migration 000003 stopped seeding the committed demo admin and 000066 removed
// it from existing databases (issue #95), this is the ONLY way to get a first admin — a fresh
// deploy has no login at all until it is run.
//
// It is a standalone, additive operator tool: a new cmd/, never imported by the server, never
// run at boot, with NO migration and NO change to any existing file's behavior. The typical
// invocation fits the Docker/compose deploy:
//
//	docker compose -f deploy/nexus/compose.prod.yml --env-file deploy/nexus/.env.prod run --rm \
//	  -e BOOTSTRAP_TENANT=acme \
//	  -e BOOTSTRAP_ADMIN_EMAIL=admin@acme.com \
//	  -e BOOTSTRAP_ADMIN_PASSWORD='a-strong-password' \
//	  --entrypoint /app/bootstrap-admin nexus
//
// Both flags are load-bearing, and both were missing until 2026-09-11:
//
//	-f …/compose.prod.yml : the repo-root docker-compose.yml declares only mysql+redis, so from the
//	                        repo root the command dies with "no such service: nexus".
//	--env-file …/.env.prod : compose.prod.yml BUILDS DATABASE_URL by interpolating ${MYSQL_USER} /
//	                        ${MYSQL_PASSWORD} / ${MYSQL_DATABASE}, and `env_file:` does NOT feed
//	                        interpolation. Measured without it: DATABASE_URL renders as
//	                        ":@tcp(mysql:3306)/?charset=…" — no user, no database — and this tool
//	                        dies at "connect database". Note the path is repo-root-relative like
//	                        -f, not bare .env.prod: --env-file resolves against the CWD.
//
// Locally, without Docker at all:
//
//	DATABASE_URL='root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=UTC' \
//	JWT_SECRET=<any non-empty> BOOTSTRAP_TENANT=acme BOOTSTRAP_ADMIN_EMAIL=admin@acme.com \
//	BOOTSTRAP_ADMIN_PASSWORD='a-strong-password' go run ./cmd/bootstrap-admin
//
// It reuses the SAME config loader, DB open, password policy (services.ValidatePassword,
// RM-17) and user-create path (services.UserService.CreateUser → bcrypt cost 12) the running
// server uses, so the admin it creates is byte-for-byte a working, loggable-in admin. Tenant
// creation has no service today, so it is an inline GORM insert on the existing auth.Tenant
// model — kept inside this cmd to preserve the zero-blast-radius promise.
//
// The tool NEVER disables/rotates/deletes any existing account — removing the old demo admin is
// migration 000066's job, and it only fires on an untouched seed hash. This tool only opens a safe
// door to the first real admin (deploy/GO-LIVE-CHECKLIST.md §5).
package main

import (
	// The embedded IANA timezone database. The runtime image is alpine WITHOUT tzdata
	// (Dockerfile stage 2 installs only ca-certificates) and the binary is CGO_ENABLED=0, so
	// without this import time.LoadLocation("Asia/Ho_Chi_Minh") fails IN PRODUCTION while
	// passing on every developer machine. That failure is silent and actively misleading: a
	// stored business calendar whose timezone will not load is treated as NO calendar
	// (services.WorkflowScheduleResolver), so every business_hours deadline degrades to
	// wall-clock and the warning says "no business calendar for this tenant" — while the admin
	// is looking at the calendar in the UI. Verified by running the shipped image shape:
	// `unknown time zone Asia/Ho_Chi_Minh` before this import, resolved after.
	// Preferred over `apk add tzdata` because it survives a base-image change.
	_ "time/tzdata"

	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/db"
	"nexus/platform/internal/services"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// The tenant slug shape is validated via services.ValidTenantSlug — the SAME pattern the
// POST /admin/tenants provisioning endpoint uses, so the two provisioning paths never drift.

// input is the operator-supplied configuration, read from the environment (see inputFromEnv).
type input struct {
	tenant   string // BOOTSTRAP_TENANT — login slug of the tenant to create/reuse
	email    string // BOOTSTRAP_ADMIN_EMAIL — the admin's login email
	password string // BOOTSTRAP_ADMIN_PASSWORD — validated by services.ValidatePassword (RM-17)
	name     string // BOOTSTRAP_ADMIN_NAME — optional full name; defaults to the email local-part
	// platformOperator (BOOTSTRAP_PLATFORM_OPERATOR=true) grants the created/reused admin the
	// is_platform_operator flag (TENANT-PROVISION mig 000052) — the ONLY grant path besides direct
	// SQL. This is how the Founder promotes the first Nexus admin to a platform operator.
	platformOperator bool
}

// deps carries the collaborators bootstrap needs. Split out so the core logic is testable: a
// unit test can pass a zero-value deps (nil db) because every pure validation runs BEFORE any
// db dereference, and the integration test wires the real GORM connection + repo + service.
type deps struct {
	db       *gorm.DB
	userRepo *auth.UserRepository
	userSvc  *services.UserService
	newID    func() string
}

// result summarizes what bootstrap did, for the operator-facing success line.
type result struct {
	tenantSlug    string
	tenantID      string
	email         string
	userID        string
	adminCreated  bool // false when the admin already existed (idempotent no-op)
	tenantCreated bool // false when the tenant already existed and was reused
	// calendarSeeded is true when THIS run inserted the tenant's default working calendar. It
	// exists so the "nothing to do" line below cannot claim nothing changed on a run that did.
	calendarSeeded bool
}

func main() {
	in := inputFromEnv()

	// Fail fast on missing required config BEFORE opening a DB connection — a clearer error
	// than a downstream validation failure, and no wasted connection.
	if in.tenant == "" || in.email == "" || in.password == "" {
		fmt.Fprintln(os.Stderr, "bootstrap-admin: BOOTSTRAP_TENANT, BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD are all required")
		os.Exit(1)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-admin: %v\n", err)
		os.Exit(1)
	}
	database, err := db.InitDB(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-admin: connect database: %v\n", err)
		os.Exit(1)
	}

	// The happy path here is three deliberate misses — "does this tenant exist?", "does this admin
	// exist?" — and GORM logs every ErrRecordNotFound in red at the default log level. An operator
	// runs this ONCE, on a fresh production deploy, with no other login available; three red
	// "record not found" lines above the success message read as a failure and invite exactly the
	// wrong next move. Silence the query logger: this tool reports its own outcome on stdout.
	database.Logger = gormlogger.Discard

	userRepo := auth.NewUserRepository(database)
	// A bare UserService is enough: RoleID is nil (the admin is gated by is_admin, exactly like
	// the demo seed which has no role), so the cross-tenant role validator never runs, and the
	// notifier/revoker paths are irrelevant to a create. Reusing CreateUser gives us the SAME
	// bcrypt cost 12, the SAME password policy, and the SAME duplicate handling as the API.
	userSvc := services.NewUserService(userRepo)

	res, err := bootstrap(context.Background(), deps{
		db:       database,
		userRepo: userRepo,
		userSvc:  userSvc,
		newID:    uuid.NewString,
	}, in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-admin: %v\n", err)
		os.Exit(1)
	}

	// TENANT-PROVISION grant: promote this admin to a platform operator when asked. Runs on BOTH
	// the created and the reused-admin path (so the Founder can flag an EXISTING northwind admin), and
	// is idempotent (sets the flag to 1). This + direct SQL are the ONLY ways the flag is ever set.
	if in.platformOperator {
		if upErr := database.Model(&auth.User{}).
			Where("id = ? AND tenant_id = ?", res.userID, res.tenantID).
			Update("is_platform_operator", true).Error; upErr != nil {
			fmt.Fprintf(os.Stderr, "bootstrap-admin: admin is ready but FAILED to grant platform-operator: %v\n", upErr)
			os.Exit(1)
		}
		fmt.Printf("granted platform-operator to %q (tenant %q)\n", res.email, res.tenantSlug)
	}

	if !res.adminCreated {
		// Idempotent re-run: the admin already exists — safe to run again. "Nothing to do" is only
		// true when this run ALSO left the calendar alone; the backfill seed can fire on a run
		// whose admin half was a no-op, and an operator must not read that as "nothing changed".
		if res.calendarSeeded {
			fmt.Printf("admin %q already exists in tenant %q — seeded the default working calendar %q (T2-T6 08:00-17:00 %s); nothing else to do\n",
				res.email, res.tenantSlug, services.DefaultCalendarName, services.DefaultCalendarTimezone)
			return
		}
		fmt.Printf("admin %q already exists in tenant %q — nothing to do\n", res.email, res.tenantSlug)
		return
	}
	if res.tenantCreated {
		fmt.Printf("created tenant %q\n", res.tenantSlug)
	} else {
		fmt.Printf("reused existing tenant %q\n", res.tenantSlug)
	}
	fmt.Printf("created admin %q (tenant %q) — you can now log in with this account.\n", res.email, res.tenantSlug)
	// No "now go disable the demo admin" line any more: migration 000003 stopped seeding it and
	// 000066 deletes it from databases that already ran the old 000003. Telling an operator to go
	// disable an account that is not there sends them looking for a problem instead of logging in.
	fmt.Println("This is now the only admin on this deployment — keep the password somewhere safe.")
}

// inputFromEnv reads the operator configuration from the environment. The password is read
// verbatim (never trimmed — trimming would change the credential); the rest are trimmed. The
// admin name defaults to the local-part of the email when BOOTSTRAP_ADMIN_NAME is empty.
func inputFromEnv() input {
	in := input{
		tenant:           strings.TrimSpace(os.Getenv("BOOTSTRAP_TENANT")),
		email:            strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_EMAIL")),
		password:         os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
		name:             strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_NAME")),
		platformOperator: strings.EqualFold(strings.TrimSpace(os.Getenv("BOOTSTRAP_PLATFORM_OPERATOR")), "true"),
	}
	if in.name == "" {
		in.name = defaultName(in.email)
	}
	return in
}

// defaultName derives a human-friendly default full name from the email local-part (before the
// '@'). An email with no '@' yields itself. This is purely cosmetic (full_name is nullable).
func defaultName(email string) string {
	email = strings.TrimSpace(email)
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// bootstrap creates (or idempotently reuses) the tenant and admin. It runs every PURE
// validation first — email presence, slug shape, and the RM-17 password policy — so a bad
// input can never leave a stray tenant row behind, and so a nil-db unit test can exercise the
// rejection branches without touching the database. Only then does it resolve/create the
// tenant, short-circuit when the admin already exists (idempotent), and finally create the
// admin through the SAME service path (bcrypt cost 12, is_admin=1) the API uses.
func bootstrap(ctx context.Context, d deps, in input) (*result, error) {
	email := strings.TrimSpace(in.email)
	if email == "" {
		return nil, fmt.Errorf("bootstrap.bootstrap: BOOTSTRAP_ADMIN_EMAIL is required")
	}

	slug := strings.ToLower(strings.TrimSpace(in.tenant))
	if !services.ValidTenantSlug(slug) {
		return nil, fmt.Errorf("bootstrap.bootstrap: BOOTSTRAP_TENANT %q is not a valid slug — use lowercase letters, digits, '-' or '_' (max 64 chars, e.g. 'acme')", in.tenant)
	}

	// RM-17 password policy — the SAME body every set-password path (CreateUser, ChangePassword,
	// ResetPassword) enforces. Checked before any DB write so a weak password fails cleanly.
	if err := services.ValidatePassword(in.password); err != nil {
		return nil, fmt.Errorf("bootstrap.bootstrap: %w", err)
	}

	name := strings.TrimSpace(in.name)
	if name == "" {
		name = defaultName(email)
	}

	tenantID, tenantCreated, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		return nil, err
	}

	// Default working calendar (DAILY-QUEUE #10 / Q9). Called on EVERY run, not only when the
	// tenant was just created, because it is idempotent — that is what backfills the tenants that
	// predate the seed (demo, northwind) without growing a second calendar each time an operator
	// re-runs bootstrap. "Idempotent" there means "has this tenant EVER had a calendar row",
	// soft-deleted ones included: a tenant that deleted its only calendar chose the wall-clock
	// fallback, and re-seeding would move every SLA instant they have. This is one of the two
	// tenant-creation paths the header promises never drift; both call the same helper.
	calendarSeeded, err := services.SeedDefaultBusinessCalendar(ctx, d.db, tenantID)
	if err != nil {
		return nil, fmt.Errorf("bootstrap.bootstrap: %w", err)
	}

	// Idempotency: if this admin already exists AND is a usable active admin, do NOT clobber it —
	// a re-run is a safe no-op. But existence of the email ALONE is not enough to declare success:
	// a disabled/non-admin/soft-deleted row with this email is a lockout footgun (an operator who
	// reads "already exists" as "you're done" would then disable the demo admin per GO-LIVE §5,
	// leaving the tenant with zero loggable-in admins). inspectExisting classifies the row
	// (including a soft-deleted one, which the tenant-pinned GetUserByEmail cannot see) so that
	// case is named and refused instead of silently reported as success.
	existing, softDeleted, err := inspectExisting(ctx, d, tenantID, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !softDeleted && existing.IsAdmin && existing.Status == "active" {
			return &result{
				tenantSlug:    slug,
				tenantID:      tenantID,
				email:         email,
				userID:        existing.ID,
				adminCreated:  false,
				tenantCreated: tenantCreated,
			}, nil
		}
		return nil, unusableExistingErr(email, existing, softDeleted)
	}

	// Create the admin through the real service path. actorIsAdmin=true is legitimate here: the
	// caller is a local operator with direct DB access running a one-shot CLI, not an API request
	// that could be an escalation attempt. is_admin=1 + RoleID nil mirrors the demo seed exactly.
	view, err := d.userSvc.CreateUser(ctx, tenantID, true, services.CreateUserInput{
		Email:    email,
		Password: in.password,
		FullName: name,
		IsAdmin:  true,
	})
	if err != nil {
		// inspectExisting above already caught every case a plain re-run would hit, so this is
		// only a TOCTOU race (a row appeared between the check and this write). Never silently
		// report success on a race — send the operator back to re-run and get a real classification.
		var ve *services.ValidationError
		if errors.As(err, &ve) && ve.Code == "USER_ALREADY_EXISTS" {
			return nil, fmt.Errorf("bootstrap.bootstrap: a user with email %q appeared concurrently — re-run bootstrap-admin to check its state before proceeding", email)
		}
		return nil, fmt.Errorf("bootstrap.bootstrap: create admin: %w", err)
	}

	return &result{
		tenantSlug:     slug,
		tenantID:       tenantID,
		email:          email,
		userID:         view.ID,
		adminCreated:   true,
		tenantCreated:  tenantCreated,
		calendarSeeded: calendarSeeded,
	}, nil
}

// resolveOrCreateTenant returns the id of an ACTIVE tenant for slug, creating one when absent.
// Reuse is active-only — mirroring login's auth.UserRepository.ResolveTenantBySlug — so bootstrap
// can never silently attach the new admin to a suspended/inactive tenant it could never log
// into; a same-slug non-active tenant is a clear, non-zero-exit error instead. There is no
// CreateTenant service today, so the insert is an inline GORM Create on the existing auth.Tenant
// model (parameterized, no string-concat SQL) — kept in this cmd to avoid adding surface to
// internal/auth. A newly-created tenant is always 'active'.
func resolveOrCreateTenant(ctx context.Context, d deps, slug string) (tenantID string, created bool, err error) {
	var t auth.Tenant
	lookupErr := d.db.WithContext(ctx).Where("slug = ?", slug).First(&t).Error
	if lookupErr == nil {
		if t.Status != "active" {
			return "", false, fmt.Errorf("bootstrap.resolveOrCreateTenant: tenant slug %q already exists but is NOT active (status=%q) — an admin attached to it could not log in; activate the tenant first or use another slug", slug, t.Status)
		}
		return t.ID, false, nil
	}
	if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		return "", false, fmt.Errorf("bootstrap.resolveOrCreateTenant: lookup %q: %w", slug, lookupErr)
	}

	t = auth.Tenant{ID: d.newID(), Slug: slug, Name: slug, Status: "active"}
	if createErr := d.db.WithContext(ctx).Create(&t).Error; createErr != nil {
		return "", false, fmt.Errorf("bootstrap.resolveOrCreateTenant: create %q: %w", slug, createErr)
	}
	return t.ID, true, nil
}

// inspectExisting looks up an existing (tenantID, email) user, INCLUDING a soft-deleted row
// (Unscoped) — so the caller can tell a genuinely usable active admin apart from the lockout
// footgun states: non-admin, disabled, or soft-deleted. u is nil when no row (of either kind)
// exists. softDeleted is true only when the row was found solely via the Unscoped lookup.
func inspectExisting(ctx context.Context, d deps, tenantID, email string) (u *auth.User, softDeleted bool, err error) {
	existing, getErr := d.userRepo.GetUserByEmail(ctx, tenantID, email)
	if getErr == nil {
		return existing, false, nil
	}
	if !errors.Is(getErr, auth.ErrUserNotFound) {
		return nil, false, fmt.Errorf("bootstrap.inspectExisting: %w", getErr)
	}

	// Not visible in the default (non-deleted) scope. A soft-deleted row with this email would
	// still trip the uq_users_tenant_email unique constraint on CreateUser (USER_ALREADY_EXISTS)
	// without ever being named — so check Unscoped up front instead of waiting for that surprise.
	var soft auth.User
	unscopedErr := d.db.WithContext(ctx).Unscoped().
		Where("tenant_id = ? AND email = ?", tenantID, email).
		First(&soft).Error
	if unscopedErr == nil {
		return &soft, true, nil
	}
	if errors.Is(unscopedErr, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("bootstrap.inspectExisting: unscoped lookup: %w", unscopedErr)
}

// unusableExistingErr is the distinct, loud rejection for a pre-existing (tenant,email) row that
// is NOT a usable active admin. It exists to close the lockout footgun (Finding A): an operator
// must never read this state as "already exists — nothing to do" and go on to disable the demo
// admin (deploy/GO-LIVE-CHECKLIST.md §5) while the tenant still has zero loggable-in admins.
func unusableExistingErr(email string, u *auth.User, softDeleted bool) error {
	if softDeleted {
		return fmt.Errorf("bootstrap.bootstrap: a user with email %q already exists but is SOFT-DELETED — restore it or use another email; NOT creating an admin (do not disable the demo admin until a usable admin exists)", email)
	}
	return fmt.Errorf("bootstrap.bootstrap: a user with email %q already exists but is NOT a usable active admin (is_admin=%v, status=%q) — restore/enable it or use another email; NOT creating an admin (do not disable the demo admin until a usable admin exists)", email, u.IsAdmin, u.Status)
}
