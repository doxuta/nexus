-- EPIC 4 M1 — Workflow/Automation foundation: sys_workflows (spec: docs/ENGINE-COVERAGE-SPEC.md
-- §EPIC 4, benchmark Salesforce Workflow Rules + SugarCRM Logic Hooks). One row per automation
-- rule a Nexus admin registers for a dynamic entity: "khi <trigger> trên <entity_name>, NẾU
-- <condition> THÌ chạy <actions>". The Event Engine hooks AFTER a record mutation commits
-- (RecordService Create/Update/SoftDelete), loads the active workflows of the entity (through a
-- cache), evaluates the condition (field op value, AND-joined), then runs the actions ASYNC and
-- FAIL-OPEN — a workflow error never breaks or slows the already-committed mutation (same
-- contract as the A7 audit dispatcher).
--
-- Multi-tenant invariant: every query against sys_workflows MUST filter tenant_id (rules: đa
-- tổ chức). entity_name carries the dynamic entity name the rule fires on ('accounts', ...).
-- trigger is an ENUM over the after-commit events. condition / actions are JSON:
--   condition = {"match":"and","rules":[{"field":"status","op":"eq","value":"won"}, ...]}
--               (an empty/NULL condition matches every event — "no entry-criteria").
--   actions   = [{"type":"field_update","field":"stage","value":"closed"},
--                {"type":"in_app_notification","userId":"<uuid>","message":"..."}]
-- is_active gates whether the Event Engine loads the rule (draft rules stay dormant).
-- audit cols: created_at / updated_at (TIMESTAMP), created_by / updated_by (actor user id),
-- deleted_at for soft-delete (a removed rule keeps its history, never fires again).
-- No hard FK on tenant_id / entity_name by design (entity_name is a logical reference into the
-- metadata registry, validated at the service layer, not a physical table). MySQL 8 / InnoDB.
CREATE TABLE IF NOT EXISTS sys_workflows (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(150) NOT NULL,                                       -- human label of the rule
    entity_name VARCHAR(100) NOT NULL,                                       -- dynamic entity the rule fires on
    trigger_event ENUM('after_create','after_update','after_delete') NOT NULL,
    `condition` JSON         DEFAULT NULL,                                   -- entry-criteria; NULL/empty = match all
    actions     JSON         NOT NULL,                                       -- ordered list of actions to run
    is_active   TINYINT(1)   NOT NULL DEFAULT 1,                             -- only active rules are loaded by the engine
    created_by  CHAR(36)     DEFAULT NULL,                                   -- actor who created the rule
    updated_by  CHAR(36)     DEFAULT NULL,                                   -- actor of the last update
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3) DEFAULT NULL,                                   -- soft-delete marker
    PRIMARY KEY (id),
    -- Hot path: the Event Engine loads "active, non-deleted rules of (tenant, entity, trigger)"
    -- after every record mutation — this composite index makes that an index range scan.
    KEY idx_wf_lookup (tenant_id, entity_name, trigger_event, is_active, deleted_at),
    KEY idx_wf_tenant (tenant_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
