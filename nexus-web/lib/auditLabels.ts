// auditLabels — shared friendly labels for the audit trail's synthetic meta rows.
// The engine writes governance/metadata changes with reserved `entityName` keys
// (internal/audit/audit.go). One map here, reused by AuditLog viewer + StudioHome feed,
// so raw tokens like `__entity__` can never leak into the UI again (DP-1.1).
// Keep in sync with backend reserved keys (FE-mirror-BE).

export const META_ENTITY_NOUNS: Record<string, string> = {
  __role__: "vai trò",
  __entity__: "đối tượng",
  __field__: "trường dữ liệu",
  __field_acl__: "phân quyền field",
  __team__: "nhóm",
  __user__: "tài khoản",
  __sso_provider__: "cấu hình SSO",
  __api_key__: "khoá API",
  __named_credential__: "khoá kết nối",
  __doctor__: "sửa lỗi hệ thống (Doctor)",
  __tenant__: "workspace",
  // specs/068: per-customer backup / restore — written in the customer's own trail, so the noun is
  // what the customer's admin reads (S13: no "tenant"/"restore").
  __tenant_backup__: "sao lưu & khôi phục",
  // specs/062 L5: the builder changed the project registry (industry, "Chạy thử", a health check) — a row of the WORKSHOP's trail.
  __project__: "sổ dự án",
  __job__: "việc nền (gửi lại)",
  // specs/066 L2: a customer admin resent a failed rule email from Nhật ký email.
  __email__: "email (gửi lại)",
  __app__: "app",
  __workflow__: "workflow",
  // specs/062 L4 (FR-023): the builder's edits to rules, reports, dashboards, branding, apps, and the two
  // doors that carry money/data in - inbound webhooks and the bank account the payment QR pays into.
  __workflow_rule__: "quy tắc tự động",
  __report__: "báo cáo",
  __dashboard__: "dashboard",
  __branding__: "thương hiệu",
  __inbound_hook__: "Webhook vào",
  __payment_config__: "Tài khoản nhận tiền",
  // specs/062 L3: a builder editing a screen layout, and opening / closing a workspace session.
  __layout__: "bố cục",
  __workspace_session__: "phiên dựng",
  // specs/064: the builder opened / closed a "xem như vai" session (create / delete of internal/audit PreviewMeta).
  __preview__: "xem như vai",
  // specs/064 S9-4/S9-7: sample data seeded (create) / wiped (delete) in one go (internal/audit SampleDataMeta).
  __sample_data__: "dữ liệu mẫu",
};

// Action word of a meta row whose create/delete mean something other than "Tạo"/"Xoá", keyed by
// (reserved key, action). The viewer's ACTIONS list (the server's `action` filter) is left alone.
const META_ACTION_VERBS: Record<string, Record<string, string>> = {
  __preview__: { create: "Bắt đầu xem như vai", delete: "Kết thúc xem như vai" },
  __sample_data__: { create: "Sinh dữ liệu mẫu", delete: "Xoá dữ liệu mẫu" },
};

export function metaVerb(name: string, action: string): string | undefined {
  return META_ACTION_VERBS[name]?.[action];
}

// Synthetic ACTOR ids (not entity keys). The workflow engine stamps SystemActorID
// (internal/workflow/actions.go) on every record write a rule makes — field_update, assign,
// set_deadline — so the History tab can answer "who changed this" with the rule, not a raw uuid.
export const ENGINE_ACTORS: Record<string, string> = {
  "f1000000-0000-0000-0000-000000000000": "Quy tắc tự động",
  // internal/inboundhook SystemActorID - a webhook whose creator is not a user of the customer's own
  // workspace (a builder) runs as this id, so the records it writes read "Webhook vào" (specs/062 D-22).
  "f2000000-0000-0000-0000-000000000000": "Webhook vào",
  // internal/tenantbackup SystemActorID — every backup the schedule takes (specs/068, FR-061).
  "f3000000-0000-0000-0000-000000000000": "Tự sao lưu theo lịch",
};

export function isMetaEntity(name: string | null | undefined): boolean {
  return !!name && name.startsWith("__") && name.endsWith("__");
}

// Feed-style noun for an audit row's entityName: meta key → friendly noun;
// unknown meta key → "cấu hình hệ thống" (never the raw token).
export function metaNoun(name: string): string {
  return META_ENTITY_NOUNS[name] ?? "cấu hình hệ thống";
}

// AuditLog viewer style: "Hệ thống · vai trò"
export function systemActorLabel(name: string): string {
  return `Hệ thống · ${metaNoun(name)}`;
}
