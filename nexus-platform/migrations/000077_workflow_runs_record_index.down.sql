-- Reverts 000077: drop the per-record run index. A record purge still removes the record's run
-- history after this, only by walking (and locking) every run of the tenant.
ALTER TABLE sys_workflow_runs
    DROP KEY idx_wfr_tenant_record;
