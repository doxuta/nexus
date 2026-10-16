-- DOWN of 000049: drop the ASSIGN-1 round-robin cursor state (v49 → v48). MySQL 8 / InnoDB.
-- The cursor rows are pure engine state (a round-robin position) — dropping them loses only the
-- current rotation offset, so a re-created table simply starts each rule from its first member
-- again. No data-loss guard needed (unlike the trigger-ENUM downs).
DROP TABLE IF EXISTS sys_assignment_cursors;
