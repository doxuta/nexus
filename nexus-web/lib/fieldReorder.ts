// fieldReorder — pure model for W5 drag-reorder of the Data Model field list (plan §3.2).
//
// Optimistic protocol (api-conventions §10.2):
//   1. onDragEnd → applyMove() reorders the LOCAL array immediately (UI feels instant).
//   2. reorderPayload() → PUT /admin/fields/reorder with the FULL active-field id list.
//   3. On 422 (FIELD_REORDER_INVALID / FIELD_REORDER_UNKNOWN_FIELD) the caller restores the
//      snapshot taken before step 1 — the backend rolled back atomically, so snapshot === server.
//
// Fields without an id (defensive: metadata should always carry one) are excluded from the
// payload; the backend keeps their stored order untouched, which matches leaving them in place.

import type { FieldMeta } from "@/lib/api";

/** Reorder by dnd-kit ids: move `activeName` to the slot currently held by `overName`. */
export function applyMove(fields: FieldMeta[], activeName: string, overName: string): FieldMeta[] {
  const from = fields.findIndex((f) => f.name === activeName);
  const to = fields.findIndex((f) => f.name === overName);
  if (from < 0 || to < 0 || from === to) return fields;
  const next = [...fields];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

/** Full ordered id list for PUT /admin/fields/reorder (backend wants EVERY active field). */
export function reorderPayload(fields: FieldMeta[]): string[] {
  return fields.map((f) => f.id).filter((id): id is string => !!id);
}

/** True when display order differs — skip the network call for a no-op drop. */
export function orderChanged(a: FieldMeta[], b: FieldMeta[]): boolean {
  if (a.length !== b.length) return true;
  return a.some((f, i) => f.name !== b[i].name);
}
