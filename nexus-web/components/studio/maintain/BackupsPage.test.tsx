// specs/079 f7-sao-luu-bang T091: the backup page — list, create in place, poll while running, download, locks.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type TenantBackup } from "@/lib/api";

const h = vi.hoisted(() => ({ bt: { state: "ok", slug: "acme", tenantId: "t-acme", name: "Acme", isHome: false } as Record<string, unknown> }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listTenantBackups: vi.fn(), createTenantBackup: vi.fn(), downloadTenantBackup: vi.fn(),
}));
vi.mock("./useBackupTenant", async (orig) => ({ ...(await orig<typeof import("./useBackupTenant")>()), useBackupTenant: () => h.bt }));
const nav = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: nav.push, replace: vi.fn() }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { createTenantBackup, downloadTenantBackup, listTenantBackups } from "@/lib/api";
import { BackupsPage } from "./BackupsPage";

const B = (x: Partial<TenantBackup> = {}): TenantBackup => ({
  id: "b1", tenantId: "t-acme", kind: "manual", status: "ready", note: null, sizeBytes: 2048, sha256: null, schemaVersion: 1, formatVersion: 1,
  rowCount: 12400, tableCount: 5, fileCount: 320, blobBytes: 0, warnings: [], error: null, createdAt: "2026-10-14T08:05:00Z", finishedAt: null,
  createdBy: null, restorable: true, notRestorableReason: null, ...x,
});
const U1 = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
const U2 = "11111111-1111-4111-8111-111111111111";
const L = (data: TenantBackup[], nextCursor: string | null = null) => ({ data, meta: { currentSchemaVersion: 1, maintenanceUntil: null, nextCursor } });
const view = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <BackupsPage slug="acme" />
    </QueryClientProvider>,
  );

beforeEach(() => {
  h.bt = { state: "ok", slug: "acme", tenantId: "t-acme", name: "Acme", isHome: false };
  vi.mocked(listTenantBackups).mockReset().mockResolvedValue(L([B()]));
  vi.mocked(createTenantBackup).mockReset().mockResolvedValue(B({ id: "b2", status: "running" }));
  vi.mocked(downloadTenantBackup).mockReset().mockResolvedValue({ sizeBytes: 2048 });
});

describe("BackupsPage", () => {
  it("empty ⇒ sentence + 'Sao lưu ngay'", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(L([]));
    view();
    expect(await screen.findByText("Chưa có bản sao lưu nào")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sao lưu ngay" })).toBeInTheDocument();
  });

  it("four columns; content cell carries size + counts; kind sits under the time", async () => {
    view();
    const table = await screen.findByRole("table");
    expect(within(table).getAllByRole("columnheader")).toHaveLength(4);
    expect(within(table).getByText("12.400 bản ghi · 320 tệp")).toBeInTheDocument();
    expect(within(table).getByText(/Thủ công/)).toBeInTheDocument();
  });

  it("creates in place: note > 200 chars is blocked before the call; ok closes the form", async () => {
    view();
    await userEvent.click(await screen.findByRole("button", { name: "Sao lưu ngay" }));
    const note = screen.getByLabelText(/Ghi chú/);
    fireEvent.change(note, { target: { value: "x".repeat(201) } });
    expect(screen.getByText("Ghi chú tối đa 200 ký tự.")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Sao lưu ngay" }).at(-1)).toBeDisabled();
    fireEvent.change(note, { target: { value: "trước nâng cấp" } });
    await userEvent.click(screen.getAllByRole("button", { name: "Sao lưu ngay" }).at(-1)!);
    await waitFor(() => expect(createTenantBackup).toHaveBeenCalledWith("t-acme", "trước nâng cấp"));
    await waitFor(() => expect(screen.queryByLabelText(/Ghi chú/)).toBeNull());
  });

  it("409 BACKUP_DISK_LOW states how much is left", async () => {
    vi.mocked(createTenantBackup).mockRejectedValue(new ApiError(409, "BACKUP_DISK_LOW", "x", { freeBytes: 1024, needBytes: 4096 }));
    view();
    await userEvent.click(await screen.findByRole("button", { name: "Sao lưu ngay" }));
    await userEvent.click(screen.getAllByRole("button", { name: "Sao lưu ngay" }).at(-1)!);
    expect(await screen.findByText(/còn 1,0 KB trống/)).toBeInTheDocument();
  });

  it("failed shows the error sentence; warnings sit under the status", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(L([
      B({ id: "f", status: "failed", error: { code: "X", message: "Hết chỗ ghi" }, restorable: false, notRestorableReason: "NOT_READY" }),
      B({ id: "w", warnings: [{ code: "BLOB_MISSING", count: 3 }] }),
    ]));
    view();
    expect(await screen.findByText("Hết chỗ ghi")).toBeInTheDocument();
    expect(screen.getByText("3 tệp đính kèm không tìm thấy")).toBeInTheDocument();
  });

  it("Restore is locked with the reason unless restorable === true; pre_restore says 'Hoàn tác'", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(L([
      B({ id: "s", restorable: false, notRestorableReason: "SCHEMA_MISMATCH" }),
      B({ id: "u", restorable: undefined as unknown as boolean }),
      B({ id: "p", kind: "pre_restore" }),
    ]));
    view();
    expect(await screen.findByText("Bản này tạo ở phiên bản cấu trúc khác; chưa khôi phục được.")).toBeInTheDocument();
    expect(screen.getByText("Chưa biết bản này khôi phục được không.")).toBeInTheDocument();
    const restores = screen.getAllByRole("button", { name: /khôi phục|Hoàn tác/i }).filter((b) => /Khôi phục|Hoàn tác/.test(b.textContent ?? ""));
    expect(restores.map((b) => (b as HTMLButtonElement).disabled)).toEqual([true, true, false]);
    expect(within(restores[2]).getByText("Hoàn tác")).toBeInTheDocument();
  });

  it("download: file name from the slug at the click time; the size is said; no storage write", async () => {
    view();
    await userEvent.click(await screen.findByRole("button", { name: /Tải xuống/ }));
    await waitFor(() => expect(downloadTenantBackup).toHaveBeenCalled());
    const args = vi.mocked(downloadTenantBackup).mock.calls[0];
    expect(args.slice(0, 2)).toEqual(["t-acme", "b1"]);
    expect(args[2]).toMatch(/^acme-\d{8}-\d{4}\.zip$/);
    expect(args[3]).toBeInstanceOf(AbortSignal);
    expect(window.localStorage.getItem("blob")).toBeNull();
  });

  it("'Xem thêm' appends the next page with after=", async () => {
    vi.mocked(listTenantBackups).mockResolvedValueOnce(L([B()], "cur1")).mockResolvedValueOnce(L([B({ id: "b9", note: "bản cũ" })]));
    view();
    await userEvent.click(await screen.findByRole("button", { name: "Xem thêm" }));
    expect(await screen.findByText("bản cũ")).toBeInTheDocument();
    expect(vi.mocked(listTenantBackups).mock.calls[1]).toEqual(["t-acme", { limit: 20, after: "cur1" }]);
  });

  it("a bare 404 (route not built) ⇒ honest line, no button", async () => {
    vi.mocked(listTenantBackups).mockRejectedValue(new ApiError(404, "UNKNOWN", "404 page not found"));
    view();
    expect(await screen.findByText("Sao lưu và khôi phục chưa có trong bản Nexus này.")).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("running ⇒ polls every 3 s and stops once ready", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(listTenantBackups).mockResolvedValueOnce(L([B({ status: "running" })])).mockResolvedValue(L([B({ status: "ready" })]));
    view();
    await screen.findByText("Đang sao lưu");
    await vi.advanceTimersByTimeAsync(3100);
    await screen.findByText("Sẵn sàng");
    const n = vi.mocked(listTenantBackups).mock.calls.length;
    await vi.advanceTimersByTimeAsync(10000);
    expect(vi.mocked(listTenantBackups).mock.calls.length).toBe(n);
    vi.useRealTimers();
  });

  it("'Khôi phục' goes to the restore page of that backup; 'Hoàn tác' of a pre_restore row goes to its own page", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(L([B({ id: U1 }), B({ id: U2, kind: "pre_restore" })]));
    view();
    await screen.findAllByText(/bản ghi/);
    const u = userEvent.setup();
    const [restore, undo] = screen.getAllByRole("button", { name: /khôi phục —|Hoàn tác —/i });
    await u.click(restore);
    expect(nav.push).toHaveBeenLastCalledWith(`/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${U1}`);
    await u.click(undo);
    expect(nav.push).toHaveBeenLastCalledWith(`/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${U2}`);
  });

  it("a pre_restore row says which backup it was taken before", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(L([
      B({ id: U1, createdAt: "2026-10-10T03:00:00Z" }),
      B({ id: U2, kind: "pre_restore", restore: { sourceBackupId: U1, status: "done", step: "doctor", committedAt: null, report: null, finishedAt: null } }),
    ]));
    view();
    expect(await screen.findByText(/^Tự sao lưu trước khi khôi phục bản /)).toBeInTheDocument();
  });
});
