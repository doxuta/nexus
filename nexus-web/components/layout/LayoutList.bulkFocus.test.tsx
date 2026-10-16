// The bulk-selection bar unmounts as soon as the selection clears (specs/038 T022 review). "Bỏ chọn"
// removes itself, a mass delete / mass edit refreshes the list and clears the selection under the
// button that opened its dialog — every time focus fell to <body>. It now lands on the grid's
// select-all checkbox, the stable control the selection started from.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

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

import { LayoutList } from "./LayoutList";

async function renderWithSelection() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="crm_contacts" onCreate={() => {}} />
    </QueryClientProvider>,
  );
  await screen.findByText("An", {}, { timeout: 3000 });
  await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 1" }));
  expect(screen.getByText("1 đã chọn")).toBeInTheDocument();
}
const selectAll = () => screen.getByRole("checkbox", { name: "Chọn tất cả" });

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/crm_contacts");
  // Slower than the prompt's focus return, like a real request: the prompt hands focus back to
  // "Xoá hàng loạt" first, and the refresh removes that button afterwards.
  bulkDeleteRecords.mockReset().mockImplementation(() => new Promise((r) => setTimeout(() => r({ succeeded: 1, failed: 0, results: [] }), 80)));
  bulkUpdateRecords.mockReset().mockResolvedValue({ succeeded: 1, failed: 0, results: [] });
});

describe("LayoutList — focus when the bulk bar goes away", () => {
  it("Bỏ chọn: focus goes to the select-all checkbox", async () => {
    await renderWithSelection();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ chọn" }));
    expect(screen.queryByText("1 đã chọn")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(selectAll()));
  });

  it("mass delete: after the refresh clears the selection, focus is on select-all", async () => {
    await renderWithSelection();
    await userEvent.click(screen.getByRole("button", { name: "Xoá hàng loạt" }));
    const prompt = await screen.findByRole("alertdialog");
    fireEvent.click(Array.from(prompt.querySelectorAll("button")).find((b) => b.textContent === "Xoá hàng loạt")!);
    await waitFor(() => expect(bulkDeleteRecords).toHaveBeenCalledWith("crm_contacts", ["r1"]));
    await waitFor(() => expect(screen.queryByText("1 đã chọn")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(selectAll()));
  });

  it("mass edit: closing the result dialog after the refresh returns focus to select-all", async () => {
    await renderWithSelection();
    await userEvent.click(screen.getByRole("button", { name: "Sửa hàng loạt" }));
    fireEvent.keyDown(await screen.findByRole("button", { name: /Thêm trường cần sửa/ }), { key: "Enter" });
    fireEvent.click(screen.getByRole("menuitem", { name: /^Tên/ }));
    fireEvent.change(screen.getByPlaceholderText("Nhập…"), { target: { value: "Mới" } });
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật 1 bản ghi/ }));
    await waitFor(() => expect(bulkUpdateRecords).toHaveBeenCalled());
    // The list refreshed under the dialog: selection cleared, "Sửa hàng loạt" is gone.
    await waitFor(() => expect(screen.queryByText("1 đã chọn")).toBeNull());
    await userEvent.click(screen.getByRole("button", { name: "Đóng" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(selectAll()));
  });
});
