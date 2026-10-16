-- Reverts 000075: drop the tenant listing index. GET /admin/jobs still answers correctly after
-- this (it reads the tenant's rows through idx_sys_jobs_dedup and sorts them), only slower.
ALTER TABLE sys_jobs
    DROP KEY idx_sys_jobs_tenant_list;
