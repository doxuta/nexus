// specs/048 T135/T137 — a mass delete / mass edit where a before-save (or before-delete) rule
// refuses some rows. The list's result strip used to print the first failed row's CODE —
// "Đã xoá 1/2 bản ghi · 1 lỗi (RECORD_HOOK_VETO)" — engine vocabulary instead of the rule's
// sentence (US4 scenario 3). The mass-edit dialog listed each failed row under an error-code badge.
// Both now say what the rule says, and no code.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
    { id: "opp-won-0001", data: { name: "Cơ hội Thắng" }, version: 1 },
    { id: "opp-open-0002", data: { name: "Cơ hội đang tìm" }, version: 1 },
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
      entityName: "crm_opportunities",
      views: { list: { isDefault: false, version: 1, columns: [{ field: "name", sortable: false }], defaultSort: { field: "created_at", order: "desc" } } },
    }),
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
    bulkDeleteRecords: (...a: unknown[]) => bulkDeleteRecords(...a),
    bulkUpdateRecords: (...a: unknown[]) => bulkUpdateRecords(...a),
  };
});

import { LayoutList } from "./LayoutList";

const vetoRow = (id: string, message: string) => ({
  id,
  ok: false,
  code: "RECORD_HOOK_VETO",
  error: message,
  details: { violations: [{ message }] },
});

async function renderAndSelectBoth() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="crm_opportunities" onCreate={() => {}} />
    </QueryClientProvider>,
  );
  await screen.findByText("Cơ hội Thắng", {}, { timeout: 3000 });
  await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 1" }));
  await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 2" }));
  expect(screen.getByText("2 đã chọn")).toBeInTheDocument();
}

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/crm_opportunities");
  bulkDeleteRecords.mockReset();
  bulkUpdateRecords.mockReset();
});

describe("LayoutList — a mass operation partly refused by a rule", () => {
  it("mass delete 2, 1 refused ⇒ the strip carries the rule's sentence, not RECORD_HOOK_VETO", async () => {
    bulkDeleteRecords.mockResolvedValue({
      succeeded: 1,
      failed: 1,
      results: [vetoRow("opp-won-0001", "Không xoá cơ hội đã Thắng"), { id: "opp-open-0002", ok: true }],
    });
    await renderAndSelectBoth();
    await userEvent.click(screen.getByRole("button", { name: "Xoá hàng loạt" }));
    const prompt = await screen.findByRole("alertdialog");
    fireEvent.click(Array.from(prompt.querySelectorAll("button")).find((b) => b.textContent === "Xoá hàng loạt")!);
    await waitFor(() => expect(bulkDeleteRecords).toHaveBeenCalledWith("crm_opportunities", ["opp-won-0001", "opp-open-0002"]));

    const strip = await screen.findByText(/Đã xoá 1\/2 bản ghi/, {}, { timeout: 3000 });
    expect(strip.textContent).toContain("Không xoá cơ hội đã Thắng");
    expect(document.body.textContent).not.toMatch(/RECORD_HOOK_VETO/);
  });

  it("mass edit 2, 1 refused ⇒ the strip and the dialog's row both carry the sentence, no code", async () => {
    bulkUpdateRecords.mockResolvedValue({
      succeeded: 1,
      failed: 1,
      results: [vetoRow("opp-won-0001", "Không sửa cơ hội đã Thắng"), { id: "opp-open-0002", ok: true }],
    });
    await renderAndSelectBoth();
    await userEvent.click(screen.getByRole("button", { name: "Sửa hàng loạt" }));
    fireEvent.keyDown(await screen.findByRole("button", { name: /Thêm trường cần sửa/ }), { key: "Enter" });
    fireEvent.click(screen.getByRole("menuitem", { name: /^Tên/ }));
    fireEvent.change(screen.getByPlaceholderText("Nhập…"), { target: { value: "Mới" } });
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật 2 bản ghi/ }));
    await waitFor(() => expect(bulkUpdateRecords).toHaveBeenCalled());

    // The list's own strip (the dialog carries its own "Đã cập nhật 1/2" summary).
    const strips = await screen.findAllByText(/Đã cập nhật 1\/2 bản ghi/, {}, { timeout: 3000 });
    const strip = strips.find((el) => !el.closest("[role='dialog']"))!;
    expect(strip.textContent).toContain("Không sửa cơ hội đã Thắng");
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Không sửa cơ hội đã Thắng")).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/RECORD_HOOK_VETO/);
  });
});
