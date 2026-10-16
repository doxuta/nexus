// Drag and drop of the form screen editor (specs/075 L1b, FR-021..FR-027). Pure helpers: sensors, the drop decision
// (`dropTarget`) and its effect on the draft (`applyDrop`). The context itself lives in ScreenEditor.
//   draggables: a cell on the form (id = cell.id) · an unplaced field (id = `u:<field>`)
//   droppables: a cell (insert at its slot) · an EMPTY section (`sec:<id>`) · the unplaced list (`UNPLACED_ZONE` = off the form)
// Keyboard (FR-027): the sensor is attached to the handle only (CanvasCell), arrows jump droppable to droppable.
import {
  KeyboardSensor, PointerSensor, pointerWithin, closestCenter,
  type Collision, type CollisionDetection, type UniqueIdentifier,
} from "@dnd-kit/core";
import type { FieldMeta } from "@/lib/api";
import { findCell, moveCell, type DraftSection } from "@/lib/layoutDraft";
import { jumpToDroppable } from "@/lib/dndKeyboard";
import { placeFieldAt, removeFieldGuarded } from "@/lib/screenEditorModel";

export const DRAG_THRESHOLD_PX = 5;
export const UNPLACED_ZONE = "unplaced";
export const sectionDropId = (sectionId: string) => `sec:${sectionId}`;
export const unplacedDragId = (field: string) => `u:${field}`;

// Same shape as useSensor(PointerSensor, options): consumed by `useSensors(...FORM_SENSORS.map(...))` in the editor.
export const FORM_SENSORS = [
  { sensor: PointerSensor, options: { activationConstraint: { distance: DRAG_THRESHOLD_PX } } },
  { sensor: KeyboardSensor, options: { coordinateGetter: jumpToDroppable } },
] as const;

const rank = (c: Collision): number => {
  const id = String(c.id);
  return id === UNPLACED_ZONE ? 2 : id.startsWith("sec:") ? 1 : 0;
};

/** The pointer decides when there is one (cells win over their section); a keyboard drag uses the nearest centre. */
export const formCollision: CollisionDetection = (args) => {
  const hits = args.pointerCoordinates ? pointerWithin(args) : closestCenter(args);
  return [...hits].sort((a, b) => rank(a) - rank(b));
};

export type DropResult =
  | { kind: "move"; cellId: string; sectionId: string; index: number }
  | { kind: "place"; field: string; sectionId: string; index: number }
  | { kind: "remove"; cellId: string }
  | { kind: "blocked"; reason: "requiredMustStay" }
  | { kind: "none" };

const NONE: DropResult = { kind: "none" };

export function dropTarget(
  draft: DraftSection[],
  fields: FieldMeta[],
  activeId: UniqueIdentifier,
  overId: UniqueIdentifier | null | undefined,
): DropResult {
  if (overId == null || overId === activeId) return NONE;
  const active = String(activeId);
  const over = String(overId);
  const fromUnplaced = active.startsWith("u:");
  if (over === UNPLACED_ZONE) {
    if (fromUnplaced) return NONE;
    const r = removeFieldGuarded(draft, active, fields);
    return r.blocked ? { kind: "blocked", reason: r.blocked } : { kind: "remove", cellId: active };
  }
  // where: a slot in a section
  let sectionId: string;
  let slot: number;
  if (over.startsWith("sec:")) {
    sectionId = over.slice(4);
    const s = draft.find((x) => x.id === sectionId);
    if (!s) return NONE;
    slot = s.cells.length;
  } else {
    const hit = findCell(draft, over);
    if (!hit) return NONE;
    sectionId = hit.section.id;
    slot = hit.index;
    // moving down inside the same section: take the place AFTER the target (moveCell's slot is in the old array)
    const from = fromUnplaced ? null : findCell(draft, active);
    if (from && from.section.id === sectionId && from.index < hit.index) slot = hit.index + 1;
  }
  if (fromUnplaced) return { kind: "place", field: active.slice(2), sectionId, index: slot };
  return { kind: "move", cellId: active, sectionId, index: slot };
}

export function applyDrop(draft: DraftSection[], r: DropResult): DraftSection[] {
  if (r.kind === "move") return moveCell(draft, r.cellId, r.sectionId, r.index);
  if (r.kind === "place") return placeFieldAt(draft, r.field, r.sectionId, r.index);
  if (r.kind === "remove") return draft.map((s) => (s.cells.some((c) => c.id === r.cellId) ? { ...s, cells: s.cells.filter((c) => c.id !== r.cellId) } : s));
  return draft;
}
