// DAILY-QUEUE #279 — Kanban lanes must show the SAME record set as the table: the list's active
// filter + search ride on every lane query, and a filter on the lane field itself never gets
// overridden by the lane condition.

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


beforeEach(() => {
  window.history.replaceState(null, "", "/");
});

describe("kanban lanes carry the list filter and search", () => {
  it("every lane query has the URL filter, q and its own lane condition", async () => {
    window.history.replaceState(null, "", "/?channel%5Beq%5D=email&q=abc");
    getLayoutMock.mockResolvedValue(layoutWith({ kanbanField: "status" }));
    renderList();
    await showAs("Kanban");
    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("status%5Beq%5D=open"))).toBe(true);
    });
    const lanes = queriesSent().filter((q) => q.includes("status%5B"));
    expect(lanes.length).toBeGreaterThanOrEqual(3);
    for (const q of lanes) {
      expect(q).toContain("channel%5Beq%5D=email");
      expect(q).toContain("q=abc");
    }
  });

  it("a filter pinning the lane field to one value leaves the other lanes unqueried", async () => {
    window.history.replaceState(null, "", "/?status%5Beq%5D=open");
    getLayoutMock.mockResolvedValue(layoutWith({ kanbanField: "status" }));
    renderList();
    await showAs("Kanban");
    await waitFor(() => {
      expect(queriesSent().some((q) => q.includes("status%5Beq%5D=open"))).toBe(true);
    });
    expect(queriesSent().some((q) => q.includes("status%5Beq%5D=closed"))).toBe(false);
    expect(queriesSent().some((q) => q.includes("status%5Bisnull%5D=1"))).toBe(false);
  });
});
