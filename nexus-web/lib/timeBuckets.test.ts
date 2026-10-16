// Unit tests for lib/timeBuckets.ts — key formats mirror the LIVE backend output
// (curl-verified 2026-07-03): day "2026-07-02" · week "2026-W27" (ISO %x-W%v) ·
// month "2026-07" · quarter "2026-Q3" · year "2026".

import { describe, it, expect } from "vitest";
import {
  isBucketId,
  isBucketKey,
  dateToBucketKey,
  nextBucketKey,
  bucketAxisLabel,
  zeroFillGroups,
  bucketSeriesToChartData,
} from "./timeBuckets";
import type { AggGroupRow } from "./api";

const g = (key: string, value: string, count = 1): AggGroupRow => ({ key, value, count });

describe("isBucketId", () => {
  it("accepts the API token set only", () => {
    for (const b of ["day", "week", "month", "quarter", "year"]) expect(isBucketId(b)).toBe(true);
    expect(isBucketId("hour")).toBe(false);
    expect(isBucketId("")).toBe(false);
    expect(isBucketId(undefined)).toBe(false);
  });
});

describe("isBucketKey", () => {
  it("validates per-bucket key shapes", () => {
    expect(isBucketKey("2026-07-02", "day")).toBe(true);
    expect(isBucketKey("2026-W27", "week")).toBe(true);
    expect(isBucketKey("2026-07", "month")).toBe(true);
    expect(isBucketKey("2026-Q3", "quarter")).toBe(true);
    expect(isBucketKey("2026", "year")).toBe(true);
    expect(isBucketKey("2026-07", "day")).toBe(false);
    expect(isBucketKey(null, "day")).toBe(false);
    expect(isBucketKey("", "month")).toBe(false); // NULL-date bucket from API
  });
});

describe("dateToBucketKey", () => {
  it("maps a calendar date into each bucket", () => {
    expect(dateToBucketKey("2026-07-03", "day")).toBe("2026-07-03");
    expect(dateToBucketKey("2026-07-03", "month")).toBe("2026-07");
    expect(dateToBucketKey("2026-07-03", "quarter")).toBe("2026-Q3");
    expect(dateToBucketKey("2026-07-03", "year")).toBe("2026");
  });
  it("ISO week matches MySQL %x-W%v (Monday-first, ISO year)", () => {
    expect(dateToBucketKey("2026-07-02", "week")).toBe("2026-W27"); // verified live
    expect(dateToBucketKey("2026-01-01", "week")).toBe("2026-W01"); // Thu 2026-01-01 → W1
    expect(dateToBucketKey("2027-01-01", "week")).toBe("2026-W53"); // Fri → belongs to ISO year 2026
    expect(dateToBucketKey("2024-12-30", "week")).toBe("2025-W01"); // Mon → ISO year 2025
  });
  it("rejects garbage", () => {
    expect(dateToBucketKey("garbage", "day")).toBeNull();
    expect(dateToBucketKey("2026-13-40", "month")).toBeNull();
  });
});

describe("nextBucketKey", () => {
  it("advances day across month/year boundaries", () => {
    expect(nextBucketKey("2026-07-02", "day")).toBe("2026-07-03");
    expect(nextBucketKey("2026-06-30", "day")).toBe("2026-07-01");
    expect(nextBucketKey("2026-12-31", "day")).toBe("2027-01-01");
    expect(nextBucketKey("2024-02-28", "day")).toBe("2024-02-29"); // leap
  });
  it("advances week across the 52/53-week ISO year boundary", () => {
    expect(nextBucketKey("2026-W27", "week")).toBe("2026-W28");
    expect(nextBucketKey("2026-W53", "week")).toBe("2027-W01"); // 2026 has 53 ISO weeks
    expect(nextBucketKey("2025-W52", "week")).toBe("2026-W01"); // 2025 has 52
  });
  it("advances month / quarter / year", () => {
    expect(nextBucketKey("2026-11", "month")).toBe("2026-12");
    expect(nextBucketKey("2026-12", "month")).toBe("2027-01");
    expect(nextBucketKey("2026-Q3", "quarter")).toBe("2026-Q4");
    expect(nextBucketKey("2026-Q4", "quarter")).toBe("2027-Q1");
    expect(nextBucketKey("2026", "year")).toBe("2027");
  });
  it("returns null for malformed keys", () => {
    expect(nextBucketKey("garbage", "day")).toBeNull();
  });
});

describe("bucketAxisLabel (plan D formats)", () => {
  it("day → dd/MM", () => {
    expect(bucketAxisLabel("2026-07-03", "day")).toBe("03/07");
    expect(bucketAxisLabel("2026-07-03", "day", true)).toBe("03/07/2026");
  });
  it("week → 'Tuần w'", () => {
    expect(bucketAxisLabel("2026-W27", "week")).toBe("Tuần 27");
    expect(bucketAxisLabel("2026-W05", "week")).toBe("Tuần 5");
    expect(bucketAxisLabel("2026-W27", "week", true)).toBe("Tuần 27/2026");
  });
  it("month → MM/yyyy", () => {
    expect(bucketAxisLabel("2026-07", "month")).toBe("07/2026");
  });
  it("quarter → 'Qq yyyy'", () => {
    expect(bucketAxisLabel("2026-Q3", "quarter")).toBe("Q3 2026");
  });
  it("year → yyyy", () => {
    expect(bucketAxisLabel("2026", "year")).toBe("2026");
  });
  it("never throws on a malformed key — returns it raw", () => {
    expect(bucketAxisLabel("(null)", "month")).toBe("(null)");
  });
});

describe("zeroFillGroups", () => {
  it("fills gaps between min and max keys with value '0'", () => {
    const out = zeroFillGroups([g("2026-07", "5"), g("2026-09", "3")], "month");
    expect(out.map((x) => x.key)).toEqual(["2026-07", "2026-08", "2026-09"]);
    expect(out[1]).toEqual({ key: "2026-08", value: "0", count: 0 });
  });
  it("keeps a dense series untouched (chronological)", () => {
    const out = zeroFillGroups([g("2026-Q1", "1"), g("2026-Q2", "2")], "quarter");
    expect(out.map((x) => x.key)).toEqual(["2026-Q1", "2026-Q2"]);
  });
  it("widens to the overlay date range when given", () => {
    const out = zeroFillGroups([g("2026-07-02", "4")], "day", { from: "2026-06-30", to: "2026-07-03" });
    expect(out.map((x) => x.key)).toEqual(["2026-06-30", "2026-07-01", "2026-07-02", "2026-07-03"]);
    expect(out[2].value).toBe("4");
  });
  it("drops NULL/malformed keys (no place on a time axis)", () => {
    const out = zeroFillGroups([g("", "9"), g("2026-07", "5")], "month");
    expect(out).toEqual([g("2026-07", "5")]);
  });
  it("fills across ISO week year boundary", () => {
    const out = zeroFillGroups([g("2026-W52", "1"), g("2027-W02", "2")], "week");
    expect(out.map((x) => x.key)).toEqual(["2026-W52", "2026-W53", "2027-W01", "2027-W02"]);
  });
  it("falls back to the sparse chronological series when the range exceeds the cap", () => {
    const out = zeroFillGroups([g("2020-01-01", "1"), g("2026-07-02", "2")], "day", { cap: 100 });
    expect(out).toEqual([g("2020-01-01", "1"), g("2026-07-02", "2")]);
  });
  it("returns [] for empty/only-null input", () => {
    expect(zeroFillGroups([], "day")).toEqual([]);
    expect(zeroFillGroups([g("", "1")], "day")).toEqual([]);
  });
});

describe("bucketSeriesToChartData", () => {
  it("labels + zero-fills + stays chronological (NOT value-desc)", () => {
    const pts = bucketSeriesToChartData([g("2026-05", "10"), g("2026-07", "99")], "month");
    expect(pts).toEqual([
      { key: "05/2026", value: 10, count: 1 },
      { key: "06/2026", value: 0, count: 0 },
      { key: "07/2026", value: 99, count: 1 },
    ]);
  });
  it("switches day/week labels to the long variant when spanning years", () => {
    const pts = bucketSeriesToChartData([g("2026-W52", "1"), g("2027-W01", "2")], "week");
    expect(pts.map((p) => p.key)).toEqual(["Tuần 52/2026", "Tuần 53/2026", "Tuần 1/2027"]);
  });
});
