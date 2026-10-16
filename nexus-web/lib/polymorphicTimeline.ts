// polymorphicTimeline — the REVERSE side of a `polymorphic_relation` field (ACTIVITY-CORE #3a).
//
// The forward side ships: a field of type `polymorphic_relation` stores { type, id } and the engine
// materializes two indexed generated columns `<f>_type` / `<f>_id`, filterable on the ordinary
// record-list endpoint (verified: internal/services/record_orchestration.go List() routes a
// `<poly>_type` / `<poly>_id` key to its physical column; polymorphic_relation_integration_test.go
// TestPolymorphic_TimelineFilter proves the query). What was missing is the QUESTION: given a record,
// which OTHER entities have a polymorphic field pointing at its entity? That is pure metadata work,
// so it lives here — no fetching in this module, which is what makes it unit-testable.
//
// Answering it needs the field metadata of the candidate entities; the caller supplies that (one
// GET /metadata/fields/:entity per entity — there is no bulk fields-across-entities route, and this
// slice deliberately does not add one).

import type { FieldMeta } from "@/lib/api";

/** One entity's field metadata, as the caller collected it from listFields(). */
export interface EntityFields {
  entity: string;
  fields: FieldMeta[];
}

/**
 * One (entity, field) pair whose polymorphic field can point at the target entity. The physical
 * filter columns are `<fieldName>_type` / `<fieldName>_id` — the caller builds the list query.
 */
export interface PolymorphicSource {
  entity: string;
  fieldName: string;
  /** Field label for the section header; falls back to the raw field name. */
  fieldLabel: string;
}

// options.targetEntities as the backend stores it: a non-empty array of entity names
// (entity_service.go validatePolymorphicField). Read defensively — `options` is an untyped
// Record<string, unknown> on the wire, so a legacy/hand-edited row can hold anything.
function targetEntitiesOf(field: FieldMeta): string[] {
  const raw = (field.options as { targetEntities?: unknown } | undefined)?.targetEntities;
  if (!Array.isArray(raw)) return [];
  // Trimmed on compare because the backend's own polymorphicTargetList() trims on READ, which
  // means untrimmed values do reach sys_fields.options and must still match here.
  return raw.filter((t): t is string => typeof t === "string").map((t) => t.trim()).filter(Boolean);
}

/**
 * Every (entity, field) pair among `candidates` whose polymorphic_relation field lists
 * `targetEntity` in its allowlist. Order is stable: candidate order, then field order.
 * An entity may appear more than once — two distinct polymorphic fields on the same entity that
 * both target this one are two independent lists, not one merged list.
 */
export function findPolymorphicSources(
  targetEntity: string,
  candidates: EntityFields[],
): PolymorphicSource[] {
  const target = targetEntity.trim();
  if (!target) return [];

  const out: PolymorphicSource[] = [];
  for (const c of candidates) {
    for (const f of c.fields) {
      if (f.fieldType !== "polymorphic_relation") continue;
      if (!targetEntitiesOf(f).includes(target)) continue;
      out.push({ entity: c.entity, fieldName: f.name, fieldLabel: f.label?.trim() || f.name });
    }
  }
  return out;
}
