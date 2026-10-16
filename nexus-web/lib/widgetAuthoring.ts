// lib/widgetAuthoring.ts — pure helpers for dashboard widget authoring: the admin panel folded
// into the app-face viewer /dashboards/[id] and the report "pin to dashboard" path.
//
// WIDGET_TYPES lists ONLY types the backend accepts (dashboard_service.go validWidgetTypes:
// kpi|bar|line|table|funnel — anything else is 422 WIDGET_TYPE_INVALID). Every option saves
// EXACTLY as labeled: the former "Tròn (Pie) ⚡ sắp ra mắt" entry silently persisted as "bar"
// and was REMOVED — no option may lie about what it stores. Pie returns only if/when the
// backend accepts a "pie" token.
//
// PURE: no React, no fetch.

import type { DashboardWidgetDef } from "./api";
import { readsReadTimeField, supportsIndexed } from "./fieldOptions";
import type { FilterRow } from "./filterModel";
import { t } from "./i18n";
import type { FieldType } from "./metadata";
import { savedFromFilterRows } from "./savedFilters";
import { isBucketId, isBucketableGroupBy } from "./timeBuckets";

export type WidgetType = "kpi" | "bar" | "line" | "table" | "funnel";

export const WIDGET_TYPES: ReadonlyArray<{ key: WidgetType; label: string }> = [
  { key: "kpi", label: "Số (KPI)" },
  { key: "bar", label: "Biểu đồ cột" },
  { key: "line", label: "Biểu đồ đường" },
  { key: "table", label: "Bảng" },
  { key: "funnel", label: "Phễu (Funnel)" },
];

/** Aggregate metrics the run engine accepts (record_aggregate.go). */
export const WIDGET_METRICS: readonly string[] = ["count", "sum", "avg", "min", "max"];

// Field types that cannot back a groupBy picker: rich_text/multi_select have no scalar column, a
// polymorphic_relation only its <f>_type/<f>_id sub-columns. relation/user DO group (their
// <field>_id column is indexed — keys are ids until the axis-label slice, specs/049 S6 / #194).
const NON_PICKABLE_FIELD_TYPES = ["polymorphic_relation", "rich_text", "multi_select"];

// Types a TABULAR report column cannot fill (REPORT-RELATION-LABELS): a relation / user column
// IS fine here — the run and the CSV resolve it to the target's label (api-conventions §4.4d) —
// so only the shapes with no readable cell text stay out of the column picker.
const NON_TABULAR_COLUMN_TYPES = ["polymorphic_relation", "rich_text", "multi_select"];

/**
 * Strip an incoherent time bucket before save. The backend requires bucket→groupBy at config
 * time (422 WIDGET_BUCKET_REQUIRES_GROUPBY) and a DATE-kind groupBy at run time
 * (AGGREGATE_BUCKET_FIELD_NOT_DATE) — a stale bucket left behind after the admin re-pointed
 * groupBy must not be persisted. Callers pass the entity's fields so a date/datetime groupBy
 * keeps its bucket; with no fields only created_at/updated_at qualify.
 */
export function normalizeWidgetBucket<T extends { groupBy?: string; bucket?: string }>(
  w: T,
  fields: ReadonlyArray<{ name: string; fieldType: string }> = [],
): T {
  if (isBucketId(w.bucket) && isBucketableGroupBy(w.groupBy, fields)) return w;
  if (!("bucket" in w)) return w;
  const { bucket: _dropped, ...rest } = w;
  return rest as unknown as T;
}

// ── Widget draft (the authoring form's state) ────────────────────────────────

/** Only the form-modeled fields — NEVER x/y/w/h, so a stale draft can not clobber a fresher grid
    layout when merged back onto the stored widget (applyWidgetDraft). `filters` are the fixed
    filters as expression-builder rows (specs/070 #266); absent ⇒ the stored ones are kept. */
export interface WidgetDraft {
  id: string;
  title: string;
  type: string;
  entity: string;
  metric: string;
  field?: string;
  groupBy?: string;
  bucket?: string;
  filters?: FilterRow[];
}

let _widgetSeq = 0;
/** Client-generated widget id — unique within a dashboard, stored verbatim by the backend. */
export function newWidgetId(): string {
  return `w${++_widgetSeq}_${Date.now()}`;
}

/** A fresh KPI count draft over `entity` — the same default the old Studio builder seeded. */
export function blankWidget(entity: string): WidgetDraft {
  return { id: newWidgetId(), title: "Ô số liệu mới", type: "kpi", entity, metric: "count" };
}

/**
 * Overlay the authored (form-modeled) fields onto the STORED widget, preserving every key the
 * form does not model — x/y/w/h grid rects, and anything future slices add — per the
 * full-replace-PUT round-trip contract (lib/dashWidgetPayload). Cleared optionals
 * (field/groupBy/bucket) are REMOVED, not persisted as "" tokens; the bucket is then
 * re-normalized against the entity's fields. Fixed filters are written from the draft's builder
 * rows when it carries them (none left ⇒ key removed); a draft without them keeps the stored ones.
 */
export function applyWidgetDraft(
  stored: DashboardWidgetDef | undefined,
  draft: WidgetDraft,
  fields: ReadonlyArray<{ name: string; fieldType: string }> = [],
): DashboardWidgetDef {
  const out: DashboardWidgetDef = {
    ...(stored ?? ({} as Partial<DashboardWidgetDef>)),
    id: draft.id,
    title: draft.title.trim() || "Ô số liệu mới",
    type: draft.type,
    entity: draft.entity,
    metric: draft.metric,
  };
  for (const k of ["field", "groupBy", "bucket"] as const) {
    const v = draft[k];
    if (v) out[k] = v;
    else delete out[k];
  }
  // A joined widget's relationships belong to its entity — re-pointing the card drops them.
  if (stored && stored.entity !== draft.entity) delete out.joins;
  // Joins only exist to serve a "<rel>." token; once neither axis uses one, the leftover INNER JOINs
  // would silently drop base rows with no link (the card number drifts from the base count).
  if (out.joins?.length) {
    const used = out.joins.some(
      (j) => (out.groupBy ?? "").startsWith(`${j.relationship}.`) || (out.field ?? "").startsWith(`${j.relationship}.`),
    );
    if (!used) delete out.joins;
  }
  if (draft.filters) {
    const saved = savedFromFilterRows(draft.filters, stored?.filters);
    if (saved.length) out.filters = saved;
    else delete out.filters;
  }
  return normalizeWidgetBucket(out, fields);
}

/** Replace-by-id (widget edit) or append (new widget) the merged draft into the widget array. */
export function upsertWidgetDraft(
  widgets: readonly DashboardWidgetDef[],
  draft: WidgetDraft,
  fields: ReadonlyArray<{ name: string; fieldType: string }> = [],
): DashboardWidgetDef[] {
  const idx = widgets.findIndex((w) => w.id === draft.id);
  const merged = applyWidgetDraft(idx < 0 ? undefined : widgets[idx], draft, fields);
  return idx < 0 ? [...widgets, merged] : widgets.map((w, i) => (i === idx ? merged : w));
}

// ── groupBy choices for the widget form <select> ─────────────────────────────

export interface GroupByChoice {
  value: string;
  label: string;
  /** false → the BE rejects this axis (AGGREGATE_REQUIRES_INDEXED / SORT_NOT_ALLOWED) until the
      field gets a physical index. */
  indexed: boolean;
  /** Not indexed yet but CAN be, in place ("Bật index ngay" — IndexFixNotice). A choice that is
      neither indexed nor fixable renders disabled: a guaranteed run-time dead-end. */
  fixable?: boolean;
}

type AxisField = {
  name: string;
  label?: string;
  fieldType: string;
  isUnique?: boolean;
  options?: Record<string, unknown>;
};

/** The field already resolves to an indexed physical column — the mirror of the engine's
    buildSortAllowlist (record_orchestration.go): relation/user as <field>_id, unique, D1 indexed. */
export function isIndexedAxisField(f: AxisField): boolean {
  return f.fieldType === "relation" || f.fieldType === "user" || !!f.isUnique || f.options?.indexed === true;
}

/** Unindexed, but `PUT /admin/fields/:id {options.indexed:true}` would index it (supportsIndexed). */
export function isIndexFixable(f: AxisField): boolean {
  return !isIndexedAxisField(f) && supportsIndexed(f.fieldType as FieldType, !!f.isUnique);
}

/**
 * Options for a group-by (and tabular sort) picker = the engine's allowlist: the system
 * timestamps and the record owner ("Người phụ trách" — ownerId → owner_id, idx_owner_id), every
 * relation / user field (their <field>_id column is always indexed — specs/070 #265), and the
 * entity's other scalar fields, gated by the physical-index rule (unique / options.indexed); an
 * unindexed field that can take an index is `fixable` in place. A stored value missing from the
 * list (legacy "id", deleted field…) is appended selectable so editing never silently drops it —
 * same keep-current contract as scopeEntityOptions.
 */
export function groupByChoices(fields: ReadonlyArray<AxisField>, current?: string): GroupByChoice[] {
  const out: GroupByChoice[] = [
    { value: "created_at", label: "Ngày tạo (created_at)", indexed: true },
    { value: "updated_at", label: "Ngày cập nhật (updated_at)", indexed: true },
  ];
  // A declared field of the same name wins (the engine seeds the reserved key before the spec loop).
  if (!fields.some((f) => f.name === "ownerId")) {
    out.push({ value: "ownerId", label: t("page.reports.config.groupByOwner"), indexed: true });
  }
  for (const f of fields) {
    if (NON_PICKABLE_FIELD_TYPES.includes(f.fieldType)) continue;
    out.push({ value: f.name, label: f.label ?? f.name, indexed: isIndexedAxisField(f), fixable: isIndexFixable(f) });
  }
  if (current && !out.some((c) => c.value === current)) {
    out.push({ value: current, label: current, indexed: true, fixable: false });
  }
  return out;
}

/**
 * A field SUM/AVG/MIN/MAX can run on — the mirror of the engine's resolveMetricField
 * (record_aggregate.go aggregateStorageType, specs/070 #264): number/currency/rating, and a formula
 * whose result is STORED as a number (resultType number or unset) or a currency. A text formula,
 * a roll-up / lookup (computed at read time) and every non-numeric type are refused with
 * AGGREGATE_FIELD_NOT_NUMERIC, so they are never offered.
 */
export function isAggregatableField(
  f: { fieldType: string; options?: Record<string, unknown> },
  siblings?: ReadonlyArray<{ name: string; fieldType: string }>,
): boolean {
  if (f.fieldType === "formula") {
    if (readsReadTimeField(f, siblings)) return false; // specs/080: computed at view, nothing stored to SUM
    const rt = f.options?.resultType;
    return rt === undefined || rt === "" || rt === "number" || rt === "currency";
  }
  // specs/083: a roll-up that keeps its total in the record sums like the number/money it holds.
  if (f.fieldType === "rollup") return f.options?.stored === true;
  return f.fieldType === "number" || f.fieldType === "currency" || f.fieldType === "rating";
}

/** Options for the value-field picker (metric target): only fields the engine can sum
    (isAggregatableField). "" = count over all records. Keeps the stored out-of-list value
    selectable, like groupByChoices. */
export function valueFieldChoices(
  fields: ReadonlyArray<{ name: string; label?: string; fieldType: string; options?: Record<string, unknown> }>,
  current?: string,
): Array<{ value: string; label: string }> {
  const out = fields.filter((f) => isAggregatableField(f, fields)).map((f) => ({ value: f.name, label: f.label ?? f.name }));
  if (current && !out.some((c) => c.value === current)) out.push({ value: current, label: current });
  return out;
}

/**
 * Column choices for a TABULAR report (ReportConfigPanel "Cột hiển thị"): every field a cell can
 * show, relation and user fields included — the report run resolves them to labels, so a
 * "Khách hàng" column reads "Công ty ABC", not a UUID. `current` keeps a saved column whose
 * field is gone in the list so the admin can still see and remove it.
 */
export function tabularColumnChoices(
  fields: ReadonlyArray<{ name: string; label?: string; fieldType: string }>,
  current?: string,
): Array<{ value: string; label: string }> {
  return fieldChoices(fields, NON_TABULAR_COLUMN_TYPES, current);
}

function fieldChoices(
  fields: ReadonlyArray<{ name: string; label?: string; fieldType: string }>,
  excludedTypes: readonly string[],
  current?: string,
): Array<{ value: string; label: string }> {
  const out = fields
    .filter((f) => !excludedTypes.includes(f.fieldType))
    .map((f) => ({ value: f.name, label: f.label ?? f.name }));
  if (current && !out.some((c) => c.value === current)) {
    out.push({ value: current, label: current });
  }
  return out;
}
