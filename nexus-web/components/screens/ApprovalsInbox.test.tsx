// ApprovalsInbox (/approvals) = "Việc của tôi" on SLDS (specs/043 S5, FE-BPM-INBOX #129). Pins what each
// persona must still be able to do: read a tab with entity LABELS (never table names), see the deadline with
// what it is counted in and an SLA badge, claim from "Chờ nhận", approve with an optional note, reject only
// with a reason, complete a rework item (and be told which required fields are empty), and get a refreshed
// list when someone else got there first (409).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalsInbox } from "./ApprovalsInbox";
import * as api from "@/lib/api";

const auth = vi.hoisted(() => ({ user: { id: "u1", fullName: "Duyệt Một", email: "duyet1@example.com", isAdmin: false } as Record<string, unknown> }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => {}),
    listWorkItems: vi.fn(),
    workItemCounts: vi.fn(async () => ({ mine: 1, pool: 2, overdue: 0 })),
    claimWorkItem: vi.fn(async () => {}),
    releaseWorkItem: vi.fn(async () => {}),
    completeWorkItem: vi.fn(async () => {}),
    approveRecord: vi.fn(async () => {}),
    rejectRecord: vi.fn(async () => {}),
  };
});

const item = (over: Record<string, unknown> = {}) => ({
  id: "w1", instanceId: "ap-1", processName: "Duyệt báo giá", nodeName: "Trưởng phòng", kind: "approval", status: "open",
  entityName: "crm_quotes", entityLabel: "Báo giá", recordId: "rec-12345678", recordLabel: "BG-001",
  assigneeType: "user", assigneeName: "Duyệt Một", claimedByMe: false,
  dueAt: "2099-10-05T04:00:00Z", dueBasis: "business", overdue: false, paused: false, createdAt: "2026-10-01T00:00:00Z", ...over,
});

function renderInbox(initialView?: "mine" | "pool" | "done") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><ApprovalsInbox initialView={initialView} /></QueryClientProvider>);
}
const page = (items: unknown[]) => ({ items, nextCursor: null });
const grid = () => screen.findByRole("grid", { name: "Danh sách việc" });
async function openRowMenu(name: RegExp) {
  (await screen.findByRole("button", { name })).focus();
  await userEvent.keyboard("{Enter}");
}

beforeEach(() => {
  vi.clearAllMocks();
  auth.user = { id: "u1", fullName: "Duyệt Một", email: "duyet1@example.com", isAdmin: false };
  vi.mocked(api.listWorkItems).mockResolvedValue(page([item()]) as never);
});

describe("ApprovalsInbox", () => {
  it("lists a work item with labels, the deadline basis and an SLA badge; tabs carry the counts", async () => {
    const { container } = renderInbox();
    const g = await grid();
    expect(await within(g).findByRole("link", { name: "Báo giá: BG-001" })).toHaveAttribute("href", "/approvals/ap-1");
    expect(within(g).getByText("Duyệt báo giá")).toBeInTheDocument();
    expect(within(g).getByText("Trưởng phòng")).toBeInTheDocument();
    expect(within(g).getByText(/\(giờ làm việc\)/)).toBeInTheDocument();
    expect(within(g).getByText("Còn hạn")).toBeInTheDocument();
    expect(await screen.findByRole("tab", { name: "Của tôi (1)" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Chờ nhận (2)" })).toBeInTheDocument();
    expect(container).not.toHaveTextContent("crm_quotes");
    expect(api.listWorkItems).toHaveBeenCalledWith("mine", undefined);
  });

  it("flags an overdue and a paused item", async () => {
    vi.mocked(api.listWorkItems).mockResolvedValue(page([
      item({ id: "a", dueAt: "2020-01-01T00:00:00Z", overdue: true }),
      item({ id: "b", recordLabel: "BG-002", dueAt: "2020-01-01T00:00:00Z", paused: true }),
    ]) as never);
    renderInbox();
    const g = await grid();
    expect(await within(g).findByText("Quá hạn")).toBeInTheDocument();
    expect(within(g).getByText("Tạm dừng")).toBeInTheDocument();
  });

  it("Chờ nhận tab loads view=pool and Nhận việc claims then refreshes", async () => {
    vi.mocked(api.listWorkItems).mockImplementation((async (view: string) =>
      page(view === "pool" ? [item({ id: "p1", assigneeType: "role", assigneeName: "Trưởng phòng KD" })] : [])) as never);
    renderInbox();
    await userEvent.click(await screen.findByRole("tab", { name: /Chờ nhận/ }));
    await waitFor(() => expect(api.listWorkItems).toHaveBeenCalledWith("pool", undefined));
    await openRowMenu(/Thêm thao tác cho Báo giá: BG-001/);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Nhận việc" }));
    await waitFor(() => expect(api.claimWorkItem).toHaveBeenCalledWith("p1"));
  });

  it("a claim-queue card lands on Chờ nhận (?view=pool → initialView)", async () => {
    renderInbox("pool");
    await waitFor(() => expect(api.listWorkItems).toHaveBeenCalledWith("pool", undefined));
    expect(api.listWorkItems).not.toHaveBeenCalledWith("mine", undefined);
    expect(screen.getByRole("tab", { name: /Chờ nhận/ })).toHaveAttribute("aria-selected", "true");
  });

  it("approve: dialog → optional note → POST approve on the instance", async () => {
    renderInbox();
    await grid();
    await openRowMenu(/Thêm thao tác cho Báo giá: BG-001/);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Duyệt" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/Ghi chú/), "ok");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xác nhận phê duyệt" }));
    await waitFor(() => expect(api.approveRecord).toHaveBeenCalledWith("ap-1", "ok"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("reject: a blank reason is refused at the box, nothing is sent", async () => {
    renderInbox();
    await grid();
    await openRowMenu(/Thêm thao tác cho Báo giá: BG-001/);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Từ chối" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/Ghi chú/), "   ");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xác nhận từ chối" }));
    const msg = "Ghi chú là bắt buộc khi từ chối";
    expect(await within(dialog).findByText(msg)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(/Ghi chú/)).toHaveAttribute("aria-invalid", "true");
    expect(api.rejectRecord).not.toHaveBeenCalled();
  });

  it("409: someone else decided first — dialog closes and every tab is re-read", async () => {
    vi.mocked(api.approveRecord).mockRejectedValueOnce(new api.ApiError(409, "APPROVAL_CONFLICT", "x"));
    renderInbox();
    await grid();
    await openRowMenu(/Thêm thao tác cho Báo giá: BG-001/);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Duyệt" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xác nhận phê duyệt" }));
    expect(await within(dialog).findByText(/vừa có người xử lý/)).toBeInTheDocument();
    const before = vi.mocked(api.listWorkItems).mock.calls.length;
    await userEvent.click(within(dialog).getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(vi.mocked(api.listWorkItems).mock.calls.length).toBeGreaterThan(before));
  });

  it("rework item: shows the return reason; Hoàn thành names the empty required fields", async () => {
    vi.mocked(api.listWorkItems).mockResolvedValue(page([
      item({ kind: "rework", nodeName: "Sửa lại và gửi lại", returnReason: "Thiếu số điện thoại" }),
    ]) as never);
    vi.mocked(api.completeWorkItem).mockRejectedValueOnce(
      new api.ApiError(422, "WORK_ITEM_REQUIRED_FIELDS", "x", { fields: ["Số điện thoại"] } as never),
    );
    renderInbox();
    const g = await grid();
    expect(await within(g).findByText("Lý do trả về: Thiếu số điện thoại")).toBeInTheDocument();
    expect(within(g).getByText("Sửa lại", { selector: "span, a" })).toBeInTheDocument();
    await openRowMenu(/Thêm thao tác cho Báo giá: BG-001/);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Hoàn thành" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xác nhận hoàn thành" }));
    expect(await within(dialog).findByText(/trường bắt buộc chưa điền: Số điện thoại/)).toBeInTheDocument();
  });

  it("empty tab says so, no fake rows", async () => {
    vi.mocked(api.listWorkItems).mockResolvedValue(page([]) as never);
    renderInbox();
    expect(await screen.findByText("Bạn không có việc nào đang chờ")).toBeInTheDocument();
  });
});
