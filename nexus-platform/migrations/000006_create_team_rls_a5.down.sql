-- Reverse of 000006 A5 team RLS schema. Drop in FK-dependency order so no constraint
-- dangles mid-rollback:
--   1. team_sets_teams  (FKs -> team_sets, teams)
--   2. team_memberships (FKs -> users, teams)
--   3. team_sets        (FK  -> tenants)
--   4. teams            (FK  -> tenants)
-- obj_* data is untouched (team_set_id / created_by were created in ddl, not here),
-- and migrations 000001-000005 are not affected. After this runs no team_* table,
-- constraint, or index remains.

DROP TABLE IF EXISTS team_sets_teams;
DROP TABLE IF EXISTS team_memberships;
DROP TABLE IF EXISTS team_sets;
DROP TABLE IF EXISTS teams;
