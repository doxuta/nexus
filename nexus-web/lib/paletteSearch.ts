// lib/paletteSearch.ts — ⌘K cross-entity record search shaping (W3 C, plan §2.1).
// The palette fans out GET /:entity?q=<query>&limit=3 to the user's accessible entities.
// Pure logic lives here (testable): which entities to query (cap 5, current-entity and
// pinned entities first) and how to shape the per-entity responses into grouped rows.
// The unified cross-entity search endpoint is P2 backend — client fan-out until then.

import type { ApiRecord, EntityMeta } from "@/lib/api";
import { recordLabel } from "@/lib/recordLabel";

export const SEARCH_ENTITY_CAP = 5;
export const SEARCH_PER_ENTITY_LIMIT = 3;
export const SEARCH_MIN_CHARS = 2;
export const SEARCH_DEBOUNCE_MS = 250;

/**
 * Pick the entities to fan out to, most-relevant first:
 *   1. the entity of the route the user is standing on (if accessible),
 *   2. pinned entities in pin order,
 *   3. the rest in metadata order — capped at SEARCH_ENTITY_CAP.
 */
export function pickSearchEntities(
  entities: EntityMeta[],
  opts: { pins?: string[]; current?: string; cap?: number } = {},
): EntityMeta[] {
  const { pins = [], current, cap = SEARCH_ENTITY_CAP } = opts;
  const byName = new Map(entities.map((e) => [e.name, e]));
  const out: EntityMeta[] = [];
  const seen = new Set<string>();

  const push = (name: string | undefined) => {
    if (!name || seen.has(name)) return;
    const e = byName.get(name);
    if (!e) return;
    seen.add(name);
    out.push(e);
  };

  push(current);
  for (const p of pins) push(p);
  for (const e of entities) push(e.name);

  return out.slice(0, cap);
}

export interface RecordHitGroup {
  entity: string;
  entityLabel: string;
  hits: { id: string; label: string; href: string }[];
}

/**
 * Shape fan-out responses into display groups. Entities with zero hits are dropped;
 * record labels use the shared VIP-label heuristic (works fields-less: name/title/…).
 */
// SEARCH-UNIFIED (Wave 3.5): shape the server /search response (already entity-grouped, ACL-
// scoped server-side) into the palette's row model. Mirrors shapeRecordResults' label + href.
export function shapeServerSearch(
  groups: { entity: string; label: string; hits: { id: string; data: Record<string, unknown> }[] }[],
): RecordHitGroup[] {
  const out: RecordHitGroup[] = [];
  for (const g of groups) {
    if (!g.hits.length) continue;
    out.push({
      entity: g.entity,
      entityLabel: g.label,
      hits: g.hits.map((h) => ({
        id: h.id,
        label: recordLabel({ id: h.id, data: h.data } as ApiRecord, []),
        href: `/${g.entity}/${h.id}`,
      })),
    });
  }
  return out;
}

export function shapeRecordResults(
  results: { entity: EntityMeta; records: ApiRecord[] }[],
): RecordHitGroup[] {
  const groups: RecordHitGroup[] = [];
  for (const { entity, records } of results) {
    if (!records.length) continue;
    groups.push({
      entity: entity.name,
      entityLabel: entity.label,
      hits: records.map((r) => ({
        id: r.id,
        label: recordLabel(r, []),
        href: `/${entity.name}/${r.id}`,
      })),
    });
  }
  return groups;
}
