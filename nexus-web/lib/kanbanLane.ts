// Query string for ONE Kanban lane: the lane condition PLUS the list's active filter/search, so the
// board shows the same record set as the table (DAILY-QUEUE #279).
export interface LaneQueryInput {
  laneField: string;
  col: { key: string; isEmpty: boolean };
  limit: number;
  expand: string[];
  /** Active list filters as `field[op]=value&…` (filtersToQuery output). */
  filterQuery?: string;
  /** Active list search text. */
  q?: string;
}

/**
 * null = this lane cannot hold a record the list's own filter allows (the filter pins the lane field
 * to another value / non-empty), so the caller shows it empty instead of querying — a lane condition
 * must never override the user's filter on the same key (the board would show rows the table hides).
 */
export function buildLaneQuery(i: LaneQueryInput): string | null {
  const p = new URLSearchParams({ limit: String(i.limit), sort: "updated_at", order: "desc" });
  if (i.q) p.set("q", i.q);
  const f = new URLSearchParams(i.filterQuery ?? "");
  const eq = f.get(`${i.laneField}[eq]`);
  const isnull = f.get(`${i.laneField}[isnull]`);
  if (i.col.isEmpty ? eq !== null || isnull === "0" : (eq !== null && eq !== i.col.key) || isnull === "1") return null;
  f.forEach((v, k) => p.set(k, v));
  if (i.col.isEmpty) p.set(`${i.laneField}[isnull]`, "1");
  else p.set(`${i.laneField}[eq]`, i.col.key);
  if (i.expand.length) p.set("expand", i.expand.join(","));
  return p.toString();
}
