-- Reverse of 000030: drop the MFA tables (child tables first, FK-safe order).

DROP TABLE IF EXISTS mfa_pending_tokens;
DROP TABLE IF EXISTS mfa_backup_codes;
DROP TABLE IF EXISTS user_mfa;
