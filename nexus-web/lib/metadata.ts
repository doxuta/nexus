// Metadata type-system — khớp contract backend (sys_entities / sys_fields / api-conventions.md).
// Đây là nguồn-sự-thật cho Layout Engine: mọi widget/grid/form render theo các type này.

import { t } from "@/lib/i18n";

export type FieldType =
  | "text"
  | "rich_text"
  | "number"
  | "boolean"
  | "date"
  | "datetime"
  | "email"
  | "currency"
  | "picklist"
  | "multi_select"
  | "relation"
  | "polymorphic_relation" // points at a record in ANY of options.targetEntities — value {type,id} (ACTIVITY-CORE #3a)
  | "user" // points at a real ACTIVE user of the tenant — value = userId, ?expand= → {id,label,email}
  | "rating"
  | "composite"
  | "file"   // ⚠️ backend pending (G1) — vẽ UI, gắn nhãn
  | "image"  // ⚠️ backend pending — preview grid + upload dropzone
  | "formula" // always read-only, computed server-side, ƒ badge
  | "lookup" // read-time only, mirrors a field from a to-one related record (ENG-COMPUTED #2)
  | "rollup" // read-time only, SUM/COUNT/AVG/MIN/MAX over child records (ENG-COMPUTED #3)
  | "auto_number"; // server-ASSIGNED sequence at Create, e.g. "INV-00042" (ENG #6)

// options theo type (sys_fields.options JSON). Chỉ field liên quan có mặt.
export interface FieldOptions {
  maxLength?: number; // text/rich_text
  pattern?: string; // text/rich_text (regex)
  min?: number; // number/currency/rating
  max?: number;
  step?: number; // number
  precision?: number; // currency (0..8)
  currencyCode?: string; // currency (VND/USD…)
  resultType?: string; // formula: "text" = show verbatim (specs/053 §3.4)
  values?: string[]; // picklist/multi_select — MÃ lưu trữ, ổn định, KHÔNG bao giờ dịch
  // picklist/multi_select — nhãn hiển thị theo mã (vd {"New":"Mới"}). Additive: pack cũ /
  // field không khai thì mọi bề mặt hiện nguyên mã (trung thực, không bịa). Mã trong `values`
  // là thứ được LƯU và được workflow/report/queue tham chiếu — dịch mã là mồ côi dữ liệu,
  // nên tiếng Việt sống ở tầng nhãn này.
  valueLabels?: Record<string, string>;
  // picklist CON phụ thuộc (specs/052): field cha (một picklist cùng entity) + map giá trị cha → tập
  // giá trị con được phép. Hai khoá đi cặp; form chỉ liệt kê con của cha đang chọn.
  controllingField?: string;
  dependentValues?: Record<string, string[]>;
  relatedEntity?: string; // relation
  targetEntities?: string[]; // polymorphic_relation — allowlist of entities the {type,id} value may point at (ACTIVITY-CORE #3a)
  labelField?: string; // relation (⚠️ pending)
  cardinality?: "1-N" | "1-1"; // relation (ENG-REL2 #5) — vắng mặt = "1-N" ngầm định
  inverseField?: string; // relation (ENG-REL2 #4) — tên field phía entity cha; vắng mặt = engine tự đặt
  subFields?: FieldSpec[]; // composite (chỉ scalar)
  // PII: giá trị KHÔNG BAO GIỜ được chép nguyên văn vào nhật ký thay đổi — mọi mutation ghi
  // `[redacted]` ở cả hai phía diff (internal/services/audit_redact.go). Bật trong Studio
  // bằng công tắc "Trường nhạy cảm"; bản ghi và màn chi tiết vẫn hiện giá trị thật.
  sensitive?: boolean;
  // lookup (ENG-COMPUTED #2) — mirror 1 field từ record liên kết qua 1 quan hệ to-one.
  via?: string; // tên field `relation` (to-one) trên CHÍNH entity này
  sourceField?: string; // tên field trên entity ĐÍCH (mà `via` trỏ tới) cần mirror
  // rollup (ENG-COMPUTED #3) — SUM/COUNT/AVG/MIN/MAX trên record con.
  childEntity?: string; // entity chứa các record con
  metric?: "count" | "sum" | "avg" | "min" | "max";
  aggField?: string; // bắt buộc trừ count — field number/currency/rating trên childEntity
  viaRelation?: string; // chế độ FK-on-child (1-N): tên field `relation` TRÊN childEntity trỏ ngược lại entity này
  relationship?: string; // chế độ N-N: tên quan hệ N-N đã đăng ký — loại trừ với viaRelation
  // auto_number (ENG #6) — sequence per-tenant per-entity per-field, server-ASSIGNED tại Create.
  prefix?: string; // tối đa 20 ký tự, nối trực tiếp trước số
  padding?: number; // 0..12 — đệm số 0 bên trái
}

export interface FieldSpec {
  name: string; // mã field (snake_case)
  label: string; // nhãn hiển thị (VN)
  fieldType: FieldType;
  required?: boolean;
  readonly?: boolean; // layout cell override (readonly:true) — detail-render even in edit mode
  unique?: boolean;
  indexed?: boolean;
  options?: FieldOptions;
  defaultVal?: unknown;
  helpText?: string; // per-VIEW helper (layout cell W5) — rendered under the widget
  description?: string; // per-FIELD admin description (sys_fields.description W5) — "?" tooltip next to the label
}

export interface EntityDef {
  name: string; // "accounts"
  label: string; // "Khách hàng"
  tableName: string; // "obj_accounts"
  badge?: string;
  options?: Record<string, unknown>;
}

// Một bản ghi động: {id, version, data:{field→value}, hệ-cột}.
export interface RecordData {
  id: string;
  version: number;
  data: Record<string, unknown>;
  createdAt?: string;
  updatedAt?: string;
  createdBy?: string;
}

// Đăng ký field-type → metadata hiển thị (badge type trong form/builder, mô tả).
// `pending` = backend chưa hỗ trợ đầy đủ (gắn nhãn "đang phát triển").
// `group: "advanced"` = xếp vào nhóm "Nâng cao / Tính toán" trong dropdown loại dữ liệu — 4 kiểu
// computed/server-managed (formula/lookup/rollup/auto_number), tách khỏi nhóm cơ bản cho gọn.
// The label is read from lib/i18n (fieldType.<type>) each time it is used, so it follows the UI language.
const typeMeta = (type: FieldType, more: { pending?: boolean; group?: "advanced" } = {}) => ({
  ...more,
  get label() {
    return t(`fieldType.${type}`);
  },
});
export const FIELD_TYPE_META: Record<FieldType, { readonly label: string; pending?: boolean; group?: "advanced" }> = {
  text: typeMeta("text"),
  rich_text: typeMeta("rich_text"),
  number: typeMeta("number"),
  boolean: typeMeta("boolean"),
  date: typeMeta("date"),
  datetime: typeMeta("datetime"),
  email: typeMeta("email"),
  currency: typeMeta("currency"),
  picklist: typeMeta("picklist"),
  multi_select: typeMeta("multi_select"),
  relation: typeMeta("relation"),
  polymorphic_relation: typeMeta("polymorphic_relation"),
  user: typeMeta("user"),
  rating: typeMeta("rating"),
  composite: typeMeta("composite"),
  file: typeMeta("file", { pending: true }),
  image: typeMeta("image", { pending: true }),
  formula: typeMeta("formula", { group: "advanced" }),
  lookup: typeMeta("lookup", { group: "advanced" }),
  rollup: typeMeta("rollup", { group: "advanced" }),
  auto_number: typeMeta("auto_number", { group: "advanced" }),
};

/**
 * Field types whose stored value is a bare id that means NOTHING to a human, and which the
 * server can resolve to a label via `?expand=` — the caller must request them or the screen
 * shows a raw UUID.
 *
 * This predicate exists because the list view and the detail view each built that request
 * independently, both hard-coded to `relation`, and both silently missed `user` for the whole
 * life of the `user` field type: backend validated it, backend expanded it, and every screen
 * still printed a UUID (found 2026-07-29 walking the customer face as a sales rep — "Phân công
 * cho: b1551a1a-…"). One predicate, so the next expandable type cannot be half-adopted.
 */
export function isExpandableType(t: FieldType | string | undefined): boolean {
  return t === "relation" || t === "user";
}

/**
 * Display label for one picklist/multi_select CODE — the single resolver every surface calls.
 *
 * The stored contract is `options.values: string[]` where the code doubles as the on-screen text;
 * the packs seeded English codes ("New", "Web", "Urgent") and the customer face read them out
 * verbatim on a Vietnamese UI. Translating the codes themselves would orphan every stored record
 * and break each pack's work queues, report filters and workflow conditions (they all compare
 * against the code by exact string) — so Vietnamese lives in `options.valueLabels` and the code
 * never changes. A code with no label renders as itself: honest, never invented.
 *
 * One function on purpose: the read view, the input widgets, kanban columns and the filter chips
 * each used to derive display text independently — the exact two-surfaces-drift-apart shape that
 * hid the `user` field expansion bug.
 */
export function valueLabel(o: FieldOptions | undefined, code: string): string {
  return o?.valueLabels?.[code] ?? code;
}
