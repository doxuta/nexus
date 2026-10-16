// TenantRestoreDialog — specs/068-tenant-backup-restore lát C (T061): "Kiểm tra khôi phục" shows
// what a restore would change, without any button that restores. lib/api is mocked at the seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TenantRestoreDialog } from "./TenantRestoreDialog";
import {
  ApiError, getTenantBackup, previewTenantRestore, startTenantRestore,
  type RestorePlan, type AccessChanges, type TenantBackup,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, previewTenantRestore: vi.fn(), startTenantRestore: vi.fn(), getTenantBackup: vi.fn() };
});

const TENANT = { id: "t-1", slug: "qa-x", name: "Khách QA X" };
const BACKUP = { id: "b-1", createdAt: "2026-10-07T01:02:03.000Z" };

const noAccess: AccessChanges = {
  total: 0, roleGrantsWidened: 0, fieldAclReopened: 0, sharesRegranted: 0, membershipsReadded: 0,
  sensitiveFieldsCleared: 0, workflowsReenabled: 0, outboundDestinationsReenabled: 0, outboundHosts: [],
  usersAffected: 0, apiKeysAffected: 0,
};

function plan(over: Partial<RestorePlan> = {}): RestorePlan {
  return {
    backupId: "b-1", backupCreatedAt: "2026-10-07T01:02:03Z", integrity: { ok: true },
    schema: { backup: 91, current: 91, ok: true },
    groups: [
      { group: "data", label: "Dữ liệu", tables: [{ table: "obj_crm_contacts", label: "Liên hệ", rowsInBackup: 120, rowsNow: 150 }] },
      { group: "access", label: "Quyền", tables: [{ table: "acl_roles", label: "acl_roles", rowsInBackup: 3, rowsNow: 4 }] },
    ],
    blobs: { toWrite: 2, bytesToWrite: 2048, toRemove: 1 },
    warnings: [{ code: "ROLE_LOST", count: 1 }, { code: "PENDING_JOBS_KEPT", count: 4 }],
    accessChanges: noAccess,
    kept: ["Người dùng, mật khẩu và đăng nhập (SSO, xác thực hai bước)", "Nhật ký hoạt động và cảnh báo bảo mật"],
    preRestoreEstimate: { rows: 154, files: 3, bytes: 4096, freeBytes: 1 << 30 },
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(previewTenantRestore).mockReset();
  vi.mocked(startTenantRestore).mockReset();
  vi.mocked(getTenantBackup).mockReset();
});

function row(restore: NonNullable<TenantBackup["restore"]>): TenantBackup {
  return {
    id: "r-1", tenantId: "t-1", kind: "pre_restore", status: "ready", note: null, sizeBytes: 1, sha256: null, schemaVersion: 91,
    formatVersion: 1, rowCount: 1, tableCount: 1, fileCount: 0, blobBytes: 0, warnings: [], error: null,
    createdAt: "2026-10-14T01:00:00Z", finishedAt: null, createdBy: null, restorable: true, notRestorableReason: null, restore,
  };
}

describe("TenantRestoreDialog", () => {
  it("shows groups, table labels, both row counts, warnings, the blobs and the kept list", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan());
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);

    expect(await screen.findByText("Liên hệ")).toBeInTheDocument();
    expect(screen.getByText("Dữ liệu")).toBeInTheDocument();
    expect(screen.getByText("Quyền")).toBeInTheDocument();
    expect(screen.getByText("120 trong bản · 150 hiện tại")).toBeInTheDocument();
    expect(screen.getByText("1 người dùng sẽ mất vai")).toBeInTheDocument();
    expect(screen.getByText("4 việc nền đang chờ được giữ nguyên")).toBeInTheDocument();
    expect(screen.getByText("Giữ nguyên")).toBeInTheDocument();
    expect(screen.getByText("Nhật ký hoạt động và cảnh báo bảo mật")).toBeInTheDocument();
    expect(previewTenantRestore).toHaveBeenCalledWith("t-1", "b-1");
    expect(screen.getByRole("button", { name: "Khôi phục về bản này" })).toBeEnabled();
  });

  it("access widened: the block stands ABOVE the groups and lists only the kinds above zero", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan({
      warnings: [{ code: "ACCESS_WIDENED", count: 3 }],
      accessChanges: { ...noAccess, total: 3, roleGrantsWidened: 2, sharesRegranted: 1, outboundDestinationsReenabled: 1, outboundHosts: ["partner.example"], usersAffected: 5, apiKeysAffected: 2 },
    }));
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);

    const block = await screen.findByText("Khôi phục sẽ MỞ RỘNG lại quyền đã bị thu hẹp");
    const groups = screen.getByText("Liên hệ");
    expect(block.compareDocumentPosition(groups) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(/2 quyền theo vai/)).toBeInTheDocument();
    expect(screen.getByText(/1 chia sẻ hồ sơ/)).toBeInTheDocument();
    expect(screen.queryByText(/quyền trường/)).toBeNull();
    expect(screen.getByText(/5 người dùng/)).toBeInTheDocument();
    expect(screen.getByText(/2 khoá API/)).toBeInTheDocument();
    expect(screen.getByText(/partner\.example/)).toBeInTheDocument();
  });

  it("no widening: no block at all", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan());
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);
    await screen.findByText("Liên hệ");
    expect(screen.queryByText("Khôi phục sẽ MỞ RỘNG lại quyền đã bị thu hẹp")).toBeNull();
  });

  it("a corrupt file says so plainly and offers nothing else", async () => {
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(422, "TENANT_BACKUP_CORRUPT", "Bản sao lưu bị hỏng: mã băm không khớp"));
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);
    expect(await screen.findByText("Bản sao lưu bị hỏng — không dùng được")).toBeInTheDocument();
    expect(screen.queryByText("Giữ nguyên")).toBeNull();
    expect(screen.queryByRole("button", { name: /Khôi phục về bản này/ })).toBeNull();
  });

  it("another refusal shows the server's own sentence", async () => {
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(409, "TENANT_RESTORE_SCHEMA_MISMATCH", "Bản sao lưu được tạo ở phiên bản cấu trúc khác với hiện tại"));
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);
    expect(await screen.findByText("Bản sao lưu được tạo ở phiên bản cấu trúc khác với hiện tại")).toBeInTheDocument();
  });

  it("the prompt locks the confirm button until the customer's slug is typed, and says the history stays", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan({
      accessChanges: { ...noAccess, total: 2, roleGrantsWidened: 2, usersAffected: 1 },
    }));
    vi.mocked(startTenantRestore).mockResolvedValue({ restoreId: "r-1", status: "running" });
    vi.mocked(getTenantBackup).mockResolvedValue(row({ sourceBackupId: "b-1", status: "running", step: "lock", committedAt: null, report: null, finishedAt: null }));
    const user = userEvent.setup();
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);
    await user.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));

    expect(await screen.findByText(/Lịch sử xuất bản giữ nguyên; cấu hình sống quay về ngày/)).toBeInTheDocument();
    expect(screen.getByText(/Mở rộng lại quyền đã thu hẹp: 2 mục/)).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: "Khôi phục" });
    expect(confirm).toBeDisabled();
    await user.type(screen.getByRole("textbox"), "qa-y");
    expect(confirm).toBeDisabled();
    await user.clear(screen.getByRole("textbox"));
    await user.type(screen.getByRole("textbox"), "qa-x");
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await waitFor(() => expect(startTenantRestore).toHaveBeenCalledWith("t-1", "b-1", "qa-x"));
  });

  it("progress follows the server's restore.step (not status) and ends with the report", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan());
    vi.mocked(startTenantRestore).mockResolvedValue({ restoreId: "r-1", status: "running" });
    vi.mocked(getTenantBackup)
      .mockResolvedValueOnce(row({ sourceBackupId: "b-1", status: "running", step: "apply", committedAt: null, report: null, finishedAt: null }))
      .mockResolvedValue(row({
        sourceBackupId: "b-1", status: "done", step: "doctor", committedAt: "2026-10-14T01:05:00Z", finishedAt: "2026-10-14T01:06:00Z",
        report: {
          sourceBackupId: "b-1", preRestoreBackupId: "r-1", inserted: { obj_x: 40, acl_roles: 2 }, rolesLost: 1, membershipsSkipped: 0,
          sharesSkipped: 0, webhookKeysDropped: 2, blobsWritten: 3, blobsRemoved: 1, doctor: { fixed: 4, remainingCritical: 0 },
          accessChanges: { ...noAccess, total: 1, sharesRegranted: 1 },
        },
      }));
    const onFinished = vi.fn();
    const user = userEvent.setup();
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} onFinished={onFinished} />);
    await user.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));
    await user.type(screen.getByRole("textbox"), "qa-x");
    await user.click(screen.getByRole("button", { name: "Khôi phục" }));

    expect(await screen.findByText("Đang khôi phục. Đóng cửa sổ này không làm dừng việc đang chạy trên máy chủ.")).toBeInTheDocument();
    expect(screen.getByText("Khoá ghi")).toBeInTheDocument();
    expect(screen.getByText("Tự sao lưu")).toBeInTheDocument();
    expect(screen.getByText("Khám sức khoẻ")).toBeInTheDocument();

    expect(await screen.findByText("Đã khôi phục xong.", {}, { timeout: 6000 })).toBeInTheDocument();
    expect(screen.getByText("1 người dùng mất vai (vai của họ không có trong bản)")).toBeInTheDocument();
    expect(screen.getByText("2 webhook ra về không khoá bí mật")).toBeInTheDocument();
    expect(screen.getByText("Khám sức khoẻ: đã sửa 4, còn 0 lỗi nghiêm trọng")).toBeInTheDocument();
    expect(screen.getByText(/1 chia sẻ hồ sơ/)).toBeInTheDocument();
    expect(onFinished).toHaveBeenCalled();
  }, 15000);

  it("a refusal on start shows the server's sentence and no progress screen", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan());
    vi.mocked(startTenantRestore).mockRejectedValue(new ApiError(409, "BACKUP_IN_PROGRESS", "Khách này đang có một tác vụ sao lưu hoặc khôi phục chạy"));
    const user = userEvent.setup();
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} onClose={() => {}} />);
    await user.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));
    await user.type(screen.getByRole("textbox"), "qa-x");
    await user.click(screen.getByRole("button", { name: "Khôi phục" }));
    expect(await screen.findByText("Khách này đang có một tác vụ sao lưu hoặc khôi phục chạy")).toBeInTheDocument();
    expect(screen.queryByText("Khám sức khoẻ")).toBeNull();
  });

  it("undo mode words the button as Hoàn tác", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue(plan());
    render(<TenantRestoreDialog tenant={TENANT} backup={BACKUP} mode="undo" onClose={() => {}} />);
    expect(await screen.findByRole("button", { name: "Hoàn tác" })).toBeInTheDocument();
  });
});
