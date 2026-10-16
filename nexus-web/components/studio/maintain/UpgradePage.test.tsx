// specs/079 f7-nang-cap-*: the one-pack upgrade page. Reads the plan (never writes), groups it, shows three columns,
// applies only what the builder chose among the server's offered actions, and says what happened.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { pathLabel } from "./CompareTable";
import { ApiError, type UpgradeItem, type UpgradePlan, type UpgradeRun, type UpgradeStatus } from "@/lib/api";

const hoisted = vi.hoisted(() => ({ ok: true }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  planPackUpgrade: vi.fn(),
  applyPackUpgrade: vi.fn(),
  listPackUpgrades: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
}));

import { applyPackUpgrade, listPackUpgrades, planPackUpgrade } from "@/lib/api";
import { UpgradePage } from "./UpgradePage";

const item = (status: UpgradeStatus, extra: Partial<UpgradeItem> = {}): UpgradeItem => ({
  itemType: "workflow", itemKey: `k-${status}`, label: `Luật ${status}`, status, actions: ["keep"], defaultAction: "keep",
  theirsPaths: [], oursPaths: [], overlap: [], sharedWith: [], warnings: [], ...extra,
});
const PLAN = (extra: Partial<UpgradePlan> = {}): UpgradePlan => ({
  slug: "ticketing", fromVersions: ["1.7.0"], toVersion: "1.8.0", source: "builtin", untracked: false, delivered: false,
  planHash: "h1", summary: {}, warnings: [],
  items: [
    item("update_safe", { actions: ["apply", "keep", "keep_always"], defaultAction: "apply", theirsPaths: ["name"], base: { name: "a" }, live: { name: "a" }, theirs: { name: "b" } }),
    item("conflict", { actions: ["keep", "keep_always", "apply"], defaultAction: "keep", theirsPaths: ["name"], oursPaths: ["isActive"],
      base: { name: "a", isActive: true }, live: { name: "a", isActive: false }, theirs: { name: "c", isActive: true } }),
    item("kept_custom", { actions: ["keep"], defaultAction: "keep" }),
    item("unchanged", { actions: [], defaultAction: "none" }),
    item("in_sync", { actions: [], defaultAction: "none", adopt: true }),
  ],
  ...extra,
});
const RUN = (extra: Partial<UpgradeRun> = {}): UpgradeRun => ({
  id: "r1", slug: "ticketing", fromVersions: "1.7.0", toVersion: "1.8.0", source: "builtin", mode: "x", status: "done", decisions: [],
  results: [{ itemType: "workflow", itemKey: "k-update_safe", action: "apply", outcome: "applied" }], counts: { applied: 1 },
  startedAt: "2026-10-14T01:00:00Z", ...extra,
});

const view = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <UpgradePage project="acme" pack="ticketing" />
    </QueryClientProvider>,
  );

beforeEach(() => {
  hoisted.ok = true;
  vi.mocked(planPackUpgrade).mockReset().mockResolvedValue(PLAN());
  vi.mocked(applyPackUpgrade).mockReset().mockResolvedValue(RUN());
  vi.mocked(listPackUpgrades).mockReset().mockResolvedValue([]);
});

describe("UpgradePage: gộp ba chiều (f7-nang-cap-gop)", () => {
  const mergeItem = item("conflict", {
    actions: ["keep", "apply", "merge"], defaultAction: "keep", oursPaths: ["isActive"], theirsTakenPaths: ["name"], theirsPaths: ["name"],
    merged: { name: "c", isActive: false }, base: { name: "a", isActive: true }, live: { name: "a", isActive: false }, theirs: { name: "c", isActive: true },
  });
  it("Gộp is offered and the preview names both lanes", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({ items: [mergeItem] }));
    view();
    const sel = await screen.findByLabelText(/Cách xử lý cho/);
    expect(within(sel).getByRole("option", { name: "Gộp" })).toBeInTheDocument();
    expect(screen.getByText(/Lấy từ gói: name/)).toBeInTheDocument();
    expect(screen.getByText(/Giữ của bạn: isActive/)).toBeInTheDocument();
  });
  it("an overlapping item gets no Gộp and no preview", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({ items: [{ ...mergeItem, overlap: ["name"], merged: undefined }] }));
    view();
    const sel = await screen.findByLabelText(/Cách xử lý cho/);
    expect(within(sel).queryByRole("option", { name: "Gộp" })).toBeNull();
    expect(screen.queryByText(/Lấy từ gói:/)).toBeNull();
  });
});

describe("UpgradePage: read", () => {
  it("shows a spinner, then the title, the summary sentence and the groups in reading order", async () => {
    view();
    expect(screen.getByText("Đang so sánh với gói mới…")).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Gói mẫu ticketing: v1.7.0 lên v1.8.0" })).toBeInTheDocument();
    expect(screen.getByText("1 mục cần anh quyết, 1 mục gói đổi áp an toàn, 1 mục anh đã tuỳ chỉnh, 2 mục đã khớp.")).toBeInTheDocument();
    const names = screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"));
    expect(names.slice(0, 3)).toEqual(["Cần anh quyết", "Gói đổi, áp an toàn", "Anh đã tuỳ chỉnh"]);
    expect(screen.getByText("2 mục đã khớp hoặc không đổi, không liệt kê.")).toBeInTheDocument();
  });

  it("reads only: the plan call carries no targetTenant and nothing is written on mount", async () => {
    view();
    await screen.findByText(/Cần anh quyết/);
    expect(planPackUpgrade).toHaveBeenCalledTimes(1);
    expect(planPackUpgrade).toHaveBeenCalledWith("ticketing", { builtin: "ticketing" });
    expect(applyPackUpgrade).not.toHaveBeenCalled();
  });

  it("a token that does not belong to the project makes zero calls, plan included", () => {
    hoisted.ok = false;
    view();
    expect(planPackUpgrade).not.toHaveBeenCalled();
    expect(listPackUpgrades).not.toHaveBeenCalled();
  });

  it("an engine without the plan endpoint says so, as information, not as a failure", async () => {
    vi.mocked(planPackUpgrade).mockRejectedValue(new ApiError(404, "UNKNOWN", "not found"));
    view();
    expect(await screen.findByText("Xem nâng cấp chưa có trong bản Nexus này.")).toBeInTheDocument();
    expect(screen.queryByText("Thử lại")).not.toBeInTheDocument();
  });

  it("a nested part path reads as words, never as actions[0].stampField", () => {
    expect(pathLabel("actions[0].stampField")).toBe("actions #1 › stampField");
    expect(pathLabel("name")).toBe("name");
  });

  it("opens three columns for an item: old pack, yours, new pack, with the part that changed", async () => {
    view();
    const row = (await screen.findByText("Luật conflict")).closest("li")!;
    fireEvent.click(within(row).getByRole("button", { name: "Xem ba bản" }));
    const table = within(row).getByRole("table");
    for (const h of ["Bản gói cũ", "Của anh", "Bản gói mới"]) expect(within(table).getByRole("columnheader", { name: h })).toBeInTheDocument();
    expect(within(table).getByRole("rowheader", { name: "name" })).toBeInTheDocument();
    expect(within(table).getByRole("rowheader", { name: "isActive" })).toBeInTheDocument();
  });

  it("offers a choice only among the server's actions; an item with only 'keep' gets text, not a select", async () => {
    view();
    const row = (await screen.findByText("Luật conflict")).closest("li")!;
    const sel = within(row).getByRole("combobox", { name: "Cách xử lý cho Luật conflict" }) as HTMLSelectElement;
    expect([...sel.options].map((o) => o.textContent)).toEqual(["Giữ của anh", "Luôn giữ", "Áp bản gói"]);
    expect(sel.value).toBe("keep");
    const only = screen.getByText("Luật kept_custom").closest("li")!;
    expect(within(only).queryByRole("combobox")).toBeNull();
  });

  it("never prints a machine code: unknown status, reason and warning code fall back to plain words / the server text", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({
      items: [item("user_modified" as UpgradeStatus, { label: "Luật mới", reason: "SOMETHING_NEW", warnings: [{ code: "NEW_CODE", level: "info", text: "Câu của máy chủ" }] })],
    }));
    view();
    expect(await screen.findByText("Mục khác")).toBeInTheDocument();
    expect(screen.getByText("Câu của máy chủ")).toBeInTheDocument();
    expect(screen.queryByText(/SOMETHING_NEW|user_modified/)).toBeNull();
  });

  it("an untracked pack says why every item needs a hand choice", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({ untracked: true }));
    view();
    expect(await screen.findByText(/trước khi hệ thống ghi nhớ từng mục của gói/)).toBeInTheDocument();
  });

  it.each([
    ["PACK_UPGRADE_DOWNGRADE", "Bản này cũ hơn bản đang cài, không nâng cấp lùi được."],
    ["PACK_UPGRADE_SLUG_MISMATCH", "Gói này không phải bản mới của gói đã cài."],
  ])("plan error %s gets its own sentence", async (code, text) => {
    vi.mocked(planPackUpgrade).mockRejectedValue(new ApiError(422, code, "raw"));
    view();
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("nothing to decide: one sentence instead of empty groups", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({ items: [item("unchanged", { actions: [], defaultAction: "none" })] }));
    view();
    expect(await screen.findByText("Dự án đã khớp bản mới của gói, không còn gì để nâng cấp.")).toBeInTheDocument();
  });
});

describe("UpgradePage: apply", () => {
  it("counts the writes from the choices, asks in place, then sends EXPLICIT decisions with the plan hash", async () => {
    view();
    await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" });
    expect(screen.getByText("Ghi nhận thêm 1 mục đã khớp sẵn.")).toBeInTheDocument();
    const row = screen.getByText("Luật conflict").closest("li")!;
    fireEvent.change(within(row).getByRole("combobox"), { target: { value: "keep_always" } });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" }));
    // the question opens in the page (no dialog) and the bar is gone until it is answered
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText(/Áp nâng cấp ticketing\? Sẽ ghi 2 mục/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp" }));
    await waitFor(() => expect(applyPackUpgrade).toHaveBeenCalledTimes(1));
    expect(applyPackUpgrade).toHaveBeenCalledWith(
      "ticketing", { builtin: "ticketing" }, "h1",
      [
        { itemType: "workflow", itemKey: "k-update_safe", action: "apply" },
        { itemType: "workflow", itemKey: "k-conflict", action: "keep_always" },
        { itemType: "workflow", itemKey: "k-kept_custom", action: "keep" },
      ],
      {},
    );
    expect(await screen.findByRole("region", { name: "Kết quả lần nâng cấp" })).toBeInTheDocument();
    expect(screen.getByText("Đã áp xong.")).toBeInTheDocument();
  });

  it("the bar hides while the question is open; 'Đặt lại lựa chọn' returns to the server's defaults", async () => {
    view();
    const row = (await screen.findByText("Luật conflict")).closest("li")!;
    fireEvent.change(within(row).getByRole("combobox"), { target: { value: "apply" } });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Đặt lại lựa chọn" }));
    expect(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    expect(screen.queryByTestId("upgrade-apply-bar")).toBeNull();
  });

  it("two quick clicks on the confirm send one request", async () => {
    let release: (r: UpgradeRun) => void = () => {};
    vi.mocked(applyPackUpgrade).mockReturnValue(new Promise<UpgradeRun>((res) => { release = res; }));
    view();
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    const go = screen.getByRole("button", { name: "Áp nâng cấp" });
    fireEvent.click(go);
    fireEvent.click(go);
    expect(applyPackUpgrade).toHaveBeenCalledTimes(1);
    release(RUN());
    await screen.findByRole("region", { name: "Kết quả lần nâng cấp" });
  });

  it("Huỷ closes the question and writes nothing", async () => {
    view();
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(applyPackUpgrade).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" })).toHaveFocus();
  });

  it("a plan that moved (409) reloads the plan and says so; a second call is not made by itself", async () => {
    vi.mocked(applyPackUpgrade).mockRejectedValue(new ApiError(409, "PACK_UPGRADE_PLAN_STALE", "raw", { planHash: "h2" }));
    view();
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText("Kế hoạch đã cũ, đã tải lại. Xem lại rồi áp.")).toBeInTheDocument();
    await waitFor(() => expect(planPackUpgrade).toHaveBeenCalledTimes(2));
    expect(applyPackUpgrade).toHaveBeenCalledTimes(1);
  });

  it("a dropped connection reads the last run instead of guessing", async () => {
    vi.mocked(applyPackUpgrade).mockRejectedValue(new TypeError("Failed to fetch"));
    vi.mocked(listPackUpgrades).mockResolvedValue([RUN({ status: "partial" })]);
    view();
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText(/Mất kết nối khi đang áp/)).toBeInTheDocument();
    expect(listPackUpgrades).toHaveBeenCalledWith("ticketing", 1);
  });

  it("delivered project: an immediate, untraced change cannot be confirmed without the tick, and the tick is sent", async () => {
    vi.mocked(planPackUpgrade).mockResolvedValue(PLAN({
      delivered: true,
      items: [item("update_safe", { actions: ["apply", "keep"], defaultAction: "apply", applyMode: "immediate_untraced" })],
    }));
    view();
    expect(await screen.findByText("Áp ngay cho khách, không có trong lịch sử")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    const go = screen.getByRole("button", { name: "Áp nâng cấp" });
    expect(go).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /Áp ngay cho khách, không qua bản nháp/ }));
    expect(go).toBeEnabled();
    fireEvent.click(go);
    await waitFor(() => expect(applyPackUpgrade).toHaveBeenCalled());
    expect(vi.mocked(applyPackUpgrade).mock.calls[0][4]).toEqual({ confirmImmediate: true });
  });

  it("drafted changes are told as going to the draft", async () => {
    vi.mocked(applyPackUpgrade).mockResolvedValue(RUN({ counts: { drafted: 3 } }));
    view();
    fireEvent.click(await screen.findByRole("button", { name: "Áp nâng cấp (1 mục)" }));
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp" }));
    expect(await screen.findByText(/3 thay đổi đã vào bản nháp/)).toBeInTheDocument();
  });
});

describe("UpgradePage: history", () => {
  it("lists past runs newest first with their status, and expands the per-item results", async () => {
    vi.mocked(listPackUpgrades).mockResolvedValue([RUN({ id: "r2", toVersion: "1.8.0" }), RUN({ id: "r1", toVersion: "1.7.0", status: "partial" })]);
    view();
    const hist = await screen.findByRole("region", { name: "Lịch sử nâng cấp" });
    await waitFor(() => expect(within(hist).getAllByRole("listitem")).toHaveLength(2));
    expect(within(hist).getByText("Xong một phần")).toBeInTheDocument();
    fireEvent.click(within(hist).getAllByRole("button", { name: "Xem từng mục" })[0]);
    expect(within(hist).getByText("Đã áp")).toBeInTheDocument();
    expect(listPackUpgrades).toHaveBeenCalledWith("ticketing", 10);
  });

  it("no runs yet -> an honest line", async () => {
    view();
    expect(await screen.findByText("Chưa có lần nâng cấp nào.")).toBeInTheDocument();
  });
});
