// relCanvas — pure logic for the Relationship Canvas v2 (W2, plan §3.1). No React, no DOM:
// everything here is unit-testable (lib/relCanvas.test.ts). The component layer
// (components/studio/relmap/*) consumes these helpers.

import dagre from "@dagrejs/dagre";
import type { AdminRelationship, FieldMeta, RelationshipImpact } from "@/lib/api";

export type RelType = "1-N" | "N-N" | "1-1";

// One canvas edge, derived in deriveEdges(). Registry-backed edges are keyed by the REAL
// sys_relationships.id (unlocks impact/delete lifecycle); `legacy` edges are relation fields
// created BEFORE migration 000028 that never got a registry row — they render honestly but
// the lifecycle endpoints can't touch them (no id to call).
export interface CanvasEdgeSpec {
  id: string;
  /** Parent entity — the "1" side (options.relatedEntity of the child's relation field). */
  source: string;
  /** Child entity — the "N" side (owns the FK column). For N-N: childEntityName. */
  target: string;
  relType: RelType;
  /** Registry row — present ⇔ not legacy. */
  rel?: AdminRelationship;
  /** The child-side relation FieldMeta backing a 1-N/1-1 edge, when resolvable. */
  field?: FieldMeta;
  /** true → relation field with NO registry row (pre-000028) — lifecycle unavailable. */
  legacy: boolean;
  /** 0-based index among edges sharing the same unordered entity pair — the custom edge
   *  offsets its center by this so parallel edges don't overlap into one line. */
  parallelIndex: number;
}

function relTypeOf(raw: string | undefined): RelType {
  return raw === "N-N" || raw === "1-1" ? raw : "1-N";
}

function fieldCardinality(f: FieldMeta): RelType {
  return (f.options?.cardinality as string | undefined) === "1-1" ? "1-1" : "1-N";
}

// deriveEdges — single source of edge truth. Registry rows FIRST (re-keyed by real
// sys_relationships.id, W2 §3.1 "danh tính"); any relation field the registry doesn't cover
// falls back to a synthetic `legacy:` edge so pre-000028 schemas still render completely.
// A registry-backed 1-N/1-1 row is matched to its child field via childFkColumn
// ("<fieldName>_id", api-conventions §6.1) so the same relation never draws twice.
export function deriveEdges(
  entityNames: ReadonlySet<string>,
  fieldsByEntity: Record<string, FieldMeta[]>,
  rows: AdminRelationship[],
): CanvasEdgeSpec[] {
  const edges: CanvasEdgeSpec[] = [];
  const covered = new Set<string>(); // "<childEntity>.<fieldName>" already drawn from registry

  for (const row of rows) {
    if (!entityNames.has(row.parentEntityName) || !entityNames.has(row.childEntityName)) continue;
    const relType = relTypeOf(row.relationshipType);
    let field: FieldMeta | undefined;
    if (relType !== "N-N" && row.childFkColumn?.endsWith("_id")) {
      const fieldName = row.childFkColumn.slice(0, -3);
      field = fieldsByEntity[row.childEntityName]?.find(
        (f) => f.name === fieldName && f.fieldType === "relation",
      );
      if (field) covered.add(`${row.childEntityName}.${field.name}`);
    }
    edges.push({
      id: row.id,
      source: row.parentEntityName,
      target: row.childEntityName,
      relType,
      rel: row,
      field,
      legacy: false,
      parallelIndex: 0,
    });
  }

  // Legacy fallback — relation fields the registry doesn't know about.
  for (const [entity, fields] of Object.entries(fieldsByEntity)) {
    if (!entityNames.has(entity)) continue;
    for (const f of fields) {
      if (f.fieldType !== "relation") continue;
      const targetEntity = f.options?.relatedEntity as string | undefined;
      if (!targetEntity || !entityNames.has(targetEntity)) continue;
      if (covered.has(`${entity}.${f.name}`)) continue;
      edges.push({
        id: `legacy:${entity}.${f.name}`,
        source: targetEntity,
        target: entity,
        relType: fieldCardinality(f),
        field: f,
        legacy: true,
        parallelIndex: 0,
      });
    }
  }

  // Parallel-edge fan-out: same unordered pair → increasing index (0,1,2…).
  const pairCount = new Map<string, number>();
  for (const e of edges) {
    const key = [e.source, e.target].sort().join("→");
    const n = pairCount.get(key) ?? 0;
    e.parallelIndex = n;
    pairCount.set(key, n + 1);
  }
  return edges;
}

// ── Auto-layout (dagre) ──
export interface NodeBox { id: string; width: number; height: number }
export interface XY { x: number; y: number }

export const NODE_W = 224;
/** EntityNode height estimate: header 40 + up-to-3 field rows ~21 + "+N" row + padding. */
export function nodeHeight(fieldCount: number): number {
  return 40 + Math.min(fieldCount, 3) * 21 + (fieldCount > 3 ? 20 : 0) + 16;
}

export function dagreLayout(
  nodes: NodeBox[],
  edges: Array<{ source: string; target: string }>,
): Record<string, XY> {
  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "LR", nodesep: 48, ranksep: 110, marginx: 24, marginy: 24 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const n of nodes) g.setNode(n.id, { width: n.width, height: n.height });
  for (const e of edges) {
    if (e.source !== e.target) g.setEdge(e.source, e.target);
  }
  dagre.layout(g);
  const out: Record<string, XY> = {};
  for (const n of nodes) {
    const pos = g.node(n.id);
    // dagre positions are CENTER-anchored; react-flow wants TOP-LEFT.
    out[n.id] = { x: Math.round(pos.x - n.width / 2), y: Math.round(pos.y - n.height / 2) };
  }
  return out;
}

// ── Position persistence (localStorage — server-side follow-up noted) ──
// Key is tenant + entity-SET scoped: adding/removing an entity produces a NEW key, so stale
// layouts for a different schema shape never half-apply. Server persistence (one sys_layouts
// row, entityName="__schema__", viewType="canvas") is the planned follow-up (§3.1 Backend);
// this local layer stays as the offline/fast path once that lands.
export function layoutStorageKey(tenantId: string, entityNames: string[]): string {
  const sorted = [...entityNames].sort().join(",");
  // djb2 — tiny stable hash to keep the key readable without embedding every entity name.
  let h = 5381;
  for (let i = 0; i < sorted.length; i++) h = ((h << 5) + h + sorted.charCodeAt(i)) >>> 0;
  return `nexus.relcanvas.v1.${tenantId || "anon"}.${sorted.length}-${h.toString(36)}`;
}

// ── Create-relation prefill ──
// Suggest the child-side relation FIELD name from the parent entity: strip the prefix the two
// names share (crm_accounts + crm_opportunities → "accounts"), then the plural "s"
// (→ "account" — matches the hand-built CRM pack convention). Deduped against existing fields.
export function suggestFieldName(
  parentEntity: string,
  childEntity: string,
  existingChildFields: ReadonlyArray<Pick<FieldMeta, "name">>,
): string {
  let base = parentEntity;
  const pi = parentEntity.indexOf("_");
  const ci = childEntity.indexOf("_");
  if (pi > 0 && ci > 0 && parentEntity.slice(0, pi) === childEntity.slice(0, ci)) {
    base = parentEntity.slice(pi + 1);
  }
  base = base.replace(/s$/, "") || parentEntity;
  const taken = new Set(existingChildFields.map((f) => f.name));
  if (!taken.has(base)) return base;
  for (let i = 2; i < 100; i++) {
    if (!taken.has(`${base}_${i}`)) return `${base}_${i}`;
  }
  return `${base}_${Date.now() % 1000}`;
}

/** snake_case guard for names typed into the create dialog (same charset the backend's
 *  SanitizeIdentifier accepts: lowercase letters, digits, underscore, must start a-z). */
export function isValidIdent(name: string): boolean {
  return /^[a-z][a-z0-9_]{0,63}$/.test(name);
}

// ── Relation map on SLDS (specs/075 L4a) ──
/** Lower-case + strip Vietnamese diacritics (đ is not a combining mark, so it is mapped by hand). */
export function foldVi(s: string): string {
  return s.toLowerCase().normalize("NFD").replace(/\p{Diacritic}/gu, "").replace(/đ/g, "d");
}

/** Table search: label or code, diacritics ignored. Blank query = everything. */
export function matchEntities<T extends { name: string; label: string }>(entities: readonly T[], query: string): T[] {
  const q = foldVi(query.trim());
  if (!q) return [...entities];
  return entities.filter((e) => foldVi(e.label).includes(q) || foldVi(e.name).includes(q));
}

/** `?bang=` filter: the table itself plus every table joined to it directly (either direction). */
export function neighbourhood(entity: string, edges: ReadonlyArray<{ source: string; target: string }>): Set<string> {
  const names = new Set<string>([entity]);
  for (const e of edges) {
    if (e.source === entity) names.add(e.target);
    if (e.target === entity) names.add(e.source);
  }
  return names;
}

/** Edge look by cardinality: arrowheads + dash + a text label. Colour is never the carrier (FR-088). */
export function edgeKindOf(t: RelType) {
  if (t === "N-N") return { dashed: true, startArrow: true, endArrow: true, labelKey: "studio.build.relMap.kind.manyMany" as const };
  if (t === "1-1") return { dashed: false, startArrow: false, endArrow: false, labelKey: "studio.build.relMap.kind.oneOne" as const };
  return { dashed: false, startArrow: false, endArrow: true, labelKey: "studio.build.relMap.kind.oneMany" as const };
}

/** The "Đang dùng" block reads the impact payload directly. */
export function impactDependents(impact: RelationshipImpact) {
  return { records: impact.affectedRecords, blocking: impact.dependentRollups, inert: impact.dependentLookups };
}
