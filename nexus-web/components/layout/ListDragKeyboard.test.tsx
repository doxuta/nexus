// Keyboard drag on the record list's board and calendar (specs/038 T022 review). With
// `pointerWithin` alone a keyboard drag never had a drop target (no pointer coordinates), and on the
// calendar the pill's wrapper was the dnd-kit role=button with the record link taken out of the tab
// order: Enter picked the pill up instead of opening the record.
//
// happy-dom has no layout, so element boxes are faked from the DOM position: calendar day cells are
// 100×80 on a grid, board lanes 300px apart; anything inside is a small box near the top of its cell
// or lane; the drag overlay (position: fixed) reports the box its style gives it.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";

vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => true, grants: [], isLoading: false }),
}));

const updateRecord = vi.fn().mockResolvedValue(undefined);
const listRecordsPage = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsPage: (...a: unknown[]) => listRecordsPage(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
  };
});

import { CalendarView } from "./CalendarView";
import { KanbanBoard } from "./KanbanBoard";

const box = (left: number, top: number, width: number, height: number) =>
  ({ x: left, y: top, left, top, width, height, right: left + width, bottom: top + height, toJSON: () => ({}) }) as DOMRect;

function fakeBox(el: Element): DOMRect {
  const fixed = el.closest<HTMLElement>("[style*='fixed']");
  if (fixed) return box(parseFloat(fixed.style.left) || 0, parseFloat(fixed.style.top) || 0, parseFloat(fixed.style.width) || 0, parseFloat(fixed.style.height) || 0);
  const td = el.closest("td");
  if (td) {
    const tr = td.parentElement!;
    const left = Array.from(tr.children).indexOf(td) * 100;
    const top = 40 + Array.from(tr.parentElement!.children).indexOf(tr) * 80;
    return el === td ? box(left, top, 100, 80) : box(left + 5, top + 20, 60, 16);
  }
  const lane = el.closest(".w-72");
  if (lane) {
    const left = Array.from(lane.parentElement!.children).indexOf(lane) * 300;
    return el.classList.contains("slds-scrollable_y") ? box(left, 60, 288, 400) : box(left + 10, 70, 260, 60);
  }
  return box(0, 0, 0, 0);
}

beforeEach(() => {
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    return fakeBox(this);
  });
  updateRecord.mockClear();
  listRecordsPage.mockReset();
});
afterEach(() => vi.restoreAllMocks());

const tick = () => act(() => new Promise((r) => setTimeout(r, 30)));
async function press(el: Element, code: string) {
  fireEvent.keyDown(el, { code, key: code === "Space" ? " " : code });
  await tick();
}

function wrap(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

// A save that outlasts an animation frame, like a real PUT: dnd-kit returns focus one frame after the drop.
const slowSave = () => new Promise<void>((r) => setTimeout(r, 80));
const page = (records: unknown[]) => ({ records, pagination: { page: 1, limit: 100, total: records.length, totalPages: 1 } });

describe("CalendarView — keyboard", () => {
  const now = new Date();
  const day = (d: number) => `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
  const FIELDS = [
    { id: "f1", name: "name", label: "Tên", fieldType: "text" },
    { id: "f2", name: "due", label: "Hạn", fieldType: "date" },
  ];

  async function renderCalendar() {
    // A tiny server: the refetch after a move returns the saved day.
    let due = day(10);
    listRecordsPage.mockImplementation(() => Promise.resolve(page([{ id: "r1", data: { name: "Deal A", due }, version: 1 }])));
    updateRecord.mockImplementation((_e: string, _id: string, patch: { due: string }) => {
      due = patch.due;
      return slowSave();
    });
    const onOpen = vi.fn();
    wrap(<CalendarView entity="crm_deals" fields={FIELDS as never} onOpen={onOpen} />);
    const link = await screen.findByRole("link", { name: "Deal A" });
    return { link, onOpen };
  }

  it("the record link stays a tab stop outside any role=button; a separate grip is the drag handle", async () => {
    const { link, onOpen } = await renderCalendar();
    expect(link).toHaveAttribute("href", "/crm_deals/r1");
    expect(link).not.toHaveAttribute("tabindex", "-1");
    expect(link.closest("[role='button']")).toBeNull();
    const pill = link.closest(".slds-pill")!;
    const grip = within(pill as HTMLElement).getByRole("button", { name: "Kéo để dời ngày: Deal A" });
    expect(grip).toHaveAttribute("aria-roledescription", "draggable");
    // Enter on the link is the link's: it opens the record, it does not pick the pill up.
    await press(link, "Enter");
    expect(grip).not.toHaveAttribute("aria-pressed", "true");
    fireEvent.click(link);
    expect(onOpen).toHaveBeenCalledWith("r1");
  });

  it("Space on the grip, ArrowDown, Space moves the record to the same weekday next week", async () => {
    const { link } = await renderCalendar();
    const grip = within(link.closest(".slds-pill") as HTMLElement).getByRole("button", { name: /Kéo để dời ngày/ });
    grip.focus();
    await press(grip, "Space");
    await press(grip, "ArrowDown");
    await press(grip, "Space");
    await waitFor(() => expect(updateRecord).toHaveBeenCalledWith("crm_deals", "r1", { due: day(17) }));
    // Focus follows the pill to its new day (it is not dropped on <body>).
    await waitFor(() => expect(document.activeElement).toHaveAccessibleName("Kéo để dời ngày: Deal A"));
    expect(document.activeElement!.closest("td")!.textContent).toMatch(/^17/);
  });
});

describe("KanbanBoard — keyboard", () => {
  const FIELDS = [
    { id: "f1", name: "name", label: "Tên", fieldType: "text" },
    { id: "f2", name: "stage", label: "Giai đoạn", fieldType: "picklist", isRequired: true, options: { values: ["new", "won"] } },
  ];
  const R1 = { id: "r1", data: { name: "Deal A", stage: "new" }, version: 1 };

  it("Space on the grip, ArrowRight, Space moves the card to the next lane", async () => {
    let stage = "new";
    // Lane queries answer after network-like delays, one lane at a time (the "Chưa đặt" lane first,
    // the card's old lane last), so a refetch is neither instant nor atomic.
    listRecordsPage.mockImplementation((_e: string, q: string) => {
      const rows = q.includes(`stage%5Beq%5D=${stage}`) ? [{ ...R1, data: { ...R1.data, stage } }] : [];
      const delay = q.includes("isnull") ? 10 : q.includes("stage%5Beq%5D=new") ? 60 : 35;
      return new Promise((r) => setTimeout(() => r(page(rows)), delay));
    });
    updateRecord.mockImplementation((_e: string, _id: string, patch: { stage: string }) => {
      stage = patch.stage;
      return slowSave();
    });
    // Like LayoutList: the parent holds the loaded page and takes the moved card back (onRecordsChange).
    function Board() {
      const [records, setRecords] = useState([R1] as never[]);
      return <KanbanBoard entity="crm_deals" records={records} fields={FIELDS as never} onRecordsChange={setRecords as never} />;
    }
    wrap(<Board />);
    await screen.findByRole("link", { name: "Deal A" });
    await waitFor(() => expect(listRecordsPage).toHaveBeenCalledTimes(3));
    for (let i = 0; i < 8; i++) await act(() => new Promise((r) => setTimeout(r, 15))); // every lane answered
    const grip = screen.getByRole("button", { name: "Kéo để chuyển cột" });
    grip.focus();
    await press(grip, "Space");
    await press(grip, "ArrowRight");
    await press(grip, "Space");
    await waitFor(() => expect(updateRecord).toHaveBeenCalledWith("crm_deals", "r1", { stage: "won" }));
    // Let the save land, the parent sync and the lanes refetch — in small steps: one long act() would
    // batch every render in between and hide a card that jumps back to its old lane for a moment.
    for (let i = 0; i < 20; i++) await act(() => new Promise((r) => setTimeout(r, 15)));
    // Focus is on the card's grip, now in the second lane — the card never jumped back to its old
    // lane in between (the parent's sync used to reseed the board from the not-yet-refetched lanes).
    expect(document.activeElement).toHaveAccessibleName("Kéo để chuyển cột");
    expect(Array.from(document.querySelectorAll(".w-72")).indexOf(document.activeElement!.closest(".w-72")!)).toBe(1);
  });
});
