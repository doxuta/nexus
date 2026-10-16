// specs/079 f7-lich-su-dong-thoi-gian T054/T055: the history timeline and the one-version page.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type PublishState, type PublishVersion } from "@/lib/api";

const hoisted = vi.hoisted(() => ({ ok: true, replace: vi.fn(), push: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  getPublishState: vi.fn(),
  listVersions: vi.fn(),
  getVersion: vi.fn(),
  getRollbackPlan: vi.fn(),
  rollbackToVersion: vi.fn(),
  retrySchemaOps: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
}));
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: hoisted.replace, push: hoisted.push }) }));
vi.mock("./RevertConfirm", () => ({ RevertConfirm: ({ versionId }: { versionId: string }) => <div>revert:{versionId}</div> }));

import { getPublishState, getRollbackPlan, getVersion, listVersions, retrySchemaOps, rollbackToVersion } from "@/lib/api";
import { HistoryPage } from "./HistoryPage";
import { VersionPage } from "./VersionPage";

const ver = (seq: number, extra: Partial<PublishVersion> = {}): PublishVersion => ({
  id: `id-${seq}`, seq, versionNo: seq, kind: "publish", note: "", docCount: 2, createdAt: "2026-10-14T01:00:00Z", createdBy: "u", createdByName: "Minh", rollbackOf: null, schemaStatus: "", ...extra,
});
const STATE = (extra: Partial<PublishState> = {}): PublishState => ({
  delivered: true, headVersion: { versionNo: 3, kind: "publish", publishedAt: "", publishedBy: "" }, pendingCount: 0, changesetHash: "", draftableTypes: [], directTypes: [], propagationSeconds: 0, ...extra,
});
const wrap = (ui: React.ReactNode) => render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui}</QueryClientProvider>);

beforeEach(() => {
  hoisted.ok = true;
  hoisted.replace.mockReset();
  hoisted.push.mockReset();
  vi.mocked(rollbackToVersion).mockReset().mockResolvedValue({ versionNo: 5, kind: "rollback", docCount: 1, publishedAt: "", propagationSeconds: 60 });
  vi.mocked(getPublishState).mockReset().mockResolvedValue(STATE());
  vi.mocked(listVersions).mockReset().mockResolvedValue({ versions: [ver(4, { versionNo: null, kind: "direct" }), ver(3), ver(2, { kind: "rollback", rollbackOf: 1 }), ver(1, { kind: "baseline" })], total: 4 });
  vi.mocked(getVersion).mockReset();
  vi.mocked(getRollbackPlan).mockReset().mockResolvedValue({ targetVersionNo: 2, headVersionNo: 3, changes: [{ docType: "workflow", docKey: "w", label: "Luật báo giá", op: "update" }], untouchedTypes: [], secretRestores: [], reactivates: [], overridesDirect: [], keptAsIs: [], schemaRestores: [], planToken: "tok" });
});

describe("HistoryPage", () => {
  it("marks only the head version as in use and an applied-at-once entry as Áp ngay with Hoàn tác", async () => {
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.getAllByText("Khách đang dùng")).toHaveLength(1);
    expect(screen.getByText("Áp ngay")).toBeTruthy();
    expect(screen.getByText("Quay về phiên bản 1")).toBeTruthy();
    expect(screen.getByText("Bản đầu tiên giao cho khách")).toBeTruthy();
    expect(screen.getAllByText("2 thay đổi").length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(screen.getByText("revert:id-4")).toBeTruthy();
  });

  it("a version whose structure delete failed shows Xoá chưa xong with Thử lại; Hoàn tác opens inline and hides its own button", async () => {
    vi.mocked(listVersions).mockResolvedValue({ versions: [ver(4, { versionNo: null, kind: "direct" }), ver(3, { schemaStatus: "failed" })], total: 2 });
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.getByText("Xoá chưa xong")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Thử lại" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(screen.getByText("revert:id-4")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
  });

  it("not delivered: says so and reads no versions", async () => {
    vi.mocked(getPublishState).mockResolvedValue(STATE({ delivered: false, headVersion: null }));
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText(/Chưa giao cho khách nên chưa có phiên bản nào/);
    expect(listVersions).not.toHaveBeenCalled();
  });

  it("old server (404 UNKNOWN): one honest line and the audit log link, no other button", async () => {
    vi.mocked(listVersions).mockRejectedValue(new ApiError(404, "UNKNOWN", "not found"));
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Lịch sử xuất bản chưa có trong bản Nexus này.");
    expect(screen.getByText("Nhật ký hoạt động")).toBeTruthy();
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });

  it("shows 20 and 'Xem thêm' asks for 20 more", async () => {
    vi.mocked(listVersions).mockImplementation(async (limit = 20) => ({ versions: Array.from({ length: Math.min(limit, 25) }, (_, i) => ver(25 - i)), total: 25 }));
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 25");
    expect(screen.getAllByText(/^Phiên bản \d+$/)).toHaveLength(20);
    fireEvent.click(screen.getByRole("button", { name: "Xem thêm" }));
    await waitFor(() => expect(screen.getAllByText(/^Phiên bản \d+$/)).toHaveLength(25));
  });

  it("a token that is not this project's makes no call at all", () => {
    hoisted.ok = false;
    wrap(<HistoryPage slug="acme" />);
    expect(getPublishState).not.toHaveBeenCalled();
    expect(listVersions).not.toHaveBeenCalled();
  });

  it("'Xem thay đổi' is a toggle that reads the version once and lists its documents", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3), createdByLabel: "Minh", docs: [{ docType: "workflow", docKey: "w", label: "Luật báo giá", op: "update", changedPaths: ["name"], moreChanged: 0, before: {}, after: {}, applyStatus: "applied" }] });
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    const btn = screen.getAllByRole("button", { name: "Xem thay đổi" })[1];
    expect(btn.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(btn);
    await screen.findByText(/Luật báo giá/);
    expect(getVersion).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Ẩn thay đổi" }).getAttribute("aria-expanded")).toBe("true");
  });
});

describe("rollback entry (f7-lich-su-quay-lai T068)", () => {
  const confirmBtn = () => screen.getByRole("button", { name: "Quay lại bản này" }) as HTMLButtonElement;
  const openRow2 = async () => {
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    fireEvent.click(screen.getByRole("button", { name: "Quay lại bản 2" }));
    await screen.findByText("1 tài liệu sẽ đổi");
    await waitFor(() => expect(getRollbackPlan).toHaveBeenCalled());
  };
  it("history: button only on numbered versions that are not the running one; click opens the plan under the row, Esc closes and returns focus to the row button", async () => {
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    // versions 3 (head) and the applied-at-once entry have none; 2 and 1 do
    expect(screen.getAllByRole("button", { name: /^Quay lại bản \d$/ })).toHaveLength(2);
    const trigger = screen.getByRole("button", { name: "Quay lại bản 2" });
    fireEvent.click(trigger);
    await screen.findByText("1 tài liệu sẽ đổi");
    expect(getRollbackPlan).toHaveBeenCalledWith("id-2");
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.keyDown(screen.getByRole("button", { name: "Huỷ" }), { key: "Escape" });
    await waitFor(() => expect(screen.queryByText("1 tài liệu sẽ đổi")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Quay lại bản 2" })));
  });
  it("history: an unknown running version (state failed) offers no rollback button on any row", async () => {
    vi.mocked(getPublishState).mockResolvedValue(STATE({ headVersion: null }));
    wrap(<HistoryPage slug="acme" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.queryAllByRole("button", { name: /^Quay lại bản \d$/ })).toHaveLength(0);
  });
  it("history: Huỷ also returns focus to the row button", async () => {
    await openRow2();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Quay lại bản 2" })));
  });
  it("history: pendingCount > 0 locks the confirm and names the count", async () => {
    vi.mocked(getPublishState).mockResolvedValue(STATE({ pendingCount: 2 }));
    await openRow2();
    expect(confirmBtn().disabled).toBe(true);
    expect(screen.getByText(/2 thay đổi chưa xuất bản/)).toBeTruthy();
  });
  it("history: an unknown pendingCount locks the confirm (never read as 0)", async () => {
    vi.mocked(getPublishState).mockResolvedValue(STATE({ pendingCount: undefined as unknown as number }));
    await openRow2();
    expect(confirmBtn().disabled).toBe(true);
    expect(screen.getByText("Chưa kiểm được số thay đổi chưa xuất bản.")).toBeTruthy();
  });
  it("history: confirm passes propagationSeconds into the sentence, sends the plan's head, closes the block and focuses the list", async () => {
    vi.mocked(getPublishState).mockResolvedValue(STATE({ propagationSeconds: 45 }));
    await openRow2();
    expect(screen.getByText(/tối đa 45 giây/)).toBeTruthy();
    await waitFor(() => expect(confirmBtn().disabled).toBe(false));
    fireEvent.click(confirmBtn());
    await waitFor(() => expect(rollbackToVersion).toHaveBeenCalledTimes(1));
    expect(vi.mocked(rollbackToVersion).mock.calls[0][0]).toBe("id-2");
    expect(vi.mocked(rollbackToVersion).mock.calls[0][1]).toMatchObject({ expectedHeadVersion: 3, expectedPlanToken: "tok" });
    await waitFor(() => expect(screen.queryByText("1 tài liệu sẽ đổi")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("list", { name: "Các lần xuất bản" })));
  });
  it("version page: button on a non-running numbered version, absent on the running one", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(2), createdByLabel: "Minh", docs: [] });
    const { unmount } = wrap(<VersionPage slug="acme" id="id-2" />);
    await screen.findByRole("button", { name: "Quay lại bản 2" });
    unmount();
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3), createdByLabel: "Minh", docs: [] });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    await waitFor(() => expect(getPublishState).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /^Quay lại bản/ })).toBeNull();
  });
  it("version page: no button while the publish state is unknown (loading or failed), so the running version is never offered", async () => {
    vi.mocked(getPublishState).mockRejectedValue(new Error("boom"));
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3), createdByLabel: "Minh", docs: [] });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.queryByRole("button", { name: /^Quay lại bản/ })).toBeNull();
    await waitFor(() => expect(getPublishState).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /^Quay lại bản/ })).toBeNull();
  });
  it("version page: Huỷ returns focus to the button; a finished rollback goes back to the history", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(2), createdByLabel: "Minh", docs: [] });
    wrap(<VersionPage slug="acme" id="id-2" />);
    fireEvent.click(await screen.findByRole("button", { name: "Quay lại bản 2" }));
    await screen.findByText("1 tài liệu sẽ đổi");
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Quay lại bản 2" })));
    fireEvent.click(screen.getByRole("button", { name: "Quay lại bản 2" }));
    await screen.findByText("1 tài liệu sẽ đổi");
    await waitFor(() => expect(confirmBtn().disabled).toBe(false));
    fireEvent.click(confirmBtn());
    await waitFor(() => expect(hoisted.push).toHaveBeenCalledTimes(1));
    expect(hoisted.push.mock.calls[0][0]).toContain("lich-su");
    expect(hoisted.push.mock.calls[0][0]).not.toContain("id-2");
  });
});

describe("VersionPage", () => {
  const docs = [
    { docType: "workflow", docKey: "w", label: "Luật A", op: "update" as const, changedPaths: ["name", "gone.x"], moreChanged: 0, before: { name: "a" }, after: { name: "b" }, applyStatus: "applied" as const },
    { docType: "report", docKey: "r", label: "Báo cáo B", op: "update" as const, changedPaths: ["title"], moreChanged: 0, before: { title: "x" }, after: { title: "y" }, applyStatus: "failed" as const, applyError: "lỗi gì đó" },
  ];
  it("shows before / after, keeps unresolved paths under Chi tiết, shows applyError, no Bảng filter without entityName", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3), createdByLabel: "Minh", docs });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.getByText("Luật A")).toBeTruthy();
    expect(screen.getByText("gone.x")).toBeTruthy();
    expect(screen.getByText(/lỗi gì đó/)).toBeTruthy();
    expect(screen.queryByLabelText("Bảng")).toBeNull();
  });
  it("filter writes ?loai= with router.replace and narrows the documents", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3), createdByLabel: "Minh", docs });
    wrap(<VersionPage slug="acme" id="id-3" loai="report" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.queryByText("Luật A")).toBeNull();
    fireEvent.change(screen.getByLabelText("Loại"), { target: { value: "workflow" } });
    expect(hoisted.replace).toHaveBeenCalledWith(expect.stringContaining("loai=workflow"));
  });
  it("schemaStatus failed on the version offers 'Thử lại phần sửa cấu trúc bảng'", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3, { schemaStatus: "failed" }), createdByLabel: "Minh", docs });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.getByRole("button", { name: "Thử lại phần sửa cấu trúc bảng" })).toBeTruthy();
  });
  it("retry on the version page: nothing sent until the confirm, then retrySchemaOps(id)", async () => {
    vi.mocked(retrySchemaOps).mockReset().mockResolvedValue({ schemaStatus: "done", schemaOps: [] });
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3, { schemaStatus: "failed" }), createdByLabel: "Minh", docs });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    fireEvent.click(screen.getByRole("button", { name: "Thử lại phần sửa cấu trúc bảng" }));
    const go = await screen.findByRole("button", { name: "Xoá tiếp" });
    expect(retrySchemaOps).not.toHaveBeenCalled();
    fireEvent.click(go);
    await waitFor(() => expect(retrySchemaOps).toHaveBeenCalledWith("id-3"));
  });
  it("no retry button when schemaStatus is not failed", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3, { schemaStatus: "done" }), createdByLabel: "Minh", docs });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText("Phiên bản 3");
    expect(screen.queryByRole("button", { name: /Thử lại/ })).toBeNull();
  });
  it("a version with notes only gets its own sentence", async () => {
    vi.mocked(getVersion).mockResolvedValue({ ...ver(3, { note: "ghi chú" }), createdByLabel: "Minh", docs: [] });
    wrap(<VersionPage slug="acme" id="id-3" />);
    await screen.findByText(/chỉ có ghi chú/);
  });
  it("VERSION_NOT_FOUND is an empty state with a way back, not an error", async () => {
    vi.mocked(getVersion).mockRejectedValue(new ApiError(404, "VERSION_NOT_FOUND", "x"));
    wrap(<VersionPage slug="acme" id="nope" />);
    await screen.findByText("Không có phiên bản này.");
    expect(screen.getByText("← Lịch sử xuất bản")).toBeTruthy();
  });
  it("a token that is not this project's makes no call", () => {
    hoisted.ok = false;
    wrap(<VersionPage slug="acme" id="id-3" />);
    expect(getVersion).not.toHaveBeenCalled();
  });
});
