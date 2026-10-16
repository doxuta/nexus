-- EPIC 4 M1 — Workflow/Automation: sys_notifications. Backs the `in_app_notification` workflow
-- action (and a future task-inbox UI surface, ENGINE-COVERAGE-SPEC §EPIC 4 "in-app notification
-- / inbox"). One row per notification a workflow (or, later, an approval step) raises for a
-- target user inside a tenant: a durable bell/inbox item the FE reads instead of an email.
--
-- Multi-tenant invariant: every query against sys_notifications MUST filter tenant_id (rules:
-- đa tổ chức). user_id is the recipient (the principal whose inbox this lands in). entity_name /
-- record_id link the notification back to the record that triggered it (e.g. "Deal #123 won"),
-- both nullable for notifications not tied to a single record. workflow_id records which rule
-- raised it (audit / debug), nullable for non-workflow sources. is_read drives the unread badge;
-- read_at stamps when the user cleared it. No hard FK on tenant_id / user_id / record_id /
-- workflow_id by design — a notification must survive (and stay readable) even if the source
-- record or rule is later deleted. MySQL 8 / InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS sys_notifications (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    user_id     CHAR(36)     NOT NULL,                    -- recipient (inbox owner)
    title       VARCHAR(200) DEFAULT NULL,                -- optional short headline
    message     TEXT         NOT NULL,                    -- notification body
    entity_name VARCHAR(100) DEFAULT NULL,                -- source entity, NULL when not record-bound
    record_id   CHAR(36)     DEFAULT NULL,                -- source record, NULL when not record-bound
    workflow_id CHAR(36)     DEFAULT NULL,                -- rule that raised it, NULL for non-workflow sources
    is_read     TINYINT(1)   NOT NULL DEFAULT 0,
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    read_at     TIMESTAMP(3) DEFAULT NULL,
    PRIMARY KEY (id),
    -- Hot path: "my unread notifications, newest first" — tenant + user + is_read, ordered by created_at.
    KEY idx_notif_inbox (tenant_id, user_id, is_read, created_at),
    KEY idx_notif_record (tenant_id, entity_name, record_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
