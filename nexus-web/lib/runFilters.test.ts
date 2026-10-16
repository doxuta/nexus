// Unit tests for lib/runFilters.ts — W4 run-overlay filter model.
// Backend contract: ?dateFrom/?dateTo/?ownerId (api-conventions §4.4), verified live 2026-07-03.

import { describe, it, expect } from "vitest";
import {
  toISODate,
  isISODate,
  presetRange,
  matchPreset,
  filtersToParams,
  parseFilters,
  countActiveFilters,
  filterChips,
  removeChip,
  formatDateVN,
  type RunFilters,
} from "./runFilters";

const sp = (entries: Record<string, string>) => ({
  get: (k: string) => (k in entries ? entries[k] : null),
});

describe("toISODate / isISODate", () => {
  it("formats local dates without UTC shift", () => {
    expect(toISODate(new Date(2026, 6, 3))).toBe("2026-07-03");
    expect(toISODate(new Date(2026, 0, 1))).toBe("2026-01-01");
  });
  it("accepts real calendar dates only", () => {
    expect(isISODate("2026-07-03")).toBe(true);
    expect(isISODate("2026-02-29")).toBe(false); // 2026 not a leap year
    expect(isISODate("2024-02-29")).toBe(true);  // 2024 is
    expect(isISODate("2026-13-01")).toBe(false);
    expect(isISODate("03/07/2026")).toBe(false);
    expect(isISODate("2026-7-3")).toBe(false);
  });
});

describe("presetRange", () => {
  const today = new Date(2026, 6, 3); // Fri 2026-07-03
  it("hôm nay = single day", () => {
    expect(presetRange("today", today)).toEqual({ dateFrom: "2026-07-03", dateTo: "2026-07-03" });
  });
  it("7 ngày qua = 7 calendar days inclusive", () => {
    expect(presetRange("7d", today)).toEqual({ dateFrom: "2026-06-27", dateTo: "2026-07-03" });
  });
  it("30 ngày qua = 30 calendar days inclusive", () => {
    expect(presetRange("30d", today)).toEqual({ dateFrom: "2026-06-04", dateTo: "2026-07-03" });
  });
  it("quý này starts at the quarter boundary", () => {
    expect(presetRange("quarter", today)).toEqual({ dateFrom: "2026-07-01", dateTo: "2026-07-03" });
    expect(presetRange("quarter", new Date(2026, 2, 15))).toEqual({ dateFrom: "2026-01-01", dateTo: "2026-03-15" });
  });
  it("năm nay starts Jan 1", () => {
    expect(presetRange("year", today)).toEqual({ dateFrom: "2026-01-01", dateTo: "2026-07-03" });
  });
  it("7d crosses a month boundary correctly", () => {
    expect(presetRange("7d", new Date(2026, 7, 2))).toEqual({ dateFrom: "2026-07-27", dateTo: "2026-08-02" });
  });
});

describe("matchPreset", () => {
  const today = new Date(2026, 6, 3);
  it("recognises an exact preset match", () => {
    expect(matchPreset({ dateFrom: "2026-06-27", dateTo: "2026-07-03" }, today)).toBe("7d");
    expect(matchPreset({ dateFrom: "2026-01-01", dateTo: "2026-07-03" }, today)).toBe("year");
  });
  it("returns null for custom ranges or partial filters", () => {
    expect(matchPreset({ dateFrom: "2026-06-01", dateTo: "2026-07-03" }, today)).toBeNull();
    expect(matchPreset({ dateFrom: "2026-06-27" }, today)).toBeNull();
    expect(matchPreset({}, today)).toBeNull();
  });
});

describe("filtersToParams / parseFilters (URL sync round-trip)", () => {
  it("serialises only active filters", () => {
    expect(filtersToParams({})).toEqual({});
    expect(filtersToParams({ dateFrom: "2026-01-01", dateTo: "2026-07-03", ownerId: "u1" })).toEqual({
      dateFrom: "2026-01-01",
      dateTo: "2026-07-03",
      ownerId: "u1",
    });
  });
  it("round-trips through a URLSearchParams-like source", () => {
    const f: RunFilters = { dateFrom: "2026-01-01", dateTo: "2026-07-03", ownerId: "u-9" };
    expect(parseFilters(sp(filtersToParams(f)))).toEqual(f);
  });
  it("drops malformed dates instead of forwarding them to the API", () => {
    expect(parseFilters(sp({ dateFrom: "garbage", dateTo: "2026-07-03" }))).toEqual({ dateTo: "2026-07-03" });
    expect(parseFilters(sp({ dateFrom: "2026-02-30" }))).toEqual({});
  });
  it("drops an inverted pair (would 422 RUN_DATE_RANGE_INVALID)", () => {
    expect(parseFilters(sp({ dateFrom: "2026-07-03", dateTo: "2026-01-01" }))).toEqual({});
  });
  it("trims ownerId and drops empty", () => {
    expect(parseFilters(sp({ ownerId: "  u1  " }))).toEqual({ ownerId: "u1" });
    expect(parseFilters(sp({ ownerId: "   " }))).toEqual({});
  });
});

describe("countActiveFilters", () => {
  it("counts the date pair as ONE filter", () => {
    expect(countActiveFilters({})).toBe(0);
    expect(countActiveFilters({ dateFrom: "2026-01-01", dateTo: "2026-07-03" })).toBe(1);
    expect(countActiveFilters({ dateFrom: "2026-01-01", dateTo: "2026-07-03", ownerId: "u" })).toBe(2);
    expect(countActiveFilters({ ownerId: "u" })).toBe(1);
  });
});

describe("filterChips / removeChip", () => {
  it("collapses the date range into one chip", () => {
    const chips = filterChips({ dateFrom: "2026-01-01", dateTo: "2026-07-03" });
    expect(chips).toEqual([{ kind: "date", label: "01/01/2026 – 03/07/2026" }]);
  });
  it("single-day range shows one date", () => {
    expect(filterChips({ dateFrom: "2026-07-03", dateTo: "2026-07-03" })[0].label).toBe("03/07/2026");
  });
  it("half-open bounds get Từ/Đến prefixes", () => {
    expect(filterChips({ dateFrom: "2026-01-01" })[0].label).toBe("Từ 01/01/2026");
    expect(filterChips({ dateTo: "2026-07-03" })[0].label).toBe("Đến 03/07/2026");
  });
  it("owner chip uses the resolved label when given", () => {
    expect(filterChips({ ownerId: "u1" }, "Chỉ của tôi")).toEqual([{ kind: "owner", label: "Chỉ của tôi" }]);
  });
  it("removeChip clears the whole date pair / the owner, immutably", () => {
    const f: RunFilters = { dateFrom: "2026-01-01", dateTo: "2026-07-03", ownerId: "u1" };
    expect(removeChip(f, "date")).toEqual({ ownerId: "u1" });
    expect(removeChip(f, "owner")).toEqual({ dateFrom: "2026-01-01", dateTo: "2026-07-03" });
    expect(f.ownerId).toBe("u1"); // original untouched
  });
});

describe("formatDateVN", () => {
  it("renders dd/MM/yyyy", () => {
    expect(formatDateVN("2026-07-03")).toBe("03/07/2026");
  });
  it("falls back to the raw string for garbage", () => {
    expect(formatDateVN("oops")).toBe("oops");
  });
});
