package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// UserMFARepository persists and consumes user_mfa rows over GORM. Every query is
// parameterized and pinned to tenant_id (rules/database.md) — there is no tenant-agnostic
// lookup on this repository (unlike the pending-token/reset-token repos, MFA config is always
// looked up by an already-authenticated {tenant,user}, never by an opaque client credential).
type UserMFARepository struct {
	db  *gorm.DB
	now func() time.Time
}

// NewUserMFARepository wires a UserMFARepository over an existing GORM connection.
func NewUserMFARepository(db *gorm.DB) *UserMFARepository {
	return &UserMFARepository{db: db, now: time.Now}
}

// Get returns the MFA row for (tenant, user). Returns ErrMFANotEnrolled when no row exists.
func (r *UserMFARepository) Get(ctx context.Context, tenantID, userID string) (*UserMFA, error) {
	var m UserMFA
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMFANotEnrolled
	}
	if err != nil {
		return nil, fmt.Errorf("auth.UserMFARepository.Get: %w", err)
	}
	return &m, nil
}

// Create inserts a brand-new (never-enrolled-before) MFA row with enabled=0. Fails with a
// UNIQUE(tenant_id,user_id) DB error if a row already exists — callers must Get first (the
// AuthService enroll flow always does) to decide between Create and UpdateSecret.
func (r *UserMFARepository) Create(ctx context.Context, m *UserMFA) error {
	if err := r.db.WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("auth.UserMFARepository.Create: %w", err)
	}
	return nil
}

// UpdateSecret overwrites the sealed secret and resets last_used_step to 0 for a row the
// caller has already confirmed is NOT enabled (a re-enroll before activation completes). It
// deliberately does not touch the enabled column.
func (r *UserMFARepository) UpdateSecret(ctx context.Context, tenantID, userID, secretEnc string) error {
	res := r.db.WithContext(ctx).
		Model(&UserMFA{}).
		Where("tenant_id = ? AND user_id = ? AND enabled = ?", tenantID, userID, false).
		Updates(map[string]any{"secret_enc": secretEnc, "last_used_step": 0, "updated_at": r.now()})
	if res.Error != nil {
		return fmt.Errorf("auth.UserMFARepository.UpdateSecret: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("auth.UserMFARepository.UpdateSecret: no matching not-yet-enabled row")
	}
	return nil
}

// ActivateWithStepAndBackupCodes atomically flips enabled=1 (stamping last_used_step=step,
// guarded by enabled=0 CAS — only the FIRST caller to activate a given enrollment wins, so a
// concurrent double-submit of POST /auth/mfa/activate cannot both succeed) AND inserts the
// freshly generated backup-code rows, in ONE database transaction (reviewer LOW-1 fix).
//
// Before this fix the enable-flip and the backup-code insert were two independent calls
// (ActivateWithStep then MFABackupCodeRepository.StoreBatch): if the insert failed AFTER the
// flip committed (a transient DB error, a CSPRNG failure surfacing between the two calls,
// etc.), the account was left permanently "MFA enabled" with ZERO backup codes and no way to
// recover except disabling via a still-valid TOTP code — re-running activate would immediately
// hit auth.ErrMFAAlreadyEnabled since the row was already enabled. Wrapping both writes in one
// transaction means a failed insert rolls back the flip too, leaving the row exactly as it was
// before the call (enabled=0) so the caller can simply retry activation.
//
// Returns (false, nil) — never an error — when the CAS itself is lost (someone already
// activated, or the row was never in the not-yet-enabled state); the caller maps that to
// auth.ErrMFAAlreadyEnabled exactly like the pre-fix ActivateWithStep did. A non-nil error means
// the transaction itself failed (CAS never evaluated meaningfully, or the insert failed and the
// whole transaction rolled back) — the caller wraps and surfaces it as an internal fault, and
// the row is guaranteed to still read enabled=0.
func (r *UserMFARepository) ActivateWithStepAndBackupCodes(ctx context.Context, tenantID, userID string, step int64, codes []*MFABackupCode) (bool, error) {
	var activated bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&UserMFA{}).
			Where("tenant_id = ? AND user_id = ? AND enabled = ?", tenantID, userID, false).
			Updates(map[string]any{"enabled": true, "last_used_step": step, "updated_at": r.now()})
		if res.Error != nil {
			return fmt.Errorf("activate: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Lost the CAS — not an error, just "nothing to do" so the transaction commits an
			// effective no-op. activated stays false; caller maps this to ErrMFAAlreadyEnabled.
			return nil
		}
		if len(codes) > 0 {
			if err := tx.Create(&codes).Error; err != nil {
				// Returning an error here rolls back the enabled=1 update above too.
				return fmt.Errorf("insert backup codes: %w", err)
			}
		}
		activated = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("auth.UserMFARepository.ActivateWithStepAndBackupCodes: %w", err)
	}
	return activated, nil
}

// ConsumeStep atomically advances last_used_step to step, guarded by enabled=1 AND
// last_used_step < step (CAS). This is the replay guard (../specs/003-mfa-totp/spec.md §"#7"
// MUST-FIX 2): step is the MATCHED time-step the presented code actually validated against
// (not wall-clock "now"), so a code resubmitted for a step at-or-before the last accepted one
// is rejected even when it is still inside the +-1 skew window. Returns whether THIS call won
// the race (false ⇒ treat the code as invalid — replay or a genuinely wrong code).
func (r *UserMFARepository) ConsumeStep(ctx context.Context, tenantID, userID string, step int64) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&UserMFA{}).
		Where("tenant_id = ? AND user_id = ? AND enabled = ? AND last_used_step < ?", tenantID, userID, true, step).
		Updates(map[string]any{"last_used_step": step, "updated_at": r.now()})
	if res.Error != nil {
		return false, fmt.Errorf("auth.UserMFARepository.ConsumeStep: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Delete hard-deletes the MFA row for (tenant, user). MFA disable is a genuine secret-removal
// operation (the sealed TOTP seed should not linger once the user has turned MFA off), unlike
// the soft-delete convention used for business records — see rules/database.md's soft-delete
// rule, which governs record DATA, not security-secret lifecycle.
func (r *UserMFARepository) Delete(ctx context.Context, tenantID, userID string) error {
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Delete(&UserMFA{}).Error; err != nil {
		return fmt.Errorf("auth.UserMFARepository.Delete: %w", err)
	}
	return nil
}

// MFABackupCodeRepository persists and consumes mfa_backup_codes rows over GORM. Every query
// is tenant-pinned.
type MFABackupCodeRepository struct {
	db  *gorm.DB
	now func() time.Time
}

// NewMFABackupCodeRepository wires a MFABackupCodeRepository over an existing GORM connection.
func NewMFABackupCodeRepository(db *gorm.DB) *MFABackupCodeRepository {
	return &MFABackupCodeRepository{db: db, now: time.Now}
}

// StoreBatch inserts the freshly generated set of backup-code hashes (only hashes — the raw
// codes are returned to the caller once and never persisted).
func (r *MFABackupCodeRepository) StoreBatch(ctx context.Context, codes []*MFABackupCode) error {
	if len(codes) == 0 {
		return nil
	}
	if err := r.db.WithContext(ctx).Create(&codes).Error; err != nil {
		return fmt.Errorf("auth.MFABackupCodeRepository.StoreBatch: %w", err)
	}
	return nil
}

// ConsumeByHash atomically claims a live backup code: a single conditional UPDATE ... WHERE
// tenant_id = ? AND user_id = ? AND code_hash = ? AND used_at IS NULL, reporting whether THIS
// call affected a row. Under a concurrent double-submit of the same code, only the winning
// caller's UPDATE flips the row; every other caller sees affected=0 (single-use CAS, mirrors
// PasswordResetTokenRepository.ConsumeByHash).
func (r *MFABackupCodeRepository) ConsumeByHash(ctx context.Context, tenantID, userID, codeHash string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&MFABackupCode{}).
		Where("tenant_id = ? AND user_id = ? AND code_hash = ? AND used_at IS NULL", tenantID, userID, codeHash).
		Update("used_at", r.now())
	if res.Error != nil {
		return false, fmt.Errorf("auth.MFABackupCodeRepository.ConsumeByHash: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteAllForUser hard-deletes every backup code for (tenant, user) — called on MFA disable
// alongside UserMFARepository.Delete, same rationale (secret-removal, not soft-delete data).
func (r *MFABackupCodeRepository) DeleteAllForUser(ctx context.Context, tenantID, userID string) error {
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Delete(&MFABackupCode{}).Error; err != nil {
		return fmt.Errorf("auth.MFABackupCodeRepository.DeleteAllForUser: %w", err)
	}
	return nil
}

// MFAPendingTokenRepository persists and consumes mfa_pending_tokens rows over GORM. FindByHash
// is deliberately tenant-agnostic (mirrors PasswordResetTokenRepository — see migration 000030
// header and ../specs/003-mfa-totp/spec.md §"#7"): POST /auth/mfa/verify carries only
// {pendingToken, code}, no tenant, and UNIQUE(token_hash) makes the lookup unambiguous. The
// caller derives tenant_id FROM the returned row and passes it into every subsequent
// tenant-pinned call.
type MFAPendingTokenRepository struct {
	db  *gorm.DB
	now func() time.Time
}

// NewMFAPendingTokenRepository wires a MFAPendingTokenRepository over an existing GORM connection.
func NewMFAPendingTokenRepository(db *gorm.DB) *MFAPendingTokenRepository {
	return &MFAPendingTokenRepository{db: db, now: time.Now}
}

// Store inserts a new pending-token record. The caller passes the already-hashed token; the
// raw token is handed to the client and never written to the database.
func (r *MFAPendingTokenRepository) Store(ctx context.Context, pt *MFAPendingToken) error {
	if err := r.db.WithContext(ctx).Create(pt).Error; err != nil {
		return fmt.Errorf("auth.MFAPendingTokenRepository.Store: %w", err)
	}
	return nil
}

// FindByHash returns the pending-token row for a token hash regardless of tenant (see type
// doc). It does not itself filter on used_at/expires_at — the caller (AuthService) inspects
// those and/or relies on the atomic ConsumeByHash to enforce single-use + expiry. Returns
// ErrMFAPendingInvalid when no row matches the hash at all.
func (r *MFAPendingTokenRepository) FindByHash(ctx context.Context, tokenHash string) (*MFAPendingToken, error) {
	var pt MFAPendingToken
	err := r.db.WithContext(ctx).
		Where("token_hash = ?", tokenHash).
		First(&pt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMFAPendingInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth.MFAPendingTokenRepository.FindByHash: %w", err)
	}
	return &pt, nil
}

// ConsumeByHash atomically claims a live pending token: a single conditional UPDATE ... WHERE
// tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?, reporting whether
// THIS call affected a row. tenantID MUST come from the row already resolved by FindByHash
// (never from client input) — mirrors PasswordResetTokenRepository.ConsumeByHash exactly,
// including the concurrent-double-submit single-winner guarantee.
func (r *MFAPendingTokenRepository) ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&MFAPendingToken{}).
		Where("tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?",
			tenantID, tokenHash, r.now()).
		Update("used_at", r.now())
	if res.Error != nil {
		return false, fmt.Errorf("auth.MFAPendingTokenRepository.ConsumeByHash: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteAllForUser hard-deletes every pending token for (tenant, user) — called from
// AuthService.MFADisable (reviewer LOW-2 hygiene fix) alongside UserMFARepository.Delete and
// MFABackupCodeRepository.DeleteAllForUser. NOT a security hole on its own even without this
// call: CompleteMFALogin re-checks the user_mfa row's Enabled flag and returns
// ErrMFAPendingInvalid once MFA has been disabled, so a leftover pending token can never
// complete a login. This purely avoids leaving orphaned rows for a since-disabled account.
func (r *MFAPendingTokenRepository) DeleteAllForUser(ctx context.Context, tenantID, userID string) error {
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Delete(&MFAPendingToken{}).Error; err != nil {
		return fmt.Errorf("auth.MFAPendingTokenRepository.DeleteAllForUser: %w", err)
	}
	return nil
}
