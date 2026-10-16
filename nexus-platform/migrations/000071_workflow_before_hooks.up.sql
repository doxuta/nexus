-- specs/048 S1a (before-save hooks): sys_workflows.trigger_event gains 'before_save' and
-- 'before_delete' — the synchronous rules RecordService runs inside a save/delete (veto, and in later
-- slices set-value / normalize) — and sys_workflow_runs.status gains 'blocked', the run outcome of a
-- veto. No new table, no new column: the before-rule config (condition.saveOn, veto field/message)
-- lives inside the existing `condition` / `actions` JSON columns.
--
-- ENUM discipline (same as 000019 / 000048): new values are APPENDED at the END so the integer
-- ordinals of the existing values are unchanged; the element count stays far below 255 so the
-- storage size is unchanged and MySQL 8 can apply it in place. Each column definition is otherwise
-- kept byte-identical (NOT NULL, table charset/collation) — a drifted attribute would force a table
-- copy of sys_workflow_runs, which grows with every mutation. Re-applying the same MODIFY is a no-op,
-- so this is naturally idempotent.
--
-- Deploy order: apply this BEFORE running a binary that can save a before rule or write a
-- 'blocked' run (the other way round, the INSERT dies 1265 "Data truncated" — lesson 000063).

ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete','scheduled','date_offset','before_save','before_delete') NOT NULL;

ALTER TABLE sys_workflow_runs
    MODIFY COLUMN status ENUM('success','failed','skipped','blocked') NOT NULL;
