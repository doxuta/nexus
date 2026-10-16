// lib/fieldOptions.test.ts — Studio field-builder option assembly for the advanced/computed types.
// The KEYS must match the backend contract (field_service.go); a wrong shape → silent-drop or 422.
import { describe, it, expect } from "vitest";
import type { PicklistRow } from "@/lib/picklistRows";
import {
  buildOptions, isRollupAggregatable,
  emptyLookup, emptyRollup, emptyAutoNumber, emptyRelation,
} from "./fieldOptions";

const NO_PICK: PicklistRow[] = [];
// Positional shim so tests read cleanly — the scalar-option args are irrelevant for the advanced types.
const build = (
  type: Parameters<typeof buildOptions>[0],
  advanced?: Parameters<typeof buildOptions>[12],
  related = "",
) => buildOptions(type, "VND", NO_PICK, related, "", "", "", "", "", "", "", "", advanced);

describe("buildOptions — formula resultType (DAILY-QUEUE #25)", () => {
  // The BE accepts only number|currency (validateFormulaConfig). A "text" default made the live
  // editor say "Hợp lệ" (the dry-run drops a text hint) and then Save answer 422.
  it("defaults resultType to number, never text", () => {
    expect(buildOptions("formula", "VND", NO_PICK, "", "", "", "", "", "", "amount * 2")).toEqual({
      formula: "amount * 2", resultType: "number",
    });
  });
  // A currency result must carry its code: without it the record write falls back to "USD"
  // (computed_fields.go) and a VND commission shows as dollars.
  it("carries an explicit currency resultType with its currencyCode", () => {
    expect(buildOptions("formula", "VND", NO_PICK, "", "", "", "", "", "",
      "IF(amount >= 100000000, amount * 0.05, amount * 0.03)", "currency")).toEqual({
      formula: "IF(amount >= 100000000, amount * 0.05, amount * 0.03)", resultType: "currency", currencyCode: "VND",
    });
  });
  it("defaults a blank currencyCode to VND on a currency result", () => {
    expect(buildOptions("formula", "", NO_PICK, "", "", "", "", "", "", "amount * 0.05", "currency")).toEqual({
      formula: "amount * 0.05", resultType: "currency", currencyCode: "VND",
    });
  });
});

describe("buildOptions — relation cardinality / inverseField (ENG-REL2 #4/#5)", () => {
  it("defaults cardinality to 1-N and omits inverseField when blank", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), autoNumber: emptyAutoNumber(), relation: emptyRelation() };
    expect(build("relation", adv, "crm_accounts")).toEqual({ relatedEntity: "crm_accounts", cardinality: "1-N" });
  });
  it("carries cardinality 1-1 + trimmed inverseField", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), autoNumber: emptyAutoNumber(),
      relation: { cardinality: "1-1" as const, inverseField: "  primary_contact " } };
    expect(build("relation", adv, "crm_accounts")).toEqual({
      relatedEntity: "crm_accounts", cardinality: "1-1", inverseField: "primary_contact",
    });
  });
  it("no relatedEntity → undefined (nothing to send)", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), autoNumber: emptyAutoNumber(), relation: emptyRelation() };
    expect(build("relation", adv, "")).toBeUndefined();
  });
});

describe("buildOptions — lookup (ENG-COMPUTED #2)", () => {
  it("assembles { via, sourceField }", () => {
    const adv = { lookup: { via: "account", sourceField: "industry" }, rollup: emptyRollup(), autoNumber: emptyAutoNumber(), relation: emptyRelation() };
    expect(build("lookup", adv)).toEqual({ via: "account", sourceField: "industry" });
  });
  it("partial (via only) still sends via", () => {
    const adv = { lookup: { via: "account", sourceField: "" }, rollup: emptyRollup(), autoNumber: emptyAutoNumber(), relation: emptyRelation() };
    expect(build("lookup", adv)).toEqual({ via: "account" });
  });
  it("empty lookup → undefined", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), autoNumber: emptyAutoNumber(), relation: emptyRelation() };
    expect(build("lookup", adv)).toBeUndefined();
  });
});

describe("buildOptions — rollup (ENG-COMPUTED #3)", () => {
  it("count metric DROPS aggField, keeps viaRelation (1-N mode)", () => {
    const adv = { lookup: emptyLookup(), autoNumber: emptyAutoNumber(), relation: emptyRelation(),
      rollup: { childEntity: "crm_opportunities", metric: "count" as const, aggField: "amount", mode: "viaRelation" as const, viaRelation: "account", relationship: "" } };
    expect(build("rollup", adv)).toEqual({ childEntity: "crm_opportunities", metric: "count", viaRelation: "account" });
  });
  it("sum metric keeps aggField", () => {
    const adv = { lookup: emptyLookup(), autoNumber: emptyAutoNumber(), relation: emptyRelation(),
      rollup: { childEntity: "crm_opportunities", metric: "sum" as const, aggField: "amount", mode: "viaRelation" as const, viaRelation: "account", relationship: "" } };
    expect(build("rollup", adv)).toEqual({ childEntity: "crm_opportunities", metric: "sum", aggField: "amount", viaRelation: "account" });
  });
  it("relationship (N-N) mode sends `relationship`, not `viaRelation` (mutually exclusive)", () => {
    const adv = { lookup: emptyLookup(), autoNumber: emptyAutoNumber(), relation: emptyRelation(),
      rollup: { childEntity: "crm_opportunities", metric: "sum" as const, aggField: "amount", mode: "relationship" as const, viaRelation: "account", relationship: "crm_contacts_opportunities" } };
    const out = build("rollup", adv)!;
    expect(out).toEqual({ childEntity: "crm_opportunities", metric: "sum", aggField: "amount", relationship: "crm_contacts_opportunities" });
    expect(out).not.toHaveProperty("viaRelation");
  });
});

// #262: the rollup "Trường cần tính" list must offer exactly what the engine sums
// (rollup_validate.go) — number/currency/rating, and a formula whose result is a number or money
// (missing resultType = number). A text formula stores a string: the engine answers 422.
describe("isRollupAggregatable — formula reading a lookup/rollup (specs/080)", () => {
  const sib = [{ name: "gia", fieldType: "lookup" }, { name: "sl", fieldType: "number" }];
  it("refuses a view-time formula, accepts a stored one", () => {
    expect(isRollupAggregatable({ fieldType: "formula", options: { formula: "sl * gia" } }, sib)).toBe(false);
    expect(isRollupAggregatable({ fieldType: "formula", options: { formula: "sl * 2" } }, sib)).toBe(true);
  });
});

describe("isRollupAggregatable — mirrors validateRollupCrossEntity (#262)", () => {
  const f = (fieldType: string, options?: Record<string, unknown>) => ({ name: "x", fieldType, options });
  it.each([
    ["number", undefined, true],
    ["currency", undefined, true],
    ["rating", undefined, true],
    ["formula", { resultType: "number" }, true],
    ["formula", { resultType: "currency", currencyCode: "VND" }, true],
    ["formula", {}, true],
    ["formula", undefined, true],
    ["formula", { resultType: "text" }, false],
    ["text", undefined, false],
    ["rollup", undefined, false],
    ["lookup", undefined, false],
  ])("%s %o → %s", (type, options, want) => {
    expect(isRollupAggregatable(f(type, options))).toBe(want);
  });
});

describe("buildOptions — polymorphic_relation (ACTIVITY-CORE #3a)", () => {
  const buildPoly = (targets: string[]) =>
    buildOptions("polymorphic_relation", "VND", NO_PICK, "", "", "", "", "", "", "", "", "", undefined, targets);
  it("sends options.targetEntities in BE shape (non-empty string array)", () => {
    expect(buildPoly(["crm_accounts", "crm_contacts"])).toEqual({ targetEntities: ["crm_accounts", "crm_contacts"] });
  });
  it("trims + drops blank targets", () => {
    expect(buildPoly(["  crm_accounts ", "", "  "])).toEqual({ targetEntities: ["crm_accounts"] });
  });
  it("no targets → undefined (server answers FIELD_POLYMORPHIC_NO_TARGETS)", () => {
    expect(buildPoly([])).toBeUndefined();
  });
});

describe("buildOptions — auto_number (ENG #6)", () => {
  it("prefix + padding parsed to int", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), relation: emptyRelation(),
      autoNumber: { prefix: "INV-", padding: "5" } };
    expect(build("auto_number", adv)).toEqual({ prefix: "INV-", padding: 5 });
  });
  it("blank padding omitted; prefix-only kept", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), relation: emptyRelation(),
      autoNumber: { prefix: "PO-", padding: "" } };
    expect(build("auto_number", adv)).toEqual({ prefix: "PO-" });
  });
  it("empty auto_number → undefined", () => {
    const adv = { lookup: emptyLookup(), rollup: emptyRollup(), relation: emptyRelation(), autoNumber: emptyAutoNumber() };
    expect(build("auto_number", adv)).toBeUndefined();
  });
});

// ── Slice 1.2 (TOTAL-UPGRADE): default value + D1 indexed flag ──
import {
  defaultInputKind, supportsDefaultValue, supportsIndexed,
  encodeDefaultValue, decodeDefaultValue, withIndexed, formulaEditOptions,
} from "./fieldOptions";
import type { FieldType } from "@/lib/metadata";

describe("defaultInputKind — per-type default input matrix (mirrors BE ValidateFieldDefaultNotSupported + record_service defaultValue())", () => {
  it("renders the right native widget per supported type", () => {
    expect(defaultInputKind("text")).toBe("text");
    expect(defaultInputKind("email")).toBe("text");
    expect(defaultInputKind("number")).toBe("number");
    expect(defaultInputKind("rating")).toBe("number");
    expect(defaultInputKind("boolean")).toBe("boolean"); // 3-state select, not a 2-state toggle
    expect(defaultInputKind("picklist")).toBe("select");
    expect(defaultInputKind("multi_select")).toBe("multi");
    expect(defaultInputKind("date")).toBe("date");
    expect(defaultInputKind("datetime")).toBe("datetime");
  });
  it("hides the input for types the BE REJECTS with FIELD_DEFAULT_NOT_SUPPORTED", () => {
    for (const t of ["currency", "rich_text", "lookup", "rollup", "auto_number", "polymorphic_relation"] as FieldType[]) {
      expect(defaultInputKind(t), t).toBeNull();
      expect(supportsDefaultValue(t), t).toBe(false);
    }
  });
  it("hides the input for types the BE silently IGNORES (dead config) + relation ", () => {
    for (const t of ["formula", "composite", "file", "image"] as FieldType[]) {
      expect(defaultInputKind(t), t).toBeNull();
    }
  });
  // queue #320: the builder has a record picker for relation (stores the record id).
  it("offers a record picker for relation", () => {
    expect(defaultInputKind("relation")).toBe("record");
    expect(supportsDefaultValue("relation")).toBe(true);
  });
});

describe("supportsIndexed — D1 positive allowlist (mirrors ddl.indexedColExpr)", () => {
  it("allows exactly the types with a generated-column projection", () => {
    for (const t of ["text", "email", "picklist", "auto_number", "number", "rating", "currency", "date"] as FieldType[]) {
      expect(supportsIndexed(t, false), t).toBe(true);
    }
  });
  it("unique field never offers the toggle (already has a physical indexed column)", () => {
    expect(supportsIndexed("text", true)).toBe(false);
  });
  it("rejects everything else (datetime 422s, relation FK already indexed, file/computed/poly guarded, rest unsupported)", () => {
    for (const t of ["datetime", "relation", "polymorphic_relation", "boolean", "rich_text", "multi_select",
      "file", "image", "lookup", "rollup", "formula", "composite"] as FieldType[]) {
      expect(supportsIndexed(t, false), t).toBe(false);
    }
  });
});

describe("buildOptions — indexed emission (create path)", () => {
  it("date + indexed → {indexed:true} even though the type otherwise sends no options", () => {
    expect(buildOptions("date", "VND", NO_PICK, "", "", "", "", "", "", "", "", "", undefined, undefined, true))
      .toEqual({ indexed: true });
  });
  it("number keeps sibling keys next to indexed", () => {
    expect(buildOptions("number", "VND", NO_PICK, "", "5", "", "", "", "", "", "", "", undefined, undefined, true))
      .toEqual({ min: 5, indexed: true });
  });
  it("indexed false/omitted → no indexed key at all (absent = false, BE boolOpt)", () => {
    expect(buildOptions("text", "VND", NO_PICK, "", "", "", "", "100", "", "", "", "", undefined, undefined, false))
      .toEqual({ maxLength: 100 });
    expect(buildOptions("date", "VND", NO_PICK, "", "", "", "", "", "")).toBeUndefined();
  });
});

describe("formulaEditOptions — edit path keeps the stored formula options", () => {
  // PUT /admin/fields/:id replaces options wholesale; dropping currencyCode made the record write
  // fall back to "USD" after any Save in the inspector (even a label-only edit).
  it("keeps currencyCode and indexed, swaps only the expression", () => {
    const stored = { formula: "amount * 0.05", resultType: "currency", currencyCode: "VND", indexed: true };
    expect(formulaEditOptions(stored, "IF(amount >= 100000000, amount * 0.05, amount * 0.03)")).toEqual({
      formula: "IF(amount >= 100000000, amount * 0.05, amount * 0.03)",
      resultType: "currency", currencyCode: "VND", indexed: true,
    });
  });
  it("defaults a missing resultType to number", () => {
    expect(formulaEditOptions(undefined, "amount * 2")).toEqual({ formula: "amount * 2", resultType: "number" });
  });
});

describe("withIndexed — edit-path overlay preserves sibling options", () => {
  it("adds indexed on top of the rebuilt per-type options", () => {
    expect(withIndexed({ values: ["a", "b"] }, true, true)).toEqual({ values: ["a", "b"], indexed: true });
  });
  it("toggle OFF returns an object WITHOUT the key (so the PUT payload replaces stored options)", () => {
    expect(withIndexed({ values: ["a"], indexed: true }, true, false)).toEqual({ values: ["a"] });
  });
  it("eligible type with no other options still sends an object (date/email toggle-off must reach BE)", () => {
    expect(withIndexed(undefined, true, false)).toEqual({});
    expect(withIndexed(undefined, true, true)).toEqual({ indexed: true });
  });
  it("ineligible type passes through untouched (relation keeps its undefined-options contract)", () => {
    expect(withIndexed(undefined, false, true)).toBeUndefined();
    expect(withIndexed({ relatedEntity: "crm_accounts" }, false, false)).toEqual({ relatedEntity: "crm_accounts" });
  });
});

describe("encode/decodeDefaultValue — wire string round-trip", () => {
  it("multi_select encodes a JSON array string (BE RM-33 (b) shape); empty → \"\" (clear)", () => {
    expect(encodeDefaultValue("multi_select", "", ["a", "b"])).toBe('["a","b"]');
    expect(encodeDefaultValue("multi_select", "", [])).toBe("");
  });
  it("multi_select decode round-trips and survives junk", () => {
    expect(decodeDefaultValue("multi_select", '["a","b"]')).toEqual({ scalar: "", multi: ["a", "b"] });
    expect(decodeDefaultValue("multi_select", "not-json")).toEqual({ scalar: "", multi: [] });
    expect(decodeDefaultValue("multi_select", undefined)).toEqual({ scalar: "", multi: [] });
  });
  it("datetime pads seconds from datetime-local (BE naive layout needs :ss)", () => {
    expect(encodeDefaultValue("datetime", "2026-07-13T10:30", [])).toBe("2026-07-13T10:30:00");
    expect(encodeDefaultValue("datetime", "2026-07-13T10:30:45", [])).toBe("2026-07-13T10:30:45"); // already padded
  });
  it("datetime decode strips fraction/offset for the native input (best-effort wall-clock display)", () => {
    expect(decodeDefaultValue("datetime", "2026-07-13T10:30:00Z")).toEqual({ scalar: "2026-07-13T10:30:00", multi: [] });
    expect(decodeDefaultValue("datetime", "2026-07-13T10:30:00.500+07:00")).toEqual({ scalar: "2026-07-13T10:30:00", multi: [] });
    expect(decodeDefaultValue("datetime", "2026-07-13T10:30:00")).toEqual({ scalar: "2026-07-13T10:30:00", multi: [] });
  });
  it("scalar types trim; stored value hydrates verbatim", () => {
    expect(encodeDefaultValue("text", "  N/A  ", [])).toBe("N/A");
    expect(encodeDefaultValue("boolean", "true", [])).toBe("true");
    expect(decodeDefaultValue("picklist", "open")).toEqual({ scalar: "open", multi: [] });
    expect(decodeDefaultValue("text", undefined)).toEqual({ scalar: "", multi: [] });
  });
});

// ── pii-sensitive-audit-redact (queue #19): the "Trường nhạy cảm" flag ──
import { supportsSensitive, withSensitive } from "./fieldOptions";

describe("withSensitive — the PII flag rides the same full-replacement seam as indexed", () => {
  it("adds sensitive on top of the rebuilt per-type options", () => {
    expect(withSensitive({ maxLength: 12 }, true, true)).toEqual({ maxLength: 12, sensitive: true });
  });
  it("toggle OFF returns an object WITHOUT the key, so the PUT actually clears the flag", () => {
    expect(withSensitive({ maxLength: 12, sensitive: true }, true, false)).toEqual({ maxLength: 12 });
  });
  it("composes with withIndexed — a date/email field can carry BOTH flags", () => {
    expect(withSensitive(withIndexed(undefined, true, true), true, true))
      .toEqual({ indexed: true, sensitive: true });
  });
  it("ineligible type passes through untouched — relation/composite keep their stored structure", () => {
    // The wipe this guards: options is a FULL REPLACEMENT, so returning {sensitive:true} here
    // would send a relation field's payload without relatedEntity (queue #65's bug class).
    expect(withSensitive(undefined, false, true)).toBeUndefined();
    for (const t of ["relation", "polymorphic_relation", "composite", "formula", "lookup", "rollup", "auto_number"] as FieldType[]) {
      expect(supportsSensitive(t), t).toBe(false);
    }
  });
  it("covers the types PII actually lives in", () => {
    for (const t of ["text", "rich_text", "number", "date", "datetime", "email", "currency", "picklist", "multi_select"] as FieldType[]) {
      expect(supportsSensitive(t), t).toBe(true);
    }
  });
});

// specs/083: "Lưu giá trị" — a 1-N roll-up may keep its total in the record; N-N never sends it.
describe("buildOptions — stored rollup (specs/083)", () => {
  const base = { childEntity: "enrollments", metric: "count" as const, aggField: "", mode: "viaRelation" as const, viaRelation: "class", relationship: "" };
  const adv = (rollup: Omit<typeof base, "mode"> & { stored?: boolean; mode: "viaRelation" | "relationship" }) =>
    ({ lookup: emptyLookup(), autoNumber: emptyAutoNumber(), relation: emptyRelation(), rollup });
  it("sends stored:true for a 1-N roll-up that opts in", () => {
    expect(build("rollup", adv({ ...base, stored: true }))).toEqual({ childEntity: "enrollments", metric: "count", viaRelation: "class", stored: true });
  });
  it("omits stored when off", () => {
    expect(build("rollup", adv({ ...base, stored: false }))).not.toHaveProperty("stored");
  });
  it("never sends stored for an N-N roll-up (BE: FIELD_ROLLUP_STORED_NN_UNSUPPORTED)", () => {
    const out = build("rollup", adv({ ...base, mode: "relationship", relationship: "class_students", stored: true }))!;
    expect(out).not.toHaveProperty("stored");
  });
});

describe("buildOptions — create path keeps the Vietnamese layer (queue #274, #292)", () => {
  it("picklist: valueLabels ride along with values on create", () => {
    const rows: PicklistRow[] = [
      { id: "a", value: "kham", label: "Khám" }, { id: "b", value: "dang_hoc", label: " Đang học " }, { id: "c", value: "xong" },
    ];
    expect(buildOptions("picklist", "VND", rows, "", "", "", "", "", "")).toEqual({
      values: ["kham", "dang_hoc", "xong"], valueLabels: { kham: "Khám", dang_hoc: "Đang học" },
    });
  });
  it("picklist without labels sends no valueLabels key", () => {
    const rows: PicklistRow[] = [{ id: "a", value: "x" }];
    expect(buildOptions("multi_select", "VND", rows, "", "", "", "", "", "")).toEqual({ values: ["x"] });
  });
  const SUB_RE = /^[a-z][a-z0-9_]{0,25}$/; // ddl.go sub-field name rule
  it("composite: sub-field names are ASCII slugs that pass the server regex, labels keep the accents", () => {
    const o = buildOptions("composite", "VND", NO_PICK, "", "", "", "", "", "", "", "", "Họ:text, Tên:text, Số nhà:text, Đường:text") as { subFields: { name: string; label: string; fieldType: string }[] };
    expect(o.subFields.map((s) => s.name)).toEqual(["ho", "ten", "so_nha", "duong"]);
    expect(o.subFields.map((s) => s.label)).toEqual(["Họ", "Tên", "Số nhà", "Đường"]);
    for (const s of o.subFields) expect(s.name).toMatch(SUB_RE);
  });
  it("composite: unsluggable or duplicate names still get a valid unique name", () => {
    const o = buildOptions("composite", "VND", NO_PICK, "", "", "", "", "", "", "", "", "123:text, Tên:text, Ten:text, Một tên rất rất rất dài vượt quá hai mươi sáu ký tự:text") as { subFields: { name: string }[] };
    const names = o.subFields.map((s) => s.name);
    expect(new Set(names).size).toBe(names.length);
    for (const n of names) expect(n).toMatch(SUB_RE);
  });
});
