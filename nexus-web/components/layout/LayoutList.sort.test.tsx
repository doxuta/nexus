// DAILY-QUEUE #4 (sortable-columns-from-grid) — the click is the contract.
//
// `sortable` on a list column is stamped by the SERVER at serve time (GET /metadata/layouts/:entity)
// from the engine's ?sort= allowlist; the renderer never computes the rule itself. What the list
// must do with that flag: a served sortable:true header is a live sort control whose click issues the
// exact request the engine answers 200 to (?sort=<field>&order=asc, then desc on the second click)
// and whose header says so (aria-sort); a served sortable:false header offers no sort control at
// all — no request, no blank list.
//
// Since specs/038 T022 the header is the SLDS data table's: a sortable column is an
// `a.slds-th__action[role=button]` inside `th[aria-sort]`, a non-sortable one plain text. Since
// FE-cursor-bootstrap-a a sort on an indexed column pages by cursor, so the request may go through
// listRecordsCursorPage — both fetchers record into `sent`.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: (action: string) => action === "view", grants: [], isLoading: false }),
}));

const sent: string[] = [];
const rows = () => [
  { id: "r1", data: { name: "A", stage: "won" }, createdAt: "2026-09-07T00:00:00Z", updatedAt: "2026-09-07T00:00:00Z" },
  { id: "r2", data: { name: "B", stage: "new" }, createdAt: "2026-09-07T00:00:00Z", updatedAt: "2026-09-07T00:00:00Z" },
];
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsPage: async (_e: string, q: string) => {
      sent.push(q);
      return { records: rows(), pagination: { page: 1, limit: 25, total: 2, totalPages: 1 } };
    },
    listRecordsCursorPage: async (_e: string, q: string) => {
      sent.push(q);
      return { records: rows(), limit: 50, nextCursor: null };
    },
    listFields: vi.fn().mockResolvedValue([
      { id: "f1", name: "name", label: "Tên cơ hội", fieldType: "text" },
      { id: "f2", name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"], indexed: true } },
    ]),
    // Served layout: the server already decided — name (plain text) false, stage (indexed) true.
    getLayout: vi.fn().mockResolvedValue({
      entityName: "crm_opportunities",
      views: {
        list: {
          isDefault: false,
          version: 1,
          columns: [
            { field: "name", sortable: false },
            { field: "stage", sortable: true },
          ],
          defaultSort: { field: "created_at", order: "desc" },
        },
      },
    }),
    // Hermetic: SavedViews and useEntities call these on mount — mock them or the unit test hits a real :8080.
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
  };
});

import { LayoutList } from "./LayoutList";

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="crm_opportunities" onCreate={() => {}} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  sent.length = 0;
});

describe("LayoutList — column headers follow the SERVED sortable flag", () => {
  it("a served sortable:true header is clickable and asks the server for ?sort=<field>&order=asc, then desc", async () => {
    renderList();
    const stage = await screen.findByRole("button", { name: /Giai đoạn/ }, { timeout: 3000 });
    expect(stage).toBeEnabled();
    const header = stage.closest("th")!;
    expect(header).toHaveAttribute("aria-sort", "none");
    const callsBefore = sent.length;

    fireEvent.click(stage);
    await waitFor(() => expect(sent.length).toBeGreaterThan(callsBefore));
    const q1 = new URLSearchParams(sent.at(-1));
    expect(q1.get("sort")).toBe("stage");
    expect(q1.get("order")).toBe("asc");
    await waitFor(() => expect(screen.getByRole("button", { name: /Giai đoạn/ }).closest("th")).toHaveAttribute("aria-sort", "ascending"));

    fireEvent.click(screen.getByRole("button", { name: /Giai đoạn/ }));
    await waitFor(() => expect(sent.length).toBeGreaterThan(callsBefore + 1));
    const q2 = new URLSearchParams(sent.at(-1));
    expect(q2.get("sort")).toBe("stage");
    expect(q2.get("order")).toBe("desc");
    await waitFor(() => expect(screen.getByRole("button", { name: /Giai đoạn/ }).closest("th")).toHaveAttribute("aria-sort", "descending"));
  });

  it("a served sortable:false header offers no sort control — click sends nothing", async () => {
    renderList();
    const name = await screen.findByRole("columnheader", { name: /Tên cơ hội/ }, { timeout: 3000 });
    expect(within(name).queryByRole("button")).toBeNull();
    expect(name).not.toHaveAttribute("aria-sort");
    const callsBefore = sent.length;
    fireEvent.click(within(name).getByText("Tên cơ hội"));
    // Nothing fires: no sort request, and no request at all was issued by the click.
    await new Promise((r) => setTimeout(r, 50));
    expect(sent.length).toBe(callsBefore);
    for (const q of sent) {
      expect(new URLSearchParams(q).get("sort")).not.toBe("name");
    }
  });
});
