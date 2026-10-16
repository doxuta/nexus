// specs/079 f7-lich-su-quay-lai T066: "Quay lại bản này" confirms IN the page, lists every document the rollback
// touches, and answers each refusal code with its own sentence. The plan is never cached and the request carries
// the head version of the plan on screen.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const hoisted = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(public status: number, public code: string, message: string, public details?: unknown) {
      super(message);
    }
  }
  return { ok: true, ApiError, getRollbackPlan: vi.fn(), rollbackToVersion: vi.fn(), toast: vi.fn(), emit: vi.fn() };
});
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  ApiError: hoisted.ApiError,
  getRollbackPlan: hoisted.getRollbackPlan,
  rollbackToVersion: hoisted.rollbackToVersion,
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => hoisted.toast(m), error: vi.fn() } }));
vi.mock("@/lib/draftEvents", () => ({ emitDraftsChanged: (s: string) => hoisted.emit(s) }));

import { RollbackPlanConfirm } from "./RollbackPlanConfirm";

const plan = (over = {}) => ({
  targetVersionNo: 2, headVersionNo: 5,
  changes: [
    { docType: "workflow", docKey: "w1", label: "Luật báo giá", op: "update" },
    { docType: "report", docKey: "r1", label: "Báo cáo tuần", op: "create" },
    { docType: "dashboard", docKey: "d9", label: "Bảng mới", op: "delete" },
  ],
  untouchedTypes: [], secretRestores: [], reactivates: [], overridesDirect: [], keptAsIs: [], schemaRestores: [], planToken: "tok", ...over,
});

const onCancel = vi.fn();
const onDone = vi.fn();
const view = (props: Partial<React.ComponentProps<typeof RollbackPlanConfirm>> = {}) =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RollbackPlanConfirm project="acme" version={{ id: "v-2", versionNo: 2 }} pendingCount={0} propagationSeconds={60} onCancel={onCancel} onDone={onDone} {...props} />
    </QueryClientProvider>,
  );
const submit = () => screen.getByRole("button", { name: "Quay lại bản này" });
const err = (code: string, msg = "x") => new hoisted.ApiError(409, code, msg);

beforeEach(() => {
  hoisted.ok = true;
  hoisted.getRollbackPlan.mockReset().mockResolvedValue(plan());
  hoisted.rollbackToVersion.mockReset().mockResolvedValue({ versionNo: 6, kind: "rollback", docCount: 3, publishedAt: "", propagationSeconds: 60 });
  hoisted.toast.mockReset();
  hoisted.emit.mockReset();
  onCancel.mockReset();
  onDone.mockReset();
});
afterEach(() => vi.useRealTimers());

describe("RollbackPlanConfirm: what it says", () => {
  it("states the consequence with the propagation seconds, drops the clause when unknown, and is not a dialog", async () => {
    const { unmount } = view();
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.getByText(/khách thấy bản này từ lần mở app kế tiếp, tối đa 60 giây/)).toBeTruthy();
    expect(document.querySelector("[role=dialog]")).toBeNull();
    unmount();
    view({ propagationSeconds: undefined });
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.queryByText(/tối đa/)).toBeNull();
  });

  it("lists every document in a focusable named region", async () => {
    view();
    const region = await screen.findByRole("region", { name: "Tài liệu sẽ đổi, 3 mục" });
    expect(region.getAttribute("tabindex")).toBe("0");
    expect(within(region).getAllByRole("listitem")).toHaveLength(3);
    expect(within(region).getByText("Luật báo giá")).toBeTruthy();
  });

  it("warns about signing keys, outbound reactivations, an overridden applied-at-once edit, and the roles sentence", async () => {
    hoisted.getRollbackPlan.mockResolvedValue(plan({
      secretRestores: [{ docType: "workflow", docKey: "w1", label: "Luật gửi webhook" }],
      reactivates: [{ docKey: "w2", label: "Luật bật lại ra ngoài", outbound: true }, { docKey: "w3", label: "Luật bật lại trong nhà", outbound: false }],
      overridesDirect: [{ docType: "role", docKey: "r", label: "Vai kinh doanh", byLabel: "Lan", at: "2026-10-01T03:00:00Z" }],
      schemaRestores: [{ docType: "entity", docKey: "e", label: "Hợp đồng", sweptCount: 12 }],
    }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.getByTestId("rollback-secrets").textContent).toContain("Luật gửi webhook");
    const re = screen.getByTestId("rollback-reactivates");
    expect(re.textContent).toContain("Luật bật lại ra ngoài");
    expect(re.textContent?.match(/có gửi dữ liệu hoặc email ra ngoài/g)).toHaveLength(1);
    expect(screen.getByTestId("rollback-overrides").textContent).toContain("ghi đè thay đổi áp ngay của Lan lúc");
    expect(screen.getByTestId("rollback-schema").textContent).toContain("Khôi phục bảng Hợp đồng cùng 12 hồ sơ đã xoá theo");
    expect(screen.getByText(/Vai và quyền không quay lại cùng phiên bản/)).toBeTruthy();
  });

  it("Huỷ gets focus on open; Esc cancels", async () => {
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    const cancel = screen.getByRole("button", { name: "Huỷ" });
    await waitFor(() => expect(document.activeElement).toBe(cancel));
    fireEvent.keyDown(cancel, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("an empty plan says there is nothing to change and keeps the button off", async () => {
    hoisted.getRollbackPlan.mockResolvedValue(plan({ changes: [] }));
    view();
    await screen.findByText(/đã giống phiên bản này/);
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
  });

  it("a plan that cannot be read says so and sends nothing", async () => {
    hoisted.getRollbackPlan.mockRejectedValue(new Error("boom"));
    view();
    await screen.findByText(/Chưa tính được kế hoạch/);
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
  });

  it("a token that does not belong to the project never fetches the plan", async () => {
    hoisted.ok = false;
    view();
    await new Promise((r) => setTimeout(r, 20));
    expect(hoisted.getRollbackPlan).not.toHaveBeenCalled();
  });
});

describe("RollbackPlanConfirm: restored structure (f7-lich-su-khoi-phuc-cau-truc)", () => {
  it("names each restored table with its record count and each field alone; sweptCount 0 or absent drops the clause", async () => {
    hoisted.getRollbackPlan.mockResolvedValue(plan({ schemaRestores: [
      { docType: "entity", docKey: "e1", label: "Hợp đồng", sweptCount: 12 },
      { docType: "entity", docKey: "e2", label: "Phụ lục", sweptCount: 0 },
      { docType: "entity", docKey: "e3", label: "Ghi chú" },
      { docType: "field", docKey: "e1.f", label: "Số hợp đồng" },
    ] }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    const box = screen.getByTestId("rollback-schema");
    expect(within(box).getByText("Sẽ được khôi phục")).toBeTruthy();
    const rows = within(box).getAllByRole("listitem").map((r) => r.textContent);
    expect(rows).toEqual(["Khôi phục bảng Hợp đồng cùng 12 hồ sơ đã xoá theo", "Khôi phục bảng Phụ lục", "Khôi phục bảng Ghi chú", "Khôi phục trường Số hợp đồng"]);
  });

  it("a plan that only hides again what a later publish released: lists it, says nothing is deleted, and can be confirmed", async () => {
    hoisted.getRollbackPlan.mockResolvedValue(plan({ changes: [], rehides: [
      { docType: "field", docKey: "f1", label: "Ghi chú nhanh · crm_deals" },
      { docType: "relationship", docKey: "r1", label: "deal_tags · crm_deals ↔ crm_tags" },
    ] }));
    view();
    const box = await screen.findByTestId("rollback-rehides");
    expect(within(box).getByText(/Sẽ ẩn lại với khách/)).toBeTruthy();
    expect(within(box).getAllByRole("listitem").map((r) => r.textContent)).toEqual([
      "Ghi chú nhanh · crm_deals · Trường", "deal_tags · crm_deals ↔ crm_tags · Quan hệ",
    ]);
    expect(screen.queryByText(/đã giống phiên bản này/)).toBeNull();
    expect((submit() as HTMLButtonElement).disabled).toBe(false);
  });

  it("no schemaRestores: no group", async () => {
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.queryByTestId("rollback-schema")).toBeNull();
  });

  it.each(["ROLLBACK_UNIQUE_CONFLICT", "ROLLBACK_NAME_TAKEN"])("%s shows the server sentence, and the label only from details.label", async (code) => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(new hoisted.ApiError(409, code, "Câu của máy chủ", { label: "Hợp đồng", field: "so_hd", name: "x" }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    const text = (await screen.findByRole("alert")).textContent ?? "";
    expect(text).toContain("Câu của máy chủ");
    expect(text).toContain("Tên: Hợp đồng.");
    expect(text).not.toContain("so_hd");
  });

  it("empty or non-string details.label is ignored; server sentence still shown", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(new hoisted.ApiError(409, "ROLLBACK_NAME_TAKEN", "Câu của máy chủ", { label: 5 }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    const text = (await screen.findByRole("alert")).textContent ?? "";
    expect(text).toContain("Câu của máy chủ");
    expect(text).not.toContain("Tên:");
  });

  it("without details the fixed sentence stands alone", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(err("ROLLBACK_NAME_TAKEN"));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    expect((await screen.findByRole("alert")).textContent).not.toContain("Tên:");
  });
});

describe("RollbackPlanConfirm: when the button is off", () => {
  it("untouched types need the tick, and without it the request is never sent", async () => {
    hoisted.getRollbackPlan.mockResolvedValue(plan({ untouchedTypes: ["report"] }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(submit());
    expect(hoisted.rollbackToVersion).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("checkbox", { name: "Tôi hiểu, giữ nguyên các loại này" }));
    expect((submit() as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalled());
    expect(hoisted.rollbackToVersion.mock.calls[0][1].acceptUntouched).toBe(true);
  });

  it("pending drafts lock the button and say how many", async () => {
    view({ pendingCount: 4 });
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.getByText("Còn 4 thay đổi chưa xuất bản. Xuất bản hoặc bỏ chúng trước khi quay lại.")).toBeTruthy();
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
  });

  it.each([[null], [undefined]])("pendingCount %s (loading, failed or absent) locks the button with the not-checked sentence", async (n) => {
    view({ pendingCount: n as number | null });
    await screen.findByText("3 tài liệu sẽ đổi");
    expect(screen.getByText("Chưa kiểm được số thay đổi chưa xuất bản.")).toBeTruthy();
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("RollbackPlanConfirm: the request", () => {
  it("sends the head version and token of the plan on screen, the note, and no acceptUntouched flag when nothing is untouched", async () => {
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.change(screen.getByLabelText("Ghi chú cho lần quay lại (không bắt buộc)"), { target: { value: " sai luật " } });
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(1));
    expect(hoisted.rollbackToVersion).toHaveBeenCalledWith("v-2", { expectedHeadVersion: 5, expectedPlanToken: "tok", note: "sai luật", acceptUntouched: undefined });
  });

  it("success: one-sentence toast, caches refreshed, onDone with the new version number", async () => {
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await waitFor(() => expect(onDone).toHaveBeenCalledWith(6));
    expect(hoisted.toast).toHaveBeenCalledWith("Đã quay về bản 2: đã xuất bản thành phiên bản 6.");
    expect(hoisted.emit).toHaveBeenCalledWith("rollback");
  });

  it("a double click sends one request", async () => {
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    const btn = submit();
    fireEvent.click(btn);
    fireEvent.click(btn);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(1);
  });

  it("HEAD_MOVED reloads the plan and the next request carries the NEW head version, never the old one", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(err("HEAD_MOVED"));
    hoisted.getRollbackPlan.mockResolvedValueOnce(plan()).mockResolvedValue(plan({ headVersionNo: 7, planToken: "tok2" }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await screen.findByText(/Có phiên bản mới hơn vừa được xuất bản/);
    await waitFor(() => expect(hoisted.getRollbackPlan).toHaveBeenCalledTimes(2));
    await waitFor(() => expect((submit() as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(2));
    expect(hoisted.rollbackToVersion.mock.calls[1][1]).toMatchObject({ expectedHeadVersion: 7, expectedPlanToken: "tok2" });
  });

  it("PLAN_CHANGED reloads the plan and the next request carries the NEW plan token", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(err("PLAN_CHANGED"));
    hoisted.getRollbackPlan.mockResolvedValueOnce(plan()).mockResolvedValue(plan({ planToken: "tok3" }));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await screen.findByText(/Kế hoạch vừa đổi/);
    await waitFor(() => expect(hoisted.getRollbackPlan).toHaveBeenCalledTimes(2));
    await waitFor(() => expect((submit() as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(2));
    expect(hoisted.rollbackToVersion.mock.calls[1][1]).toMatchObject({ expectedPlanToken: "tok3" });
  });

  it("a failed reload after HEAD_MOVED locks the button and says so", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(err("HEAD_MOVED"));
    hoisted.getRollbackPlan.mockResolvedValueOnce(plan()).mockRejectedValue(new Error("net"));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.getRollbackPlan).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
  });

  it.each([
    ["DRAFTS_PENDING", /Còn thay đổi chưa xuất bản/],
    ["ROLLBACK_NAME_TAKEN", /một tên đã bị dùng cho tài liệu khác/],
    ["ROLLBACK_UNIQUE_CONFLICT", /giá trị trùng giữa các hồ sơ/],
    ["ROLLBACK_NO_SNAPSHOT", /chưa chụp một số loại tài liệu/],
    ["ROLLBACK_USE_REVERT", /Hoàn tác/],
  ])("%s gets its own sentence and keeps the confirm open", async (code, re) => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(err(code));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await screen.findByText(re);
    expect(onDone).not.toHaveBeenCalled();
  });

  it("PUBLISH_BUSY is retried exactly once, then answered", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    hoisted.rollbackToVersion.mockRejectedValue(err("PUBLISH_BUSY"));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(1));
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    await waitFor(() => expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(2));
    await screen.findByText(/Hệ thống đang bận xuất bản/);
    await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
    expect(hoisted.rollbackToVersion).toHaveBeenCalledTimes(2);
  });

  it("an unknown failure shows the generic sentence with the server's message", async () => {
    hoisted.rollbackToVersion.mockRejectedValueOnce(new hoisted.ApiError(500, "WEIRD", "máy chủ hỏng"));
    view();
    await screen.findByText("3 tài liệu sẽ đổi");
    fireEvent.click(submit());
    await screen.findByText(/Chưa quay lại được: máy chủ hỏng/);
  });
});
