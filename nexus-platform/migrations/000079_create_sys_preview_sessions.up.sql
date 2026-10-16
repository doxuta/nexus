-- specs/064-preview-as-role-sample-data S9-1 (DAILY-QUEUE #393, solves #298 part 1): one row per
-- "preview as role" session a builder opens from Studio. It is the durable trail of the session
-- (written synchronously BEFORE the token is minted) and the thing every request of the preview
-- token is re-checked against, uncached: not ended, not expired, same role / principal / builder,
-- and the builder's own login session (actor_session_id) still has a live refresh token.
--
-- principal_id is the synthetic subject UUIDv5(ns, tenant|role) — no users row, never admin.
-- actor_tenant_id equals tenant_id until specs/062-customer-workspace lets a builder preview inside
-- a customer workspace (S9-8); workspace_session_id and draft are reserved for S9-8 / S9-9 so those
-- slices need no migration. actor_session_id is NOT NULL: a builder token without `sid` never gets
-- a preview token (there would be no login session to revoke it with).
CREATE TABLE IF NOT EXISTS sys_preview_sessions (
    id                   CHAR(36)    NOT NULL,
    tenant_id            CHAR(36)    NOT NULL,
    role_id              CHAR(36)    NOT NULL,
    principal_id         CHAR(36)    NOT NULL,
    actor_user_id        CHAR(36)    NOT NULL,
    actor_tenant_id      CHAR(36)    NOT NULL,
    actor_session_id     CHAR(36)    NOT NULL,
    workspace_session_id CHAR(36)    NULL DEFAULT NULL,
    app_id               CHAR(36)    NULL DEFAULT NULL,
    draft                TINYINT(1)  NOT NULL DEFAULT 0,
    expires_at           TIMESTAMP   NOT NULL,
    ended_at             TIMESTAMP   NULL DEFAULT NULL,
    end_reason           VARCHAR(16) NULL DEFAULT NULL,
    created_by           CHAR(36)    NOT NULL,
    created_at           TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at           TIMESTAMP   NULL DEFAULT NULL,
    PRIMARY KEY (id),
    KEY idx_preview_actor   (tenant_id, actor_user_id, ended_at),
    KEY idx_preview_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
