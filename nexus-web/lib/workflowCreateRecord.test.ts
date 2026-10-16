import { describe, it, expect } from "vitest";
import type { FieldMeta } from "@/lib/api";
import {
  crValuePairs, crPairsToString, crEmitValues, crHydrateRows,
  dmyToIso, isoToDmy, dmyTimeToIso, isoTimeToDmy, OWNER_SENTINEL,
} from "./workflowCreateRecord";

const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({
  name, fieldType, ...extra,
});

// A representative target entity (mixes the types #80 was breaking + the ones that already worked).
const fields: FieldMeta[] = [
  f("subject", "text"),
  f("amount", "currency", { options: { currencyCode: "USD" } }),
  f("qty", "number"),
  f("done", "boolean"),
  f("stars", "rating"),
  f("stage", "picklist", { options: { values: ["new", "won"] } }),
  f("owner", "relation", { options: { relatedEntity: "users" } }),
  f("due", "date"),
  f("remind_at", "datetime"),
];

describe("crValuePairs / crPairsToString", () => {
  it("round-trips typed rows through the params JSON string", () => {
    const rows = crPairsToString([["qty", 3], ["done", true], ["amount", "10.5"]]);
    expect(crValuePairs(rows)).toEqual([["qty", 3], ["done", true], ["amount", "10.5"]]);
  });
  it("malformed / empty → []", () => {
    expect(crValuePairs(undefined)).toEqual([]);
    expect(crValuePairs("{not json")).toEqual([]);
  });
  it("widens legacy [string,string] pairs unharmed", () => {
    expect(crValuePairs(JSON.stringify([["subject", "Hi"]]))).toEqual([["subject", "Hi"]]);
  });
});

describe("date converters (isolated to create_record)", () => {
  it("dmyToIso: widget DD/MM/YYYY → backend YYYY-MM-DD (no UTC day-shift)", () => {
    expect(dmyToIso("15/01/2026")).toBe("2026-01-15");
    expect(dmyToIso("1/2/2026")).toBe("2026-02-01");   // zero-pads
    expect(dmyToIso("2026-01-15")).toBe("2026-01-15"); // already ISO → passthrough
    expect(dmyToIso("")).toBe("");
  });
  it("isoToDmy: backend YYYY-MM-DD → widget DD/MM/YYYY (hydration highlights the calendar)", () => {
    expect(isoToDmy("2026-01-15")).toBe("15/01/2026");
    expect(isoToDmy("15/01/2026")).toBe("15/01/2026"); // already DMY → passthrough
  });
  it("dmyTimeToIso: widget 'DD/MM/YYYY · HH:mm' → 'YYYY-MM-DDTHH:mm:00'", () => {
    expect(dmyTimeToIso("15/01/2026 · 09:30")).toBe("2026-01-15T09:30:00");
    expect(dmyTimeToIso("15/01/2026")).toBe("2026-01-15T00:00:00"); // no time → midnight
  });
  it("isoTimeToDmy: RFC3339 or MySQL literal → widget display", () => {
    expect(isoTimeToDmy("2026-01-15T09:30:00Z")).toBe("15/01/2026 · 09:30");
    expect(isoTimeToDmy("2026-01-15 09:30:00")).toBe("15/01/2026 · 09:30");
  });
});

describe("crEmitValues — the #80 fix (correctly-typed wire values)", () => {
  it("number/currency/boolean/rating emit as the RIGHT type, not strings", () => {
    const out = crEmitValues([
      ["qty", 3],
      ["amount", "10.5"],
      ["done", true],
      ["stars", 4],
    ], fields);
    expect(out).toEqual({
      qty: 3,                                        // number, not "3"
      amount: { amount: 10.5, currencyCode: "USD" }, // currency object, not "10.5"
      done: true,                                    // bool, not "true"
      stars: 4,                                      // number, not "4"
    });
  });
  it("boolean false is KEPT (not dropped as empty)", () => {
    expect(crEmitValues([["done", false]], fields)).toEqual({ done: false });
  });
  it("date/datetime emit backend ISO (DD/MM → YYYY-MM-DD, RFC3339-local)", () => {
    const out = crEmitValues([["due", "15/01/2026"], ["remind_at", "15/01/2026 · 09:30"]], fields);
    expect(out).toEqual({ due: "2026-01-15", remind_at: "2026-01-15T09:30:00" });
  });
  it("text/picklist/relation pass through as strings", () => {
    expect(crEmitValues([["subject", "Hi"], ["stage", "won"], ["owner", "u-9"]], fields))
      .toEqual({ subject: "Hi", stage: "won", owner: "u-9" });
  });
  it("$owner sentinel survives verbatim for a relation target", () => {
    expect(crEmitValues([["owner", OWNER_SENTINEL]], fields)).toEqual({ owner: "$owner" });
  });
  it("blank field rows and empty optionals are dropped", () => {
    expect(crEmitValues([["", "x"], ["subject", ""], ["qty", ""]], fields)).toEqual({});
  });
  it("unknown field (metadata not loaded) → non-destructive passthrough", () => {
    expect(crEmitValues([["ghost", "keepme"]], [])).toEqual({ ghost: "keepme" });
  });
});

describe("crHydrateRows — round-trip a stored rule back into typed inputs (item #5)", () => {
  it("no stale-string downgrade: each type reverses to its widget-native value", () => {
    const rows = crHydrateRows({
      qty: 3,
      amount: { amount: 10.5, currencyCode: "USD" },
      done: true,
      due: "2026-01-15",
      remind_at: "2026-01-15T09:30:00Z",
      owner: OWNER_SENTINEL,
      subject: "Hi",
    }, fields);
    const asObj = Object.fromEntries(rows);
    expect(asObj.qty).toBe(3);
    expect(asObj.amount).toBe("10.5");           // currency object → decimal string (widget-native)
    expect(asObj.done).toBe(true);
    expect(asObj.due).toBe("15/01/2026");        // ISO → DD/MM so the picker highlights
    expect(asObj.remind_at).toBe("15/01/2026 · 09:30");
    expect(asObj.owner).toBe(OWNER_SENTINEL);
    expect(asObj.subject).toBe("Hi");
  });

  it("hydrate → emit is stable (re-saving an untouched rule does not corrupt values)", () => {
    const stored = {
      qty: 3,
      amount: { amount: 10.5, currencyCode: "USD" },
      done: false,
      due: "2026-01-15",
      remind_at: "2026-01-15T09:30:00",
      owner: OWNER_SENTINEL,
    };
    const rehydrated = crEmitValues(crHydrateRows(stored, fields), fields);
    expect(rehydrated).toEqual(stored);
  });
});
