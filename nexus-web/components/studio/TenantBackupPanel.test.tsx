// TenantBackupPanel — specs/068-tenant-backup-restore lát A (T029): the operator's list of one
// customer's backups, "Sao lưu ngay", the poll while a backup runs, and the download.
// lib/api is mocked at the module seam (same pattern as PackDiffPanel.test.tsx).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TenantBackupPanel } from "./TenantBackupPanel";
import {
  ApiError, clearTenantMaintenance, createTenantBackup, downloadTenantBackup, getTenantBackup, listTenantBackups,
  deleteTenantBackup, getTenantBackupSchedule, previewTenantRestore, putTenantBackupSchedule, setTenantMaintenance,
  type TenantBackup, type TenantBackupSchedule,
} from "@/lib/api";
import { toast } from "@/components/ui/Toaster";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listTenantBackups: vi.fn(), createTenantBackup: vi.fn(), getTenantBackup: vi.fn(), downloadTenantBackup: vi.fn(),
    setTenantMaintenance: vi.fn(), clearTenantMaintenance: vi.fn(), previewTenantRestore: vi.fn(),
    deleteTenantBackup: vi.fn(), getTenantBackupSchedule: vi.fn(), putTenantBackupSchedule: vi.fn() };
});
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const TENANT = { id: "t-1", slug: "qa-x", name: "Khách QA X" };

function backup(over: Partial<TenantBackup>): TenantBackup {
  return {
    id: "b-1", tenantId: "t-1", kind: "manual", status: "ready", note: null, sizeBytes: 2048, sha256: "a".repeat(64),
    schemaVersion: 85, formatVersion: 1, rowCount: 120, tableCount: 40, fileCount: 3, blobBytes: 1000, warnings: [],
    error: null, createdAt: "2026-10-07T01:02:03.000Z", finishedAt: "2026-10-07T01:02:09.000Z",
    createdBy: { id: "u-1", label: "Nguyễn Văn A (Nexus)" }, restorable: true, notRestorableReason: null, ...over,
  };
}
const list = (data: TenantBackup[], nextCursor: string | null = null) => ({
  data, meta: { currentSchemaVersion: 85, maintenanceUntil: null, nextCursor },
});

const SCHEDULE: TenantBackupSchedule = {
  enabled: true, keepScheduled: 7, hourLocal: 2, lastScheduledAt: "2026-10-13T19:00:05.000Z", nextDueAt: "2026-10-14T19:00:00.000Z",
};

beforeEach(() => {
  vi.mocked(getTenantBackupSchedule).mockReset().mockResolvedValue(SCHEDULE);
  vi.mocked(putTenantBackupSchedule).mockReset();
  vi.mocked(deleteTenantBackup).mockReset();
  vi.mocked(listTenantBackups).mockReset();
  vi.mocked(createTenantBackup).mockReset();
  vi.mocked(getTenantBackup).mockReset();
  vi.mocked(downloadTenantBackup).mockReset();
  vi.mocked(setTenantMaintenance).mockReset();
  vi.mocked(clearTenantMaintenance).mockReset();
  vi.mocked(toast.success).mockReset();
  vi.mocked(toast.error).mockReset();
});
afterEach(() => { vi.useRealTimers(); });

describe("TenantBackupPanel", () => {
  it("shows each backup's status, size, data counts, kind, author and warnings", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([
      backup({ id: "b-ready", warnings: [{ code: "BLOB_MISSING", count: 2 }] }),
      backup({ id: "b-run", status: "running", sizeBytes: null, rowCount: null, fileCount: null, restorable: false, notRestorableReason: "NOT_READY" }),
      backup({ id: "b-fail", status: "failed", error: { code: "BACKUP_WRITE_FAILED", message: "Không ghi được bản sao lưu" }, kind: "scheduled", createdBy: null }),
    ]));
    render(<TenantBackupPanel tenant={TENANT} />);

    expect(await screen.findByText("Sẵn sàng")).toBeInTheDocument();
    expect(screen.getByText("Đang sao lưu")).toBeInTheDocument();
    expect(screen.getByText("Lỗi")).toBeInTheDocument();
    // the ready and the failed row both carry counts; the running one shows a dash
    expect(screen.getAllByText("120 dòng · 3 tệp")).toHaveLength(2);
    expect(screen.getByText("2 tệp đính kèm không tìm thấy")).toBeInTheDocument();
    expect(screen.getByText("Theo lịch")).toBeInTheDocument();
    expect(screen.getByText("Không ghi được bản sao lưu")).toBeInTheDocument();
    expect(screen.getAllByText(/bởi Nguyễn Văn A \(Nexus\)/).length).toBeGreaterThan(0);
    expect(listTenantBackups).toHaveBeenCalledWith("t-1", expect.anything());
  });

  it("the privacy warning about real data is always visible next to the download", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({})]));
    render(<TenantBackupPanel tenant={TENANT} />);
    expect(await screen.findByText(/chứa dữ liệu thật của khách/)).toBeInTheDocument();
  });

  it("empty list says so and offers the action", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    render(<TenantBackupPanel tenant={TENANT} />);
    expect(await screen.findByText("Chưa có bản sao lưu nào")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sao lưu ngay" })).toBeEnabled();
  });

  it("download is locked unless the backup is ready", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([
      backup({ id: "b-ready" }),
      backup({ id: "b-run", status: "running", restorable: false }),
      backup({ id: "b-fail", status: "failed", restorable: false }),
    ]));
    render(<TenantBackupPanel tenant={TENANT} />);
    await screen.findByText("Sẵn sàng");
    const buttons = screen.getAllByRole("button", { name: /Tải xuống/ });
    expect(buttons).toHaveLength(3);
    expect(buttons[0]).toBeEnabled();
    expect(buttons[1]).toBeDisabled();
    expect(buttons[2]).toBeDisabled();
  });

  it("downloads with a file name built from the slug, the time and the kind", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-ready" })]));
    vi.mocked(downloadTenantBackup).mockResolvedValue({ sizeBytes: 0 });
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("button", { name: /Tải xuống/ }));
    expect(downloadTenantBackup).toHaveBeenCalledWith("t-1", "b-ready", "nexus-qa-x-20261007-010203-manual.zip");
  });

  it("a failed download shows the server's own sentence", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({})]));
    vi.mocked(downloadTenantBackup).mockRejectedValue(new ApiError(409, "BACKUP_FILE_MISSING", "Tệp sao lưu không còn trên máy chủ"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("button", { name: /Tải xuống/ }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Tệp sao lưu không còn trên máy chủ"));
  });

  it("'Sao lưu ngay' sends the trimmed note, adds the running row on top and clears the note", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-old" })]));
    vi.mocked(createTenantBackup).mockResolvedValue(backup({ id: "b-new", status: "running", sizeBytes: null, restorable: false }));
    render(<TenantBackupPanel tenant={TENANT} />);
    await screen.findByText("Sẵn sàng");
    const note = screen.getByLabelText("Ghi chú (không bắt buộc)");
    await userEvent.type(note, "  trước nâng cấp ");
    await userEvent.click(screen.getByRole("button", { name: "Sao lưu ngay" }));
    expect(createTenantBackup).toHaveBeenCalledWith("t-1", "  trước nâng cấp ");
    await screen.findByText("Đang sao lưu");
    expect(note).toHaveValue("");
    expect(toast.success).toHaveBeenCalled();
  });

  it("a 409 'already running' is shown with the server's message", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    vi.mocked(createTenantBackup).mockRejectedValue(new ApiError(409, "BACKUP_IN_PROGRESS", "Khách này đang có một tác vụ chạy"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("button", { name: "Sao lưu ngay" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Khách này đang có một tác vụ chạy"));
  });

  it("polls a running backup every 3s and STOPS once nothing is running", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-run", status: "running", sizeBytes: null, restorable: false })]));
    vi.mocked(getTenantBackup)
      .mockResolvedValueOnce(backup({ id: "b-run", status: "running", sizeBytes: null, restorable: false }))
      .mockResolvedValueOnce(backup({ id: "b-run", status: "ready" }));
    render(<TenantBackupPanel tenant={TENANT} />);
    await screen.findByText("Đang sao lưu");
    expect(getTenantBackup).not.toHaveBeenCalled();

    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(getTenantBackup).toHaveBeenCalledTimes(1);
    expect(getTenantBackup).toHaveBeenCalledWith("t-1", "b-run");
    expect(screen.getByText("Đang sao lưu")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    await waitFor(() => expect(screen.getByText("Sẵn sàng")).toBeInTheDocument());
    expect(getTenantBackup).toHaveBeenCalledTimes(2);

    await act(async () => { await vi.advanceTimersByTimeAsync(12000); });
    expect(getTenantBackup).toHaveBeenCalledTimes(2); // nothing running => no more polling
  });

  it("stops polling and says so after 3 failed polls in a row (backup purged / access lost)", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-run", status: "running", sizeBytes: null, restorable: false })]));
    vi.mocked(getTenantBackup).mockRejectedValue(new ApiError(404, "BACKUP_NOT_FOUND", "x"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await screen.findByText("Đang sao lưu");

    await act(async () => { await vi.advanceTimersByTimeAsync(9000); });
    expect(getTenantBackup).toHaveBeenCalledTimes(3);
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("Không theo dõi được"));
    await act(async () => { await vi.advanceTimersByTimeAsync(12000); });
    expect(getTenantBackup).toHaveBeenCalledTimes(3);
  });

  it("a list that cannot load says so instead of showing an empty state", async () => {
    vi.mocked(listTenantBackups).mockRejectedValue(new ApiError(403, "FORBIDDEN", "Yêu cầu quyền quản trị viên"));
    render(<TenantBackupPanel tenant={TENANT} />);
    expect(await screen.findByText("Yêu cầu quyền quản trị viên")).toBeInTheDocument();
    expect(screen.queryByText("Chưa có bản sao lưu nào")).not.toBeInTheDocument();
  });

  it("'Tải thêm' appends the next page using the server cursor", async () => {
    vi.mocked(listTenantBackups)
      .mockResolvedValueOnce(list([backup({ id: "b-1" })], "CUR1"))
      .mockResolvedValueOnce(list([backup({ id: "b-2", note: "bản cũ" })], null));
    render(<TenantBackupPanel tenant={TENANT} />);
    const more = await screen.findByRole("button", { name: "Tải thêm" });
    await userEvent.click(more);
    await screen.findByText(/bản cũ/);
    expect(listTenantBackups).toHaveBeenLastCalledWith("t-1", { after: "CUR1" });
    expect(screen.queryByRole("button", { name: "Tải thêm" })).not.toBeInTheDocument();
    const rows = screen.getAllByRole("row");
    expect(rows.length).toBeGreaterThanOrEqual(3);
    expect(within(rows[0]).queryByText(/Tải xuống/)).toBeNull();
  });

  // specs/068 lát B (T053): the write-freeze switch lives in this panel.
  it("the freeze toggle sends the chosen minutes, shows the deadline badge, and 'Mở khoá' lifts it", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    const until = new Date(Date.now() + 15 * 60_000).toISOString();
    vi.mocked(setTenantMaintenance).mockResolvedValue({ maintenanceUntil: until });
    vi.mocked(clearTenantMaintenance).mockResolvedValue(undefined);
    const onChange = vi.fn();
    render(<TenantBackupPanel tenant={TENANT} onMaintenanceChange={onChange} />);

    const toggle = await screen.findByRole("switch", { name: /Tạm khoá ghi dữ liệu của khách/ });
    expect(toggle).not.toBeChecked();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Khoá trong" }), "30");
    await userEvent.click(toggle);

    expect(setTenantMaintenance).toHaveBeenCalledWith("t-1", 30);
    expect(await screen.findByText(/Đang bảo trì đến/)).toBeInTheDocument();
    expect(toggle).toBeChecked();
    expect(onChange).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByRole("button", { name: "Mở khoá" }));
    expect(clearTenantMaintenance).toHaveBeenCalledWith("t-1");
    await waitFor(() => expect(screen.queryByText(/Đang bảo trì đến/)).not.toBeInTheDocument());
    expect(onChange).toHaveBeenCalledTimes(2);
  });

  it("opens already frozen when the list reports a live deadline, and a server refusal is toasted verbatim", async () => {
    const until = new Date(Date.now() + 10 * 60_000).toISOString();
    vi.mocked(listTenantBackups).mockResolvedValue({ data: [], meta: { currentSchemaVersion: 85, maintenanceUntil: until, nextCursor: null } });
    vi.mocked(clearTenantMaintenance).mockRejectedValue(new ApiError(409, "BACKUP_IN_PROGRESS", "Khách này đang khôi phục dữ liệu"));
    render(<TenantBackupPanel tenant={TENANT} />);

    expect(await screen.findByText(/Đang bảo trì đến/)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: /Tạm khoá ghi dữ liệu của khách/ })).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Mở khoá" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Khách này đang khôi phục dữ liệu"));
    expect(screen.getByText(/Đang bảo trì đến/)).toBeInTheDocument(); // still frozen: the refusal changed nothing
  });

  it("'Kiểm tra khôi phục' is locked with the reason when the backup is not restorable", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([
      backup({ id: "b-old", restorable: false, notRestorableReason: "SCHEMA_MISMATCH" }),
    ]));
    render(<TenantBackupPanel tenant={TENANT} />);
    const btn = await screen.findByRole("button", { name: /Kiểm tra khôi phục — bản sao lưu lúc/ });
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute("title", "Bản này được tạo ở phiên bản cấu trúc khác với hiện tại.");
  });

  it("'Kiểm tra khôi phục' opens the check for that backup", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-ok" })]));
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(422, "TENANT_BACKUP_CORRUPT", "x"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("button", { name: /Kiểm tra khôi phục — bản sao lưu lúc/ }));
    expect(await screen.findByText("Bản sao lưu bị hỏng — không dùng được")).toBeInTheDocument();
    expect(previewTenantRestore).toHaveBeenCalledWith("t-1", "b-ok");
  });

  // specs/068 lát E (T087): the schedule block and the delete button.
  it("shows the schedule and saves a switch-off with the current keep count", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    vi.mocked(putTenantBackupSchedule).mockResolvedValue({ ...SCHEDULE, enabled: false, nextDueAt: null });
    render(<TenantBackupPanel tenant={TENANT} />);

    const toggle = await screen.findByRole("switch", { name: /Tự sao lưu hằng ngày lúc 02:00/ });
    expect(toggle).toBeChecked();
    expect(screen.getByText(/Lần tự sao lưu gần nhất/)).toBeInTheDocument();
    expect(screen.getByText(/Lần kế tiếp dự kiến/)).toBeInTheDocument();

    await userEvent.click(toggle);
    expect(putTenantBackupSchedule).toHaveBeenCalledWith("t-1", false, 7);
    await waitFor(() => expect(screen.getByRole("switch", { name: /Tự sao lưu hằng ngày/ })).not.toBeChecked());
    expect(screen.queryByText(/Lần kế tiếp dự kiến/)).not.toBeInTheDocument(); // nothing is due while switched off
  });

  it("a refused schedule save is toasted verbatim and the switch keeps its old state", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    vi.mocked(putTenantBackupSchedule).mockRejectedValue(new ApiError(422, "BACKUP_SCHEDULE_INVALID", "keepScheduled phải từ 1 đến 30"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("switch", { name: /Tự sao lưu hằng ngày/ }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("keepScheduled phải từ 1 đến 30"));
    expect(screen.getByRole("switch", { name: /Tự sao lưu hằng ngày/ })).toBeChecked();
  });

  it("a failed schedule load says so instead of hiding the problem", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([]));
    vi.mocked(getTenantBackupSchedule).mockRejectedValue(new ApiError(500, "BACKUP_FAILED", "x"));
    render(<TenantBackupPanel tenant={TENANT} />);
    expect(await screen.findByText("Không đọc được lịch tự sao lưu")).toBeInTheDocument();
  });

  it("'Xoá' only exists for a backup that is not running / restoring, and asks before deleting", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([
      backup({ id: "b-ready" }),
      backup({ id: "b-run", status: "running", restorable: false, notRestorableReason: "NOT_READY" }),
      backup({ id: "b-restoring", kind: "pre_restore", restore: { sourceBackupId: "b-ready", status: "running", step: "apply", committedAt: null, report: null, finishedAt: null } }),
    ]));
    vi.mocked(deleteTenantBackup).mockResolvedValue();
    render(<TenantBackupPanel tenant={TENANT} />);

    const buttons = await screen.findAllByRole("button", { name: /Xoá bản sao lưu lúc/ });
    expect(buttons).toHaveLength(1);
    await userEvent.click(buttons[0]);
    expect(deleteTenantBackup).not.toHaveBeenCalled(); // nothing happens before the confirmation
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(deleteTenantBackup).toHaveBeenCalledWith("t-1", "b-ready"));
    await waitFor(() => expect(screen.queryByRole("button", { name: /Xoá bản sao lưu lúc/ })).not.toBeInTheDocument());
  });

  it("a delete the server refuses (in use) is toasted and the row stays", async () => {
    vi.mocked(listTenantBackups).mockResolvedValue(list([backup({ id: "b-ready" })]));
    vi.mocked(deleteTenantBackup).mockRejectedValue(new ApiError(409, "BACKUP_DELETE_IN_USE", "Bản sao lưu đang được dùng"));
    render(<TenantBackupPanel tenant={TENANT} />);
    await userEvent.click(await screen.findByRole("button", { name: /Xoá bản sao lưu lúc/ }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Bản sao lưu đang được dùng"));
    expect(screen.getByText("Sẵn sàng")).toBeInTheDocument();
  });
});
