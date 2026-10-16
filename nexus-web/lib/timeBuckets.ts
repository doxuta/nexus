// lib/timeBuckets.ts — W4 time-series bucketing helpers (FE side of api-conventions §4.3).
// Backend key text verified live 2026-07-03 (MySQL DATE_FORMAT):
//   day     "2026-07-02"   (%Y-%m-%d)
//   week    "2026-W27"     (%x-W%v — ISO-8601, Monday-first, zero-padded, ISO year)
//   month   "2026-07"      (%Y-%m)
//   quarter "2026-Q3"      (CONCAT(YEAR, '-Q', QUARTER))
//   year    "2026"         (%Y)
// Empty buckets are OMITTED by the server (no zero-fill server-side) and bucketed groups
// arrive in CHRONOLOGICAL order (agg_key ASC) — this module densifies the series for the
// time axis and formats axis labels. PURE: no React, no fetch.

import type { AggGroupRow } from "./api";
import type { ChartSeriesPoint } from "./analytics";

export type BucketId = "day" | "week" | "month" | "quarter" | "year";

export const VALID_BUCKETS: readonly BucketId[] = ["day", "week", "month", "quarter", "year"];

export function isBucketId(s: unknown): s is BucketId {
  return typeof s === "string" && (VALID_BUCKETS as readonly string[]).includes(s);
}

// ── Bucket AUTHORING (Studio builders — Report/Dashboard) ────────────────────

/** Options for the builder bucket <select>; "" = no time bucketing (plain groupBy). */
export const BUCKET_CHOICES: ReadonlyArray<{ value: "" | BucketId; label: string }> = [
  { value: "", label: "Không gom" },
  { value: "day", label: "Ngày" },
  { value: "week", label: "Tuần" },
  { value: "month", label: "Tháng" },
  { value: "quarter", label: "Quý" },
  { value: "year", label: "Năm" },
];

/**
 * Mirrors backend isBucketableGroupBy (record_aggregate.go): a time bucket may ride ONLY on
 * the system timestamps created_at/updated_at or on a declared date/datetime field. Anything
 * else fails at run time with 422 AGGREGATE_BUCKET_FIELD_NOT_DATE, so the builders gate the
 * bucket selector — and the saved payload — on this check.
 */
export function isBucketableGroupBy(
  groupBy: string | undefined,
  fields: ReadonlyArray<{ name: string; fieldType: string }>,
): boolean {
  if (!groupBy) return false;
  if (groupBy === "created_at" || groupBy === "updated_at") return true;
  const f = fields.find((x) => x.name === groupBy);
  return !!f && (f.fieldType === "date" || f.fieldType === "datetime");
}

// ── ISO-8601 week arithmetic (UTC-based — date-only math, no DST drift) ──────

/** Monday (UTC) of ISO week `week` in ISO year `isoYear`. Jan 4 is always in week 1. */
function mondayOfISOWeek(isoYear: number, week: number): Date {
  const jan4 = new Date(Date.UTC(isoYear, 0, 4));
  const jan4Dow = (jan4.getUTCDay() + 6) % 7; // Mon=0
  const mondayW1 = new Date(jan4);
  mondayW1.setUTCDate(jan4.getUTCDate() - jan4Dow);
  const monday = new Date(mondayW1);
  monday.setUTCDate(mondayW1.getUTCDate() + (week - 1) * 7);
  return monday;
}

/** ISO week key ("2026-W27") for a UTC date — mirrors MySQL %x-W%v exactly. */
function isoWeekKeyOf(d: Date): string {
  const t = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()));
  const dow = (t.getUTCDay() + 6) % 7; // Mon=0
  t.setUTCDate(t.getUTCDate() - dow + 3); // Thursday decides the ISO year
  const isoYear = t.getUTCFullYear();
  const firstThu = new Date(Date.UTC(isoYear, 0, 4));
  firstThu.setUTCDate(firstThu.getUTCDate() - ((firstThu.getUTCDay() + 6) % 7) + 3);
  const week = 1 + Math.round((t.getTime() - firstThu.getTime()) / (7 * 86400_000));
  return `${isoYear}-W${String(week).padStart(2, "0")}`;
}

// ── Key parse / validate / advance ───────────────────────────────────────────

const KEY_RE: Record<BucketId, RegExp> = {
  day: /^(\d{4})-(\d{2})-(\d{2})$/,
  week: /^(\d{4})-W(\d{2})$/,
  month: /^(\d{4})-(\d{2})$/,
  quarter: /^(\d{4})-Q([1-4])$/,
  year: /^(\d{4})$/,
};

/** True when `key` is a well-formed bucket key for `bucket`. NULL-date buckets arrive as
    null/"" from the API and are NOT valid time-axis keys. */
export function isBucketKey(key: string | null | undefined, bucket: BucketId): boolean {
  return typeof key === "string" && KEY_RE[bucket].test(key);
}

/** The bucket key that a calendar date (YYYY-MM-DD) falls into. Null for invalid input. */
export function dateToBucketKey(isoDate: string, bucket: BucketId): string | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(isoDate);
  if (!m) return null;
  const [, ys, ms, ds] = m;
  const y = Number(ys), mo = Number(ms), d = Number(ds);
  if (mo < 1 || mo > 12 || d < 1 || d > 31) return null;
  switch (bucket) {
    case "day": return isoDate;
    case "week": return isoWeekKeyOf(new Date(Date.UTC(y, mo - 1, d)));
    case "month": return `${ys}-${ms}`;
    case "quarter": return `${ys}-Q${Math.floor((mo - 1) / 3) + 1}`;
    case "year": return ys;
  }
}

/** The chronologically next bucket key. Null when `key` is malformed. */
export function nextBucketKey(key: string, bucket: BucketId): string | null {
  const m = KEY_RE[bucket].exec(key);
  if (!m) return null;
  switch (bucket) {
    case "day": {
      const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3])));
      d.setUTCDate(d.getUTCDate() + 1);
      return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
    }
    case "week": {
      const monday = mondayOfISOWeek(Number(m[1]), Number(m[2]));
      monday.setUTCDate(monday.getUTCDate() + 7);
      return isoWeekKeyOf(monday);
    }
    case "month": {
      let y = Number(m[1]), mo = Number(m[2]) + 1;
      if (mo > 12) { mo = 1; y += 1; }
      return `${y}-${String(mo).padStart(2, "0")}`;
    }
    case "quarter": {
      let y = Number(m[1]), q = Number(m[2]) + 1;
      if (q > 4) { q = 1; y += 1; }
      return `${y}-Q${q}`;
    }
    case "year":
      return String(Number(m[1]) + 1);
  }
}

// ── Axis labels — per plan D: day dd/MM · week "Tuần w" · month MM/yyyy · quarter "Qq yyyy" · year yyyy ──

/**
 * Human axis label for a bucket key. `withYear` widens the short variants (day, week)
 * when a series spans multiple years — otherwise "03/07" of 2025 and 2026 would collide.
 */
export function bucketAxisLabel(key: string, bucket: BucketId, withYear = false): string {
  const m = KEY_RE[bucket].exec(key);
  if (!m) return key; // malformed / NULL bucket — show raw, never throw on chart path
  switch (bucket) {
    case "day":
      return withYear ? `${m[3]}/${m[2]}/${m[1]}` : `${m[3]}/${m[2]}`;
    case "week":
      return withYear ? `Tuần ${Number(m[2])}/${m[1]}` : `Tuần ${Number(m[2])}`;
    case "month":
      return `${m[2]}/${m[1]}`;
    case "quarter":
      return `Q${m[2]} ${m[1]}`;
    case "year":
      return m[1];
  }
}

// ── Zero-fill — densify the series across the returned (or requested) range ──

/** Max points we will densify to — beyond this the raw sparse series is returned
    untouched (still chronological) instead of flooding the chart with zeros. */
export const ZERO_FILL_CAP = 400;

export interface ZeroFillOptions {
  /** Widen the range to the run overlay's dateFrom (YYYY-MM-DD) when active. */
  from?: string;
  /** Widen the range to the run overlay's dateTo (YYYY-MM-DD) when active. */
  to?: string;
  cap?: number;
}

/**
 * Fill empty buckets with {value:"0", count:0} between the series' min and max keys
 * (optionally widened to the overlay date range). Groups with NULL/malformed keys
 * (records whose date field is NULL) cannot sit on a time axis and are dropped here —
 * the table view still lists them. Output is chronological.
 */
export function zeroFillGroups(
  groups: AggGroupRow[],
  bucket: BucketId,
  opts: ZeroFillOptions = {},
): AggGroupRow[] {
  const valid = groups.filter((g) => isBucketKey(g.key, bucket));
  if (valid.length === 0) return [];

  const keys = valid.map((g) => g.key).sort();
  let start = keys[0];
  let end = keys[keys.length - 1];
  if (opts.from) {
    const k = dateToBucketKey(opts.from, bucket);
    if (k && k < start) start = k;
  }
  if (opts.to) {
    const k = dateToBucketKey(opts.to, bucket);
    if (k && k > end) end = k;
  }

  const cap = opts.cap ?? ZERO_FILL_CAP;
  const byKey = new Map(valid.map((g) => [g.key, g]));
  const out: AggGroupRow[] = [];
  let cursor: string | null = start;
  while (cursor !== null && cursor <= end) {
    out.push(byKey.get(cursor) ?? { key: cursor, value: "0", count: 0 });
    if (out.length > cap) {
      // Range too wide to densify — honest fallback: sparse but chronological.
      return [...valid].sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    }
    cursor = nextBucketKey(cursor, bucket);
  }
  return out;
}

/**
 * One-stop mapping for time-series charts: zero-fill + chronological order + axis labels.
 * Keys become display labels (tooltip shows the same label); when the range spans more
 * than one calendar/ISO year the short day/week labels automatically include the year.
 */
export function bucketSeriesToChartData(
  groups: AggGroupRow[],
  bucket: BucketId,
  opts: ZeroFillOptions = {},
): ChartSeriesPoint[] {
  const filled = zeroFillGroups(groups, bucket, opts);
  const years = new Set(filled.map((g) => g.key.slice(0, 4)));
  const withYear = years.size > 1;
  return filled.map((g) => ({
    key: bucketAxisLabel(g.key, bucket, withYear),
    value: Number(g.value),
    count: g.count,
  }));
}
