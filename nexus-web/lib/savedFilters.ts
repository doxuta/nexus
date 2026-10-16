// lib/savedFilters.ts — the FIXED filters saved on a report (`config.filters`) or a dashboard widget
// (`widget.filters`) ↔ the expression builder's rows (components/slds/ExpressionBuilder, the list's
// FilterRow contract in lib/filterModel), specs/070 #266.
//
// Wire shape = report_service.go ReportFilterInput {field, op, values[]}, the SAME dialect as the
// list's ?field[op]=value — so the split mirrors dynamic_record_handler.go exactly: in/between are
// comma-split with blank members dropped, isnull/notnull carry no value, every other operator one
// value. Relative dates ("today", "this_month", …) travel as their token and are resolved by the
// server at run time (relative_dates.go). PURE: no React, no fetch.
import type { DashboardWidgetFilter } from "./api";
import { isActiveFilter, isMultiValueOp, isNullOp, nextFilterId, type FilterRow } from "./filterModel";
import { t } from "./i18n";

export type SavedFilter = DashboardWidgetFilter;

/** Saved filters → builder rows (unknown shapes are skipped; a missing op is the eq default). */
export function filterRowsFromSaved(saved: unknown): FilterRow[] {
  if (!Array.isArray(saved)) return [];
  const rows: FilterRow[] = [];
  for (const f of saved) {
    if (!f || typeof f !== "object" || typeof (f as SavedFilter).field !== "string") continue;
    const { field, op, values } = f as SavedFilter;
    const vals = Array.isArray(values) ? values.map(String) : [];
    rows.push({ id: nextFilterId(), field, op: op || "eq", value: isMultiValueOp(op ?? "") ? vals.join(",") : (vals[0] ?? "") });
  }
  return rows;
}

/** Builder rows → saved filters. Only ACTIVE rows (field + op + a value, or a null operator) are
    kept — callers block the save on a half-typed row first (filterRowIssues), so nothing the admin
    typed is dropped silently. */
export function savedFromFilterRows(rows: readonly FilterRow[], original?: unknown): SavedFilter[] {
  // A row the user did not touch keeps its saved filter verbatim: re-splitting "Công ty A, B" on the
  // comma would change its meaning (#451). Matched by the row's round-tripped (field, op, value).
  const orig = Array.isArray(original) ? (original as SavedFilter[]) : [];
  const pool = orig
    .filter((f) => f && typeof f === "object" && typeof f.field === "string")
    .map((f) => ({ r: filterRowsFromSaved([f])[0], f, used: false }));
  return rows.filter(isActiveFilter).map(({ field, op, value }) => {
    const hit = pool.find((p) => !p.used && p.r.field === field && p.r.op === op && p.r.value === value);
    if (hit) { hit.used = true; return hit.f; }
    if (isNullOp(op)) return { field, op };
    const values = isMultiValueOp(op) ? value.split(",").map((s) => s.trim()).filter(Boolean) : [value.trim()];
    return { field, op, values };
  });
}

/** Per-row errors (keyed by row id, the ExpressionBuilder `errors` prop) for a condition that was
    started but cannot run: no value, or a range with one end. A row with no field yet is inert. */
export function filterRowIssues(rows: readonly FilterRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    if (!r.field || !r.op || isNullOp(r.op)) continue;
    if (!r.value.trim()) out[r.id] = t("page.reports.config.filterValueMissing");
    else if (isMultiValueOp(r.op)) {
      const n = r.value.split(",").filter((s) => s.trim()).length;
      if (r.op === "in" && n === 0) out[r.id] = t("page.reports.config.filterValueMissing");
      else if (r.op === "between" && n !== 2) out[r.id] = t("page.reports.config.filterRangeMissing");
    }
  }
  return out;
}
