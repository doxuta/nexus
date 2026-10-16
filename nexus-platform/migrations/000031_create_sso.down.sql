-- Reverse of 000031: drop the SSO tables (child table first, FK-safe order).

DROP TABLE IF EXISTS sso_identities;
DROP TABLE IF EXISTS sso_providers;
