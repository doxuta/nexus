-- ASSIGN-1 (engine-depth primitive #4 — docs/assignment-rule-spec.md §3): the round-robin
-- cursor state for the `assign` workflow action. One row per (tenant, workflow, action_key)
-- remembers the member picked LAST TIME so the next fire hands the record to the next member
-- in the team (wrap at the end). It is engine STATE (like sys_jobs), not a business obj_* table,
-- so it deliberately carries no created_by/team_set/deleted_at — no soft-delete, no RLS.
--
-- Concurrency contract (spec §4): the assign action advances this cursor inside ONE transaction
-- with a `SELECT last_user_id ... FOR UPDATE` row lock, so two records firing the SAME rule
-- concurrently serialize and receive DIFFERENT members. The UNIQUE key is the lock target.
CREATE TABLE IF NOT EXISTS sys_assignment_cursors (
    id            CHAR(36)    NOT NULL PRIMARY KEY,
    tenant_id     CHAR(36)    NOT NULL,
    workflow_id   CHAR(36)    NOT NULL,
    action_key    VARCHAR(64) NOT NULL,          -- action index within the rule (e.g. "a0") — two assign actions in one rule keep two independent cursors
    team_id       CHAR(36)    NOT NULL,          -- the team the members are drawn from (cosmetic: the member list is resolved from the action's team param, not this column)
    last_user_id  CHAR(36)    DEFAULT NULL,      -- member picked LAST TIME; NULL = never rotated yet (start from first)
    updated_at    TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_asgn_cursor (tenant_id, workflow_id, action_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
