// lib/reportCell.ts — the text ONE tabular-report cell shows. PURE (no React, no fetch).
//
// A projected relation / `user` column arrives from GET /reports/:id/run already resolved by the
// list engine's expand (api-conventions §4.4d): {id,label} · {id,label,email} · {id} (field-ACL
// withheld the label) · a bare id string (target gone / fetcher unwired). The report table has no
// per-column FieldSpec (unlike the record grid, which hands FieldDisplay one), so this is the one
// place that turns any of those shapes into what a human reads — never "[object Object]", never a
// 36-char UUID when a name was served. The client-side CSV fallback calls the SAME function, so
// the fallback file matches the screen cell for cell.
import { formatMoney } from "./format";

export function reportCellText(v: unknown, empty = "—"): string {
  if (v == null || v === "") return empty;
  if (Array.isArray(v)) return v.map((x) => reportCellText(x, "")).join(", ");
  if (typeof v === "object") {
    const o = v as Record<string, unknown>;
    // Expanded relation / user value: label → email → id (the FieldDisplay tri-state). An
    // {id}-only object is the EXP-01 shape: the caller may not read the target's name.
    if (typeof o.id === "string" && ("label" in o || Object.keys(o).length === 1)) {
      return str(o.label) || str(o.email) || o.id;
    }
    // Stored currency shape {amount, currencyCode} — the same formatter the record grid uses.
    if ("amount" in o) return formatMoney(Number(o.amount), str(o.currencyCode) || undefined);
    // Anything else (polymorphic {type,id}, composite): compact JSON, like the server CSV cell —
    // readable and greppable instead of the useless "[object Object]".
    return JSON.stringify(v);
  }
  return String(v);
}

const str = (x: unknown): string => (typeof x === "string" ? x : "");

// csvGuard mirrors the server's (record_export.go csvGuard) for the client-side CSV fallback: a
// cell starting with '=', '@', TAB or CR — or with '+'/'-' not followed by a plain number — is
// prefixed with a single quote so Excel/Sheets render it as text instead of executing it (OWASP
// CSV injection). Numeric cells ("-100", "+3.5") pass through so columns keep sorting numerically.
export function csvGuard(s: string): string {
  if (s === "") return s;
  const c = s[0];
  if (c === "=" || c === "@" || c === "\t" || c === "\r") return "'" + s;
  if ((c === "+" || c === "-") && Number.isNaN(Number(s))) return "'" + s;
  return s;
}
