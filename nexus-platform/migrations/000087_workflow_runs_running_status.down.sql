-- Down for 000087: a 'running' row has no ENUM slot in the narrower column, so it is closed as
-- 'failed' first (history keeps the row; nothing is deleted) and then the ENUM is narrowed back.
UPDATE sys_workflow_runs
SET status = 'failed',
    error = COALESCE(error, 'Lần chạy chưa kết thúc khi hạ cấp phiên bản')
WHERE status = 'running';

ALTER TABLE sys_workflow_runs
    MODIFY COLUMN status ENUM('success','failed','skipped','blocked') NOT NULL;
