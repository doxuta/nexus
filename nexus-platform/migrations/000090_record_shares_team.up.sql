-- sys_record_shares + team grantee (DAILY-QUEUE #224 record-manual-share-team, specs/051-record-manual-share).
-- Head before this migration is v89. MySQL 8 / InnoDB / utf8mb4. Idempotent: the whole change is
-- guarded by one sentinel column (DDL is atomic in MySQL 8, so a table has all of it or none of it).
--
-- WHAT IT CHANGES
--   A share row names EXACTLY ONE grantee: a person (user_id) or a team (team_id).
--   * user_id    NOT NULL -> NULL   a team row has no person.
--   * team_id    CHAR(36) NULL      the team the record is shared with; the row filter reads it through
--                                   the viewer's team closure (dynrepo/teamtree.go) — members of the
--                                   team, and of any team ABOVE it, see the record, never read-write.
--   * grantee_id STORED generated   COALESCE(user_id, team_id): the one value a revoke / re-share is
--                                   keyed by. UUIDs never collide across users and teams.
--   * uq_record_shares              now (tenant_id, entity_name, record_id, grantee_id): one live-or-
--                                   revoked row per (record, grantee), user or team — a NULL user_id
--                                   would otherwise let team rows duplicate under the old key.
--   * idx_record_shares_team        serves the team half of the row-filter subquery
--                                   `record_id WHERE tenant_id=? AND entity_name=? AND team_id IN (…)
--                                   AND deleted_at IS NULL`, same shape as idx_record_shares_grantee.
--   * chk_record_shares_one_grantee exactly one of user_id / team_id is set (enforced by MySQL >= 8.0.16).
-- Existing rows keep user_id and get team_id NULL: nothing already shared changes meaning.
-- No hard FK (same rationale as 000073): every query pins tenant_id in the application layer.
DROP PROCEDURE IF EXISTS _record_shares_team;
CREATE PROCEDURE _record_shares_team()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_record_shares' AND COLUMN_NAME = 'grantee_id') THEN
        ALTER TABLE sys_record_shares
            MODIFY COLUMN user_id CHAR(36) NULL,
            ADD COLUMN team_id    CHAR(36) NULL AFTER user_id,
            ADD COLUMN grantee_id CHAR(36) GENERATED ALWAYS AS (COALESCE(user_id, team_id)) STORED,
            DROP KEY uq_record_shares,
            ADD UNIQUE KEY uq_record_shares (tenant_id, entity_name, record_id, grantee_id),
            ADD KEY idx_record_shares_team (tenant_id, team_id, entity_name, deleted_at, record_id),
            ADD CONSTRAINT chk_record_shares_one_grantee CHECK ((user_id IS NULL) <> (team_id IS NULL));
    END IF;
END;
CALL _record_shares_team();
DROP PROCEDURE IF EXISTS _record_shares_team;
