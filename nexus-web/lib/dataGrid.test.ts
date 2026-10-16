// lib/dataGrid.test.ts — W9a DataGrid pure-helper contract:
//   1. buildGridTemplate — the LayoutList column-width behavior must survive the swap
//      VERBATIM (lead 40px slot, first column fluid 1.6fr, declared widths, 140 fallback)
//      with resize as the ONLY new visible change (resized column → hard px).
//   2. resolveRowHeightPx — density token stays the source of truth (var read at runtime).
//   3. load/save column widths — per-user + per-grid isolation, junk tolerance.

import { describe, it, expect, beforeEach } from "vitest";
import {
  buildGridTemplate,
  resolveRowHeightPx,
  gridWidthStorageKey,
  loadColWidths,
  saveColWidths,
} from "@/lib/dataGrid";

describe("buildGridTemplate", () => {
  const cols = [
    { id: "name" },              // first — fluid
    { id: "amount", width: 120 },
    { id: "stage" },             // no width → 140 fallback
  ];

  it("preserves the LayoutList default template: lead slot + fluid first + declared widths", () => {
    expect(buildGridTemplate(cols, {}, 40)).toBe(
      "40px minmax(0,1.6fr) minmax(0,120px) minmax(0,140px)",
    );
  });

  it("omits the lead slot when not selectable", () => {
    expect(buildGridTemplate(cols, {})).toBe(
      "minmax(0,1.6fr) minmax(0,120px) minmax(0,140px)",
    );
  });

  it("a user-resized column becomes a hard px width (rounded)", () => {
    expect(buildGridTemplate(cols, { amount: 213.6 }, 40)).toBe(
      "40px minmax(0,1.6fr) 214px minmax(0,140px)",
    );
  });

  it("resizing the FIRST column replaces the fluid track", () => {
    expect(buildGridTemplate(cols, { name: 320 }, 40)).toBe(
      "40px 320px minmax(0,120px) minmax(0,140px)",
    );
  });

  it("ignores junk sizing values (0, negative, NaN) → defaults hold", () => {
    expect(buildGridTemplate(cols, { name: 0, amount: -5, stage: NaN }, 40)).toBe(
      "40px minmax(0,1.6fr) minmax(0,120px) minmax(0,140px)",
    );
  });

  it("empty columns → only the lead slot (or empty string)", () => {
    expect(buildGridTemplate([], {}, 40)).toBe("40px");
    expect(buildGridTemplate([], {})).toBe("");
  });
});

describe("resolveRowHeightPx", () => {
  it("resolves a density token through the var reader", () => {
    const read = (name: string) => (name === "--row-compact" ? "36px" : "");
    expect(resolveRowHeightPx("var(--row-compact)", read)).toBe(36);
  });

  it("passes through a plain px length without reading vars", () => {
    expect(resolveRowHeightPx("60px", () => { throw new Error("must not read"); })).toBe(60);
  });

  it("falls back to 48 (cozy) when the var is empty/garbage", () => {
    expect(resolveRowHeightPx("var(--row-comfy)", () => "")).toBe(48);
    expect(resolveRowHeightPx("var(--row-comfy)", () => "abc")).toBe(48);
    expect(resolveRowHeightPx("nonsense", () => "")).toBe(48);
  });
});

describe("column width persistence", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("round-trips widths per grid + per user", () => {
    saveColWidths("list:crm_accounts", { name: 320, amount: 120 }, "user-1");
    expect(loadColWidths("list:crm_accounts", "user-1")).toEqual({ name: 320, amount: 120 });
    // another user on the same browser sees nothing (per-user namespacing)
    expect(loadColWidths("list:crm_accounts", "user-2")).toEqual({});
    // another grid for the same user sees nothing
    expect(loadColWidths("list:crm_contacts", "user-1")).toEqual({});
  });

  it("saving an empty record REMOVES the key (reset-all keeps storage clean)", () => {
    saveColWidths("g", { a: 100 }, "u");
    saveColWidths("g", {}, "u");
    expect(window.localStorage.getItem(gridWidthStorageKey("g", "u"))).toBeNull();
  });

  it("filters junk entries and survives corrupt JSON", () => {
    window.localStorage.setItem(
      gridWidthStorageKey("g", "u"),
      JSON.stringify({ ok: 200, zero: 0, neg: -4, str: "x" }),
    );
    expect(loadColWidths("g", "u")).toEqual({ ok: 200 });

    window.localStorage.setItem(gridWidthStorageKey("g", "u"), "{not json");
    expect(loadColWidths("g", "u")).toEqual({});

    window.localStorage.setItem(gridWidthStorageKey("g", "u"), JSON.stringify([1, 2]));
    expect(loadColWidths("g", "u")).toEqual({});
  });
});
