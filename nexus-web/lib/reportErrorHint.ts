// lib/reportErrorHint.ts — the sentence a failed report run or CSV export shows, per error code
// (specs/049 F0, FR-024). The "đánh index" advice appears ONLY for the real index error (a staff
// member hitting FORBIDDEN was once told to go index a field). Staff (non-admin) get a plain
// Vietnamese sentence: no machine code, no config-key token (subGroups[1], columnGroup…) — the
// admin additionally gets the failing axis as a label and `showCode` for diagnosis.
// PURE: no React, no fetch.

import { t, tf } from "./i18n";

export interface ReportErrorHint {
  /** Short headline of the cause. */
  title: string;
  /** Guidance, worded for the viewer's role. */
  body: string;
  /** Admin only: show the machine code + the server's raw sentence next to the hint. */
  showCode: boolean;
}

const num = (v: unknown): string | null =>
  typeof v === "number" && Number.isFinite(v) ? v.toLocaleString("vi-VN") : null;

// details.axis → a label ("groupBy" = level 1, "subGroups[i]" = level i+2, "columnGroup").
// Anything else is dropped rather than echoed.
function axisLabel(axis: unknown): string | null {
  if (axis === "groupBy") return tf("page.reports.error.axisLevel", { n: 1 });
  if (axis === "columnGroup") return t("page.reports.error.axisColumn");
  const m = typeof axis === "string" ? /^subGroups\[(\d+)\]$/.exec(axis) : null;
  return m ? tf("page.reports.error.axisLevel", { n: Number(m[1]) + 2 }) : null;
}

function cellsSentence(d: Record<string, unknown>): string {
  const [cap, cells, rows, columns] = [num(d.cap) ?? "1.000", num(d.cells), num(d.rows), num(d.columns)];
  if (!cells) return t("page.reports.error.cellsBare");
  if (rows && columns) return tf("page.reports.error.cellsGrid", { rows, columns, cells, cap });
  return tf(d.lowerBound === true ? "page.reports.error.cellsAtLeast" : "page.reports.error.cells", { cells, cap });
}

function groupsSentence(d: Record<string, unknown>): string {
  const [cap, groups] = [num(d.cap) ?? "1.000", num(d.groups)];
  if (!groups) return t("page.reports.error.groupsBare");
  return tf(d.lowerBound === true ? "page.reports.error.groupsAtLeast" : "page.reports.error.groups", { groups, cap });
}

export function reportErrorHint(
  code: string | null | undefined,
  details: Record<string, unknown> | null | undefined,
  isAdmin: boolean,
): ReportErrorHint {
  const d = details ?? {};
  const byRole = (admin: string, user: string) => (isAdmin ? admin : user);
  let title = t("page.reports.viewer.errorTitle");
  let body: string;
  switch (code) {
    case "FORBIDDEN":
      body = t("page.reports.viewer.errorForbidden");
      break;
    case "NOT_FOUND":
      body = t("page.reports.error.notFound");
      break;
    case "FIELD_FORBIDDEN":
    case "LIST_UNKNOWN_FILTER": // a filter on a field hidden from the viewer answers like an unknown one (queue #317)
    case "SORT_NOT_ALLOWED":
      body = t("page.reports.error.fieldForbidden");
      break;
    case "REPORT_MATRIX_TOO_MANY_CELLS":
      title = t("page.reports.error.cellsTitle");
      body = `${cellsSentence(d)} ${byRole(t("page.reports.error.narrowAdmin"), t("page.reports.error.narrowUser"))}`;
      break;
    case "REPORT_TOO_MANY_GROUPS":
      title = t("page.reports.error.groupsTitle");
      body = `${groupsSentence(d)} ${byRole(t("page.reports.error.narrowAdmin"), t("page.reports.error.narrowUser"))}`;
      break;
    case "REPORT_HAVING_INVALID":
      title = t("page.reports.error.havingTitle");
      body = byRole(t("page.reports.error.havingAdmin"), t("page.reports.error.havingUser"));
      break;
    case "REPORT_EXPORT_GRID_UNSUPPORTED":
      title = t("page.reports.error.exportGridTitle");
      body = t("page.reports.error.exportGrid");
      break;
    case "AGGREGATE_REQUIRES_INDEXED":
      body = `${t("page.reports.viewer.errorIndexHint")} ${byRole(t("page.reports.viewer.errorIndexAdmin"), t("page.reports.viewer.errorIndexUser"))}`;
      break;
    default:
      body = byRole(t("page.reports.error.genericAdmin"), t("page.reports.error.genericUser"));
  }
  const axis = isAdmin ? axisLabel(d.axis) : null;
  if (axis) body = `${body} ${tf("page.reports.error.axis", { axis })}`;
  return { title, body, showCode: isAdmin };
}
