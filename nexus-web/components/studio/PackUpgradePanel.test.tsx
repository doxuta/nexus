// PackUpgradePanel — specs/069 L1 rendering contract (READ-ONLY): every status group renders with its
// S13 label, settled items are counted not listed, the three-version box only exists for items
// that carry it, the untracked banner shows, and there is NO apply control at this slice.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { PackUpgradePanel } from "./PackUpgradePanel";
import { planPackUpgrade, applyPackUpgrade, listPackUpgrades, ApiError, type UpgradeItem, type UpgradePlan, type UpgradeRun, type UpgradeStatus } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, planPackUpgrade: vi.fn(), applyPackUpgrade: vi.fn(), listPackUpgrades: vi.fn() };
});

function item(partial: Partial<UpgradeItem> & { itemKey: string; status: UpgradeStatus }): UpgradeItem {
  const settled = partial.status === "unchanged" || partial.status === "in_sync";
  return {
    itemType: "workflow", label: partial.itemKey, actions: settled ? [] : ["keep"], defaultAction: settled ? "none" : "keep",
    theirsPaths: [], oursPaths: [], overlap: [], sharedWith: [], warnings: [], reason: settled ? undefined : "ADAPTER_NOT_READY",
    ...partial,
  };
}

const PLAN: UpgradePlan = {
  slug: "ticketing", fromVersions: ["1.7.0"], toVersion: "1.8.0", source: "builtin", untracked: false, delivered: false,
  planHash: "abc", summary: {}, warnings: [],
  items: [
    item({ itemKey: "Đặt hạn SLA — phiếu Khẩn", status: "update_safe", theirsPaths: ["actions[0].stampField"], base: { a: 1 }, theirs: { a: 2 }, live: { a: 1 } }),
    item({ itemKey: "Đặt hạn SLA — phiếu thường", status: "conflict", theirsPaths: ["actions[0].stampField"], oursPaths: ["isActive"] }),
    item({ itemType: "field", itemKey: "tk_tickets.due_date_auto", status: "added" }),
    item({ itemKey: "Tính lại hạn SLA", status: "protected", reason: "ADAPTER_NOT_READY" }),
    item({ itemKey: "Định tuyến", status: "unchanged" }),
    item({ itemKey: "Nhắc hạn", status: "in_sync" }),
  ],
};

beforeEach(() => {
  vi.mocked(planPackUpgrade).mockReset();
  vi.mocked(applyPackUpgrade).mockReset();
  vi.mocked(listPackUpgrades).mockReset();
});

describe("PackUpgradePanel", () => {
  it("renders the version range, status groups with S13 labels, paths and a settled count", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);

    expect(await screen.findByText(/Nâng cấp từ v1\.7\.0 lên v1\.8\.0/)).toBeInTheDocument();
    expect(planPackUpgrade).toHaveBeenCalledWith("ticketing", { builtin: "ticketing" });
    for (const label of ["Gói đổi — áp an toàn", "Cả hai đã đổi", "Gói thêm mới", "Giữ bản riêng"]) {
      expect(screen.getByRole("region", { name: label })).toBeInTheDocument();
    }
    expect(screen.getAllByText(/đổi ở: actions\[0\]\.stampField/)).toHaveLength(2);
    // Type labels (field is a sub-item of a table).
    expect(screen.getByText("Trường")).toBeInTheDocument();
    // Settled (unchanged + in_sync) are counted, never listed.
    expect(screen.getByText(/2 hạng mục không đổi hoặc đã khớp bản mới/)).toBeInTheDocument();
    expect(screen.queryByText("Định tuyến")).not.toBeInTheDocument();
    // The reason a keep-only item cannot be applied is shown in words.
    expect(screen.getAllByText("Chưa hỗ trợ nâng cấp loại này").length).toBeGreaterThan(0);
  });

  it("three-version box exists only for items that carry the versions, and is read-only", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);

    // Only the update_safe row carries base/theirs/live → exactly one toggle.
    const toggles = screen.getAllByRole("button", { name: "Xem ba bản" });
    expect(toggles).toHaveLength(1);
    fireEvent.click(toggles[0]);
    const li = toggles[0].closest("li") as HTMLElement;
    expect(within(li).getByText("Bản gốc (lần cài trước)")).toBeInTheDocument();
    expect(within(li).getByText("Của bạn (đang chạy)")).toBeInTheDocument();
    expect(within(li).getByText("Bản mới của gói")).toBeInTheDocument();
    // Every item here is keep-only, so nothing can be applied: the button exists but is disabled.
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeDisabled();
  });

  it("untracked install shows the honest notification and the plan still lists items", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({ ...PLAN, untracked: true });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    expect(await screen.findByText(/không có bản gốc/)).toBeInTheDocument();
    expect(screen.getByText("Đặt hạn SLA — phiếu Khẩn")).toBeInTheDocument();
  });

  it("plan with nothing to decide says so; plan warnings are shown", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...PLAN, items: [item({ itemKey: "x", status: "unchanged" })], warnings: ["Loại \"report\" không được nối ở máy chủ này — bỏ qua khi nâng cấp"],
    });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    expect(await screen.findByText(/Không có hạng mục nào cần quyết định/)).toBeInTheDocument();
    expect(screen.getByText(/không được nối ở máy chủ này/)).toBeInTheDocument();
  });

  it("shows the server's message when the plan is refused", async () => {
    vi.mocked(planPackUpgrade).mockRejectedValue(new ApiError(422, "PACK_UPGRADE_DOWNGRADE", "Phiên bản mới (1.0.0) thấp hơn phiên bản đã cài (1.8.0)"));
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    expect(await screen.findByText(/thấp hơn phiên bản đã cài/)).toBeInTheDocument();
  });

  // ── L2a: the apply step ──
  const applyItem = (key: string, status: UpgradeStatus, actions: string[], def: string, extra: Partial<UpgradeItem> = {}) =>
    item({ itemKey: key, status, actions, defaultAction: def, reason: undefined, ...extra });
  const APPLY_PLAN: UpgradePlan = {
    ...PLAN, planHash: "h1",
    items: [
      applyItem("Rule safe", "update_safe", ["apply", "keep"], "apply"),
      applyItem("Rule conflict", "conflict", ["apply", "keep"], "keep"),
      applyItem("Rule settled", "in_sync", [], "none", { adopt: true }),
    ],
  };
  const RUN: UpgradeRun = {
    id: "r1", slug: "ticketing", fromVersions: "1.7.0", toVersion: "1.8.0", source: "builtin", mode: "live", status: "done",
    decisions: [], counts: { applied: 1 }, startedAt: "2026-10-14T00:00:00Z",
    results: [{ itemType: "workflow", itemKey: "Rule safe", action: "apply", outcome: "applied" }],
  };

  it("pre-selects the server defaults, counts only writes, and sends explicit decisions after confirm", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(APPLY_PLAN);
    vi.mocked(applyPackUpgrade).mockResolvedValue(RUN);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);

    expect(screen.getByLabelText(/Chọn cách xử lý cho Rule safe/)).toHaveValue("apply");
    expect(screen.getByLabelText(/Chọn cách xử lý cho Rule conflict/)).toHaveValue("keep");
    expect(screen.getByText(/nhận nuôi 1 mục/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    // The confirm lists exactly what will be written.
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Áp bản mới: Rule safe")).toBeInTheDocument();
    expect(within(dialog).queryByText(/Rule conflict/)).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Áp nâng cấp" }));

    expect(await screen.findByText("Kết quả lần nâng cấp")).toBeInTheDocument();
    expect(applyPackUpgrade).toHaveBeenCalledWith("ticketing", { builtin: "ticketing" }, "h1", [
      { itemType: "workflow", itemKey: "Rule safe", action: "apply" },
      { itemType: "workflow", itemKey: "Rule conflict", action: "keep" },
    ]);
  });

  it("a builder's override of a default is what gets sent", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(APPLY_PLAN);
    vi.mocked(applyPackUpgrade).mockResolvedValue(RUN);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    fireEvent.change(screen.getByLabelText(/Chọn cách xử lý cho Rule conflict/), { target: { value: "apply" } });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Áp nâng cấp" }));
    await screen.findByText("Kết quả lần nâng cấp");
    expect(vi.mocked(applyPackUpgrade).mock.calls[0][3]).toContainEqual({ itemType: "workflow", itemKey: "Rule conflict", action: "apply" });
  });

  it("only adoptable items still enables the button (N = 0, M > 0)", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({ ...APPLY_PLAN, items: [APPLY_PLAN.items[2]] });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeEnabled();
  });

  it("409 stale reloads the plan and says so; 409 in-progress shows the start time", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(APPLY_PLAN);
    vi.mocked(applyPackUpgrade).mockRejectedValueOnce(new ApiError(409, "PACK_UPGRADE_PLAN_STALE", "x", { planHash: "h2" }));
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText(/đã tải lại kế hoạch/)).toBeInTheDocument();
    await vi.waitFor(() => expect(planPackUpgrade).toHaveBeenCalledTimes(2));

    vi.mocked(applyPackUpgrade).mockRejectedValueOnce(new ApiError(409, "PACK_UPGRADE_IN_PROGRESS", "x", { startedAt: "2026-10-14T01:02:03Z" }));
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText(/đang chạy \(bắt đầu 2026-10-14T01:02:03Z\)/)).toBeInTheDocument();
  });

  it("a dropped connection reads the history instead of guessing", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(APPLY_PLAN);
    vi.mocked(applyPackUpgrade).mockRejectedValue(new TypeError("Failed to fetch"));
    vi.mocked(listPackUpgrades).mockResolvedValue([RUN]);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText(/Mất kết nối khi đang áp/)).toBeInTheDocument();
    expect(listPackUpgrades).toHaveBeenCalledWith("ticketing", 1);
    expect(screen.getByText("Kết quả lần nâng cấp")).toBeInTheDocument();
  });

  it("access-widening and access-narrowing steps are worded, never pre-selected; the owner role is keep-only", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...PLAN,
      items: [
        item({ itemType: "role_grant", itemKey: "qa_staff/qa_contracts/view", status: "update_safe", actions: ["apply", "keep"], defaultAction: "keep",
          reason: undefined, warnings: [{ code: "GRANT_NARROWING", level: "confirm", text: "server text" }] }),
        item({ itemType: "field_acl", itemKey: "qa_staff:qa_devices.cost_price", status: "update_safe", actions: ["apply", "keep"], defaultAction: "keep",
          reason: undefined, warnings: [{ code: "FIELD_ACL_OPENING", level: "confirm", text: "server text" }] }),
        item({ itemType: "role", itemKey: "tenant_owner", status: "protected", reason: "ROLE_NAME_RESERVED" }),
      ],
    });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.getByText(/thu hẹp quyền đang chạy/)).toBeInTheDocument();
    expect(screen.getByText(/mở lại trường đang bị ẩn hoặc khoá/)).toBeInTheDocument();
    expect(screen.getByText("Quyền theo trường")).toBeInTheDocument();
    expect(screen.getByText("Vai do hệ thống quản lý — nâng cấp gói không đụng vào")).toBeInTheDocument();
    // Nothing pre-selected: the apply button counts 0 writes.
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeDisabled();
    // The owner role offers no choice at all (a single action renders no select).
    expect(screen.queryByLabelText("Chọn cách xử lý cho tenant_owner")).not.toBeInTheDocument();
  });

  // ── L2b: "Luôn giữ" ──
  it("'Luôn giữ' is offered only where the server lists it, counts as a change and is sent as such", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...PLAN, planHash: "h2",
      items: [
        applyItem("Màn A", "conflict", ["apply", "keep", "keep_always"], "keep", { itemType: "layout" }),
        applyItem("Màn B", "update_safe", ["apply", "keep"], "apply", { itemType: "layout" }),
      ],
    });
    vi.mocked(applyPackUpgrade).mockResolvedValue({ ...RUN, counts: { protected: 1, applied: 1 } });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);

    const a = screen.getByLabelText(/Chọn cách xử lý cho Màn A/) as HTMLSelectElement;
    const b = screen.getByLabelText(/Chọn cách xử lý cho Màn B/) as HTMLSelectElement;
    expect([...a.options].map((o) => o.value)).toEqual(["apply", "keep", "keep_always"]);
    expect([...b.options].map((o) => o.value)).toEqual(["apply", "keep"]);
    expect(within(a).getByRole("option", { name: "Luôn giữ bản của tôi" })).toBeInTheDocument();

    expect(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" })).toBeEnabled();
    fireEvent.change(a, { target: { value: "keep_always" } });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Luôn giữ bản của tôi: Màn A")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Áp nâng cấp" }));
    await screen.findByText("Kết quả lần nâng cấp");
    expect(vi.mocked(applyPackUpgrade).mock.calls[0][3]).toContainEqual({ itemType: "layout", itemKey: "Màn A", action: "keep_always" });
  });

  // ── L6: "Gộp" ──
  it("'Gộp' is offered with the paths taken from the pack, defaults to keep, counts as a change and is sent", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...PLAN, planHash: "h3",
      items: [applyItem("QA rule", "conflict", ["apply", "keep", "merge"], "keep", {
        itemType: "workflow", merged: { name: "QA rule" }, theirsTakenPaths: ["actions"], theirs: {}, live: {}, base: {},
      })],
    });
    vi.mocked(applyPackUpgrade).mockResolvedValue({ ...RUN, counts: { merged: 1 } });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.getByText("Lấy từ gói: actions")).toBeInTheDocument();
    const sel = screen.getByLabelText(/Chọn cách xử lý cho QA rule/) as HTMLSelectElement;
    expect(sel.value).toBe("keep");
    expect(within(sel).getByRole("option", { name: "Gộp bản của tôi với bản mới" })).toBeInTheDocument();
    fireEvent.change(sel, { target: { value: "merge" } });
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Áp nâng cấp" }));
    await screen.findByText("Kết quả lần nâng cấp");
    expect(vi.mocked(applyPackUpgrade).mock.calls[0][3]).toContainEqual({ itemType: "workflow", itemKey: "QA rule", action: "merge" });
  });

  it("only 'Luôn giữ' chosen still enables the button, and its outcome is worded", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...PLAN, planHash: "h3",
      items: [applyItem("Màn A", "kept_custom", ["keep", "restore", "keep_always"], "keep", { itemType: "layout" })],
    });
    vi.mocked(applyPackUpgrade).mockResolvedValue({
      ...RUN, counts: { protected: 1 }, results: [{ itemType: "layout", itemKey: "Màn A", action: "keep_always", outcome: "protected" }],
    });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/Chọn cách xử lý cho Màn A/), { target: { value: "keep_always" } });
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText("Đã đánh dấu luôn giữ")).toBeInTheDocument();
  });

  // ── L7: a delivered project ──
  it("delivered: shows how each change reaches the customer, gates the untraced ones behind the tick, reports drafts", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue({
      ...APPLY_PLAN, delivered: true,
      items: [
        applyItem("Rule safe", "update_safe", ["apply", "keep"], "apply", { applyMode: "draft" }),
        applyItem("today", "added", ["apply", "keep"], "apply", { itemType: "work_queue", applyMode: "immediate_untraced" }),
      ],
    });
    vi.mocked(applyPackUpgrade).mockResolvedValue({ ...RUN, mode: "draft", counts: { drafted: 1, applied: 1 } });
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.getByText("Vào bản nháp — khách thấy khi Xuất bản")).toBeInTheDocument();
    expect(screen.getByText("Áp ngay cho khách (không qua nháp)")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" }));
    const dialog = await screen.findByRole("alertdialog");
    const go = within(dialog).getByRole("button", { name: "Áp nâng cấp" });
    expect(go).toBeDisabled(); // no tick, no apply
    fireEvent.click(within(dialog).getByLabelText("Áp ngay cho khách"));
    expect(go).toBeEnabled();
    fireEvent.click(go);
    expect(await screen.findByText(/1 thay đổi đã vào bản nháp — Xuất bản để khách thấy/)).toBeInTheDocument();
    expect(vi.mocked(applyPackUpgrade).mock.calls[0][4]).toEqual({ confirmImmediate: true });
  });

  it("not delivered: no mode line, no tick, the call keeps its four arguments", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(APPLY_PLAN);
    vi.mocked(applyPackUpgrade).mockResolvedValue(RUN);
    render(<PackUpgradePanel slug="ticketing" source={{ builtin: "ticketing" }} />);
    await screen.findByText(/Nâng cấp từ/);
    expect(screen.queryByText(/Vào bản nháp/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).queryByLabelText("Áp ngay cho khách")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Áp nâng cấp" }));
    await screen.findByText("Kết quả lần nâng cấp");
    expect(vi.mocked(applyPackUpgrade).mock.calls[0]).toHaveLength(4);
  });
});
