-- Reverse SYS-JOBS-DEDUP-INDEX. No data is lost: payload_hash is VIRTUAL (derived from payload,
-- never written by anyone). The dedup probe goes back to a scan of the tenant's rows.
--
-- The post-000067 binary keeps working on this schema: its probe never names payload_hash (it
-- writes the generated expression over `payload`), so it only loses the seek.
ALTER TABLE sys_jobs
    ADD KEY idx_sys_jobs_tenant (tenant_id),
    DROP KEY idx_sys_jobs_dedup;

ALTER TABLE sys_jobs DROP COLUMN payload_hash;
