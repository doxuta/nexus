-- A5 Team-based row-level security schema (spec: docs/auth-security.md §A5, lines
-- 96-103; Nexus team mapping per .claude/rules/database.md). Four tables back the
-- row-level filter on obj_* data, which already carries team_set_id + created_by
-- (internal/ddl/create_table.go). A record is visible when created_by = user
-- (owner) OR its team_set_id resolves to a team the user belongs to (team) --
-- gated by acl_role_actions.access_level (A4, none/owner/team/all).
--
-- Modeling notes (Nexus lesson, rules:162-169):
--   * team_memberships   = user <-> team      (who is on a team)
--   * team_sets_teams     = team_set <-> team  (composition of a set; DISTINCT from
--                                               memberships) -- a record's
--                                               team_set_id points at a team_sets row,
--                                               which expands to N teams here.
--   * team_sets.team_md5  = MD5 of the sorted team-id list, so identical team sets
--                           dedupe to one row per tenant (UNIQUE(tenant_id, team_md5)).
-- tenant_id lives on teams + team_sets so the repo layer keeps team/teamset lookups
-- inside one tenant (no cross-tenant leak). MySQL 8 / InnoDB / utf8mb4.

-- teams: a team within a tenant; parent_id gives an optional self-referential
-- hierarchy. parent_id is intentionally NOT a hard FK (NULL-able, hierarchy can be
-- reparented/seeded out of order); it is indexed for descendant lookups.
CREATE TABLE IF NOT EXISTS teams (
    id         CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)     NOT NULL,
    name       VARCHAR(150) NOT NULL,
    parent_id  CHAR(36)     DEFAULT NULL,            -- self-ref hierarchy (no hard FK)
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_teams_tenant (tenant_id),
    INDEX idx_teams_parent (parent_id),
    CONSTRAINT fk_teams_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- team_memberships: the user<->team join. UNIQUE(user_id, team_id) makes a user a
-- member of a team at most once. Both FKs cascade so removing a user or a team
-- cleans up its memberships. idx_tm_team backs the membership->team_set expansion
-- in the row-level filter (WHERE tm.user_id = ? joined on tm.team_id).
CREATE TABLE IF NOT EXISTS team_memberships (
    id         CHAR(36)  NOT NULL PRIMARY KEY,
    user_id    CHAR(36)  NOT NULL,
    team_id    CHAR(36)  NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_tm_user_team (user_id, team_id),
    INDEX idx_tm_team (team_id),
    CONSTRAINT fk_tm_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_tm_team FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- team_sets: a deduplicated set of teams per tenant. team_md5 is the MD5 hash of the
-- canonical (sorted) member team-id list; UNIQUE(tenant_id, team_md5) lets the record
-- layer resolve-or-create a set without duplicating identical sets. primary_team_id is
-- an optional convenience pointer to the "owning" team of the set (NULL-able, no hard
-- FK so a set may be built before any primary is chosen).
CREATE TABLE IF NOT EXISTS team_sets (
    id              CHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id       CHAR(36)  NOT NULL,
    team_md5        CHAR(32)  NOT NULL,              -- MD5 of sorted team-id list (dedupe key)
    primary_team_id CHAR(36)  DEFAULT NULL,          -- optional owning team (no hard FK)
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_ts_tenant_md5 (tenant_id, team_md5),
    INDEX idx_ts_tenant (tenant_id),
    CONSTRAINT fk_ts_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- team_sets_teams: composition of a team_set (team_set <-> team). PRIMARY KEY
-- (team_set_id, team_id) makes each (set, team) pair single-valued AND indexes the
-- set->team direction. idx_tst_team indexes the team->set direction, which is the hot
-- path of the row-level team filter:
--   team_set_id IN (SELECT st.team_set_id FROM team_sets_teams st
--                   JOIN team_memberships tm ON tm.team_id = st.team_id
--                   WHERE tm.user_id = ?)
-- Both FKs cascade so dropping a team_set or a team removes its composition rows.
CREATE TABLE IF NOT EXISTS team_sets_teams (
    team_set_id CHAR(36) NOT NULL,
    team_id     CHAR(36) NOT NULL,
    PRIMARY KEY (team_set_id, team_id),
    INDEX idx_tst_team (team_id),
    CONSTRAINT fk_tst_set  FOREIGN KEY (team_set_id) REFERENCES team_sets (id) ON DELETE CASCADE,
    CONSTRAINT fk_tst_team FOREIGN KEY (team_id)     REFERENCES teams (id)     ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
