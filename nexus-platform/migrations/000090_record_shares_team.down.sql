-- Reverse 000090: back to person-only shares. ROLL THE BINARY BACK FIRST: a binary of #224 or later
-- reads team_id / grantee_id on every owner/team-level read (and on the share routes).
--
-- What is lost: every share addressed to a TEAM is deleted (the old schema has no place for it) — the
-- team's members lose the rows they reached only through it (fail closed: nobody gains access). Person
-- shares are untouched. Idempotent: guarded by the sentinel column.
DROP PROCEDURE IF EXISTS _record_shares_team_down;
CREATE PROCEDURE _record_shares_team_down()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_record_shares' AND COLUMN_NAME = 'grantee_id') THEN
        DELETE FROM sys_record_shares WHERE user_id IS NULL;
        ALTER TABLE sys_record_shares
            DROP CONSTRAINT chk_record_shares_one_grantee,
            DROP KEY idx_record_shares_team,
            DROP KEY uq_record_shares,
            DROP COLUMN grantee_id,
            DROP COLUMN team_id,
            MODIFY COLUMN user_id CHAR(36) NOT NULL,
            ADD UNIQUE KEY uq_record_shares (tenant_id, entity_name, record_id, user_id);
    END IF;
END;
CALL _record_shares_team_down();
DROP PROCEDURE IF EXISTS _record_shares_team_down;
