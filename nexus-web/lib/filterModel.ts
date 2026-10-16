// filterModel — pure, framework-agnostic filter logic shared by the Nexus Kit FilterBuilder
// primitive (components/ui/FilterBuilder.tsx) and its consumers (record list today; report /
// dashboard / workflow-condition / approval-criteria in later waves).
//
// Extracted VERBATIM from components/layout/LayoutList.tsx so the list's data contract is
// unchanged byte-for-byte:
//   • the ?field[op]=value URL syntax  (comma-split for in/between; "1" for isnull/notnull;
//     bare field==eq is a backend concern — we always emit the [op] form)
//   • the 11-operator per-field-type map (eq/neq/contains/gt/gte/lt/lte/in/between/isnull/notnull)
//   • the drill-through parse (RESERVED_QUERY_KEYS + /^(.+)\[(\w+)\]$/), run only inside a client
//     effect by the caller (this module is SSR-safe — it never touches `window`).
//
// This file has NO React import — it is unit-testable in isolation.

// One active filter condition. Rows combine with AND semantics (flat, no grouping).
//
// FUTURE (OR): to support OR, wrap rows in a group model
//   { conjunction: "and" | "or"; rows: FilterRow[] }
// and teach filtersToQuery to emit a grouped param (e.g. a JSON `?filter=` blob); the backend
// filter parser would need a matching grammar. Kept flat/AND on purpose for now — every current
// consumer (list, report overlay, aggregate) is AND-only.
export interface FilterRow { id: string; field: string; op: string; value: string }

// Operator sets per field type — each one a subset of what the server's validateFilterOperator
// (internal/services/record_orchestration.go) accepts for that type. A saved report / widget filter
// is only shape-checked on save, so offering an operator the server refuses is a 422 on every run.
export const TEXT_OPS = ["eq", "neq", "contains", "isnull", "notnull"] as const;
export const NUM_OPS = ["eq", "neq", "gt", "gte", "lt", "lte", "between", "isnull", "notnull"] as const;
export const PICK_OPS = ["eq", "neq", "in", "isnull", "notnull"] as const;
export const DATE_OPS = ["eq", "neq", "gt", "gte", "lt", "lte", "between", "isnull", "notnull"] as const;
export const BOOL_OPS = ["eq", "isnull", "notnull"] as const;
// multi_select stores a list: the server takes only contains (one member) and the null checks.
export const MULTI_OPS = ["contains", "isnull", "notnull"] as const;
export const DEFAULT_OPS = ["eq", "neq", "isnull", "notnull"] as const;

export const OP_LABELS: Record<string, string> = {
  eq: "=", neq: "≠", gt: ">", gte: "≥", lt: "<", lte: "≤",
  in: "thuộc", between: "trong khoảng",
  isnull: "rỗng", notnull: "không rỗng", contains: "chứa",
};

export function opsForType(ft: string): readonly string[] {
  switch (ft) {
    case "number": case "currency": case "rating": return NUM_OPS;
    case "date": case "datetime": return DATE_OPS;
    // relation / user compare the stored id: equality and membership, never contains.
    case "picklist": case "relation": case "user": return PICK_OPS;
    case "multi_select": return MULTI_OPS;
    case "boolean": return BOOL_OPS;
    // contains is a text-only operator on the server (text/email/rich_text).
    case "text": case "email": case "rich_text": return TEXT_OPS;
    default: return DEFAULT_OPS;
  }
}

/** The type a field FILTERS as. A stored roll-up (specs/083, options.stored) keeps its total in the
 *  record as a plain number / currency, so it takes that type's operators (mirrors the server's
 *  validateFilterOperator); a read-time roll-up stays equality-only. */
export function filterTypeOf(f: { fieldType: string; options?: Record<string, unknown> }): string {
  if (f.fieldType === "rollup" && f.options?.stored === true) {
    return f.options.valueType === "currency" ? "currency" : "number";
  }
  // A formula's result is stored in the record at write time, so it filters as its resultType.
  if (f.fieldType === "formula") {
    const rt = f.options?.resultType;
    return rt === "currency" || rt === "text" ? rt : "number";
  }
  return f.fieldType;
}

// Operators that carry NO value (the query encodes them as "1").
export function isNullOp(op: string): boolean {
  return op === "isnull" || op === "notnull";
}

// Operators that carry MULTIPLE comma-separated values (?status[in]=open,pending — see
// internal/handlers/dynamic_record_handler.go: strings.Split(rawVal, ",")).
export function isMultiValueOp(op: string): boolean {
  return op === "in" || op === "between";
}

// Field types that cannot be filtered (structured/blob/binary/derived) — mirrors the exclusion
// LayoutList's FilterPanel already applied to its field picker.
// polymorphic_relation: the BARE field is not filterable (only its physical sub-columns
// <field>_type / <field>_id are — those aren't surfaced as pickable fields), so exclude it here.
export const NON_FILTERABLE_TYPES = ["composite", "rich_text", "file", "image", "polymorphic_relation"];
export function isFilterableType(ft: string): boolean {
  return !NON_FILTERABLE_TYPES.includes(ft);
}

// Module-scoped id counter (matches LayoutList's former `_filterId`). Ids are UI keys only — they
// never reach the server (filtersToQuery drops them).
let _filterId = 0;
export function nextFilterId(): string { return String(++_filterId); }
export function newFilterRow(field: string, op: string): FilterRow {
  return { id: nextFilterId(), field, op, value: "" };
}

// Serialize filter rows to URL query params: ?field[op]=value.
// Verbatim from LayoutList: isnull/notnull → "1"; non-null rows only when value is non-blank;
// value is trimmed; rows missing a field or op are skipped.
export function filtersToQuery(filters: FilterRow[]): string {
  const p = new URLSearchParams();
  for (const f of filters) {
    if (!f.field || !f.op) continue;
    if (f.op === "isnull" || f.op === "notnull") {
      p.set(`${f.field}[${f.op}]`, "1");
    } else if (f.value.trim()) {
      p.set(`${f.field}[${f.op}]`, f.value.trim());
    }
  }
  return p.toString();
}

// Reserved list params (sort/order/page/limit/q/expand/after) are NOT filters.
export const RESERVED_QUERY_KEYS = new Set(["sort", "order", "page", "limit", "q", "expand", "after"]);

// Parse a URL query string `?field[op]=value` into filter rows (drill-through / deep-link).
// PURE: takes the search string explicitly (the caller passes window.location.search from inside a
// client effect) so this never runs during SSR and never touches `window`.
export function parseFilterQuery(search: string): FilterRow[] {
  const rows: FilterRow[] = [];
  new URLSearchParams(search).forEach((value, key) => {
    const m = key.match(/^(.+)\[(\w+)\]$/);
    if (!m) return;
    const [, field, op] = m;
    if (RESERVED_QUERY_KEYS.has(field)) return;
    rows.push({ id: nextFilterId(), field, op, value });
  });
  return rows;
}

// ── Active / chip model ────────────────────────────────────────────────────────────
// A row is "active" (contributes to the query) when it has a field + op and either a null-op or a
// non-blank value. Half-typed rows are inert — they neither fetch nor show a chip.
export function isActiveFilter(f: FilterRow): boolean {
  if (!f.field || !f.op) return false;
  if (isNullOp(f.op)) return true;
  return f.value.trim() !== "";
}

export function activeFilters(filters: FilterRow[]): FilterRow[] {
  return filters.filter(isActiveFilter);
}

export interface FilterChip { id: string; label: string }

// Human label for one active filter, e.g. "Giai đoạn = Proposal" / "Số điện thoại không rỗng".
// `labelFor` maps a field NAME → its display label (falls back to the name).
// `valueLabelFor` (optional third param): display text for one raw filter VALUE — the picklist
// valueLabels hook ("New" → "Mới"). Optional because most callers/values have no label layer;
// omitted or unmatched, the raw value shows as before. The filter itself keeps comparing codes.
export function filterChipLabel(
  f: FilterRow,
  labelFor: (field: string) => string,
  valueLabelFor?: (field: string, value: string) => string,
): string {
  const field = labelFor(f.field);
  const op = OP_LABELS[f.op] ?? f.op;
  if (isNullOp(f.op)) return `${field} · ${op}`;
  const val = f.value.trim();
  const show = (v: string) => (valueLabelFor ? valueLabelFor(f.field, v) : v);
  // in/between show the comma-joined members (e.g. "50, 200"), each resolved through the label layer.
  const shown = isMultiValueOp(f.op)
    ? val.split(",").map((s) => s.trim()).filter(Boolean).map(show).join(", ")
    : show(val);
  return `${field} ${op} ${shown}`;
}

export function filterChips(
  filters: FilterRow[],
  labelFor: (field: string) => string,
  valueLabelFor?: (field: string, value: string) => string,
): FilterChip[] {
  return activeFilters(filters).map((f) => ({ id: f.id, label: filterChipLabel(f, labelFor, valueLabelFor) }));
}

// ── Expression model (components/slds/ExpressionBuilder) ─────────────────────────────
// Additive on top of the flat FilterRow[] contract above: a caller that only passes rows keeps the
// AND-only semantics every current backend expects. Match mode, custom logic and one level of
// groups are opt-in and carry no serialization yet — the caller maps them to its own payload
// (workflow conditions have OR; list/approval filters are AND-only).

/** Minimal field descriptor the builders need (a superset-compatible subset of FieldMeta). */
export interface FilterField {
  name: string;
  label?: string;
  fieldType: string;
  options?: Record<string, unknown>;
}

/** The owner dimension as a pickable filter field: every record carries `ownerId` (the server
 *  accepts it as a reserved key with `user` semantics), but no entity declares it as a field. */
export const OWNER_FILTER_FIELD: FilterField = { name: "ownerId", label: "Người phụ trách", fieldType: "user" };

/** Fields + the owner dimension (a declared field named ownerId wins). */
export function withOwnerFilter<T extends FilterField>(fields: readonly T[]): (T | FilterField)[] {
  return fields.some((f) => f.name === OWNER_FILTER_FIELD.name) ? [...fields] : [...fields, OWNER_FILTER_FIELD];
}

/** "Take action when": all rows (AND), any row (OR), or a custom logic string over row numbers. */
export type FilterMatch = "all" | "any" | "custom";

/** One nested condition group (one level deep; a group holds rows, never groups). */
export interface FilterGroup { id: string; match: "all" | "any"; rows: FilterRow[] }

export function newFilterGroup(): FilterGroup {
  return { id: nextFilterId(), match: "all", rows: [newFilterRow("", "")] };
}

// Relative-date tokens the backend resolves at query time (internal/services/relative_dates.go
// relativeDateTokens) and the operators that accept one ("" = eq default; between/in/neq → 422).
export const RELATIVE_DATE_TOKENS = [
  "today", "yesterday", "tomorrow", "this_week", "last_week", "next_week", "this_month", "last_month",
  "this_quarter", "this_year", "last_7_days", "last_30_days", "last_90_days", "next_7_days", "next_30_days",
] as const;
export const RELATIVE_OK_OPS = new Set(["", "eq", "gt", "gte", "lt", "lte"]);
export function isRelativeDateToken(v: string): boolean {
  return (RELATIVE_DATE_TOKENS as readonly string[]).includes(v);
}

/** Problem with a custom logic string, or null when it is usable. */
export type LogicIssue = { code: "syntax" } | { code: "unknown" | "unused"; n: number };

/** Checks a custom logic string ("1 AND (2 OR NOT 3)") against `count` numbered conditions:
 *  grammar expr := term ((AND|OR) term)* ; term := NOT term | "(" expr ")" | number (case-insensitive);
 *  every number must exist and every condition must be referenced (Salesforce's rule). */
export function checkCustomLogic(logic: string, count: number): LogicIssue | null {
  const tokens = logic.toUpperCase().match(/\d+|AND|OR|NOT|[()]|[^\s()]+/g) ?? [];
  const used = new Set<number>();
  let unknown: number | undefined;
  let i = 0;
  const term = (): boolean => {
    const tk = tokens[i++];
    if (tk === "NOT") return term();
    if (tk === "(") return expr() && tokens[i++] === ")";
    if (tk === undefined || !/^\d+$/.test(tk)) return false;
    const n = Number(tk);
    if ((n < 1 || n > count) && unknown === undefined) unknown = n;
    used.add(n);
    return true;
  };
  const expr = (): boolean => {
    if (!term()) return false;
    while (tokens[i] === "AND" || tokens[i] === "OR") {
      i++;
      if (!term()) return false;
    }
    return true;
  };
  if (!tokens.length || !expr() || i !== tokens.length) return { code: "syntax" };
  if (unknown !== undefined) return { code: "unknown", n: unknown };
  for (let n = 1; n <= count; n++) if (!used.has(n)) return { code: "unused", n };
  return null;
}
