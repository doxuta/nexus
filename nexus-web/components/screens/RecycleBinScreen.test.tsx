// RecycleBinScreen on SLDS (specs/038 T027): object home (page-headers/object-home) over the kit
// DataTable. Deleted rows render through the shared list-column pipeline, "Khôi phục" is the row
// action (menus/dropdown) confirmed in a prompt, and without the `delete` grant the bin is read-only.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApiRecord, EntityLayout, FieldMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  canDelete: true,
  listDeletedPage: vi.fn(),
  restoreRecord: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: false } }) }));
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: (action: string) => action !== "delete" || seam.canDelete }),
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" }] }),
}));
vi.mock("@/components/ui/Toaster", () => ({
  toast: { success: (m: string) => seam.toastSuccess(m), error: (m: string) => seam.toastError(m) },
}));

const FIELDS: FieldMeta[] = [
  { name: "name", label: "Tên công ty", fieldType: "text" },
  { name: "phone", label: "Điện thoại", fieldType: "text" },
];
const LAYOUT = {
  views: { list: { columns: [{ field: "name", width: 200 }, { field: "phone", width: 140 }] } },
} as unknown as EntityLayout;
const ROWS: ApiRecord[] = [
  { id: "r1", version: 1, data: { name: "Acme Corp", phone: "0901" } },
  { id: "r2", version: 1, data: { name: "Globex Inc", phone: "0902" } },
];

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => {}),
    getLayout: vi.fn(async () => LAYOUT),
    listFields: vi.fn(async () => FIELDS),
    listDeletedPage: (...a: unknown[]) => seam.listDeletedPage(...a),
    restoreRecord: (...a: unknown[]) => seam.restoreRecord(...a),
  };
});

import { RecycleBinScreen } from "@/components/screens/RecycleBinScreen";

describe("RecycleBinScreen (SLDS)", () => {
  beforeEach(() => {
    seam.canDelete = true;
    seam.listDeletedPage.mockReset().mockResolvedValue({
      records: ROWS,
      pagination: { page: 1, totalPages: 1, total: 2 },
    });
    seam.restoreRecord.mockReset().mockResolvedValue({ id: "r1" });
    seam.toastSuccess.mockReset();
    seam.toastError.mockReset();
  });

  it("lists the deleted rows under the object's name with the deleted count", async () => {
    render(<RecycleBinScreen entity="crm_accounts" />);
    const grid = await screen.findByRole("grid", { name: "Thùng rác Khách hàng" });
    expect(within(grid).getByText("Acme Corp")).toBeInTheDocument();
    expect(within(grid).getByText("0902")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Thùng rác/ })).toBeInTheDocument();
    expect(screen.getByText("2 đã xoá")).toBeInTheDocument();
    expect(screen.getByText("Đã tải 2 / 2 bản ghi")).toBeInTheDocument();
    expect(seam.listDeletedPage).toHaveBeenCalledWith("crm_accounts", "limit=50");
  });

  it("restores a row through the row action and the confirm prompt", async () => {
    render(<RecycleBinScreen entity="crm_accounts" />);
    await screen.findByText("Acme Corp");

    await userEvent.click(screen.getByRole("button", { name: "Thêm thao tác cho Acme Corp" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Khôi phục" }));
    const prompt = await screen.findByRole("alertdialog");
    expect(prompt).toHaveTextContent("Khôi phục bản ghi này?");
    expect(prompt).toHaveTextContent("Acme Corp sẽ được đưa trở lại danh sách đang hoạt động.");
    await userEvent.click(within(prompt).getByRole("button", { name: "Khôi phục" }));

    await waitFor(() => expect(seam.restoreRecord).toHaveBeenCalledWith("crm_accounts", "r1"));
    await waitFor(() => expect(screen.queryByText("Acme Corp")).not.toBeInTheDocument());
    expect(screen.getByText("Globex Inc")).toBeInTheDocument();
    expect(screen.getByText("1 đã xoá")).toBeInTheDocument();
    expect(seam.toastSuccess).toHaveBeenCalledWith('Đã khôi phục "Acme Corp".');
  });

  it("is read-only without the delete grant: a note, no row actions", async () => {
    seam.canDelete = false;
    render(<RecycleBinScreen entity="crm_accounts" />);
    await screen.findByText("Acme Corp");
    expect(screen.getByText(/Chế độ chỉ xem/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Thêm thao tác cho/ })).not.toBeInTheDocument();
  });

  it("shows the empty illustration when nothing is in the bin", async () => {
    seam.listDeletedPage.mockResolvedValue({ records: [], pagination: { page: 1, totalPages: 1, total: 0 } });
    render(<RecycleBinScreen entity="crm_accounts" />);
    expect(await screen.findByText("Thùng rác trống")).toBeInTheDocument();
  });
});
