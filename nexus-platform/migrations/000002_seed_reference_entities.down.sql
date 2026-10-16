-- Precise inverse of the up seed: remove ONLY the rows 000002 inserted, by primary key —
-- NOT `WHERE tenant_id = @t`, which would also wipe any entities/fields a user created in the
-- demo tenant (data loss on a routine `migrate down`). Fields first (they reference entities).
DELETE FROM sys_fields WHERE id IN (
  '22222222-2222-2222-2222-222222222201',
  '22222222-2222-2222-2222-222222222202',
  '22222222-2222-2222-2222-222222222203',
  '22222222-2222-2222-2222-222222222204'
);
DELETE FROM sys_entities WHERE id IN (
  '11111111-1111-1111-1111-111111111101',
  '11111111-1111-1111-1111-111111111102',
  '11111111-1111-1111-1111-111111111103'
);
