-- Reverse 000073: drop the manual record shares. Every grantee loses the rows they could see only
-- through a share (fail closed — nobody gains access). A binary that still reads the table fails
-- (500) on owner/team-level reads, so roll the binary back together with the schema. No FKs reference
-- it. Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_record_shares;
