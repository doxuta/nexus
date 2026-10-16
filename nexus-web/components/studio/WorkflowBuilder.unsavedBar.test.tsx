// specs/074 lát 11 T456: on a saved rule the Huỷ / Lưu bar appears only once the form holds a change, Huỷ restores the
// stored rule, and a failed save keeps the bar with the engine's own error.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, updateWorkflow } from "@/lib/api";

const RULE = { id: "r-1", name: "Nhắc việc", entityName: "orders", trigger: "after_create", isActive: true, condition: { match: "and", rules: [] }, actions: [{ type: "in_app_notification", userId: "$owner", message: "Có đơn" }] };

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    createWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "orders", label: "Đơn hàng" }]),
    listFields: vi.fn(async () => [{ name: "amount", label: "Số tiền", fieldType: "number" }]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  listBusinessCalendars: vi.fn(async () => []),
  listTeams: vi.fn(async () => []),
}));

const name = () => screen.findByRole("textbox", { name: /Tên luật/ }, { timeout: 3000 }) as Promise<HTMLInputElement>;

beforeEach(() => vi.mocked(updateWorkflow).mockClear());

describe("WorkflowBuilder — the unsaved bar", () => {
  it("opens clean (no Lưu), shows the bar + words after an edit, and goes away when the edit is undone", async () => {
    render(<WorkflowBuilder />);
    const box = await name();
    expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull();
    fireEvent.change(box, { target: { value: "Nhắc việc 2" } });
    expect(await screen.findByRole("button", { name: "Cập nhật" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Có thay đổi chưa lưu");
    fireEvent.change(box, { target: { value: "Nhắc việc" } });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull());
  });

  it("Huỷ restores the stored rule and drops the bar", async () => {
    render(<WorkflowBuilder />);
    const box = await name();
    fireEvent.change(box, { target: { value: "Đổi tên" } });
    await userEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(box).toHaveValue("Nhắc việc"));
    expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull();
  });

  it("a failed save keeps the bar and shows the engine's own error; a good one drops it", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(new ApiError(422, "WORKFLOW_NAME_TAKEN", "Tên luật đã có", {}));
    render(<WorkflowBuilder />);
    fireEvent.change(await name(), { target: { value: "Trùng tên" } });
    await userEvent.click(await screen.findByRole("button", { name: "Cập nhật" }));
    expect(await screen.findByText(/Tên luật đã có/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cập nhật" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull());
    expect(await screen.findByText("Đã lưu")).toBeInTheDocument();
  });

  it("the sections are flat: Khi, Nếu, Thì, then 'Lần chạy gần đây', no card and no tab", async () => {
    const { container } = render(<WorkflowBuilder hideList />);
    await name();
    for (const h of ["Khi", "Nếu", "Thì", "Lần chạy gần đây"]) expect(screen.getByRole("heading", { name: h })).toBeInTheDocument();
    expect(screen.queryByRole("tab")).toBeNull();
    expect(container.querySelectorAll(".slds-card")).toHaveLength(0);
  });
});
