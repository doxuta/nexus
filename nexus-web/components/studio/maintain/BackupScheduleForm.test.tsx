// specs/079 f7-sao-luu-lich-xoa T129: schedule form (toggle from the real schedule, keep 1–30, PUT) + row delete.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type TenantBackup } from "@/lib/api";

vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  getTenantBackupSchedule: vi.fn(), putTenantBackupSchedule: vi.fn(), deleteTenantBackup: vi.fn(),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { deleteTenantBackup, getTenantBackupSchedule, putTenantBackupSchedule } from "@/lib/api";
import { BackupScheduleForm } from "./BackupScheduleForm";
import { BackupTable } from "./BackupTable";

const S = (x = {}) => ({ enabled: true, keepScheduled: 7, hourLocal: 3, lastScheduledAt: "2026-10-13T20:00:00Z", nextDueAt: "2026-10-14T20:00:00Z", ...x });
const wrap = (ui: React.ReactNode) => render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui}</QueryClientProvider>);
const B = (x: Partial<TenantBackup> = {}): TenantBackup => ({
  id: "b1", tenantId: "t", kind: "manual", status: "ready", note: null, sizeBytes: 1, sha256: null, schemaVersion: 1, formatVersion: 1,
  rowCount: 1, tableCount: 1, fileCount: 0, blobBytes: 0, warnings: [], error: null, createdAt: "2026-10-14T08:05:00Z", finishedAt: null,
  createdBy: null, restorable: true, notRestorableReason: null, ...x,
});

beforeEach(() => {
  vi.mocked(getTenantBackupSchedule).mockReset().mockResolvedValue(S());
  vi.mocked(putTenantBackupSchedule).mockReset().mockResolvedValue(S({ keepScheduled: 10 }));
  vi.mocked(deleteTenantBackup).mockReset().mockResolvedValue();
});

describe("BackupScheduleForm", () => {
  it("toggle label uses the real hour; shows last and next", async () => {
    wrap(<BackupScheduleForm tenantId="t" />);
    expect(await screen.findByRole("switch", { name: /lúc 03:00/ })).toBeChecked();
    expect(screen.getByText(/Lần tự sao lưu gần nhất/)).toBeInTheDocument();
    expect(screen.getByText(/Lần kế tiếp dự kiến/)).toBeInTheDocument();
  });

  it("keep 0 and 31 are stopped at the field; 10 saves {enabled, keepScheduled}", async () => {
    wrap(<BackupScheduleForm tenantId="t" />);
    const keep = await screen.findByLabelText("Giữ lại số bản tự động");
    for (const bad of ["0", "31"]) {
      fireEvent.change(keep, { target: { value: bad } });
      await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
      expect(screen.getByText("Nhập số bản từ 1 đến 30.")).toBeInTheDocument();
    }
    expect(putTenantBackupSchedule).not.toHaveBeenCalled();
    fireEvent.change(keep, { target: { value: "10" } });
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(putTenantBackupSchedule).toHaveBeenCalledWith("t", true, 10));
  });

  it("server BACKUP_SCHEDULE_INVALID shows at the field", async () => {
    vi.mocked(putTenantBackupSchedule).mockRejectedValue(new ApiError(422, "BACKUP_SCHEDULE_INVALID", "x"));
    wrap(<BackupScheduleForm tenantId="t" />);
    await screen.findByRole("switch");
    await userEvent.click(screen.getByRole("switch"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(await screen.findByText("Nhập số bản từ 1 đến 30.")).toBeInTheDocument();
    expect(putTenantBackupSchedule).toHaveBeenCalledWith("t", false, 7);
  });

  it("engine missing (404) draws nothing", async () => {
    vi.mocked(getTenantBackupSchedule).mockRejectedValue(new ApiError(404, "UNKNOWN", "x"));
    const { container } = wrap(<BackupScheduleForm tenantId="t" />);
    await waitFor(() => expect(getTenantBackupSchedule).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(container.textContent).toBe("");
  });
});

describe("BackupTable delete", () => {
  const props = { downloadingId: null, onDownload: vi.fn(), onRestore: vi.fn() };
  it("Xoá asks first; confirm calls onDelete once", async () => {
    const onDelete = vi.fn();
    render(<BackupTable rows={[B()]} {...props} onDelete={onDelete} />);
    await userEvent.click(screen.getByRole("button", { name: /^Xoá bản/ }));
    expect(onDelete).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Xoá bản này" }));
    expect(onDelete).toHaveBeenCalledWith(expect.objectContaining({ id: "b1" }));
  });
  it("pre_restore row keeps 'Hoàn tác'", () => {
    render(<BackupTable rows={[B({ kind: "pre_restore" })]} {...props} onDelete={vi.fn()} />);
    expect(screen.getByRole("button", { name: /Hoàn tác/ })).toBeInTheDocument();
  });
});
