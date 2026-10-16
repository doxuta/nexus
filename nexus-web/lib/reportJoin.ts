// lib/reportJoin.ts — pure helpers for REPORT-JOIN (cross-object report, up to 2 chained hops).
//
// Mirrors backend specs/025-report-join/spec.md + api-conventions.md §19/§19a:
//   - A joined field is referenced as "<relationshipName>.<fieldName>" in groupBy /
//     metric field of a summary/matrix report config (no dot = base field, unchanged).
//   - config.joins = [{ relationship, optional }] — at most TWO chained hops (hop 2 starts from the entity hop 1
//     reaches, queue #293), each over FK-on-child (1-1/1-N)
//     OR N-N (REPORT-JOIN-NN: base → rel_<a>_<b> → far entity; REPORT_JOIN_NN_UNSUPPORTED is
//     retired); tabular + joins → REPORT_JOIN_TABULAR_UNSUPPORTED.
//   - Over an N-N hop, metric sum/avg → 422 AGGREGATE_NN_FANOUT_UNSAFE UNCONDITIONALLY (fan-out
//     is symmetric, so no field side is safe); count/min/max stay valid. See isMetricBlockedByNN.
//   - optional: true → LEFT JOIN (keep base rows with no related match, group key "—");
//     false/absent → INNER JOIN.
//
// PURE: no React, no fetch.

export interface ReportJoin {
  relationship: string;
  optional?: boolean;
}

/** Minimal structural slice of lib/api's AdminRelationship this module needs. */
export interface JoinableRel {
  name: string;
  parentEntityName: string;
  childEntityName: string;
  relationshipType: string; // "1-1" | "1-N" | "N-N"
}

/** Build the namespaced "<rel>.<field>" token for a joined field. */
export function buildJoinedField(relationship: string, field: string): string {
  return `${relationship}.${field}`;
}

/**
 * Parse a groupBy/field token. Split at the FIRST dot (same rule as the backend's
 * splitJoinedToken); no dot → null (a plain base field).
 */
export function parseJoinedField(token: string): { relationship: string; field: string } | null {
  const i = token.indexOf(".");
  if (i < 0) return null;
  return { relationship: token.slice(0, i), field: token.slice(i + 1) };
}

export function isJoinedField(token: string): boolean {
  return token.indexOf(".") >= 0;
}

/**
 * Relationships usable as a report join from baseEntity: any type the backend's resolveJoinEdge
 * has a shape for — FK-on-child (1-1 / 1-N) or N-N (link table) — AND baseEntity must be one edge
 * of the relationship (else the backend rejects with REPORT_JOIN_ENTITY_MISMATCH).
 *
 * Listed by type rather than "any relationship touching the entity" on purpose: a registry row of
 * some other/unknown type has no join shape in the engine, so offering it would guarantee a 422.
 */
export function joinableRelationships<T extends JoinableRel>(rels: T[], baseEntity: string): T[] {
  return rels.filter(
    (r) =>
      (r.relationshipType === "1-1" || r.relationshipType === "1-N" || r.relationshipType === "N-N") &&
      (r.parentEntityName === baseEntity || r.childEntityName === baseEntity),
  );
}

/**
 * True when the backend would reject this metric over this join: sum/avg across an N-N hop returns
 * 422 AGGREGATE_NN_FANOUT_UNSAFE (validateNNFanoutSafety), at SAVE time and at run time, because
 * an N-N fan-out is symmetric — a base row can match many far rows AND a far row can match many
 * base rows — so an additive metric double-counts no matter which side its field lives on.
 * count/min/max are unaffected (a repeated row changes neither a pair count nor an extremum),
 * so this is deliberately NOT "count only".
 */
export function isMetricBlockedByNN(rel: JoinableRel | null | undefined, metric: string): boolean {
  return rel?.relationshipType === "N-N" && (metric === "sum" || metric === "avg");
}

/**
 * The entity on the OTHER side of the hop for baseEntity — what the joined field picker lists.
 * null when baseEntity is not an edge (the relationship is not joinable from here).
 * A self-referential relationship (parent === child) resolves to the same entity.
 */
export function relatedEntityOf(rel: JoinableRel, baseEntity: string): string | null {
  if (rel.childEntityName === baseEntity) return rel.parentEntityName;
  if (rel.parentEntityName === baseEntity) return rel.childEntityName;
  return null;
}

/**
 * Drop a stale namespaced token when the selected join changed: a token namespaced onto a
 * DIFFERENT relationship (or onto anything when no join is selected) would 422 at save/run
 * (REPORT_JOIN_ENTITY_MISMATCH / AGGREGATE_REQUIRES_INDEXED), so it resets to "".
 * Base (un-namespaced) tokens always survive.
 */
export function stripStaleJoinToken(token: string, selectedRelationship: string | readonly string[]): string {
  const parsed = parseJoinedField(token);
  if (!parsed) return token;
  const keep = typeof selectedRelationship === "string" ? [selectedRelationship] : selectedRelationship;
  return keep.includes(parsed.relationship) ? token : "";
}

/**
 * Apply the join onto a built report config (after lib/reportConfig's buildReportConfig).
 * Writes config.joins ONLY for summary/matrix with a selected relationship — tabular never
 * carries joins (backend: REPORT_JOIN_TABULAR_UNSUPPORTED). Returns the same object, mutated,
 * for call-site chaining.
 */
export function withReportJoin(
  cfg: Record<string, unknown>,
  type: string,
  join: ReportJoin | null,
): Record<string, unknown> {
  return withReportJoins(cfg, type, join ? [join] : []);
}

/** Max chained relationships a report (or a pinned widget) may span — mirrors BE maxReportJoinHops. */
export const MAX_JOIN_HOPS = 2;

/** The wire shape of a join chain (report config.joins / widget joins): blank names dropped, optional only when true. */
export function joinsPayload(joins: readonly ReportJoin[]): Array<{ relationship: string; optional?: true }> {
  return joins
    .filter((j) => j.relationship)
    .slice(0, MAX_JOIN_HOPS)
    .map((j) => ({ relationship: j.relationship, ...(j.optional ? { optional: true as const } : {}) }));
}

/** withReportJoin for a chain of up to two hops (queue #293): hop 2 starts from the entity hop 1 reaches. */
export function withReportJoins(
  cfg: Record<string, unknown>,
  type: string,
  joins: readonly ReportJoin[],
): Record<string, unknown> {
  const payload = joinsPayload(joins);
  if (type === "tabular" || payload.length === 0) return cfg;
  cfg.joins = payload;
  return cfg;
}
