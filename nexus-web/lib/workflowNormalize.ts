// specs/048 S5 — the `normalize` action of a before_save rule: { type, field, ops[] }. `ops` is an ORDERED
// list (1–4, no repeats) of trim / lower / upper / collapse_spaces, applied left to right to a text or
// email field. Editor params are flat strings like every action card, so ops travels as a JSON array
// string — opening and saving a rule keeps the stored order (a plain String(array) became "trim,lower"
// and a 422). React-free so vitest exercises it directly.
import type { FieldMeta } from "@/lib/api";

export const NORMALIZE_OPS = ["trim", "lower", "upper", "collapse_spaces"] as const;
export const MAX_NORMALIZE_OPS = 4;

/** Fields the server lets a normalize rewrite (normalizeTarget): free text a person typed. */
export const normalizeTargets = (fields: FieldMeta[]) => fields.filter((f) => f.fieldType === "text" || f.fieldType === "email");

export function normOps(params: Record<string, string>): string[] {
  try {
    const v = JSON.parse(params.ops ?? "[]");
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export const normParamsFromAction = (a: Record<string, unknown>): Record<string, string> => ({
  field: typeof a.field === "string" ? a.field : "",
  ops: JSON.stringify(Array.isArray(a.ops) ? a.ops.filter((x) => typeof x === "string") : []),
});

/** Sent as built — an empty list is refused by the server (NORMALIZE_OPS_INVALID) and pinned on the picklist. */
export const normPayload = (params: Record<string, string>) => ({ type: "normalize", field: params.field ?? "", ops: normOps(params) });
