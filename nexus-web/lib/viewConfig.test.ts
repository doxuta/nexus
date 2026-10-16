// viewConfig — pure-logic unit tests for the W9c saved-views model.
// Covers: serialize→parse→apply round-trip (filters/sort/density/columns), the unknown-version
// fallback, structural dirty-detection (viewConfigEqual), and the ownership/visibility rules that
// mirror the backend ViewService (shared=admin-only, personal=owner-only).

import { describe, it, expect } from "vitest";
import type { FilterRow } from "./filterModel";
import {
  type ViewState,
  DEFAULT_VIEW_CONFIG,
  serializeViewConfig,
  parseViewConfig,
  applyViewConfig,
  viewConfigEqual,
  canMutateView,
  canCreateShared,
} from "./viewConfig";

const fr = (field: string, op: string, value = "", id = "x"): FilterRow => ({ id, field, op, value });

const sampleState: ViewState = {
  filters: [fr("stage", "eq", "Proposal", "1"), fr("amount", "gte", "1000", "2")],
  sort: { field: "created_at", order: "asc" },
  density: "compact",
  hiddenColumns: ["notes", "phone"],
};

// ── serialize ────────────────────────────────────────────────────────────────────────
describe("serializeViewConfig", () => {
  it("captures filters (sans UI id) / sort / density / hidden columns at v:1", () => {
    const cfg = serializeViewConfig(sampleState);
    expect(cfg.v).toBe(1);
    expect(cfg.filters).toEqual([
      { field: "stage", op: "eq", value: "Proposal" },
      { field: "amount", op: "gte", value: "1000" },
    ]);
    expect(cfg.sort).toEqual({ field: "created_at", order: "asc" });
    expect(cfg.density).toBe("compact");
    expect(cfg.columns.hidden).toEqual(["notes", "phone"]);
  });

  it("drops incomplete filter rows (missing field or op)", () => {
    const cfg = serializeViewConfig({
      ...sampleState,
      filters: [fr("stage", "eq", "X"), fr("", "eq", "Y"), fr("amount", "", "Z")],
    });
    expect(cfg.filters).toEqual([{ field: "stage", op: "eq", value: "X" }]);
  });

  it("coerces an invalid density to normal", () => {
    const cfg = serializeViewConfig({ ...sampleState, density: "huge" as never });
    expect(cfg.density).toBe("normal");
  });
});

// ── round-trip: serialize → parse → apply ─────────────────────────────────────────────
describe("round-trip serialize → parse → apply", () => {
  it("restores the same logical state (filters get fresh ids)", () => {
    const cfg = serializeViewConfig(sampleState);
    // simulate the server storing/returning the config object verbatim (JSON round-trip)
    const stored = JSON.parse(JSON.stringify(cfg));
    const parsed = parseViewConfig(stored);
    const applied = applyViewConfig(parsed);

    expect(applied.sort).toEqual(sampleState.sort);
    expect(applied.density).toBe(sampleState.density);
    expect(applied.hiddenColumns).toEqual(sampleState.hiddenColumns);
    // filters equal on the meaningful triple; ids are re-generated, not preserved
    expect(applied.filters.map((f) => ({ field: f.field, op: f.op, value: f.value }))).toEqual(
      sampleState.filters.map((f) => ({ field: f.field, op: f.op, value: f.value })),
    );
    for (const f of applied.filters) expect(f.id).toBeTruthy();
  });

  it("re-serialising the applied state equals the original config (idempotent)", () => {
    const cfg = serializeViewConfig(sampleState);
    const back = serializeViewConfig(applyViewConfig(parseViewConfig(cfg)));
    expect(viewConfigEqual(cfg, back)).toBe(true);
  });
});

// ── parse: defensive + unknown-version fallback ───────────────────────────────────────
describe("parseViewConfig — defensive + unknown-version fallback", () => {
  it("unknown version → DEFAULT_VIEW_CONFIG", () => {
    expect(parseViewConfig({ v: 2, filters: [{ field: "a", op: "eq", value: "b" }] })).toEqual(
      DEFAULT_VIEW_CONFIG,
    );
  });

  it("missing version → DEFAULT_VIEW_CONFIG", () => {
    expect(parseViewConfig({ filters: [] })).toEqual(DEFAULT_VIEW_CONFIG);
  });

  it("non-object (null / array / scalar) → DEFAULT_VIEW_CONFIG", () => {
    expect(parseViewConfig(null)).toEqual(DEFAULT_VIEW_CONFIG);
    expect(parseViewConfig([1, 2])).toEqual(DEFAULT_VIEW_CONFIG);
    expect(parseViewConfig("nope")).toEqual(DEFAULT_VIEW_CONFIG);
  });

  it("v:1 with garbage sub-fields → sanitised, never throws", () => {
    const parsed = parseViewConfig({
      v: 1,
      filters: [{ field: "ok", op: "eq", value: "1" }, { field: 5, op: "eq" }, "junk", null],
      sort: { field: "x", order: "sideways" },
      density: "enormous",
      columns: { hidden: ["keep", 9, null] },
    });
    expect(parsed.filters).toEqual([{ field: "ok", op: "eq", value: "1" }]);
    expect(parsed.sort).toBeNull(); // invalid order dropped
    expect(parsed.density).toBe("normal"); // invalid density coerced
    expect(parsed.columns.hidden).toEqual(["keep"]); // non-strings dropped
  });

  it("fills a missing value with empty string", () => {
    const parsed = parseViewConfig({ v: 1, filters: [{ field: "a", op: "isnull" }] });
    expect(parsed.filters).toEqual([{ field: "a", op: "isnull", value: "" }]);
  });
});

// ── dirty detection ────────────────────────────────────────────────────────────────────
describe("viewConfigEqual — dirty detection", () => {
  const base = serializeViewConfig(sampleState);

  it("equal to itself", () => {
    expect(viewConfigEqual(base, { ...base })).toBe(true);
  });

  it("hidden columns compare as a set (order-insensitive)", () => {
    const reordered = serializeViewConfig({ ...sampleState, hiddenColumns: ["phone", "notes"] });
    expect(viewConfigEqual(base, reordered)).toBe(true);
  });

  it("a changed density is dirty", () => {
    expect(viewConfigEqual(base, serializeViewConfig({ ...sampleState, density: "relaxed" }))).toBe(false);
  });

  it("a changed sort order is dirty", () => {
    expect(
      viewConfigEqual(base, serializeViewConfig({ ...sampleState, sort: { field: "created_at", order: "desc" } })),
    ).toBe(false);
  });

  it("a changed filter value is dirty", () => {
    const changed = serializeViewConfig({
      ...sampleState,
      filters: [fr("stage", "eq", "Won", "1"), fr("amount", "gte", "1000", "2")],
    });
    expect(viewConfigEqual(base, changed)).toBe(false);
  });

  it("filter order is significant (dirty when swapped)", () => {
    const swapped = serializeViewConfig({
      ...sampleState,
      filters: [fr("amount", "gte", "1000", "2"), fr("stage", "eq", "Proposal", "1")],
    });
    expect(viewConfigEqual(base, swapped)).toBe(false);
  });

  it("null vs non-null sort is dirty", () => {
    expect(viewConfigEqual(base, serializeViewConfig({ ...sampleState, sort: null }))).toBe(false);
  });
});

// ── ownership / visibility (mirrors backend ViewService) ──────────────────────────────
describe("canMutateView + canCreateShared", () => {
  const admin = { userId: "admin-1", isAdmin: true };
  const u1 = { userId: "u1", isAdmin: false };
  const u2 = { userId: "u2", isAdmin: false };

  it("shared view: only admin may mutate", () => {
    const shared = { userId: null, shared: true };
    expect(canMutateView(shared, admin)).toBe(true);
    expect(canMutateView(shared, u1)).toBe(false);
  });

  it("personal view: only its owner may mutate", () => {
    const owned = { userId: "u1", shared: false };
    expect(canMutateView(owned, u1)).toBe(true);
    expect(canMutateView(owned, u2)).toBe(false);
    expect(canMutateView(owned, admin)).toBe(false); // admin is not the owner of a personal view
  });

  it("only admin may create a shared view", () => {
    expect(canCreateShared(admin)).toBe(true);
    expect(canCreateShared(u1)).toBe(false);
  });

  it("undefined viewer id can never own a personal view", () => {
    expect(canMutateView({ userId: "u1", shared: false }, { userId: undefined, isAdmin: false })).toBe(false);
  });
});
