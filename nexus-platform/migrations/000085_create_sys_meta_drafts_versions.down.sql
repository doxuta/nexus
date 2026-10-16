-- Reverts 000085: drops the draft/publish layer (drafts and version history go with it).
DROP TABLE IF EXISTS sys_meta_version_docs;
DROP TABLE IF EXISTS sys_meta_versions;
DROP TABLE IF EXISTS sys_meta_drafts;
