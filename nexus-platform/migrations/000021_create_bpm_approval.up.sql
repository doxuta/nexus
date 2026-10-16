-- EPIC 5 M1 — BPM / Approval / SLA foundation (spec: docs/ENGINE-COVERAGE-SPEC.md §EPIC 5;
-- benchmark Salesforce Approval Processes + SugarCRM/ServiceNow approval + SLA engines). This is
-- the first BPM substrate in Nexus (ARCHITECTURE.md §6 was ⬜). It turns the metadata-driven
-- database into business software with a HUMAN gate: an admin registers an approval PROCESS
-- ("khi <trigger> trên <entity>, NẾU <criteria> THÌ chờ duyệt tuần tự theo <steps>"), and when a
-- record matches the criteria the engine spins up an approval INSTANCE that blocks until each step
-- is approved (sequential), or short-circuits on the first rejection.
--
-- Head before this migration is v20 (000020_create_sys_reports). Reverts cleanly to v20.
-- (Migration 000018 was skipped in the original numbering; 000021 is the next free slot — the lead
-- renumbers on gate if it collides with a sibling worker.) MySQL 8 / InnoDB / utf8mb4.
--
-- Multi-tenant invariant: every query against either table MUST filter tenant_id (rules: đa tổ
-- chức). No hard FK on tenant_id / entity_name / record_id by design — entity_name is a logical
-- reference into the metadata registry (validated at the service layer) and record_id points into
-- a dynamic obj_* table, so a physical FK is impossible. Reuses the same fail-open + tenant-pin
-- contract as the EPIC 4 workflow engine.

-- ------------------------------------------------------------------------------------------------
-- sys_approval_processes: the DEFINITION an admin registers (one row per approval rule).
--   trigger_event = the after-commit event that arms the process (after_create / after_update).
--   criteria      = entry-criteria JSON {"match":"and","rules":[{"field","op","value"}]} — same
--                   vocabulary as sys_workflows.condition; NULL/empty = match every event.
--   steps         = ORDERED JSON array of approval steps, each
--                   {"name":"...","approverType":"user"|"role","approverId":"<uuid>",
--                    "slaSeconds":N} — N=0 means "no SLA on this step". The engine walks the array
--                   in order; step i+1 only opens once step i is approved (sequential).
--   is_active gates whether the engine arms the process (draft processes stay dormant).
CREATE TABLE IF NOT EXISTS sys_approval_processes (
    id            CHAR(36)     NOT NULL,
    tenant_id     CHAR(36)     NOT NULL,
    name          VARCHAR(150) NOT NULL,                                    -- human label of the process
    entity_name   VARCHAR(100) NOT NULL,                                    -- dynamic entity the process arms on
    trigger_event ENUM('after_create','after_update') NOT NULL,            -- after-commit event that arms it
    criteria      JSON         DEFAULT NULL,                                -- entry-criteria; NULL/empty = match all
    steps         JSON         NOT NULL,                                    -- ordered list of approval steps
    is_active     TINYINT(1)   NOT NULL DEFAULT 1,                          -- only active processes are armed
    created_by    CHAR(36)     DEFAULT NULL,
    updated_by    CHAR(36)     DEFAULT NULL,
    created_at    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at    TIMESTAMP(3) DEFAULT NULL,                                -- soft-delete marker
    PRIMARY KEY (id),
    -- Hot path: the engine loads "active, non-deleted processes of (tenant, entity, trigger)" after
    -- every matching record mutation — this composite index makes that an index range scan.
    KEY idx_ap_lookup (tenant_id, entity_name, trigger_event, is_active, deleted_at),
    KEY idx_ap_tenant (tenant_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ------------------------------------------------------------------------------------------------
-- sys_approval_instances: a RUNNING approval (one row per record that entered a process).
--   process_id    = the sys_approval_processes row that spawned it.
--   entity_name / record_id = the dynamic record under approval.
--   current_step  = 0-based index into the process steps array of the step now awaiting a decision
--                   (meaningless once status is terminal).
--   status        = pending (awaiting current step) | approved (all steps cleared) |
--                   rejected (a step was rejected — terminal) | cancelled (process removed/recalled).
--   current_approver_type / current_approver_id = denormalised from the active step so the
--                   "pending for me" query is a cheap indexed lookup (no JSON walk per request).
--   sla_due_at    = absolute UTC deadline of the CURRENT step (NULL when the step has no SLA).
--                   The SLA scheduler scans pending instances past their due date and escalates.
--   escalated     = 1 once the SLA scheduler has fired the breach action for the current step
--                   (an idempotency flag so a breach notifies/escalates AT MOST ONCE per step).
--   history       = append-only JSON audit array, one entry per decision
--                   {"step":i,"action":"approved"|"rejected"|"escalated","actor":"<uuid>",
--                    "comment":"...","at":"<rfc3339>"}.
CREATE TABLE IF NOT EXISTS sys_approval_instances (
    id                    CHAR(36)     NOT NULL,
    tenant_id             CHAR(36)     NOT NULL,
    process_id            CHAR(36)     NOT NULL,                            -- definition that spawned it
    entity_name           VARCHAR(100) NOT NULL,                           -- dynamic entity under approval
    record_id             CHAR(36)     NOT NULL,                           -- record under approval
    current_step          INT UNSIGNED NOT NULL DEFAULT 0,                 -- 0-based active step index
    status                ENUM('pending','approved','rejected','cancelled') NOT NULL DEFAULT 'pending',
    current_approver_type ENUM('user','role') DEFAULT NULL,                -- denormalised active-step approver kind
    current_approver_id   CHAR(36)     DEFAULT NULL,                        -- denormalised active-step approver id
    sla_due_at            TIMESTAMP(3) NULL DEFAULT NULL,                   -- current step deadline (NULL = no SLA)
    escalated             TINYINT(1)   NOT NULL DEFAULT 0,                  -- breach already actioned for current step
    history               JSON         NOT NULL,                           -- append-only decision log
    created_by            CHAR(36)     DEFAULT NULL,                        -- actor whose mutation armed it (submitter)
    created_at            TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at            TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    -- Hot path "pending for me": tenant + approver kind/id + status, newest-first.
    KEY idx_ai_pending (tenant_id, current_approver_type, current_approver_id, status, created_at),
    -- SLA scan: pending instances of a tenant past their due date that have not yet escalated.
    KEY idx_ai_sla (status, escalated, sla_due_at),
    -- Idempotency lookup: the engine checks "is there already a PENDING instance for this
    -- (tenant, record)?" before arming a new one, so a record under approval is not double-armed.
    -- status is included so the check is an index range scan and a record can re-enter a NEW
    -- process once its prior instance reaches a terminal (approved/rejected/cancelled) state.
    KEY idx_ai_record (tenant_id, entity_name, record_id, status),
    KEY idx_ai_process (tenant_id, process_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
