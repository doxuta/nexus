-- specs/062-customer-workspace L1a (DAILY-QUEUE #371, absorbs the BE of #299): one row per builder
-- workspace session — a Nexus builder working directly inside a customer's workspace. The row is the
-- authority every request of the session token is re-checked against, uncached (GetLive), together
-- with the builder's own live login (ActorLive): not closed, not past the hard 8h deadline, same
-- tenant / builder / home tenant / login session as the token claims.
--
-- tenant_id is the CUSTOMER (the token's `tid`); actor_tenant_id is the builder's home workshop
-- (`att`); actor_session_id is the home login session (refresh_tokens.session_id = `sid`) the
-- session dies with. id is the `wsid` claim. actor_label ("Full name (Workshop)") is stamped at open
-- time and read back by the audit trail — never the builder's e-mail. There is no refresh token and
-- no FK: the table is an authentication trail, not a business object (backup class: Skip).
CREATE TABLE IF NOT EXISTS sys_workspace_sessions (
    id               CHAR(36)     NOT NULL,
    tenant_id        CHAR(36)     NOT NULL,
    actor_user_id    CHAR(36)     NOT NULL,
    actor_tenant_id  CHAR(36)     NOT NULL,
    actor_session_id CHAR(36)     NOT NULL,
    actor_label      VARCHAR(191) NOT NULL,
    expires_at       TIMESTAMP(3) NOT NULL,
    closed_at        TIMESTAMP(3) NULL DEFAULT NULL,
    closed_reason    VARCHAR(16)  NULL DEFAULT NULL,
    created_by       CHAR(36)     NOT NULL,
    created_at       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at       TIMESTAMP(3) NULL DEFAULT NULL,
    PRIMARY KEY (id),
    KEY idx_ws_actor (tenant_id, actor_user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
