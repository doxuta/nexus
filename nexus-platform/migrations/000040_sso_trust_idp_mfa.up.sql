-- SSO-MFA-POLICY (backlog loop/STATE.md; docs/api-conventions.md §3c.8 decision #4 follow-up):
-- sso_providers gains
--   trust_idp_mfa TINYINT(1) NOT NULL DEFAULT 0
-- — the per-provider, admin-set opt-in that lets a FOUND-IDENTITY SSO login skip the LOCAL MFA
-- step (mfaLoginGate) because the admin explicitly trusts that IdP to run its own strong MFA.
--
-- DEFAULT 0 (off) is the load-bearing safety property: every existing provider row and every
-- newly created one stays on the MFA-ENFORCE default (local MFA always enforced when the account
-- is enrolled) until a Founder/admin deliberately flips this flag via
-- PATCH /admin/sso/providers/:id. MySQL's ALTER TABLE ... NOT NULL DEFAULT 0 backfills every
-- pre-existing row with 0, so no provider silently becomes trusted by this migration.
--
-- SECURITY ASSUMPTION (documented, deliberate): trust_idp_mfa=1 is BLANKET trust in the IdP —
-- the callback does NOT inspect the id_token's amr/acr claims to verify an MFA actually happened
-- at the IdP for this specific login. See docs/api-conventions.md §3c.8.
--
-- MySQL 8 does NOT support ADD COLUMN IF NOT EXISTS, so the ALTER is wrapped in a
-- stored-procedure information_schema existence guard (same pattern as
-- 000033_sys_fields_order_description / 000026_add_notification_type). Idempotent per
-- rules/database.md. Head before this migration is v39 (000039_sys_ai_runs_budget_index).
-- MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _sso40_trust_idp_mfa_apply;
CREATE PROCEDURE _sso40_trust_idp_mfa_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sso_providers'
          AND COLUMN_NAME  = 'trust_idp_mfa'
    ) THEN
        ALTER TABLE sso_providers
            ADD COLUMN trust_idp_mfa TINYINT(1) NOT NULL DEFAULT 0 AFTER jit_provisioning;
    END IF;
END;
CALL _sso40_trust_idp_mfa_apply();
DROP PROCEDURE IF EXISTS _sso40_trust_idp_mfa_apply;
