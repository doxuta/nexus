-- 000062 — valueLabels tiếng Việt cho picklist của pack đã cài (MỘT XƯỞNG B-PICKLIST-VN).
--
-- Install pack là idempotent (FIELD_ALREADY_EXISTS = skip, options KHÔNG reconcile) nên tenant đã
-- cài không bao giờ tự nhận nhãn mới từ builtin.go — phải backfill một lần ở đây.
--
-- Hình dạng cố ý:
--   • JSON_SET đúng MỘT khoá '$.valueLabels' — KHÔNG ghi trọn options, vì `indexed`/`values` sống
--     cùng blob và ghi trọn là xoá chúng.
--   • Idempotent: chạy lại chỉ set lại cùng giá trị.
--   • KHÔNG đè bàn tay người: field nào ĐÃ có valueLabels (admin tự đặt) thì giữ nguyên.
--   • WHERE join theo TÊN entity+field, cố ý cross-tenant: mọi tenant cài pack đều nhận nhãn.
--   • Mã trong `values` không đổi một ký tự — record/queue/report/workflow so mã vẫn nguyên.

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Technology','Công nghệ','Retail','Bán lẻ','Healthcare','Y tế','Education','Giáo dục',
  'Finance','Tài chính','Manufacturing','Sản xuất','Other','Khác'))
WHERE e.name = 'crm_accounts' AND f.name = 'industry' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Prospecting','Tìm kiếm','Qualification','Đánh giá','Proposal','Báo giá',
  'Negotiation','Đàm phán','Closed Won','Thắng','Closed Lost','Thua'))
WHERE e.name = 'crm_opportunities' AND f.name = 'stage' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'New','Mới','Working','Đang xử lý','Qualified','Đạt chuẩn','Lost','Mất'))
WHERE e.name = 'crm_leads' AND f.name = 'status' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Web','Website','Referral','Giới thiệu','Cold Call','Gọi chào','Event','Sự kiện','Other','Khác'))
WHERE e.name = 'crm_leads' AND f.name = 'source' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Call','Cuộc gọi','Meeting','Cuộc họp','Email','Email','Task','Việc cần làm'))
WHERE e.name = 'crm_activities' AND f.name = 'type' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Planned','Đã lên lịch','Completed','Hoàn thành','Cancelled','Đã huỷ'))
WHERE e.name = 'crm_activities' AND f.name = 'status' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Open','Đang mở','Pending','Chờ xử lý','Resolved','Đã xử lý','Closed','Đã đóng'))
WHERE e.name = 'tk_tickets' AND f.name = 'status' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Low','Thấp','Medium','Trung bình','High','Cao','Urgent','Khẩn'))
WHERE e.name = 'tk_tickets' AND f.name = 'priority' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT(
  'Email','Email','Phone','Điện thoại','Web','Website','Chat','Chat'))
WHERE e.name = 'tk_tickets' AND f.name = 'channel' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;

UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_SET(f.options, '$.valueLabels', JSON_OBJECT('To Do','Cần làm'))
WHERE e.name = 'pm_tasks' AND f.name = 'status' AND f.field_type = 'picklist'
  AND JSON_EXTRACT(f.options, '$.valueLabels') IS NULL;
