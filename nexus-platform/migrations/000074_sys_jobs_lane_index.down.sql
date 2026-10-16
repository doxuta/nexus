-- Reverts 000074: drop the dedicated-lane claim index. The dedicated claim still works after this
-- (it rides idx_sys_jobs_claim and filters job_type), only slower under a mixed due backlog.
ALTER TABLE sys_jobs
    DROP KEY idx_sys_jobs_lane;
