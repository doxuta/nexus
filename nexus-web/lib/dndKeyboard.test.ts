// Keyboard drag on the list's board and calendar (@dnd-kit/core 6.3.1). A keyboard drag carries no
// pointer coordinates, so `pointerWithin` alone never found a drop target: Space picked the item up,
// the arrows moved the overlay, and Space dropped it on nothing.
import { describe, expect, it } from "vitest";
import type { ClientRect, SensorContext, UniqueIdentifier } from "@dnd-kit/core";
import { dndAccessibility, jumpToDroppable, pointerOrClosest } from "@/lib/dndKeyboard";

const rect = (left: number, top: number, width: number, height: number): ClientRect => ({
  left,
  top,
  width,
  height,
  right: left + width,
  bottom: top + height,
});

// A 3×2 month-like grid of 100×80 cells; "c-2-1" = column 2, row 1.
const CELLS = new Map<UniqueIdentifier, ClientRect>();
for (let row = 0; row < 2; row++) for (let col = 0; col < 3; col++) CELLS.set(`c-${col}-${row}`, rect(col * 100, row * 80, 100, 80));

function context(collisionRect: ClientRect, disabled: UniqueIdentifier[] = []): SensorContext {
  const enabled = [...CELLS.keys()].filter((id) => !disabled.includes(id)).map((id) => ({ id }));
  return {
    collisionRect,
    droppableRects: CELLS,
    droppableContainers: { getEnabled: () => enabled },
  } as unknown as SensorContext;
}

const key = (code: string) => new KeyboardEvent("keydown", { code, cancelable: true });

function jump(code: string, from: ClientRect, disabled?: UniqueIdentifier[]) {
  const event = key(code);
  const to = jumpToDroppable(event, { active: "r1", context: context(from, disabled), currentCoordinates: { x: from.left, y: from.top } });
  return { to, prevented: event.defaultPrevented };
}

// A 60×16 pill near the top of cell (0,0): centre (40, 18).
const PILL = rect(10, 10, 60, 16);

describe("jumpToDroppable", () => {
  it("an arrow moves the dragged item onto the centre of the next droppable that way (one lane / one day)", () => {
    // Right → cell (1,0), centre (150, 40): the pill's centre moves by (110, 22).
    expect(jump("ArrowRight", PILL).to).toEqual({ x: 120, y: 32 });
    // Down → cell (0,1), centre (50, 120).
    expect(jump("ArrowDown", PILL).to).toEqual({ x: 20, y: 112 });
  });

  it("prefers the same row / column over a nearer-looking diagonal", () => {
    // From the centre of cell (1,0), Right is cell (2,0), not (2,1).
    const centred = rect(120, 22, 60, 36);
    expect(jump("ArrowRight", centred).to).toEqual({ x: 220, y: 22 });
  });

  it("skips disabled droppables", () => {
    expect(jump("ArrowRight", PILL, ["c-1-0"]).to).toEqual({ x: 220, y: 32 });
  });

  it("stays put at the edge, and still keeps the arrow from scrolling the page", () => {
    const r = jump("ArrowLeft", PILL);
    expect(r.to).toEqual({ x: 10, y: 10 });
    expect(r.prevented).toBe(true);
  });

  it("leaves every other key to dnd-kit", () => {
    expect(jump("KeyA", PILL).to).toBeUndefined();
  });
});

describe("pointerOrClosest", () => {
  const droppableContainers = [...CELLS.keys()].map((id) => ({ id, rect: { current: CELLS.get(id)! } }));
  const args = (collisionRect: ClientRect, pointerCoordinates: { x: number; y: number } | null) =>
    ({ active: { id: "r1" }, collisionRect, droppableRects: CELLS, droppableContainers, pointerCoordinates }) as unknown as Parameters<typeof pointerOrClosest>[0];

  it("keyboard drag (no pointer): the droppable nearest the dragged item", () => {
    expect(pointerOrClosest(args(rect(120, 22, 60, 36), null))[0]?.id).toBe("c-1-0");
  });

  it("pointer drag: the droppable under the pointer, not the one nearest the item", () => {
    expect(pointerOrClosest(args(rect(120, 22, 60, 36), { x: 250, y: 100 }))[0]?.id).toBe("c-2-1");
  });
});

describe("dndAccessibility", () => {
  it("announces in Vietnamese with record and target names, never raw ids", () => {
    const a = dndAccessibility(
      (id) => (id === "r1" ? "Deal A" : String(id)),
      (id) => (id === "won" ? "Thắng" : String(id)),
    );
    const active = { id: "r1" } as never;
    expect(a.announcements.onDragStart({ active })).toBe("Đã nhấc Deal A.");
    expect(a.announcements.onDragOver({ active, over: { id: "won" } as never })).toBe("Deal A đang ở trên Thắng.");
    expect(a.announcements.onDragEnd({ active, over: { id: "won" } as never })).toBe("Đã thả Deal A vào Thắng.");
    expect(a.announcements.onDragEnd({ active, over: null })).toBe("Đã thả Deal A ngoài vùng thả — không có gì thay đổi.");
    expect(a.announcements.onDragCancel({ active, over: null })).toBe("Đã huỷ kéo Deal A.");
    expect(a.screenReaderInstructions.draggable).toMatch(/phím cách/);
  });
});
