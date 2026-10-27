package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrUserAlreadyExists is returned by CreateUser when the (tenant_id, email) pair already
// exists (the uq_users_tenant_email unique constraint fires). The service maps it to a 409
// so the API caller learns the email is taken WITHIN their tenant — this is not a
// cross-tenant leak because the uniqueness scope is the tenant itself.
var ErrUserAlreadyExists = errors.New("auth: user already exists")

// ErrLastAdmin is returned by the *GuardLastAdmin write paths when the requested demotion /
// deletion would drop the tenant's active-admin count to zero. It is the typed signal the
// user service folds into a 422 LAST_ADMIN. Unlike the old service-side check-then-act, the
// guard runs the count under a FOR UPDATE lock in the SAME transaction as the write, so two
// concurrent demote/delete requests racing at the count==2 boundary can no longer both pass:
// the second transaction blocks on the row lock, then re-reads count==1 and is refused. This
// closes the TOCTOU window (docs/_planner/GD1-review-bugs.md §19).
var ErrLastAdmin = errors.New("auth: would remove last active admin")

// ErrUserTierConflict is returned when a write would make a user both admin and customer owner
// (CHECK chk_users_tier, MySQL 3819) - the service maps it to 422 USER_TIER_CONFLICT.
var ErrUserTierConflict = errors.New("auth: user cannot be both admin and customer owner")

// ErrUserNotManageable is returned when the target of an owner-tier write is not a plain staff account
// the actor may change (specs/065), or when a non-operator admin reaches for an operator account (B2).
// The service maps it to 403 USER_NOT_MANAGEABLE.
var ErrUserNotManageable = errors.New("auth: user not manageable by this actor")

// mysqlCheckViolation is ER_CHECK_CONSTRAINT_VIOLATED.
const mysqlCheckViolation = 3819

// isTierConflict reports whether err is the chk_users_tier violation.
func isTierConflict(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlCheckViolation
}

// mysqlDuplicateEntry is the MySQL error number for a unique-constraint violation (ER_DUP_ENTRY).
const mysqlDuplicateEntry = 1062

// isDuplicateEntry reports whether err is a MySQL duplicate-key (1062) error, used to fold a
// unique-constraint violation on (tenant_id, email) into the typed ErrUserAlreadyExists.
func isDuplicateEntry(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlDuplicateEntry
}

// ListUsers returns every active (non-soft-deleted) user for a tenant, ordered by created_at
// then id for a stable page. The query is ALWAYS pinned to tenant_id, so it can never return
// a row from another tenant. password_hash is selected into the model but never serialized
// (User.PasswordHash has json:"-").
func (r *UserRepository) ListUsers(ctx context.Context, tenantID string) ([]User, error) {
	var users []User
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at ASC, id ASC").
		Find(&users).Error
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.ListUsers: %w", err)
	}
	return users, nil
}

// PrivilegedHolders returns the tenant's admins and platform operators who hold one of roleIDs:
// the only users propose_access asks about (issue #23), instead of every user of the tenant.
// Pinned to tenant_id; an empty roleIDs is (nil, nil) without a query.
func (r *UserRepository) PrivilegedHolders(ctx context.Context, tenantID string, roleIDs []string) ([]User, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	var users []User
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND role_id IN ? AND (is_admin = ? OR is_platform_operator = ?)", tenantID, roleIDs, true, true).
		Order("created_at ASC, id ASC").
		Find(&users).Error
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.PrivilegedHolders: %w", err)
	}
	return users, nil
}

// CreateUser inserts u (which must already carry a server-derived ID and TenantID). A
// duplicate (tenant_id, email) is reported as ErrUserAlreadyExists; any other failure is
// wrapped. The caller is responsible for hashing the password before this runs — the repo
// persists PasswordHash verbatim and never sees the plaintext.
func (r *UserRepository) CreateUser(ctx context.Context, u *User) error {
	err := r.db.WithContext(ctx).Create(u).Error
	if err != nil {
		if isDuplicateEntry(err) {
			return ErrUserAlreadyExists
		}
		if isTierConflict(err) {
			return ErrUserTierConflict
		}
		return fmt.Errorf("auth.UserRepository.CreateUser: %w", err)
	}
	return nil
}

// UpdateUser applies the non-empty columns in fields to the tenant-scoped user identified by
// id, returning the refreshed row. The WHERE is pinned to (tenant_id, id) so an attacker
// cannot update a row in another tenant by guessing an id. An unknown/cross-tenant/soft-deleted
// id resolves to ErrUserNotFound (zero rows affected). A duplicate email surfaces as
// ErrUserAlreadyExists. fields uses the same column names as the users table.
func (r *UserRepository) UpdateUser(ctx context.Context, tenantID, id string, fields map[string]any) (*User, error) {
	if len(fields) == 0 {
		// Nothing to change: just re-read so the caller still gets the current row (and a
		// not-found for a bad id) without issuing a no-op UPDATE.
		return r.GetUserByID(ctx, tenantID, id)
	}

	res := r.db.WithContext(ctx).
		Model(&User{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Updates(fields)
	if res.Error != nil {
		if isDuplicateEntry(res.Error) {
			return nil, ErrUserAlreadyExists
		}
		if isTierConflict(res.Error) {
			return nil, ErrUserTierConflict
		}
		return nil, fmt.Errorf("auth.UserRepository.UpdateUser: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Either the id does not exist in this tenant, or it is soft-deleted (GORM's
		// default scope excludes deleted_at IS NOT NULL). Both are a not-found to the caller.
		return nil, ErrUserNotFound
	}
	return r.GetUserByID(ctx, tenantID, id)
}

// UpdateUserIfManageable is the customer owner's write (specs/065 D-4): ONE conditional UPDATE whose
// WHERE carries the whole manageable rule (staff only, not the actor, not disabled), so an operator
// promoting the target between the service's read and this write cannot be raced. Zero rows matched =>
// re-read and classify: not found (ErrUserNotFound) / not manageable (ErrUserNotManageable) / manageable
// (the stored values already equal the request - success). Correct under clientFoundRows, never reads
// RowsAffected as "changed". Pinned to (tenant_id, id); soft-deleted rows are excluded by GORM's scope.
func (r *UserRepository) UpdateUserIfManageable(ctx context.Context, tenantID, id, actorID string, fields map[string]any) (*User, error) {
	if len(fields) > 0 {
		res := r.db.WithContext(ctx).
			Model(&User{}).
			Where("tenant_id = ? AND id = ? AND is_admin = ? AND can_manage_users = ? AND is_platform_operator = ? AND status <> ? AND id <> ?",
				tenantID, id, false, false, false, "disabled", actorID).
			Updates(fields)
		if res.Error != nil {
			return nil, fmt.Errorf("auth.UserRepository.UpdateUserIfManageable: %w", res.Error)
		}
		if res.RowsAffected > 0 {
			return r.GetUserByID(ctx, tenantID, id)
		}
	}
	u, err := r.GetUserByID(ctx, tenantID, id)
	if err != nil {
		return nil, err // ErrUserNotFound passes through unwrapped for the service's errors.Is
	}
	if !u.ManageableBy(actorID) {
		return nil, ErrUserNotManageable
	}
	return u, nil
}

// SoftDeleteUser soft-deletes (sets deleted_at) the tenant-scoped user identified by id. The
// WHERE is pinned to (tenant_id, id); an unknown/cross-tenant/already-deleted id returns
// ErrUserNotFound. GORM's soft-delete (the DeletedAt field) is used so the row is recoverable
// and stays excluded from every default-scoped query, including the uniqueness re-check.
func (r *UserRepository) SoftDeleteUser(ctx context.Context, tenantID, id string) error {
	res := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&User{})
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.SoftDeleteUser: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdatePasswordHash replaces the password_hash for (tenant_id, id) with the supplied bcrypt hash.
// The WHERE is pinned to (tenant_id, id) so only the caller's own tenant row is mutated.
// An unknown/cross-tenant/soft-deleted id resolves to ErrUserNotFound.
// This is the ONLY write path for passwords — callers must hash before calling.
func (r *UserRepository) UpdatePasswordHash(ctx context.Context, tenantID, id, newHash string) error {
	res := r.db.WithContext(ctx).
		Model(&User{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Update("password_hash", newHash)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.UpdatePasswordHash: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// RecordFailedLogin atomically increments failed_login_count for the (tenant_id, id) user and,
// when the incremented count reaches threshold, stamps locked_until = lockUntil — all in ONE
// UPDATE so two concurrent wrong-password attempts can never lose an increment or both skip the
// lock (persistent lockout, prod-ops slice b / mig 000043).
//
// ORDER OF THE SET LIST IS LOAD-BEARING: MySQL evaluates UPDATE assignments left-to-right and
// later assignments see the NEW value of earlier ones — so locked_until (which needs the OLD
// count via failed_login_count + 1) MUST be assigned before the increment itself. Raw
// parameterized Exec (never string concat) because GORM's Updates(map) sorts keys
// alphabetically, which would flip that order.
//
// A missing/cross-tenant/soft-deleted id is a silent no-op (0 rows): the caller is on the
// login failure path and must stay anti-enumeration-generic anyway.
func (r *UserRepository) RecordFailedLogin(ctx context.Context, tenantID, id string, threshold int, lockUntil time.Time) error {
	res := r.db.WithContext(ctx).Exec(
		`UPDATE users
		    SET locked_until = CASE WHEN failed_login_count + 1 >= ? THEN ? ELSE locked_until END,
		        failed_login_count = failed_login_count + 1
		  WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		threshold, lockUntil, tenantID, id,
	)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.RecordFailedLogin: %w", res.Error)
	}
	return nil
}

// ResetLoginFailures clears the lockout state (failed_login_count = 0, locked_until = NULL)
// for the (tenant_id, id) user after a successful password verification. The WHERE includes
// the non-zero guard so the frequent happy path (no prior failures) never issues a write —
// the service additionally skips the call entirely when the loaded row is already clean.
func (r *UserRepository) ResetLoginFailures(ctx context.Context, tenantID, id string) error {
	res := r.db.WithContext(ctx).Exec(
		`UPDATE users
		    SET failed_login_count = 0, locked_until = NULL
		  WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL
		    AND (failed_login_count <> 0 OR locked_until IS NOT NULL)`,
		tenantID, id,
	)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.ResetLoginFailures: %w", res.Error)
	}
	return nil
}

// CountActiveAdmins returns how many usable admins remain in the tenant: rows with is_admin = 1
// AND status = 'active' that are not soft-deleted (GORM's default scope already excludes
// deleted_at IS NOT NULL). The WHERE is pinned to tenant_id so the count never leaks across
// tenants. It powers the anti-lock-out guards in the user service (refusing the delete/demote
// that would drop the last door into the /admin surface).
func (r *UserRepository) CountActiveAdmins(ctx context.Context, tenantID string) (int, error) {
	var n int64
	err := r.db.WithContext(ctx).
		Model(&User{}).
		Where("tenant_id = ? AND is_admin = ? AND status = ?", tenantID, true, "active").
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("auth.UserRepository.CountActiveAdmins: %w", err)
	}
	return int(n), nil
}

// lockedActiveAdminCount counts the tenant's active, non-deleted admins under a row-level
// FOR UPDATE lock, INSIDE the supplied transaction. Acquiring the lock here is what makes the
// last-admin guard race-free: a concurrent demote/delete transaction trying to lock the same
// admin rows must wait until this transaction commits, then sees the post-write count. Pinned
// to tenant_id so the count (and the locks) never span tenants.
func lockedActiveAdminCount(tx *gorm.DB, tenantID string) (int, error) {
	var n int64
	err := tx.Model(&User{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND is_admin = ? AND status = ?", tenantID, true, "active").
		Count(&n).Error
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// UpdateUserGuardLastAdmin applies fields to the (tenant_id, id) user but, in the SAME
// transaction, first takes a FOR UPDATE lock on the tenant's active-admin set and refuses the
// write with ErrLastAdmin when only one active admin remains. It is the race-free replacement
// for the service-side count-then-update on the demote path (is_admin=false or status leaving
// "active" on the last active admin). The caller must invoke this ONLY when the update would
// demote an active admin; benign updates use the plain UpdateUser. The WHERE on the write is
// pinned to (tenant_id, id); an unknown/cross-tenant/soft-deleted id is ErrUserNotFound, a
// duplicate email is ErrUserAlreadyExists. Returns the refreshed row.
func (r *UserRepository) UpdateUserGuardLastAdmin(ctx context.Context, tenantID, id string, fields map[string]any) (*User, error) {
	var refreshed *User
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		n, err := lockedActiveAdminCount(tx, tenantID)
		if err != nil {
			return fmt.Errorf("lock active admins: %w", err)
		}
		if n <= 1 {
			return ErrLastAdmin
		}

		if len(fields) > 0 {
			res := tx.Model(&User{}).
				Where("tenant_id = ? AND id = ?", tenantID, id).
				Updates(fields)
			if res.Error != nil {
				if isDuplicateEntry(res.Error) {
					return ErrUserAlreadyExists
				}
				return fmt.Errorf("update user: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return ErrUserNotFound
			}
		}

		var u User
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return fmt.Errorf("reload user: %w", err)
		}
		refreshed = &u
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLastAdmin) || errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrUserAlreadyExists) {
			return nil, err
		}
		return nil, fmt.Errorf("auth.UserRepository.UpdateUserGuardLastAdmin: %w", err)
	}
	return refreshed, nil
}

// SoftDeleteUserGuardLastAdmin soft-deletes the (tenant_id, id) user but, in the SAME
// transaction, first takes a FOR UPDATE lock on the tenant's active-admin set and refuses with
// ErrLastAdmin when only one active admin remains. It is the race-free replacement for the
// service-side count-then-delete when the target is the tenant's active admin. The caller must
// invoke this ONLY when the target is an active admin; deleting a non-admin/disabled target
// uses the plain SoftDeleteUser. An unknown/cross-tenant/already-deleted id is ErrUserNotFound.
func (r *UserRepository) SoftDeleteUserGuardLastAdmin(ctx context.Context, tenantID, id string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		n, err := lockedActiveAdminCount(tx, tenantID)
		if err != nil {
			return fmt.Errorf("lock active admins: %w", err)
		}
		if n <= 1 {
			return ErrLastAdmin
		}

		res := tx.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&User{})
		if res.Error != nil {
			return fmt.Errorf("delete user: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrUserNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLastAdmin) || errors.Is(err, ErrUserNotFound) {
			return err
		}
		return fmt.Errorf("auth.UserRepository.SoftDeleteUserGuardLastAdmin: %w", err)
	}
	return nil
}
