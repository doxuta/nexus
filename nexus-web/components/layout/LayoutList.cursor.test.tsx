// FE-cursor-bootstrap-a (DAILY-QUEUE #43, specs/039-cursor-bootstrap) — the list grid pages by
// cursor whenever its sort is keyset-able, so "Tải thêm" runs past the offset wall (page 1001 →
// 422 PAGE_TOO_DEEP) without skipping or repeating rows while colleagues insert.
//
// Pinned here at the component level (the query builder itself is lib/pagination.fetch.test.ts):
//   • default sort created_at → page 1 is `paginate=cursor`, never `page`;
//   • "Tải thêm" sends `after=<nextCursor>` with the SAME sort/order, and disappears when the
//     server says nextCursor = null;
//   • the header count says "N+ mục" while more pages exist (cursor pages carry no total), the
//     exact count once the chain ends;
//   • clicking a sortable header starts a NEW chain (no `after` — a cursor is bound to its sort);
//   • a sort the engine cannot keyset (plain JSON field) stays offset: `page=1`, server total.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: (action: string) => action === "view", grants: [], isLoading: false }),
}));

const rec = (id: string, name: string) => ({ id, data: { name, stage: "new" }, version: 1 });
const cursorCalls: string[] = [];
const offsetCalls: string[] = [];
const cursorPage = vi.fn();
const offsetPage = vi.fn();
const getLayoutMock = vi.fn();

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsCursorPage: (_e: string, q: string) => {
      cursorCalls.push(q);
      return cursorPage(q);
    },
    listRecordsPage: (_e: string, q: string) => {
      offsetCalls.push(q);
      return offsetPage(q);
    },
    listFields: vi.fn().mockResolvedValue([
      { id: "f1", name: "name", label: "Tên", fieldType: "text" },
      { id: "f2", name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"], indexed: true } },
    ]),
    getLayout: (...a: unknown[]) => getLayoutMock(...a),
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
  };
});

import { LayoutList } from "./LayoutList";

const layout = (defaultSort: { field: string; order: "asc" | "desc" }) => ({
  entityName: "crm_contacts",
  views: {
    list: {
      isDefault: false,
      version: 1,
      columns: [{ field: "name", sortable: false }, { field: "stage", sortable: true }],
      defaultSort,
    },
  },
});

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="crm_contacts" onCreate={() => {}} />
    </QueryClientProvider>,
  );
}

const q = (s: string | undefined) => new URLSearchParams(s ?? "");

beforeEach(() => {
  cursorCalls.length = 0;
  offsetCalls.length = 0;
  cursorPage.mockReset();
  offsetPage.mockReset();
  getLayoutMock.mockReset();
  window.localStorage.clear();
  window.history.replaceState(null, "", "/crm_contacts");
});

describe("LayoutList — cursor pagination (keyset-able sort)", () => {
  it("opens the chain with paginate=cursor, follows nextCursor with the same sort, stops at null", async () => {
    getLayoutMock.mockResolvedValue(layout({ field: "created_at", order: "desc" }));
    cursorPage.mockImplementation(async (s: string) =>
      q(s).get("after") === "c1"
        ? { records: [rec("r3", "Cường")], limit: 50, nextCursor: null }
        : { records: [rec("r1", "An"), rec("r2", "Bình")], limit: 50, nextCursor: "c1" },
    );
    renderList();

    expect(await screen.findByText("An", {}, { timeout: 3000 })).toBeInTheDocument();
    const first = q(cursorCalls[0]);
    expect(first.get("paginate")).toBe("cursor");
    expect(first.get("sort")).toBe("created_at");
    expect(first.get("order")).toBe("desc");
    expect(first.has("page")).toBe(false);
    expect(first.has("after")).toBe(false);
    expect(offsetCalls).toHaveLength(0);

    // No total in cursor mode: "2+ mục" while more exist.
    expect(screen.getByText(/2\+ mục/)).toBeInTheDocument();
    expect(screen.getByText("Đã tải 2 bản ghi")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Tải thêm/ }));
    expect(await screen.findByText("Cường")).toBeInTheDocument();
    const second = q(cursorCalls.at(-1));
    expect(second.get("after")).toBe("c1");
    expect(second.get("sort")).toBe("created_at");
    expect(second.get("order")).toBe("desc");
    expect(second.has("page")).toBe(false);

    // Chain ended: exact count, no more "Tải thêm".
    await waitFor(() => expect(screen.getByText(/^3 mục/)).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /Tải thêm/ })).toBeNull();
  });

  it("a header sort starts a NEW chain: paginate=cursor with the new sort and no after", async () => {
    getLayoutMock.mockResolvedValue(layout({ field: "created_at", order: "desc" }));
    cursorPage.mockResolvedValue({ records: [rec("r1", "An")], limit: 50, nextCursor: "c1" });
    renderList();

    fireEvent.click(await screen.findByRole("button", { name: /Giai đoạn/ }, { timeout: 3000 }));
    await waitFor(() => expect(q(cursorCalls.at(-1)).get("sort")).toBe("stage"));
    const chain = q(cursorCalls.at(-1));
    expect(chain.get("paginate")).toBe("cursor");
    expect(chain.get("order")).toBe("asc");
    expect(chain.has("after")).toBe(false);
    expect(chain.has("page")).toBe(false);
  });
});

describe("LayoutList — offset pagination (sort the engine cannot keyset)", () => {
  it("stays on ?page= with the server total", async () => {
    getLayoutMock.mockResolvedValue(layout({ field: "name", order: "asc" }));
    offsetPage.mockImplementation(async (s: string) =>
      q(s).get("page") === "2"
        ? { records: [rec("r3", "Cường")], pagination: { page: 2, limit: 50, total: 3, totalPages: 2 } }
        : { records: [rec("r1", "An"), rec("r2", "Bình")], pagination: { page: 1, limit: 50, total: 3, totalPages: 2 } },
    );
    renderList();

    expect(await screen.findByText("An", {}, { timeout: 3000 })).toBeInTheDocument();
    expect(cursorCalls).toHaveLength(0);
    expect(q(offsetCalls[0]).get("page")).toBe("1");
    expect(q(offsetCalls[0]).has("paginate")).toBe(false);
    expect(screen.getByText(/^3 mục/)).toBeInTheDocument();
    expect(screen.getByText("Đã tải 2 / 3 bản ghi")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Tải thêm/ }));
    expect(await screen.findByText("Cường")).toBeInTheDocument();
    expect(q(offsetCalls.at(-1)).get("page")).toBe("2");
    await waitFor(() => expect(screen.queryByRole("button", { name: /Tải thêm/ })).toBeNull());
  });
});
