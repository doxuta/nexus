// specs/048 T133/T134 — a drag refused by a before-save rule on the calendar and the board.
// The toast carries the rule's own sentence (the calendar used to swallow every error behind
// "Không dời được lịch"), and the item goes back where it was.
//
// Same happy-dom harness as ListDragKeyboard.test.tsx: element boxes are faked from DOM position so
// dnd-kit's keyboard sensor finds a drop target.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { ApiError } from "@/lib/api";

vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => true, grants: [], isLoading: false }),
}));

const toast = vi.hoisted(() => ({ error: vi.fn(), success: vi.fn(), info: vi.fn(), warning: vi.fn() }));
vi.mock("@/components/ui/Toaster", () => ({ toast }));

const updateRecord = vi.fn();
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
  updateRecord.mockReset();
  listRecordsPage.mockReset();
  Object.values(toast).forEach((f) => f.mockClear());
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
const page = (records: unknown[]) => ({ records, pagination: { page: 1, limit: 100, total: records.length, totalPages: 1 } });

const VETO = new ApiError(422, "RECORD_HOOK_VETO", "Không dời hạn vào cuối tuần", {
  field: "due",
  violations: [{ field: "due", message: "Không dời hạn vào cuối tuần" }],
});

describe("CalendarView — a move refused by a rule", () => {
  const now = new Date();
  const day = (d: number) => `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
  const FIELDS = [
    { id: "f1", name: "name", label: "Tên", fieldType: "text" },
    { id: "f2", name: "due", label: "Hạn", fieldType: "date" },
  ];

  async function dragDownOneWeek(err: unknown) {
    // The server keeps the old day: the refetch after the refusal puts the pill back on the 10th.
    listRecordsPage.mockImplementation(() => Promise.resolve(page([{ id: "r1", data: { name: "Deal A", due: day(10) }, version: 1 }])));
    updateRecord.mockRejectedValue(err);
    wrap(<CalendarView entity="crm_deals" fields={FIELDS as never} onOpen={() => {}} />);
    const link = await screen.findByRole("link", { name: "Deal A" });
    const grip = within(link.closest(".slds-pill") as HTMLElement).getByRole("button", { name: /Kéo để dời ngày/ });
    grip.focus();
    await press(grip, "Space");
    await press(grip, "ArrowDown");
    await press(grip, "Space");
    await waitFor(() => expect(updateRecord).toHaveBeenCalledWith("crm_deals", "r1", { due: day(17) }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledTimes(1));
    return String(toast.error.mock.calls[0][0]);
  }

  it("toast shows the rule's sentence, not the generic 'could not move', and the pill returns", async () => {
    const msg = await dragDownOneWeek(VETO);
    expect(msg).toContain("Không dời hạn vào cuối tuần");
    expect(msg).not.toContain("Không dời được lịch");
    expect(msg).not.toMatch(/RECORD_HOOK_VETO/);
    await waitFor(() => expect(screen.getByRole("link", { name: "Deal A" }).closest("td")!.textContent).toMatch(/^10/));
  });

  it("503 ⇒ 'busy, retry'", async () => {
    const msg = await dragDownOneWeek(new ApiError(503, "RECORD_HOOKS_UNAVAILABLE", "Hệ thống bận, thử lại sau ít giây"));
    expect(msg).toContain("Hệ thống bận, thử lại sau ít giây");
  });

  it("a network failure keeps the old line", async () => {
    const msg = await dragDownOneWeek(new TypeError("Failed to fetch"));
    expect(msg).toBe("Không dời được lịch. Đã hoàn tác.");
  });
});

describe("KanbanBoard — a move refused by a rule", () => {
  const FIELDS = [
    { id: "f1", name: "name", label: "Tên", fieldType: "text" },
    { id: "f2", name: "stage", label: "Giai đoạn", fieldType: "picklist", isRequired: true, options: { values: ["new", "won"] } },
  ];
  const R1 = { id: "r1", data: { name: "Deal A", stage: "new" }, version: 1 };

  it("toast shows the rule's sentence and the card goes back to its lane", async () => {
    listRecordsPage.mockImplementation((_e: string, q: string) =>
      Promise.resolve(page(q.includes("stage%5Beq%5D=new") ? [R1] : [])),
    );
    updateRecord.mockRejectedValue(
      new ApiError(422, "RECORD_HOOK_VETO", "Chốt Thắng phải có Ngày chốt", {
        violations: [{ message: "Chốt Thắng phải có Ngày chốt" }],
      }),
    );
    function Board() {
      const [records, setRecords] = useState([R1] as never[]);
      return <KanbanBoard entity="crm_deals" records={records} fields={FIELDS as never} onRecordsChange={setRecords as never} />;
    }
    wrap(<Board />);
    await screen.findByRole("link", { name: "Deal A" });
    await waitFor(() => expect(listRecordsPage).toHaveBeenCalledTimes(3));
    for (let i = 0; i < 8; i++) await act(() => new Promise((r) => setTimeout(r, 15)));
    const grip = screen.getByRole("button", { name: "Kéo để chuyển cột" });
    grip.focus();
    await press(grip, "Space");
    await press(grip, "ArrowRight");
    await press(grip, "Space");
    await waitFor(() => expect(updateRecord).toHaveBeenCalledWith("crm_deals", "r1", { stage: "won" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledTimes(1));
    const msg = String(toast.error.mock.calls[0][0]);
    expect(msg).toContain("Chốt Thắng phải có Ngày chốt");
    expect(msg).not.toMatch(/RECORD_HOOK_VETO/);
    for (let i = 0; i < 10; i++) await act(() => new Promise((r) => setTimeout(r, 15)));
    const lanes = Array.from(document.querySelectorAll(".w-72"));
    expect(lanes.indexOf(screen.getByRole("link", { name: "Deal A" }).closest(".w-72")!)).toBe(0); // back in "new"
  });
});
