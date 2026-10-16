// lib/workflowConds.ts — pure mapping between the WorkflowBuilder's condition rows (UI tokens)
// and the backend condition rule payload (workflow_service.go buildConditionJSON).
//
// Backend validOperators (internal/workflow/workflow.go): eq/neq/gt/gte/lt/lte/contains/
// isnull/notnull/changed/changedTo — all 11 offered here. isnull/notnull carry NO value: a rule that sends one
// gets 422 WORKFLOW_CONDITION_INVALID ("operator %q không nhận value"), and every OTHER
// operator REQUIRES a value ("operator %q cần value"). So condToRule OMITS the `value` key
// entirely for null-operators and always includes it otherwise.
//
// PURE: no React, no fetch.

export const OPS = ["=", "≠", ">", "≥", "<", "≤", "chứa", "trống", "không trống", "vừa đổi", "vừa đổi thành"] as const;
export type UiOp = (typeof OPS)[number];

export const OP_MAP: Record<string, string> = {
  "=": "eq",
  "≠": "neq",
  ">": "gt",
  "≥": "gte",
  "<": "lt",
  "≤": "lte",
  "chứa": "contains",
  "trống": "isnull",
  "không trống": "notnull",
  // Edge operators: they compare the record against its own before-image instead of a constant.
  // "vừa đổi thành" is the one people mean when they say "khi báo giá được duyệt" — written with
  // "=" the rule re-fires on every later save of an already-approved quote.
  "vừa đổi": "changed",
  "vừa đổi thành": "changedTo",
};

// Operators that carry NO value. "changed" joins them: it asks only whether the field moved.
// "changedTo" is NOT here — it pins where the value landed and needs one.
const NULL_OPS = new Set(["isnull", "notnull", "changed"]);

/** True when the (UI or backend) operator token is a null-operator — no value input shown,
    no `value` key in the payload. */
export function isNullOp(op: string): boolean {
  return NULL_OPS.has(OP_MAP[op] ?? op);
}

export interface CondRow {
  field: string;
  op: string; // UI token ("=", "trống", …) — loadWorkflow may leave a raw backend token as-is
  val: string;
}

const FIELD_REF = "@field:";

/** A stored rule value as the editor's text: a field reference becomes "@field:<name>". */
export function ruleValueText(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "object" && typeof (v as { field?: unknown }).field === "string") return FIELD_REF + (v as { field: string }).field;
  return String(v);
}

/** UI condition row → backend rule. Null-operators omit `value` ENTIRELY. */
export function condToRule(c: CondRow): { field: string; op: string; value?: unknown } {
  const op = OP_MAP[c.op] ?? c.op;
  if (NULL_OPS.has(op)) return { field: c.field, op };
  // queue #286: "@field:<name>" is a reference to another field of the record → {"field": name}.
  if (c.val.startsWith(FIELD_REF)) return { field: c.field, op, value: { field: c.val.slice(FIELD_REF.length) } };
  return { field: c.field, op, value: c.val };
}

/** Backend rule → UI condition row (loadWorkflow restore). Reverse-maps the operator token;
    a null-operator's absent value renders as an empty string. */
export function ruleToCond(r: { field: string; op: string; value?: unknown }): CondRow {
  const ui = Object.entries(OP_MAP).find(([, v]) => v === r.op)?.[0] ?? r.op;
  return { field: r.field, op: ui, val: ruleValueText(r.value) };
}
