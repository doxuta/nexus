// lib/reportConfig.ts — pure builder for the report `config` payload (report authoring —
// the /reports/[id] admin ReportConfigPanel + the /reports create dialog).
//
// Mirrors backend validateReportConfig (report_service.go):
//   - tabular: config.columns (≥1). config.bucket is REJECTED on tabular
//     (422 REPORT_CONFIG_INVALID "config.bucket chỉ dùng cho report summary/matrix") — never
//     emitted here.
//   - summary/matrix: config.{metric, field?, groupBy?}. config.bucket
//     (day|week|month|quarter|year) is written ONLY when groupBy can carry it
//     (created_at/updated_at or a date/datetime field — record_aggregate.go
//     isBucketableGroupBy). The date-type check re-runs at RUN time
//     (AGGREGATE_BUCKET_FIELD_NOT_DATE), so a stale bucket left after the admin re-pointed
//     groupBy at a non-date field is dropped here instead of saved-then-failing.
//
// specs/049 F0: PUT /admin/reports replaces the WHOLE config, so keys the panel has no editor for
// (the grid keys subGroups/columnGroup/having) ride along from `saved` — per report type, never
// across an entity change, never with an axis namespaced onto a relationship that is no longer
// joined. `joins` stays withReportJoin's (lib/reportJoin). specs/070 #266: `filters` and the
// tabular `sort` are EDITED by the panel now — passed in, they replace the saved value; left
// undefined (the create dialog), they still ride along.
//
// The viewer (app/(app)/reports/[id]) reads the SAME shape: reportMeta.config.bucket.
// PURE: no React, no fetch.

import type { ReportRunData } from "./api";
import { t, tf } from "./i18n";
import type { I18nKey } from "./i18n/vi";
import { stripStaleJoinToken } from "./reportJoin";
import { isBucketId, isBucketableGroupBy } from "./timeBuckets";

export type ReportType = "tabular" | "summary" | "matrix";

export interface ReportConfigInput {
  type: ReportType;
  columns: string[];
  metric: string;
  field?: string;
  groupBy?: string;
  bucket?: string;
  /** Entity fields (name+fieldType) — decide whether groupBy can carry the time bucket. */
  fields: ReadonlyArray<{ name: string; fieldType: string }>;
  /** Fallback column when the tabular column list is empty (builder legacy: fields[0] ?? "id"). */
  fallbackColumn?: string;
  /** Saved config the editor opened on (ReportConfigPanel). Absent in the create dialog. */
  saved?: Record<string, unknown>;
  /** Entity `saved` belongs to; when it differs from entityName nothing is carried. */
  savedEntity?: string;
  /** Entity the editor now points at. */
  entityName?: string;
  /** Relationship joined right now ("" = none) — carried axes namespaced elsewhere are dropped. */
  joinRel?: string;
  /** Every relationship joined right now (a 2-hop chain); wins over joinRel when given. */
  joinRels?: readonly string[];
  /** Fixed filters from the editor (specs/070 #266). Given ⇒ they REPLACE the saved ones (empty ⇒
      no key); undefined ⇒ the saved ones ride along (F0 passthrough). */
  filters?: ReadonlyArray<{ field: string; op?: string; values?: string[] }>;
  /** Tabular sort from the editor: given (null = default order) ⇒ replaces the saved one;
      undefined ⇒ passthrough. Never written on summary/matrix (only the List path reads it). */
  sort?: { field: string; dir: "asc" | "desc" } | null;
  /** specs/049 F3 — grid keys edited by the panel. Given (null/[] = none) ⇒ replace the saved one;
      undefined ⇒ the saved one rides along (F0 passthrough, e.g. the create dialog). */
  columnGroup?: GridAxis | null;
  subGroups?: ReadonlyArray<GridAxis>;
  having?: GridHaving | null;
}

export interface GridAxis { field: string; bucket?: string }
export interface GridHaving { on: "value" | "count"; op: string; values: string[] }

// Saved keys this builder does not manage, per type (plan §7.2). A key not listed for a type is
// dropped on a type change (matrix→summary loses columnGroup; →tabular loses having).
const CARRIED: Record<ReportType, readonly string[]> = {
  tabular: ["filters", "sort"],
  summary: ["filters", "subGroups", "having"],
  matrix: ["filters", "columnGroup", "having"],
};

const axisField = (a: unknown) =>
  a && typeof a === "object" ? String((a as { field?: unknown }).field ?? "") : "";

function carried(inp: ReportConfigInput): Record<string, unknown> {
  const s = inp.saved;
  if (!s || (inp.entityName ?? "") !== (inp.savedEntity ?? "")) return {};
  const rel = inp.joinRels ?? [inp.joinRel ?? ""];
  const out: Record<string, unknown> = {};
  for (const k of CARRIED[inp.type]) {
    if (k === "filters" && inp.filters !== undefined) continue; // the editor owns it (managed)
    if (k === "sort" && inp.sort !== undefined) continue;
    if (k === "columnGroup" && inp.columnGroup !== undefined) continue;
    if (k === "subGroups" && inp.subGroups !== undefined) continue;
    if (k === "having" && inp.having !== undefined) continue;
    if (s[k] != null) out[k] = s[k];
  }
  // An axis on a relationship that is not joined any more would 422 at every run.
  if (Array.isArray(out.subGroups)) {
    const kept = out.subGroups.filter((g) => stripStaleJoinToken(axisField(g), rel) !== "");
    if (kept.length) out.subGroups = kept;
    else delete out.subGroups;
  }
  if (out.columnGroup !== undefined && stripStaleJoinToken(axisField(out.columnGroup), rel) === "") {
    delete out.columnGroup;
  }
  return out;
}

// Keys the editor manages (specs/070 #266): written only when the caller passed them.
function managed(inp: ReportConfigInput): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (inp.filters?.length) out.filters = inp.filters;
  if (inp.type === "tabular" && inp.sort) out.sort = inp.sort;
  if (inp.type === "tabular") return out;
  const axis = (a: GridAxis, f: ReportConfigInput["fields"]) =>
    isBucketId(a.bucket) && isBucketableGroupBy(a.field, f) ? { field: a.field, bucket: a.bucket } : { field: a.field };
  const key = (a: GridAxis) => `${a.field}|${isBucketId(a.bucket) ? a.bucket : ""}`;
  const seen = new Set<string>([key({ field: inp.groupBy ?? "", bucket: inp.bucket })]);
  if (inp.type === "matrix" && inp.columnGroup?.field && !seen.has(key(inp.columnGroup))) {
    out.columnGroup = axis(inp.columnGroup, inp.fields);
  }
  if (inp.type === "summary" && inp.groupBy && inp.subGroups) {
    const kept: Array<{ field: string; bucket?: string }> = [];
    for (const g of inp.subGroups) {
      if (!g.field || seen.has(key(g)) || kept.length >= 2) continue;
      seen.add(key(g));
      kept.push(axis(g, inp.fields));
    }
    if (kept.length) out.subGroups = kept;
  }
  const h = inp.having;
  if (h && h.values.length >= (h.op === "between" ? 2 : 1) && h.values.every((v) => v.trim() !== "")) {
    out.having = { on: h.on, op: h.op, values: h.op === "between" ? h.values.slice(0, 2) : h.values.slice(0, 1) };
  }
  return out;
}

export function buildReportConfig(inp: ReportConfigInput): Record<string, unknown> {
  if (inp.type === "tabular") {
    // Tabular has no GROUP BY — the backend rejects a bucket here, so none is ever written.
    return { columns: inp.columns.length ? inp.columns : [inp.fallbackColumn ?? "id"], ...managed(inp), ...carried(inp) };
  }
  const cfg: Record<string, unknown> = {
    metric: inp.metric,
    field: inp.field || undefined,
    groupBy: inp.groupBy || undefined,
  };
  if (isBucketId(inp.bucket) && isBucketableGroupBy(inp.groupBy, inp.fields)) {
    cfg.bucket = inp.bucket;
  }
  return { ...cfg, ...managed(inp), ...carried(inp) };
}

/** The config leaves the one-axis path (specs/049): the dashboard widget cannot hold it. */
export function isGridConfig(cfg: Record<string, unknown> | undefined | null): boolean {
  if (!cfg) return false;
  return cfg.columnGroup != null
    || (Array.isArray(cfg.subGroups) && cfg.subGroups.length > 0)
    || cfg.having != null;
}

/** "grid" = matrix with a column axis, "tree" = levelled groups, "flat" = the one-axis shape. */
export function reportResultShape(data: ReportRunData): "grid" | "tree" | "flat" {
  if (data.type === "tabular") return "flat";
  if (data.columnAxis || data.cells) return "grid";
  if (data.levels?.length) return "tree";
  return "flat";
}

// Exact vi-VN grouping of a decimal string ("1500.5" → "1.500,5") — no float round-trip, the
// threshold is shown as saved.
function viNumber(s: string): string {
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(s.trim());
  if (!m) return s;
  return `${m[1]}${m[2].replace(/\B(?=(\d{3})+(?!\d))/g, ".")}${m[3] ? `,${m[3]}` : ""}`;
}

const HAVING_OPS = new Set(["gt", "gte", "lt", "lte", "eq", "neq", "between"]);

/**
 * Notes the viewer shows above a summary/matrix result it does not fully draw yet (FR-025) and the
 * threshold in force (FR-012: it filters the LEAF groups — cells of a grid, the deepest level of a
 * tree). `fieldLabel` names an axis; an axis it cannot name is never shown as its token.
 */
export function reportShapeNote(
  data: ReportRunData,
  reportType: string | undefined,
  isAdmin: boolean,
  fieldLabel: (name: string) => string | undefined = () => undefined,
): string[] {
  if (data.type === "tabular") return [];
  const shape = reportResultShape(data);
  const notes: string[] = [];
  // Staff: nothing is wrong from where they sit; the admin can act on it.
  if (reportType === "matrix" && shape !== "grid" && isAdmin) notes.push(t("page.reports.viewer.note.matrixNoColumn"));

  const h = data.having;
  if (h && Array.isArray(h.values)) {
    const [a = "", b = ""] = h.values.map(viNumber);
    const cond = !HAVING_OPS.has(h.op)
      ? `${h.op} ${a}`
      : h.op === "between"
        ? tf("page.reports.viewer.having.op.between", { lo: a, hi: b })
        : tf(`page.reports.viewer.having.op.${h.op}` as I18nKey, { v: a });
    const on = t(h.on === "count" ? "page.reports.viewer.having.on.count" : "page.reports.viewer.having.on.value");
    if (shape === "grid") {
      notes.push(tf("page.reports.viewer.note.havingCell", { on, cond }));
    } else {
      const deepest = data.levels?.[data.levels.length - 1]?.field;
      const level = deepest ? fieldLabel(deepest) : undefined;
      notes.push(level
        ? tf("page.reports.viewer.note.havingGroup", { level, on, cond })
        : tf("page.reports.viewer.note.havingDeepest", { on, cond }));
    }
  }
  return notes;
}
