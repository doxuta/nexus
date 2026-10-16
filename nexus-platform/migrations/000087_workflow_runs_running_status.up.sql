-- DAILY-QUEUE #338 (specs/059 §7): the engine writes the sys_workflow_runs row BEFORE the rule's
-- actions run, so a queued email that gives up at once can always find its row. A row written up
-- front has no outcome yet: sys_workflow_runs.status gains 'running'. The engine finishes the row
-- when the actions return; the janitor turns a 'running' row older than an hour (the server died
-- mid-run) into 'failed'.
--
-- ENUM discipline (same as 000071): the new value is APPENDED at the END so the ordinals of the
-- existing values are unchanged; the column is otherwise byte-identical (NOT NULL, table charset) so
-- MySQL 8 applies it in place instead of copying a table that grows with every mutation. Re-applying
-- the same MODIFY is a no-op.
--
-- Deploy order: apply this BEFORE the binary that writes 'running' (the other way round, the INSERT
-- dies 1265 "Data truncated" and run history silently stops recording — lesson 000063).

ALTER TABLE sys_workflow_runs
    MODIFY COLUMN status ENUM('success','failed','skipped','blocked','running') NOT NULL;
