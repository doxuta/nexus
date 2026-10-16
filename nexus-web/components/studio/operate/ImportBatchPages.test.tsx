// specs/078 lát u7-du-lieu-nhap-csv T084/T085a: CSV import and batch create mounted on the Studio work surface.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { OperateBatch, OperateImport } from "./ImportBatchPages";

const push = vi.fn();
const perm = vi.hoisted(() => ({ create: true }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: () => perm.create, isLoading: false }) }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntityLabel: () => "Khách hàng" }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn(),
  listFields: vi.fn(async () => [{ id: "f1", name: "name", label: "Tên", fieldType: "text" }]),
  createRecordsBulk: vi.fn(async () => ({ mode: "partial", created: 2, failed: 0, skipped: 0, results: [] })),
}));
const list = "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts";

beforeEach(() => { vi.clearAllMocks(); perm.create = true; });

describe("Studio import / batch create", () => {
  it("import: embedded flat (0 .slds-card, one h2, horizontal steps) and Cancel goes to the Studio list", async () => {
    const { container } = render(<EmbeddedSurface><OperateImport slug="northwind" bang="crm_accounts" /></EmbeddedSurface>);
    await screen.findByRole("heading", { level: 2 });
    expect(container.querySelectorAll(".slds-card")).toHaveLength(0);
    expect(container.querySelectorAll("h2")).toHaveLength(1);
    expect(container.querySelector(".slds-progress")).not.toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(push).toHaveBeenCalledWith(list);
  });

  it("batch: no dialog, cancel goes to the Studio list", async () => {
    const { container } = render(<OperateBatch slug="northwind" bang="crm_accounts" />);
    await screen.findByRole("button", { name: "Quay lại danh sách" });
    expect(container.querySelectorAll("[role=dialog]")).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Quay lại danh sách" }));
    expect(push).toHaveBeenCalledWith(list);
  });

  it.each([["import", OperateImport], ["batch", OperateBatch]])("%s: no create grant -> notice, no wizard", (_n, C) => {
    perm.create = false;
    render(<C slug="northwind" bang="crm_accounts" />);
    expect(screen.getByText("Anh không có quyền tạo bản ghi ở bảng này.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Huỷ" })).toBeNull();
  });
});
