-- Reverse 000072: drop the named-credential vault.
--
-- ⚠️ Dropping the table destroys every stored partner credential — the secrets exist nowhere else
-- (they are sealed only here; the master key alone cannot bring them back). Rules that still point
-- at a credentialId then record CREDENTIAL_NOT_FOUND at send time and send nothing (no crash). Roll
-- the binary back FIRST.
-- ⚠️ Once Doctor C9 (specs/050 lát S3b) has moved inline webhook secrets into this table, those
-- secrets were REMOVED from sys_workflows: running this down migration then loses every migrated
-- partner key permanently. On a database with real data this is a Founder decision (campaign §6).
DROP TABLE IF EXISTS sys_named_credentials;
