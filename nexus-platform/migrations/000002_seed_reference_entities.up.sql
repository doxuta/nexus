-- Seed reference entities across THREE verticals for the demo tenant, to prove the
-- metadata read path (one engine → many products). These are sample rows, not core code.

SET @t = '00000000-0000-0000-0000-000000000001';

-- Idempotent seed (migrations must be idempotent): re-apply via
-- `migrate force <prev> && migrate up` or partial-failure recovery must NOT violate the
-- uq_sys_entities_tenant_name unique key (sys_entities) or the sys_fields PK. Mirrors the
-- ON DUPLICATE KEY pattern already used by 000003_create_auth_a1 for the demo tenant/admin.
INSERT INTO sys_entities (id, tenant_id, name, label, table_name, description) VALUES
 ('11111111-1111-1111-1111-111111111101', @t, 'accounts', 'Khách hàng (CRM)',            'obj_accounts', 'Vertical CRM — doanh nghiệp/khách hàng'),
 ('11111111-1111-1111-1111-111111111102', @t, 'students', 'Học viên (LMS)',              'obj_students', 'Vertical LMS — học viên'),
 ('11111111-1111-1111-1111-111111111103', @t, 'tickets',  'Yêu cầu hỗ trợ (Ticketing)',  'obj_tickets',  'Vertical Ticketing — phiếu hỗ trợ')
 ON DUPLICATE KEY UPDATE name = name;

INSERT INTO sys_fields (id, entity_id, name, label, field_type, is_required, is_unique) VALUES
 ('22222222-2222-2222-2222-222222222201', '11111111-1111-1111-1111-111111111102', 'student_code', 'Mã học viên', 'text',     1, 1),
 ('22222222-2222-2222-2222-222222222202', '11111111-1111-1111-1111-111111111102', 'full_name',    'Họ tên',      'text',     1, 0),
 ('22222222-2222-2222-2222-222222222203', '11111111-1111-1111-1111-111111111103', 'subject',      'Tiêu đề',     'text',     1, 0),
 ('22222222-2222-2222-2222-222222222204', '11111111-1111-1111-1111-111111111103', 'priority',     'Mức ưu tiên', 'picklist', 0, 0)
 ON DUPLICATE KEY UPDATE id = id;
