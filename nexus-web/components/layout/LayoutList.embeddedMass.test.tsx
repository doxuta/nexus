import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const GUARD = vi.hoisted(() => ({ assertScope: () => true }));
vi.mock("@/components/studio/operate/useScopeGate", () => ({ useScopeGate: () => GUARD }));
vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => true, grants: [], isLoading: false }),
}));

const { ROWS, bulkDeleteRecords, bulkUpdateRecords } = vi.hoisted(() => ({
  ROWS: [
    { id: "r1", data: { name: "An" }, version: 1 },
    { id: "r2", data: { name: "Bình" }, version: 1 },
  ],
  bulkDeleteRecords: vi.fn(),
  bulkUpdateRecords: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsCursorPage: vi.fn().mockResolvedValue({ records: ROWS, limit: 50, nextCursor: null }),
    listRecordsPage: vi.fn().mockResolvedValue({ records: ROWS, pagination: { page: 1, limit: 50, total: 2, totalPages: 1 } }),
    listFields: vi.fn().mockResolvedValue([{ id: "f1", name: "name", label: "Tên", fieldType: "text" }]),
    getLayout: vi.fn().mockResolvedValue({
      entityName: "crm_contacts",
      views: { list: { isDefault: false, version: 1, columns: [{ field: "name", sortable: false }], defaultSort: { field: "created_at", order: "desc" } } },
    }),
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
    bulkDeleteRecords: (...a: unknown[]) => bulkDeleteRecords(...a),
    bulkUpdateRecords: (...a: unknown[]) => bulkUpdateRecords(...a),
  };
});

import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { LayoutList } from "./LayoutList";

// specs/078 T067: in the Studio frame mass edit/delete stay in the page (no dialog); the customer face is covered by
// LayoutList.bulkFocus/massVeto, which still expect the modal and the prompt.
async function renderEmbeddedWithSelection() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <EmbeddedSurface>
        <LayoutList entity="crm_contacts" onCreate={() => {}} />
      </EmbeddedSurface>
    </QueryClientProvider>,
  );
  await screen.findByText("An", {}, { timeout: 3000 });
  await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 1" }));
  await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 2" }));
}

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/crm_contacts");
  bulkDeleteRecords.mockReset().mockResolvedValue({ succeeded: 2, failed: 0, results: [] });
  bulkUpdateRecords.mockReset().mockResolvedValue({ succeeded: 2, failed: 0, results: [] });
});

describe("LayoutList embedded — mass ops in the page", () => {
  it("'Sửa 2' opens MassEditBand, not a dialog", async () => {
    await renderEmbeddedWithSelection();
    await userEvent.click(screen.getByRole("button", { name: "Sửa 2" }));
    expect(screen.getByRole("group", { name: "Sửa 2" })).toBeInTheDocument();
    expect(document.querySelector("[role=dialog],[role=alertdialog]")).toBeNull();
  });

  it("'Xoá 2' asks inline (no promised number of days), then deletes with the scope guard", async () => {
    await renderEmbeddedWithSelection();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 2" }));
    expect(screen.getByText("Chuyển 2 bản ghi vào thùng rác; khôi phục được từ thùng rác.")).toBeInTheDocument();
    expect(document.querySelector("[role=dialog],[role=alertdialog]")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Chuyển 2 vào thùng rác" }));
    await waitFor(() => expect(bulkDeleteRecords).toHaveBeenCalledWith("crm_contacts", ["r1", "r2"], GUARD));
  });
});
