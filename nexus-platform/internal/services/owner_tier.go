package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// OwnerTierResult is what SetOwnerTier did. Converted / SkippedWithRole / OwnerRoleID are set by
// enabling, Reverted by disabling; RevokeFailed counts sessions that could not be ended (the
// switch itself is committed regardless). It carries no e-mail or user id.
type OwnerTierResult struct {
	Converted       int
	SkippedWithRole int
	Reverted        int
	OwnerRoleID     string
	RevokeFailed    int
}

// ownerTierAuditState is the slice of an account the __user__ audit row shows for a tier switch.
// By names the builder as "Họ tên (Tên xưởng)" — never an e-mail or id (specs/065 FR-030).
type ownerTierAuditState struct {
	IsAdmin        bool    `json:"isAdmin"`
	CanManageUsers bool    `json:"canManageUsers"`
	RoleID         *string `json:"roleId,omitempty"`
	By             string  `json:"by,omitempty"`
}

// SetOwnerTier converts (enabled) a customer workspace's role-less, non-operator admins into customer
// owners, or reverts (disabled) the owners back to admins — in one transaction, idempotent (a second
// call changes nothing and says 0). It is operator-only at the HTTP layer; the service still refuses a
// workspace converting ITSELF (the operator would lose its own builder area) and a workspace that is
// unknown, retired or not active.
//
// Enable: the rows are locked first (FOR UPDATE) so a concurrent role change cannot slip in, the
// reserved role is seeded, and only admins WITHOUT a role are converted — an admin who already carries
// a role was set up by hand and is left alone (SkippedWithRole). Disable: only accounts that are owners
// AND carry the reserved role go back, so a staff member the operator flagged some other way is untouched.
func (s *TenantProvisionService) SetOwnerTier(ctx context.Context, callerTenantID, targetTenantID string, enabled bool) (*OwnerTierResult, error) {
	callerTenantID, targetTenantID = strings.TrimSpace(callerTenantID), strings.TrimSpace(targetTenantID)
	if targetTenantID == "" {
		return nil, &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
	}
	if targetTenantID == callerTenantID {
		return nil, &ValidationError{Code: "TENANT_SELF_OWNER_TIER", Msg: "không đổi tầng tài khoản của chính xưởng bạn đang đứng — sẽ mất khu quản trị của bạn"}
	}
	var target auth.Tenant
	if err := s.db.WithContext(ctx).Where("id = ?", targetTenantID).First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
		}
		return nil, fmt.Errorf("services.TenantProvisionService.SetOwnerTier: load tenant: %w", err)
	}
	if target.Status != "active" {
		return nil, &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
	}
	label := s.operatorLabel(ctx, callerTenantID)

	var (
		res OwnerTierResult
		ids []string
	)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if enabled {
			if err := tx.Raw(`SELECT id FROM users WHERE tenant_id = ? AND is_admin = 1 AND is_platform_operator = 0
				AND role_id IS NULL AND deleted_at IS NULL FOR UPDATE`, targetTenantID).Scan(&ids).Error; err != nil {
				return fmt.Errorf("lock admins: %w", err)
			}
			roleID, _, err := SeedOwnerRole(ctx, tx, targetTenantID)
			if err != nil {
				return err
			}
			res.OwnerRoleID = roleID
			if len(ids) > 0 {
				if err := tx.Exec(`UPDATE users SET is_admin = 0, can_manage_users = 1, role_id = ?, updated_at = ?
					WHERE tenant_id = ? AND id IN ?`, roleID, time.Now().UTC(), targetTenantID, ids).Error; err != nil {
					return fmt.Errorf("convert admins: %w", err)
				}
			}
			res.Converted = len(ids)
			var skipped int64
			if err := tx.Raw(`SELECT COUNT(*) FROM users WHERE tenant_id = ? AND is_admin = 1 AND is_platform_operator = 0
				AND role_id IS NOT NULL AND deleted_at IS NULL`, targetTenantID).Scan(&skipped).Error; err != nil {
				return fmt.Errorf("count skipped: %w", err)
			}
			res.SkippedWithRole = int(skipped)
			return nil
		}
		var roleIDs []string
		if err := tx.Raw(`SELECT id FROM acl_roles WHERE tenant_id = ? AND name = ?`, targetTenantID, auth.OwnerRoleName).Scan(&roleIDs).Error; err != nil {
			return fmt.Errorf("find owner role: %w", err)
		}
		if len(roleIDs) == 0 {
			return nil
		}
		if err := tx.Raw(`SELECT id FROM users WHERE tenant_id = ? AND can_manage_users = 1 AND role_id = ?
			AND deleted_at IS NULL FOR UPDATE`, targetTenantID, roleIDs[0]).Scan(&ids).Error; err != nil {
			return fmt.Errorf("lock owners: %w", err)
		}
		if len(ids) > 0 {
			if err := tx.Exec(`UPDATE users SET is_admin = 1, can_manage_users = 0, role_id = NULL, updated_at = ?
				WHERE tenant_id = ? AND id IN ?`, time.Now().UTC(), targetTenantID, ids).Error; err != nil {
				return fmt.Errorf("revert owners: %w", err)
			}
		}
		res.Reverted = len(ids)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.SetOwnerTier: %w", err)
	}

	// After commit: the change lands on each account's next request, their sessions end, and the
	// trail names who did it. Revoking is best-effort — a failure is reported, not rolled back.
	before := ownerTierAuditState{IsAdmin: enabled, CanManageUsers: !enabled}
	after := ownerTierAuditState{IsAdmin: !enabled, CanManageUsers: enabled, By: label}
	for _, id := range ids {
		s.claims.Invalidate(targetTenantID, id)
		if err := s.revoker.RevokeAllForUser(ctx, targetTenantID, id); err != nil {
			res.RevokeFailed++
			s.logger.Warn("services.TenantProvisionService.SetOwnerTier: revoke sessions failed",
				zap.String("tenant_id", targetTenantID), zap.String("user_id", id), zap.Error(err))
		}
		emitMetaAuditFor(ctx, s.auditor, targetTenantID, "__user__", id, audit.ActionUpdate, before, after)
	}
	if len(ids) > 0 {
		emitMetaAudit(ctx, s.auditor, callerTenantID, "__tenant__", "owner_tier", nil, struct {
			Slug      string `json:"slug"`
			Enabled   bool   `json:"enabled"`
			Converted int    `json:"converted,omitempty"`
			Reverted  int    `json:"reverted,omitempty"`
		}{Slug: target.Slug, Enabled: enabled, Converted: res.Converted, Reverted: res.Reverted})
	}
	return &res, nil
}

// operatorLabel builds "Họ tên (Tên xưởng)" for the acting builder from their own row in their own
// tenant, read once before the transaction. Blank name => "Người dựng (Tên xưởng)". Never an e-mail.
func (s *TenantProvisionService) operatorLabel(ctx context.Context, callerTenantID string) string {
	person, shop := "Người dựng", ""
	if actor := audit.ActorFromContext(ctx); actor != "" {
		var u auth.User
		if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", callerTenantID, actor).First(&u).Error; err == nil && strings.TrimSpace(u.FullName) != "" {
			person = strings.TrimSpace(u.FullName)
		}
	}
	var t auth.Tenant
	if err := s.db.WithContext(ctx).Where("id = ?", callerTenantID).First(&t).Error; err == nil {
		shop = t.Name
	}
	if shop == "" {
		return person
	}
	return person + " (" + shop + ")"
}
