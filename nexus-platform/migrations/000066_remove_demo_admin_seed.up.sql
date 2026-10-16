-- Remove the source-committed demo admin (issue #95) from databases that already ran 000003.
--
-- 000003 seeded 'admin@demo.local' with is_admin=1 and the bcrypt hash of "ChangeMe!123", a
-- password written into this repository. 000003 has been fixed so fresh installs never create it,
-- but that edit is invisible to every database where 000003 already applied — which is all of
-- them. This migration closes those.
--
-- THE WHERE CLAUSE IS THE WHOLE DESIGN. The delete fires only when the row is still, byte for
-- byte, the untouched seed: same id, same email, AND the same committed password_hash. An operator
-- who followed GO-LIVE-CHECKLIST.md §5 and rotated the password has a DIFFERENT hash (bcrypt salts
-- per call, so even re-hashing "ChangeMe!123" would not match), and their account — possibly the
-- only admin they can log in as — is left completely alone. A promoted-to-real demo admin is
-- likewise untouched. Deleting an admin nobody can still log into as is safe; deleting one someone
-- depends on is an outage, so the condition errs entirely toward not firing.
--
-- Cascades: every FK that references users(id) is ON DELETE CASCADE (refresh_tokens 000003,
-- team_memberships 000006, password_reset_tokens 000027, user_mfa/backup codes/pending 000030,
-- sso_identities 000031), so the row leaves cleanly and no orphan bookkeeping survives.
--
-- The demo TENANT row is deliberately kept: 000002's reference entities live under it, and a
-- tenant is a namespace, not a credential.
--
-- ⚠️ BLOCKING PRE-DEPLOY CHECK. The guard fires exactly when the password was never rotated — so
-- if anyone is still logging into production as admin@demo.local / ChangeMe!123, this deletes the
-- account they use. Before deploying the release that carries this migration, run:
--   SELECT COUNT(*) FROM users WHERE is_admin = 1 AND email <> 'admin@demo.local';
-- 0  -> STOP: create a real admin with cmd/bootstrap-admin first, log in once, then deploy.
-- >0 -> safe. (deploy/GO-LIVE-CHECKLIST.md §2b.)
--
-- After this runs, cmd/bootstrap-admin is the only path to a first admin:
--   docker compose -f deploy/nexus/compose.prod.yml --env-file deploy/nexus/.env.prod run --rm -e BOOTSTRAP_TENANT=... -e BOOTSTRAP_ADMIN_EMAIL=... \
--     -e BOOTSTRAP_ADMIN_PASSWORD=... --entrypoint /app/bootstrap-admin nexus
-- Local dev databases seeded before this migration lose the demo login too — that is the point;
-- use bootstrap-admin there as well.
DELETE FROM users
 WHERE id = '99999999-9999-9999-9999-999999999901'
   AND email = 'admin@demo.local'
   AND password_hash = '$2a$12$5ty7htFXswRE/iMR1yMcFOsn6zxVPEI8WvV2bRaLWBuaawGT643AS';
