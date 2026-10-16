// Keyboard drag for the record list's board (KanbanBoard) and calendar (CalendarView), @dnd-kit/core
// 6.3.1. Both drop by "which lane / day is the item over"; with the keyboard that needs three things
// dnd-kit does not give by default:
//   • pointerOrClosest — pointerWithin reads pointer coordinates, and a keyboard drag has none
//     (getEventCoordinates(KeyboardEvent) = null → []), so a keyboard drop never had an `over`.
//     Without a pointer the target is the droppable nearest the dragged item.
//   • jumpToDroppable — the default arrow step is 25px (a dozen presses per lane); here one press
//     moves the item onto the next lane / day in that direction, same row or column first.
//   • dndAccessibility — the default announcements are English and name raw ids (record UUIDs).
import {
  closestCenter,
  pointerWithin,
  type Announcements,
  type CollisionDetection,
  type KeyboardCoordinateGetter,
  type ScreenReaderInstructions,
  type UniqueIdentifier,
} from "@dnd-kit/core";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export const pointerOrClosest: CollisionDetection = (args) => (args.pointerCoordinates ? pointerWithin(args) : closestCenter(args));

const STEP: Record<string, [number, number]> = { ArrowRight: [1, 0], ArrowLeft: [-1, 0], ArrowDown: [0, 1], ArrowUp: [0, -1] };

export const jumpToDroppable: KeyboardCoordinateGetter = (event, { context, currentCoordinates }) => {
  const step = STEP[event.code];
  const { collisionRect, droppableRects, droppableContainers } = context;
  if (!step || !collisionRect) return undefined;
  event.preventDefault(); // an arrow at the edge must not scroll the page either
  const [dx, dy] = step;
  const cx = collisionRect.left + collisionRect.width / 2;
  const cy = collisionRect.top + collisionRect.height / 2;
  const enabled = droppableContainers.getEnabled().flatMap(({ id }) => droppableRects.get(id) ?? []);
  // Steps are measured from the lane / day the item is over now (its centre sits anywhere inside it).
  const from = enabled.find((r) => cx >= r.left && cx <= r.right && cy >= r.top && cy <= r.bottom) ?? collisionRect;
  const fx = from.left + from.width / 2;
  const fy = from.top + from.height / 2;
  let best: { x: number; y: number; score: number } | null = null;
  for (const r of enabled) {
    // Strictly beyond `from` that way, and in the same row band (horizontal) or column (vertical).
    const beyond = dx > 0 ? r.left >= from.right - 1 : dx < 0 ? r.right <= from.left + 1 : dy > 0 ? r.top >= from.bottom - 1 : r.bottom <= from.top + 1;
    const inLine = dx ? r.top < from.bottom && r.bottom > from.top : r.left < from.right && r.right > from.left;
    if (!beyond || !inLine) continue;
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    const score = (x - fx) * dx + (y - fy) * dy + Math.abs((x - fx) * dy) + Math.abs((y - fy) * dx);
    if (!best || score < best.score) best = { x, y, score };
  }
  if (!best) return currentCoordinates;
  // Centre the item on the target, so the nearest-droppable collision picks exactly that one.
  return { x: currentCoordinates.x + best.x - cx, y: currentCoordinates.y + best.y - cy };
};

const say = (key: I18nKey, values: Record<string, string>) => t(key).replace(/\{(\w+)\}/g, (m, k: string) => values[k] ?? m);

/** Vietnamese live-region text for a DndContext: `item` names the dragged record, `target` a lane / day. */
export function dndAccessibility(
  item: (id: UniqueIdentifier) => string,
  target: (id: UniqueIdentifier) => string,
): { announcements: Announcements; screenReaderInstructions: ScreenReaderInstructions } {
  return {
    screenReaderInstructions: { draggable: t("page.list.dnd.instructions") },
    announcements: {
      onDragStart: ({ active }) => say("page.list.dnd.pickedUp", { item: item(active.id) }),
      onDragOver: ({ active, over }) =>
        over
          ? say("page.list.dnd.over", { item: item(active.id), target: target(over.id) })
          : say("page.list.dnd.notOver", { item: item(active.id) }),
      onDragEnd: ({ active, over }) =>
        over
          ? say("page.list.dnd.dropped", { item: item(active.id), target: target(over.id) })
          : say("page.list.dnd.droppedNowhere", { item: item(active.id) }),
      onDragCancel: ({ active }) => say("page.list.dnd.cancelled", { item: item(active.id) }),
    },
  };
}
