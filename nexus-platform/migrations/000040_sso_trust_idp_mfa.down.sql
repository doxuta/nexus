-- Reversal of 000040_sso_trust_idp_mfa.up.sql — drops sso_providers.trust_idp_mfa.
-- Guarded via information_schema (MySQL 8 has no DROP COLUMN IF EXISTS) so a partial apply
-- reverts cleanly — same pattern as 000033's down. Reverting is SAFE-by-direction: dropping the
-- column returns every provider to the MFA-ENFORCE default (local MFA always enforced), i.e. the
-- rollback can only make the system MORE strict, never less. Reverts to v39.

DROP PROCEDURE IF EXISTS _sso40_trust_idp_mfa_revert;
CREATE PROCEDURE _sso40_trust_idp_mfa_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sso_providers'
          AND COLUMN_NAME  = 'trust_idp_mfa'
    ) THEN
        ALTER TABLE sso_providers DROP COLUMN trust_idp_mfa;
    END IF;
END;
CALL _sso40_trust_idp_mfa_revert();
DROP PROCEDURE IF EXISTS _sso40_trust_idp_mfa_revert;
