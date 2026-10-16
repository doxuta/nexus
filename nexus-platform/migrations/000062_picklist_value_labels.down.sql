-- Đảo 000062: gỡ đúng khoá '$.valueLabels' khỏi các field pack đã backfill. KHÔNG restore snapshot
-- (không có gì khác bị sửa), và cố ý gỡ CẢ nhãn admin tự đặt trên đúng các field này sau backfill —
-- down migration không phân biệt được hai nguồn, chấp nhận và ghi rõ.
UPDATE sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
SET f.options = JSON_REMOVE(f.options, '$.valueLabels')
WHERE f.field_type = 'picklist'
  AND ((e.name = 'crm_accounts'      AND f.name = 'industry')
    OR (e.name = 'crm_opportunities' AND f.name = 'stage')
    OR (e.name = 'crm_leads'         AND f.name IN ('status', 'source'))
    OR (e.name = 'crm_activities'    AND f.name IN ('type', 'status'))
    OR (e.name = 'tk_tickets'        AND f.name IN ('status', 'priority', 'channel'))
    OR (e.name = 'pm_tasks'          AND f.name = 'status'));
