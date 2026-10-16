// lib/approvalCriteria.ts — pure mapping between the ApprovalBuilder's entry-criteria rows
// (FilterRow, the Nexus Kit FilterBuilder model) and the backend approval criteria payload
// (approval_service.go buildApprovalCriteriaJSON).
//
// BE contract (verified against internal/services/approval_service.go + handlers/approval_handler.go):
//   • POST/PUT body key `criteria`: { match?: "and" | "or", rules: [{ field, op, value? }] }
//   • operators = the WORKFLOW vocabulary (workflow.ValidOperator): eq/neq/gt/gte/lt/lte/
//     contains/isnull/notnull — NO `in`, NO `between` (those are record-LIST ops only) — plus the
//     edge ops changed/changedTo, accepted by the BE on after_update processes only (specs/043 S2b).
//   • match "and" (default) or "or" since S2b. THIS screen authors AND rows only (the VÀ/HOẶC
//     toggle and the edge operators are FE-BPM-PROPS-b, on SLDS) — but it carries a stored "or"
//     back unchanged: re-sending "and" would silently rewrite an API-configured process.
//   • isnull/notnull/changed must OMIT `value` entirely (a value → 422); every other op REQUIRES one.
//   • no rules → omit `criteria` from the payload → NULL column → the process fires on EVERY
//     create/update of the entity (match-all).
//
// PURE: no React, no fetch — unit-tested in approvalCriteria.test.ts.

import { type FilterRow, opsForType, isNullOp, nextFilterId } from "@/lib/filterModel";

// The closed operator set buildApprovalCriteriaJSON accepts (workflow.validOperators).
const APPROVAL_OPS = new Set(["eq", "neq", "gt", "gte", "lt", "lte", "contains", "isnull", "notnull"]);

/** Per-field-type operator list for approval criteria: the record-list map (opsForType) minus
    anything outside the workflow/approval 9-op vocabulary (drops in/between). */
export function approvalOpsForType(fieldType: string): string[] {
  return opsForType(fieldType).filter((op) => APPROVAL_OPS.has(op));
}

/** Operator list when the process fires on an UPDATE: adds the edge operators (changed / changedTo —
    the server refuses them on a create/manual process, S2b). */
export function approvalOpsWithEdge(fieldType: string): string[] {
  return [...approvalOpsForType(fieldType), "changed", "changedTo"];
}

// type aliases (not interfaces) so they stay assignable to the api.ts `Record<string, unknown>`
// criteria payload slot.
export type ApprovalCriteriaRule = { field: string; op: string; value?: string };
export type ApprovalMatch = "and" | "or";
export type ApprovalCriteria = { match: ApprovalMatch; rules: ApprovalCriteriaRule[] };

// Operators with no operand: the null checks, and `changed` (it asks only whether the field moved).
export const noValueOp = (op: string) => isNullOp(op) || op === "changed";

/** The stored join mode (GET `criteria.match`): "or" only when the document says so, else AND —
    the BE default for an absent/empty match. */
export function criteriaMatch(raw: unknown): ApprovalMatch {
  return raw && typeof raw === "object" && (raw as { match?: unknown }).match === "or" ? "or" : "and";
}

/** Criteria rows → the POST/PUT `criteria` value. Null-operators omit `value` ENTIRELY;
    every other op always carries it (string — the BE evaluator coerces per operator, same as
    workflow conditions). No rows → undefined so the caller OMITS the key (BE: match-all). */
export function criteriaPayload(rows: FilterRow[], match: ApprovalMatch = "and"): ApprovalCriteria | undefined {
  if (rows.length === 0) return undefined;
  return {
    match,
    rules: rows.map((r) =>
      noValueOp(r.op) ? { field: r.field, op: r.op } : { field: r.field, op: r.op, value: r.value },
    ),
  };
}

/** First invalid row, if any (pre-save guard): a row needs a field, and the operand must match
    the operator — a value operator needs a non-blank value (an admin should use "rỗng"/"không
    rỗng" to test emptiness, not a blank operand that silently compares ""), and a no-value
    operator must not carry one: this screen shows a stored `changed` row as "= [ô giá trị]"
    (no label for it until FE-BPM-PROPS-b), and criteriaPayload would drop what was typed there
    while the save reported success. Returns null when every row is sendable. */
export function invalidCriteriaRow(rows: FilterRow[]): FilterRow | null {
  return rows.find((r) => !r.field.trim() || noValueOp(r.op) === (r.value.trim() !== "")) ?? null;
}

/** Stored criteria (GET response `criteria`: object or null) → editable rows. Defensive: null /
    non-object / missing rules / malformed entries hydrate to nothing rather than crashing the
    editor on hand-written JSON. Non-string stored values (number/bool from another client)
    render as their string form — byte-compatible with what the BE evaluator compares. */
export function criteriaToRows(raw: unknown): FilterRow[] {
  if (!raw || typeof raw !== "object") return [];
  const rules = (raw as { rules?: unknown }).rules;
  if (!Array.isArray(rules)) return [];
  return rules.flatMap((r): FilterRow[] => {
    if (!r || typeof r !== "object") return [];
    const { field, op, value } = r as { field?: unknown; op?: unknown; value?: unknown };
    if (typeof field !== "string" || typeof op !== "string") return [];
    return [{ id: nextFilterId(), field, op, value: value == null ? "" : String(value) }];
  });
}

/** A condition document held as the editor shows it (rows have ids so inputs keep focus). */
export interface CondState { match: ApprovalMatch; rows: FilterRow[] }

/** Stored condition ({match, rules}) → editable state; anything else = no rows. */
export const condFromWire = (raw: unknown): CondState => ({ match: criteriaMatch(raw), rows: criteriaToRows(raw) });

/** Editable state → the wire condition; no rows = undefined (the key is left out). */
export const condToWire = (c: CondState | null | undefined): ApprovalCriteria | undefined => (c ? criteriaPayload(c.rows, c.match) : undefined);
