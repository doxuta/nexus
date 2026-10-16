// kanbanModel — pure model for the W9c Kanban board (group-by-picklist + optimistic drag).
//
// The board groups the LOADED records by ONE picklist field into columns whose ORDER matches the
// picklist's declared value order (declaration order is meaningful for a pipeline: Prospecting →
// … → Closed). A trailing "Chưa đặt" bucket collects records whose value is empty or no longer a
// declared picklist value (stale value after an admin edits the picklist). No per-entity hard-coding:
// the grouping field + its values come entirely from `sys_fields` metadata.
//
// Drag protocol (mirrors lib/fieldReorder + the pins optimistic pattern):
//   1. begin  → moveRecordValue() reorders the LOCAL records immediately (UI feels instant),
//               keeping a snapshot for rollback.
//   2. PUT /:entity/:id { <field>: newValue } (partial update — one field).
//   3. commit → drop the snapshot on success; OR rollback → restore the snapshot on 403/409/422.
// The reducer below is the state machine the component drives; keeping it pure makes the
// optimistic-move + rollback path unit-testable without a DOM or network.

import type { ApiRecord, FieldMeta } from "@/lib/api";

// Sentinel column key for records with no (or a stale) picklist value. Kept distinct from any real
// picklist value so it can never collide with a legitimate option.
export const EMPTY_BUCKET = "__kanban_empty__";
export const EMPTY_LABEL = "Chưa đặt";

export interface KanbanColumn {
  key: string; // picklist value, or EMPTY_BUCKET for the trailing bucket
  label: string; // display label (valueLabels[code] ?? code for a picklist option; EMPTY_LABEL for the empty bucket)
  isEmpty: boolean; // true only for the trailing "Chưa đặt" bucket
}

/**
 * Choose the picklist field to group by. An explicit `preferred` field name wins IF it names a
 * picklist field on this entity; otherwise the FIRST picklist field in metadata order. Returns
 * undefined when the entity has no picklist field (the board then shows an honest empty note).
 *
 * `preferred` is the layout's declared `kanbanField` (GET /metadata/layouts/:entity, see
 * lib/api.ts LayoutView), passed by KanbanBoard since DAILY-QUEUE #5. The name is re-checked
 * against the fields on hand rather than trusted: field ACL may have stripped it for this user,
 * or the field may have been deleted or retyped after the layout was saved. A name that no longer
 * fits falls back to the first picklist — the same honest guess as an entity that declares nothing.
 */
export function pickKanbanField(fields: FieldMeta[], preferred?: string): FieldMeta | undefined {
  const picklists = fields.filter((f) => f.fieldType === "picklist");
  if (preferred) {
    const p = picklists.find((f) => f.name === preferred);
    if (p) return p;
  }
  return picklists[0];
}

/** Ordered picklist string values (defensive: coerce `{value}` objects, drop empties/dupes). */
export function fieldValues(field: FieldMeta | undefined): string[] {
  const raw = field?.options?.values;
  if (!Array.isArray(raw)) return [];
  const out: string[] = [];
  const seen = new Set<string>();
  for (const v of raw) {
    let s: string;
    if (typeof v === "string") s = v;
    else if (v != null && typeof v === "object" && "value" in (v as Record<string, unknown>)) {
      s = String((v as Record<string, unknown>).value);
    } else s = String(v);
    if (s === "" || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
  }
  return out;
}

/** Columns in picklist DECLARATION order, with the empty bucket appended LAST.
 *
 * `label` goes through options.valueLabels ("New" → "Mới") while `key` stays the stored CODE —
 * columnOf() and every drag write compare/emit the key, so the board's mechanics never see the
 * Vietnamese text. Not routed through lib/metadata's valueLabel() only because FieldMeta.options
 * here is the loose wire shape (Record<string, unknown>), not FieldOptions; the fallback rule is
 * the same (no label → the code, never invented). */
export function columnsForField(field: FieldMeta | undefined): KanbanColumn[] {
  const labels = (field?.options?.valueLabels ?? {}) as Record<string, string>;
  const cols: KanbanColumn[] = fieldValues(field).map((v) => ({ key: v, label: labels[v] ?? v, isEmpty: false }));
  cols.push({ key: EMPTY_BUCKET, label: EMPTY_LABEL, isEmpty: true });
  return cols;
}

/** Which column key a record belongs to: its value if that value is a known column, else EMPTY_BUCKET. */
export function columnOf(record: ApiRecord, fieldName: string, validValues: string[]): string {
  const raw = record.data[fieldName];
  const v = raw == null ? "" : String(raw);
  return v !== "" && validValues.includes(v) ? v : EMPTY_BUCKET;
}

/** Group records into columns (every column initialised; record order preserved within each). */
export function groupRecords(
  records: ApiRecord[],
  fieldName: string,
  columns: KanbanColumn[],
): Record<string, ApiRecord[]> {
  const valid = columns.filter((c) => !c.isEmpty).map((c) => c.key);
  const out: Record<string, ApiRecord[]> = {};
  for (const c of columns) out[c.key] = [];
  for (const r of records) {
    const key = columnOf(r, fieldName, valid);
    (out[key] ??= []).push(r);
  }
  return out;
}

/** Immutable: a new records array with `cardId`'s field set to `newValue` (null clears the field). */
export function moveRecordValue(
  records: ApiRecord[],
  cardId: string,
  fieldName: string,
  newValue: string | null,
): ApiRecord[] {
  return records.map((r) =>
    r.id === cardId ? { ...r, data: { ...r.data, [fieldName]: newValue } } : r,
  );
}

// ── Optimistic-move state machine ──────────────────────────────────────────────────
// One in-flight move at a time (the component disables dragging while `pending` is set, which also
// sidesteps snapshot clobbering under rapid successive drags). `snapshot` is the records array as it
// was BEFORE the move, so rollback === restore snapshot (server rolled its own write back on error).

export interface MoveState {
  records: ApiRecord[];
  pending: { cardId: string; snapshot: ApiRecord[] } | null;
}

export type MoveAction =
  | { type: "begin"; cardId: string; fieldName: string; newValue: string | null }
  | { type: "commit" }
  | { type: "rollback" }
  | { type: "reset"; records: ApiRecord[] };

export function moveReducer(state: MoveState, action: MoveAction): MoveState {
  switch (action.type) {
    case "begin":
      return {
        records: moveRecordValue(state.records, action.cardId, action.fieldName, action.newValue),
        pending: { cardId: action.cardId, snapshot: state.records },
      };
    case "commit":
      return { records: state.records, pending: null };
    case "rollback":
      return state.pending ? { records: state.pending.snapshot, pending: null } : state;
    case "reset":
      return { records: action.records, pending: null };
    default:
      return state;
  }
}
