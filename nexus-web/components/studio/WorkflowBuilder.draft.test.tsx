// specs/063-draft-publish T045 + T051c: on a delivered workspace the rule list wears Nháp / Mới / Sẽ xoá,
// a save says it went to the draft ("khách chưa thấy"), and a LIVE rule can be turned off for the
// customer at once ("Tắt ngay cho khách") behind an ÁP NGAY confirm.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { disableWorkflowNow, listWorkflows, updateWorkflow } from "@/lib/api";
import { toast } from "@/components/ui/Toaster";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { touchRule } from "@/test/touchRule";

const base = {
  entityName: "orders", trigger: "after_update", isActive: true, condition: null,
  actions: [{ type: "in_app_notification", userId: "$owner", message: "Có đơn" }],
};
const LIVE = { ...base, id: "w-live", name: "Luật đang chạy", draftState: "live" };
const EDITED = { ...base, id: "w-edited", name: "Luật đang sửa", draftState: "edited" };
const NEW = { ...base, id: "w-new", name: "Luật mới tạo", draftState: "new" };
const DELETED = { ...base, id: "w-del", name: "Luật sẽ xoá", draftState: "deleted" };

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(),
    updateWorkflow: vi.fn(),
    createWorkflow: vi.fn(),
    disableWorkflowNow: vi.fn(),
    listEntities: vi.fn(async () => [{ name: "orders", label: "Đơn hàng" }]),
    listFields: vi.fn(async () => [{ name: "status", label: "Trạng thái", fieldType: "text" }]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  listBusinessCalendars: vi.fn(async () => []),
  listTeams: vi.fn(async () => []),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() } }));

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listWorkflows).mockResolvedValue([LIVE, EDITED, NEW, DELETED] as never);
  vi.mocked(updateWorkflow).mockResolvedValue({ ...EDITED } as never);
  vi.mocked(disableWorkflowNow).mockResolvedValue({ ...LIVE, isActive: false } as never);
});

async function openRule(name: string) {
  render(<WorkflowBuilder />);
  const row = await screen.findByRole("button", { name: new RegExp("^" + name) });
  fireEvent.click(row);
  await screen.findByDisplayValue(name, {}, { timeout: 3000 });
}

describe("WorkflowBuilder — draft labels", () => {
  it("the list marks Nháp / Mới / Sẽ xoá and leaves a live rule bare", async () => {
    render(<WorkflowBuilder />);
    const rowOf = async (name: string) => (await screen.findByRole("button", { name: new RegExp("^" + name) })).closest("li") as HTMLElement;
    expect(within(await rowOf("Luật đang sửa")).getByText("Nháp")).toBeInTheDocument();
    expect(within(await rowOf("Luật mới tạo")).getByText("Mới, khách thấy khi xuất bản")).toBeInTheDocument();
    expect(within(await rowOf("Luật sẽ xoá")).getByText("Sẽ xoá khi xuất bản")).toBeInTheDocument();
    const live = within(await rowOf("Luật đang chạy"));
    expect(live.queryByText("Nháp")).not.toBeInTheDocument();
    expect(live.queryByText("Mới")).not.toBeInTheDocument();
  });

  it("the open rule's header wears the same draft label as its list row; a live rule stays bare", async () => {
    await openRule("Luật đang sửa");
    expect(screen.getAllByText("Nháp").length).toBeGreaterThanOrEqual(2); // list row + editor header
  });

  it("a save on a delivered workspace says it went to the draft", async () => {
    vi.mocked(listWorkflows).mockResolvedValue([EDITED] as never);
    await openRule("Luật đang sửa");
    await touchRule();
    fireEvent.click(await screen.findByRole("button", { name: "Cập nhật" }));
    expect(await screen.findByText("Đã lưu vào bản nháp — khách chưa thấy")).toBeInTheDocument();
    expect(screen.queryByText("Đã lưu")).not.toBeInTheDocument();
  });

  it("a save on a workspace that is not delivered keeps the plain message", async () => {
    const plain = { ...base, id: "w-plain", name: "Luật thường" }; // no draftState: nothing is delivered
    vi.mocked(listWorkflows).mockResolvedValue([plain] as never);
    vi.mocked(updateWorkflow).mockResolvedValue(plain as never);
    await openRule("Luật thường");
    await touchRule();
    fireEvent.click(await screen.findByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByText("Đã lưu vào bản nháp — khách chưa thấy")).not.toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Tắt ngay cho khách" })).not.toBeInTheDocument();
  });
});

describe("WorkflowBuilder — stays in step with the draft bar", () => {
  it("reloads its list when the bar publishes or discards (the badges must not go stale)", async () => {
    render(<WorkflowBuilder />);
    await screen.findByRole("button", { name: /^Luật đang sửa/ });
    const reads = vi.mocked(listWorkflows).mock.calls.length;
    vi.mocked(listWorkflows).mockResolvedValue([{ ...EDITED, draftState: "live" }] as never);
    emitDraftsChanged(); // the bar's announcement
    await waitFor(() => expect(vi.mocked(listWorkflows).mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Luật mới tạo/ })).not.toBeInTheDocument());
  });

  it("does not react to its own announcement (it has just refreshed itself)", async () => {
    render(<WorkflowBuilder />);
    await screen.findByRole("button", { name: /^Luật đang sửa/ });
    const reads = vi.mocked(listWorkflows).mock.calls.length;
    emitDraftsChanged("workflow-builder");
    await new Promise((r) => setTimeout(r, 30));
    expect(vi.mocked(listWorkflows).mock.calls.length).toBe(reads);
  });
});

describe("WorkflowBuilder — Tắt ngay cho khách", () => {
  it("is offered on a live rule and on one with a pending edit, never on a draft-only or to-be-deleted one", async () => {
    for (const [name, offered] of [["Luật đang chạy", true], ["Luật đang sửa", true], ["Luật mới tạo", false], ["Luật sẽ xoá", false]] as const) {
      const { unmount } = render(<WorkflowBuilder />);
      fireEvent.click(await screen.findByRole("button", { name: new RegExp("^" + name) }));
      await screen.findByDisplayValue(name, {}, { timeout: 3000 });
      const btn = screen.queryByRole("button", { name: "Tắt ngay cho khách" });
      expect(!!btn, name).toBe(offered);
      unmount();
    }
  });

  it("asks ÁP NGAY, then switches the live rule off and refreshes", async () => {
    const user = userEvent.setup();
    await openRule("Luật đang chạy");
    await user.click(await screen.findByRole("button", { name: "Tắt ngay cho khách" }));
    const dialog = await screen.findByRole("group", { name: "Tắt luật ngay cho người dùng thật" });
    expect(within(dialog).getByText(/ÁP NGAY cho người dùng thật/)).toBeInTheDocument();
    expect(disableWorkflowNow).not.toHaveBeenCalled(); // asks first
    await user.click(within(dialog).getByRole("button", { name: "Tắt ngay" }));
    await waitFor(() => expect(disableWorkflowNow).toHaveBeenCalledWith("w-live"));
    expect(toast.success).toHaveBeenCalledWith("Đã tắt luật “Luật đang chạy” cho khách");
    await waitFor(() => expect(vi.mocked(listWorkflows).mock.calls.length).toBeGreaterThan(1));
  });

  it("after Tắt ngay the editor shows the rule OFF, so the next save cannot silently switch it back on", async () => {
    const user = userEvent.setup();
    await openRule("Luật đang chạy");
    const toggle = () => screen.getByRole("switch", { name: /Bật luật/ }) as HTMLInputElement;
    expect(toggle().checked).toBe(true);
    await user.click(await screen.findByRole("button", { name: "Tắt ngay cho khách" }));
    await user.click(within(await screen.findByRole("group", { name: "Tắt luật ngay cho người dùng thật" })).getByRole("button", { name: "Tắt ngay" }));
    await waitFor(() => expect(disableWorkflowNow).toHaveBeenCalledWith("w-live"));
    await waitFor(() => expect(toggle().checked).toBe(false));
  });

  it("warns when the rule also has a draft", async () => {
    const user = userEvent.setup();
    await openRule("Luật đang sửa");
    await user.click(await screen.findByRole("button", { name: "Tắt ngay cho khách" }));
    expect(await within(await screen.findByRole("group", { name: "Tắt luật ngay cho người dùng thật" })).findByText(/lần Xuất bản kế tiếp sẽ báo xung đột/)).toBeInTheDocument();
  });

  it("shows the server's refusal and keeps the rule", async () => {
    vi.mocked(disableWorkflowNow).mockRejectedValue(new Error("không tìm thấy workflow"));
    const user = userEvent.setup();
    await openRule("Luật đang chạy");
    await user.click(await screen.findByRole("button", { name: "Tắt ngay cho khách" }));
    await user.click(within(await screen.findByRole("group", { name: "Tắt luật ngay cho người dùng thật" })).getByRole("button", { name: "Tắt ngay" }));
    expect(await screen.findByText(/Không tắt được luật: không tìm thấy workflow/)).toBeInTheDocument();
  });
});
