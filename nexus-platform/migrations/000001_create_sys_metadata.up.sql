-- Core metadata registry for the Nexus platform.
-- These tables describe entities/fields/relationships/layouts; the engine reads
-- them at runtime to generate APIs and UI — nothing here is hardcoded in Go.

CREATE TABLE IF NOT EXISTS sys_entities (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(100) NOT NULL,
    label       VARCHAR(100) NOT NULL,
    table_name  VARCHAR(100) NOT NULL,
    description TEXT         DEFAULT NULL,
    options     JSON         DEFAULT NULL,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,
    UNIQUE KEY uq_sys_entities_tenant_name (tenant_id, name),
    INDEX idx_sys_entities_tenant (tenant_id),
    INDEX idx_sys_entities_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_fields (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    entity_id   CHAR(36)     NOT NULL,
    name        VARCHAR(100) NOT NULL,
    label       VARCHAR(100) NOT NULL,
    field_type  VARCHAR(50)  NOT NULL,
    is_required TINYINT(1)   NOT NULL DEFAULT 0,
    is_unique   TINYINT(1)   NOT NULL DEFAULT 0,
    default_val VARCHAR(255) DEFAULT NULL,
    options     JSON         DEFAULT NULL,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,
    INDEX idx_sys_fields_entity (entity_id),
    INDEX idx_sys_fields_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_relationships (
    id                 CHAR(36)     NOT NULL PRIMARY KEY,
    name               VARCHAR(150) NOT NULL,
    parent_entity_name VARCHAR(100) NOT NULL,
    child_entity_name  VARCHAR(100) NOT NULL,
    relationship_type  VARCHAR(20)  NOT NULL,
    join_table_name    VARCHAR(100) DEFAULT NULL,
    created_at         TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at         TIMESTAMP    NULL DEFAULT NULL,
    INDEX idx_sys_rel_parent (parent_entity_name),
    INDEX idx_sys_rel_child (child_entity_name),
    INDEX idx_sys_rel_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_layouts (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    entity_name VARCHAR(100) NOT NULL,
    view_type   VARCHAR(50)  NOT NULL,
    layout_json JSON         DEFAULT NULL,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,
    INDEX idx_sys_layouts_tenant (tenant_id),
    INDEX idx_sys_layouts_entity (entity_name),
    INDEX idx_sys_layouts_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
