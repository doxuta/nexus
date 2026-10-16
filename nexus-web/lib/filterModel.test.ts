// filterModel — pure-logic unit tests for the shared FilterBuilder model.
// Covers: operator-per-fieldtype map, value→query-param encoding, the chip/active model, and the
// serialize↔parse round-trip. These lock the ?field[op]=value contract that every entity list (and
// future report/workflow/approval consumer) depends on.

import { describe, it, expect } from "vitest";
import {
  type FilterRow,
  opsForType, filterTypeOf,
  TEXT_OPS, NUM_OPS, PICK_OPS, DATE_OPS, BOOL_OPS, MULTI_OPS, DEFAULT_OPS,
  isNullOp, isMultiValueOp, isFilterableType, NON_FILTERABLE_TYPES,
  filtersToQuery, parseFilterQuery, RESERVED_QUERY_KEYS,
  isActiveFilter, activeFilters, filterChipLabel, filterChips,
  newFilterRow,
  checkCustomLogic, newFilterGroup, isRelativeDateToken, RELATIVE_OK_OPS,
} from "./filterModel";

const row = (field: string, op: string, value = "", id = "1"): FilterRow => ({ id, field, op, value });

// ── operator-per-fieldtype map ──────────────────────────────────────────────────────
describe("opsForType — 11-operator map matches dynrepo per field type", () => {
  it("number / currency / rating → numeric ops (with between)", () => {
    expect(opsForType("number")).toBe(NUM_OPS);
    expect(opsForType("currency")).toBe(NUM_OPS);
    expect(opsForType("rating")).toBe(NUM_OPS);
    expect(NUM_OPS).toContain("between");
    expect(NUM_OPS).toContain("gte");
  });
  it("date / datetime → date ops (with between)", () => {
    expect(opsForType("date")).toBe(DATE_OPS);
    expect(opsForType("datetime")).toBe(DATE_OPS);
  });
  it("picklist → pick ops (with in, no gt/lt)", () => {
    expect(opsForType("picklist")).toBe(PICK_OPS);
    expect(PICK_OPS).toContain("in");
    expect(PICK_OPS).not.toContain("between");
  });
  it("multi_select → contains + null ops only (the value is a list)", () => {
    expect(opsForType("multi_select")).toBe(MULTI_OPS);
    expect(MULTI_OPS).toEqual(["contains", "isnull", "notnull"]);
  });
  it("relation / user → id equality and membership, never contains", () => {
    expect(opsForType("relation")).toBe(PICK_OPS);
    expect(opsForType("user")).toBe(PICK_OPS);
  });
  it("boolean → eq + null ops only", () => {
    expect(opsForType("boolean")).toBe(BOOL_OPS);
    expect(BOOL_OPS).toEqual(["eq", "isnull", "notnull"]);
  });
  it("text / email / rich_text → text ops (contains, no ordering)", () => {
    expect(opsForType("text")).toBe(TEXT_OPS);
    expect(opsForType("email")).toBe(TEXT_OPS);
    expect(opsForType("rich_text")).toBe(TEXT_OPS);
    expect(TEXT_OPS).toContain("contains");
    expect(TEXT_OPS).not.toContain("gt");
  });
  it("other types (auto_number, phone, url, lookup, unknown) → equality + null ops, no contains", () => {
    for (const ft of ["auto_number", "phone", "url", "lookup", "rollup", "something_new"]) {
      expect(opsForType(ft)).toBe(DEFAULT_OPS);
    }
  });
  it("filterTypeOf: a stored roll-up filters as its number/currency; a read-time roll-up stays equality-only (specs/083)", () => {
    expect(opsForType(filterTypeOf({ fieldType: "rollup", options: { stored: true } }))).toBe(NUM_OPS);
    expect(filterTypeOf({ fieldType: "rollup", options: { stored: true, valueType: "currency" } })).toBe("currency");
    expect(opsForType(filterTypeOf({ fieldType: "rollup", options: {} }))).toBe(DEFAULT_OPS);
    expect(filterTypeOf({ fieldType: "text" })).toBe("text");
  });
  // Parity with the server: every operator offered for a type is one validateFilterOperator
  // (internal/services/record_orchestration.go) accepts for it. A saved report / widget filter is
  // only checked for shape on save, so an operator the server refuses becomes a 422 on every run.
  it("never offers an operator the server refuses for the field type", () => {
    const ordered = ["number", "currency", "rating", "date", "datetime"];
    const contains = ["text", "email", "rich_text", "multi_select"];
    const inList = ["text", "email", "picklist", "relation", "user"];
    const accepts = (ft: string, op: string): boolean => {
      if (op === "isnull" || op === "notnull") return true;
      if (ft === "multi_select") return op === "contains";
      switch (op) {
        case "eq": case "neq": return true;
        case "gt": case "gte": case "lt": case "lte": case "between": return ordered.includes(ft);
        case "in": return inList.includes(ft);
        case "contains": return contains.includes(ft);
        default: return false;
      }
    };
    const types = [
      "text", "email", "rich_text", "number", "currency", "rating", "date", "datetime", "picklist",
      "multi_select", "boolean", "relation", "user", "auto_number", "phone", "url", "lookup", "rollup",
    ];
    for (const ft of types) {
      for (const op of opsForType(ft)) expect(accepts(ft, op), `${ft} ${op}`).toBe(true);
    }
  });
  it("the full operator vocabulary is exactly the 11 documented operators", () => {
    const all = new Set<string>([...TEXT_OPS, ...NUM_OPS, ...PICK_OPS, ...DATE_OPS, ...BOOL_OPS]);
    expect([...all].sort()).toEqual(
      ["between", "contains", "eq", "gt", "gte", "in", "isnull", "lt", "lte", "neq", "notnull"],
    );
    expect(all.size).toBe(11);
  });
  it("isNullOp / isMultiValueOp classify operators", () => {
    expect(isNullOp("isnull")).toBe(true);
    expect(isNullOp("notnull")).toBe(true);
    expect(isNullOp("eq")).toBe(false);
    expect(isMultiValueOp("in")).toBe(true);
    expect(isMultiValueOp("between")).toBe(true);
    expect(isMultiValueOp("eq")).toBe(false);
  });
});

// ── filterable field types ──────────────────────────────────────────────────────────
describe("isFilterableType — excludes structured/blob/derived types", () => {
  it("excludes exactly composite/rich_text/file/image/formula/polymorphic_relation", () => {
    // polymorphic_relation bare field is not filterable (only its <f>_type/<f>_id sub-columns are).
    expect(NON_FILTERABLE_TYPES.sort()).toEqual(["composite", "file", "image", "polymorphic_relation", "rich_text"]);
    for (const t of NON_FILTERABLE_TYPES) expect(isFilterableType(t)).toBe(false);
  });
  it("allows scalar/relation types", () => {
    for (const t of ["text", "number", "currency", "date", "picklist", "boolean", "relation"]) {
      expect(isFilterableType(t)).toBe(true);
    }
  });
});

// ── value → query-param encoding (the ?field[op]=value contract) ─────────────────────
describe("filtersToQuery — ?field[op]=value serialization", () => {
  it("emits field[op]=value for eq", () => {
    expect(new URLSearchParams(filtersToQuery([row("subject", "eq", "hello")])).get("subject[eq]")).toBe("hello");
  });
  it("isnull / notnull → value '1', ignoring any typed value", () => {
    expect(new URLSearchParams(filtersToQuery([row("deleted_at", "isnull")])).get("deleted_at[isnull]")).toBe("1");
    expect(new URLSearchParams(filtersToQuery([row("phone", "notnull", "junk")])).get("phone[notnull]")).toBe("1");
  });
  it("trims the value", () => {
    expect(new URLSearchParams(filtersToQuery([row("name", "contains", "  abc  ")])).get("name[contains]")).toBe("abc");
  });
  it("skips rows with a blank value (non-null ops)", () => {
    expect(filtersToQuery([row("name", "contains", "")])).toBe("");
    expect(filtersToQuery([row("name", "contains", "   ")])).toBe("");
  });
  it("skips rows missing a field or op", () => {
    expect(filtersToQuery([row("", "eq", "value")])).toBe("");
    expect(filtersToQuery([row("name", "", "value")])).toBe("");
  });
  it("in / between carry comma-joined members verbatim", () => {
    const inQ = new URLSearchParams(filtersToQuery([row("stage", "in", "open,won")]));
    expect(inQ.get("stage[in]")).toBe("open,won");
    const btwQ = new URLSearchParams(filtersToQuery([row("amount", "between", "50,200")]));
    expect(btwQ.get("amount[between]")).toBe("50,200");
  });
  it("serializes multiple rows (AND — one param each)", () => {
    const p = new URLSearchParams(filtersToQuery([row("status", "eq", "open", "1"), row("priority", "eq", "high", "2")]));
    expect(p.get("status[eq]")).toBe("open");
    expect(p.get("priority[eq]")).toBe("high");
  });
});

// ── parse (drill-through / deep-link) ───────────────────────────────────────────────
describe("parseFilterQuery — reads ?field[op]=value back into rows", () => {
  it("parses a bracketed key into field/op/value", () => {
    const rows = parseFilterQuery("?owner_id[eq]=abc123");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ field: "owner_id", op: "eq", value: "abc123" });
    expect(rows[0].id).toBeTruthy();
  });
  it("ignores reserved list params", () => {
    for (const k of RESERVED_QUERY_KEYS) {
      // a reserved key can never appear in bracket form, but a bare reserved key must never parse
      expect(parseFilterQuery(`?${k}=x`)).toHaveLength(0);
    }
  });
  it("ignores non-bracketed keys", () => {
    expect(parseFilterQuery("?plainKey=value")).toHaveLength(0);
  });
  it("parses several conditions", () => {
    const rows = parseFilterQuery("?stage[eq]=Proposal&amount[gte]=1000");
    expect(rows.map((r) => `${r.field}[${r.op}]=${r.value}`).sort()).toEqual(
      ["amount[gte]=1000", "stage[eq]=Proposal"],
    );
  });
});

// ── serialize ↔ parse round-trip ────────────────────────────────────────────────────
describe("round-trip — filtersToQuery → parseFilterQuery preserves field/op/value", () => {
  it("value operators survive the round-trip", () => {
    const src = [row("stage", "eq", "Proposal", "a"), row("amount", "between", "50,200", "b"), row("name", "contains", "acme", "c")];
    const parsed = parseFilterQuery("?" + filtersToQuery(src));
    const norm = (rs: FilterRow[]) => rs.map(({ field, op, value }) => ({ field, op, value })).sort((x, y) => x.field.localeCompare(y.field));
    expect(norm(parsed)).toEqual(norm(src));
  });
  it("null operators round-trip with value '1' (the encoded sentinel)", () => {
    const parsed = parseFilterQuery("?" + filtersToQuery([row("deleted_at", "isnull", "", "x")]));
    expect(parsed[0]).toMatchObject({ field: "deleted_at", op: "isnull", value: "1" });
  });
});

// ── active / chip model ─────────────────────────────────────────────────────────────
describe("isActiveFilter / activeFilters — which rows contribute", () => {
  it("null ops are always active", () => {
    expect(isActiveFilter(row("x", "isnull"))).toBe(true);
    expect(isActiveFilter(row("x", "notnull"))).toBe(true);
  });
  it("value ops need a non-blank value", () => {
    expect(isActiveFilter(row("x", "eq", "v"))).toBe(true);
    expect(isActiveFilter(row("x", "eq", "   "))).toBe(false);
    expect(isActiveFilter(row("x", "eq", ""))).toBe(false);
  });
  it("rows missing a field or op are inert", () => {
    expect(isActiveFilter(row("", "eq", "v"))).toBe(false);
    expect(isActiveFilter(row("x", "", "v"))).toBe(false);
  });
  it("activeFilters drops the half-typed rows", () => {
    const rows = [row("a", "eq", "1", "1"), row("b", "eq", "", "2"), row("c", "isnull", "", "3")];
    expect(activeFilters(rows).map((r) => r.id)).toEqual(["1", "3"]);
  });
});

describe("filterChipLabel / filterChips — human summary", () => {
  const labelFor = (n: string) => ({ stage: "Giai đoạn", phone: "Số điện thoại", amount: "Giá trị" }[n] ?? n);
  it("value op → 'Label op value'", () => {
    expect(filterChipLabel(row("stage", "eq", "Proposal"), labelFor)).toBe("Giai đoạn = Proposal");
  });
  it("null op → 'Label · opLabel'", () => {
    expect(filterChipLabel(row("phone", "notnull"), labelFor)).toBe("Số điện thoại · không rỗng");
  });
  it("multi-value op normalizes comma spacing", () => {
    expect(filterChipLabel(row("amount", "between", "50, 200"), labelFor)).toBe("Giá trị trong khoảng 50, 200");
  });
  it("filterChips only produces chips for active rows", () => {
    const rows = [row("stage", "eq", "Proposal", "1"), row("phone", "eq", "", "2")];
    const chips = filterChips(rows, labelFor);
    expect(chips).toHaveLength(1);
    expect(chips[0]).toMatchObject({ id: "1", label: "Giai đoạn = Proposal" });
  });
});

// ── id generator ────────────────────────────────────────────────────────────────────
describe("newFilterRow — fresh rows with unique ids", () => {
  it("creates a row with the given field/op, blank value, and a unique id", () => {
    const a = newFilterRow("stage", "eq");
    const b = newFilterRow("stage", "eq");
    expect(a).toMatchObject({ field: "stage", op: "eq", value: "" });
    expect(a.id).not.toBe(b.id);
  });
});

// ── expression model ─────────────────────────────────────────────────────────────────
describe("checkCustomLogic — custom logic over numbered conditions", () => {
  it("accepts AND/OR/NOT with parentheses, case-insensitive", () => {
    expect(checkCustomLogic("1 AND (2 OR NOT 3)", 3)).toBeNull();
    expect(checkCustomLogic("(1 or 2) and not 3", 3)).toBeNull();
  });
  it("flags broken grammar, unknown and unreferenced conditions", () => {
    expect(checkCustomLogic("", 1)).toEqual({ code: "syntax" });
    expect(checkCustomLogic("1 AND", 1)).toEqual({ code: "syntax" });
    expect(checkCustomLogic("(1 OR 2", 2)).toEqual({ code: "syntax" });
    expect(checkCustomLogic("1 VÀ 2", 2)).toEqual({ code: "syntax" });
    expect(checkCustomLogic("1 OR 4", 2)).toEqual({ code: "unknown", n: 4 });
    expect(checkCustomLogic("1 OR 2", 3)).toEqual({ code: "unused", n: 3 });
  });
});

describe("newFilterGroup / relative dates", () => {
  it("a new group starts as AND with one blank row", () => {
    const g = newFilterGroup();
    expect(g.match).toBe("all");
    expect(g.rows).toHaveLength(1);
    expect(g.rows[0]).toMatchObject({ field: "", op: "", value: "" });
  });
  it("relative-date tokens mirror the backend set; between/in/neq take none", () => {
    expect(isRelativeDateToken("this_week")).toBe(true);
    expect(isRelativeDateToken("2026-09-29")).toBe(false);
    expect(RELATIVE_OK_OPS.has("between")).toBe(false);
  });
});

describe("formula fields filter as their resultType (queue #285)", () => {
  it("is filterable and takes the operators of its result type", () => {
    expect(isFilterableType("formula")).toBe(true);
    expect(filterTypeOf({ fieldType: "formula" })).toBe("number");
    expect(filterTypeOf({ fieldType: "formula", options: { resultType: "currency" } })).toBe("currency");
    expect(filterTypeOf({ fieldType: "formula", options: { resultType: "text" } })).toBe("text");
  });
});
