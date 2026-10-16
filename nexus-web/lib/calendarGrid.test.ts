import { describe, expect, it } from "vitest";
import {
  addDays, assignLanes, eventWhen, hourWindow, rangeFilter, toWire, weekStart,
} from "@/lib/calendarGrid";

describe("calendarGrid", () => {
  it("reads a datetime in Vietnam time, not UTC (a 20:00Z booking is 03:00 the NEXT day in VN)", () => {
    expect(eventWhen("2026-10-08T02:30:00Z", "datetime")).toEqual({ day: "2026-10-08", minutes: 9 * 60 + 30 });
    expect(eventWhen("2026-10-08T20:00:00Z", "datetime")).toEqual({ day: "2026-10-09", minutes: 3 * 60 });
    expect(eventWhen("2026-10-08 02:30:00", "datetime")).toEqual({ day: "2026-10-08", minutes: 9 * 60 + 30 });
  });
  it("a date field has no clock", () => {
    expect(eventWhen("2026-10-08", "date")).toEqual({ day: "2026-10-08", minutes: null });
    expect(eventWhen(null, "date")).toBeNull();
    expect(eventWhen("junk", "datetime")).toBeNull();
  });
  it("range filter covers the whole last day for datetime (bare end date would cut it at 00:00)", () => {
    expect(rangeFilter("date", "2026-10-01", "2026-10-31")).toBe("2026-10-01,2026-10-31");
    expect(rangeFilter("datetime", "2026-10-05", "2026-10-11")).toBe("2026-10-04T17:00:00Z,2026-10-11T16:59:59Z");
  });
  it("wire value is VN local with offset; date is the bare day", () => {
    expect(toWire("datetime", "2026-10-08", 9 * 60 + 30)).toBe("2026-10-08T09:30:00+07:00");
    expect(toWire("date", "2026-10-08", 600)).toBe("2026-10-08");
  });
  it("weeks start on Monday; addDays crosses months", () => {
    expect(weekStart("2026-10-08")).toBe("2026-10-05"); // Thu
    expect(weekStart("2026-10-11")).toBe("2026-10-05"); // Sun
    expect(addDays("2026-10-30", 3)).toBe("2026-11-02");
  });
  it("hour window defaults to office hours and widens to hold early/late events", () => {
    expect(hourWindow([])).toEqual([7, 19]);
    expect(hourWindow([6 * 60 + 10, 20 * 60 + 5])).toEqual([6, 21]);
  });
  it("overlapping events go in side-by-side lanes", () => {
    const l = assignLanes([{ id: "a", minutes: 540 }, { id: "b", minutes: 550 }, { id: "c", minutes: 600 }]);
    expect(l.get("a")).toEqual({ lane: 0, lanes: 2 });
    expect(l.get("b")).toEqual({ lane: 1, lanes: 2 });
    expect(l.get("c")).toEqual({ lane: 0, lanes: 1 });
  });
});
