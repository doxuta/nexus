-- A7 Security Alerts audit table (spec: docs/security-alerts.md §4). One row per
-- security-alert email the platform attempts to send, so the async notifier has a
-- durable audit trail (truy vết) and a basis for future dedupe/rate-limit. The send
-- path is fail-open: a row is written before/around the SMTP attempt and the worker
-- flips status -> sent/failed afterwards; nothing here blocks the originating business
-- flow (password change, role change, account disable/delete, refresh reuse).
--
-- Multi-tenant invariant: every query against security_alerts MUST filter tenant_id
-- (rules: đa tổ chức). recipient snapshots the user's email at send time so the audit
-- stays accurate even if the user later changes their address. No soft-delete column:
-- this is an append-only audit log, never updated except status/sent_at by the worker.
-- No hard FK on tenant_id/user_id by design — alerts must survive (and remain
-- auditable) even if the account is later deleted (E4 = account_deleted is itself one
-- of the events recorded here). MySQL 8 / InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS security_alerts (
    id         CHAR(36)     NOT NULL,
    tenant_id  CHAR(36)     NOT NULL,
    user_id    CHAR(36)     NOT NULL,
    alert_type VARCHAR(40)  NOT NULL,                    -- password_changed / role_changed / account_disabled / account_deleted / refresh_reuse
    channel    VARCHAR(20)  NOT NULL DEFAULT 'email',
    recipient  VARCHAR(255) NOT NULL,                    -- email snapshot at send time
    status     VARCHAR(20)  NOT NULL DEFAULT 'pending',  -- pending / sent / failed / skipped
    error      TEXT         NULL,
    created_at DATETIME     NOT NULL,
    sent_at    DATETIME     NULL,
    PRIMARY KEY (id),
    KEY idx_security_alerts_tenant_user (tenant_id, user_id),
    KEY idx_security_alerts_type (tenant_id, alert_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
