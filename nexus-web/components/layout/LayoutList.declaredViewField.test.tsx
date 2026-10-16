// DAILY-QUEUE #5 (kanban-calendar-declared-field) — the board groups by the column the maker
// DECLARED, not by whichever picklist metadata happened to list first.
//
// Before this slice KanbanBoard called pickKanbanField(fields) with no preference and CalendarView
// took `fields.find(date|datetime)`, so a Ticket entity with priority/channel/status and
// reported_at/due_date came out grouped by "Kênh tiếp nhận" and laid out on "Ngày tiếp nhận" —
// wrong for the business, with no way for the maker to say otherwise. The layout now carries
// kanbanField/calendarField and the two views must honour them.
//
// The fixture is deliberately built so the declared field is NOT the first of its kind: on an
// entity with one picklist, "grouped by the declared field" and "grouped by the first field" look
// identical and the test proves nothing.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: (action: string) => action === "view", grants: [], isLoading: false }),
}));

const listRecordsPageMock = vi.fn();
const getLayoutMock = vi.fn();

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsPage: (...args: unknown[]) => listRecordsPageMock(...args),
    // The TABLE's own first page pages by cursor (default sort created_at, FE-cursor-bootstrap-a);
    // the kanban lanes and the calendar month still query through listRecordsPage above.
    listRecordsCursorPage: vi.fn().mockResolvedValue({ records: [], limit: 50, nextCursor: null }),
    // Inlined (not a top-level const): vi.mock factories are hoisted above module init.
    // THREE picklists and TWO date fields, so "declared" and "first of its kind" differ.
    // due_date is deliberately NOT indexed: the calendar filters with [between] (a JSON path with
    // no sortable gate) and sorts by created_at, so a non-indexed date must still render a month.
    listFields: vi.fn().mockResolvedValue([
  { id: "f1", name: "subject", label: "Tiêu đề", fieldType: "text" },
  { id: "f2", name: "priority", label: "Mức ưu tiên", fieldType: "picklist", options: { values: ["low", "high"] } },
  { id: "f3", name: "channel", label: "Kênh tiếp nhận", fieldType: "picklist", options: { values: ["email", "phone"] } },
  { id: "f4", name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["open", "closed"] } },
  { id: "f5", name: "reported_at", label: "Ngày tiếp nhận", fieldType: "date" },
  // due_date is deliberately NOT indexed: the calendar filters with [between] (a JSON path, no
  // sortable gate) and sorts by created_at, so a non-indexed date must still render a full month.
  { id: "f6", name: "due_date", label: "Hạn xử lý", fieldType: "datetime" },
]),
    getLayout: (...args: unknown[]) => getLayoutMock(...args),
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
  };
});

import { LayoutList } from "./LayoutList";

const layoutWith = (extra: Record<string, unknown>) => ({
  entityName: "tk_tickets",
  views: {
    list: {
      isDefault: false,
      version: 1,
      columns: [{ field: "subject", sortable: false }],
      defaultSort: { field: "created_at", order: "desc" },
      ...extra,
    },
  },
});

const emptyPage = { records: [], pagination: { page: 1, limit: 25, total: 0, totalPages: 1 } };

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="tk_tickets" onCreate={() => {}} />
    </QueryClientProvider>,
  );
}

/** Switch the list's view through the object-home "Hiển thị dạng" menu (SLDS menus/dropdown). */
async function showAs(view: "Kanban" | "Lịch") {
  fireEvent.keyDown(await screen.findByRole("button", { name: /Hiển thị dạng/ }), { key: "Enter" });
  fireEvent.click(screen.getByRole("menuitemcheckbox", { name: view }));
}

/** Every query string LayoutList's children asked the records endpoint for. */
const queriesSent = () => listRecordsPageMock.mock.calls.map((c) => String(c[1] ?? ""));

beforeEach(() => {
  listRecordsPageMock.mockReset();
  listRecordsPageMock.mockResolvedValue(emptyPage);
  getLayoutMock.mockReset();
  window.localStorage.clear();
});

describe("declared kanbanField", () => {
  it("groups the board by the declared picklist, not the first one", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({ kanbanField: "status" }));
    renderList();
    await showAs("Kanban");

    // One lane query per declared value, plus the empty bucket — all on `status`.
    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("status%5Beq%5D=open"))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("status%5Beq%5D=closed"))).toBe(true);
    expect(queriesSent().some((q) => q.includes("status%5Bisnull%5D=1"))).toBe(true);
    // The old guess (first picklist) must not drive a single lane query.
    expect(queriesSent().some((q) => q.includes("priority%5B"))).toBe(false);
  });

  it("falls back to the first picklist when nothing is declared", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({}));
    renderList();
    await showAs("Kanban");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("priority%5Beq%5D=low"))).toBe(true);
    });
  });
});

describe("a declaration that no longer fits the fields on hand", () => {
  // Reachable two ways: field ACL stripped the field out of /metadata/fields for THIS user, or the
  // field was deleted / retyped after the layout was saved. The renderer must fall back to its
  // guess — never build a query on a field it does not have.
  it("falls back when kanbanField names a field this user cannot see", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({ kanbanField: "khong_thay_duoc" }));
    renderList();
    await showAs("Kanban");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("priority%5Beq%5D=low"))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("khong_thay_duoc"))).toBe(false);
  });

  it("falls back when kanbanField names a field of the wrong type", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({ kanbanField: "subject" }));
    renderList();
    await showAs("Kanban");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("priority%5Beq%5D=low"))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("subject%5B"))).toBe(false);
  });

  it("falls back when calendarField names a field of the wrong type", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({ calendarField: "status" }));
    renderList();
    await showAs("Lịch");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("reported_at%5Bbetween%5D="))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("status%5Bbetween%5D="))).toBe(false);
  });
});

describe("declared calendarField", () => {
  it("lays the month out on the declared date field, not the first one", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({ calendarField: "due_date" }));
    renderList();
    await showAs("Lịch");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("due_date%5Bbetween%5D="))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("reported_at%5Bbetween%5D="))).toBe(false);
  });

  it("falls back to the first date field when nothing is declared", async () => {
    getLayoutMock.mockResolvedValue(layoutWith({}));
    renderList();
    await showAs("Lịch");

    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("reported_at%5Bbetween%5D="))).toBe(true);
    });
  });
});
