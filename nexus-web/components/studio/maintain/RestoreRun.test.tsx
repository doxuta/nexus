// specs/079 f7-khoi-phuc-chay: phases 2-3 of the restore page (?lan=<restoreId>): four-step progress read from the
// server's own `restore.step`, the report, and the next action of every failure branch (US9.4-9.6, FR-122, FR-124).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type ProjectRow, type TenantBackup } from "@/lib/api";

const nav = vi.hoisted(() => ({ replace: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: nav.replace, push: vi.fn() }) }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  previewTenantRestore: vi.fn(),
  startTenantRestore: vi.fn(),
  getTenantBackup: vi.fn(),
  enterWorkspace: vi.fn(),
  listProjects: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ status: "ready", home: "northwind", me: { tenantId: "t-home", isPlatformOperator: true }, matches: () => true }),
}));

import { enterWorkspace, getTenantBackup, listProjects, previewTenantRestore, startTenantRestore } from "@/lib/api";
import { RestorePage } from "./RestorePage";

const BID = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
const LAN = "11111111-1111-4111-8111-111111111111";
type Restore = NonNullable<TenantBackup["restore"]>;
const ROW = (restore: Partial<Restore> | null, x: Partial<TenantBackup> = {}): TenantBackup => ({
  id: LAN, tenantId: "t-acme", kind: "pre_restore", status: "ready", note: null, sizeBytes: 10, sha256: null, schemaVersion: 1, formatVersion: 1,
  rowCount: 1, tableCount: 1, fileCount: 0, blobBytes: 0, warnings: [], error: null, createdAt: "2026-10-14T08:00:00Z", finishedAt: null,
  createdBy: null, restorable: true, notRestorableReason: null,
  restore: restore && { sourceBackupId: BID, status: "running", step: "lock", committedAt: null, report: null, finishedAt: null, ...restore },
  ...x,
});
const REPORT = {
  sourceBackupId: BID, preRestoreBackupId: LAN, deleted: { obj_a: 5, rel_a_b: 2, sys_x: 1, sys_files: 3 }, inserted: { obj_a: 7, sys_x: 1 },
  rolesLost: 2, membershipsSkipped: 3, sharesSkipped: 4, webhookKeysDropped: 1, blobsWritten: 6, blobsRemoved: 8, doctor: { fixed: 2, remainingCritical: 1 },
};

const view = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RestorePage project="acme" backupId={BID} lan={LAN} />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.mocked(listProjects).mockReset().mockResolvedValue([{ tenantId: "t-acme", slug: "acme", name: "acme" } as ProjectRow]);
  vi.mocked(getTenantBackup).mockReset();
  vi.mocked(previewTenantRestore).mockReset();
  vi.mocked(startTenantRestore).mockReset();
  vi.mocked(enterWorkspace).mockReset().mockResolvedValue();
});
afterEach(() => vi.useRealTimers());

const steps = () => Array.from(document.querySelectorAll(".slds-progress__item"));

describe("RestorePage with ?lan=", () => {
  it("follows the restore row (no plan call) and marks the step the server wrote", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "running", step: "apply", committedAt: "2026-10-14T08:01:00Z" }));
    view();
    expect(await screen.findByText("Ghi lại dữ liệu và cấu hình")).toBeInTheDocument();
    expect(getTenantBackup).toHaveBeenCalledWith("t-acme", LAN);
    expect(previewTenantRestore).not.toHaveBeenCalled();
    const items = steps();
    expect(items).toHaveLength(4);
    expect(items[0]).toHaveClass("slds-is-completed");
    expect(items[1]).toHaveClass("slds-is-completed");
    expect(items[2]).toHaveClass("slds-is-active");
    expect(items[3]).not.toHaveClass("slds-is-active");
    expect(screen.getByText(/Đã ghi dữ liệu lúc/)).toBeInTheDocument();
    expect(screen.getByText(/Việc vẫn chạy nếu anh rời trang/)).toBeInTheDocument();
  });

  it("polls every 2 seconds while running and stops at done", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(getTenantBackup)
      .mockResolvedValueOnce(ROW({ status: "running", step: "lock" }))
      .mockResolvedValueOnce(ROW({ status: "running", step: "doctor", committedAt: "2026-10-14T08:01:00Z" }))
      .mockResolvedValue(ROW({ status: "done", step: "doctor", committedAt: "2026-10-14T08:01:00Z", report: REPORT, finishedAt: "2026-10-14T08:02:00Z" }));
    view();
    await screen.findByText("Khoá ghi");
    expect(getTenantBackup).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2100);
    await waitFor(() => expect(getTenantBackup).toHaveBeenCalledTimes(2));
    await vi.advanceTimersByTimeAsync(2100);
    await waitFor(() => expect(screen.getByText("Báo cáo khôi phục")).toBeInTheDocument());
    const calls = vi.mocked(getTenantBackup).mock.calls.length;
    await vi.advanceTimersByTimeAsync(7000);
    expect(getTenantBackup).toHaveBeenCalledTimes(calls);
  });

  it("a dropped poll does not end the watch", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(getTenantBackup)
      .mockResolvedValueOnce(ROW({ status: "running", step: "lock" }))
      .mockRejectedValueOnce(new TypeError("fetch failed"))
      .mockResolvedValue(ROW({ status: "done", step: "doctor", report: REPORT, finishedAt: "2026-10-14T08:02:00Z" }));
    view();
    await screen.findByText("Khoá ghi");
    await vi.advanceTimersByTimeAsync(2100);
    // the dropped poll keeps the progress on screen with a warning, never the red error + retry
    await waitFor(() => expect(screen.getByText(/Việc vẫn chạy trên máy chủ/)).toBeInTheDocument());
    expect(steps()).toHaveLength(4);
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
    await vi.advanceTimersByTimeAsync(2100);
    await waitFor(() => expect(screen.getByText("Báo cáo khôi phục")).toBeInTheDocument());
  });

  it("moves focus to the progress heading once it appears", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "running", step: "lock" }));
    view();
    const h = await screen.findByRole("heading", { name: "Tiến trình khôi phục" });
    await waitFor(() => expect(document.activeElement).toBe(h));
  });

  it("done: report with the numbers, 'Mở lại dự án' enters the project, 'Hoàn tác' goes to the pre_restore row, live region says done", async () => {
    const u = userEvent.setup();
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "done", step: "doctor", committedAt: "2026-10-14T08:01:00Z", report: REPORT, finishedAt: "2026-10-14T08:02:30Z" }));
    view();
    expect(await screen.findByText("Báo cáo khôi phục")).toBeInTheDocument();
    expect(screen.getByText("Người mất vai: 2")).toBeInTheDocument();
    expect(screen.getByText("Thành viên đội bỏ qua: 3")).toBeInTheDocument();
    expect(screen.getByText("Chia sẻ bỏ qua: 4")).toBeInTheDocument();
    expect(screen.getByText(/1 luật gửi dữ liệu ra ngoài không còn khoá ký \(cần đặt lại khoá\)/)).toBeInTheDocument();
    expect(screen.getByText("Tệp đính kèm: ghi 6, gỡ 8")).toBeInTheDocument();
    expect(screen.getByText("Khám sức khoẻ: sửa 2, còn 1 lỗi nặng")).toBeInTheDocument();
    expect(screen.getByText("Thời gian: 2 phút 30 giây")).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/obj_|rel_|sys_/);
    const row = (name: string) => screen.getByRole("rowheader", { name }).closest("tr") as HTMLElement;
    expect(row("Dữ liệu").textContent).toContain("7");
    expect(row("Tệp").textContent).toContain("3");
    expect(screen.getByText("Khôi phục xong", { selector: "[aria-live]" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Hoàn tác khôi phục này" })).toHaveAttribute("href", `/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${LAN}`);
    await u.click(screen.getByRole("button", { name: "Mở lại dự án" }));
    expect(enterWorkspace).toHaveBeenCalledWith("acme", { next: "/studio/du-an/acme/van-hanh/du-lieu" });
  });

  it("resumedAfterCrash is said", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "done", report: { ...REPORT, resumedAfterCrash: true } }));
    view();
    expect(await screen.findByText(/Máy chủ dừng giữa chừng — phần dọn dẹp đã chạy lại/)).toBeInTheDocument();
  });

  it("failed BEFORE the write: customer untouched, plan link + back link, no restore-again button", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "failed", step: "prebackup" }, { error: { code: "TENANT_RESTORE_FAILED", message: "disk" } }));
    view();
    expect(await screen.findByText(/dữ liệu của khách không đổi/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Xem lại kế hoạch" })).toHaveAttribute("href", `/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${BID}`);
    expect(screen.getByRole("link", { name: /Sao lưu/ })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Khôi phục về bản tự sao lưu trước đó" })).toBeNull();
    expect(steps()[1]).toHaveClass("slds-has-error");
  });

  it("PREBACKUP_FAILED: the customer was not touched", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "failed", step: "prebackup" }, { error: { code: "TENANT_RESTORE_PREBACKUP_FAILED", message: "x" } }));
    view();
    expect(await screen.findByText(/khách không bị đụng gì/)).toBeInTheDocument();
  });

  it("failed AFTER the write: data changed in part; the way back is offered only when the pre_restore row is restorable", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "failed", step: "doctor", committedAt: "2026-10-14T08:01:00Z" }, { error: { code: "TENANT_RESTORE_FAILED", message: "x" } }));
    const a = view();
    expect(await screen.findByText("Dữ liệu của khách đã bị đổi một phần")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Khôi phục về bản tự sao lưu trước đó" })).toHaveAttribute("href", `/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${LAN}`);
    a.unmount();
    vi.mocked(getTenantBackup).mockResolvedValue(
      ROW({ status: "failed", step: "doctor", committedAt: "2026-10-14T08:01:00Z" }, { restorable: false, notRestorableReason: "FILE_MISSING", error: { code: "TENANT_RESTORE_FAILED", message: "x" } }),
    );
    view();
    expect(await screen.findByText(/Bản tự sao lưu trước đó không dùng được: Tệp của bản này không còn trên máy chủ\./)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Khôi phục về bản tự sao lưu trước đó" })).toBeNull();
  });

  it("a ?lan= of another restore, or a 404, reads 'không có lần khôi phục này' with a way back", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ sourceBackupId: "another" }));
    const a = view();
    expect(await screen.findByText("Không có lần khôi phục này trong dự án")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Sao lưu/ })).toBeInTheDocument();
    a.unmount();
    vi.mocked(getTenantBackup).mockRejectedValue(new ApiError(404, "BACKUP_NOT_FOUND", "x"));
    view();
    expect(await screen.findByText("Không có lần khôi phục này trong dự án")).toBeInTheDocument();
  });

  it("closes the projects cache once when the restore ends", async () => {
    vi.mocked(getTenantBackup).mockResolvedValue(ROW({ status: "done", report: REPORT, finishedAt: "2026-10-14T08:02:00Z" }));
    const q = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(q, "invalidateQueries");
    render(<QueryClientProvider client={q}><RestorePage project="acme" backupId={BID} lan={LAN} /></QueryClientProvider>);
    await screen.findByText("Báo cáo khôi phục");
    await waitFor(() => expect(spy).toHaveBeenCalledWith({ queryKey: ["studio", "projects"] }));
    expect(spy).toHaveBeenCalledWith({ queryKey: ["projects"] });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["home", "acme", "t-acme", "backups"] });
  });
});
