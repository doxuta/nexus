-- RECORD-HARD-DELETE (specs/056 US2, DAILY-QUEUE #47): purging a record from the recycle bin also
-- removes its automation run history, keyed only by record:
--
--   DELETE FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ?
--
-- sys_workflow_runs (mig 000032) is indexed for the runs API only — (tenant_id, workflow_id,
-- started_at) and (tenant_id, started_at) — so without this the DELETE walks every run of the
-- tenant and, inside the purge transaction, next-key-locks all of them (every automation of that
-- tenant waits on the purge to log its run). With it the DELETE touches exactly that record's runs.
--
-- In-place build (ALGORITHM=INPLACE, LOCK=NONE): the run log stays writable while it builds.
ALTER TABLE sys_workflow_runs
    ADD KEY idx_wfr_tenant_record (tenant_id, record_id);
